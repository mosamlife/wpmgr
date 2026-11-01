package pdf

import (
	"bytes"
	"context"
	"testing"
)

// The fetch checks that a logo runs to its end by walking the structure of the
// file: the chunks of a PNG, the segments and scans of a JPEG. These tests hold
// that walk to two things. It finds a file cut anywhere, and it does not refuse
// a file for anything but stopping early.

// pngWithoutIEND is a whole PNG less its last chunk, the 12 bytes of IEND. The
// header decodes, and so does every chunk that is left.
func pngWithoutIEND(t *testing.T) []byte {
	t.Helper()
	full := encodePNG(t, gradientImage(40, 40, false))
	return cutTo(t, full, len(full)-12)
}

// jpegWithoutEOI is a whole JPEG less its last two bytes, the end-of-image
// marker.
func jpegWithoutEOI(t *testing.T, full []byte) []byte {
	t.Helper()
	return cutTo(t, full, len(full)-2)
}

// withThumbnail is a JPEG whose first segment after the start-of-image marker
// is an APP1 carrying a whole smaller JPEG, as the EXIF block of a camera's
// file does. The thumbnail has an end-of-image marker inside that segment.
func withThumbnail(t *testing.T, jpg []byte) []byte {
	t.Helper()
	thumb := encodeJPEG(t, gradientImage(8, 8, false))
	app1 := jpegSegment(0xE1, append([]byte("Exif\x00\x00"), thumb...)...)
	out := append([]byte{}, jpg[:2]...)
	out = append(out, app1...)
	return append(out, jpg[2:]...)
}

// onlyTheWholeFileReachesItsEnd checks reaches against every prefix of full:
// the whole file reaches its end and no shorter prefix does. A file cut at any
// byte, in a header, a data chunk, a scan or a marker, is found.
func onlyTheWholeFileReachesItsEnd(t *testing.T, full []byte, reaches func([]byte) bool) {
	t.Helper()
	if !reaches(full) {
		t.Fatalf("the whole file of %d bytes does not reach its end", len(full))
	}
	for n := 0; n < len(full); n++ {
		if reaches(full[:n]) {
			t.Fatalf("the first %d of %d bytes reach the end", n, len(full))
		}
	}
}

func TestPNGReachesIENDOnlyWhenWhole(t *testing.T) {
	t.Run("encoded images", func(t *testing.T) {
		images := map[string][]byte{
			"gradient":         encodePNG(t, gradientImage(40, 40, false)),
			"alpha":            encodePNG(t, gradientImage(40, 40, true)),
			"noise, in chunks": encodePNG(t, noisyImage(150, 150)),
			"16-bit":           deepPNG(t, 64, 48),
		}
		for name, full := range images {
			t.Run(name, func(t *testing.T) { onlyTheWholeFileReachesItsEnd(t, full, pngReachesIEND) })
		}
	})

	sig := []byte("\x89PNG\r\n\x1a\n")
	file := func(parts ...[]byte) []byte { return append(append([]byte{}, sig...), bytes.Join(parts, nil)...) }
	ihdr := pngChunk("IHDR", make([]byte, 13))
	idat := pngChunk("IDAT", []byte{1, 2, 3, 4, 5})
	iend := pngChunk("IEND", nil)
	whole := file(ihdr, idat, iend)
	cases := []struct {
		name string
		data []byte
		want bool
	}{
		{"header, data, end", whole, true},
		{"ancillary chunks and data in several chunks", file(ihdr, pngChunk("tEXt", []byte("k\x00v")), idat, idat, iend), true},
		{"bytes after IEND", append(append([]byte{}, whole...), "trailing bytes"...), true},
		{"no IEND", file(ihdr, idat), false},
		{"IEND cut before its CRC", whole[:len(whole)-2], false},
		{"a chunk longer than the rest of the file", file(ihdr, []byte{0x00, 0x00, 0x10, 0x00, 'I', 'D', 'A', 'T', 1, 2, 3}), false},
		{"a chunk of almost 4 GB", file(ihdr, []byte{0xFF, 0xFF, 0xFF, 0xFF, 'I', 'D', 'A', 'T', 1, 2, 3, 4}), false},
		{"the signature alone", file(), false},
		{"nothing", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pngReachesIEND(tc.data); got != tc.want {
				t.Errorf("pngReachesIEND = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestJPEGReachesEOIOnlyWhenWhole(t *testing.T) {
	t.Run("encoded images", func(t *testing.T) {
		images := map[string][]byte{
			"gradient":                     encodeJPEG(t, gradientImage(40, 40, false)),
			"noise, with stuffed bytes":    encodeJPEG(t, noisyImage(96, 96)),
			"thumbnail in a segment":       withThumbnail(t, encodeJPEG(t, noisyImage(96, 96))),
			"progressive, four components": progressiveCMYK(64, 48),
		}
		for name, full := range images {
			t.Run(name, func(t *testing.T) { onlyTheWholeFileReachesItsEnd(t, full, jpegReachesEOI) })
		}
	})

	join := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
	soi, eoi := []byte{0xFF, 0xD8}, []byte{0xFF, 0xD9}
	sos := jpegSegment(0xDA, 0xAA, 0xBB)
	dht := jpegSegment(0xC4, 0xCC, 0xDD)
	cases := []struct {
		name string
		data []byte
		want bool
	}{
		{"the two markers", join(soi, eoi), true},
		{"a scan with stuffed bytes, restart markers and fill bytes",
			join(soi, sos, []byte{0x12, 0xFF, 0x00, 0x34, 0xFF, 0xD0, 0x56, 0xFF, 0xD7, 0x78, 0xFF, 0xFF}, eoi), true},
		{"two scans", join(soi, sos, []byte{1, 2}, dht, sos, []byte{3, 4}, eoi), true},
		{"bytes after the end marker", join(soi, sos, []byte{1}, eoi, []byte{0, 0, 0xFF, 0xD9, 'x'}), true},
		{"no end marker", join(soi, sos, []byte{1, 2, 3}), false},
		{"a scan that stops after a stuffed byte", join(soi, sos, []byte{1, 0xFF, 0x00}), false},
		{"a scan that stops after a restart marker", join(soi, sos, []byte{1, 0xFF, 0xD3}), false},
		{"a scan that stops at a 0xFF", join(soi, sos, []byte{1, 0xFF}), false},
		{"a segment longer than the rest of the file", join(soi, []byte{0xFF, 0xE0, 0x00, 0x20, 1, 2}), false},
		{"a segment whose length is cut", join(soi, []byte{0xFF, 0xE0, 0x00}), false},
		{"a segment shorter than its own length", join(soi, []byte{0xFF, 0xE0, 0x00, 0x01}, eoi), false},
		{"an end marker inside a segment", join(soi, jpegSegment(0xE1, 'E', 'x', 0xFF, 0xD9, 'i', 'f')), false},
		{"the start marker alone", soi, false},
		{"one byte", []byte{0xFF}, false},
		{"nothing", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := jpegReachesEOI(tc.data); got != tc.want {
				t.Errorf("jpegReachesEOI = %v, want %v", got, tc.want)
			}
		})
	}
}

// Bytes after the end of an image are not the fetch's business: decoders stop
// at the end, and some tools pad or append. A file is refused for stopping
// early, never for going on.
func TestFetchAndValidateLogoKeepsAnImageWithBytesAfterItsEnd(t *testing.T) {
	cases := map[string]struct {
		contentType string
		body        []byte
	}{
		"png":                       {"image/png", append(encodePNG(t, gradientImage(40, 40, false)), "trailing bytes"...)},
		"jpeg, zero padded":         {"image/jpeg", append(encodeJPEG(t, gradientImage(40, 40, false)), 0, 0, 0, 0)},
		"jpeg, with text after":     {"image/jpeg", append(encodeJPEG(t, gradientImage(40, 40, false)), "trailing bytes"...)},
		"jpeg with a thumbnail":     {"image/jpeg", withThumbnail(t, encodeJPEG(t, noisyImage(96, 96)))},
		"progressive cmyk jpeg":     {"image/jpeg", progressiveCMYK(64, 48)},
		"16-bit png":                {"image/png", deepPNG(t, 64, 48)},
		"a second image after jpeg": {"image/jpeg", append(encodeJPEG(t, gradientImage(40, 40, false)), encodeJPEG(t, gradientImage(8, 8, false))...)},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := FetchAndValidateLogo(context.Background(), logoClient(), serveLogo(t, tc.contentType, tc.body))
			if err != nil {
				t.Fatalf("a whole image was refused: %v", err)
			}
			if !bytes.Equal(got, tc.body) {
				t.Errorf("returned %d bytes, want the %d the server sent", len(got), len(tc.body))
			}
		})
	}
}
