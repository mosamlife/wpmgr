// scan_plugin_checksums_regression_test.go: regression coverage for a MUST-FIX
// bug introduced by the wp.org sha256-storage change (m106). repo.go's
// UpsertPluginChecksums moved from ON CONFLICT DO NOTHING to ON CONFLICT DO
// UPDATE so the new sha256 column could be backfilled, but the batch was not
// deduplicated by its conflict key first. A single INSERT ... ON CONFLICT DO
// UPDATE statement cannot affect the same (kind, slug, version, path, md5)
// row twice in one statement; Postgres raises cardinality_violation ("ON
// CONFLICT DO UPDATE command cannot affect row a second time") when it does,
// which decodeMD5Variants can produce from real wp.org data (a duplicate md5
// listed twice in the same file's accepted-variant array). The old DO NOTHING
// form tolerated this; the new form aborted the whole batch, and the caller
// swallowed the error while still stamping the 30-day positive freshness
// cache over zero written rows, silently disabling plugin file-integrity
// checksum comparison with no log and no finding for 30 days.
//
// These tests run against a real Postgres 16 (testcontainers) so the actual
// primary-key/CHECK constraints and the real UpsertPluginChecksums SQL are
// exercised, not an in-memory fake. See
// internal/scan/integrity_test.go:TestDedupePluginChecksumRows_* for the
// white-box (no-DB) coverage of the same dedup property.
package tests

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mosamlife/wpmgr/apps/api/internal/scan"
)

// TestScanPluginChecksums_DuplicateConflictKeyDoesNotAbortBatch reproduces the
// PG16 cardinality_violation directly against UpsertPluginChecksums and
// proves the repo-level dedup fix keeps the whole batch alive, applying the
// last occurrence of a duplicated key (last-write-wins; see
// dedupePluginChecksumRows).
func TestScanPluginChecksums_DuplicateConflictKeyDoesNotAbortBatch(t *testing.T) {
	t.Parallel()
	pool := startPostgres(t)
	repo := scan.NewRepo(pool)
	ctx := context.Background()

	rows := []scan.PluginChecksumRow{
		{Kind: "plugin", Slug: "dupe-plugin", Version: "1.0.0", Path: "dupe.php", MD5: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", SHA256: "sha-first"},
		// Same ON CONFLICT (kind, slug, version, path, md5) target as above,
		// mirroring wp.org listing a duplicate md5 within one file's
		// accepted-variant array.
		{Kind: "plugin", Slug: "dupe-plugin", Version: "1.0.0", Path: "dupe.php", MD5: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", SHA256: "sha-second"},
		{Kind: "plugin", Slug: "dupe-plugin", Version: "1.0.0", Path: "other.php", MD5: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", SHA256: "sha-other"},
	}

	if err := repo.UpsertPluginChecksums(ctx, rows); err != nil {
		t.Fatalf("UpsertPluginChecksums with a duplicate conflict key in the same batch must not error, got: %v", err)
	}

	got, err := repo.GetPluginChecksums(ctx, "plugin", "dupe-plugin", "1.0.0")
	if err != nil {
		t.Fatalf("GetPluginChecksums: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 persisted rows (one per distinct md5), got %d: %+v", len(got), got)
	}
	found := false
	for _, row := range got {
		if row.Path != "dupe.php" {
			continue
		}
		found = true
		if row.SHA256 != "sha-second" {
			t.Errorf("expected last-write-wins sha256 %q for the duplicated key, got %q", "sha-second", row.SHA256)
		}
	}
	if !found {
		t.Fatal("expected a persisted row for dupe.php")
	}
}

// TestScanPluginChecksums_FreshnessMetaNotStampedOnPersistFailure verifies
// that when the checksum persist genuinely fails (here, a CHECK-constraint
// violation on kind, independent of the dedup fix), ChecksumProvider.Plugin
// does NOT stamp the positive freshness cache. Stamping it despite the
// failure is the severity multiplier of this bug: it would silently disable
// plugin file-integrity checksum comparison for the full 30-day TTL with no
// log and no finding.
func TestScanPluginChecksums_FreshnessMetaNotStampedOnPersistFailure(t *testing.T) {
	t.Parallel()
	pool := startPostgres(t)
	repo := scan.NewRepo(pool)
	ctx := context.Background()

	payload := `{"files":{"broken.php":{"md5":"cccccccccccccccccccccccccccccccc"}}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(payload))
	}))
	defer srv.Close()

	provider := scan.NewChecksumProvider(repo, &redirectHTTPDoer{client: srv.Client(), baseURL: srv.URL}, nil)

	// "bogus" fails wporg_plugin_checksums_kind_chk (kind must be 'plugin' or
	// 'theme'), forcing a genuine persist failure downstream of a successful
	// HTTP fetch: exactly the scenario where the old code silently stamped
	// the positive freshness cache over zero written rows.
	result, err := provider.Plugin(ctx, "bogus", "broken-plugin", "1.0.0")
	if err != nil {
		t.Fatalf("Plugin must degrade gracefully (nil error) even when the persist fails, got: %v", err)
	}
	if len(result) == 0 {
		t.Error("expected the in-memory fetch result to still be returned for this scan even though persist failed")
	}

	_, _, metaFound, metaErr := repo.GetPluginChecksumsMeta(ctx, "bogus", "broken-plugin", "1.0.0")
	if metaErr != nil {
		t.Fatalf("GetPluginChecksumsMeta: %v", metaErr)
	}
	if metaFound {
		t.Error("freshness meta must NOT be stamped when the checksum persist failed")
	}

	persisted, rowsErr := repo.GetPluginChecksums(ctx, "bogus", "broken-plugin", "1.0.0")
	if rowsErr != nil {
		t.Fatalf("GetPluginChecksums: %v", rowsErr)
	}
	if len(persisted) != 0 {
		t.Errorf("expected 0 persisted rows after a failed insert, got %d", len(persisted))
	}
}

// redirectHTTPDoer redirects every request to a fixed test-server base URL so
// scan.ChecksumProvider's wp.org fetch can be pointed at an httptest.Server.
// It implements the exported scan.HTTPDoer interface (internal/scan has its
// own private equivalent used by its in-package tests).
type redirectHTTPDoer struct {
	client  *http.Client
	baseURL string
}

func (c *redirectHTTPDoer) Do(req *http.Request) (*http.Response, error) {
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
