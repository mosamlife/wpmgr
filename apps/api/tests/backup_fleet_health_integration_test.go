package tests

// backup_fleet_health_integration_test.go — DB-real integration coverage for
// GH #214: GET /api/v1/backups/health returned 500 (fleet_backup_health_failed)
// whenever ANY in-scope site had never had a completed backup snapshot.
//
// Root cause: FleetBackupHealth's latest_size_bytes scalar subquery over
// backup_snapshots.total_size (bigint NOT NULL -> sqlc int64) returns SQL NULL
// when a site has zero completed snapshots. pgx then fails at rows.Scan with
// "cannot scan NULL into *int64", which repo.go wraps as
// domain.Internal("fleet_backup_health_failed") -> 500 for the WHOLE fleet
// response, not just the unbacked site's row. The fix wraps that subquery in
// COALESCE(..., 0), mirroring the existing precedent in perf.sql.
//
// This test seeds one site with a completed snapshot (real size) and one site
// with NO snapshots at all (and zero site_destinations rows, ruling out the
// reporter's site_destinations red herring), then calls the real
// backup.Service.FleetBackupHealth -> pgRepo.FleetBackupHealth -> the actual
// SQL against a live Postgres. Before the fix this fails with a NULL-scan
// error; after the fix it returns 200-equivalent (no error), one item per
// site, with the unbacked site classified Unprotected and
// LatestSizeBytes == 0.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/backup"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// seedCompletedSnapshot inserts a completed backup_snapshots row with an
// explicit total_size and finished_at (both required for FleetBackupHealth's
// last_completed_at / latest_size_bytes correlated subqueries to resolve
// non-NULL for this site).
func seedCompletedSnapshot(t *testing.T, pool *db.Pool, tenant, siteID uuid.UUID, totalSize int64, finishedAt time.Time) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pool.InTenantTx(context.Background(), tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `
			INSERT INTO backup_snapshots
				(tenant_id, site_id, kind, status, age_recipient, total_size, started_at, finished_at)
			VALUES ($1, $2, 'files', 'completed', $3, $4, $5, $5)
			RETURNING id`,
			tenant, siteID, testRecipient, totalSize, finishedAt,
		).Scan(&id)
	})
	if err != nil {
		t.Fatalf("seed completed snapshot: %v", err)
	}
	return id
}

// TestFleetBackupHealth_UnbackedSiteDoesNotFailWholeFleet is the regression
// test for GH #214.
func TestFleetBackupHealth_UnbackedSiteDoesNotFailWholeFleet(t *testing.T) {
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "fleet-health-214")

	backedSite := seedSite(t, pool, tenant, "https://fleet-health-214-backed.example.com")
	unbackedSite := seedSite(t, pool, tenant, "https://fleet-health-214-unbacked.example.com")

	// A completed snapshot with a real size for the "backed" site.
	seedCompletedSnapshot(t, pool, tenant, backedSite, 12345, time.Now().Add(-time.Hour))

	// The "unbacked" site has ZERO snapshots and ZERO site_destinations rows
	// (the reporter's red herring: FleetBackupHealth never references
	// site_destinations at all, so this is deliberately left empty).

	repo := backup.NewRepo(pool)
	p := domain.Principal{TenantID: tenant, Scope: ""} // org-scoped, matches the fleet dashboard's real caller shape

	items, err := repo.FleetBackupHealth(context.Background(), p, tenant, []uuid.UUID{backedSite, unbackedSite})
	if err != nil {
		t.Fatalf("FleetBackupHealth: unexpected error (this is the GH #214 500): %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("FleetBackupHealth: expected 2 items, got %d", len(items))
	}

	var backed, unbacked *backup.FleetBackupHealthItem
	for i := range items {
		switch items[i].SiteID {
		case backedSite:
			backed = &items[i]
		case unbackedSite:
			unbacked = &items[i]
		}
	}
	if backed == nil || unbacked == nil {
		t.Fatalf("FleetBackupHealth: missing expected site(s) in %+v", items)
	}

	if unbacked.LatestSizeBytes != 0 {
		t.Fatalf("unbacked site LatestSizeBytes = %d, want 0", unbacked.LatestSizeBytes)
	}
	if unbacked.Status != backup.HealthStatusUnprotected {
		t.Fatalf("unbacked site Status = %q, want %q", unbacked.Status, backup.HealthStatusUnprotected)
	}
	if unbacked.LastCompletedAt != nil {
		t.Fatalf("unbacked site LastCompletedAt = %v, want nil", unbacked.LastCompletedAt)
	}

	if backed.LatestSizeBytes != 12345 {
		t.Fatalf("backed site LatestSizeBytes = %d, want 12345", backed.LatestSizeBytes)
	}
	if backed.LastCompletedAt == nil {
		t.Fatal("backed site LastCompletedAt = nil, want a timestamp")
	}
}
