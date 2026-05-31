//go:build cgo

package encoder

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
)

// makeJPEG renders a small gradient JPEG fixture in-memory so the golden test
// needs no checked-in binary. 256x256 is large enough that AVIF/WebP produce a
// non-trivial output and small enough to encode fast.
func makeJPEG(t *testing.T) []byte {
	t.Helper()
	const w, h = 256, 256
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: uint8((x + y) / 2), A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 92}); err != nil {
		t.Fatalf("encode fixture jpeg: %v", err)
	}
	return buf.Bytes()
}

func TestLilliputEncode_Golden(t *testing.T) {
	src := makeJPEG(t)
	enc := NewLilliputEncoder(2)
	defer enc.Close()

	cases := []struct {
		name     string
		target   string
		wantMime string
		magic    []byte // a stable prefix/marker we can assert on the output
		magicAt  int
	}{
		{name: "avif", target: "avif", wantMime: mimeAVIF, magic: []byte("ftyp"), magicAt: 4},
		{name: "webp", target: "webp", wantMime: mimeWebP, magic: []byte("WEBP"), magicAt: 8},
		{name: "original_jpeg", target: "original", wantMime: mimeJPEG, magic: []byte{0xFF, 0xD8}, magicAt: 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := enc.Encode(context.Background(), EncodeRequest{
				Source:       src,
				TargetFormat: tc.target,
			})
			if err != nil {
				t.Fatalf("encode %s: %v", tc.target, err)
			}
			if res.OutputMime != tc.wantMime {
				t.Errorf("output mime = %q, want %q", res.OutputMime, tc.wantMime)
			}
			if res.SourceMime != mimeJPEG {
				t.Errorf("source mime = %q, want %q (magic-byte detection)", res.SourceMime, mimeJPEG)
			}
			if len(res.Output) == 0 {
				t.Fatalf("empty output for %s", tc.target)
			}
			// Size band: a 256x256 gradient should encode well under the source.
			if len(res.Output) > len(src)*4 {
				t.Errorf("output %d bytes is implausibly large vs source %d", len(res.Output), len(src))
			}
			// Magic-byte / container marker assertion.
			if tc.magicAt+len(tc.magic) <= len(res.Output) {
				got := res.Output[tc.magicAt : tc.magicAt+len(tc.magic)]
				if !bytes.Equal(got, tc.magic) {
					t.Errorf("output marker at %d = %x, want %x", tc.magicAt, got, tc.magic)
				}
			}
			if res.Width != 256 || res.Height != 256 {
				t.Errorf("dims = %dx%d, want 256x256", res.Width, res.Height)
			}
		})
	}
}

func TestLilliputEncode_UnsupportedSource(t *testing.T) {
	enc := NewLilliputEncoder(1)
	defer enc.Close()

	_, err := enc.Encode(context.Background(), EncodeRequest{
		Source:       []byte("not an image"),
		TargetFormat: "avif",
	})
	if err == nil {
		t.Fatal("expected error for non-image source")
	}
}
