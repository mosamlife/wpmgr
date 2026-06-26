package backup

// delete_cancel_test.go — unit tests for the user-facing DELETE + CANCEL paths
// (DeleteSnapshotForUser chain-safety, CancelSnapshot status gating, and the
// post-cancel late-submit rejection in the Submit*Manifest guards). White-box,
// in-memory fakes only; no DB or network.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// deleteCancelFakeRepo extends fakeRepo with the chain + delete bookkeeping the
// delete/cancel paths touch. ListChainSnapshots/FailSnapshot/DeleteSnapshot are
// overridden so the base fake's panic-stubs don't fire.
type deleteCancelFakeRepo struct {
	*fakeRepo
	chains  map[uuid.UUID][]Snapshot // chainID -> snapshots
	failed  map[uuid.UUID]string     // snapshotID -> error msg passed to FailSnapshot
	deleted map[uuid.UUID]bool       // snapshotIDs removed
}

func newDeleteCancelFakeRepo() *deleteCancelFakeRepo {
	return &deleteCancelFakeRepo{
		fakeRepo: newFakeRepo(),
		chains:   map[uuid.UUID][]Snapshot{},
		failed:   map[uuid.UUID]string{},
		deleted:  map[uuid.UUID]bool{},
	}
}

func (r *deleteCancelFakeRepo) addChainSnap(chainID uuid.UUID, s Snapshot) {
	s.ChainID = &chainID
	r.fakeRepo.setSnapshot(s)
	r.chains[chainID] = append(r.chains[chainID], s)
}

func (r *deleteCancelFakeRepo) ListChainSnapshots(_ context.Context, _ uuid.UUID, chainID uuid.UUID, maxGen int) ([]Snapshot, error) {
	var out []Snapshot
	for _, s := range r.chains[chainID] {
		if s.Generation <= maxGen {
			out = append(out, s)
		}
	}
	return out, nil
}

func (r *deleteCancelFakeRepo) FailSnapshot(_ context.Context, _, snapshotID uuid.UUID, msg string) (Snapshot, error) {
	r.failed[snapshotID] = msg
	s := r.snapshots[snapshotID]
	s.Status = StatusFailed
	s.Error = msg
	r.snapshots[snapshotID] = s
	return s, nil
}

func (r *deleteCancelFakeRepo) DeleteSnapshot(_ context.Context, _, snapshotID uuid.UUID) error {
	r.deleted[snapshotID] = true
	delete(r.snapshots, snapshotID)
	return nil
}

// --- chunk reclamation stubs (the post-delete RunRetentionGC reuses these). The
// GC is best-effort in DeleteSnapshotForUser, so an empty/zero sweep is fine; we
// only need it to not panic. ---

func (r *deleteCancelFakeRepo) ListSiteIDsWithSnapshots(_ context.Context, _ uuid.UUID) ([]uuid.UUID, error) {
	return nil, nil
}
func (r *deleteCancelFakeRepo) DBNow(_ context.Context, _ uuid.UUID) (time.Time, error) {
	return time.Now(), nil
}
func (r *deleteCancelFakeRepo) ListInFlightSnapshotFloor(_ context.Context, _ uuid.UUID) (time.Time, error) {
	return time.Time{}, nil
}
func (r *deleteCancelFakeRepo) SweepTenantChunks(_ context.Context, _ uuid.UUID, _ time.Time, acquired *bool, _ func(SweepChunk) (bool, error)) error {
	*acquired = true
	return nil
}
func (r *deleteCancelFakeRepo) SetSnapshotLocked(_ context.Context, _, id uuid.UUID, locked bool) (Snapshot, error) {
	s := r.snapshots[id]
	s.Locked = locked
	r.snapshots[id] = s
	return s, nil
}

func buildDeleteCancelSvc(repo *deleteCancelFakeRepo) *Service {
	return &Service{repo: repo, clock: fakeClock{t: time.Now()}}
}

// --- CancelSnapshot ---------------------------------------------------------

func TestCancelSnapshot_RunningTransitionsToFailed(t *testing.T) {
	repo := newDeleteCancelFakeRepo()
	svc := buildDeleteCancelSvc(repo)
	tenantID, siteID := uuid.New(), uuid.New()
	snap := Snapshot{ID: uuid.New(), TenantID: tenantID, SiteID: siteID, Status: StatusRunning}
	repo.setSnapshot(snap)

	out, err := svc.CancelSnapshot(context.Background(), tenantID, snap.ID)
	if err != nil {
		t.Fatalf("CancelSnapshot: unexpected error: %v", err)
	}
	if out.Status != StatusFailed {
		t.Fatalf("status = %q, want %q", out.Status, StatusFailed)
	}
	if repo.failed[snap.ID] != cancelByOperatorMsg {
		t.Fatalf("FailSnapshot msg = %q, want %q", repo.failed[snap.ID], cancelByOperatorMsg)
	}
}

func TestCancelSnapshot_PendingIsCancelable(t *testing.T) {
	repo := newDeleteCancelFakeRepo()
	svc := buildDeleteCancelSvc(repo)
	tenantID := uuid.New()
	snap := Snapshot{ID: uuid.New(), TenantID: tenantID, SiteID: uuid.New(), Status: StatusPending}
	repo.setSnapshot(snap)

	if _, err := svc.CancelSnapshot(context.Background(), tenantID, snap.ID); err != nil {
		t.Fatalf("CancelSnapshot(pending): unexpected error: %v", err)
	}
}

func TestCancelSnapshot_TerminalRejected(t *testing.T) {
	for _, status := range []string{StatusCompleted, StatusFailed} {
		repo := newDeleteCancelFakeRepo()
		svc := buildDeleteCancelSvc(repo)
		tenantID := uuid.New()
		snap := Snapshot{ID: uuid.New(), TenantID: tenantID, SiteID: uuid.New(), Status: status}
		repo.setSnapshot(snap)

		_, err := svc.CancelSnapshot(context.Background(), tenantID, snap.ID)
		de, ok := domain.AsDomain(err)
		if !ok || de.Kind != domain.KindConflict || de.Code != "snapshot_not_cancelable" {
			t.Fatalf("status %q: err = %v, want Conflict snapshot_not_cancelable", status, err)
		}
	}
}

// --- DeleteSnapshotForUser --------------------------------------------------

func TestDeleteSnapshotForUser_StandaloneDeletes(t *testing.T) {
	repo := newDeleteCancelFakeRepo()
	svc := buildDeleteCancelSvc(repo)
	tenantID := uuid.New()
	// Non-chained completed full backup (ChainID == nil).
	snap := Snapshot{ID: uuid.New(), TenantID: tenantID, SiteID: uuid.New(), Status: StatusCompleted}
	repo.setSnapshot(snap)

	if err := svc.DeleteSnapshotForUser(context.Background(), tenantID, snap.ID); err != nil {
		t.Fatalf("DeleteSnapshotForUser: unexpected error: %v", err)
	}
	if !repo.deleted[snap.ID] {
		t.Fatalf("snapshot row was not deleted")
	}
}

func TestDeleteSnapshotForUser_BaseWithDependentsRefused(t *testing.T) {
	repo := newDeleteCancelFakeRepo()
	svc := buildDeleteCancelSvc(repo)
	tenantID, siteID := uuid.New(), uuid.New()
	chainID := uuid.New()
	base := Snapshot{ID: chainID, TenantID: tenantID, SiteID: siteID, Status: StatusCompleted, Generation: 0}
	inc := Snapshot{ID: uuid.New(), TenantID: tenantID, SiteID: siteID, Status: StatusCompleted, Generation: 1, IsIncremental: true}
	repo.addChainSnap(chainID, base)
	repo.addChainSnap(chainID, inc)

	err := svc.DeleteSnapshotForUser(context.Background(), tenantID, base.ID)
	de, ok := domain.AsDomain(err)
	if !ok || de.Kind != domain.KindValidation || de.Code != "chain_has_dependents" {
		t.Fatalf("err = %v, want Validation chain_has_dependents", err)
	}
	if repo.deleted[base.ID] {
		t.Fatalf("base must NOT be deleted while a dependent increment exists")
	}
}

func TestDeleteSnapshotForUser_LeafIncrementDeletes(t *testing.T) {
	repo := newDeleteCancelFakeRepo()
	svc := buildDeleteCancelSvc(repo)
	tenantID, siteID := uuid.New(), uuid.New()
	chainID := uuid.New()
	base := Snapshot{ID: chainID, TenantID: tenantID, SiteID: siteID, Status: StatusCompleted, Generation: 0}
	leaf := Snapshot{ID: uuid.New(), TenantID: tenantID, SiteID: siteID, Status: StatusCompleted, Generation: 1, IsIncremental: true}
	repo.addChainSnap(chainID, base)
	repo.addChainSnap(chainID, leaf)

	// Deleting the highest-generation increment (the leaf/tip) is safe.
	if err := svc.DeleteSnapshotForUser(context.Background(), tenantID, leaf.ID); err != nil {
		t.Fatalf("DeleteSnapshotForUser(leaf): unexpected error: %v", err)
	}
	if !repo.deleted[leaf.ID] {
		t.Fatalf("leaf increment should have been deleted")
	}
}

func TestDeleteSnapshotForUser_RunningRefused(t *testing.T) {
	repo := newDeleteCancelFakeRepo()
	svc := buildDeleteCancelSvc(repo)
	tenantID := uuid.New()
	snap := Snapshot{ID: uuid.New(), TenantID: tenantID, SiteID: uuid.New(), Status: StatusRunning}
	repo.setSnapshot(snap)

	err := svc.DeleteSnapshotForUser(context.Background(), tenantID, snap.ID)
	de, ok := domain.AsDomain(err)
	if !ok || de.Kind != domain.KindValidation || de.Code != "snapshot_in_progress" {
		t.Fatalf("err = %v, want Validation snapshot_in_progress", err)
	}
}

// A locked snapshot is exempt from manual delete just as it is from the auto-GC
// (see gc.go: locked metas are pulled out of the deleteSet). The operator must
// unlock first — that is what makes a lock a real protection, not just a hint.
func TestDeleteSnapshotForUser_LockedRefused(t *testing.T) {
	repo := newDeleteCancelFakeRepo()
	svc := buildDeleteCancelSvc(repo)
	tenantID := uuid.New()
	snap := Snapshot{ID: uuid.New(), TenantID: tenantID, SiteID: uuid.New(), Status: StatusCompleted, Locked: true}
	repo.setSnapshot(snap)

	err := svc.DeleteSnapshotForUser(context.Background(), tenantID, snap.ID)
	de, ok := domain.AsDomain(err)
	if !ok || de.Kind != domain.KindValidation || de.Code != "snapshot_locked" {
		t.Fatalf("err = %v, want Validation snapshot_locked", err)
	}
	if repo.deleted[snap.ID] {
		t.Fatalf("locked snapshot must NOT be deleted")
	}
}

// --- post-cancel late-submit rejection --------------------------------------

func TestSubmitManifest_RejectsCanceledSnapshot(t *testing.T) {
	repo := newDeleteCancelFakeRepo()
	svc := buildDeleteCancelSvc(repo)
	tenantID := uuid.New()
	snap := Snapshot{ID: uuid.New(), TenantID: tenantID, SiteID: uuid.New(), Status: StatusFailed, Error: cancelByOperatorMsg}
	repo.setSnapshot(snap)

	_, _, err := svc.SubmitManifest(context.Background(), tenantID, snap.ID, agentcmd.SubmitManifestRequest{})
	de, ok := domain.AsDomain(err)
	if !ok || de.Kind != domain.KindConflict || de.Code != "snapshot_canceled" {
		t.Fatalf("SubmitManifest err = %v, want Conflict snapshot_canceled", err)
	}
}

// ADR-051: an archive-delta increment submits through SubmitManifest, so the
// post-cancel late-submit rejection is covered by TestSubmitManifest_RejectsCanceledSnapshot
// above — there is no separate SubmitIncrementalManifest path to test.

// --- manifest index deletion on snapshot delete ----------------------------

// fakeIndexDeleter records Delete calls and optionally returns an error.
type fakeIndexDeleter struct {
	deleted []string
	errOn   string // if non-empty, return an error when this key is deleted
}

func (f *fakeIndexDeleter) Delete(_ context.Context, key string) error {
	f.deleted = append(f.deleted, key)
	if f.errOn != "" && f.errOn == key {
		return errors.New("simulated storage error")
	}
	return nil
}

// TestDeleteSnapshotForUser_DeletesManifestObject asserts that deleting a
// snapshot also deletes its manifest.json index object from object storage, and
// that the expected key matches the manifestIndexKey construction.
func TestDeleteSnapshotForUser_DeletesManifestObject(t *testing.T) {
	repo := newDeleteCancelFakeRepo()
	deleter := &fakeIndexDeleter{}
	svc := buildDeleteCancelSvc(repo)
	svc.SetIndexDeleter(deleter)

	tenantID, siteID := uuid.New(), uuid.New()
	snap := Snapshot{ID: uuid.New(), TenantID: tenantID, SiteID: siteID, Status: StatusCompleted}
	repo.setSnapshot(snap)

	if err := svc.DeleteSnapshotForUser(context.Background(), tenantID, snap.ID); err != nil {
		t.Fatalf("DeleteSnapshotForUser: unexpected error: %v", err)
	}
	if !repo.deleted[snap.ID] {
		t.Fatalf("snapshot DB row was not deleted")
	}

	want := manifestIndexKey(tenantID, siteID, snap.ID)
	if len(deleter.deleted) != 1 || deleter.deleted[0] != want {
		t.Fatalf("manifest delete calls = %v, want [%q]", deleter.deleted, want)
	}
}

// TestDeleteSnapshotForUser_MissingManifestDoesNotFail asserts that a storage
// error on manifest delete (e.g. already-absent object returning an unexpected
// status) does NOT fail the snapshot delete — best-effort contract.
func TestDeleteSnapshotForUser_MissingManifestDoesNotFail(t *testing.T) {
	repo := newDeleteCancelFakeRepo()
	tenantID, siteID := uuid.New(), uuid.New()
	snap := Snapshot{ID: uuid.New(), TenantID: tenantID, SiteID: siteID, Status: StatusCompleted}
	repo.setSnapshot(snap)

	wantKey := manifestIndexKey(tenantID, siteID, snap.ID)
	deleter := &fakeIndexDeleter{errOn: wantKey}
	svc := buildDeleteCancelSvc(repo)
	svc.SetIndexDeleter(deleter)

	// Even though the deleter returns an error, DeleteSnapshotForUser must succeed.
	if err := svc.DeleteSnapshotForUser(context.Background(), tenantID, snap.ID); err != nil {
		t.Fatalf("DeleteSnapshotForUser: manifest delete error must not propagate, got: %v", err)
	}
	if !repo.deleted[snap.ID] {
		t.Fatalf("snapshot DB row was not deleted")
	}
	// The delete was still attempted.
	if len(deleter.deleted) == 0 {
		t.Fatalf("manifest delete was not attempted")
	}
}

// TestDeleteSnapshotForUser_NoDeleterIsNoop asserts that when no IndexDeleter
// is wired (nil), DeleteSnapshotForUser still succeeds without panicking.
func TestDeleteSnapshotForUser_NoDeleterIsNoop(t *testing.T) {
	repo := newDeleteCancelFakeRepo()
	svc := buildDeleteCancelSvc(repo) // no SetIndexDeleter
	tenantID := uuid.New()
	snap := Snapshot{ID: uuid.New(), TenantID: tenantID, SiteID: uuid.New(), Status: StatusCompleted}
	repo.setSnapshot(snap)

	if err := svc.DeleteSnapshotForUser(context.Background(), tenantID, snap.ID); err != nil {
		t.Fatalf("DeleteSnapshotForUser (no deleter): unexpected error: %v", err)
	}
	if !repo.deleted[snap.ID] {
		t.Fatalf("snapshot DB row was not deleted")
	}
}
