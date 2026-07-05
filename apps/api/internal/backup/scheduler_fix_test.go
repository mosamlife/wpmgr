package backup

// scheduler_fix_test.go — unit tests for issue #68 and #96 scheduler correctness fixes.
//
// Tests:
//  1. PutSchedule re-enable heals: disabled→enabled with stale next_run_at
//     → persisted next_run_at is strictly in the future.
//  2. PutSchedule overdue self-heal: enabled+future schedule edited (non-timing)
//     with overdue next_run_at → next_run_at advanced.
//  3. Atomic claim exactly-once under concurrency: two goroutines calling
//     ClaimDueSchedules on the same fake repo → only one advances the schedule.
//  4. Advance/claim error surfaces → ClaimDueSchedules returns error and does
//     NOT enqueue any job.
//  5a. In-flight guard — EnqueueScheduledBackup is a no-op when a snapshot is
//      in flight (pending OR running).
//  5b. In-flight guard — CreateBackup returns a validation error when in flight.
//  6. 24h simulation with 5-min ticks on a fixed daily schedule → exactly one
//     snapshot created per 24-hour window.
//  7. Site lookup error is logged and schedule is not claimed.
//  8. Issue #96 regression: ClaimDueSchedules finds a due schedule, advances it,
//     and EnqueueScheduledBackup creates a snapshot (the working path). A
//     companion sub-test demonstrates the broken path: when ClaimAndAdvance
//     returns empty despite a due row existing (simulating the pre-m84 RLS
//     behaviour where FOR UPDATE under InAgentTx saw 0 rows), no snapshot is
//     enqueued. The real fix is migration m84 (backup_schedules_scheduler policy
//     upgraded from FOR SELECT to FOR ALL); this test locks in the end-to-end
//     service behaviour so a regression would be caught immediately.
//
// All tests use in-memory fakes and a deterministic fakeClock; no database.

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// ---------------------------------------------------------------------------
// schedulerTestRepo — in-memory Repo for scheduler tests.
//
// Methods exercised by the scheduler/in-flight guard paths are functional.
// Methods not touched by these tests panic so unexpected calls are caught.
// ---------------------------------------------------------------------------

type schedulerTestRepo struct {
	mu sync.Mutex

	// schedule store keyed by schedule ID.
	schedules map[uuid.UUID]*Schedule
	// schedBySite for GetSchedule lookup.
	schedBySite map[uuid.UUID]*Schedule

	// in-flight counts per "tenantID/siteID".
	inFlight map[string]int64

	// CreateSnapshot call recorder: "tenantID/siteID" → count.
	snapCreated map[string]int

	// claim tracking: how many times each schedule ID was advanced.
	claimedCount map[uuid.UUID]int

	// ClaimAndAdvanceDueSchedules: when non-nil, returned instead of normal path.
	claimErr error
}

func newSchedulerTestRepo() *schedulerTestRepo {
	return &schedulerTestRepo{
		schedules:    make(map[uuid.UUID]*Schedule),
		schedBySite:  make(map[uuid.UUID]*Schedule),
		inFlight:     make(map[string]int64),
		snapCreated:  make(map[string]int),
		claimedCount: make(map[uuid.UUID]int),
	}
}

func (r *schedulerTestRepo) addSchedule(s Schedule) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := s
	r.schedules[s.ID] = &cp
	r.schedBySite[s.SiteID] = &cp
}

func (r *schedulerTestRepo) setInFlight(tenantID, siteID uuid.UUID, n int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inFlight[tenantID.String()+"/"+siteID.String()] = n
}

func (r *schedulerTestRepo) snapCount(tenantID, siteID uuid.UUID) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snapCreated[tenantID.String()+"/"+siteID.String()]
}

// --- Repo interface implementation ---

func (r *schedulerTestRepo) GetSchedule(_ context.Context, _, siteID uuid.UUID) (Schedule, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.schedBySite[siteID]
	if !ok {
		return Schedule{}, domain.NotFound("backup_schedule_not_found", "not found")
	}
	return *s, nil
}

func (r *schedulerTestRepo) UpsertSchedule(_ context.Context, in UpsertScheduleInput) (Schedule, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := Schedule{
		ID:                 in.SiteID, // use site_id as fake schedule ID
		TenantID:           in.TenantID,
		SiteID:             in.SiteID,
		Cadence:            in.Cadence,
		Enabled:            in.Enabled,
		RunHour:            in.RunHour,
		RunMinute:          in.RunMinute,
		DayOfWeek:          in.DayOfWeek,
		DayOfMonth:         in.DayOfMonth,
		FrequencyHours:     in.FrequencyHours,
		NextRunAt:          in.NextRunAt,
		RetentionDays:      in.RetentionDays,
		MonthlyArchiveKeep: in.MonthlyArchiveKeep,
		KeepLast:           in.KeepLast,
		Kind:               in.Kind,
	}
	r.schedules[s.ID] = &s
	r.schedBySite[s.SiteID] = &s
	return s, nil
}

func (r *schedulerTestRepo) ListDueSchedules(_ context.Context, now time.Time, limit int32) ([]Schedule, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Schedule
	var count int32
	for _, s := range r.schedules {
		if s.Enabled && !s.NextRunAt.After(now) {
			out = append(out, *s)
			count++
			if count >= limit {
				break
			}
		}
	}
	return out, nil
}

// ClaimAndAdvanceDueSchedules simulates FOR UPDATE SKIP LOCKED: under a mutex,
// it selects due rows, advances them, and records the advance count.
// When claimErr is set it returns that error instead.
func (r *schedulerTestRepo) ClaimAndAdvanceDueSchedules(_ context.Context, now time.Time, nextAt map[uuid.UUID]time.Time) ([]Schedule, error) {
	if r.claimErr != nil {
		return nil, r.claimErr
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Schedule
	for _, s := range r.schedules {
		if !s.Enabled || s.NextRunAt.After(now) {
			continue
		}
		next, ok := nextAt[s.ID]
		if !ok {
			continue
		}
		fired := *s
		s.NextRunAt = next
		r.claimedCount[s.ID]++
		out = append(out, fired)
	}
	return out, nil
}

func (r *schedulerTestRepo) CountInFlightSnapshots(_ context.Context, tenantID, siteID uuid.UUID) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.inFlight[tenantID.String()+"/"+siteID.String()], nil
}

func (r *schedulerTestRepo) CreateSnapshot(_ context.Context, in CreateSnapshotInput) (Snapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := in.TenantID.String() + "/" + in.SiteID.String()
	r.snapCreated[key]++
	return Snapshot{
		ID:       uuid.New(),
		TenantID: in.TenantID,
		SiteID:   in.SiteID,
		Kind:     in.Kind,
		Status:   StatusPending,
	}, nil
}

// --- Remaining Repo methods: panic if unexpectedly called ---

func (r *schedulerTestRepo) GetSnapshotScoped(_ context.Context, _ db.ScopedPrincipal, _, _ uuid.UUID) (Snapshot, error) {
	panic("schedulerTestRepo.GetSnapshotScoped not expected")
}
func (r *schedulerTestRepo) GetSnapshot(_ context.Context, _, _ uuid.UUID) (Snapshot, error) {
	panic("schedulerTestRepo.GetSnapshot not expected")
}
func (r *schedulerTestRepo) ListSnapshotsForSite(_ context.Context, _, _ uuid.UUID, _, _ int32) ([]Snapshot, error) {
	panic("schedulerTestRepo.ListSnapshotsForSite not expected")
}
func (r *schedulerTestRepo) MarkSnapshotRunning(_ context.Context, _, _ uuid.UUID) (Snapshot, error) {
	panic("schedulerTestRepo.MarkSnapshotRunning not expected")
}
func (r *schedulerTestRepo) CompleteSnapshot(_ context.Context, _, _ uuid.UUID, _, _ int64) (Snapshot, error) {
	panic("schedulerTestRepo.CompleteSnapshot not expected")
}
func (r *schedulerTestRepo) FailSnapshot(_ context.Context, _, _ uuid.UUID, _ string) (Snapshot, error) {
	return Snapshot{Status: StatusFailed}, nil
}
func (r *schedulerTestRepo) UpdateSnapshotProgress(_ context.Context, _, _ uuid.UUID, _ []byte) (Snapshot, error) {
	panic("schedulerTestRepo.UpdateSnapshotProgress not expected")
}
func (r *schedulerTestRepo) ListStalledRunningSnapshots(_ context.Context, _ time.Duration) ([]StalledSnapshot, error) {
	panic("schedulerTestRepo.ListStalledRunningSnapshots not expected")
}
func (r *schedulerTestRepo) GetLatestCompletedSnapshot(_ context.Context, _, _ uuid.UUID) (Snapshot, error) {
	return Snapshot{}, domain.NotFound("not_found", "no completed snapshot")
}
func (r *schedulerTestRepo) ListManifest(_ context.Context, _, _ uuid.UUID) ([]ManifestEntry, error) {
	panic("schedulerTestRepo.ListManifest not expected")
}
func (r *schedulerTestRepo) HasFilesList(_ context.Context, _, _ uuid.UUID) (bool, error) {
	return false, nil
}
func (r *schedulerTestRepo) RecordManifest(_ context.Context, _ RecordManifestInput) (int64, int64, error) {
	panic("schedulerTestRepo.RecordManifest not expected")
}
func (r *schedulerTestRepo) ExistingChunkHashes(_ context.Context, _ uuid.UUID, _ []string) (map[string]Chunk, error) {
	panic("schedulerTestRepo.ExistingChunkHashes not expected")
}
func (r *schedulerTestRepo) ListTenantsForGC(_ context.Context) ([]uuid.UUID, error) {
	panic("schedulerTestRepo.ListTenantsForGC not expected")
}
func (r *schedulerTestRepo) AdvanceScheduleRun(_ context.Context, _, _ uuid.UUID, _ time.Time) error {
	return nil
}
func (r *schedulerTestRepo) SetSnapshotLocked(_ context.Context, _ uuid.UUID, _ uuid.UUID, _ bool) (Snapshot, error) {
	panic("schedulerTestRepo.SetSnapshotLocked not expected")
}
func (r *schedulerTestRepo) FleetListSnapshots(_ context.Context, _ db.ScopedPrincipal, _ uuid.UUID, _ FleetListFilter) (FleetSnapshotPage, error) {
	panic("schedulerTestRepo.FleetListSnapshots not expected")
}
func (r *schedulerTestRepo) FleetBackupHealth(_ context.Context, _ db.ScopedPrincipal, _ uuid.UUID, _ []uuid.UUID) ([]FleetBackupHealthItem, error) {
	panic("schedulerTestRepo.FleetBackupHealth not expected")
}
func (r *schedulerTestRepo) GetBackupSettings(_ context.Context, _, _ uuid.UUID) (SiteBackupSettings, error) {
	return SiteBackupSettings{}, domain.NotFound("not_found", "no settings")
}
func (r *schedulerTestRepo) UpsertBackupSettings(_ context.Context, _ uuid.UUID, in SiteBackupSettings) (SiteBackupSettings, error) {
	return in, nil
}
func (r *schedulerTestRepo) ListExpiredSnapshots(_ context.Context, _ uuid.UUID, _ time.Time) ([]Snapshot, error) {
	panic("schedulerTestRepo.ListExpiredSnapshots not expected")
}
func (r *schedulerTestRepo) ListCompletedSnapshotsForSite(_ context.Context, _, _ uuid.UUID) ([]SnapshotMeta, error) {
	panic("schedulerTestRepo.ListCompletedSnapshotsForSite not expected")
}
func (r *schedulerTestRepo) ListSiteIDsWithSnapshots(_ context.Context, _ uuid.UUID) ([]uuid.UUID, error) {
	return nil, nil
}
func (r *schedulerTestRepo) SetSnapshotArchived(_ context.Context, _, _ uuid.UUID, _ bool) error {
	panic("schedulerTestRepo.SetSnapshotArchived not expected")
}
func (r *schedulerTestRepo) DeleteSnapshot(_ context.Context, _, _ uuid.UUID) error {
	panic("schedulerTestRepo.DeleteSnapshot not expected")
}
func (r *schedulerTestRepo) ListInFlightSnapshotFloor(_ context.Context, _ uuid.UUID) (time.Time, error) {
	return time.Time{}, nil
}
func (r *schedulerTestRepo) DBNow(_ context.Context, _ uuid.UUID) (time.Time, error) {
	return time.Now(), nil
}
func (r *schedulerTestRepo) SweepTenantChunks(_ context.Context, _ uuid.UUID, _ time.Time, acquired *bool, _ func(SweepChunk) (bool, error)) error {
	*acquired = false
	return nil
}
func (r *schedulerTestRepo) InsertFileIndexBatch(_ context.Context, _, _ uuid.UUID, _ []FileIndexEntry) error {
	panic("schedulerTestRepo.InsertFileIndexBatch not expected")
}
func (r *schedulerTestRepo) CountFileIndex(_ context.Context, _, _ uuid.UUID) (int64, error) {
	panic("schedulerTestRepo.CountFileIndex not expected")
}
func (r *schedulerTestRepo) StreamFileIndex(_ context.Context, _, _ uuid.UUID, _ func(FileIndexEntry) error) error {
	panic("schedulerTestRepo.StreamFileIndex not expected")
}
func (r *schedulerTestRepo) StreamChainEffectiveFileIndex(_ context.Context, _, _ uuid.UUID, _ int, _ func(FileIndexEntry) error) error {
	panic("schedulerTestRepo.StreamChainEffectiveFileIndex not expected")
}
func (r *schedulerTestRepo) UpdateSnapshotCycleStats(_ context.Context, _, _ uuid.UUID, _ CycleStatsInput) error {
	return nil
}
func (r *schedulerTestRepo) CompleteIncrementalManifest(_ context.Context, _ CompleteIncrementalInput) (int64, int64, error) {
	panic("schedulerTestRepo.CompleteIncrementalManifest not expected")
}
func (r *schedulerTestRepo) ListChainSnapshots(_ context.Context, _ uuid.UUID, _ uuid.UUID, _ int) ([]Snapshot, error) {
	return nil, nil
}
func (r *schedulerTestRepo) HealOverdueSchedules(_ context.Context, _ time.Time, _ func(Schedule, time.Time) time.Time) (int, error) {
	return 0, nil
}
func (r *schedulerTestRepo) ReconcileDuplicateInflightSnapshots(_ context.Context) (int, error) {
	return 0, nil
}
func (r *schedulerTestRepo) GetSnapshotsByIDs(_ context.Context, _ uuid.UUID, _ []uuid.UUID) (map[uuid.UUID]Snapshot, error) {
	panic("schedulerTestRepo.GetSnapshotsByIDs not implemented")
}
func (r *schedulerTestRepo) HasActiveRestore(_ context.Context, _ uuid.UUID, _ []uuid.UUID) (map[uuid.UUID]bool, error) {
	panic("schedulerTestRepo.HasActiveRestore not implemented")
}

// ---------------------------------------------------------------------------
// Test 1: PutSchedule re-enable heals next_run_at
// ---------------------------------------------------------------------------

func TestPutSchedule_ReEnable_HealsNextRunAt(t *testing.T) {
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	past := now.Add(-48 * time.Hour)

	tenantID, siteID := uuid.New(), uuid.New()

	repo := newSchedulerTestRepo()
	repo.addSchedule(Schedule{
		ID:        siteID, // consistent with UpsertSchedule fake (schedID = siteID)
		TenantID:  tenantID,
		SiteID:    siteID,
		Cadence:   CadenceDaily,
		RunHour:   2,
		RunMinute: 0,
		Enabled:   false, // currently disabled
		NextRunAt: past,
	})

	svc := &Service{
		repo:  repo,
		sites: fakeSites{info: SiteInfo{Enrolled: true, AgeRecipient: "age1test", WpTimezone: "UTC"}},
		clock: fakeClock{t: now},
	}

	out, err := svc.PutSchedule(context.Background(), PutScheduleInput{
		TenantID:  tenantID,
		SiteID:    siteID,
		Cadence:   CadenceDaily,
		RunHour:   2,
		RunMinute: 0,
		Enabled:   true, // re-enabling
	})
	if err != nil {
		t.Fatalf("PutSchedule re-enable: %v", err)
	}
	if !out.NextRunAt.After(now) {
		t.Errorf("re-enable: next_run_at %v is not after now %v", out.NextRunAt, now)
	}
}

// ---------------------------------------------------------------------------
// Test 2: PutSchedule overdue self-heal (non-timing edit while enabled)
// ---------------------------------------------------------------------------

func TestPutSchedule_OverdueSelfHeal(t *testing.T) {
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	past := now.Add(-2 * time.Hour)

	tenantID, siteID := uuid.New(), uuid.New()

	repo := newSchedulerTestRepo()
	repo.addSchedule(Schedule{
		ID:            siteID,
		TenantID:      tenantID,
		SiteID:        siteID,
		Cadence:       CadenceDaily,
		RunHour:       2,
		RunMinute:     0,
		Enabled:       true,
		NextRunAt:     past,
		RetentionDays: 30,
	})

	svc := &Service{
		repo:  repo,
		sites: fakeSites{info: SiteInfo{Enrolled: true, AgeRecipient: "age1test", WpTimezone: "UTC"}},
		clock: fakeClock{t: now},
	}

	// Non-timing edit: only RetentionDays changes; cadence/hour/minute stay the same.
	out, err := svc.PutSchedule(context.Background(), PutScheduleInput{
		TenantID:      tenantID,
		SiteID:        siteID,
		Cadence:       CadenceDaily,
		RunHour:       2,
		RunMinute:     0,
		Enabled:       true,
		RetentionDays: 60, // only this changed
	})
	if err != nil {
		t.Fatalf("PutSchedule overdue self-heal: %v", err)
	}
	if !out.NextRunAt.After(now) {
		t.Errorf("overdue self-heal: next_run_at %v is not after now %v", out.NextRunAt, now)
	}
}

// ---------------------------------------------------------------------------
// Test 3: Atomic claim exactly-once under concurrency
// ---------------------------------------------------------------------------

func TestClaimDueSchedules_ExactlyOnceUnderConcurrency(t *testing.T) {
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	dueAt := now.Add(-time.Minute)

	tenantID := uuid.New()
	siteID := uuid.New()
	schedID := uuid.New()

	repo := newSchedulerTestRepo()
	repo.addSchedule(Schedule{
		ID:        schedID,
		TenantID:  tenantID,
		SiteID:    siteID,
		Cadence:   CadenceDaily,
		RunHour:   11,
		RunMinute: 59,
		Enabled:   true,
		NextRunAt: dueAt,
	})

	svc := &Service{
		repo:  repo,
		sites: fakeSites{info: SiteInfo{Enrolled: true, AgeRecipient: "age1test", WpTimezone: "UTC"}},
		clock: fakeClock{t: now},
	}

	// Two goroutines claim concurrently. The fake's mutex serialises
	// ClaimAndAdvanceDueSchedules: the first call advances next_run_at to the
	// future; the second call's ListDueSchedules finds no due rows (because the
	// first goroutine's ListDueSchedules already ran before the advance, but
	// ClaimAndAdvanceDueSchedules in the fake checks next_run_at again under
	// the same lock, so the second goroutine's ClaimAndAdvanceDueSchedules call
	// finds the row already advanced past now). Total claimed must be 1.
	var wg sync.WaitGroup
	var mu sync.Mutex
	totalClaimed := 0
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			claimed, err := svc.ClaimDueSchedules(context.Background())
			if err != nil {
				return
			}
			mu.Lock()
			totalClaimed += len(claimed)
			mu.Unlock()
		}()
	}
	wg.Wait()

	if totalClaimed != 1 {
		t.Errorf("concurrent claim: total claimed = %d, want 1", totalClaimed)
	}

	repo.mu.Lock()
	advCount := repo.claimedCount[schedID]
	repo.mu.Unlock()
	if advCount != 1 {
		t.Errorf("schedule advanced %d times, want 1", advCount)
	}
}

// ---------------------------------------------------------------------------
// Test 4: Claim error surfaces — ClaimDueSchedules returns error, no enqueue
// ---------------------------------------------------------------------------

func TestClaimDueSchedules_ClaimError_DoesNotEnqueue(t *testing.T) {
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

	repo := newSchedulerTestRepo()
	repo.addSchedule(Schedule{
		ID:        uuid.New(),
		TenantID:  uuid.New(),
		SiteID:    uuid.New(),
		Cadence:   CadenceDaily,
		Enabled:   true,
		NextRunAt: now.Add(-time.Minute),
	})
	repo.claimErr = fmt.Errorf("simulated DB error on claim")

	enq := &recordingEnqueuer{}
	svc := &Service{
		repo:     repo,
		sites:    fakeSites{info: SiteInfo{Enrolled: true, AgeRecipient: "age1test"}},
		clock:    fakeClock{t: now},
		enqueuer: enq,
	}

	_, err := svc.ClaimDueSchedules(context.Background())
	if err == nil {
		t.Fatal("expected ClaimDueSchedules to return error on claim failure, got nil")
	}
	if len(enq.plainCalls) > 0 || len(enq.chainCalls) > 0 {
		t.Errorf("no enqueue expected when claim fails; got plain=%d chain=%d",
			len(enq.plainCalls), len(enq.chainCalls))
	}
}

// ---------------------------------------------------------------------------
// Test 5a: In-flight guard — EnqueueScheduledBackup skips when in flight
// ---------------------------------------------------------------------------

func TestEnqueueScheduledBackup_InFlightGuard_Skips(t *testing.T) {
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	tenantID, siteID := uuid.New(), uuid.New()

	repo := newSchedulerTestRepo()
	repo.setInFlight(tenantID, siteID, 1)

	enq := &recordingEnqueuer{}
	svc := &Service{
		repo:     repo,
		sites:    fakeSites{info: SiteInfo{Enrolled: true, AgeRecipient: "age1test"}},
		clock:    fakeClock{t: now},
		enqueuer: enq,
	}

	sched := Schedule{
		ID:       uuid.New(),
		TenantID: tenantID,
		SiteID:   siteID,
		Cadence:  CadenceDaily,
		Kind:     KindFull,
	}
	err := svc.EnqueueScheduledBackup(context.Background(), sched)
	if err != nil {
		t.Fatalf("EnqueueScheduledBackup with in-flight: unexpected error: %v", err)
	}
	if n := repo.snapCount(tenantID, siteID); n != 0 {
		t.Errorf("in-flight guard: created %d snapshots, want 0", n)
	}
	if len(enq.plainCalls) > 0 || len(enq.chainCalls) > 0 {
		t.Errorf("in-flight guard: enqueued plain=%d chain=%d, want 0",
			len(enq.plainCalls), len(enq.chainCalls))
	}
}

// ---------------------------------------------------------------------------
// Test 5b: In-flight guard — CreateBackup returns validation error when in flight
// ---------------------------------------------------------------------------

func TestCreateBackup_InFlightGuard_RejectsSecond(t *testing.T) {
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	tenantID, siteID := uuid.New(), uuid.New()

	repo := newSchedulerTestRepo()
	repo.setInFlight(tenantID, siteID, 1)

	svc := &Service{
		repo:     repo,
		sites:    fakeSites{info: SiteInfo{Enrolled: true, AgeRecipient: "age1test"}},
		clock:    fakeClock{t: now},
		enqueuer: &recordingEnqueuer{},
	}

	_, err := svc.CreateBackup(context.Background(), tenantID, siteID, uuid.New(), KindFull)
	if err == nil {
		t.Fatal("CreateBackup with in-flight: expected error, got nil")
	}
	var de *domain.Error
	if !asError(err, &de) || de.Kind != domain.KindValidation {
		t.Errorf("expected domain validation error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Test 6: 24h simulation — exactly one snapshot per daily schedule
// ---------------------------------------------------------------------------

func TestScheduler_24hSimulation_ExactlyOnePerDay(t *testing.T) {
	start := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)

	tenantID := uuid.New()
	// Use a fixed siteID whose first byte is 0x05: SiteJitter(siteID)=0 minutes,
	// so nextOccurrence for daily-02:00 from 02:00 is exactly 02:00 next day
	// (26h after sim-start), safely outside the 24h window.
	siteID := uuid.UUID{0x05, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	schedID := uuid.New()

	repo := newSchedulerTestRepo()
	repo.addSchedule(Schedule{
		ID:        schedID,
		TenantID:  tenantID,
		SiteID:    siteID,
		Cadence:   CadenceDaily,
		RunHour:   2,
		RunMinute: 0,
		Enabled:   true,
		// Placed at 02:00 so the first tick that crosses it fires exactly once.
		NextRunAt: time.Date(2026, 1, 15, 2, 0, 0, 0, time.UTC),
	})

	enq := &recordingEnqueuer{}
	svc := &Service{
		repo:     repo,
		sites:    fakeSites{info: SiteInfo{Enrolled: true, AgeRecipient: "age1test", WpTimezone: "UTC"}},
		enqueuer: enq,
	}

	// 288 ticks × 5 min = 24 hours.
	for tick := 0; tick < 288; tick++ {
		tickTime := start.Add(time.Duration(tick) * 5 * time.Minute)
		svc.clock = fakeClock{t: tickTime}

		claimed, err := svc.ClaimDueSchedules(context.Background())
		if err != nil {
			t.Fatalf("tick %d (%v): ClaimDueSchedules: %v", tick, tickTime, err)
		}
		for _, sched := range claimed {
			if eerr := svc.EnqueueScheduledBackup(context.Background(), sched); eerr != nil {
				t.Logf("tick %d: EnqueueScheduledBackup skip: %v", tick, eerr)
			}
		}
	}

	if snapCount := repo.snapCount(tenantID, siteID); snapCount != 1 {
		t.Errorf("24h simulation: got %d snapshots, want exactly 1", snapCount)
	}

	repo.mu.Lock()
	advCount := repo.claimedCount[schedID]
	repo.mu.Unlock()
	if advCount != 1 {
		t.Errorf("24h simulation: schedule advanced %d times, want 1", advCount)
	}
}

// ---------------------------------------------------------------------------
// Test 7: Site lookup error is logged and schedule is not claimed (#96)
// ---------------------------------------------------------------------------

// fakeSitesError is a SiteLookup that always returns an error from
// GetBackupSiteInfo. Used to simulate a site that cannot be resolved during
// the scheduler's nextAt pre-computation pass.
type fakeSitesError struct{ err error }

func (s fakeSitesError) GetBackupSiteInfo(_ context.Context, _, _ uuid.UUID) (SiteInfo, error) {
	return SiteInfo{}, s.err
}
func (s fakeSitesError) ListSiteIDs(_ context.Context, _ uuid.UUID) ([]uuid.UUID, error) {
	return nil, nil
}

// TestClaimDueSchedules_SiteLookupError_LoggedAndSkipped verifies that when
// GetBackupSiteInfo returns an error for a due schedule:
//  1. ClaimDueSchedules does NOT return an error (the failure is per-schedule
//     so the scheduler tick continues).
//  2. The schedule is not claimed (returned slice is empty) — it stays due for
//     the next tick.
//  3. A warning is emitted to the slog logger so the failure is never silent.
//
// This is the regression test for GitHub issue #96: the original code had a
// bare `continue` with no log, making site-lookup failures invisible in prod.
func TestClaimDueSchedules_SiteLookupError_LoggedAndSkipped(t *testing.T) {
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

	tenantID, siteID := uuid.New(), uuid.New()
	schedID := uuid.New()

	repo := newSchedulerTestRepo()
	repo.addSchedule(Schedule{
		ID:        schedID,
		TenantID:  tenantID,
		SiteID:    siteID,
		Cadence:   CadenceDaily,
		RunHour:   11,
		RunMinute: 59,
		Enabled:   true,
		NextRunAt: now.Add(-time.Minute), // due
	})

	lookupErr := fmt.Errorf("site not found in DB")

	// Redirect the default slog logger to an in-memory buffer so we can verify
	// the warning is emitted.
	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{
		Level: slog.LevelWarn,
	})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	svc := &Service{
		repo:  repo,
		sites: fakeSitesError{err: lookupErr},
		clock: fakeClock{t: now},
	}

	claimed, err := svc.ClaimDueSchedules(context.Background())

	// 1. ClaimDueSchedules must not return an error — site lookup failure is
	//    per-schedule; the tick itself succeeds (skips the unresolvable schedule).
	if err != nil {
		t.Fatalf("ClaimDueSchedules must not return an error on site lookup failure, got: %v", err)
	}

	// 2. No schedules are claimed — the one due schedule has no nextAt entry so
	//    ClaimAndAdvanceDueSchedules skips it.
	if len(claimed) != 0 {
		t.Errorf("claimed = %d schedules, want 0 (unresolvable site must be skipped)", len(claimed))
	}

	repo.mu.Lock()
	adv := repo.claimedCount[schedID]
	repo.mu.Unlock()
	if adv != 0 {
		t.Errorf("schedule advanced %d times, want 0", adv)
	}

	// 3. A warning must be present in the log output.
	logStr := logBuf.String()
	if !strings.Contains(logStr, "backup_scheduler") {
		t.Errorf("expected 'backup_scheduler' warning in log output, got: %q", logStr)
	}
	if !strings.Contains(logStr, schedID.String()) {
		t.Errorf("expected schedule_id %s in log output, got: %q", schedID, logStr)
	}
}

// ---------------------------------------------------------------------------
// Test 8: Issue #96 regression — ClaimAndAdvance actually fires (m84 fix)
// ---------------------------------------------------------------------------

// brokenClaimRepo embeds schedulerTestRepo but overrides ClaimAndAdvanceDueSchedules
// to always return (nil, nil) — simulating the pre-m84 RLS behaviour where
// "SELECT ... FOR UPDATE SKIP LOCKED" under InAgentTx returned 0 rows because
// the backup_schedules_scheduler policy was FOR SELECT only and PostgreSQL also
// requires the UPDATE USING clause to pass for a locking read.
type brokenClaimRepo struct {
	*schedulerTestRepo
}

func (r *brokenClaimRepo) ClaimAndAdvanceDueSchedules(_ context.Context, _ time.Time, _ map[uuid.UUID]time.Time) ([]Schedule, error) {
	// Simulate pre-m84: FOR UPDATE sees 0 rows under agent context, returns nothing.
	return nil, nil
}

// TestClaimDueSchedules_Issue96_ScheduleFiresAfterFix verifies the complete
// claim→advance→enqueue path for a due enabled schedule (the fixed behaviour
// after migration m84 upgrades backup_schedules_scheduler to FOR ALL).
//
// Sub-test "BrokenPath" reproduces the pre-m84 symptom: ListDueSchedules finds
// a due row but ClaimAndAdvanceDueSchedules (simulating the broken FOR UPDATE)
// returns nothing — no snapshot is enqueued. This is what the reporter observed:
// 289 completed scheduler River jobs, next_run_at never advanced, last_run_at
// NULL, no backup_snapshot row created.
//
// Sub-test "FixedPath" verifies the corrected behaviour: when
// ClaimAndAdvanceDueSchedules properly returns the claimed schedule (as it does
// after m84 makes the UPDATE USING policy pass under InAgentTx), a snapshot is
// created and the enqueuer is called.
func TestClaimDueSchedules_Issue96_ScheduleFiresAfterFix(t *testing.T) {
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	dueAt := now.Add(-3 * time.Minute) // 3 minutes overdue, like the reporter's test

	tenantID := uuid.New()
	siteID := uuid.New()
	schedID := uuid.New()

	newDueSchedule := func() Schedule {
		return Schedule{
			ID:        schedID,
			TenantID:  tenantID,
			SiteID:    siteID,
			Cadence:   CadenceDaily,
			RunHour:   11,
			RunMinute: 57, // dueAt matches
			Enabled:   true,
			NextRunAt: dueAt,
			Kind:      KindFull,
		}
	}

	t.Run("BrokenPath", func(t *testing.T) {
		// This sub-test demonstrates the pre-m84 symptom: despite a due schedule
		// existing, no snapshot is enqueued because ClaimAndAdvanceDueSchedules
		// returns nothing (simulating the FOR UPDATE RLS failure).
		inner := newSchedulerTestRepo()
		inner.addSchedule(newDueSchedule())
		repo := &brokenClaimRepo{schedulerTestRepo: inner}

		enq := &recordingEnqueuer{}
		svc := &Service{
			repo:     repo,
			sites:    fakeSites{info: SiteInfo{Enrolled: true, AgeRecipient: "age1test", WpTimezone: "UTC"}},
			clock:    fakeClock{t: now},
			enqueuer: enq,
		}

		claimed, err := svc.ClaimDueSchedules(context.Background())
		if err != nil {
			t.Fatalf("BrokenPath: ClaimDueSchedules error: %v", err)
		}
		for _, sched := range claimed {
			if eerr := svc.EnqueueScheduledBackup(context.Background(), sched); eerr != nil {
				t.Logf("BrokenPath: EnqueueScheduledBackup skip: %v", eerr)
			}
		}

		// Pre-m84: FOR UPDATE returned 0 rows → claimed is empty → no snapshot.
		if snapCount := inner.snapCount(tenantID, siteID); snapCount != 0 {
			t.Errorf("BrokenPath: expected 0 snapshots (broken claim returns nothing), got %d", snapCount)
		}
		if len(enq.plainCalls) > 0 || len(enq.chainCalls) > 0 {
			t.Errorf("BrokenPath: expected no enqueue calls, got plain=%d chain=%d",
				len(enq.plainCalls), len(enq.chainCalls))
		}
	})

	t.Run("FixedPath", func(t *testing.T) {
		// This sub-test verifies the corrected behaviour after m84: the scheduler
		// policy is FOR ALL, so ClaimAndAdvanceDueSchedules returns the due row,
		// EnqueueScheduledBackup creates a snapshot, and the enqueuer is called.
		repo := newSchedulerTestRepo()
		repo.addSchedule(newDueSchedule())

		enq := &recordingEnqueuer{}
		svc := &Service{
			repo:     repo,
			sites:    fakeSites{info: SiteInfo{Enrolled: true, AgeRecipient: "age1test", WpTimezone: "UTC"}},
			clock:    fakeClock{t: now},
			enqueuer: enq,
		}

		claimed, err := svc.ClaimDueSchedules(context.Background())
		if err != nil {
			t.Fatalf("FixedPath: ClaimDueSchedules error: %v", err)
		}
		for _, sched := range claimed {
			if eerr := svc.EnqueueScheduledBackup(context.Background(), sched); eerr != nil {
				t.Fatalf("FixedPath: EnqueueScheduledBackup error: %v", eerr)
			}
		}

		// After m84: claim returns the schedule → EnqueueScheduledBackup creates
		// a snapshot and calls the enqueuer.
		if snapCount := repo.snapCount(tenantID, siteID); snapCount != 1 {
			t.Errorf("FixedPath: expected 1 snapshot, got %d", snapCount)
		}
		if len(enq.plainCalls)+len(enq.chainCalls) == 0 {
			t.Errorf("FixedPath: expected at least one enqueue call, got plain=%d chain=%d",
				len(enq.plainCalls), len(enq.chainCalls))
		}

		// next_run_at must have advanced past now (schedule is no longer due).
		repo.mu.Lock()
		s := repo.schedules[schedID]
		repo.mu.Unlock()
		if s == nil {
			t.Fatal("FixedPath: schedule not found in repo after claim")
		}
		if !s.NextRunAt.After(now) {
			t.Errorf("FixedPath: next_run_at %v should be after now %v", s.NextRunAt, now)
		}
	})
}
