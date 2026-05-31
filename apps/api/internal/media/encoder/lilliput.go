//go:build cgo

package encoder

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/discord/lilliput"
)

// maxImageDim is the largest single dimension lilliput's ImageOps buffer is
// sized for. 8192 covers the full-resolution camera originals we optimize; the
// pixel guard (MaxSourcePixels) catches anything pathological.
const maxImageDim = 8192

// outBufCap is the initial output buffer size handed to Transform. lilliput
// grows it if needed; 50 MB matches the MaxSourceBytes guard so a worst-case
// re-encode never reallocs in the common path.
const outBufCap = MaxSourceBytes

// LilliputEncoder is the CGO Encoder. It keeps a bounded pool of ImageOps so a
// worker concurrency of N never allocates more than N native contexts.
type LilliputEncoder struct {
	pool chan *lilliput.ImageOps
}

// NewLilliputEncoder builds an encoder with `concurrency` pooled ImageOps
// contexts. concurrency should equal the media_encode queue's MaxWorkers so
// every in-flight encode has its own context (Transform is not goroutine-safe
// per ImageOps). Call Close() at shutdown.
func NewLilliputEncoder(concurrency int) *LilliputEncoder {
	if concurrency <= 0 {
		concurrency = 1
	}
	pool := make(chan *lilliput.ImageOps, concurrency)
	for i := 0; i < concurrency; i++ {
		pool <- lilliput.NewImageOps(maxImageDim)
	}
	return &LilliputEncoder{pool: pool}
}

// Close frees every pooled ImageOps context.
func (e *LilliputEncoder) Close() {
	close(e.pool)
	for ops := range e.pool {
		ops.Close()
	}
}

// Encode detects the source format from magic bytes, enforces the size/pixel
// guards, and re-encodes to the target format with the ADR-043 §4 knobs. A
// per-encode timeout (EncodeTimeout) bounds the native call.
func (e *LilliputEncoder) Encode(ctx context.Context, req EncodeRequest) (EncodeResult, error) {
	if len(req.Source) == 0 {
		return EncodeResult{}, ErrUnsupportedSource
	}
	if len(req.Source) > MaxSourceBytes {
		return EncodeResult{}, ErrDimensionsTooBig
	}

	// Decode the header to learn the REAL source format + dimensions (lilliput
	// reads the magic bytes; the agent's claimed MIME is never trusted).
	decoder, err := lilliput.NewDecoder(req.Source)
	if err != nil {
		return EncodeResult{}, fmt.Errorf("%w: %v", ErrUnsupportedSource, err)
	}
	defer decoder.Close()

	header, err := decoder.Header()
	if err != nil {
		return EncodeResult{}, fmt.Errorf("%w: %v", ErrUnsupportedSource, err)
	}
	srcMime := mimeForDecoderType(decoder.Description())
	if srcMime == "" {
		return EncodeResult{}, ErrUnsupportedSource
	}

	w, h := header.Width(), header.Height()
	if int64(w)*int64(h) > MaxSourcePixels {
		return EncodeResult{}, ErrDimensionsTooBig
	}

	params, ok := paramsFor(req.TargetFormat, req.Lossless, srcMime)
	if !ok {
		return EncodeResult{}, ErrUnsupportedSource
	}

	// Acquire a pooled ImageOps context.
	var ops *lilliput.ImageOps
	select {
	case o, alive := <-e.pool:
		if !alive {
			return EncodeResult{}, errors.New("encoder: pool closed")
		}
		ops = o
	case <-ctx.Done():
		return EncodeResult{}, ctx.Err()
	}
	defer func() {
		// Best-effort return to the pool; a closed pool drops it.
		defer func() { _ = recover() }()
		e.pool <- ops
	}()

	opts := &lilliput.ImageOptions{
		FileType:             params.outputExt,
		Width:                w,
		Height:               h,
		ResizeMethod:         lilliput.ImageOpsNoResize, // re-encode at native size
		NormalizeOrientation: true,
		// EncodeTimeout bounds lilliput's own native encode loop. It defaults to
		// the zero value (which lilliput treats as "expired immediately"), so it
		// MUST be set explicitly — we mirror our per-encode budget.
		EncodeTimeout: EncodeTimeout,
		EncodeOptions: encodeOptionsFor(params),
	}

	// Bound the native encode with the per-encode timeout. lilliput.Transform is
	// synchronous CGO, so we run it on a goroutine and race the deadline.
	type out struct {
		buf []byte
		err error
	}
	resCh := make(chan out, 1)
	dst := make([]byte, outBufCap)
	go func() {
		b, terr := ops.Transform(decoder, opts, dst)
		resCh <- out{buf: b, err: terr}
	}()

	encCtx, cancel := context.WithTimeout(ctx, EncodeTimeout)
	defer cancel()
	start := time.Now()
	select {
	case r := <-resCh:
		_ = start
		if r.err != nil {
			return EncodeResult{}, fmt.Errorf("encoder: transform: %w", r.err)
		}
		// Copy out of the reusable dst slice header (b aliases dst; return a
		// right-sized copy so callers own the bytes).
		out := make([]byte, len(r.buf))
		copy(out, r.buf)
		return EncodeResult{
			Output:     out,
			OutputMime: params.outputMime,
			SourceMime: srcMime,
			Width:      w,
			Height:     h,
		}, nil
	case <-encCtx.Done():
		if errors.Is(encCtx.Err(), context.DeadlineExceeded) {
			return EncodeResult{}, ErrEncoderTimeout
		}
		return EncodeResult{}, encCtx.Err()
	}
}

// encodeOptionsFor maps resolved params to lilliput's EncodeOptions map. Only
// the keys relevant to the chosen output extension are set.
func encodeOptionsFor(p encodeParams) map[int]int {
	switch p.outputExt {
	case ".avif":
		return map[int]int{
			lilliput.AvifQuality: p.avifQuality,
			lilliput.AvifSpeed:   p.avifSpeed,
		}
	case ".webp":
		return map[int]int{
			lilliput.WebpQuality: p.webpQuality,
		}
	case ".jpeg":
		return map[int]int{
			lilliput.JpegQuality:     p.jpegQuality,
			lilliput.JpegProgressive: p.jpegProgressive,
		}
	case ".png":
		return map[int]int{
			lilliput.PngCompression: p.pngCompression,
		}
	}
	return map[int]int{}
}

// mimeForDecoderType maps lilliput's decoder description to our accepted source
// MIME set. Only jpeg + png are optimizable (ADR-043 §4); everything else maps
// to "" (ErrUnsupportedSource).
func mimeForDecoderType(desc string) string {
	switch desc {
	case "JPEG":
		return mimeJPEG
	case "PNG":
		return mimePNG
	default:
		return ""
	}
}

// Compile-time assertion that *LilliputEncoder satisfies Encoder.
var _ Encoder = (*LilliputEncoder)(nil)
