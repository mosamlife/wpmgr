package backup

// bulk_delete_test.go — unit tests for issue #115 (chain-aware bulk snapshot
// delete). White-box, in-memory fakes only (reuses deleteCancelFakeRepo from
// delete_cancel_test.go); no DB or network. See tests/backup_integration_test.go
// for the DB-real counterpart that exercises the actual GetSnapshotsByIDs /
// HasActiveRestore SQL against a live Postgres.

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// resultFor returns the result row for id, failing the test if absent.
func resultFor(t *testing.T, out BulkDeleteOutput, id uuid.UUID) BulkDeleteResult {
	t.Helper()
	for _, r := range out.Results {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("no result row for id %s (results: %+v)", id, out.Results)
	return BulkDeleteResult{}
}

// --- chain orphan refusal ----------------------------------------------------

// TestBulkDelete_ChainOrphanRefusal: base + mid-chain increment requested, the
// tip is NOT in the request. Both requested ids must be skipped
// chain_has_dependents (the live, un-requested tip still depends on them), and
// the tip itself must remain completely untouched.
func TestBulkDelete_ChainOrphanRefusal(t *testing.T) {
	repo := newDeleteCancelFakeRepo()
	svc := buildDeleteCancelSvc(repo)
	tenantID, siteID := uuid.New(), uuid.New()
	chainID := uuid.New()
	base := Snapshot{ID: chainID, TenantID: tenantID, SiteID: siteID, Status: StatusCompleted, ChainID: &chainID, Generation: 0}
	mid := Snapshot{ID: uuid.New(), TenantID: tenantID, SiteID: siteID, Status: StatusCompleted, ChainID: &chainID, Generation: 1, IsIncremental: true, ParentSnapshotID: &base.ID}
	tip := Snapshot{ID: uuid.New(), TenantID: tenantID, SiteID: siteID, Status: StatusCompleted, ChainID: &chainID, Generation: 2, IsIncremental: true, ParentSnapshotID: &mid.ID}
	repo.addChainSnap(chainID, base)
	repo.addChainSnap(chainID, mid)
	repo.addChainSnap(chainID, tip)

	out, err := svc.BulkDeleteSnapshots(context.Background(), tenantID, siteID, []uuid.UUID{base.ID, mid.ID}, false)
	if err != nil {
		t.Fatalf("BulkDeleteSnapshots: unexpected error: %v", err)
	}
	if out.Deleted != 0 || out.Skipped != 2 {
		t.Fatalf("counts = deleted:%d skipped:%d, want 0/2", out.Deleted, out.Skipped)
	}
	for _, id := range []uuid.UUID{base.ID, mid.ID} {
		r := resultFor(t, out, id)
		if r.Outcome != BulkDeleteOutcomeSkipped || r.Code != SkipChainHasDependents {
			t.Fatalf("id %s: outcome=%q code=%q, want skipped/chain_has_dependents", id, r.Outcome, r.Code)
		}
	}
	if repo.deleted[base.ID] || repo.deleted[mid.ID] || repo.deleted[tip.ID] {
		t.Fatalf("nothing should have been deleted; deleted=%v", repo.deleted)
	}
	if _, ok := repo.snapshots[tip.ID]; !ok {
		t.Fatalf("tip must remain untouched")
	}
}

// --- newest-first delete order -----------------------------------------------

// TestBulkDelete_DeleteOrderNewestFirst: requesting a FULL chain (base + every
// increment) deletes all of them, in strictly descending generation order
// (tip first, base last) — the invariant that keeps every surviving snapshot's
// chain contiguous 0..tip at every intermediate point of the request.
func TestBulkDelete_DeleteOrderNewestFirst(t *testing.T) {
	repo := newDeleteCancelFakeRepo()
	svc := buildDeleteCancelSvc(repo)
	tenantID, siteID := uuid.New(), uuid.New()
	chainID := uuid.New()
	base := Snapshot{ID: chainID, TenantID: tenantID, SiteID: siteID, Status: StatusCompleted, ChainID: &chainID, Generation: 0, TotalSize: 100}
	gen1 := Snapshot{ID: uuid.New(), TenantID: tenantID, SiteID: siteID, Status: StatusCompleted, ChainID: &chainID, Generation: 1, IsIncremental: true, TotalSize: 10, ParentSnapshotID: &base.ID}
	gen2 := Snapshot{ID: uuid.New(), TenantID: tenantID, SiteID: siteID, Status: StatusCompleted, ChainID: &chainID, Generation: 2, IsIncremental: true, TotalSize: 20, ParentSnapshotID: &gen1.ID}
	repo.addChainSnap(chainID, base)
	repo.addChainSnap(chainID, gen1)
	repo.addChainSnap(chainID, gen2)

	ids := []uuid.UUID{base.ID, gen1.ID, gen2.ID}
	out, err := svc.BulkDeleteSnapshots(context.Background(), tenantID, siteID, ids, false)
	if err != nil {
		t.Fatalf("BulkDeleteSnapshots: unexpected error: %v", err)
	}
	if out.Deleted != 3 || out.Skipped != 0 {
		t.Fatalf("counts = deleted:%d skipped:%d, want 3/0", out.Deleted, out.Skipped)
	}
	for _, id := range ids {
		r := resultFor(t, out, id)
		if r.Outcome != BulkDeleteOutcomeDeleted {
			t.Fatalf("id %s: outcome=%q, want deleted", id, r.Outcome)
		}
	}
	if out.ReclaimedBytesEstimate != 130 {
		t.Fatalf("reclaimed_bytes_estimate = %d, want 130", out.ReclaimedBytesEstimate)
	}
	if len(repo.deleteOrder) != 3 {
		t.Fatalf("deleteOrder = %v, want 3 entries", repo.deleteOrder)
	}
	wantOrder := []uuid.UUID{gen2.ID, gen1.ID, base.ID}
	for i, want := range wantOrder {
		if repo.deleteOrder[i] != want {
			t.Fatalf("deleteOrder[%d] = %s, want %s (generation-descending)", i, repo.deleteOrder[i], want)
		}
	}
}

// --- locked exclusion ---------------------------------------------------------

// TestBulkDelete_LockedExclusion: a locked snapshot in the batch is skipped
// (never silently unlocked) and remains locked + present afterward.
func TestBulkDelete_LockedExclusion(t *testing.T) {
	repo := newDeleteCancelFakeRepo()
	svc := buildDeleteCancelSvc(repo)
	tenantID, siteID := uuid.New(), uuid.New()
	locked := Snapshot{ID: uuid.New(), TenantID: tenantID, SiteID: siteID, Status: StatusCompleted, Locked: true}
	normal := Snapshot{ID: uuid.New(), TenantID: tenantID, SiteID: siteID, Status: StatusCompleted}
	repo.setSnapshot(locked)
	repo.setSnapshot(normal)

	out, err := svc.BulkDeleteSnapshots(context.Background(), tenantID, siteID, []uuid.UUID{locked.ID, normal.ID}, false)
	if err != nil {
		t.Fatalf("BulkDeleteSnapshots: unexpected error: %v", err)
	}
	if out.Deleted != 1 || out.Skipped != 1 {
		t.Fatalf("counts = deleted:%d skipped:%d, want 1/1", out.Deleted, out.Skipped)
	}
	r := resultFor(t, out, locked.ID)
	if r.Outcome != BulkDeleteOutcomeSkipped || r.Code != SkipSnapshotLocked {
		t.Fatalf("locked id: outcome=%q code=%q, want skipped/snapshot_locked", r.Outcome, r.Code)
	}
	if repo.deleted[locked.ID] {
		t.Fatalf("locked snapshot must not be deleted")
	}
	if s := repo.snapshots[locked.ID]; !s.Locked {
		t.Fatalf("locked snapshot must remain locked (never silently unlocked)")
	}
	if !repo.deleted[normal.ID] {
		t.Fatalf("the non-locked snapshot should have been deleted")
	}
}

// --- in-progress skip ---------------------------------------------------------

// TestBulkDelete_InProgressSkipped: a running or pending snapshot is skipped
// snapshot_in_progress rather than aborting the whole batch.
func TestBulkDelete_InProgressSkipped(t *testing.T) {
	for _, status := range []string{StatusRunning, StatusPending} {
		repo := newDeleteCancelFakeRepo()
		svc := buildDeleteCancelSvc(repo)
		tenantID, siteID := uuid.New(), uuid.New()
		inFlight := Snapshot{ID: uuid.New(), TenantID: tenantID, SiteID: siteID, Status: status}
		done := Snapshot{ID: uuid.New(), TenantID: tenantID, SiteID: siteID, Status: StatusCompleted}
		repo.setSnapshot(inFlight)
		repo.setSnapshot(done)

		out, err := svc.BulkDeleteSnapshots(context.Background(), tenantID, siteID, []uuid.UUID{inFlight.ID, done.ID}, false)
		if err != nil {
			t.Fatalf("status %q: BulkDeleteSnapshots: unexpected error: %v", status, err)
		}
		r := resultFor(t, out, inFlight.ID)
		if r.Outcome != BulkDeleteOutcomeSkipped || r.Code != SkipSnapshotInProgress {
			t.Fatalf("status %q: outcome=%q code=%q, want skipped/snapshot_in_progress", status, r.Outcome, r.Code)
		}
		if repo.deleted[inFlight.ID] {
			t.Fatalf("status %q: in-flight snapshot must not be deleted", status)
		}
		if !repo.deleted[done.ID] {
			t.Fatalf("status %q: the completed snapshot should have been deleted", status)
		}
	}
}

// --- restore-in-progress guard (bulk + single) --------------------------------

// TestBulkDelete_RestoreInProgressSkipped: an active restore anchored on ANY
// member of a chain must skip EVERY requested id in that chain — not just the
// exact snapshot the restore targeted — because a restore reads the WHOLE
// chain up to its target generation.
func TestBulkDelete_RestoreInProgressSkipped(t *testing.T) {
	repo := newDeleteCancelFakeRepo()
	svc := buildDeleteCancelSvc(repo)
	tenantID, siteID := uuid.New(), uuid.New()
	chainID := uuid.New()
	base := Snapshot{ID: chainID, TenantID: tenantID, SiteID: siteID, Status: StatusCompleted, ChainID: &chainID, Generation: 0}
	tip := Snapshot{ID: uuid.New(), TenantID: tenantID, SiteID: siteID, Status: StatusCompleted, ChainID: &chainID, Generation: 1, IsIncremental: true}
	repo.addChainSnap(chainID, base)
	repo.addChainSnap(chainID, tip)
	// The active restore targets the TIP, but only the BASE is requested for
	// delete — the base must still be refused because the restore reads the
	// whole chain (grouped by chain_id, which is what restoreGroupKey returns
	// for every member of this chain).
	repo.activeRestores[chainID] = true

	out, err := svc.BulkDeleteSnapshots(context.Background(), tenantID, siteID, []uuid.UUID{base.ID}, false)
	if err != nil {
		t.Fatalf("BulkDeleteSnapshots: unexpected error: %v", err)
	}
	r := resultFor(t, out, base.ID)
	if r.Outcome != BulkDeleteOutcomeSkipped || r.Code != SkipRestoreInProgress {
		t.Fatalf("outcome=%q code=%q, want skipped/restore_in_progress", r.Outcome, r.Code)
	}
	if repo.deleted[base.ID] {
		t.Fatalf("base must not be deleted while its chain has an active restore")
	}
}

// TestDeleteSnapshotForUser_RestoreInProgressRefused is the single-delete
// counterpart: DeleteSnapshotForUser previously had NO restore_in_progress
// guard at all (issue #115 closes that gap). A standalone snapshot with an
// active restore targeting it must be refused, not deleted.
func TestDeleteSnapshotForUser_RestoreInProgressRefused(t *testing.T) {
	repo := newDeleteCancelFakeRepo()
	svc := buildDeleteCancelSvc(repo)
	tenantID := uuid.New()
	snap := Snapshot{ID: uuid.New(), TenantID: tenantID, SiteID: uuid.New(), Status: StatusCompleted}
	repo.setSnapshot(snap)
	// Standalone: restoreGroupKey(snap) == snap.ID.
	repo.activeRestores[snap.ID] = true

	err := svc.DeleteSnapshotForUser(context.Background(), tenantID, snap.ID)
	de, ok := domain.AsDomain(err)
	if !ok || de.Kind != domain.KindValidation || de.Code != SkipRestoreInProgress {
		t.Fatalf("err = %v, want Validation restore_in_progress", err)
	}
	if repo.deleted[snap.ID] {
		t.Fatalf("snapshot must not be deleted while an active restore reads it")
	}
}

// --- single GC pass ------------------------------------------------------------

// TestBulkDelete_SingleGCPass: deleting N snapshots in one bulk call must run
// the retention GC exactly ONCE — never once per deleted id (each pass
// re-marks every retained manifest tenant-wide).
func TestBulkDelete_SingleGCPass(t *testing.T) {
	repo := newDeleteCancelFakeRepo()
	svc := buildDeleteCancelSvc(repo)
	tenantID, siteID := uuid.New(), uuid.New()
	ids := make([]uuid.UUID, 0, 5)
	for i := 0; i < 5; i++ {
		s := Snapshot{ID: uuid.New(), TenantID: tenantID, SiteID: siteID, Status: StatusCompleted}
		repo.setSnapshot(s)
		ids = append(ids, s.ID)
	}

	out, err := svc.BulkDeleteSnapshots(context.Background(), tenantID, siteID, ids, false)
	if err != nil {
		t.Fatalf("BulkDeleteSnapshots: unexpected error: %v", err)
	}
	if out.Deleted != 5 {
		t.Fatalf("deleted = %d, want 5", out.Deleted)
	}
	if repo.gcCalls != 1 {
		t.Fatalf("gcCalls = %d, want exactly 1 for a 5-snapshot batch", repo.gcCalls)
	}
}

// TestBulkDelete_SingleGCPass_SkipEverythingRunsNoGC: when NOTHING was
// actually deleted (every id skipped), the GC must not run at all — there is
// nothing new to reclaim, so a pass would be pure waste.
func TestBulkDelete_SingleGCPass_SkipEverythingRunsNoGC(t *testing.T) {
	repo := newDeleteCancelFakeRepo()
	svc := buildDeleteCancelSvc(repo)
	tenantID, siteID := uuid.New(), uuid.New()
	locked := Snapshot{ID: uuid.New(), TenantID: tenantID, SiteID: siteID, Status: StatusCompleted, Locked: true}
	repo.setSnapshot(locked)

	out, err := svc.BulkDeleteSnapshots(context.Background(), tenantID, siteID, []uuid.UUID{locked.ID}, false)
	if err != nil {
		t.Fatalf("BulkDeleteSnapshots: unexpected error: %v", err)
	}
	if out.Deleted != 0 || out.Skipped != 1 {
		t.Fatalf("counts = deleted:%d skipped:%d, want 0/1", out.Deleted, out.Skipped)
	}
	if repo.gcCalls != 0 {
		t.Fatalf("gcCalls = %d, want 0 when nothing was deleted", repo.gcCalls)
	}
}

// --- dry run ---------------------------------------------------------------

// TestBulkDelete_DryRun: dry_run=true computes the exact plan (same outcomes,
// same reclaimed-bytes estimate) but performs NO mutation: every row is still
// present afterward, and the GC never runs.
func TestBulkDelete_DryRun(t *testing.T) {
	repo := newDeleteCancelFakeRepo()
	svc := buildDeleteCancelSvc(repo)
	tenantID, siteID := uuid.New(), uuid.New()
	a := Snapshot{ID: uuid.New(), TenantID: tenantID, SiteID: siteID, Status: StatusCompleted, TotalSize: 50}
	locked := Snapshot{ID: uuid.New(), TenantID: tenantID, SiteID: siteID, Status: StatusCompleted, Locked: true, TotalSize: 999}
	repo.setSnapshot(a)
	repo.setSnapshot(locked)

	out, err := svc.BulkDeleteSnapshots(context.Background(), tenantID, siteID, []uuid.UUID{a.ID, locked.ID}, true)
	if err != nil {
		t.Fatalf("BulkDeleteSnapshots(dry_run): unexpected error: %v", err)
	}
	if !out.DryRun {
		t.Fatalf("DryRun = false, want true")
	}
	if out.Deleted != 1 || out.Skipped != 1 {
		t.Fatalf("counts = deleted:%d skipped:%d, want 1/1", out.Deleted, out.Skipped)
	}
	if out.ReclaimedBytesEstimate != 50 {
		t.Fatalf("reclaimed_bytes_estimate = %d, want 50 (only the would-delete row)", out.ReclaimedBytesEstimate)
	}
	// Nothing was actually touched.
	if len(repo.deleted) != 0 {
		t.Fatalf("dry_run must not delete any row; deleted=%v", repo.deleted)
	}
	if _, ok := repo.snapshots[a.ID]; !ok {
		t.Fatalf("dry_run must not remove the snapshot row")
	}
	if _, ok := repo.snapshots[locked.ID]; !ok {
		t.Fatalf("dry_run must not remove the locked snapshot row")
	}
	if repo.gcCalls != 0 {
		t.Fatalf("gcCalls = %d, want 0 in dry_run mode", repo.gcCalls)
	}
}

// --- partial best-effort -----------------------------------------------------

// TestBulkDelete_PartialBestEffort: a mix of a locked snapshot, an in-flight
// ("failed" is used here as the not-yet-eligible id via lock; running covers
// in-flight) snapshot, and a normal completed snapshot all in one request
// succeeds as a whole (nil error) with the good one deleted and the bad ones
// skipped — never all-or-nothing.
func TestBulkDelete_PartialBestEffort(t *testing.T) {
	repo := newDeleteCancelFakeRepo()
	svc := buildDeleteCancelSvc(repo)
	tenantID, siteID := uuid.New(), uuid.New()
	locked := Snapshot{ID: uuid.New(), TenantID: tenantID, SiteID: siteID, Status: StatusCompleted, Locked: true}
	inFlight := Snapshot{ID: uuid.New(), TenantID: tenantID, SiteID: siteID, Status: StatusRunning}
	good := Snapshot{ID: uuid.New(), TenantID: tenantID, SiteID: siteID, Status: StatusCompleted}
	missing := uuid.New() // never persisted: exercises snapshot_not_found in the same batch
	repo.setSnapshot(locked)
	repo.setSnapshot(inFlight)
	repo.setSnapshot(good)

	out, err := svc.BulkDeleteSnapshots(context.Background(), tenantID, siteID,
		[]uuid.UUID{locked.ID, inFlight.ID, good.ID, missing}, false)
	if err != nil {
		t.Fatalf("BulkDeleteSnapshots: unexpected error (must be best-effort, never all-or-nothing): %v", err)
	}
	if out.Requested != 4 || out.Deleted != 1 || out.Skipped != 3 {
		t.Fatalf("counts = requested:%d deleted:%d skipped:%d, want 4/1/3", out.Requested, out.Deleted, out.Skipped)
	}
	if r := resultFor(t, out, locked.ID); r.Code != SkipSnapshotLocked {
		t.Fatalf("locked id code = %q, want snapshot_locked", r.Code)
	}
	if r := resultFor(t, out, inFlight.ID); r.Code != SkipSnapshotInProgress {
		t.Fatalf("in-flight id code = %q, want snapshot_in_progress", r.Code)
	}
	if r := resultFor(t, out, missing); r.Code != SkipSnapshotNotFound {
		t.Fatalf("missing id code = %q, want snapshot_not_found", r.Code)
	}
	if r := resultFor(t, out, good.ID); r.Outcome != BulkDeleteOutcomeDeleted {
		t.Fatalf("good id outcome = %q, want deleted", r.Outcome)
	}
	if !repo.deleted[good.ID] {
		t.Fatalf("the good snapshot should have been deleted")
	}
}

// --- validation --------------------------------------------------------------

// TestBulkDelete_CapExceeded: more than BulkDeleteMaxIDs unique ids is
// rejected with a Validation too_many_ids error and does no work at all.
func TestBulkDelete_CapExceeded(t *testing.T) {
	repo := newDeleteCancelFakeRepo()
	svc := buildDeleteCancelSvc(repo)
	tenantID, siteID := uuid.New(), uuid.New()
	ids := make([]uuid.UUID, 0, BulkDeleteMaxIDs+1)
	for i := 0; i < BulkDeleteMaxIDs+1; i++ {
		ids = append(ids, uuid.New())
	}

	_, err := svc.BulkDeleteSnapshots(context.Background(), tenantID, siteID, ids, false)
	de, ok := domain.AsDomain(err)
	if !ok || de.Kind != domain.KindValidation || de.Code != "too_many_ids" {
		t.Fatalf("err = %v, want Validation too_many_ids", err)
	}
	if len(repo.deleted) != 0 {
		t.Fatalf("no work should have been attempted: deleted=%v", repo.deleted)
	}
}

// TestBulkDelete_MissingIDs: an empty ids slice (after dedup) is rejected.
func TestBulkDelete_MissingIDs(t *testing.T) {
	repo := newDeleteCancelFakeRepo()
	svc := buildDeleteCancelSvc(repo)
	tenantID, siteID := uuid.New(), uuid.New()

	_, err := svc.BulkDeleteSnapshots(context.Background(), tenantID, siteID, nil, false)
	de, ok := domain.AsDomain(err)
	if !ok || de.Kind != domain.KindValidation || de.Code != "missing_ids" {
		t.Fatalf("err = %v, want Validation missing_ids", err)
	}
}

// TestBulkDelete_DuplicateIDsDeduped: the same id repeated in the request is
// deduplicated — it counts once toward Requested and produces exactly one
// result row, not two.
func TestBulkDelete_DuplicateIDsDeduped(t *testing.T) {
	repo := newDeleteCancelFakeRepo()
	svc := buildDeleteCancelSvc(repo)
	tenantID, siteID := uuid.New(), uuid.New()
	snap := Snapshot{ID: uuid.New(), TenantID: tenantID, SiteID: siteID, Status: StatusCompleted}
	repo.setSnapshot(snap)

	out, err := svc.BulkDeleteSnapshots(context.Background(), tenantID, siteID, []uuid.UUID{snap.ID, snap.ID, snap.ID}, false)
	if err != nil {
		t.Fatalf("BulkDeleteSnapshots: unexpected error: %v", err)
	}
	if out.Requested != 1 || len(out.Results) != 1 {
		t.Fatalf("requested=%d results=%d, want 1/1 after dedup", out.Requested, len(out.Results))
	}
}

// --- site mismatch -----------------------------------------------------------

// TestBulkDelete_SiteMismatch: a snapshot id that belongs to the tenant but a
// DIFFERENT site is treated exactly like "not found" — never leaking that the
// row exists under another site.
func TestBulkDelete_SiteMismatch(t *testing.T) {
	repo := newDeleteCancelFakeRepo()
	svc := buildDeleteCancelSvc(repo)
	tenantID, siteA, siteB := uuid.New(), uuid.New(), uuid.New()
	other := Snapshot{ID: uuid.New(), TenantID: tenantID, SiteID: siteB, Status: StatusCompleted}
	repo.setSnapshot(other)

	out, err := svc.BulkDeleteSnapshots(context.Background(), tenantID, siteA, []uuid.UUID{other.ID}, false)
	if err != nil {
		t.Fatalf("BulkDeleteSnapshots: unexpected error: %v", err)
	}
	r := resultFor(t, out, other.ID)
	if r.Outcome != BulkDeleteOutcomeSkipped || r.Code != SkipSnapshotNotFound {
		t.Fatalf("outcome=%q code=%q, want skipped/snapshot_not_found", r.Outcome, r.Code)
	}
	if repo.deleted[other.ID] {
		t.Fatalf("a different site's snapshot must never be deleted")
	}
}
