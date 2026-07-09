// Integration tests for GH #196: the daily diagnostics push now taps
// wp-paths-sizes.fields.database_size to append a site_db_size_history point
// automatically, alongside the pre-existing manual "Scan database" writer
// (perf.Repo.UpsertDBScanResult). Exercises the real diagnostics.Service +
// diagnostics.Repo + perf.Repo against a real Postgres (testcontainers) so
// the RLS-scoped InTenantTx write path and the (site_id, scanned_at)
// idempotent-insert constraint are genuinely proven, not mocked.
//
// Requires Docker; skips (via startPostgres) when unavailable.
package tests

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/diagnostics"
	"github.com/mosamlife/wpmgr/apps/api/internal/perf"
)

// newDiagnosticsDBSizeHarness wires the real diagnostics.Service against the
// real diagnostics.Repo + perf.Repo (the DBSizeHistorySink adapter), mirroring
// the cmd/wpmgr wiring (diagnosticsSvc.SetDBSizeHistorySink(perfRepo)).
func newDiagnosticsDBSizeHarness(pool *db.Pool) (*diagnostics.Service, *perf.Repo) {
	diagRepo := diagnostics.NewRepo(pool)
	perfRepo := perf.NewRepo(pool)
	svc := diagnostics.NewService(diagRepo)
	svc.SetDBSizeHistorySink(perfRepo)
	return svc, perfRepo
}

// wpNativePayloadJSON builds a full diagnostics push body carrying only the
// wp_native category, shaped like the agent's real
// WP_Debug_Data::debug_data() + SizeProbe merge output
// (wp-paths-sizes.fields.database_size.debug). collectedAt is the top-level
// agent-side collection timestamp (unix seconds) IngestDiagnostics applies to
// every category row — and the value the DB-size tap uses as scanned_at.
func wpNativePayloadJSON(dbSizeBytes int64, collectedAt time.Time) []byte {
	return []byte(fmt.Sprintf(
		`{"wp_native":{"wp-paths-sizes":{"fields":{"database_size":{"value":"size","debug":%d}}}},"collected_at":%d}`,
		dbSizeBytes, collectedAt.Unix(),
	))
}

// TestDiagnosticsPushRecordsDBSizeHistoryPoint proves the happy path: a
// diagnostics push carrying a positive wp-paths-sizes database_size appends
// exactly one site_db_size_history point for the right site/tenant, with the
// right byte count and scanned_at == the push's own collected_at.
func TestDiagnosticsPushRecordsDBSizeHistoryPoint(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()

	tenantID := seedTenant(t, pool, "dbsize-happy-"+uuid.NewString()[:8])
	siteID := seedSiteFor(t, admin, tenantID, "https://"+uuid.NewString()+".example.com")

	svc, perfRepo := newDiagnosticsDBSizeHarness(pool)
	collectedAt := time.Now().UTC().Truncate(time.Second)

	const dbSizeBytes = int64(1288490188) // ~1.2 GB
	body := wpNativePayloadJSON(dbSizeBytes, collectedAt)

	count, err := svc.IngestDiagnostics(ctx, tenantID, siteID, body)
	if err != nil {
		t.Fatalf("IngestDiagnostics: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 category ingested (wp_native only), got %d", count)
	}

	points, err := perfRepo.GetDBSizeHistory(ctx, tenantID, siteID, collectedAt.Add(-time.Hour))
	if err != nil {
		t.Fatalf("GetDBSizeHistory: %v", err)
	}
	if len(points) != 1 {
		t.Fatalf("expected exactly 1 db-size history point, got %d: %+v", len(points), points)
	}
	if points[0].DBSizeBytes != dbSizeBytes {
		t.Errorf("db_size_bytes = %d, want %d", points[0].DBSizeBytes, dbSizeBytes)
	}
	if !points[0].ScannedAt.Equal(collectedAt) {
		t.Errorf("scanned_at = %v, want %v (the push's own collected_at)", points[0].ScannedAt, collectedAt)
	}
	// No prior scan/history row exists for this brand-new site, so
	// table_count must carry forward to the documented default of 0.
	if points[0].TableCount != 0 {
		t.Errorf("table_count = %d, want 0 (no prior history row to carry forward)", points[0].TableCount)
	}
}

// TestDiagnosticsPushSkipsMissingOrZeroDBSize proves the guard: a push whose
// wp_native category has no usable database_size (missing field, zero bytes,
// or WP's un-resolved "Loading…" placeholder) must NOT write a history point.
func TestDiagnosticsPushSkipsMissingOrZeroDBSize(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()

	tenantID := seedTenant(t, pool, "dbsize-skip-"+uuid.NewString()[:8])
	siteID := seedSiteFor(t, admin, tenantID, "https://"+uuid.NewString()+".example.com")

	svc, perfRepo := newDiagnosticsDBSizeHarness(pool)
	collectedAt := time.Now().UTC().Truncate(time.Second)

	cases := []struct {
		name string
		body []byte
	}{
		{
			name: "database_size field entirely absent",
			body: []byte(fmt.Sprintf(`{"wp_native":{"wp-paths-sizes":{"fields":{}}},"collected_at":%d}`, collectedAt.Unix())),
		},
		{
			name: "database_size.debug is zero",
			body: wpNativePayloadJSON(0, collectedAt),
		},
		{
			name: "database_size.debug is the WP un-resolved placeholder string",
			body: []byte(fmt.Sprintf(
				`{"wp_native":{"wp-paths-sizes":{"fields":{"database_size":{"value":"Loading&hellip;","debug":"Loading&hellip;"}}}},"collected_at":%d}`,
				collectedAt.Unix(),
			)),
		},
		{
			name: "wp-paths-sizes section entirely absent",
			body: []byte(fmt.Sprintf(`{"wp_native":{"wp-core":{"fields":{}}},"collected_at":%d}`, collectedAt.Unix())),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			count, err := svc.IngestDiagnostics(ctx, tenantID, siteID, tc.body)
			if err != nil {
				t.Fatalf("IngestDiagnostics: %v", err)
			}
			if count != 1 {
				t.Fatalf("expected 1 category ingested (wp_native), got %d", count)
			}
			points, err := perfRepo.GetDBSizeHistory(ctx, tenantID, siteID, collectedAt.Add(-time.Hour))
			if err != nil {
				t.Fatalf("GetDBSizeHistory: %v", err)
			}
			if len(points) != 0 {
				t.Fatalf("expected no db-size history point, got %d: %+v", len(points), points)
			}
		})
	}
}

// TestDiagnosticsPushDBSizeHistoryInsertFailureIsNonFatal proves the sink is
// best-effort: when the history-sink write fails, IngestDiagnostics must
// still succeed (the 15-category upsert has already landed) — a downstream
// history-table outage must never turn the agent's daily diagnostics push
// into a failed request.
func TestDiagnosticsPushDBSizeHistoryInsertFailureIsNonFatal(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()

	tenantID := seedTenant(t, pool, "dbsize-failsafe-"+uuid.NewString()[:8])
	siteID := seedSiteFor(t, admin, tenantID, "https://"+uuid.NewString()+".example.com")

	diagRepo := diagnostics.NewRepo(pool)
	svc := diagnostics.NewService(diagRepo)
	sink := &failingDBSizeHistorySink{}
	svc.SetDBSizeHistorySink(sink)

	collectedAt := time.Now().UTC().Truncate(time.Second)
	body := wpNativePayloadJSON(1288490188, collectedAt)

	count, err := svc.IngestDiagnostics(ctx, tenantID, siteID, body)
	if err != nil {
		t.Fatalf("IngestDiagnostics must succeed even when the history sink fails, got: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 category ingested (wp_native), got %d", count)
	}
	if !sink.called {
		t.Fatal("expected the (failing) DBSizeHistorySink to have been invoked")
	}

	// The core diagnostics row must still be persisted — the failure is
	// scoped entirely to the best-effort history tap.
	rows, err := diagRepo.ListDiagnosticsBySite(ctx, tenantID, siteID)
	if err != nil {
		t.Fatalf("ListDiagnosticsBySite: %v", err)
	}
	if len(rows) != 1 || rows[0].Category != diagnostics.CategoryWPNative {
		t.Fatalf("expected the wp_native diagnostics row to be stored despite the history-sink failure, got: %+v", rows)
	}
}

// TestDiagnosticsPushCarriesForwardTableCount proves table_count is carried
// forward from the most recent site_db_size_history row (a prior manual scan
// or a prior diagnostics-sourced point) rather than always writing 0 — the
// diagnostics push itself never carries a table inventory.
func TestDiagnosticsPushCarriesForwardTableCount(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()

	tenantID := seedTenant(t, pool, "dbsize-carry-"+uuid.NewString()[:8])
	siteID := seedSiteFor(t, admin, tenantID, "https://"+uuid.NewString()+".example.com")

	// Seed a prior history point (standing in for an earlier manual "Scan
	// database" run) with a known table_count, via the superuser admin pool
	// (bypasses RLS for fixture setup, mirroring seedSiteFor above).
	priorScannedAt := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Second)
	const priorTableCount = 42
	if _, err := admin.Exec(ctx,
		`INSERT INTO site_db_size_history (site_id, tenant_id, db_size_bytes, table_count, scanned_at)
		 VALUES ($1, $2, $3, $4, $5)`,
		siteID, tenantID, int64(900000000), priorTableCount, priorScannedAt,
	); err != nil {
		t.Fatalf("seed prior db-size history point: %v", err)
	}

	svc, perfRepo := newDiagnosticsDBSizeHarness(pool)
	collectedAt := time.Now().UTC().Truncate(time.Second)
	body := wpNativePayloadJSON(1288490188, collectedAt)

	if _, err := svc.IngestDiagnostics(ctx, tenantID, siteID, body); err != nil {
		t.Fatalf("IngestDiagnostics: %v", err)
	}

	points, err := perfRepo.GetDBSizeHistory(ctx, tenantID, siteID, priorScannedAt.Add(-time.Hour))
	if err != nil {
		t.Fatalf("GetDBSizeHistory: %v", err)
	}
	if len(points) != 2 {
		t.Fatalf("expected 2 db-size history points (prior + diagnostics-sourced), got %d: %+v", len(points), points)
	}
	// Points are ordered oldest-first; the newest (diagnostics-sourced) point
	// must carry forward the prior point's table_count.
	newest := points[len(points)-1]
	if !newest.ScannedAt.Equal(collectedAt) {
		t.Fatalf("newest point scanned_at = %v, want %v", newest.ScannedAt, collectedAt)
	}
	if newest.TableCount != priorTableCount {
		t.Errorf("table_count = %d, want %d (carried forward from the prior history row)", newest.TableCount, priorTableCount)
	}
}

// failingDBSizeHistorySink is a diagnostics.DBSizeHistorySink fake that always
// errors, used to prove the tap is best-effort/non-fatal.
type failingDBSizeHistorySink struct {
	called bool
}

func (f *failingDBSizeHistorySink) RecordDBSizeHistoryFromDiagnostics(ctx context.Context, tenantID, siteID uuid.UUID, dbSizeBytes int64, scannedAt time.Time) error {
	f.called = true
	return errors.New("boom: history sink unavailable")
}
