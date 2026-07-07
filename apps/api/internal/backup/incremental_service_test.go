package backup

// incremental_service_test.go — table-driven unit tests for ADR-048
// resolveChainForSite and SubmitIncrementalManifest.
// All tests run inside the package (white-box) and use in-memory fakes;
// no database is required.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// ---------------------------------------------------------------------------
// fakeRepo — minimal in-memory Repo stub for incremental tests.
// Only the methods exercised by resolveChainForSite and
// SubmitIncrementalManifest are implemented; everything else panics.
// ---------------------------------------------------------------------------

type fakeRepo struct {
	mu              sync.Mutex
	snapshots       map[uuid.UUID]Snapshot // snapshotID → row
	latestCompleted map[string]Snapshot    // "tenantID/siteID" → row
	fileIndexRows   map[uuid.UUID][]FileIndexEntry
	fileIndexCounts map[uuid.UUID]int64
	cycleStats      map[uuid.UUID]CycleStatsInput
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		snapshots:       make(map[uuid.UUID]Snapshot),
		latestCompleted: make(map[string]Snapshot),
		fileIndexRows:   make(map[uuid.UUID][]FileIndexEntry),
		fileIndexCounts: make(map[uuid.UUID]int64),
		cycleStats:      make(map[uuid.UUID]CycleStatsInput),
	}
}

func (r *fakeRepo) setSnapshot(s Snapshot) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.snapshots[s.ID] = s
	if s.Status == StatusCompleted {
		key := s.TenantID.String() + "/" + s.SiteID.String()
		// Keep the newest completed snapshot (naive: last set wins).
		r.latestCompleted[key] = s
	}
}

func siteKey(tenantID, siteID uuid.UUID) string {
	return tenantID.String() + "/" + siteID.String()
}

func (r *fakeRepo) GetLatestCompletedSnapshot(_ context.Context, tenantID, siteID uuid.UUID) (Snapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.latestCompleted[siteKey(tenantID, siteID)]
	if !ok {
		return Snapshot{}, domain.NotFound("backup_snapshot_not_found", "no completed snapshot found for site")
	}
	return s, nil
}

func (r *fakeRepo) CountFileIndex(_ context.Context, _, snapshotID uuid.UUID) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.fileIndexCounts[snapshotID]; ok {
		return c, nil
	}
	return int64(len(r.fileIndexRows[snapshotID])), nil
}

func (r *fakeRepo) InsertFileIndexBatch(_ context.Context, _, snapshotID uuid.UUID, entries []FileIndexEntry) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fileIndexRows[snapshotID] = append(r.fileIndexRows[snapshotID], entries...)
	return nil
}

func (r *fakeRepo) StreamFileIndex(_ context.Context, _, snapshotID uuid.UUID, fn func(FileIndexEntry) error) error {
	r.mu.Lock()
	rows := append([]FileIndexEntry(nil), r.fileIndexRows[snapshotID]...)
	r.mu.Unlock()
	for _, e := range rows {
		if err := fn(e); err != nil {
			return err
		}
	}
	return nil
}

// StreamChainEffectiveFileIndex is not exercised by the bare fakeRepo (which has
// no chain bookkeeping); chainFakeRepo provides the real merge. It panics here so
// any accidental chain use through the base fake is caught loudly.
func (r *fakeRepo) StreamChainEffectiveFileIndex(_ context.Context, _, _ uuid.UUID, _ int, _ func(FileIndexEntry) error) error {
	panic("fakeRepo.StreamChainEffectiveFileIndex not implemented")
}

func (r *fakeRepo) UpdateSnapshotCycleStats(_ context.Context, _, snapshotID uuid.UUID, in CycleStatsInput) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cycleStats[snapshotID] = in
	return nil
}

func (r *fakeRepo) GetSnapshot(_ context.Context, _, snapshotID uuid.UUID) (Snapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.snapshots[snapshotID]
	if !ok {
		return Snapshot{}, domain.NotFound("backup_snapshot_not_found", "not found")
	}
	return s, nil
}

func (r *fakeRepo) CompleteSnapshot(_ context.Context, tenantID, snapshotID uuid.UUID, totalSize, chunkCount int64) (Snapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.snapshots[snapshotID]
	if !ok {
		return Snapshot{}, domain.NotFound("backup_snapshot_not_found", "not found")
	}
	s.Status = StatusCompleted
	s.TotalSize = totalSize
	s.ChunkCount = chunkCount
	r.snapshots[snapshotID] = s
	return s, nil
}

func (r *fakeRepo) RecordManifest(_ context.Context, in RecordManifestInput) (int64, int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.snapshots[in.SnapshotID]
	if !ok {
		return 0, 0, domain.NotFound("backup_snapshot_not_found", "not found")
	}
	s.Status = StatusCompleted
	r.snapshots[in.SnapshotID] = s
	var refs int64
	for _, e := range in.Entries {
		refs += int64(len(e.ChunkHashes))
	}
	return refs, int64(len(in.Chunks)), nil
}

// Unimplemented stubs that panic if accidentally called.
func (r *fakeRepo) CreateSnapshot(_ context.Context, _ CreateSnapshotInput) (Snapshot, error) {
	panic("fakeRepo.CreateSnapshot not implemented in this test")
}
func (r *fakeRepo) GetSnapshotScoped(_ context.Context, _ db.ScopedPrincipal, _, _ uuid.UUID) (Snapshot, error) {
	panic("fakeRepo.GetSnapshotScoped not implemented")
}
func (r *fakeRepo) ListSnapshotsForSite(_ context.Context, _, _ uuid.UUID, _, _ int32) ([]Snapshot, error) {
	panic("fakeRepo.ListSnapshotsForSite not implemented")
}
func (r *fakeRepo) MarkSnapshotRunning(_ context.Context, _, _ uuid.UUID) (Snapshot, error) {
	panic("fakeRepo.MarkSnapshotRunning not implemented")
}
func (r *fakeRepo) FailSnapshot(_ context.Context, _, _ uuid.UUID, _ string) (Snapshot, error) {
	panic("fakeRepo.FailSnapshot not implemented")
}
func (r *fakeRepo) UpdateSnapshotProgress(_ context.Context, _, _ uuid.UUID, _ []byte) (Snapshot, error) {
	panic("fakeRepo.UpdateSnapshotProgress not implemented")
}
func (r *fakeRepo) ListStalledRunningSnapshots(_ context.Context, _ time.Duration) ([]StalledSnapshot, error) {
	panic("fakeRepo.ListStalledRunningSnapshots not implemented")
}
func (r *fakeRepo) ListManifest(_ context.Context, _, _ uuid.UUID) ([]ManifestEntry, error) {
	panic("fakeRepo.ListManifest not implemented")
}

// HasFilesList: the base fake tracks no manifest entries, so it reports false
// (legacy / file-index model). chainFakeRepo overrides it to scan its manifests.
func (r *fakeRepo) HasFilesList(_ context.Context, _, _ uuid.UUID) (bool, error) {
	return false, nil
}
func (r *fakeRepo) ExistingChunkHashes(_ context.Context, _ uuid.UUID, _ []string) (map[string]Chunk, error) {
	panic("fakeRepo.ExistingChunkHashes not implemented")
}
func (r *fakeRepo) GetSchedule(_ context.Context, _, _ uuid.UUID) (Schedule, error) {
	return Schedule{}, domain.NotFound("backup_schedule_not_found", "no schedule")
}
func (r *fakeRepo) UpsertSchedule(_ context.Context, _ UpsertScheduleInput) (Schedule, error) {
	panic("fakeRepo.UpsertSchedule not implemented")
}
func (r *fakeRepo) GetBackupSettings(_ context.Context, _, _ uuid.UUID) (SiteBackupSettings, error) {
	// Return NotFound so scheduleBackupScope degrades gracefully in tests that
	// do not exercise backup-settings scope.
	return SiteBackupSettings{}, domain.NotFound("backup_settings_not_found", "no settings")
}
func (r *fakeRepo) UpsertBackupSettings(_ context.Context, _ uuid.UUID, in SiteBackupSettings) (SiteBackupSettings, error) {
	return in, nil
}
func (r *fakeRepo) ListDueSchedules(_ context.Context, _ time.Time, _ int32) ([]Schedule, error) {
	panic("fakeRepo.ListDueSchedules not implemented")
}
func (r *fakeRepo) ListTenantsForGC(_ context.Context) ([]uuid.UUID, error) {
	panic("fakeRepo.ListTenantsForGC not implemented")
}
func (r *fakeRepo) AdvanceScheduleRun(_ context.Context, _, _ uuid.UUID, _ time.Time) error {
	panic("fakeRepo.AdvanceScheduleRun not implemented")
}
func (r *fakeRepo) ListExpiredSnapshots(_ context.Context, _ uuid.UUID, _ time.Time) ([]Snapshot, error) {
	panic("fakeRepo.ListExpiredSnapshots not implemented")
}
func (r *fakeRepo) ListCompletedSnapshotsForSite(_ context.Context, _, _ uuid.UUID) ([]SnapshotMeta, error) {
	panic("fakeRepo.ListCompletedSnapshotsForSite not implemented")
}
func (r *fakeRepo) ListSiteIDsWithSnapshots(_ context.Context, _ uuid.UUID) ([]uuid.UUID, error) {
	panic("fakeRepo.ListSiteIDsWithSnapshots not implemented")
}
func (r *fakeRepo) SetSnapshotArchived(_ context.Context, _, _ uuid.UUID, _ bool) error {
	panic("fakeRepo.SetSnapshotArchived not implemented")
}
func (r *fakeRepo) DeleteSnapshot(_ context.Context, _, _ uuid.UUID) error {
	panic("fakeRepo.DeleteSnapshot not implemented")
}
func (r *fakeRepo) ListInFlightSnapshotFloor(_ context.Context, _ uuid.UUID) (time.Time, error) {
	panic("fakeRepo.ListInFlightSnapshotFloor not implemented")
}
func (r *fakeRepo) DBNow(_ context.Context, _ uuid.UUID) (time.Time, error) {
	panic("fakeRepo.DBNow not implemented")
}
func (r *fakeRepo) SweepTenantChunks(_ context.Context, _ uuid.UUID, _ time.Time, _ *bool, _ func(SweepChunk, func() (bool, error)) (bool, error)) error {
	panic("fakeRepo.SweepTenantChunks not implemented")
}
func (r *fakeRepo) CompleteIncrementalManifest(_ context.Context, in CompleteIncrementalInput) (int64, int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Mirror the real atomic method: insert file-index rows, optionally record the
	// DB manifest, then complete the snapshot — all observable together.
	r.fileIndexRows[in.SnapshotID] = append(r.fileIndexRows[in.SnapshotID], in.FileEntries...)
	var refs, stored int64
	if in.DBManifest != nil {
		for _, e := range in.DBManifest.Entries {
			refs += int64(len(e.ChunkHashes))
		}
		stored = int64(len(in.DBManifest.Chunks))
	} else {
		refs = in.ChunkRefs
	}
	if s, ok := r.snapshots[in.SnapshotID]; ok {
		s.Status = StatusCompleted
		r.snapshots[in.SnapshotID] = s
	}
	return refs, stored, nil
}
func (r *fakeRepo) ListChainSnapshots(_ context.Context, _ uuid.UUID, _ uuid.UUID, _ int) ([]Snapshot, error) {
	panic("fakeRepo.ListChainSnapshots not implemented")
}
func (r *fakeRepo) ListCompletedChainSnapshots(_ context.Context, _ uuid.UUID, _ uuid.UUID, _ int) ([]Snapshot, error) {
	panic("fakeRepo.ListCompletedChainSnapshots not implemented")
}
func (r *fakeRepo) SetSnapshotLocked(_ context.Context, _, id uuid.UUID, locked bool) (Snapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.snapshots[id]
	s.Locked = locked
	r.snapshots[id] = s
	return s, nil
}
func (r *fakeRepo) FleetListSnapshots(_ context.Context, _ db.ScopedPrincipal, _ uuid.UUID, _ FleetListFilter) (FleetSnapshotPage, error) {
	panic("fakeRepo.FleetListSnapshots not implemented")
}
func (r *fakeRepo) FleetBackupHealth(_ context.Context, _ db.ScopedPrincipal, _ uuid.UUID, _ []uuid.UUID) ([]FleetBackupHealthItem, error) {
	panic("fakeRepo.FleetBackupHealth not implemented")
}
func (r *fakeRepo) ClaimAndAdvanceDueSchedules(_ context.Context, _ time.Time, _ map[uuid.UUID]time.Time) ([]Schedule, error) {
	panic("fakeRepo.ClaimAndAdvanceDueSchedules not implemented")
}
func (r *fakeRepo) CountInFlightSnapshots(_ context.Context, _, _ uuid.UUID) (int64, error) {
	return 0, nil // default: no in-flight snapshots
}
func (r *fakeRepo) HealOverdueSchedules(_ context.Context, _ time.Time, _ func(Schedule, time.Time) time.Time) (int, error) {
	return 0, nil
}
func (r *fakeRepo) ReconcileDuplicateInflightSnapshots(_ context.Context) (int, error) {
	return 0, nil
}
func (r *fakeRepo) GetSnapshotsByIDs(_ context.Context, _ uuid.UUID, _ []uuid.UUID) (map[uuid.UUID]Snapshot, error) {
	panic("fakeRepo.GetSnapshotsByIDs not implemented")
}
func (r *fakeRepo) HasActiveRestore(_ context.Context, _ uuid.UUID, _ []uuid.UUID) (map[uuid.UUID]bool, error) {
	panic("fakeRepo.HasActiveRestore not implemented")
}

// ---------------------------------------------------------------------------
// fakeClock — deterministic clock for tests.
// ---------------------------------------------------------------------------

type fakeClock struct{ t time.Time }

func (c fakeClock) Now() time.Time { return c.t }

// ---------------------------------------------------------------------------
// buildSvc — convenience builder for the test service.
// ---------------------------------------------------------------------------

func buildIncrementalSvc(repo *fakeRepo, now time.Time) *Service {
	return &Service{
		repo:  repo,
		clock: fakeClock{t: now},
	}
}

// ---------------------------------------------------------------------------
// TestResolveChainForSite — AUTO-BASE rule table tests.
// ---------------------------------------------------------------------------

func TestResolveChainForSite_NoHistory(t *testing.T) {
	repo := newFakeRepo()
	svc := buildIncrementalSvc(repo, time.Now())
	tenantID := uuid.New()
	siteID := uuid.New()

	res, err := svc.resolveChainForSite(context.Background(), tenantID, siteID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// BASE bootstrap (ADR-048 fix): no history → gen-0 base-increment, NOT a
	// plain full. The agent writes a full file index off an empty baseline so
	// the next run can produce a real increment.
	if !res.IsIncremental {
		t.Error("expected gen-0 base-increment (no history), got is_incremental=false")
	}
	if res.Generation != 0 {
		t.Errorf("expected generation=0, got %d", res.Generation)
	}
	if res.ParentSnapshotID != nil {
		t.Errorf("expected nil parent for a gen-0 base, got %v", res.ParentSnapshotID)
	}
	if res.BaseSnapshotID != nil {
		t.Errorf("expected nil base for a gen-0 base, got %v", res.BaseSnapshotID)
	}
	if res.ChainID != nil {
		t.Errorf("expected nil chain (repo self-anchors at create), got %v", res.ChainID)
	}
}

func TestResolveChainForSite_FirstIncrement(t *testing.T) {
	repo := newFakeRepo()
	now := time.Now()
	svc := buildIncrementalSvc(repo, now)

	tenantID := uuid.New()
	siteID := uuid.New()
	prevID := uuid.New()
	finishedAt := now.Add(-1 * time.Hour) // 1 hour ago — well within 7 days

	// A prior completed full backup WITH a file index.
	prev := Snapshot{
		ID:            prevID,
		TenantID:      tenantID,
		SiteID:        siteID,
		Status:        StatusCompleted,
		IsIncremental: false,
		Generation:    0,
		FinishedAt:    &finishedAt,
	}
	repo.setSnapshot(prev)
	// Simulate that the prior snapshot has file index rows.
	repo.fileIndexCounts[prevID] = 10

	res, err := svc.resolveChainForSite(context.Background(), tenantID, siteID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.IsIncremental {
		t.Error("expected is_incremental=true for first increment off a full base with file index")
	}
	if res.Generation != 1 {
		t.Errorf("expected generation=1, got %d", res.Generation)
	}
	if res.ParentSnapshotID == nil || *res.ParentSnapshotID != prevID {
		t.Errorf("expected ParentSnapshotID=%v, got %v", prevID, res.ParentSnapshotID)
	}
	if res.ChainID == nil || *res.ChainID != prevID {
		t.Errorf("expected ChainID=%v (equals base), got %v", prevID, res.ChainID)
	}
}

// TestResolveChainForSite_IncrementOffBaseIncrement covers the bootstrap chain:
// the prior snapshot is the gen-0 BASE-INCREMENT (is_incremental=true,
// generation=0, chain_id=self, base_snapshot_id=NULL). The next run must resolve
// to gen-1 with base_snapshot_id = the base itself (prev.ID) — NOT the zero UUID,
// which previously stamped a non-existent base_snapshot_id FK and 500'd the first
// increment.
func TestResolveChainForSite_IncrementOffBaseIncrement(t *testing.T) {
	repo := newFakeRepo()
	now := time.Now()
	svc := buildIncrementalSvc(repo, now)

	tenantID := uuid.New()
	siteID := uuid.New()
	baseID := uuid.New()
	finishedAt := now.Add(-1 * time.Hour)

	// The prior snapshot is the gen-0 base-increment: incremental, chain anchored
	// to itself, with NO base above it.
	prev := Snapshot{
		ID:            baseID,
		TenantID:      tenantID,
		SiteID:        siteID,
		Status:        StatusCompleted,
		IsIncremental: true,
		Generation:    0,
		ChainID:       &baseID,
		// BaseSnapshotID intentionally nil — it IS the base.
		FinishedAt: &finishedAt,
	}
	repo.setSnapshot(prev)
	repo.fileIndexCounts[baseID] = 42 // the base wrote a full file index

	res, err := svc.resolveChainForSite(context.Background(), tenantID, siteID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.IsIncremental || res.Generation != 1 {
		t.Fatalf("expected incremental gen 1, got incremental=%v gen=%d", res.IsIncremental, res.Generation)
	}
	if res.ParentSnapshotID == nil || *res.ParentSnapshotID != baseID {
		t.Errorf("expected ParentSnapshotID=%v, got %v", baseID, res.ParentSnapshotID)
	}
	if res.ChainID == nil || *res.ChainID != baseID {
		t.Errorf("expected ChainID=%v, got %v", baseID, res.ChainID)
	}
	// The crux: base must be the gen-0 base itself, never the zero UUID.
	if res.BaseSnapshotID == nil || *res.BaseSnapshotID != baseID {
		t.Errorf("expected BaseSnapshotID=%v (the base), got %v", baseID, res.BaseSnapshotID)
	}
	if res.BaseSnapshotID != nil && *res.BaseSnapshotID == uuid.Nil {
		t.Error("BaseSnapshotID is the zero UUID — would FK-violate and 500 the first increment")
	}
}

// TestResolveChainForSite_IncrementalBaseWithEmptyIndex_Rebases covers the
// version-skew bug: a base taken on an old agent records is_incremental=true but
// writes ZERO backup_file_index rows (full-zip fallback). The next run must NOT
// try to diff against it (that re-hashes the whole tree — the 24-min QA bug); it
// must re-base to a fresh gen-0 base-increment.
func TestResolveChainForSite_IncrementalBaseWithEmptyIndex_Rebases(t *testing.T) {
	repo := newFakeRepo()
	now := time.Now()
	svc := buildIncrementalSvc(repo, now)

	tenantID := uuid.New()
	siteID := uuid.New()
	prevID := uuid.New()
	finishedAt := now.Add(-1 * time.Hour)

	prevChain := prevID
	repo.setSnapshot(Snapshot{
		ID:            prevID,
		TenantID:      tenantID,
		SiteID:        siteID,
		Status:        StatusCompleted,
		IsIncremental: true,
		Generation:    0,
		ChainID:       &prevChain,
		FinishedAt:    &finishedAt,
	})
	repo.fileIndexCounts[prevID] = 0 // empty index — cannot be diffed

	res, err := svc.resolveChainForSite(context.Background(), tenantID, siteID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Must re-base: gen-0 base-increment with no parent (NOT a gen-1 increment).
	if !res.IsIncremental || res.Generation != 0 {
		t.Fatalf("expected a gen-0 base-increment re-base, got incremental=%v gen=%d", res.IsIncremental, res.Generation)
	}
	if res.ParentSnapshotID != nil {
		t.Errorf("expected no parent on a re-base, got %v", res.ParentSnapshotID)
	}
}

func TestResolveChainForSite_StaleChain(t *testing.T) {
	repo := newFakeRepo()
	now := time.Now()
	svc := buildIncrementalSvc(repo, now)

	tenantID := uuid.New()
	siteID := uuid.New()
	prevID := uuid.New()
	// Finished 8 days ago — exceeds the 7-day base window.
	finishedAt := now.Add(-8 * 24 * time.Hour)

	prev := Snapshot{
		ID:         prevID,
		TenantID:   tenantID,
		SiteID:     siteID,
		Status:     StatusCompleted,
		Generation: 1,
		FinishedAt: &finishedAt,
	}
	repo.setSnapshot(prev)
	repo.fileIndexCounts[prevID] = 10

	res, err := svc.resolveChainForSite(context.Background(), tenantID, siteID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Stale chain re-bases as a gen-0 base-increment (no parent), not a plain full.
	if !res.IsIncremental {
		t.Error("expected gen-0 base-increment (stale chain >7 days), got is_incremental=false")
	}
	if res.Generation != 0 {
		t.Errorf("expected generation=0 on re-base, got %d", res.Generation)
	}
	if res.ParentSnapshotID != nil {
		t.Errorf("expected nil parent on stale re-base, got %v", res.ParentSnapshotID)
	}
}

func TestResolveChainForSite_MaxDepth(t *testing.T) {
	repo := newFakeRepo()
	now := time.Now()
	svc := buildIncrementalSvc(repo, now)

	tenantID := uuid.New()
	siteID := uuid.New()
	prevID := uuid.New()
	chainID := uuid.New()
	finishedAt := now.Add(-1 * time.Hour)

	prev := Snapshot{
		ID:            prevID,
		TenantID:      tenantID,
		SiteID:        siteID,
		Status:        StatusCompleted,
		IsIncremental: true,
		Generation:    BackupMaxChainDepth, // at the limit
		ChainID:       &chainID,
		FinishedAt:    &finishedAt,
	}
	repo.setSnapshot(prev)
	repo.fileIndexCounts[prevID] = 10

	res, err := svc.resolveChainForSite(context.Background(), tenantID, siteID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Max-depth re-bases as a gen-0 base-increment (no parent), not a plain full.
	if !res.IsIncremental {
		t.Errorf("expected gen-0 base-increment (generation=%d >= max %d), got is_incremental=false",
			BackupMaxChainDepth, BackupMaxChainDepth)
	}
	if res.Generation != 0 {
		t.Errorf("expected generation=0 on re-base, got %d", res.Generation)
	}
	if res.ParentSnapshotID != nil {
		t.Errorf("expected nil parent on max-depth re-base, got %v", res.ParentSnapshotID)
	}
}

func TestResolveChainForSite_NoFileIndex_AutoBase(t *testing.T) {
	repo := newFakeRepo()
	now := time.Now()
	svc := buildIncrementalSvc(repo, now)

	tenantID := uuid.New()
	siteID := uuid.New()
	prevID := uuid.New()
	finishedAt := now.Add(-1 * time.Hour)

	// A completed full backup but NO file index rows (pre-m44 zip-based backup).
	prev := Snapshot{
		ID:            prevID,
		TenantID:      tenantID,
		SiteID:        siteID,
		Status:        StatusCompleted,
		IsIncremental: false,
		Generation:    0,
		FinishedAt:    &finishedAt,
	}
	repo.setSnapshot(prev)
	// fileIndexCounts not set → defaults to 0

	res, err := svc.resolveChainForSite(context.Background(), tenantID, siteID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// A prior full backup with no file index can't be diffed, so we bootstrap a
	// fresh gen-0 base-increment (writes its own full file index) rather than
	// dispatching another plain full.
	if !res.IsIncremental {
		t.Error("expected gen-0 base-increment (no file index rows on prior), got is_incremental=false")
	}
	if res.Generation != 0 {
		t.Errorf("expected generation=0, got %d", res.Generation)
	}
	if res.ParentSnapshotID != nil {
		t.Errorf("expected nil parent for a gen-0 base, got %v", res.ParentSnapshotID)
	}
}

// ---------------------------------------------------------------------------
// ADR-051 archive-delta increment recording — SubmitManifest service tests.
// An increment now submits the SAME SubmitManifestRequest as a full backup
// (zip parts + DB dump + files-list + tombstones manifest entries) with its
// per-cycle telemetry as optional top-level fields. The retired
// SubmitIncrementalManifest / backup_file_index path is gone.
// ---------------------------------------------------------------------------

func TestSubmitManifest_StampsCycleStats(t *testing.T) {
	repo := newFakeRepo()
	svc := buildIncrementalSvc(repo, time.Now())

	tenantID := uuid.New()
	snapshotID := uuid.New()
	repo.setSnapshot(Snapshot{ID: snapshotID, TenantID: tenantID, Status: StatusRunning})

	// An increment that packed one changed file (one zip part) + a files-list +
	// one tombstone, carrying cycle telemetry top-level.
	chunkHash := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	req := agentcmd.SubmitManifestRequest{
		SnapshotID:   snapshotID.String(),
		AgeRecipient: "age1test",
		Entries: []agentcmd.ManifestEntry{
			{Path: "plugins.part001.zip", EntryKind: EntryKindPlugin, Chunks: []agentcmd.ChunkRef{{Blake3: chunkHash, Size: 100}}},
			{Path: "files.list", EntryKind: EntryKindFilesList, Chunks: []agentcmd.ChunkRef{{Blake3: chunkHash, Size: 50}}},
			{Path: "wp-content/plugins/deleted/main.php", EntryKind: EntryKindTombstones},
		},
		CycleFilesScanned:  100,
		CycleFilesChanged:  1,
		CycleFilesDeleted:  1,
		CycleBytesUploaded: 100,
	}

	if _, _, err := svc.SubmitManifest(context.Background(), tenantID, snapshotID, req); err != nil {
		t.Fatalf("SubmitManifest error: %v", err)
	}

	// Cycle telemetry must be stamped on the snapshot.
	stats := repo.cycleStats[snapshotID]
	if stats.CycleFilesScanned != 100 {
		t.Errorf("expected cycle_files_scanned=100, got %d", stats.CycleFilesScanned)
	}
	if stats.CycleFilesDeleted != 1 {
		t.Errorf("expected cycle_files_deleted=1, got %d", stats.CycleFilesDeleted)
	}
	if stats.CycleBytesUploaded != 100 {
		t.Errorf("expected cycle_bytes_uploaded=100, got %d", stats.CycleBytesUploaded)
	}

	// The archive-delta increment must NOT write backup_file_index rows (the
	// per-file index is retired as the diff oracle).
	if rows := repo.fileIndexRows[snapshotID]; len(rows) != 0 {
		t.Errorf("archive-delta increment must not write backup_file_index rows; got %d", len(rows))
	}
}

func TestSubmitManifest_FullBackupNoCycleStats(t *testing.T) {
	// A full backup sends zero cycle counters → the snapshot row is left untouched.
	repo := newFakeRepo()
	svc := buildIncrementalSvc(repo, time.Now())

	tenantID := uuid.New()
	snapshotID := uuid.New()
	repo.setSnapshot(Snapshot{ID: snapshotID, TenantID: tenantID, Status: StatusRunning})

	req := agentcmd.SubmitManifestRequest{
		SnapshotID:   snapshotID.String(),
		AgeRecipient: "age1test",
		Entries:      []agentcmd.ManifestEntry{},
	}
	if _, _, err := svc.SubmitManifest(context.Background(), tenantID, snapshotID, req); err != nil {
		t.Fatalf("SubmitManifest error: %v", err)
	}

	if _, ok := repo.cycleStats[snapshotID]; ok {
		t.Error("full backup (zero cycle counters) must not stamp cycle stats")
	}
	if rows := repo.fileIndexRows[snapshotID]; len(rows) != 0 {
		t.Errorf("full-backup path should not write backup_file_index rows; got %d rows", len(rows))
	}
}

func TestSubmitManifest_RejectsAlreadyCompleted(t *testing.T) {
	repo := newFakeRepo()
	svc := buildIncrementalSvc(repo, time.Now())

	tenantID := uuid.New()
	snapshotID := uuid.New()
	repo.setSnapshot(Snapshot{ID: snapshotID, TenantID: tenantID, Status: StatusRunning})

	req := agentcmd.SubmitManifestRequest{SnapshotID: snapshotID.String()}

	if _, _, err := svc.SubmitManifest(context.Background(), tenantID, snapshotID, req); err != nil {
		t.Fatalf("first call error: %v", err)
	}
	// Second call: snapshot is now completed → conflict.
	_, _, err := svc.SubmitManifest(context.Background(), tenantID, snapshotID, req)
	if err == nil {
		t.Fatal("expected conflict error on second call, got nil")
	}
	var de *domain.Error
	if !errors.As(err, &de) || de.Kind != domain.KindConflict {
		t.Errorf("expected KindConflict, got: %v", err)
	}
}
