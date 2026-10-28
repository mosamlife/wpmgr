// Package pdf contains the PDF renderer for client reports.
// This file handles logo fetching and validation — the ONLY untrusted input
// path per the locked build caveats. Any failure returns (nil, err) and the
// caller renders without a logo.
package pdf

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"image"
	_ "image/jpeg" // register jpeg decoder
	_ "image/png"  // register png decoder
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/mosamlife/wpmgr/apps/api/internal/httpclient"
)

// byteReader wraps a byte slice as an io.Reader.
func byteReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }

const (
	maxLogoBytes = 2 << 20 // 2 MB
	maxLogoDim   = 4000    // pixels per dimension
)

// FetchAndValidateLogo fetches and validates the logo at rawURL.
// Returns (nil, err) on ANY failure; callers should log Warn and render
// without the logo. Never fails the report.
func FetchAndValidateLogo(ctx context.Context, client *httpclient.Client, rawURL string) ([]byte, error) {
	if rawURL == "" {
		return nil, nil //nolint:nilerr // no logo to fetch
	}
	// Scheme must be http or https only.
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("logo url parse: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return nil, fmt.Errorf("logo url scheme %q not allowed (http/https only)", u.Scheme)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("logo fetch request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("logo fetch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("logo fetch: status %d", resp.StatusCode)
	}

	// Content-Type allowlist.
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	// Strip parameters.
	if i := strings.Index(ct, ";"); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	if ct != "image/png" && ct != "image/jpeg" {
		return nil, fmt.Errorf("logo content-type %q not allowed (image/png or image/jpeg only)", ct)
	}

	// Size cap: read up to 2MB+1; reject if larger.
	limited := io.LimitReader(resp.Body, maxLogoBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("logo read: %w", err)
	}
	if int64(len(data)) > maxLogoBytes {
		return nil, fmt.Errorf("logo exceeds %d byte size cap", maxLogoBytes)
	}

	// The header comes first: it carries the format and the size, and an image
	// past the size cap is refused here, before a decoder allocates its pixels.
	cfg, format, err := image.DecodeConfig(byteReader(data))
	if err != nil {
		return nil, fmt.Errorf("logo decode config: %w", err)
	}
	if format != "png" && format != "jpeg" {
		return nil, fmt.Errorf("logo format %q not allowed (png or jpeg only)", format)
	}
	if cfg.Width > maxLogoDim || cfg.Height > maxLogoDim {
		return nil, fmt.Errorf("logo dimensions %dx%d exceed %d px cap", cfg.Width, cfg.Height, maxLogoDim)
	}

	// Then the end of the file. A header can be intact on a file whose body was
	// cut short, and a file cut short is not a logo. Only the structure of the
	// file is read: the pixels are never decoded here, because the size of a
	// decoded image follows from its dimensions and bit depth and not from the
	// size of the file, and this process renders reports for every tenant. A
	// logo the PDF library cannot draw is left out by the renderer.
	reachesEnd := pngReachesIEND
	if format == "jpeg" {
		reachesEnd = jpegReachesEOI
	}
	if !reachesEnd(data) {
		return nil, fmt.Errorf("logo %s is cut short", format)
	}

	return data, nil
}

// pngReachesIEND reports whether the chunks of a PNG run unbroken from the
// signature to the IEND chunk. A chunk is a four-byte length, a four-byte type,
// that many bytes of data and a four-byte CRC, so the walk steps from one
// chunk to the next without reading any pixel. Bytes after IEND are ignored,
// as a decoder ignores them.
func pngReachesIEND(data []byte) bool {
	pos := uint64(8) // the signature, which DecodeConfig has checked
	for pos+12 <= uint64(len(data)) {
		end := pos + 12 + uint64(binary.BigEndian.Uint32(data[pos:]))
		if end > uint64(len(data)) {
			return false
		}
		if string(data[pos+4:pos+8]) == "IEND" {
			return true
		}
		pos = end
	}
	return false
}

// jpegReachesEOI reports whether the segments of a JPEG run unbroken from the
// start-of-image marker to the end-of-image marker. A segment is a marker and
// a length that counts its own two bytes. The entropy-coded data of a scan has
// no length: inside it a 0xFF is followed by 0x00 or a restart marker, so the
// next marker that is anything else ends it. The walk steps over each segment
// and each scan without reading any pixel, and an end-of-image marker inside a
// segment (a thumbnail's, say) is skipped with the segment. Bytes after the
// end-of-image marker are ignored, as a decoder ignores them.
func jpegReachesEOI(data []byte) bool {
	pos := 2 // the start-of-image marker, which DecodeConfig has checked
	if len(data) < pos {
		return false
	}
	for {
		// The next marker is a 0xFF and the byte after it, past any fill bytes.
		i := bytes.IndexByte(data[pos:], 0xFF)
		if i < 0 {
			return false
		}
		pos += i
		for pos < len(data) && data[pos] == 0xFF {
			pos++
		}
		if pos == len(data) {
			return false
		}
		marker := data[pos]
		pos++
		switch {
		case marker == 0xD9: // EOI
			return true
		case marker == 0x00, marker == 0x01, marker >= 0xD0 && marker <= 0xD8:
			// A stuffed 0xFF, TEM, RSTn and SOI have no length.
			continue
		}
		if pos+2 > len(data) {
			return false
		}
		size := int(binary.BigEndian.Uint16(data[pos:]))
		if size < 2 || pos+size > len(data) {
			return false
		}
		pos += size
	}
}
