package backup

// schedule_incremental_wiring_test.go — ADR-048 P5 entry-point wiring tests.
//
// Verifies that the per-schedule incremental_enabled toggle gates whether the
// run-now (CreateBackup) and scheduled (EnqueueScheduledBackup) paths consult
// resolveChainForSite + EnqueueBackupWithChain, and that the toggle-OFF path is
// byte-identical to the historical full path (zero-value CreateSnapshotInput +
// EnqueueBackup, resolveChainForSite never consulted).
//
// White-box, in-memory fakes; no database.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// ---------------------------------------------------------------------------
// wiringRepo — a focused Repo fake that records CreateSnapshot inputs and
// answers GetSchedule / GetLatestCompletedSnapshot / CountFileIndex. Every
// other method panics (unused by these tests).
// ---------------------------------------------------------------------------

type wiringRepo struct {
	schedule          *Schedule // nil → GetSchedule returns NotFound
	latestCompleted   *Snapshot // nil → GetLatestCompletedSnapshot returns NotFound
	fileIndexCount    int64
	createInputs      []CreateSnapshotInput
	getLatestCalled   int
	countFileIdxCalls int
}

func (r *wiringRepo) GetSchedule(_ context.Context, _, _ uuid.UUID) (Schedule, error) {
	if r.schedule == nil {
		return Schedule{}, domain.NotFound("backup_schedule_not_found", "not found")
	}
	return *r.schedule, nil
}

func (r *wiringRepo) GetLatestCompletedSnapshot(_ context.Context, _, _ uuid.UUID) (Snapshot, error) {
	r.getLatestCalled++
	if r.latestCompleted == nil {
		return Snapshot{}, domain.NotFound("backup_snapshot_not_found", "not found")
	}
	return *r.latestCompleted, nil
}

func (r *wiringRepo) CountFileIndex(_ context.Context, _, _ uuid.UUID) (int64, error) {
	r.countFileIdxCalls++
	return r.fileIndexCount, nil
}

// HasFilesList: these wiring tests exercise the LEGACY file-index diffability
// path, so report false → the resolver falls through to the CountFileIndex gate
// (preserving the original fileIndexCount-driven assertions).
func (r *wiringRepo) HasFilesList(_ context.Context, _, _ uuid.UUID) (bool, error) {
	return false, nil
}

func (r *wiringRepo) CreateSnapshot(_ context.Context, in CreateSnapshotInput) (Snapshot, error) {
	r.createInputs = append(r.createInputs, in)
	// Mirror repo.CreateSnapshot's return shape: chain fields populated on the
	// returned snapshot, gen-0 anchors its own chain.
	snap := Snapshot{
		ID:               uuid.New(),
		TenantID:         in.TenantID,
		SiteID:           in.SiteID,
		Kind:             in.Kind,
		Status:           StatusPending,
		IsIncremental:    in.IsIncremental,
		ParentSnapshotID: in.ParentSnapshotID,
		BaseSnapshotID:   in.BaseSnapshotID,
		ChainID:          in.ChainID,
		Generation:       in.Generation,
	}
	if snap.ChainID == nil && in.Generation == 0 {
		id := snap.ID
		snap.ChainID = &id
	}
	return snap, nil
}

// --- unused Repo methods: panic if touched ---

func (r *wiringRepo) GetSnapshot(_ context.Context, _, _ uuid.UUID) (Snapshot, error) {
	panic("unused")
}
func (r *wiringRepo) GetSnapshotScoped(_ context.Context, _ db.ScopedPrincipal, _, _ uuid.UUID) (Snapshot, error) {
	panic("unused")
}
func (r *wiringRepo) ListSnapshotsForSite(_ context.Context, _, _ uuid.UUID, _, _ int32) ([]Snapshot, error) {
	panic("unused")
}
func (r *wiringRepo) MarkSnapshotRunning(_ context.Context, _, _ uuid.UUID) (Snapshot, bool, error) {
	panic("unused")
}
func (r *wiringRepo) CompleteSnapshot(_ context.Context, _, _ uuid.UUID, _, _ int64) (Snapshot, bool, error) {
	panic("unused")
}
func (r *wiringRepo) FailSnapshot(_ context.Context, _, _ uuid.UUID, _ string) (Snapshot, bool, error) {
	panic("unused")
}
func (r *wiringRepo) FailStalledSnapshot(_ context.Context, _, _ uuid.UUID, _ string) (int64, error) {
	panic("unused")
}
func (r *wiringRepo) UpdateSnapshotProgress(_ context.Context, _, _ uuid.UUID, _ []byte) (Snapshot, error) {
	panic("unused")
}
func (r *wiringRepo) ListStalledRunningSnapshots(_ context.Context, _, _ time.Duration) ([]StalledSnapshot, error) {
	panic("unused")
}
func (r *wiringRepo) MarkSnapshotStalled(_ context.Context, _, _ uuid.UUID) (bool, error) {
	panic("unused")
}
func (r *wiringRepo) ClearSnapshotStalled(_ context.Context, _, _ uuid.UUID) (bool, error) {
	panic("unused")
}
func (r *wiringRepo) RecordManifest(_ context.Context, _ RecordManifestInput) (int64, int64, error) {
	panic("unused")
}
func (r *wiringRepo) ListManifest(_ context.Context, _, _ uuid.UUID) ([]ManifestEntry, error) {
	panic("unused")
}
func (r *wiringRepo) ExistingChunkHashes(_ context.Context, _ uuid.UUID, _ []string) (map[string]Chunk, error) {
	panic("unused")
}
func (r *wiringRepo) UpsertSchedule(_ context.Context, _ UpsertScheduleInput) (Schedule, error) {
	panic("unused")
}
func (r *wiringRepo) ListDueSchedules(_ context.Context, _ time.Time, _ int32) ([]Schedule, error) {
	panic("unused")
}
func (r *wiringRepo) ListTenantsForGC(_ context.Context) ([]uuid.UUID, error) { panic("unused") }

// AdvanceScheduleRun is a no-op: the scheduled path always advances next_run_at,
// but its result is irrelevant to the toggle-gating these tests assert.
func (r *wiringRepo) AdvanceScheduleRun(_ context.Context, _, _ uuid.UUID, _ time.Time) error {
	return nil
}
func (r *wiringRepo) ListExpiredSnapshots(_ context.Context, _ uuid.UUID, _ time.Time) ([]Snapshot, error) {
	panic("unused")
}
func (r *wiringRepo) ListCompletedSnapshotsForSite(_ context.Context, _, _ uuid.UUID) ([]SnapshotMeta, error) {
	panic("unused")
}
func (r *wiringRepo) ListSiteIDsWithSnapshots(_ context.Context, _ uuid.UUID) ([]uuid.UUID, error) {
	panic("unused")
}
func (r *wiringRepo) SetSnapshotArchived(_ context.Context, _, _ uuid.UUID, _ bool) error {
	panic("unused")
}
func (r *wiringRepo) DeleteSnapshot(_ context.Context, _, _ uuid.UUID) error { panic("unused") }
func (r *wiringRepo) ListInFlightSnapshotFloor(_ context.Context, _ uuid.UUID) (time.Time, error) {
	panic("unused")
}
func (r *wiringRepo) DBNow(_ context.Context, _ uuid.UUID) (time.Time, error) { panic("unused") }
func (r *wiringRepo) SweepTenantChunks(_ context.Context, _ uuid.UUID, _ time.Time, _ *bool, _ func(SweepChunk, func() (bool, error)) (bool, error)) error {
	panic("unused")
}
func (r *wiringRepo) InsertFileIndexBatch(_ context.Context, _, _ uuid.UUID, _ []FileIndexEntry) error {
	panic("unused")
}
func (r *wiringRepo) StreamFileIndex(_ context.Context, _, _ uuid.UUID, _ func(FileIndexEntry) error) error {
	panic("unused")
}
func (r *wiringRepo) StreamChainEffectiveFileIndex(_ context.Context, _, _ uuid.UUID, _ int, _ func(FileIndexEntry) error) error {
	panic("unused")
}
func (r *wiringRepo) UpdateSnapshotCycleStats(_ context.Context, _, _ uuid.UUID, _ CycleStatsInput) error {
	panic("unused")
}
func (r *wiringRepo) CompleteIncrementalManifest(_ context.Context, _ CompleteIncrementalInput) (int64, int64, error) {
	panic("unused")
}
func (r *wiringRepo) ListChainSnapshots(_ context.Context, _ uuid.UUID, _ uuid.UUID, _ int) ([]Snapshot, error) {
	panic("unused")
}
func (r *wiringRepo) ListCompletedChainSnapshots(_ context.Context, _ uuid.UUID, _ uuid.UUID, _ int) ([]Snapshot, error) {
	panic("unused")
}
func (r *wiringRepo) SetSnapshotLocked(_ context.Context, _ uuid.UUID, _ uuid.UUID, _ bool) (Snapshot, error) {
	panic("unused")
}
func (r *wiringRepo) GetBackupSettings(_ context.Context, _, _ uuid.UUID) (SiteBackupSettings, error) {
	return SiteBackupSettings{}, domain.NotFound("backup_settings_not_found", "no settings")
}
func (r *wiringRepo) UpsertBackupSettings(_ context.Context, _ uuid.UUID, in SiteBackupSettings) (SiteBackupSettings, error) {
	panic("unused")
}
func (r *wiringRepo) FleetListSnapshots(_ context.Context, _ db.ScopedPrincipal, _ uuid.UUID, _ FleetListFilter) (FleetSnapshotPage, error) {
	panic("wiringRepo.FleetListSnapshots not implemented")
}
func (r *wiringRepo) FleetBackupHealth(_ context.Context, _ db.ScopedPrincipal, _ uuid.UUID, _ []uuid.UUID) ([]FleetBackupHealthItem, error) {
	panic("wiringRepo.FleetBackupHealth not implemented")
}
func (r *wiringRepo) ClaimAndAdvanceDueSchedules(_ context.Context, _ time.Time, _ map[uuid.UUID]time.Time) ([]Schedule, error) {
	panic("wiringRepo.ClaimAndAdvanceDueSchedules not implemented")
}
func (r *wiringRepo) CountInFlightSnapshots(_ context.Context, _, _ uuid.UUID) (int64, error) {
	return 0, nil // default: no in-flight snapshots
}
func (r *wiringRepo) HealOverdueSchedules(_ context.Context, _ time.Time, _ func(Schedule, time.Time) time.Time) (int, error) {
	return 0, nil
}
func (r *wiringRepo) GetSnapshotsByIDs(_ context.Context, _ uuid.UUID, _ []uuid.UUID) (map[uuid.UUID]Snapshot, error) {
	panic("wiringRepo.GetSnapshotsByIDs not implemented")
}
func (r *wiringRepo) HasActiveRestore(_ context.Context, _ uuid.UUID, _ []uuid.UUID) (map[uuid.UUID]bool, error) {
	panic("wiringRepo.HasActiveRestore not implemented")
}
func (r *wiringRepo) ReconcileDuplicateInflightSnapshots(_ context.Context) (int, error) {
	return 0, nil
}

// ---------------------------------------------------------------------------
// recordingEnqueuer — records which enqueue method was called.
// ---------------------------------------------------------------------------

type recordingEnqueuer struct {
	plainCalls []uuid.UUID // snapshot IDs passed to EnqueueBackup
	chainCalls []Snapshot  // snapshots passed to EnqueueBackupWithChain
}

func (e *recordingEnqueuer) EnqueueBackup(_ context.Context, _, snapshotID uuid.UUID) error {
	e.plainCalls = append(e.plainCalls, snapshotID)
	return nil
}
func (e *recordingEnqueuer) EnqueueBackupWithChain(_ context.Context, snap Snapshot) error {
	e.chainCalls = append(e.chainCalls, snap)
	return nil
}
func (e *recordingEnqueuer) EnqueueRestore(_ context.Context, _, _ uuid.UUID, _ RestoreSelection, _ uuid.UUID) error {
	panic("unused")
}

// ---------------------------------------------------------------------------
// fakeSites — minimal SiteLookup.
// ---------------------------------------------------------------------------

type fakeSites struct{ info SiteInfo }

func (s fakeSites) GetBackupSiteInfo(_ context.Context, _, _ uuid.UUID) (SiteInfo, error) {
	return s.info, nil
}
func (s fakeSites) ListSiteIDs(_ context.Context, _ uuid.UUID) ([]uuid.UUID, error) {
	return nil, nil
}

func buildWiringSvc(repo *wiringRepo, enq *recordingEnqueuer, now time.Time) *Service {
	return &Service{
		repo:     repo,
		enqueuer: enq,
		sites:    fakeSites{info: SiteInfo{Enrolled: true, AgeRecipient: "age1test"}},
		clock:    fakeClock{t: now},
	}
}

// ---------------------------------------------------------------------------
// CreateBackup (run-now)
// ---------------------------------------------------------------------------

func TestCreateBackup_ToggleOff_NoRegression(t *testing.T) {
	repo := &wiringRepo{schedule: &Schedule{IncrementalEnabled: false}}
	enq := &recordingEnqueuer{}
	svc := buildWiringSvc(repo, enq, time.Now())

	_, err := svc.CreateBackup(context.Background(), uuid.New(), uuid.New(), uuid.New(), KindFull)
	if err != nil {
		t.Fatalf("CreateBackup error: %v", err)
	}
	if len(enq.plainCalls) != 1 || len(enq.chainCalls) != 0 {
		t.Fatalf("expected EnqueueBackup (plain), got plain=%d chain=%d", len(enq.plainCalls), len(enq.chainCalls))
	}
	if repo.getLatestCalled != 0 {
		t.Errorf("resolveChainForSite must NOT be consulted when toggle off; GetLatestCompletedSnapshot called %d times", repo.getLatestCalled)
	}
	if len(repo.createInputs) != 1 {
		t.Fatalf("expected 1 CreateSnapshot, got %d", len(repo.createInputs))
	}
	in := repo.createInputs[0]
	if in.IsIncremental || in.Generation != 0 || in.ParentSnapshotID != nil || in.BaseSnapshotID != nil || in.ChainID != nil {
		t.Errorf("toggle-off CreateSnapshotInput must be zero-value full: %+v", in)
	}
}

func TestCreateBackup_NoSchedule_TreatedAsOff(t *testing.T) {
	repo := &wiringRepo{schedule: nil} // GetSchedule → NotFound
	enq := &recordingEnqueuer{}
	svc := buildWiringSvc(repo, enq, time.Now())

	_, err := svc.CreateBackup(context.Background(), uuid.New(), uuid.New(), uuid.New(), KindFull)
	if err != nil {
		t.Fatalf("CreateBackup error: %v", err)
	}
	if len(enq.plainCalls) != 1 || len(enq.chainCalls) != 0 {
		t.Fatalf("un-scheduled site must take the full path; got plain=%d chain=%d", len(enq.plainCalls), len(enq.chainCalls))
	}
}

func TestCreateBackup_ToggleOn_NoPrior_BaseIncrement(t *testing.T) {
	repo := &wiringRepo{schedule: &Schedule{IncrementalEnabled: true}} // no latestCompleted → NotFound
	enq := &recordingEnqueuer{}
	svc := buildWiringSvc(repo, enq, time.Now())

	_, err := svc.CreateBackup(context.Background(), uuid.New(), uuid.New(), uuid.New(), KindFull)
	if err != nil {
		t.Fatalf("CreateBackup error: %v", err)
	}
	if len(enq.chainCalls) != 1 || len(enq.plainCalls) != 0 {
		t.Fatalf("toggle-on must use EnqueueBackupWithChain; got plain=%d chain=%d", len(enq.plainCalls), len(enq.chainCalls))
	}
	if repo.getLatestCalled != 1 {
		t.Errorf("resolveChainForSite should be consulted once; got %d", repo.getLatestCalled)
	}
	// ADR-048 fix: the first run bootstraps a gen-0 base-INCREMENT (no parent)
	// so the agent writes a full file index instead of a plain full zip.
	if !enq.chainCalls[0].IsIncremental {
		t.Error("first run (no prior snapshot) must resolve to a gen-0 base-increment, not a plain full")
	}
	if enq.chainCalls[0].Generation != 0 {
		t.Errorf("first run must be generation=0, got %d", enq.chainCalls[0].Generation)
	}
	if enq.chainCalls[0].ParentSnapshotID != nil {
		t.Errorf("first run base-increment must have nil parent, got %v", enq.chainCalls[0].ParentSnapshotID)
	}
	if !repo.createInputs[0].IsIncremental || repo.createInputs[0].Generation != 0 {
		t.Errorf("CreateSnapshotInput must be a gen-0 base-increment on first run: %+v", repo.createInputs[0])
	}
	if repo.createInputs[0].ParentSnapshotID != nil {
		t.Errorf("CreateSnapshotInput must have nil parent on first run, got %v", repo.createInputs[0].ParentSnapshotID)
	}
}

func TestCreateBackup_ToggleOn_RecentBase_Increment(t *testing.T) {
	now := time.Now()
	tenantID, siteID := uuid.New(), uuid.New()
	baseID := uuid.New()
	finishedAt := now.Add(-1 * time.Hour)
	repo := &wiringRepo{
		schedule: &Schedule{IncrementalEnabled: true},
		latestCompleted: &Snapshot{
			ID:            baseID,
			TenantID:      tenantID,
			SiteID:        siteID,
			Status:        StatusCompleted,
			IsIncremental: false,
			Generation:    0,
			FinishedAt:    &finishedAt,
		},
		fileIndexCount: 10,
	}
	enq := &recordingEnqueuer{}
	svc := buildWiringSvc(repo, enq, now)

	_, err := svc.CreateBackup(context.Background(), tenantID, siteID, uuid.New(), KindFull)
	if err != nil {
		t.Fatalf("CreateBackup error: %v", err)
	}
	if len(enq.chainCalls) != 1 {
		t.Fatalf("expected one EnqueueBackupWithChain, got %d", len(enq.chainCalls))
	}
	snap := enq.chainCalls[0]
	if !snap.IsIncremental {
		t.Error("recent base present → must resolve to incremental")
	}
	if snap.Generation != 1 {
		t.Errorf("expected generation=1, got %d", snap.Generation)
	}
	if snap.ParentSnapshotID == nil || *snap.ParentSnapshotID != baseID {
		t.Errorf("expected parent=%v, got %v", baseID, snap.ParentSnapshotID)
	}
	if snap.BaseSnapshotID == nil || *snap.BaseSnapshotID != baseID {
		t.Errorf("expected base=%v, got %v", baseID, snap.BaseSnapshotID)
	}
}

// ---------------------------------------------------------------------------
// EnqueueScheduledBackup (scheduled)
// ---------------------------------------------------------------------------

func TestEnqueueScheduledBackup_ToggleOff_NoRegression(t *testing.T) {
	tenantID, siteID := uuid.New(), uuid.New()
	repo := &wiringRepo{}
	enq := &recordingEnqueuer{}
	svc := buildWiringSvc(repo, enq, time.Now())

	sched := Schedule{
		ID:                 uuid.New(),
		TenantID:           tenantID,
		SiteID:             siteID,
		Cadence:            CadenceDaily,
		Kind:               KindFull,
		IncrementalEnabled: false,
		NextRunAt:          time.Now(),
	}
	if err := svc.EnqueueScheduledBackup(context.Background(), sched); err != nil {
		t.Fatalf("EnqueueScheduledBackup error: %v", err)
	}
	if len(enq.plainCalls) != 1 || len(enq.chainCalls) != 0 {
		t.Fatalf("toggle-off scheduled run must use EnqueueBackup; got plain=%d chain=%d", len(enq.plainCalls), len(enq.chainCalls))
	}
	if repo.getLatestCalled != 0 {
		t.Errorf("resolveChainForSite must NOT be consulted when toggle off; got %d", repo.getLatestCalled)
	}
	if len(repo.createInputs) != 1 || repo.createInputs[0].IsIncremental || repo.createInputs[0].Generation != 0 {
		t.Errorf("toggle-off CreateSnapshotInput must be zero-value full: %+v", repo.createInputs)
	}
}

func TestEnqueueScheduledBackup_ToggleOn_RecentBase_Increment(t *testing.T) {
	now := time.Now()
	tenantID, siteID := uuid.New(), uuid.New()
	baseID := uuid.New()
	finishedAt := now.Add(-2 * time.Hour)
	repo := &wiringRepo{
		latestCompleted: &Snapshot{
			ID:            baseID,
			TenantID:      tenantID,
			SiteID:        siteID,
			Status:        StatusCompleted,
			IsIncremental: false,
			Generation:    0,
			FinishedAt:    &finishedAt,
		},
		fileIndexCount: 5,
	}
	enq := &recordingEnqueuer{}
	svc := buildWiringSvc(repo, enq, now)

	sched := Schedule{
		ID:                 uuid.New(),
		TenantID:           tenantID,
		SiteID:             siteID,
		Cadence:            CadenceDaily,
		Kind:               KindFull,
		IncrementalEnabled: true,
		NextRunAt:          now,
	}
	if err := svc.EnqueueScheduledBackup(context.Background(), sched); err != nil {
		t.Fatalf("EnqueueScheduledBackup error: %v", err)
	}
	if len(enq.chainCalls) != 1 || len(enq.plainCalls) != 0 {
		t.Fatalf("toggle-on scheduled run must use EnqueueBackupWithChain; got plain=%d chain=%d", len(enq.plainCalls), len(enq.chainCalls))
	}
	snap := enq.chainCalls[0]
	if !snap.IsIncremental || snap.Generation != 1 {
		t.Errorf("expected incremental gen=1; got is_incremental=%v gen=%d", snap.IsIncremental, snap.Generation)
	}
}
