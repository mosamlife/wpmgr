package scan

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// checksumsFetchURL is the WordPress.org checksums API endpoint.
// JSON shape: {"checksums":{"<locale>":{"<path>":"<md5>"}}}
const checksumsFetchURL = "https://api.wordpress.org/core/checksums/1.0/?version=%s&locale=%s"

// positiveCacheTTL is how long we treat a successfully-fetched checksum set
// as fresh. WordPress releases are immutable so 30 days is safe.
const positiveCacheTTL = 30 * 24 * time.Hour

// negativeCacheTTL is how long we honour a 404/empty fetch failure before
// retrying. Intentionally short (6h) so a transient outage doesn't block
// scans permanently.
const negativeCacheTTL = 6 * time.Hour

// HTTPDoer is the subset of httpclient.Client the ChecksumProvider needs.
// Using an interface keeps the scan package free of a direct httpclient import
// (the concrete *httpclient.Client is wired in main).
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// ChecksumProvider fetches and caches WordPress.org core checksums.
// Thread-safe: no mutable in-process state — freshness is stored in Postgres.
type ChecksumProvider struct {
	repo   *Repo
	client HTTPDoer
}

// NewChecksumProvider builds a ChecksumProvider backed by the given Repo and
// SSRF-safe HTTP client.
func NewChecksumProvider(repo *Repo, client HTTPDoer) *ChecksumProvider {
	return &ChecksumProvider{repo: repo, client: client}
}

// Core returns the known-good checksum map for the given WordPress version and
// locale. The map keys are relative core file paths (e.g. "wp-login.php",
// "wp-admin/about.php"); values are 32-char lowercase hex MD5 strings.
//
// Caching strategy:
//   - On cache hit (positive, <30d): return stored rows immediately.
//   - On negative-cache hit (<6h):   return nil map (no checksums available).
//   - On cache miss / stale:         fetch from api.wordpress.org, store, return.
//   - locale fallback:               if the requested locale fails, retry en_US.
//   - NEVER hard-fails:              a fetch error returns an empty map + logged error.
func (p *ChecksumProvider) Core(ctx context.Context, version, locale string) (map[string]string, error) {
	if locale == "" {
		locale = "en_US"
	}
	result, err := p.coreForLocale(ctx, version, locale)
	if err != nil || (result == nil && locale != "en_US") {
		// Locale-specific fetch failed or returned empty — fall back to en_US.
		result, err = p.coreForLocale(ctx, version, "en_US")
	}
	return result, err
}

// coreForLocale fetches/returns checksums for a specific version+locale pair.
func (p *ChecksumProvider) coreForLocale(ctx context.Context, version, locale string) (map[string]string, error) {
	// --- check meta (positive / negative cache) ---
	fetchedAt, ok, found, err := p.repo.GetChecksumsMeta(ctx, version, locale)
	if err != nil {
		// DB error — degrade gracefully (empty map, no hard failure).
		return nil, nil //nolint:nilerr
	}
	if found {
		age := time.Since(fetchedAt)
		if !ok && age < negativeCacheTTL {
			// Negative cache still fresh: return empty map (no known-good checksums).
			return map[string]string{}, nil
		}
		if ok && age < positiveCacheTTL {
			// Positive cache still fresh: load from DB.
			rows, rerr := p.repo.GetChecksums(ctx, version, locale)
			if rerr != nil {
				return nil, nil //nolint:nilerr
			}
			return rowsToMap(rows), nil
		}
	}

	// --- cache miss or stale: fetch from api.wordpress.org ---
	checksums, fetchErr := p.fetchFromWPOrg(ctx, version, locale)
	if fetchErr != nil || len(checksums) == 0 {
		// Negative-cache the failure so we don't hammer wp.org on every scan.
		_ = p.repo.UpsertChecksumsMeta(ctx, version, locale, false)
		return map[string]string{}, nil
	}

	// Persist checksums + meta (positive cache).
	dbRows := make([]ChecksumRow, 0, len(checksums))
	for path, md5 := range checksums {
		dbRows = append(dbRows, ChecksumRow{Path: path, MD5: md5})
	}
	_ = p.repo.UpsertChecksums(ctx, version, locale, dbRows)
	_ = p.repo.UpsertChecksumsMeta(ctx, version, locale, true)

	return checksums, nil
}

// fetchFromWPOrg hits the wp.org checksums API and returns the flat map.
// The JSON shape is: {"checksums":{"<locale>":{"<path>":"<md5>"}}}
func (p *ChecksumProvider) fetchFromWPOrg(ctx context.Context, version, locale string) (map[string]string, error) {
	rawURL := fmt.Sprintf(checksumsFetchURL, version, locale)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build wp.org request: %w", err)
	}
	req.Header.Set("User-Agent", "WPMgr-ScanChecksums/1.0")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch wp.org checksums: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 8<<20))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil // 404 → negative-cache
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("wp.org checksums returned HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("read wp.org body: %w", err)
	}

	// The API wraps under "checksums" then locale.
	var envelope struct {
		Checksums map[string]json.RawMessage `json:"checksums"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("decode wp.org checksums: %w", err)
	}

	// Try the requested locale first, then en_US if absent.
	for _, try := range []string{locale, "en_US"} {
		raw, ok := envelope.Checksums[try]
		if !ok || len(raw) == 0 {
			continue
		}
		var m map[string]string
		if err := json.Unmarshal(raw, &m); err != nil {
			continue
		}
		// Normalize: strip leading slashes.
		out := make(map[string]string, len(m))
		for path, md5 := range m {
			out[strings.TrimLeft(path, "/")] = strings.ToLower(md5)
		}
		return out, nil
	}
	return nil, nil // empty response → negative-cache
}

func rowsToMap(rows []ChecksumRow) map[string]string {
	m := make(map[string]string, len(rows))
	for _, r := range rows {
		m[r.Path] = r.MD5
	}
	return m
}
