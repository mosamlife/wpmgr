package pdf

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	fpdflib "codeberg.org/go-pdf/fpdf"
)

// A PNG logo with transparency is limited to 2000 x 2000 pixels. Every other
// logo may be up to 4000 x 4000. The fetch reads the limit from the header: a
// PNG has transparency when its colour model has an alpha channel, or is a
// palette with an entry that is not fully opaque.

var (
	// A palette of two entries, black and white.
	plte = pngChunk("PLTE", []byte{0, 0, 0, 255, 255, 255})
	// The palette's first entry is fully transparent.
	transparentFirst = pngChunk("tRNS", []byte{0})
	// Every entry of the palette is fully opaque.
	allOpaque = pngChunk("tRNS", []byte{255, 255})
)

func TestFetchAndValidateLogoRefusesATransparentPNGOver2000Pixels(t *testing.T) {
	cases := map[string][]byte{
		"4000x4000 RGBA":                 flatPNG(t, 4000, 4000, 6, 8),
		"2001 wide, 1 tall":              flatPNG(t, 2001, 1, 6, 8),
		"1 wide, 2001 tall":              flatPNG(t, 1, 2001, 6, 8),
		"2001x2001 16-bit RGBA":          flatPNG(t, 2001, 2001, 6, 16),
		"2001x2001 gray with alpha":      flatPNG(t, 2001, 2001, 4, 8),
		"2001x2001 16-bit gray w. alpha": flatPNG(t, 2001, 2001, 4, 16),
		"2001x2001 palette, transparent": flatPNG(t, 2001, 2001, 3, 8, plte, transparentFirst),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := FetchAndValidateLogo(context.Background(), logoClient(), serveLogo(t, "image/png", body))
			if err == nil {
				t.Fatalf("a PNG with transparency past 2000 pixels was accepted (%d bytes)", len(got))
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

func TestFetchAndValidateLogoKeepsATransparentPNGWithin2000Pixels(t *testing.T) {
	cases := map[string][]byte{
		"2000x2000 RGBA":                 flatPNG(t, 2000, 2000, 6, 8),
		"2000 wide, 1 tall":              flatPNG(t, 2000, 1, 6, 8),
		"1 wide, 2000 tall":              flatPNG(t, 1, 2000, 6, 8),
		"2000x2000 16-bit RGBA":          flatPNG(t, 2000, 2000, 6, 16),
		"2000x2000 gray with alpha":      flatPNG(t, 2000, 2000, 4, 8),
		"2000x2000 palette, transparent": flatPNG(t, 2000, 2000, 3, 8, plte, transparentFirst),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := FetchAndValidateLogo(context.Background(), logoClient(), serveLogo(t, "image/png", body))
			if err != nil {
				t.Fatalf("a PNG with transparency within 2000 pixels was refused: %v", err)
			}
			if !bytes.Equal(got, body) {
				t.Errorf("returned %d bytes, want the %d the server sent", len(got), len(body))
			}
		})
	}
}

func TestPDFDrawsATransparentPNGOf2000Pixels(t *testing.T) {
	fpdflib.SetDefaultCompression(false)
	t.Cleanup(func() { fpdflib.SetDefaultCompression(true) })

	body := flatPNG(t, 2000, 2000, 6, 8)
	logo, err := FetchAndValidateLogo(context.Background(), logoClient(), serveLogo(t, "image/png", body))
	if err != nil {
		t.Fatalf("a 2000x2000 PNG with transparency was refused: %v", err)
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
}

// Past 2000 pixels the limit stays 4000 for a PNG without transparency. A
// 16-bit RGB image is an RGBA64 to the standard library and has no alpha.
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
