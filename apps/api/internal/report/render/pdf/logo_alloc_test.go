package pdf

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"runtime"
	"testing"
)

// The size of a file says little about the size of the image in it. A logo
// within every limit the fetch sets can still decode to hundreds of megabytes,
// and the workers that fetch logos share a process with every tenant. So the
// fetch reads a logo's header and the shape of its file, and never decodes its
// pixels.

// deepPNG is a w x h PNG of 16-bit RGBA pixels, all zero. Zeros compress to
// almost nothing, so the file is small while the decoded image is not. The
// pixel data is written a row at a time, so building the fixture does not
// need the memory it stands for.
func deepPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	var idat bytes.Buffer
	zw, err := zlib.NewWriterLevel(&idat, zlib.BestSpeed)
	if err != nil {
		t.Fatal(err)
	}
	row := make([]byte, 1+8*w) // a filter byte, then 8 bytes a pixel
	for y := 0; y < h; y++ {
		if _, err := zw.Write(row); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	ihdr := binary.BigEndian.AppendUint32(nil, uint32(w))
	ihdr = binary.BigEndian.AppendUint32(ihdr, uint32(h))
	ihdr = append(ihdr, 16, 6, 0, 0, 0) // 16 bits, RGBA, deflate, adaptive filter, no interlace

	b := []byte("\x89PNG\r\n\x1a\n")
	b = append(b, pngChunk("IHDR", ihdr)...)
	b = append(b, pngChunk("IDAT", idat.Bytes())...)
	return append(b, pngChunk("IEND", nil)...)
}

// pngChunk is one PNG chunk: the length of the data, the type, the data, and a
// CRC over the type and the data.
func pngChunk(typ string, data []byte) []byte {
	b := binary.BigEndian.AppendUint32(nil, uint32(len(data)))
	b = append(b, typ...)
	b = append(b, data...)
	return binary.BigEndian.AppendUint32(b, crc32.ChecksumIEEE(b[4:]))
}

// jpegSegment is one JPEG segment: a marker, a length that counts its own two
// bytes, and the data.
func jpegSegment(marker byte, data ...byte) []byte {
	b := []byte{0xFF, marker}
	b = binary.BigEndian.AppendUint16(b, uint16(len(data)+2))
	return append(b, data...)
}

// progressiveCMYK is a w x h progressive JPEG with four components, CMYK. It
// has the one scan that carries every block's DC coefficient, and each of them
// is zero, which is one bit a block. The file is small and a decoder still
// allocates the coefficients of every block of every component.
func progressiveCMYK(w, h int) []byte {
	b := []byte{0xFF, 0xD8}
	// Adobe APP14 with transform 0: the four components are C, M, Y and K.
	b = append(b, jpegSegment(0xEE, 'A', 'd', 'o', 'b', 'e', 0, 100, 0, 0, 0, 0, 0)...)
	// One quantisation table, every entry 1.
	b = append(b, jpegSegment(0xDB, append([]byte{0}, bytes.Repeat([]byte{1}, 64)...)...)...)
	// Progressive DCT, 8 bits, four components sampled 1x1, all on table 0.
	sof := []byte{8}
	sof = binary.BigEndian.AppendUint16(sof, uint16(h))
	sof = binary.BigEndian.AppendUint16(sof, uint16(w))
	sof = append(sof, 4, 1, 0x11, 0, 2, 0x11, 0, 3, 0x11, 0, 4, 0x11, 0)
	b = append(b, jpegSegment(0xC2, sof...)...)
	// One DC Huffman table: a single code, one bit long, for the symbol 0.
	dht := append([]byte{0x00, 1}, make([]byte, 15)...)
	b = append(b, jpegSegment(0xC4, append(dht, 0)...)...)
	// The scan of all four components: DC coefficients only, first pass.
	b = append(b, jpegSegment(0xDA, 4, 1, 0x00, 2, 0x00, 3, 0x00, 4, 0x00, 0, 0, 0x00)...)
	blocks := ((w + 7) / 8) * ((h + 7) / 8)
	b = append(b, make([]byte, (4*blocks+7)/8)...) // each block codes the one-bit symbol 0
	return append(b, 0xFF, 0xD9)
}

// allocated returns how many bytes the process allocated while f ran,
// whether or not they were freed again.
func allocated(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// The fixtures are the logos the test is about: a 16-bit PNG and a progressive
// CMYK JPEG at the largest size the fetch allows. Their headers are read at
// full size, which costs nothing, and the same builders are decoded in full at
// a small size, which shows the files are real images and not bytes that happen
// to look like a header.
func TestLogoFixturesAreTheImagesTheyStandFor(t *testing.T) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(deepPNG(t, maxLogoDim, maxLogoDim)))
	if err != nil || format != "png" || cfg.Width != maxLogoDim || cfg.Height != maxLogoDim || cfg.ColorModel != color.NRGBA64Model {
		t.Errorf("16-bit png: config %+v, format %q, err %v", cfg, format, err)
	}
	img, _, err := image.Decode(bytes.NewReader(deepPNG(t, 64, 48)))
	if err != nil {
		t.Fatalf("a small 16-bit png does not decode: %v", err)
	}
	if _, ok := img.(*image.NRGBA64); !ok || img.Bounds().Dx() != 64 || img.Bounds().Dy() != 48 {
		t.Errorf("a small 16-bit png decoded to %T %v", img, img.Bounds())
	}

	cfg, format, err = image.DecodeConfig(bytes.NewReader(progressiveCMYK(maxLogoDim, maxLogoDim)))
	if err != nil || format != "jpeg" || cfg.Width != maxLogoDim || cfg.Height != maxLogoDim || cfg.ColorModel != color.CMYKModel {
		t.Errorf("cmyk jpeg: config %+v, format %q, err %v", cfg, format, err)
	}
	img, _, err = image.Decode(bytes.NewReader(progressiveCMYK(64, 48)))
	if err != nil {
		t.Fatalf("a small progressive cmyk jpeg does not decode: %v", err)
	}
	if _, ok := img.(*image.CMYK); !ok || img.Bounds().Dx() != 64 || img.Bounds().Dy() != 48 {
		t.Errorf("a small progressive cmyk jpeg decoded to %T %v", img, img.Bounds())
	}
}

func TestFetchAndValidateLogoDoesNotDecodeThePixels(t *testing.T) {
	const limit = 16 << 20
	cases := map[string]struct {
		contentType string
		body        []byte
	}{
		"16-bit png":            {"image/png", deepPNG(t, maxLogoDim, maxLogoDim)},
		"progressive cmyk jpeg": {"image/jpeg", progressiveCMYK(maxLogoDim, maxLogoDim)},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			url := serveLogo(t, tc.contentType, tc.body)
			client := logoClient()
			var got []byte
			var err error
			n := allocated(func() { got, err = FetchAndValidateLogo(context.Background(), client, url) })
			// A logo within every limit is kept: this is the fetch doing its
			// work, not a refusal that would also allocate little.
			if err != nil {
				t.Fatalf("an image within every limit was refused: %v", err)
			}
			if !bytes.Equal(got, tc.body) {
				t.Errorf("returned %d bytes, want the %d the server sent", len(got), len(tc.body))
			}
			if n > limit {
				t.Errorf("the fetch allocated %d MiB for a %d KiB file, over the %d MiB limit: the pixels were decoded",
					n>>20, len(tc.body)>>10, limit>>20)
			}
		})
	}
}
