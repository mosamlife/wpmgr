package pdf

import (
	"bytes"
	"context"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mosamlife/wpmgr/apps/api/internal/httpclient"
)

// The logo is the one input to a report that comes from somewhere else: the
// agency types a URL, and the worker fetches it. Reading only an image's header
// proves nothing about its body, so a file cut short in transit, or cut on
// purpose, used to pass the fetch and then fail the whole report when the PDF
// library read the pixels.

// gradientImage is an image with real pixel data, so a cut file really loses
// pixels. With translucent set, some pixels are not opaque and the PNG encoder
// writes an alpha channel.
func gradientImage(w, h int, translucent bool) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		for y := 0; y < h; y++ {
			a := uint8(255)
			if translucent && x < w/4 {
				a = uint8(40 + y)
			}
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(x * 6), G: uint8(y * 6), B: 90, A: a})
		}
	}
	return img
}

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func encodeJPEG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// noisyImage is an image that does not compress well, so an encoded copy has a
// body worth cutting. The generator is seeded, so every run cuts the same bytes.
func noisyImage(w, h int) *image.NRGBA {
	rng := rand.New(rand.NewSource(1))
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		for y := 0; y < h; y++ {
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(rng.Intn(256)), G: uint8(rng.Intn(256)), B: uint8(rng.Intn(256)), A: 255})
		}
	}
	return img
}

// cutTo keeps the first n bytes of an encoded image. The test setup insists
// that the header of the result still decodes and its body does not, which is
// exactly the file that used to get through.
func cutTo(t *testing.T, full []byte, n int) []byte {
	t.Helper()
	cut := full[:n]
	if _, _, err := image.DecodeConfig(bytes.NewReader(cut)); err != nil {
		t.Fatalf("test setup: the cut image should still decode its header: %v", err)
	}
	if _, _, err := image.Decode(bytes.NewReader(cut)); err == nil {
		t.Fatal("test setup: the cut image should not decode in full")
	}
	return cut
}

func truncatedAlphaPNG(t *testing.T) []byte {
	full := encodePNG(t, gradientImage(40, 40, true))
	return cutTo(t, full, len(full)/2)
}

// A JPEG the encoder writes has no JFIF segment, so reading its header goes on
// past the frame header to the start of the scan. The cut has to fall inside
// the scan for the header to decode.
func truncatedJPEG(t *testing.T) []byte {
	full := encodeJPEG(t, noisyImage(96, 96))
	return cutTo(t, full, len(full)*3/4)
}

// pngChunkCRC rewrites the CRC of the IHDR chunk, which starts at byte 8 of
// every PNG, after the signature.
func pngChunkCRC(b []byte) {
	const ihdrType, ihdrEnd = 12, 29
	binary.BigEndian.PutUint32(b[ihdrEnd:], crc32.ChecksumIEEE(b[ihdrType:ihdrEnd]))
}

// interlacedPNG is a PNG whose header says the image is interlaced, a form
// the PDF library does not take. Its pixels are not interlaced, so only code
// that never decodes it can tell.
func interlacedPNG(t *testing.T) []byte {
	t.Helper()
	b := encodePNG(t, gradientImage(40, 40, false))
	const interlaceByte = 28
	b[interlaceByte] = 1
	pngChunkCRC(b)
	return b
}

// headerOnlyPNG is the signature and the header chunk of a w x h, 8-bit RGB
// PNG, and nothing else.
func headerOnlyPNG(w, h uint32) []byte {
	b := []byte("\x89PNG\r\n\x1a\n")
	b = binary.BigEndian.AppendUint32(b, 13)
	b = append(b, "IHDR"...)
	b = binary.BigEndian.AppendUint32(b, w)
	b = binary.BigEndian.AppendUint32(b, h)
	b = append(b, 8, 2, 0, 0, 0) // 8 bits, RGB, deflate, adaptive filter, no interlace
	b = binary.BigEndian.AppendUint32(b, 0)
	pngChunkCRC(b)
	return b
}

// serveLogo serves body as contentType and returns the URL of the logo.
func serveLogo(t *testing.T, contentType string, body []byte) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/logo"
}

// logoClient reaches the test server on loopback, which the production client
// refuses; everything else about the fetch is the production path.
func logoClient() *httpclient.Client {
	return httpclient.New(httpclient.Config{AllowPrivateNetworks: true, Timeout: 5 * time.Second})
}

func TestFetchAndValidateLogoKeepsAWholeImage(t *testing.T) {
	cases := map[string]struct {
		contentType string
		body        []byte
	}{
		"png":       {"image/png", encodePNG(t, gradientImage(40, 40, false))},
		"alpha png": {"image/png", encodePNG(t, gradientImage(40, 40, true))},
		"jpeg":      {"image/jpeg", encodeJPEG(t, gradientImage(40, 40, false))},
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

func TestFetchAndValidateLogoRefusesAnImageWhoseBodyIsCut(t *testing.T) {
	cases := map[string]struct {
		contentType string
		body        []byte
	}{
		"png":       {"image/png", truncatedPNG(t)},
		"alpha png": {"image/png", truncatedAlphaPNG(t)},
		"jpeg":      {"image/jpeg", truncatedJPEG(t)},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := FetchAndValidateLogo(context.Background(), logoClient(), serveLogo(t, tc.contentType, tc.body))
			if err == nil {
				t.Fatalf("an image whose body is cut short was accepted (%d bytes)", len(got))
			}
			if got != nil {
				t.Errorf("returned %d bytes alongside an error", len(got))
			}
		})
	}
}

// The size limit is read from the header, before any pixel is decoded: a
// declared size past the limit never reaches a decoder that would allocate it.
func TestFetchAndValidateLogoRefusesOversizeFromTheHeader(t *testing.T) {
	for name, body := range map[string][]byte{
		"too wide": headerOnlyPNG(maxLogoDim+1, 10),
		"too tall": headerOnlyPNG(10, maxLogoDim+1),
	} {
		t.Run(name, func(t *testing.T) {
			got, err := FetchAndValidateLogo(context.Background(), logoClient(), serveLogo(t, "image/png", body))
			if err == nil {
				t.Fatalf("an oversize image was accepted (%d bytes)", len(got))
			}
			if !strings.Contains(err.Error(), "dimensions") {
				t.Errorf("error %q is not the size refusal: the image was decoded before its size was checked", err)
			}
			if got != nil {
				t.Errorf("returned %d bytes alongside an error", len(got))
			}
		})
	}
}

// The renderer reads a logo that is not a PNG as a JPEG, so an image in any
// other format the process can decode (the PDF library registers GIF) is refused
// at the fetch, whatever the server calls it.
func TestFetchAndValidateLogoRefusesAFormatTheReportCannotDraw(t *testing.T) {
	var b bytes.Buffer
	if err := gif.Encode(&b, gradientImage(40, 40, false), nil); err != nil {
		t.Fatal(err)
	}
	got, err := FetchAndValidateLogo(context.Background(), logoClient(), serveLogo(t, "image/png", b.Bytes()))
	if err == nil {
		t.Fatalf("a GIF served as image/png was accepted (%d bytes)", len(got))
	}
	if !strings.Contains(err.Error(), "format") {
		t.Errorf("error %q is not the format refusal", err)
	}
	if got != nil {
		t.Errorf("returned %d bytes alongside an error", len(got))
	}
}

func TestFetchAndValidateLogoRefusesBytesThatAreNotAnImage(t *testing.T) {
	got, err := FetchAndValidateLogo(context.Background(), logoClient(),
		serveLogo(t, "image/png", []byte("<html>not a picture</html>")))
	if err == nil {
		t.Fatalf("text served as image/png was accepted (%d bytes)", len(got))
	}
	if got != nil {
		t.Errorf("returned %d bytes alongside an error", len(got))
	}
}
