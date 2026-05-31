package encoder

import "time"

// Resource + timeout guards (ADR-043 §5).
const (
	// MaxSourceBytes rejects sources larger than 50 MB.
	MaxSourceBytes = 50 << 20
	// MaxSourcePixels rejects sources larger than 100 megapixels.
	MaxSourcePixels = 100 * 1000 * 1000
	// EncodeTimeout bounds a single encode (60s).
	EncodeTimeout = 60 * time.Second
)

// Output MIME types.
const (
	mimeJPEG = "image/jpeg"
	mimePNG  = "image/png"
	mimeWebP = "image/webp"
	mimeAVIF = "image/avif"
)

// encodeParams is the resolved lilliput knob set for one (target, lossless,
// source) combination. The fields mirror lilliput's EncodeOptions keys; the
// lilliput.go impl translates them into the map it passes to ImageOps.Transform.
type encodeParams struct {
	// outputExt is the lilliput output extension ("avif"|"webp"|"jpeg"|"png").
	outputExt string
	// outputMime is the response MIME.
	outputMime string
	// avifQuality / avifSpeed are set for AVIF targets.
	avifQuality int
	avifSpeed   int
	// webpQuality is set for WebP targets.
	webpQuality int
	// jpegQuality / jpegProgressive are set for JPEG (original) targets.
	jpegQuality     int
	jpegProgressive int
	// pngCompression is set for PNG (original) targets.
	pngCompression int
}

// paramsFor resolves the ADR-043 §4 quality table for a target format + lossless
// flag + the DETECTED source mime (the source mime only matters for
// target="original", which preserves the source codec).
//
// | Source → target | Lossy (default)             | Lossless mode              |
// |-----------------|-----------------------------|----------------------------|
// | → AVIF          | AvifQuality=50, AvifSpeed=6  | AvifQuality=100, Speed=6   |
// | → WebP          | WebpQuality=80               | WebpQuality=100            |
// | → original JPEG | JpegQuality=82, progressive  | JpegQuality=95, progressive|
// | → original PNG  | lossless, PngCompression=9   | lossless, PngCompression=9 |
func paramsFor(target string, lossless bool, sourceMime string) (encodeParams, bool) {
	switch target {
	case "avif":
		q := 50
		if lossless {
			q = 100
		}
		return encodeParams{
			outputExt: ".avif", outputMime: mimeAVIF,
			avifQuality: q, avifSpeed: 6,
		}, true
	case "webp":
		q := 80
		if lossless {
			q = 100
		}
		return encodeParams{
			outputExt: ".webp", outputMime: mimeWebP,
			webpQuality: q,
		}, true
	case "original":
		switch sourceMime {
		case mimeJPEG:
			q := 82
			if lossless {
				q = 95
			}
			return encodeParams{
				outputExt: ".jpeg", outputMime: mimeJPEG,
				jpegQuality: q, jpegProgressive: 1,
			}, true
		case mimePNG:
			// PNG is always lossless; PngCompression=9 either way.
			return encodeParams{
				outputExt: ".png", outputMime: mimePNG,
				pngCompression: 9,
			}, true
		}
	}
	return encodeParams{}, false
}
