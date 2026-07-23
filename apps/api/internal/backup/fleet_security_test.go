package backup

// fleet_security_test.go — unit tests for the fail-closed security invariants
// introduced to harden the fleet backup endpoints against empty site-ID slices.
//
// Tests run entirely in-process with in-memory stubs; no database required.
//
// Invariants verified:
//   1. FleetListSnapshots returns an empty page (no repo call) when the
//      principal is site-scoped and the resolved siteIDs slice is empty.
//   2. FleetBackupHealth returns an empty list (no repo call) when the
//      principal is site-scoped and the resolved siteIDs slice is empty.
//   3. intersectSiteIDs is fail-closed: empty allowed ⇒ empty result.
//   4. Org-scoped principals still reach the repo when siteIDs is non-empty.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// ---------------------------------------------------------------------------
// secFleetRepo — minimal Repo stub for fleet security tests.
// Only the two fleet methods have meaningful implementations; every other
// method panics to surface unexpected calls.
// ---------------------------------------------------------------------------

type secFleetRepo struct {
	// called is set to true when a fleet query method is invoked.
	called bool
	// returnPage / returnHealth control what the methods return.
	returnPage   FleetSnapshotPage
	returnHealth []FleetBackupHealthItem
}

func (r *secFleetRepo) FleetListSnapshots(_ context.Context, _ db.ScopedPrincipal, _ uuid.UUID, _ FleetListFilter) (FleetSnapshotPage, error) {
	r.called = true
	return r.returnPage, nil
}
func (r *secFleetRepo) FleetBackupHealth(_ context.Context, _ db.ScopedPrincipal, _ uuid.UUID, _ []uuid.UUID) ([]FleetBackupHealthItem, error) {
	r.called = true
	return r.returnHealth, nil
}

// --- All other Repo methods — panic to detect unexpected calls. ---

func (r *secFleetRepo) CreateSnapshot(_ context.Context, _ CreateSnapshotInput) (Snapshot, error) {
	panic("secFleetRepo.CreateSnapshot not implemented")
}
func (r *secFleetRepo) GetSnapshot(_ context.Context, _, _ uuid.UUID) (Snapshot, error) {
	panic("secFleetRepo.GetSnapshot not implemented")
}
func (r *secFleetRepo) GetSnapshotScoped(_ context.Context, _ db.ScopedPrincipal, _, _ uuid.UUID) (Snapshot, error) {
	panic("secFleetRepo.GetSnapshotScoped not implemented")
}
func (r *secFleetRepo) ListSnapshotsForSite(_ context.Context, _, _ uuid.UUID, _, _ int32) ([]Snapshot, error) {
	panic("secFleetRepo.ListSnapshotsForSite not implemented")
}
func (r *secFleetRepo) MarkSnapshotRunning(_ context.Context, _, _ uuid.UUID) (Snapshot, error) {
	panic("secFleetRepo.MarkSnapshotRunning not implemented")
}
func (r *secFleetRepo) CompleteSnapshot(_ context.Context, _, _ uuid.UUID, _, _ int64) (Snapshot, error) {
	panic("secFleetRepo.CompleteSnapshot not implemented")
}
func (r *secFleetRepo) FailSnapshot(_ context.Context, _, _ uuid.UUID, _ string) (Snapshot, error) {
	panic("secFleetRepo.FailSnapshot not implemented")
}
func (r *secFleetRepo) FailStalledSnapshot(_ context.Context, _, _ uuid.UUID, _ string) (int64, error) {
	panic("secFleetRepo.FailStalledSnapshot not implemented")
}
func (r *secFleetRepo) UpdateSnapshotProgress(_ context.Context, _, _ uuid.UUID, _ []byte) (Snapshot, error) {
	panic("secFleetRepo.UpdateSnapshotProgress not implemented")
}
func (r *secFleetRepo) ListStalledRunningSnapshots(_ context.Context, _, _ time.Duration) ([]StalledSnapshot, error) {
	panic("secFleetRepo.ListStalledRunningSnapshots not implemented")
}
func (r *secFleetRepo) MarkSnapshotStalled(_ context.Context, _, _ uuid.UUID) (bool, error) {
	panic("secFleetRepo.MarkSnapshotStalled not implemented")
}
func (r *secFleetRepo) ClearSnapshotStalled(_ context.Context, _, _ uuid.UUID) (bool, error) {
	panic("secFleetRepo.ClearSnapshotStalled not implemented")
}
func (r *secFleetRepo) GetLatestCompletedSnapshot(_ context.Context, _, _ uuid.UUID) (Snapshot, error) {
	panic("secFleetRepo.GetLatestCompletedSnapshot not implemented")
}
func (r *secFleetRepo) ListManifest(_ context.Context, _, _ uuid.UUID) ([]ManifestEntry, error) {
	panic("secFleetRepo.ListManifest not implemented")
}
func (r *secFleetRepo) HasFilesList(_ context.Context, _, _ uuid.UUID) (bool, error) {
	panic("secFleetRepo.HasFilesList not implemented")
}
func (r *secFleetRepo) RecordManifest(_ context.Context, _ RecordManifestInput) (int64, int64, error) {
	panic("secFleetRepo.RecordManifest not implemented")
}
func (r *secFleetRepo) ExistingChunkHashes(_ context.Context, _ uuid.UUID, _ []string) (map[string]Chunk, error) {
	panic("secFleetRepo.ExistingChunkHashes not implemented")
}
func (r *secFleetRepo) GetSchedule(_ context.Context, _, _ uuid.UUID) (Schedule, error) {
	panic("secFleetRepo.GetSchedule not implemented")
}
func (r *secFleetRepo) UpsertSchedule(_ context.Context, _ UpsertScheduleInput) (Schedule, error) {
	panic("secFleetRepo.UpsertSchedule not implemented")
}
func (r *secFleetRepo) ListDueSchedules(_ context.Context, _ time.Time, _ int32) ([]Schedule, error) {
	panic("secFleetRepo.ListDueSchedules not implemented")
}
func (r *secFleetRepo) ListTenantsForGC(_ context.Context) ([]uuid.UUID, error) {
	panic("secFleetRepo.ListTenantsForGC not implemented")
}
func (r *secFleetRepo) AdvanceScheduleRun(_ context.Context, _, _ uuid.UUID, _ time.Time) error {
	panic("secFleetRepo.AdvanceScheduleRun not implemented")
}
func (r *secFleetRepo) SetSnapshotLocked(_ context.Context, _, _ uuid.UUID, _ bool) (Snapshot, error) {
	panic("secFleetRepo.SetSnapshotLocked not implemented")
}
func (r *secFleetRepo) GetBackupSettings(_ context.Context, _, _ uuid.UUID) (SiteBackupSettings, error) {
	panic("secFleetRepo.GetBackupSettings not implemented")
}
func (r *secFleetRepo) UpsertBackupSettings(_ context.Context, _ uuid.UUID, _ SiteBackupSettings) (SiteBackupSettings, error) {
	panic("secFleetRepo.UpsertBackupSettings not implemented")
}
func (r *secFleetRepo) ListExpiredSnapshots(_ context.Context, _ uuid.UUID, _ time.Time) ([]Snapshot, error) {
	panic("secFleetRepo.ListExpiredSnapshots not implemented")
}
func (r *secFleetRepo) ListCompletedSnapshotsForSite(_ context.Context, _, _ uuid.UUID) ([]SnapshotMeta, error) {
	panic("secFleetRepo.ListCompletedSnapshotsForSite not implemented")
}
func (r *secFleetRepo) ListSiteIDsWithSnapshots(_ context.Context, _ uuid.UUID) ([]uuid.UUID, error) {
	panic("secFleetRepo.ListSiteIDsWithSnapshots not implemented")
}
func (r *secFleetRepo) SetSnapshotArchived(_ context.Context, _, _ uuid.UUID, _ bool) error {
	panic("secFleetRepo.SetSnapshotArchived not implemented")
}
func (r *secFleetRepo) DeleteSnapshot(_ context.Context, _, _ uuid.UUID) error {
	panic("secFleetRepo.DeleteSnapshot not implemented")
}
func (r *secFleetRepo) ListInFlightSnapshotFloor(_ context.Context, _ uuid.UUID) (time.Time, error) {
	panic("secFleetRepo.ListInFlightSnapshotFloor not implemented")
}
func (r *secFleetRepo) DBNow(_ context.Context, _ uuid.UUID) (time.Time, error) {
	panic("secFleetRepo.DBNow not implemented")
}
func (r *secFleetRepo) SweepTenantChunks(_ context.Context, _ uuid.UUID, _ time.Time, _ *bool, _ func(SweepChunk, func() (bool, error)) (bool, error)) error {
	panic("secFleetRepo.SweepTenantChunks not implemented")
}
func (r *secFleetRepo) CompleteIncrementalManifest(_ context.Context, _ CompleteIncrementalInput) (int64, int64, error) {
	panic("secFleetRepo.CompleteIncrementalManifest not implemented")
}
func (r *secFleetRepo) ListChainSnapshots(_ context.Context, _ uuid.UUID, _ uuid.UUID, _ int) ([]Snapshot, error) {
	panic("secFleetRepo.ListChainSnapshots not implemented")
}
func (r *secFleetRepo) ListCompletedChainSnapshots(_ context.Context, _ uuid.UUID, _ uuid.UUID, _ int) ([]Snapshot, error) {
	panic("secFleetRepo.ListCompletedChainSnapshots not implemented")
}
func (r *secFleetRepo) InsertFileIndexBatch(_ context.Context, _, _ uuid.UUID, _ []FileIndexEntry) error {
	panic("secFleetRepo.InsertFileIndexBatch not implemented")
}
func (r *secFleetRepo) CountFileIndex(_ context.Context, _, _ uuid.UUID) (int64, error) {
	panic("secFleetRepo.CountFileIndex not implemented")
}
func (r *secFleetRepo) StreamFileIndex(_ context.Context, _, _ uuid.UUID, _ func(FileIndexEntry) error) error {
	panic("secFleetRepo.StreamFileIndex not implemented")
}
func (r *secFleetRepo) StreamChainEffectiveFileIndex(_ context.Context, _, _ uuid.UUID, _ int, _ func(FileIndexEntry) error) error {
	panic("secFleetRepo.StreamChainEffectiveFileIndex not implemented")
}
func (r *secFleetRepo) UpdateSnapshotCycleStats(_ context.Context, _, _ uuid.UUID, _ CycleStatsInput) error {
	panic("secFleetRepo.UpdateSnapshotCycleStats not implemented")
}
func (r *secFleetRepo) ClaimAndAdvanceDueSchedules(_ context.Context, _ time.Time, _ map[uuid.UUID]time.Time) ([]Schedule, error) {
	panic("secFleetRepo.ClaimAndAdvanceDueSchedules not implemented")
}
func (r *secFleetRepo) CountInFlightSnapshots(_ context.Context, _, _ uuid.UUID) (int64, error) {
	return 0, nil
}
func (r *secFleetRepo) HealOverdueSchedules(_ context.Context, _ time.Time, _ func(Schedule, time.Time) time.Time) (int, error) {
	return 0, nil
}
func (r *secFleetRepo) ReconcileDuplicateInflightSnapshots(_ context.Context) (int, error) {
	return 0, nil
}
func (r *secFleetRepo) GetSnapshotsByIDs(_ context.Context, _ uuid.UUID, _ []uuid.UUID) (map[uuid.UUID]Snapshot, error) {
	panic("secFleetRepo.GetSnapshotsByIDs not implemented")
}
func (r *secFleetRepo) HasActiveRestore(_ context.Context, _ uuid.UUID, _ []uuid.UUID) (map[uuid.UUID]bool, error) {
	panic("secFleetRepo.HasActiveRestore not implemented")
}

// compile-time interface check.
var _ Repo = (*secFleetRepo)(nil)

// ---------------------------------------------------------------------------
// secSiteLookup — minimal SiteLookup stub for these tests.
// ---------------------------------------------------------------------------

type secSiteLookup struct {
	siteIDs []uuid.UUID
}

func (l *secSiteLookup) GetBackupSiteInfo(_ context.Context, _, _ uuid.UUID) (SiteInfo, error) {
	panic("secSiteLookup.GetBackupSiteInfo not implemented")
}
func (l *secSiteLookup) ListSiteIDs(_ context.Context, _ uuid.UUID) ([]uuid.UUID, error) {
	return l.siteIDs, nil
}

// newSecFleetService builds a Service with the given repo and site lookup and
// otherwise-zero Config/clock (fleet tests do not invoke time-dependent paths).
func newSecFleetService(repo Repo, sites SiteLookup) *Service {
	return NewService(repo, sites, nil, nil, fakeClock{}, Config{})
}

// ---------------------------------------------------------------------------
// Tests: FleetListSnapshots fail-closed for site-scoped principal
// ---------------------------------------------------------------------------

// TestFleetListSnapshots_SiteScoped_EmptySiteIDs_NoRepoCall verifies that when
// the principal is site-scoped and the resolved siteIDs slice is empty (the
// principal has zero granted sites), FleetListSnapshots returns an empty page
// WITHOUT issuing any DB query.
func TestFleetListSnapshots_SiteScoped_EmptySiteIDs_NoRepoCall(t *testing.T) {
	repo := &secFleetRepo{}
	svc := newSecFleetService(repo, &secSiteLookup{})

	tenantID := uuid.New()
	p := domain.Principal{
		TenantID:       tenantID,
		Scope:          domain.ScopeSite,
		AllowedSiteIDs: []uuid.UUID{}, // zero granted sites
	}

	page, err := svc.FleetListSnapshots(context.Background(), p, tenantID, FleetListFilter{})
	if err != nil {
		t.Fatalf("FleetListSnapshots: unexpected error: %v", err)
	}
	if repo.called {
		t.Fatal("FleetListSnapshots: repo was called for site-scoped principal with empty AllowedSiteIDs — fail-closed invariant violated")
	}
	if page.Items == nil {
		t.Fatal("FleetListSnapshots: Items must be non-nil empty slice, got nil")
	}
	if len(page.Items) != 0 {
		t.Fatalf("FleetListSnapshots: expected 0 items, got %d", len(page.Items))
	}
	if page.NextOffset != nil {
		t.Fatalf("FleetListSnapshots: expected nil NextOffset, got %v", page.NextOffset)
	}
}

// TestFleetListSnapshots_OrgScoped_NonEmpty_RepoCall verifies that org-scoped
// principals with a non-empty site list still reach the repo (behavior unchanged).
func TestFleetListSnapshots_OrgScoped_NonEmpty_RepoCall(t *testing.T) {
	siteID := uuid.New()
	repo := &secFleetRepo{
		returnPage: FleetSnapshotPage{Items: []Snapshot{{SiteID: siteID}}},
	}
	svc := newSecFleetService(repo, &secSiteLookup{siteIDs: []uuid.UUID{siteID}})

	tenantID := uuid.New()
	p := domain.Principal{
		TenantID: tenantID,
		Scope:    "", // org-scoped (zero value)
	}
	f := FleetListFilter{SiteIDs: []uuid.UUID{siteID}}

	page, err := svc.FleetListSnapshots(context.Background(), p, tenantID, f)
	if err != nil {
		t.Fatalf("FleetListSnapshots org-scoped: unexpected error: %v", err)
	}
	if !repo.called {
		t.Fatal("FleetListSnapshots org-scoped: repo was NOT called — org-scoped behavior broken")
	}
	if len(page.Items) != 1 {
		t.Fatalf("FleetListSnapshots org-scoped: expected 1 item, got %d", len(page.Items))
	}
}

// TestFleetListSnapshots_SiteScoped_WithSiteIDs_RepoCall verifies that a
// site-scoped principal with non-empty AllowedSiteIDs reaches the repo.
func TestFleetListSnapshots_SiteScoped_WithSiteIDs_RepoCall(t *testing.T) {
	siteID := uuid.New()
	repo := &secFleetRepo{
		returnPage: FleetSnapshotPage{Items: []Snapshot{{SiteID: siteID}}},
	}
	svc := newSecFleetService(repo, &secSiteLookup{siteIDs: []uuid.UUID{siteID}})

	tenantID := uuid.New()
	p := domain.Principal{
		TenantID:       tenantID,
		Scope:          domain.ScopeSite,
		AllowedSiteIDs: []uuid.UUID{siteID}, // one granted site
	}
	f := FleetListFilter{SiteIDs: []uuid.UUID{siteID}}

	page, err := svc.FleetListSnapshots(context.Background(), p, tenantID, f)
	if err != nil {
		t.Fatalf("FleetListSnapshots site-scoped non-empty: unexpected error: %v", err)
	}
	if !repo.called {
		t.Fatal("FleetListSnapshots site-scoped non-empty: repo was NOT called — site-scoped with granted sites should reach repo")
	}
	if len(page.Items) != 1 {
		t.Fatalf("FleetListSnapshots site-scoped non-empty: expected 1 item, got %d", len(page.Items))
	}
}

// ---------------------------------------------------------------------------
// Tests: FleetBackupHealth fail-closed for site-scoped principal
// ---------------------------------------------------------------------------

// TestFleetBackupHealth_SiteScoped_EmptySiteIDs_NoRepoCall verifies that when
// the principal is site-scoped and siteIDs is empty, FleetBackupHealth returns
// an empty list WITHOUT issuing any DB query.
func TestFleetBackupHealth_SiteScoped_EmptySiteIDs_NoRepoCall(t *testing.T) {
	repo := &secFleetRepo{}
	svc := newSecFleetService(repo, &secSiteLookup{})

	tenantID := uuid.New()
	p := domain.Principal{
		TenantID:       tenantID,
		Scope:          domain.ScopeSite,
		AllowedSiteIDs: []uuid.UUID{}, // zero granted sites
	}

	items, err := svc.FleetBackupHealth(context.Background(), p, tenantID, []uuid.UUID{})
	if err != nil {
		t.Fatalf("FleetBackupHealth: unexpected error: %v", err)
	}
	if repo.called {
		t.Fatal("FleetBackupHealth: repo was called for site-scoped principal with empty siteIDs — fail-closed invariant violated")
	}
	if items == nil {
		t.Fatal("FleetBackupHealth: items must be non-nil empty slice, got nil")
	}
	if len(items) != 0 {
		t.Fatalf("FleetBackupHealth: expected 0 items, got %d", len(items))
	}
}

// TestFleetBackupHealth_OrgScoped_NonEmpty_RepoCall verifies that org-scoped
// principals with a non-empty site list still reach the repo.
func TestFleetBackupHealth_OrgScoped_NonEmpty_RepoCall(t *testing.T) {
	siteID := uuid.New()
	repo := &secFleetRepo{
		returnHealth: []FleetBackupHealthItem{{SiteID: siteID, Status: HealthStatusProtected}},
	}
	svc := newSecFleetService(repo, &secSiteLookup{siteIDs: []uuid.UUID{siteID}})

	tenantID := uuid.New()
	p := domain.Principal{
		TenantID: tenantID,
		Scope:    "", // org-scoped
	}

	items, err := svc.FleetBackupHealth(context.Background(), p, tenantID, []uuid.UUID{siteID})
	if err != nil {
		t.Fatalf("FleetBackupHealth org-scoped: unexpected error: %v", err)
	}
	if !repo.called {
		t.Fatal("FleetBackupHealth org-scoped: repo was NOT called — org-scoped behavior broken")
	}
	if len(items) != 1 {
		t.Fatalf("FleetBackupHealth org-scoped: expected 1 item, got %d", len(items))
	}
}

// ---------------------------------------------------------------------------
// Tests: intersectSiteIDs fail-closed semantics
// ---------------------------------------------------------------------------

func TestIntersectSiteIDs_EmptyAllowedNil_ReturnsEmpty(t *testing.T) {
	candidate := []uuid.UUID{uuid.New(), uuid.New()}
	got := intersectSiteIDs(nil, candidate)
	if len(got) != 0 {
		t.Fatalf("intersectSiteIDs(nil, ...): expected empty, got %d items", len(got))
	}
}

func TestIntersectSiteIDs_EmptyAllowedSlice_ReturnsEmpty(t *testing.T) {
	candidate := []uuid.UUID{uuid.New()}
	got := intersectSiteIDs([]uuid.UUID{}, candidate)
	if len(got) != 0 {
		t.Fatalf("intersectSiteIDs(empty slice, ...): expected empty, got %d items", len(got))
	}
}

func TestIntersectSiteIDs_NonEmpty_FiltersCorrectly(t *testing.T) {
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	allowed := []uuid.UUID{a, b}
	candidate := []uuid.UUID{a, c}
	got := intersectSiteIDs(allowed, candidate)
	if len(got) != 1 || got[0] != a {
		t.Fatalf("intersectSiteIDs: expected [%s], got %v", a, got)
	}
}

func TestIntersectSiteIDs_BothEmpty_ReturnsEmpty(t *testing.T) {
	got := intersectSiteIDs([]uuid.UUID{}, []uuid.UUID{})
	if len(got) != 0 {
		t.Fatalf("intersectSiteIDs(both empty): expected empty, got %d items", len(got))
	}
}

func TestIntersectSiteIDs_AllMatch_ReturnsAll(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	allowed := []uuid.UUID{a, b}
	candidate := []uuid.UUID{a, b}
	got := intersectSiteIDs(allowed, candidate)
	if len(got) != 2 {
		t.Fatalf("intersectSiteIDs(all match): expected 2, got %d", len(got))
	}
}
