package pdf

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	fpdflib "codeberg.org/go-pdf/fpdf"
)

// A PNG logo with transparency is limited to 4 million pixels in all, so a wide
// banner is kept where a large square is not. Every logo, with transparency or
// without, is also limited to 4000 pixels a side. The fetch reads the limits
// from the header: a PNG has transparency when its colour model has an alpha
// channel, or is a palette with an entry that is not fully opaque.

var (
	// A palette of two entries, black and white.
	plte = pngChunk("PLTE", []byte{0, 0, 0, 255, 255, 255})
	// The palette's first entry is fully transparent.
	transparentFirst = pngChunk("tRNS", []byte{0})
	// Every entry of the palette is fully opaque.
	allOpaque = pngChunk("tRNS", []byte{255, 255})
)

func TestFetchAndValidateLogoRefusesATransparentPNGOver4MillionPixels(t *testing.T) {
	cases := map[string][]byte{
		"4000x4000 RGBA": flatPNG(t, 4000, 4000, 6, 8),
		"2001x2001 RGBA": flatPNG(t, 2001, 2001, 6, 8),
		// One row or one column more than 4 million.
		"4000x1001 RGBA": flatPNG(t, 4000, 1001, 6, 8),
		"1001x4000 RGBA": flatPNG(t, 1001, 4000, 6, 8),
		// The limit on a side holds for a small area too.
		"4001 wide, 1 tall RGBA":         flatPNG(t, 4001, 1, 6, 8),
		"2001x2001 16-bit RGBA":          flatPNG(t, 2001, 2001, 6, 16),
		"2001x2001 gray with alpha":      flatPNG(t, 2001, 2001, 4, 8),
		"2001x2001 16-bit gray w. alpha": flatPNG(t, 2001, 2001, 4, 16),
		"2001x2001 palette, transparent": flatPNG(t, 2001, 2001, 3, 8, plte, transparentFirst),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := FetchAndValidateLogo(context.Background(), logoClient(), serveLogo(t, "image/png", body))
			if err == nil {
				t.Fatalf("a PNG with transparency past the limit was accepted (%d bytes)", len(got))
			}
			if !strings.Contains(err.Error(), "dimensions") {
				t.Errorf("error %q is not the size refusal", err)
			}
			if got != nil {
				t.Errorf("returned %d bytes alongside an error", len(got))
			}
		})
	}
}

func TestFetchAndValidateLogoKeepsATransparentPNGWithin4MillionPixels(t *testing.T) {
	cases := map[string][]byte{
		"2400x600 RGBA": flatPNG(t, 2400, 600, 6, 8),
		// Exactly 4 million pixels.
		"2000x2000 RGBA": flatPNG(t, 2000, 2000, 6, 8),
		"4000x1000 RGBA": flatPNG(t, 4000, 1000, 6, 8),
		"1000x4000 RGBA": flatPNG(t, 1000, 4000, 6, 8),
		// The largest a side may be.
		"4000 wide, 1 tall RGBA":         flatPNG(t, 4000, 1, 6, 8),
		"1 wide, 4000 tall RGBA":         flatPNG(t, 1, 4000, 6, 8),
		"2000x2000 16-bit RGBA":          flatPNG(t, 2000, 2000, 6, 16),
		"2400x600 gray with alpha":       flatPNG(t, 2400, 600, 4, 8),
		"2400x600 palette, transparent":  flatPNG(t, 2400, 600, 3, 8, plte, transparentFirst),
		"2000x2000 palette, transparent": flatPNG(t, 2000, 2000, 3, 8, plte, transparentFirst),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := FetchAndValidateLogo(context.Background(), logoClient(), serveLogo(t, "image/png", body))
			if err != nil {
				t.Fatalf("a PNG with transparency within 4 million pixels was refused: %v", err)
			}
			if !bytes.Equal(got, body) {
				t.Errorf("returned %d bytes, want the %d the server sent", len(got), len(body))
			}
		})
	}
}

// A banner and the largest square the limit allows both reach the PDF.
func TestPDFDrawsATransparentPNGWithin4MillionPixels(t *testing.T) {
	fpdflib.SetDefaultCompression(false)
	t.Cleanup(func() { fpdflib.SetDefaultCompression(true) })

	sizes := map[string][2]int{"2400x600": {2400, 600}, "2000x2000": {2000, 2000}}
	for name, size := range sizes {
		t.Run(name, func(t *testing.T) {
			body := flatPNG(t, size[0], size[1], 6, 8)
			logo, err := FetchAndValidateLogo(context.Background(), logoClient(), serveLogo(t, "image/png", body))
			if err != nil {
				t.Fatalf("a %s PNG with transparency was refused: %v", name, err)
			}
			logs := recordLogs(t)
			out, err := NewFpdfRenderer().Render(pdfFullFixture(), logo)
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			if !bytes.Contains(out, imageObject) {
				t.Error("rendered PDF does not carry the logo")
			}
			if n := logs.count(slog.LevelWarn); n != 0 {
				t.Errorf("a logo that draws logged %d warnings", n)
			}
		})
	}
}

// The area limit is for a PNG with transparency alone: without it a PNG may be
// 4000 x 4000. A 16-bit RGB image is an RGBA64 to the standard library and has
// no alpha.
func TestFetchAndValidateLogoKeepsAnOpaquePNGUpTo4000Pixels(t *testing.T) {
	cases := map[string][]byte{
		"4000x4000 RGB":                 flatPNG(t, maxLogoDim, maxLogoDim, 2, 8),
		"4000x4000 16-bit RGB":          flatPNG(t, maxLogoDim, maxLogoDim, 2, 16),
		"4000x4000 gray":                flatPNG(t, maxLogoDim, maxLogoDim, 0, 8),
		"4000x4000 palette":             flatPNG(t, maxLogoDim, maxLogoDim, 3, 8, plte),
		"4000x4000 palette, all opaque": flatPNG(t, maxLogoDim, maxLogoDim, 3, 8, plte, allOpaque),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := FetchAndValidateLogo(context.Background(), logoClient(), serveLogo(t, "image/png", body))
			if err != nil {
				t.Fatalf("a PNG without transparency within 4000 pixels was refused: %v", err)
			}
			if !bytes.Equal(got, body) {
				t.Errorf("returned %d bytes, want the %d the server sent", len(got), len(body))
			}
		})
	}
}
