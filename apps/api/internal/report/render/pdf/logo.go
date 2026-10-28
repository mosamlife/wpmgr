// Package pdf contains the PDF renderer for client reports.
// This file handles logo fetching and validation — the ONLY untrusted input
// path per the locked build caveats. Any failure returns (nil, err) and the
// caller renders without a logo.
package pdf

import (
	"bytes"
	"context"
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

	// Then every pixel. A header can be intact on a file whose body was cut
	// short, and nothing downstream notices until the PDF library reads the
	// pixels, when it fails the whole report.
	if _, _, err := image.Decode(byteReader(data)); err != nil {
		return nil, fmt.Errorf("logo decode: %w", err)
	}

	return data, nil
}
