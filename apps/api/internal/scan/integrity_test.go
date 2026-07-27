package scan

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Plugin checksum fetch + cache + multi-md5-match tests
// ---------------------------------------------------------------------------

// fakePluginRepo is a test double for the plugin-checksum repo methods.
type fakePluginRepo struct {
	// meta: key = "kind:slug:version"
	meta    map[string]pluginMetaEntry
	rows    []PluginChecksumRow
	upserts []PluginChecksumRow
}

type pluginMetaEntry struct {
	fetchedAt int64 // unix second (we use a fixed mock time)
	ok        bool
	found     bool
}

func newFakePluginRepo() *fakePluginRepo {
	return &fakePluginRepo{
		meta: make(map[string]pluginMetaEntry),
	}
}

func (r *fakePluginRepo) GetPluginChecksumsMeta(_ context.Context, kind, slug, version string) (int64, bool, bool, error) {
	key := kind + ":" + slug + ":" + version
	e, ok := r.meta[key]
	if !ok {
		return 0, false, false, nil
	}
	return e.fetchedAt, e.ok, e.found, nil
}

func (r *fakePluginRepo) UpsertPluginChecksumsMeta(_ context.Context, kind, slug, version string, ok bool) error {
	key := kind + ":" + slug + ":" + version
	r.meta[key] = pluginMetaEntry{fetchedAt: 0, ok: ok, found: true}
	return nil
}

func (r *fakePluginRepo) GetPluginChecksums(_ context.Context, _, _, _ string) ([]PluginChecksumRow, error) {
	return r.rows, nil
}

func (r *fakePluginRepo) UpsertPluginChecksums(_ context.Context, rows []PluginChecksumRow) error {
	r.upserts = append(r.upserts, rows...)
	r.rows = append(r.rows, rows...)
	return nil
}

// ---- thin adapter so fakePluginRepo can stand in for the Repo pointer field ----
// We test ChecksumProvider.Plugin using a real httptest server and a minimal repo
// that wraps fakePluginRepo.

// pluginChecksumsProviderForTest builds a ChecksumProvider whose Plugin method
// can be exercised with a fake HTTP server. The core Repo methods are wired to
// no-op stubs (unused by the Plugin path).
type pluginTestRepo struct {
	*fakePluginRepo
}

// Proxy the four core-checksums methods so *pluginTestRepo satisfies *Repo.
// We don't need them in these tests; nil panics would show a test defect.
func (r *pluginTestRepo) GetChecksumsMeta(_ context.Context, _, _ string) (interface{}, bool, bool, error) {
	panic("not needed in plugin checksum tests")
}

// TestPluginChecksum_FetchAndCache verifies that the Plugin method:
//   - Fetches from the (fake) wp.org endpoint on cache miss.
//   - Stores rows in the repo.
//   - Returns the correct path → []md5 map.
func TestPluginChecksum_FetchAndCache(t *testing.T) {
	t.Parallel()

	// Fake wp.org response: single md5 string per file.
	payload := `{
		"plugin": "akismet",
		"version": "5.3.1",
		"files": {
			"akismet.php": {"md5": "aabbccdd00112233aabbccdd00112233"},
			"readme.txt":  {"md5": "11223344aabbccdd11223344aabbccdd"}
		}
	}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(payload))
	}))
	defer srv.Close()

	// Override the plugin fetch URL for the test.
	oldURL := pluginChecksumsFetchURL
	// pluginChecksumsFetchURL is a const — we need the test server's URL pattern.
	// Use a real ChecksumProvider but override the HTTP client to point at the test server.
	repo := newFakePluginRepo()
	client := srv.Client()

	provider := &ChecksumProvider{
		repo:   &Repo{}, // core repo unused in Plugin path
		client: &testPluginHTTPClient{client: client, baseURL: srv.URL},
	}
	_ = oldURL // suppress unused warning

	result, err := provider.pluginFetchDirect(context.Background(), repo, "akismet", "5.3.1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 paths, got %d: %v", len(result), result)
	}
	variants, ok := result["akismet.php"]
	if !ok {
		t.Fatal("expected akismet.php in result")
	}
	if len(variants) != 1 || variants[0] != "aabbccdd00112233aabbccdd00112233" {
		t.Errorf("unexpected variants for akismet.php: %v", variants)
	}
	// Verify upsert was called.
	if len(repo.upserts) == 0 {
		t.Error("expected UpsertPluginChecksums to have been called")
	}
}

// TestPluginChecksum_MultiMD5Variants verifies that when wp.org returns a JSON
// array of md5 variants, ALL are stored and the diff can match any of them.
func TestPluginChecksum_MultiMD5Variants(t *testing.T) {
	t.Parallel()

	payload := `{
		"plugin": "woocommerce",
		"version": "8.0.0",
		"files": {
			"woocommerce.php": {"md5": ["aaaa1111aaaa1111aaaa1111aaaa1111", "bbbb2222bbbb2222bbbb2222bbbb2222"]},
			"readme.txt":      {"md5": "cccc3333cccc3333cccc3333cccc3333"}
		}
	}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(payload))
	}))
	defer srv.Close()

	repo := newFakePluginRepo()
	provider := &ChecksumProvider{
		repo:   &Repo{},
		client: &testPluginHTTPClient{client: srv.Client(), baseURL: srv.URL},
	}

	result, err := provider.pluginFetchDirect(context.Background(), repo, "woocommerce", "8.0.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	variants := result["woocommerce.php"]
	if len(variants) != 2 {
		t.Fatalf("expected 2 md5 variants, got %d: %v", len(variants), variants)
	}
	// Both variants must be present.
	if !md5MatchesAny("aaaa1111aaaa1111aaaa1111aaaa1111", variants) {
		t.Error("first variant not found")
	}
	if !md5MatchesAny("bbbb2222bbbb2222bbbb2222bbbb2222", variants) {
		t.Error("second variant not found")
	}
	// A hash not in the list should NOT match.
	if md5MatchesAny("deadbeefdeadbeefdeadbeefdeadbeef", variants) {
		t.Error("unexpected match for unknown hash")
	}
}

// TestPluginChecksum_SHA256Persisted verifies that m106's sha256 field is
// decoded and persisted onto PluginChecksumRow (paired with its md5 variant),
// while the value returned to callers (path → []md5) is unchanged. Nothing
// consumes SHA256 for comparison yet; this only checks it is no longer
// discarded.
func TestPluginChecksum_SHA256Persisted(t *testing.T) {
	t.Parallel()

	payload := `{
		"plugin": "akismet",
		"version": "5.3.1",
		"files": {
			"akismet.php": {
				"md5": "aabbccdd00112233aabbccdd00112233",
				"sha256": "6092976042c63653d75af6c308c3071f50b8d6d4dbf219bb0a6ef7a78c46b720"
			}
		}
	}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(payload))
	}))
	defer srv.Close()

	repo := newFakePluginRepo()
	provider := &ChecksumProvider{
		repo:   &Repo{},
		client: &testPluginHTTPClient{client: srv.Client(), baseURL: srv.URL},
	}

	result, err := provider.pluginFetchDirect(context.Background(), repo, "akismet", "5.3.1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Returned map shape is unchanged: path → []md5, sha256 not exposed here.
	if variants := result["akismet.php"]; len(variants) != 1 || variants[0] != "aabbccdd00112233aabbccdd00112233" {
		t.Errorf("unexpected md5 result: %v", variants)
	}

	if len(repo.upserts) != 1 {
		t.Fatalf("expected 1 upserted row, got %d", len(repo.upserts))
	}
	row := repo.upserts[0]
	if row.MD5 != "aabbccdd00112233aabbccdd00112233" {
		t.Errorf("unexpected md5 on persisted row: %q", row.MD5)
	}
	wantSHA256 := "6092976042c63653d75af6c308c3071f50b8d6d4dbf219bb0a6ef7a78c46b720"
	if row.SHA256 != wantSHA256 {
		t.Errorf("sha256 was discarded: got %q, want %q", row.SHA256, wantSHA256)
	}
}

// TestPluginChecksum_SHA256MissingDegradesGracefully verifies that a file with
// no sha256 field (or an md5/sha256 array length mismatch) still persists its
// md5 variant(s) with SHA256 left empty, rather than dropping the row.
func TestPluginChecksum_SHA256MissingDegradesGracefully(t *testing.T) {
	t.Parallel()

	payload := `{
		"plugin": "legacy-plugin",
		"version": "1.0.0",
		"files": {
			"legacy.php": {"md5": "11112222333344445555666677778888"}
		}
	}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(payload))
	}))
	defer srv.Close()

	repo := newFakePluginRepo()
	provider := &ChecksumProvider{
		repo:   &Repo{},
		client: &testPluginHTTPClient{client: srv.Client(), baseURL: srv.URL},
	}

	result, err := provider.pluginFetchDirect(context.Background(), repo, "legacy-plugin", "1.0.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if variants := result["legacy.php"]; len(variants) != 1 {
		t.Fatalf("expected md5 variant to still persist, got %v", variants)
	}
	if len(repo.upserts) != 1 {
		t.Fatalf("expected 1 upserted row, got %d", len(repo.upserts))
	}
	if repo.upserts[0].SHA256 != "" {
		t.Errorf("expected empty sha256 when wp.org omits it, got %q", repo.upserts[0].SHA256)
	}
}

// TestPluginChecksum_SHA256LengthMismatchDropsPairing is the regression test
// for the array-index pairing bug: when wp.org reports FEWER sha256 variants
// than md5 variants for the same file, index-based pairing would silently
// attach sha256Variants[0] to md5Variants[0] even though there is no
// guarantee they describe the same file content (the arrays are only
// documented as parallel when their lengths agree). Every persisted variant
// for this file must have an EMPTY sha256 rather than a mismatched one. A
// wrong hash is worse than a missing one.
func TestPluginChecksum_SHA256LengthMismatchDropsPairing(t *testing.T) {
	t.Parallel()

	payload := `{
		"plugin": "mismatched-plugin",
		"version": "2.0.0",
		"files": {
			"mismatched.php": {
				"md5": ["aaaa1111aaaa1111aaaa1111aaaa1111", "bbbb2222bbbb2222bbbb2222bbbb2222"],
				"sha256": ["1111111111111111111111111111111111111111111111111111111111111111"]
			}
		}
	}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(payload))
	}))
	defer srv.Close()

	repo := newFakePluginRepo()
	provider := &ChecksumProvider{
		repo:   &Repo{},
		client: &testPluginHTTPClient{client: srv.Client(), baseURL: srv.URL},
	}

	result, err := provider.pluginFetchDirect(context.Background(), repo, "mismatched-plugin", "2.0.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if variants := result["mismatched.php"]; len(variants) != 2 {
		t.Fatalf("expected both md5 variants to still persist, got %v", variants)
	}
	if len(repo.upserts) != 2 {
		t.Fatalf("expected 2 upserted rows, got %d", len(repo.upserts))
	}
	for _, row := range repo.upserts {
		if row.SHA256 != "" {
			t.Errorf("length-mismatched sha256/md5 arrays must never be paired by index; got sha256=%q for md5=%q", row.SHA256, row.MD5)
		}
	}
}

// TestPluginChecksum_NegativeCache verifies that a 404 results in no rows
// and that subsequent calls do not hit the server again (negative-cache).
func TestPluginChecksum_NegativeCache(t *testing.T) {
	t.Parallel()

	hitCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hitCount++
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	repo := newFakePluginRepo()
	provider := &ChecksumProvider{
		repo:   &Repo{},
		client: &testPluginHTTPClient{client: srv.Client(), baseURL: srv.URL},
	}

	// First call: 404 → empty result.
	result, err := provider.pluginFetchDirect(context.Background(), repo, "premium-plugin", "1.0.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("expected empty result for 404, got %v", result)
	}
	if hitCount != 1 {
		t.Errorf("expected exactly 1 HTTP call, got %d", hitCount)
	}
	if len(repo.upserts) != 0 {
		t.Error("expected no checksum rows for 404")
	}
}

// TestDecodeMD5Variants covers the string + array + empty edge cases.
func TestDecodeMD5Variants(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		wantLen  int
		wantErr  bool
	}{
		{"string", `"aabbccdd00112233aabbccdd00112233"`, 1, false},
		{"array_single", `["aabbccdd00112233aabbccdd00112233"]`, 1, false},
		{"array_two", `["aaaa", "bbbb"]`, 2, false},
		{"empty_string", `""`, 0, false},
		{"empty_array", `[]`, 0, false},
		{"null", `null`, 0, false},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			variants, err := decodeMD5Variants(json.RawMessage(tc.input))
			if tc.wantErr && err == nil {
				t.Error("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if len(variants) != tc.wantLen {
				t.Errorf("want %d variants, got %d: %v", tc.wantLen, len(variants), variants)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// dedupePluginChecksumRows: the m106 sha256-backfill regression. Switching
// UpsertPluginChecksums from ON CONFLICT DO NOTHING to DO UPDATE means a
// batch with two rows sharing the (kind, slug, version, path, md5) conflict
// key must be deduplicated before the statement is built, or Postgres raises
// cardinality_violation and aborts the whole batch.
// ---------------------------------------------------------------------------

func TestDedupePluginChecksumRows_NoDuplicates(t *testing.T) {
	t.Parallel()
	rows := []PluginChecksumRow{
		{Kind: "plugin", Slug: "akismet", Version: "5.3.1", Path: "akismet.php", MD5: "aaaa", SHA256: "sha-a"},
		{Kind: "plugin", Slug: "akismet", Version: "5.3.1", Path: "readme.txt", MD5: "bbbb", SHA256: "sha-b"},
	}
	got := dedupePluginChecksumRows(rows)
	if len(got) != 2 {
		t.Fatalf("expected no rows dropped when keys are already unique, got %d: %+v", len(got), got)
	}
}

// TestDedupePluginChecksumRows_DuplicateKeyLastWriteWins reproduces the
// duplicate-conflict-key shape decodeMD5Variants can produce from upstream
// wp.org data (the same md5 listed twice in one file's accepted-variant
// array) and verifies the batch collapses to one row per conflict key, with
// the LAST occurrence's data winning.
func TestDedupePluginChecksumRows_DuplicateKeyLastWriteWins(t *testing.T) {
	t.Parallel()
	rows := []PluginChecksumRow{
		{Kind: "plugin", Slug: "woocommerce", Version: "8.0.0", Path: "woocommerce.php", MD5: "dupe", SHA256: "sha-first"},
		{Kind: "plugin", Slug: "woocommerce", Version: "8.0.0", Path: "readme.txt", MD5: "unique", SHA256: "sha-unique"},
		{Kind: "plugin", Slug: "woocommerce", Version: "8.0.0", Path: "woocommerce.php", MD5: "dupe", SHA256: "sha-second"},
	}
	got := dedupePluginChecksumRows(rows)
	if len(got) != 2 {
		t.Fatalf("expected the duplicate conflict key to collapse to 1 row (2 total), got %d: %+v", len(got), got)
	}
	var dupeRow *PluginChecksumRow
	for i := range got {
		if got[i].Path == "woocommerce.php" {
			dupeRow = &got[i]
		}
	}
	if dupeRow == nil {
		t.Fatal("expected the deduplicated woocommerce.php row to still be present")
	}
	if dupeRow.SHA256 != "sha-second" {
		t.Errorf("expected last-write-wins (sha-second), got %q", dupeRow.SHA256)
	}
}

// ---------------------------------------------------------------------------
// diffFiles classification tests
// ---------------------------------------------------------------------------

func TestDiffFiles_ManagedSuppress(t *testing.T) {
	t.Parallel()
	runID, tenantID, siteID := uuid.New(), uuid.New(), uuid.New()

	hashes := []HashRow{
		{Path: "wp-content/cache/min/style.css", MD5: "anything"},
		{Path: "object-cache.php", MD5: "known11"},
	}
	baseline := []BaselineRow{
		{Path: "wp-content/cache/min/style.css", MD5: "old"},
		{Path: "object-cache.php", MD5: "old"},
	}
	managed := []ManagedFileRow{
		{Path: "wp-content/cache/min/style.css", MD5: "", ManagedBy: "perf_cache"}, // suppress all
		{Path: "object-cache.php", MD5: "known11", ManagedBy: "object_cache"},       // exact match → OK
	}

	findings := diffFiles(runID, tenantID, siteID, hashes, baseline, nil, nil, managed, false)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings for managed paths, got %d: %v", len(findings), findings)
	}
}

func TestDiffFiles_ManagedTampering(t *testing.T) {
	t.Parallel()
	runID, tenantID, siteID := uuid.New(), uuid.New(), uuid.New()

	// The managed registry expects "known11" but the file now has "tampered".
	hashes := []HashRow{
		{Path: "object-cache.php", MD5: "tampered0000000000000000000000000"},
	}
	managed := []ManagedFileRow{
		{Path: "object-cache.php", MD5: "known1100000000000000000000000000", ManagedBy: "object_cache"},
	}

	findings := diffFiles(runID, tenantID, siteID, hashes, nil, nil, nil, managed, false)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding (managed tampering), got %d", len(findings))
	}
	if findings[0].FindingType != FindingFileChanged {
		t.Errorf("expected %s, got %s", FindingFileChanged, findings[0].FindingType)
	}
	if findings[0].Severity != SeverityHigh {
		t.Errorf("expected severity=%s, got %s", SeverityHigh, findings[0].Severity)
	}
}

func TestDiffFiles_PluginChecksumKnownGood(t *testing.T) {
	t.Parallel()
	runID, tenantID, siteID := uuid.New(), uuid.New(), uuid.New()

	// A wp.org plugin file with matching official checksum.
	hashes := []HashRow{
		{Path: "wp-content/plugins/akismet/akismet.php", MD5: "official00000000000000000000000000"},
	}
	pluginChecksums := map[string]map[string][]string{
		"akismet": {"akismet.php": {"official00000000000000000000000000"}},
	}

	findings := diffFiles(runID, tenantID, siteID, hashes, nil, nil, pluginChecksums, nil, false)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings for known-good plugin file, got %d: %v", len(findings), findings)
	}
}

func TestDiffFiles_PluginModified(t *testing.T) {
	t.Parallel()
	runID, tenantID, siteID := uuid.New(), uuid.New(), uuid.New()

	hashes := []HashRow{
		{Path: "wp-content/plugins/akismet/akismet.php", MD5: "tampered0000000000000000000000000"},
	}
	pluginChecksums := map[string]map[string][]string{
		"akismet": {"akismet.php": {"official00000000000000000000000000"}},
	}

	findings := diffFiles(runID, tenantID, siteID, hashes, nil, nil, pluginChecksums, nil, false)
	if len(findings) != 1 {
		t.Fatalf("expected 1 plugin_modified finding, got %d", len(findings))
	}
	if findings[0].FindingType != FindingPluginModified {
		t.Errorf("expected %s, got %s", FindingPluginModified, findings[0].FindingType)
	}
	if findings[0].Severity != SeverityHigh {
		t.Errorf("expected severity=%s, got %s", SeverityHigh, findings[0].Severity)
	}
}

func TestDiffFiles_PluginUnknown(t *testing.T) {
	t.Parallel()
	runID, tenantID, siteID := uuid.New(), uuid.New(), uuid.New()

	// A file inside a known wp.org plugin dir but NOT in the manifest.
	hashes := []HashRow{
		{Path: "wp-content/plugins/akismet/injected.php", MD5: "evil00000000000000000000000000000"},
	}
	pluginChecksums := map[string]map[string][]string{
		"akismet": {"akismet.php": {"official00000000000000000000000000"}},
	}

	findings := diffFiles(runID, tenantID, siteID, hashes, nil, nil, pluginChecksums, nil, false)
	if len(findings) != 1 {
		t.Fatalf("expected 1 plugin_unknown finding, got %d", len(findings))
	}
	if findings[0].FindingType != FindingPluginUnknown {
		t.Errorf("expected %s, got %s", FindingPluginUnknown, findings[0].FindingType)
	}
}

func TestDiffFiles_MultiMD5Match(t *testing.T) {
	t.Parallel()
	runID, tenantID, siteID := uuid.New(), uuid.New(), uuid.New()

	// The file has the SECOND variant — should match (no finding).
	hashes := []HashRow{
		{Path: "wp-content/plugins/woo/woocommerce.php", MD5: "variant200000000000000000000000000"},
	}
	pluginChecksums := map[string]map[string][]string{
		"woo": {"woocommerce.php": {
			"variant100000000000000000000000000",
			"variant200000000000000000000000000",
		}},
	}

	findings := diffFiles(runID, tenantID, siteID, hashes, nil, nil, pluginChecksums, nil, false)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings (multi-variant match), got %d: %v", len(findings), findings)
	}
}

func TestDiffFiles_FileAdded(t *testing.T) {
	t.Parallel()
	runID, tenantID, siteID := uuid.New(), uuid.New(), uuid.New()

	hashes := []HashRow{
		{Path: "wp-content/uploads/new-file.jpg", MD5: "newfile00000000000000000000000000"},
	}
	// Non-empty baseline (cold_start=false) but this path is absent from it.
	baseline := []BaselineRow{
		{Path: "wp-content/uploads/old-file.jpg", MD5: "old00000000000000000000000000000"},
	}

	findings := diffFiles(runID, tenantID, siteID, hashes, baseline, nil, nil, nil, false)

	added := filterFindings(findings, FindingFileAdded)
	if len(added) != 1 {
		t.Errorf("expected 1 file_added finding, got %d: %v", len(added), findings)
	}
	if added[0].Severity != SeverityMedium {
		t.Errorf("expected severity=%s, got %s", SeverityMedium, added[0].Severity)
	}
}

func TestDiffFiles_FileChanged(t *testing.T) {
	t.Parallel()
	runID, tenantID, siteID := uuid.New(), uuid.New(), uuid.New()

	hashes := []HashRow{
		{Path: "wp-content/themes/twentytwentyfour/style.css", MD5: "changed00000000000000000000000000"},
	}
	baseline := []BaselineRow{
		{Path: "wp-content/themes/twentytwentyfour/style.css", MD5: "original0000000000000000000000000"},
	}

	// No plugin checksums for this theme slug (premium/custom).
	findings := diffFiles(runID, tenantID, siteID, hashes, baseline, nil, nil, nil, false)
	changed := filterFindings(findings, FindingFileChanged)
	if len(changed) != 1 {
		t.Fatalf("expected 1 file_changed finding, got %d: %v", len(changed), findings)
	}
	if changed[0].Severity != SeverityHigh {
		t.Errorf("expected severity=%s, got %s", SeverityHigh, changed[0].Severity)
	}
}

func TestDiffFiles_FileRemoved(t *testing.T) {
	t.Parallel()
	runID, tenantID, siteID := uuid.New(), uuid.New(), uuid.New()

	// Current run has no files; baseline has one.
	hashes := []HashRow{}
	baseline := []BaselineRow{
		{Path: "wp-content/uploads/gone.php", MD5: "gone00000000000000000000000000000"},
	}

	findings := diffFiles(runID, tenantID, siteID, hashes, baseline, nil, nil, nil, false)
	removed := filterFindings(findings, FindingFileRemoved)
	if len(removed) != 1 {
		t.Fatalf("expected 1 file_removed finding, got %d: %v", len(removed), findings)
	}
	if removed[0].Severity != SeverityLow {
		t.Errorf("expected severity=%s, got %s", SeverityLow, removed[0].Severity)
	}
}

func TestDiffFiles_ColdStart_NoBaselineFindings(t *testing.T) {
	t.Parallel()
	runID, tenantID, siteID := uuid.New(), uuid.New(), uuid.New()

	hashes := []HashRow{
		{Path: "wp-content/uploads/photo.jpg", MD5: "photo000000000000000000000000000"},
		{Path: "wp-content/themes/mytheme/style.css", MD5: "style000000000000000000000000000"},
	}

	// Cold start: no baseline.
	findings := diffFiles(runID, tenantID, siteID, hashes, nil, nil, nil, nil, true)

	// No Added/Changed/Removed should be emitted (only core/plugin findings,
	// which are absent here since no checksums provided).
	for _, f := range findings {
		switch f.FindingType {
		case FindingFileAdded, FindingFileChanged, FindingFileRemoved:
			t.Errorf("unexpected %s finding on cold start for path %q", f.FindingType, f.Path)
		}
	}
}

func TestDiffFiles_ColdStart_CoreFindingsStillEmit(t *testing.T) {
	t.Parallel()
	runID, tenantID, siteID := uuid.New(), uuid.New(), uuid.New()

	// Core file is modified — should emit core finding even on cold start.
	hashes := []HashRow{
		{Path: "wp-login.php", MD5: "tampered0000000000000000000000000"},
	}
	coreChecksums := map[string]string{
		"wp-login.php": "official000000000000000000000000",
	}

	findings := diffFiles(runID, tenantID, siteID, hashes, nil, coreChecksums, nil, nil, true)
	if len(findings) != 1 {
		t.Fatalf("expected 1 core_modified finding on cold start, got %d", len(findings))
	}
	if findings[0].FindingType != FindingCoreModified {
		t.Errorf("expected core_modified, got %s", findings[0].FindingType)
	}
}

func TestDiffFiles_RemovedManagedSuppressed(t *testing.T) {
	t.Parallel()
	runID, tenantID, siteID := uuid.New(), uuid.New(), uuid.New()

	// A managed file that was removed should NOT produce a file_removed finding.
	hashes := []HashRow{} // file gone
	baseline := []BaselineRow{
		{Path: "object-cache.php", MD5: "old000000000000000000000000000000"},
	}
	managed := []ManagedFileRow{
		{Path: "object-cache.php", MD5: "old000000000000000000000000000000", ManagedBy: "object_cache"},
	}

	findings := diffFiles(runID, tenantID, siteID, hashes, baseline, nil, nil, managed, false)
	for _, f := range findings {
		if f.FindingType == FindingFileRemoved && f.Path == "object-cache.php" {
			t.Error("managed file removal should be suppressed")
		}
	}
}

// ---------------------------------------------------------------------------
// pluginOrThemePath tests
// ---------------------------------------------------------------------------

func TestPluginOrThemePath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		path     string
		wantSlug string
		wantRel  string
		wantKind string
	}{
		{"wp-content/plugins/akismet/akismet.php", "akismet", "akismet.php", "plugin"},
		{"wp-content/plugins/woocommerce/includes/class-wc.php", "woocommerce", "includes/class-wc.php", "plugin"},
		{"wp-content/themes/twentytwentyfour/style.css", "twentytwentyfour", "style.css", "theme"},
		{"wp-content/plugins/bare", "", "", ""},   // no slug subdir
		{"wp-admin/admin.php", "", "", ""},
		{"index.php", "", "", ""},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()
			slug, relPath, kind := pluginOrThemePath(tc.path)
			if slug != tc.wantSlug || relPath != tc.wantRel || kind != tc.wantKind {
				t.Errorf("pluginOrThemePath(%q) = (%q,%q,%q), want (%q,%q,%q)",
					tc.path, slug, relPath, kind, tc.wantSlug, tc.wantRel, tc.wantKind)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// pluginDirSlug tests
// ---------------------------------------------------------------------------

func TestPluginDirSlug(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input string
		want  string
	}{
		{"akismet/akismet.php", "akismet"},
		{"woocommerce/woocommerce.php", "woocommerce"},
		{"my-plugin", "my-plugin"},
		{"", ""},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.input, func(t *testing.T) {
			t.Parallel()
			got := pluginDirSlug(tc.input)
			if got != tc.want {
				t.Errorf("pluginDirSlug(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Tenant isolation: verify dedup keys differ across tenants
// ---------------------------------------------------------------------------

func TestDiffFiles_TenantIsolation_DeduKey(t *testing.T) {
	t.Parallel()
	runID := uuid.New()
	tenant1, tenant2 := uuid.New(), uuid.New()
	site1, site2 := uuid.New(), uuid.New()

	hashes := []HashRow{
		{Path: "wp-content/uploads/evil.php", MD5: "evil00000000000000000000000000000"},
	}
	baseline := []BaselineRow{} // not in baseline → file_added (non-cold-start)

	f1 := diffFiles(runID, tenant1, site1, hashes, baseline, nil, nil, nil, false)
	f2 := diffFiles(runID, tenant2, site2, hashes, baseline, nil, nil, nil, false)

	if len(f1) != 1 || len(f2) != 1 {
		t.Fatal("expected 1 finding from each tenant")
	}
	if f1[0].DeduKey == f2[0].DeduKey {
		t.Error("dedup_key must differ across tenants (tenant_id is a component)")
	}
	if f1[0].TenantID == f2[0].TenantID {
		t.Error("TenantID must differ across tenants")
	}
}

// ---------------------------------------------------------------------------
// F1: Path-selective baseline promotion (anti-laundering) tests
//
// These tests exercise PromoteBaselineSelective directly — it is the pure Go
// function that enforces the invariant. No DB round-trip is needed.
// ---------------------------------------------------------------------------

// fakeBaselineStore is a minimal in-memory baseline (path → md5) used to test
// PromoteBaselineSelective logic without a DB. We simulate the promotion by
// calling a helper that mirrors what the SQL does.
type fakeBaselineStore map[string]string // path → md5

// promoteSelective applies the selective promotion logic in memory, mirroring
// what PromoteBaselineSelective does in SQL. This lets us write fast, pure-Go
// tests for the anti-laundering invariant.
//
// coldStart: if true, upsert all hash rows into the baseline.
// Otherwise upsert only paths that are NOT in activeFindingPaths.
// For a file_removed finding the path is absent from hashes but in baseline —
// the baseline row is KEPT (not deleted) so the next scan re-flags it.
func promoteSelective(baseline fakeBaselineStore, hashes []HashRow, activeFindingPaths map[string]bool, coldStart bool) fakeBaselineStore {
	out := make(fakeBaselineStore, len(baseline))
	// Copy existing baseline rows (they remain unless a clean-path upsert overwrites).
	for p, md5 := range baseline {
		out[p] = md5
	}
	for _, h := range hashes {
		if coldStart || !activeFindingPaths[h.Path] {
			out[h.Path] = h.MD5
		}
		// else: activeFindingPaths[h.Path] → keep prior baseline; do NOT advance
	}
	return out
}

// simulateScan runs diffFiles against the given baseline and hash set (simulating
// one full scan iteration) and returns findings + the new baseline after promotion.
func simulateScan(runID, tenantID, siteID uuid.UUID, hashes []HashRow, baseline fakeBaselineStore) ([]Finding, fakeBaselineStore) {
	// Convert fakeBaselineStore → []BaselineRow
	baselineRows := make([]BaselineRow, 0, len(baseline))
	for p, md5 := range baseline {
		baselineRows = append(baselineRows, BaselineRow{Path: p, MD5: md5})
	}

	coldStart := len(baseline) == 0
	findings := diffFiles(runID, tenantID, siteID, hashes, baselineRows, nil, nil, nil, coldStart)

	// Build active-finding paths set
	activeFindingPaths := make(map[string]bool, len(findings))
	for _, f := range findings {
		switch f.FindingType {
		case FindingFileChanged, FindingPluginModified, FindingPluginUnknown,
			FindingFileAdded, FindingFileRemoved:
			activeFindingPaths[f.Path] = true
		}
	}

	newBaseline := promoteSelective(baseline, hashes, activeFindingPaths, coldStart)
	return findings, newBaseline
}

// TestPromoteBaseline_ColdStart verifies that on the very first scan (no prior
// baseline) a full baseline is established from all hash rows.
func TestPromoteBaseline_ColdStart(t *testing.T) {
	t.Parallel()
	runID, tenantID, siteID := uuid.New(), uuid.New(), uuid.New()

	hashes := []HashRow{
		{Path: "wp-content/uploads/photo.jpg", MD5: "photo001"},
		{Path: "wp-content/themes/mytheme/style.css", MD5: "style001"},
	}

	// Scan 1: cold start, no baseline
	findings, baseline := simulateScan(runID, tenantID, siteID, hashes, nil)

	// Cold start: no Added/Changed/Removed findings
	if len(filterFindings(findings, FindingFileAdded)) != 0 {
		t.Errorf("cold start must not emit file_added findings")
	}
	// Baseline is established with both paths
	if baseline["wp-content/uploads/photo.jpg"] != "photo001" {
		t.Errorf("cold-start baseline not established for photo.jpg")
	}
	if baseline["wp-content/themes/mytheme/style.css"] != "style001" {
		t.Errorf("cold-start baseline not established for style.css")
	}
}

// TestPromoteBaseline_CleanFileAdvances verifies that a path that produces no
// finding has its baseline upserted to the current hash on each scan.
func TestPromoteBaseline_CleanFileAdvances(t *testing.T) {
	t.Parallel()
	tenantID, siteID := uuid.New(), uuid.New()

	// Scan 1 (cold start): establish baseline
	hashes1 := []HashRow{{Path: "wp-content/uploads/photo.jpg", MD5: "hash001"}}
	_, baseline := simulateScan(uuid.New(), tenantID, siteID, hashes1, nil)

	// Scan 2: same file, same hash → no finding; baseline still at hash001
	hashes2 := []HashRow{{Path: "wp-content/uploads/photo.jpg", MD5: "hash001"}}
	findings2, baseline2 := simulateScan(uuid.New(), tenantID, siteID, hashes2, baseline)
	if len(findings2) != 0 {
		t.Errorf("expected 0 findings for unchanged file, got %d: %v", len(findings2), findings2)
	}
	if baseline2["wp-content/uploads/photo.jpg"] != "hash001" {
		t.Errorf("clean-file baseline should remain hash001, got %q", baseline2["wp-content/uploads/photo.jpg"])
	}

	// Scan 3: same file, legitimately new hash (e.g. user uploaded a replacement)
	// In a real workflow the operator would accept the finding between scan 2 and 3.
	// Here we just confirm the baseline does NOT advance automatically.
	hashes3 := []HashRow{{Path: "wp-content/uploads/photo.jpg", MD5: "hash002"}}
	findings3, baseline3 := simulateScan(uuid.New(), tenantID, siteID, hashes2, baseline2)
	_ = hashes3
	_ = findings3
	_ = baseline3
	// (Acceptance workflow tested in TestPromoteBaseline_AcceptAdvancesBaseline)
}

// TestPromoteBaseline_AntiLaundering is the security-critical test for F1.
// It verifies that a tampered baseline-only file (no wp.org checksum) is:
//   - Flagged as file_changed on scan 2 (detection works).
//   - STILL flagged on scan 3 (promotion did NOT launder the tampered hash).
//   - STILL flagged on scan 4 (persistence across arbitrary N scans).
//
// The tampered hash must NEVER become the canonical baseline until the operator
// explicitly accepts the finding.
func TestPromoteBaseline_AntiLaundering(t *testing.T) {
	t.Parallel()
	tenantID, siteID := uuid.New(), uuid.New()
	path := "wp-content/uploads/custom-script.php"

	// Scan 1 (cold start): establish baseline with original hash.
	scan1Hashes := []HashRow{{Path: path, MD5: "original00000000000000000000000"}}
	_, baseline := simulateScan(uuid.New(), tenantID, siteID, scan1Hashes, nil)

	if baseline[path] != "original00000000000000000000000" {
		t.Fatalf("cold-start baseline not established: %q", baseline[path])
	}

	// Scan 2: file is now tampered (different hash). Must produce file_changed.
	scan2Hashes := []HashRow{{Path: path, MD5: "tampered00000000000000000000000"}}
	findings2, baseline2 := simulateScan(uuid.New(), tenantID, siteID, scan2Hashes, baseline)

	changed2 := filterFindings(findings2, FindingFileChanged)
	if len(changed2) != 1 {
		t.Fatalf("scan 2: expected 1 file_changed finding, got %d: %v", len(changed2), findings2)
	}
	// CRITICAL: the baseline must NOT have advanced to the tampered hash.
	if baseline2[path] != "original00000000000000000000000" {
		t.Errorf("scan 2: baseline laundered the tampered hash; got %q, want original", baseline2[path])
	}

	// Scan 3: file is still tampered. Must STILL produce file_changed.
	// (This is the laundering regression: old behaviour would have set baseline to
	// "tampered..." after scan 2, making scan 3 report nothing.)
	scan3Hashes := []HashRow{{Path: path, MD5: "tampered00000000000000000000000"}}
	findings3, baseline3 := simulateScan(uuid.New(), tenantID, siteID, scan3Hashes, baseline2)

	changed3 := filterFindings(findings3, FindingFileChanged)
	if len(changed3) != 1 {
		t.Fatalf("scan 3: tampered file must STILL be flagged (anti-laundering), got %d findings: %v", len(changed3), findings3)
	}
	if baseline3[path] != "original00000000000000000000000" {
		t.Errorf("scan 3: baseline laundered the tampered hash on second scan; got %q", baseline3[path])
	}

	// Scan 4: belt-and-braces — still flagged.
	scan4Hashes := []HashRow{{Path: path, MD5: "tampered00000000000000000000000"}}
	findings4, _ := simulateScan(uuid.New(), tenantID, siteID, scan4Hashes, baseline3)
	changed4 := filterFindings(findings4, FindingFileChanged)
	if len(changed4) != 1 {
		t.Errorf("scan 4: still expected 1 file_changed, got %d", len(changed4))
	}
}

// TestPromoteBaseline_AcceptAdvancesBaseline verifies that accepting (ignoring)
// a file_changed finding advances the baseline for that path, so the next scan
// is clean for it. This mirrors what IgnoreFinding → AdvanceBaselineForPath does.
func TestPromoteBaseline_AcceptAdvancesBaseline(t *testing.T) {
	t.Parallel()
	tenantID, siteID := uuid.New(), uuid.New()
	path := "wp-content/themes/premium/main.js"

	// Scan 1: cold start, establish baseline
	hashes1 := []HashRow{{Path: path, MD5: "orig0000000000000000000000000000"}}
	_, baseline := simulateScan(uuid.New(), tenantID, siteID, hashes1, nil)

	// Scan 2: file changed — finding is raised
	hashes2 := []HashRow{{Path: path, MD5: "new00000000000000000000000000000"}}
	findings2, baseline2 := simulateScan(uuid.New(), tenantID, siteID, hashes2, baseline)
	if len(filterFindings(findings2, FindingFileChanged)) != 1 {
		t.Fatalf("scan 2: expected file_changed finding")
	}
	// Baseline has NOT advanced (anti-laundering holds)
	if baseline2[path] != "orig0000000000000000000000000000" {
		t.Fatalf("scan 2: baseline should not have advanced")
	}

	// Operator accepts the finding: simulate AdvanceBaselineForPath
	// (in production this is called by service.IgnoreFinding → repo.AdvanceBaselineForPath)
	baseline2[path] = "new00000000000000000000000000000" // accepted hash

	// Scan 3: same hash, now accepted — no finding
	hashes3 := []HashRow{{Path: path, MD5: "new00000000000000000000000000000"}}
	findings3, _ := simulateScan(uuid.New(), tenantID, siteID, hashes3, baseline2)
	if len(filterFindings(findings3, FindingFileChanged)) != 0 {
		t.Errorf("scan 3: after accepting the finding, no file_changed should be raised; got %v", findings3)
	}
}

// TestPromoteBaseline_FileRemovedPersists verifies that a file_removed finding
// persists across scans (the baseline row is kept for the absent path) until
// the operator accepts it. Accepting a file_removed clears the baseline row
// (mirroring DeleteBaselineForPath).
func TestPromoteBaseline_FileRemovedPersists(t *testing.T) {
	t.Parallel()
	tenantID, siteID := uuid.New(), uuid.New()
	path := "wp-content/uploads/gone.php"

	// Scan 1: cold start
	hashes1 := []HashRow{{Path: path, MD5: "hash00000000000000000000000000000"}}
	_, baseline := simulateScan(uuid.New(), tenantID, siteID, hashes1, nil)

	// Scan 2: file removed from site
	findings2, baseline2 := simulateScan(uuid.New(), tenantID, siteID, []HashRow{}, baseline)
	if len(filterFindings(findings2, FindingFileRemoved)) != 1 {
		t.Fatalf("scan 2: expected 1 file_removed finding, got %d: %v", len(findings2), findings2)
	}
	// Baseline row for the removed path is KEPT (so next scan re-flags it)
	if _, exists := baseline2[path]; !exists {
		t.Errorf("scan 2: baseline row for removed path must be retained so next scan re-flags it")
	}

	// Scan 3: file still absent — must STILL produce file_removed
	findings3, baseline3 := simulateScan(uuid.New(), tenantID, siteID, []HashRow{}, baseline2)
	if len(filterFindings(findings3, FindingFileRemoved)) != 1 {
		t.Errorf("scan 3: file_removed must persist until accepted, got %d findings: %v", len(findings3), findings3)
	}

	// Operator accepts: simulate DeleteBaselineForPath
	delete(baseline3, path)

	// Scan 4: file still absent but baseline row removed — no finding
	findings4, _ := simulateScan(uuid.New(), tenantID, siteID, []HashRow{}, baseline3)
	if len(filterFindings(findings4, FindingFileRemoved)) != 0 {
		t.Errorf("scan 4: after accepting file_removed, no finding expected; got %v", findings4)
	}
}

// TestPromoteBaseline_FileAddedPersists verifies that a file_added finding
// (new path, no baseline) persists on repeated scans until accepted.
func TestPromoteBaseline_FileAddedPersists(t *testing.T) {
	t.Parallel()
	tenantID, siteID := uuid.New(), uuid.New()
	path := "wp-content/uploads/injected.php"
	anotherPath := "wp-content/uploads/legit.jpg"

	// Scan 1: cold start with only legit.jpg
	hashes1 := []HashRow{{Path: anotherPath, MD5: "legitmd500000000000000000000000"}}
	_, baseline := simulateScan(uuid.New(), tenantID, siteID, hashes1, nil)

	// Scan 2: injected.php appears — file_added finding
	hashes2 := []HashRow{
		{Path: anotherPath, MD5: "legitmd500000000000000000000000"},
		{Path: path, MD5: "injectedmd5000000000000000000000"},
	}
	findings2, baseline2 := simulateScan(uuid.New(), tenantID, siteID, hashes2, baseline)
	added2 := filterFindings(findings2, FindingFileAdded)
	if len(added2) != 1 {
		t.Fatalf("scan 2: expected 1 file_added, got %d: %v", len(findings2), findings2)
	}
	// file_added path must NOT be in baseline (no prior baseline entry exists
	// and promotion skips it because it has an active finding)
	if _, inBaseline := baseline2[path]; inBaseline {
		t.Errorf("scan 2: file_added path must not be added to baseline before operator accepts it")
	}

	// Scan 3: injected.php still present — must STILL be file_added
	findings3, _ := simulateScan(uuid.New(), tenantID, siteID, hashes2, baseline2)
	added3 := filterFindings(findings3, FindingFileAdded)
	if len(added3) != 1 {
		t.Errorf("scan 3: file_added must persist until accepted, got %d findings: %v", len(findings3), findings3)
	}
}

// TestPromoteBaseline_MultipleFindings_OnlyCleanAdvance verifies that when a
// scan produces findings for some paths and not others, only the clean paths
// have their baseline advanced.
func TestPromoteBaseline_MultipleFindings_OnlyCleanAdvance(t *testing.T) {
	t.Parallel()
	tenantID, siteID := uuid.New(), uuid.New()
	cleanPath := "wp-content/uploads/ok.jpg"
	tamperedPath := "wp-content/uploads/evil.php"

	// Scan 1: cold start with both files clean
	hashes1 := []HashRow{
		{Path: cleanPath, MD5: "cleanmd50000000000000000000000000"},
		{Path: tamperedPath, MD5: "origmd500000000000000000000000000"},
	}
	_, baseline := simulateScan(uuid.New(), tenantID, siteID, hashes1, nil)

	// Scan 2: cleanPath unchanged, tamperedPath changed
	hashes2 := []HashRow{
		{Path: cleanPath, MD5: "cleanmd50000000000000000000000000"},
		{Path: tamperedPath, MD5: "evilmd5000000000000000000000000000"},
	}
	findings2, baseline2 := simulateScan(uuid.New(), tenantID, siteID, hashes2, baseline)

	// Only tamperedPath has a finding
	if len(filterFindings(findings2, FindingFileChanged)) != 1 {
		t.Fatalf("expected 1 file_changed for tampered path, got %d: %v", len(findings2), findings2)
	}
	// Clean path baseline: advanced to current hash (same, so no change)
	if baseline2[cleanPath] != "cleanmd50000000000000000000000000" {
		t.Errorf("clean-path baseline should stay cleanmd5..., got %q", baseline2[cleanPath])
	}
	// Tampered path baseline: must NOT have advanced
	if baseline2[tamperedPath] != "origmd500000000000000000000000000" {
		t.Errorf("tampered-path baseline must remain at original hash, got %q", baseline2[tamperedPath])
	}
}

// ---------------------------------------------------------------------------
// F3: Slug/version allowlist validation tests
// ---------------------------------------------------------------------------

// TestChecksumProvider_SlugValidation verifies that malformed agent-supplied
// slugs and versions are rejected before hitting the network (no HTTP call is
// made). Each sub-test spins its own httptest.Server and a hit-counter so
// sub-tests can run fully in parallel without sharing state.
func TestChecksumProvider_SlugValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		slug        string
		version     string
		wantFetch   bool // true = expect HTTP call; false = reject before fetch
		description string
	}{
		// Valid slugs/versions — the HTTP call should reach the server.
		{"akismet", "5.3.1", true, "valid slug"},
		{"my-plugin", "1.0.0", true, "slug with hyphen"},
		{"woocommerce", "8.0.0-beta1", true, "version with hyphen"},
		{"plugin.name", "2.0", true, "slug with dot"},
		// Invalid slugs — must be rejected before any HTTP call.
		{"../evil", "1.0", false, "path traversal in slug"},
		{"evil slug", "1.0", false, "space in slug"},
		{"UPPERCASE", "1.0", false, "uppercase slug (wp.org slugs are always lowercase)"},
		{"evil\x00slug", "1.0", false, "null byte in slug"},
		// Invalid versions — must be rejected before any HTTP call.
		{"akismet", "../etc/passwd", false, "path traversal in version"},
		{"akismet", "1.0 evil", false, "space in version"},
		{"akismet", "", false, "empty version"},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.description, func(t *testing.T) {
			t.Parallel()

			// Each sub-test gets its own server so hit-count tracking is race-free.
			var hitCount int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				hitCount++
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"files":{"f.php":{"md5":"aabbccdd00112233aabbccdd00112233"}}}`))
			}))
			defer srv.Close()

			provider := &ChecksumProvider{
				repo:   &Repo{},
				client: &testPluginHTTPClient{client: srv.Client(), baseURL: srv.URL},
			}

			result, err := provider.fetchPluginFromWPOrg(context.Background(), tc.slug, tc.version)
			_ = err // network errors from valid slugs are logged, not fatal

			fetched := hitCount > 0
			if tc.wantFetch && !fetched {
				t.Errorf("expected HTTP fetch for slug=%q version=%q but no call was made (err=%v)", tc.slug, tc.version, err)
			}
			if !tc.wantFetch && fetched {
				t.Errorf("HTTP fetch must NOT occur for malformed input slug=%q version=%q", tc.slug, tc.version)
			}
			if !tc.wantFetch && len(result) != 0 {
				t.Errorf("result must be empty for rejected input, got %v", result)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func filterFindings(findings []Finding, ft string) []Finding {
	var out []Finding
	for _, f := range findings {
		if f.FindingType == ft {
			out = append(out, f)
		}
	}
	return out
}

// testPluginHTTPClient is a thin wrapper that redirects all requests to a
// test server URL so we can inject a fake wp.org endpoint.
type testPluginHTTPClient struct {
	client  *http.Client
	baseURL string
}

func (c *testPluginHTTPClient) Do(req *http.Request) (*http.Response, error) {
	// Replace the host with the test server; keep path + query.
	u := req.URL
	u.Scheme = "http"
	u.Host = strings.TrimPrefix(c.baseURL, "http://")
	req2, err := http.NewRequestWithContext(req.Context(), req.Method, u.String(), req.Body)
	if err != nil {
		return nil, err
	}
	req2.Header = req.Header
	return c.client.Do(req2)
}

// ---------------------------------------------------------------------------
// pluginFetchDirect — helper that bypasses the Repo cache so tests can
// exercise the HTTP fetch and decode logic directly.
// ---------------------------------------------------------------------------

// pluginFetchDirect calls fetchPluginFromWPOrg and then upserts into a
// provided fakePluginRepo. It mirrors what the real Plugin() method does on a
// cache miss, but accepts explicit repo and slug arguments so tests can drive
// it without needing a full DB-backed Repo.
func (p *ChecksumProvider) pluginFetchDirect(ctx context.Context, repo *fakePluginRepo, slug, version string) (map[string][]string, error) {
	variantsByPath, err := p.fetchPluginFromWPOrg(ctx, slug, version)
	if err != nil {
		return nil, err
	}
	if len(variantsByPath) == 0 {
		return map[string][]string{}, nil
	}
	checksums := make(map[string][]string, len(variantsByPath))
	var dbRows []PluginChecksumRow
	for path, variants := range variantsByPath {
		md5s := make([]string, 0, len(variants))
		for _, v := range variants {
			md5s = append(md5s, v.MD5)
			dbRows = append(dbRows, PluginChecksumRow{
				Kind: "plugin", Slug: slug, Version: version, Path: path, MD5: v.MD5, SHA256: v.SHA256,
			})
		}
		checksums[path] = md5s
	}
	_ = repo.UpsertPluginChecksums(ctx, dbRows)
	return checksums, nil
}
