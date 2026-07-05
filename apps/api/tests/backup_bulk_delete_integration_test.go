package tests

// backup_bulk_delete_integration_test.go — DB-real integration coverage for
// issue #115 (chain-aware bulk snapshot delete). Runs against a live Postgres
// (testcontainers) so the actual SQL behind Repo.GetSnapshotsByIDs and
// Repo.HasActiveRestore (the restore_runs JOIN backup_snapshots grouping
// query) is exercised for real, not just against an in-memory fake. See
// internal/backup/bulk_delete_test.go for the white-box unit coverage of the
// service-level guard/ordering/GC-count logic.

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/backup"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
)

// seedChainSnapshot inserts a completed backup_snapshots row directly with an
// explicit id/chain_id/generation/total_size, mirroring the real chain
// convention: the base (generation 0) has chain_id == its own id.
func seedChainSnapshot(t *testing.T, pool *db.Pool, tenant, siteID, id, chainID uuid.UUID, generation int, totalSize int64) {
	t.Helper()
	err := pool.InTenantTx(context.Background(), tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `
			INSERT INTO backup_snapshots
				(id, tenant_id, site_id, kind, status, age_recipient, total_size,
				 is_incremental, chain_id, generation)
			VALUES ($1, $2, $3, 'files', 'completed', $4, $5, $6, $7, $8)`,
			id, tenant, siteID, testRecipient, totalSize, generation > 0, chainID, generation,
		)
		return err
	})
	if err != nil {
		t.Fatalf("seed chain snapshot: %v", err)
	}
}

// countSnapshotRows returns how many of the given ids still exist as rows.
func countSnapshotRows(t *testing.T, pool *db.Pool, tenant uuid.UUID, ids []uuid.UUID) int {
	t.Helper()
	n := 0
	err := pool.InTenantTx(context.Background(), tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(context.Background(),
			`SELECT id FROM backup_snapshots WHERE tenant_id = $1 AND id = ANY($2)`, tenant, ids)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			n++
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("count snapshot rows: %v", err)
	}
	return n
}

// TestBulkDeleteSnapshots_RestoreInProgressGuardsWholeChain proves — against a
// REAL Postgres restore_runs JOIN backup_snapshots query — that an active
// restore anchored on the chain TIP blocks deleting every member of the chain,
// including ones never mentioned by the restore_runs row itself. Once the
// restore run is finalized, the same request succeeds and deletes newest-
// generation-first.
func TestBulkDeleteSnapshots_RestoreInProgressGuardsWholeChain(t *testing.T) {
	pool := startPostgres(t)
	store := startBlobstore(t)
	tenant := seedTenant(t, pool, "bulk-del-restore")
	siteID := seedSite(t, pool, tenant, "https://bulk-del-restore.example.com")

	svc := newBackupService(t, pool, store, stubSiteLookup{
		info: enrolledSiteInfo(siteID, "https://bulk-del-restore.example.com"),
	}, &stubEnqueuer{})

	baseID, midID, tipID := uuid.New(), uuid.New(), uuid.New()
	chainID := baseID // base's chain_id is its own id, by convention.
	seedChainSnapshot(t, pool, tenant, siteID, baseID, chainID, 0, 100)
	seedChainSnapshot(t, pool, tenant, siteID, midID, chainID, 1, 10)
	seedChainSnapshot(t, pool, tenant, siteID, tipID, chainID, 2, 20)

	restoreRepo := backup.NewRestoreRunRepo(pool)
	run, err := restoreRepo.CreateRestoreRun(context.Background(), backup.CreateRestoreRunInput{
		TenantID:   tenant,
		SiteID:     siteID,
		SnapshotID: tipID, // anchored on the TIP only.
		Mode:       "full",
	})
	if err != nil {
		t.Fatalf("create restore run: %v", err)
	}

	ids := []uuid.UUID{baseID, midID, tipID}

	// While the restore is active, EVERY member of the chain must be skipped —
	// including base/mid, which the restore_runs row never directly names.
	out, err := svc.BulkDeleteSnapshots(context.Background(), tenant, siteID, ids, false)
	if err != nil {
		t.Fatalf("BulkDeleteSnapshots (restore active): unexpected error: %v", err)
	}
	if out.Deleted != 0 || out.Skipped != 3 {
		t.Fatalf("counts = deleted:%d skipped:%d, want 0/3 while restore is active", out.Deleted, out.Skipped)
	}
	for _, r := range out.Results {
		if r.Outcome != backup.BulkDeleteOutcomeSkipped || r.Code != backup.SkipRestoreInProgress {
			t.Fatalf("id %s: outcome=%q code=%q, want skipped/restore_in_progress", r.ID, r.Outcome, r.Code)
		}
	}
	if got := countSnapshotRows(t, pool, tenant, ids); got != 3 {
		t.Fatalf("all 3 rows should still exist while restore is active, found %d", got)
	}

	// Finalize the restore run — the chain is now deletable.
	if err := restoreRepo.MarkRestoreRunStatus(context.Background(), backup.MarkRestoreRunStatusInput{
		TenantID:    tenant,
		RunID:       run.ID,
		Status:      backup.RestoreStatusCompleted,
		SetFinished: true,
	}); err != nil {
		t.Fatalf("finalize restore run: %v", err)
	}

	out2, err := svc.BulkDeleteSnapshots(context.Background(), tenant, siteID, ids, false)
	if err != nil {
		t.Fatalf("BulkDeleteSnapshots (restore finalized): unexpected error: %v", err)
	}
	if out2.Deleted != 3 || out2.Skipped != 0 {
		t.Fatalf("counts = deleted:%d skipped:%d, want 3/0 once the restore is finalized", out2.Deleted, out2.Skipped)
	}
	if out2.ReclaimedBytesEstimate != 130 {
		t.Fatalf("reclaimed_bytes_estimate = %d, want 130", out2.ReclaimedBytesEstimate)
	}
	if got := countSnapshotRows(t, pool, tenant, ids); got != 0 {
		t.Fatalf("all 3 rows should be gone, found %d remaining", got)
	}
}

// TestBulkDeleteSnapshots_DryRunLeavesRowsIntact proves dry_run=true against a
// REAL Postgres connection performs no mutation whatsoever: the exact same
// plan is computed (via the real GetSnapshotsByIDs query), but every row
// still exists afterward.
func TestBulkDeleteSnapshots_DryRunLeavesRowsIntact(t *testing.T) {
	pool := startPostgres(t)
	store := startBlobstore(t)
	tenant := seedTenant(t, pool, "bulk-del-dryrun")
	siteID := seedSite(t, pool, tenant, "https://bulk-del-dryrun.example.com")

	svc := newBackupService(t, pool, store, stubSiteLookup{
		info: enrolledSiteInfo(siteID, "https://bulk-del-dryrun.example.com"),
	}, &stubEnqueuer{})

	snap, err := svc.CreateBackup(context.Background(), tenant, siteID, uuid.Nil, "full")
	if err != nil {
		t.Fatalf("create backup: %v", err)
	}
	submitManifest(t, svc, tenant, snap.ID, minimalEntries(chunkHashes(1)))

	out, err := svc.BulkDeleteSnapshots(context.Background(), tenant, siteID, []uuid.UUID{snap.ID}, true)
	if err != nil {
		t.Fatalf("BulkDeleteSnapshots(dry_run): unexpected error: %v", err)
	}
	if !out.DryRun || out.Deleted != 1 {
		t.Fatalf("out = %+v, want DryRun=true Deleted=1", out)
	}
	if got := countSnapshotRows(t, pool, tenant, []uuid.UUID{snap.ID}); got != 1 {
		t.Fatalf("dry_run must not delete the row; found %d", got)
	}
}

// TestBulkDeleteSnapshots_SiteMismatchIsNotFound proves the real
// GetSnapshotsByIDs query, combined with the handler-level site check, treats
// a snapshot that belongs to the tenant but a DIFFERENT site exactly like
// "not found" rather than leaking its existence.
func TestBulkDeleteSnapshots_SiteMismatchIsNotFound(t *testing.T) {
	pool := startPostgres(t)
	store := startBlobstore(t)
	tenant := seedTenant(t, pool, "bulk-del-sitemismatch")
	siteA := seedSite(t, pool, tenant, "https://bulk-del-a.example.com")
	siteB := seedSite(t, pool, tenant, "https://bulk-del-b.example.com")

	svc := newBackupService(t, pool, store, stubSiteLookup{
		info: enrolledSiteInfo(siteB, "https://bulk-del-b.example.com"),
	}, &stubEnqueuer{})

	snap, err := svc.CreateBackup(context.Background(), tenant, siteB, uuid.Nil, "full")
	if err != nil {
		t.Fatalf("create backup on site B: %v", err)
	}

	// Request the deletion scoped to site A.
	out, err := svc.BulkDeleteSnapshots(context.Background(), tenant, siteA, []uuid.UUID{snap.ID}, false)
	if err != nil {
		t.Fatalf("BulkDeleteSnapshots: unexpected error: %v", err)
	}
	if len(out.Results) != 1 || out.Results[0].Outcome != backup.BulkDeleteOutcomeSkipped ||
		out.Results[0].Code != backup.SkipSnapshotNotFound {
		t.Fatalf("result = %+v, want skipped/snapshot_not_found", out.Results)
	}
	if got := countSnapshotRows(t, pool, tenant, []uuid.UUID{snap.ID}); got != 1 {
		t.Fatalf("site B's snapshot must be untouched, found %d", got)
	}
}
