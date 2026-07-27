package scan

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// pluginSlugRe is the allowlist for agent-supplied plugin/theme slugs before
// they are interpolated into the wp.org fetch URL. Accepts the form used by
// all wp.org-hosted slugs: starts with an alphanumeric, then any combination
// of alphanumerics, dots, underscores, and hyphens.
// This is a defence-in-depth guard; the ssrfClient already prevents host
// injection, but we reject clearly-malformed slugs early.
var pluginSlugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// pluginVersionRe is the allowlist for agent-supplied plugin/theme version
// strings. Accepts semver and WordPress-style version strings.
var pluginVersionRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

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
	logger *slog.Logger
}

// NewChecksumProvider builds a ChecksumProvider backed by the given Repo and
// SSRF-safe HTTP client. logger may be nil, in which case slog.Default() is
// used (matching NewScanRunWorker/NewHashGCWorker's convention).
func NewChecksumProvider(repo *Repo, client HTTPDoer, logger *slog.Logger) *ChecksumProvider {
	if logger == nil {
		logger = slog.Default()
	}
	return &ChecksumProvider{repo: repo, client: client, logger: logger}
}

// log returns a usable logger even when a ChecksumProvider was built via a
// bare struct literal (as the package's tests do) rather than through
// NewChecksumProvider.
func (p *ChecksumProvider) log() *slog.Logger {
	if p.logger != nil {
		return p.logger
	}
	return slog.Default()
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

// pluginChecksumsFetchURL is the wp.org plugin-checksums endpoint.
// The slug is the directory name of the plugin (first path segment of the
// plugin file path, e.g. "akismet" from "akismet/akismet.php").
// Shape: {"plugin":"akismet","version":"x.y.z","files":{"path":{"md5":"…"|["…","…"],"sha256":…}}}
const pluginChecksumsFetchURL = "https://downloads.wordpress.org/plugin-checksums/%s/%s.json"

// Plugin returns the set of accepted md5 variants per plugin-relative file
// path for a wp.org-hosted plugin or theme. kind must be "plugin" or "theme".
// The returned map is path → []md5 (multiple accepted variants; match ANY).
//
// A 404 or non-2xx response is negative-cached for negativeCacheTTL so a
// premium/unknown plugin (no public checksums) does not hammer wp.org.
// Errors never hard-fail: they return an empty map (graceful degradation so
// the diff falls through to baseline-only for that slug).
//
// Theme note: the wp.org plugin-checksums endpoint currently only covers
// plugins (404 for themes). When a theme slug 404s, this method negative-caches
// it and returns an empty map; the diff falls through to baseline-only for
// that theme — which is the documented Phase 2 limitation for themes.
func (p *ChecksumProvider) Plugin(ctx context.Context, kind, slug, version string) (map[string][]string, error) {
	// Check freshness meta (positive / negative cache).
	fetchedAt, ok, found, err := p.repo.GetPluginChecksumsMeta(ctx, kind, slug, version)
	if err != nil {
		return nil, nil //nolint:nilerr — DB error: degrade gracefully
	}
	if found {
		age := time.Since(fetchedAt)
		if !ok && age < negativeCacheTTL {
			// Negative cache still fresh: no public checksums available.
			return map[string][]string{}, nil
		}
		if ok && age < positiveCacheTTL {
			// Positive cache: load from DB.
			rows, rerr := p.repo.GetPluginChecksums(ctx, kind, slug, version)
			if rerr != nil {
				return nil, nil //nolint:nilerr
			}
			return pluginRowsToMap(rows), nil
		}
	}

	// Cache miss or stale: fetch from wp.org.
	// Themes use the same endpoint pattern; if wp.org returns 404, we
	// negative-cache and fall through to baseline-only.
	variantsByPath, fetchErr := p.fetchPluginFromWPOrg(ctx, slug, version)
	if fetchErr != nil || len(variantsByPath) == 0 {
		_ = p.repo.UpsertPluginChecksumsMeta(ctx, kind, slug, version, false)
		return map[string][]string{}, nil
	}

	// checksums is the returned path → []md5 map; unchanged shape/content from
	// before m106 (comparison logic elsewhere still consumes md5 only).
	checksums := make(map[string][]string, len(variantsByPath))
	// Persist all variants (md5 in PK so each variant is a separate row);
	// sha256 rides alongside md5 on the same row, not compared against yet.
	var dbRows []PluginChecksumRow
	for path, variants := range variantsByPath {
		md5s := make([]string, 0, len(variants))
		for _, v := range variants {
			md5s = append(md5s, v.MD5)
			dbRows = append(dbRows, PluginChecksumRow{
				Kind:    kind,
				Slug:    slug,
				Version: version,
				Path:    path,
				MD5:     v.MD5,
				SHA256:  v.SHA256,
			})
		}
		checksums[path] = md5s
	}
	if upsertErr := p.repo.UpsertPluginChecksums(ctx, dbRows); upsertErr != nil {
		// Do NOT stamp the positive freshness cache when the persist failed:
		// doing so would mark this (kind, slug, version) fresh for the full
		// 30-day positive-cache TTL over zero (or partially) written rows,
		// silently disabling plugin file-integrity checksum comparison with
		// no log and no finding until the cache expires. Leaving the meta row
		// untouched means the next scan retries the fetch+persist instead.
		p.log().Error("scan: failed to persist wp.org plugin checksums",
			slog.String("kind", kind),
			slog.String("slug", slug),
			slog.String("version", version),
			slog.Int("row_count", len(dbRows)),
			slog.Any("error", upsertErr))
		return checksums, nil
	}
	_ = p.repo.UpsertPluginChecksumsMeta(ctx, kind, slug, version, true)

	return checksums, nil
}

// pluginChecksumAPIResponse is the shape returned by the wp.org
// plugin-checksums endpoint. The md5 and sha256 fields may each be a string OR
// an array of strings (multiple accepted variants for line-ending / build
// differences); when both are arrays they are parallel, so index i of sha256
// is the stronger hash for the same file content as index i of md5.
//
// Trust-model rule (program-wide, applies beyond this file): the agent only
// computes md5_file(), so md5 is a NEGATIVE filter only, meaning "hashes
// differ, therefore the file must be analyzed further." An md5 match must
// never by itself grant a file positive trust (auto-clearing it as
// known-good), because MD5 has cheap chosen-prefix collisions. Any future
// positive-trust / auto-clear decision must be based on a stronger hash such
// as this sha256.
//
// This change (m106) only stops discarding sha256 so it is persisted for a
// later phase to use. It does not change any comparison/diff logic here, and
// nothing in this codebase reads PluginChecksumRow.SHA256 yet.
type pluginChecksumAPIResponse struct {
	Files map[string]struct {
		MD5    json.RawMessage `json:"md5"`
		SHA256 json.RawMessage `json:"sha256"`
	} `json:"files"`
}

// pluginFileVariant pairs one accepted md5 variant of a file with its
// wp.org-reported sha256 counterpart (same array index in the source JSON).
// SHA256 is "" when wp.org did not report one for that variant.
type pluginFileVariant struct {
	MD5    string
	SHA256 string
}

// fetchPluginFromWPOrg fetches plugin checksums from downloads.wordpress.org
// and decodes the multi-variant md5+sha256 shape into path → []pluginFileVariant.
func (p *ChecksumProvider) fetchPluginFromWPOrg(ctx context.Context, slug, version string) (map[string][]pluginFileVariant, error) {
	// Defensive allowlist: reject malformed agent-supplied slug/version before
	// interpolating them into the URL. The ssrfClient guards against host
	// injection, but a clearly-invalid slug indicates bad data and should not
	// reach the network at all.
	if !pluginSlugRe.MatchString(slug) {
		return nil, nil // treat as no checksums available (same as 404)
	}
	if !pluginVersionRe.MatchString(version) {
		return nil, nil
	}
	rawURL := fmt.Sprintf(pluginChecksumsFetchURL, slug, version)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build plugin-checksums request: %w", err)
	}
	req.Header.Set("User-Agent", "WPMgr-ScanChecksums/1.0")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch plugin checksums: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 8<<20))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil // 404: premium or unknown plugin → negative-cache
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("wp.org plugin-checksums returned HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("read plugin-checksums body: %w", err)
	}

	var envelope pluginChecksumAPIResponse
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("decode plugin-checksums: %w", err)
	}
	if len(envelope.Files) == 0 {
		return nil, nil
	}

	out := make(map[string][]pluginFileVariant, len(envelope.Files))
	for path, entry := range envelope.Files {
		normalized := strings.TrimLeft(path, "/")
		md5Variants, decErr := decodeMD5Variants(entry.MD5)
		if decErr != nil || len(md5Variants) == 0 {
			continue
		}
		// sha256 uses the same string-or-array shape as md5; decodeMD5Variants
		// is hash-agnostic (just lowercases hex). A decode failure or absent
		// field degrades to "no sha256 known" rather than dropping the md5.
		sha256Variants, decErr := decodeMD5Variants(entry.SHA256)
		if decErr != nil {
			sha256Variants = nil
		}
		// The md5/sha256 arrays are documented as parallel (index i of one is
		// the counterpart of index i of the other) ONLY when their lengths
		// agree. If wp.org reports fewer sha256 entries than md5 entries, we
		// cannot tell which md5 each sha256 actually belongs to; pairing by
		// index anyway would silently attach a wrong sha256 to the low-index
		// md5 variants. A wrong hash is worse than a missing one (it is a
		// false positive-trust signal for a later phase), so on a length
		// mismatch we drop sha256 entirely for this file rather than guess.
		if len(sha256Variants) != len(md5Variants) {
			sha256Variants = nil
		}
		variants := make([]pluginFileVariant, 0, len(md5Variants))
		for i, md5val := range md5Variants {
			v := pluginFileVariant{MD5: md5val}
			if i < len(sha256Variants) {
				v.SHA256 = sha256Variants[i]
			}
			variants = append(variants, v)
		}
		out[normalized] = variants
	}
	return out, nil
}

// decodeMD5Variants decodes a json.RawMessage that may be either a JSON string
// or a JSON array of strings. Both forms appear in the wp.org plugin-checksums
// API (different build/line-ending variants of the same file).
func decodeMD5Variants(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	// Try single string first.
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		if single != "" {
			return []string{strings.ToLower(single)}, nil
		}
		return nil, nil
	}
	// Try array of strings.
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(many))
	for _, v := range many {
		if v != "" {
			out = append(out, strings.ToLower(v))
		}
	}
	return out, nil
}

// pluginRowsToMap converts PluginChecksumRows to path → []md5 (all variants).
func pluginRowsToMap(rows []PluginChecksumRow) map[string][]string {
	m := make(map[string][]string)
	for _, r := range rows {
		m[r.Path] = append(m[r.Path], r.MD5)
	}
	return m
}
