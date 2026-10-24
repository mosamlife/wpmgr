//go:build cgo

package encoder

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"

	"github.com/discord/lilliput"
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

func TestLilliputEncode_GreyscalePixels(t *testing.T) {
	const width, height = 97, 65
	grey := image.NewGray(image.Rect(0, 0, width, height))
	rgb := image.NewRGBA(grey.Bounds())
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			v := uint8(160*x/(width-1) + 80*y/(height-1))
			grey.SetGray(x, y, color.Gray{Y: v})
			rgb.SetRGBA(x, y, color.RGBA{R: v, G: v, B: v, A: 255})
		}
	}
	enc := NewLilliputEncoder(1)
	defer enc.Close()
	for _, tc := range []struct {
		name     string
		img      image.Image
		png      bool
		alpha    bool
		oriented bool
		channels int
	}{
		{name: "grey_jpeg", img: grey, channels: 1},
		{name: "rgb_jpeg", img: rgb, channels: 3},
		{name: "grey_png", img: grey, png: true, channels: 1},
		{name: "grey_alpha_png", img: grey, png: true, alpha: true, channels: 4},
		{name: "grey_jpeg_exif", img: grey, oriented: true, channels: 1},
	} {
		var source bytes.Buffer
		if tc.alpha {
			source.Write(makeGreyAlphaPNG(t, grey))
		} else if tc.png {
			if err := png.Encode(&source, tc.img); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := jpeg.Encode(&source, tc.img, &jpeg.Options{Quality: 95}); err != nil {
				t.Fatal(err)
			}
		}
		encoded := source.Bytes()
		if tc.oriented {
			// APP1 contains a little-endian TIFF with orientation 6 (90 degrees clockwise).
			exif := []byte{
				0xff, 0xe1, 0, 34, 'E', 'x', 'i', 'f', 0, 0,
				'I', 'I', 42, 0, 8, 0, 0, 0, 1, 0,
				0x12, 1, 3, 0, 1, 0, 0, 0, 6, 0, 0, 0, 0, 0, 0, 0,
			}
			encoded = append(append(append([]byte{}, encoded[:2]...), exif...), encoded[2:]...)
		}
		want, _, err := image.Decode(bytes.NewReader(encoded))
		if err != nil {
			t.Fatal(err)
		}
		decoder, err := lilliput.NewDecoder(encoded)
		if err != nil {
			t.Fatal(err)
		}
		header, err := decoder.Header()
		decoder.Close()
		if err != nil {
			t.Fatal(err)
		}
		if channels := header.PixelType().Channels(); channels != tc.channels {
			t.Fatalf("%s: source channels = %d, want %d", tc.name, channels, tc.channels)
		}
		if tc.oriented {
			if header.Orientation() != lilliput.OrientationRightTop {
				t.Fatalf("source orientation = %v, want right-top", header.Orientation())
			}
			rotated := image.NewNRGBA(image.Rect(0, 0, height, width))
			for y := 0; y < height; y++ {
				for x := 0; x < width; x++ {
					rotated.Set(height-1-y, x, want.At(x, y))
				}
			}
			want = rotated
		}
		for _, target := range []string{"avif", "webp", "original"} {
			t.Run(tc.name+"/"+target, func(t *testing.T) {
				res, err := enc.Encode(context.Background(), EncodeRequest{
					Source: encoded, TargetFormat: target,
				})
				if err != nil {
					t.Fatal(err)
				}
				got := decodeEncodedPixels(t, res.Output)
				assertPixelsClose(t, got, want)
			})
		}
	}
}

func TestLilliputEncode_GreyscaleCapacity(t *testing.T) {
	const poolDim = 24
	previousCapacity := imageOpsBufferBytes
	imageOpsBufferBytes = poolDim * poolDim * 4
	t.Cleanup(func() { imageOpsBufferBytes = previousCapacity })

	pool := make(chan *lilliput.ImageOps, 1)
	pool <- lilliput.NewImageOps(poolDim)
	enc := &LilliputEncoder{pool: pool}
	defer enc.Close()
	for _, tc := range []struct {
		name          string
		width, height int
		wantErr       bool
	}{
		{name: "just_over_capacity", width: imageOpsBufferBytes/3 + 1, height: 1, wantErr: true},
		{name: "exact_capacity", width: 32, height: 24},
	} {
		t.Run(tc.name, func(t *testing.T) {
			grey := image.NewGray(image.Rect(0, 0, tc.width, tc.height))
			for y := 0; y < tc.height; y++ {
				for x := 0; x < tc.width; x++ {
					grey.SetGray(x, y, color.Gray{Y: uint8(160*x/max(1, tc.width-1) + 80*y/max(1, tc.height-1))})
				}
			}
			var source bytes.Buffer
			if err := png.Encode(&source, grey); err != nil {
				t.Fatal(err)
			}
			res, err := enc.Encode(context.Background(), EncodeRequest{Source: source.Bytes(), TargetFormat: "avif"})
			if !tc.wantErr {
				if err != nil {
					t.Fatal(err)
				}
				assertPixelsClose(t, decodeEncodedPixels(t, res.Output), grey)
				return
			}
			if !errors.Is(err, ErrDimensionsTooBig) {
				t.Errorf("encode error = %v, want ErrDimensionsTooBig", err)
			}

			decoder, err := lilliput.NewDecoder(source.Bytes())
			if err != nil {
				t.Fatal(err)
			}
			defer decoder.Close()
			if _, err := decoder.Header(); err != nil {
				t.Fatal(err)
			}
			probe := &decodeProbe{Decoder: decoder}
			adapter := &bgrDecoder{Decoder: probe}
			dst := lilliput.NewFramebuffer(poolDim, poolDim)
			defer dst.Close()
			// Header may allocate its metadata object; rejection must add no
			// allocations for expansion beyond that baseline.
			headerAllocs := testing.AllocsPerRun(1, func() { _, err = probe.Header() })
			if err != nil {
				t.Fatal(err)
			}
			allocs := testing.AllocsPerRun(1, func() { err = adapter.DecodeTo(dst) })
			if !errors.Is(err, ErrDimensionsTooBig) {
				t.Errorf("adapter error = %v, want ErrDimensionsTooBig", err)
			}
			if probe.calls != 0 || allocs != headerAllocs {
				t.Errorf("rejected expansion: decode calls = %d, expansion allocations = %g; want none", probe.calls, allocs-headerAllocs)
			}
		})
	}
}

type decodeProbe struct {
	lilliput.Decoder
	calls int
}

func (d *decodeProbe) DecodeTo(*lilliput.Framebuffer) error {
	d.calls++
	return errors.New("pixel decoding reached")
}

// makeGreyAlphaPNG writes PNG colour type 4: 8-bit grey plus alpha. Go's PNG
// encoder writes NRGBA fixtures as colour type 6, so the channels are explicit here.
func makeGreyAlphaPNG(t *testing.T, grey *image.Gray) []byte {
	t.Helper()
	var file, compressed bytes.Buffer
	file.WriteString("\x89PNG\r\n\x1a\n")
	chunk := func(kind string, data []byte) {
		if err := binary.Write(&file, binary.BigEndian, uint32(len(data))); err != nil {
			t.Fatal(err)
		}
		file.WriteString(kind)
		file.Write(data)
		crc := crc32.NewIEEE()
		crc.Write([]byte(kind))
		crc.Write(data)
		if err := binary.Write(&file, binary.BigEndian, crc.Sum32()); err != nil {
			t.Fatal(err)
		}
	}
	header := make([]byte, 13)
	binary.BigEndian.PutUint32(header, uint32(grey.Bounds().Dx()))
	binary.BigEndian.PutUint32(header[4:], uint32(grey.Bounds().Dy()))
	header[8], header[9] = 8, 4
	chunk("IHDR", header)
	writer := zlib.NewWriter(&compressed)
	for y := 0; y < grey.Bounds().Dy(); y++ {
		row := []byte{0} // PNG filter: none.
		for x := 0; x < grey.Bounds().Dx(); x++ {
			row = append(row, grey.GrayAt(x, y).Y, uint8(64+160*y/(grey.Bounds().Dy()-1)))
		}
		if _, err := writer.Write(row); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	chunk("IDAT", compressed.Bytes())
	chunk("IEND", nil)
	return file.Bytes()
}

func decodeEncodedPixels(t *testing.T, encoded []byte) image.Image {
	t.Helper()
	decoder, err := lilliput.NewDecoder(encoded)
	if err != nil {
		t.Fatal(err)
	}
	defer decoder.Close()
	header, err := decoder.Header()
	if err != nil {
		t.Fatal(err)
	}
	frame := lilliput.NewFramebuffer(header.Width(), header.Height())
	defer frame.Close()
	if err := decoder.DecodeTo(frame); err != nil {
		t.Fatal(err)
	}
	writer, err := lilliput.NewEncoder(".png", decoder, make([]byte, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	pixels, err := writer.Encode(frame, nil)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(pixels))
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func assertPixelsClose(t *testing.T, got, want image.Image) {
	t.Helper()
	if got.Bounds() != want.Bounds() {
		t.Fatalf("decoded bounds = %v, want %v", got.Bounds(), want.Bounds())
	}
	var total, maxDelta int
	for y := want.Bounds().Min.Y; y < want.Bounds().Max.Y; y++ {
		for x := want.Bounds().Min.X; x < want.Bounds().Max.X; x++ {
			g := color.NRGBAModel.Convert(got.At(x, y)).(color.NRGBA)
			w := color.NRGBAModel.Convert(want.At(x, y)).(color.NRGBA)
			for i, gv := range []uint8{g.R, g.G, g.B, g.A} {
				wv := []uint8{w.R, w.G, w.B, w.A}[i]
				delta := int(gv) - int(wv)
				if delta < 0 {
					delta = -delta
				}
				total += delta
				if delta > maxDelta {
					maxDelta = delta
				}
			}
		}
	}
	mean := float64(total) / float64(want.Bounds().Dx()*want.Bounds().Dy()*4)
	if mean > 8 || maxDelta > 35 {
		t.Errorf("pixel error: mean %.2f (limit 8), max %d (limit 35)", mean, maxDelta)
	}
}
