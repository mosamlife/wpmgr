// Package encoder is the lilliput-backed (CGO) image encoder for the Media
// Optimizer. It is imported ONLY by internal/media/worker (the EncodeWorker)
// and through it ONLY by cmd/media-encoder. The main API (cmd/wpmgr) MUST NEVER
// transitively import this package — it builds CGO_ENABLED=0 on distroless/
// static, and lilliput is CGO + native codec libs (ADR-043 §1).
//
// This file (encoder.go) carries the PURE interface + errors so the package
// type-checks even where CGO is unavailable; the lilliput implementation lives
// in lilliput.go behind a `//go:build cgo` tag.
package encoder

import (
	"context"
	"errors"
)

// Encoder turns source image bytes into an optimized variant in the requested
// target format. Implementations detect the SOURCE format from magic bytes
// (never the caller's claimed MIME — ADR-043 §4).
type Encoder interface {
	// Encode runs one variant encode. It is safe for concurrent use up to the
	// pool size the implementation was built with.
	Encode(ctx context.Context, req EncodeRequest) (EncodeResult, error)
	// Close releases native resources (the ImageOps pool). Call once at shutdown.
	Close()
}

// EncodeRequest is one variant to encode.
type EncodeRequest struct {
	// Source is the raw source image bytes (jpeg/png), already presigned-GET'd
	// from object storage by the worker. The encoder NEVER touches the network.
	Source []byte
	// TargetFormat is the requested output format: "avif" | "webp" | "original".
	// "original" re-encodes to the detected source format (mozjpeg-class JPEG or
	// lossless PNG) per the ADR-043 §4 table.
	TargetFormat string
	// Lossless selects the lossless column of the ADR-043 §4 quality table.
	Lossless bool
}

// EncodeResult is the optimized output.
type EncodeResult struct {
	// Output is the optimized image bytes (to be presigned-PUT by the worker).
	Output []byte
	// OutputMime is derived from the target format (e.g. "image/avif").
	OutputMime string
	// SourceMime is the format detected from the source magic bytes.
	SourceMime string
	// Width / Height of the source image (after decode).
	Width  int
	Height int
}

// Encoder errors (ADR-043 §4/§5). The worker maps these to a per-variant human
// reason in sizes_unoptimized; they do NOT fail sibling variants.
var (
	// ErrUnsupportedSource is returned when the source magic bytes are not an
	// optimizable format (only image/jpeg and image/png are accepted).
	ErrUnsupportedSource = errors.New("encoder: unsupported source format")
	// ErrEncoderTimeout is returned when a single encode exceeds the per-encode
	// deadline (EncodeTimeout, 60s).
	ErrEncoderTimeout = errors.New("encoder: encode timed out")
	// ErrDimensionsTooBig is returned when the source exceeds the size or pixel
	// guards (> 50 MB or > 100 MP).
	ErrDimensionsTooBig = errors.New("encoder: source exceeds size/dimension limits")
)
