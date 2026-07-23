package backup

// stall_watchdog_test.go — GH #279 CP-side two-tier progress watchdog +
// proof-of-life tests. All tests run inside the package (white-box) and use
// in-memory fakes; no database is required, matching the convention in
// incremental_service_test.go.

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// ---------------------------------------------------------------------------
// watchdogFakeRepo — composes the incremental_service_test.go fakeRepo
// (which already implements the full Repo interface, panicking on methods it
// doesn't need) and overrides only the snapshot lifecycle methods these tests
// exercise. GetSnapshot / CompleteSnapshot / RecordManifest are inherited
// unchanged from the embedded fakeRepo, and share its lock + snapshots map.
// ---------------------------------------------------------------------------

type watchdogFakeRepo struct {
	*fakeRepo
	// stalledFeed is what ListStalledRunningSnapshots reports on the next
	// call; tests set it directly rather than re-deriving soft/hard interval
	// math in the fake (the real interval computation lives in the SQL query
	// and is out of scope for this unit-level suite).
	stalledFeed []StalledSnapshot
}

func newWatchdogFakeRepo() *watchdogFakeRepo {
	return &watchdogFakeRepo{fakeRepo: newFakeRepo()}
}

func (r *watchdogFakeRepo) ExistingChunkHashes(_ context.Context, _ uuid.UUID, _ []string) (map[string]Chunk, error) {
	// No chunk is pre-stored, so PresignChunks presigns every hash offered.
	return map[string]Chunk{}, nil
}

func (r *watchdogFakeRepo) FailSnapshot(_ context.Context, _, snapshotID uuid.UUID, errMsg string) (Snapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.snapshots[snapshotID]
	if !ok {
		return Snapshot{}, domain.NotFound("backup_snapshot_not_found", "not found")
	}
	s.Status = StatusFailed
	s.Error = errMsg
	r.snapshots[snapshotID] = s
	return s, nil
}

// FailStalledSnapshot mirrors the real FailStalledBackupSnapshot query's
// guard exactly: only a still-'running' row is transitioned to 'failed'.
// Anything else (already completed/failed/cancelled/resumed) reports
// rowsAffected=0 with no error -- the TOCTOU-safe no-op the Fix 1 must-fix
// depends on.
func (r *watchdogFakeRepo) FailStalledSnapshot(_ context.Context, _, snapshotID uuid.UUID, errMsg string) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.snapshots[snapshotID]
	if !ok || s.Status != StatusRunning {
		return 0, nil
	}
	s.Status = StatusFailed
	s.Error = errMsg
	r.snapshots[snapshotID] = s
	return 1, nil
}

func (r *watchdogFakeRepo) UpdateSnapshotProgress(_ context.Context, _, snapshotID uuid.UUID, progress []byte) (Snapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.snapshots[snapshotID]
	if !ok {
		return Snapshot{}, domain.NotFound("backup_snapshot_not_found", "not found")
	}
	s.Progress = progress
	r.snapshots[snapshotID] = s
	return s, nil
}

func (r *watchdogFakeRepo) ListStalledRunningSnapshots(_ context.Context, _, _ time.Duration) ([]StalledSnapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]StalledSnapshot(nil), r.stalledFeed...), nil
}

// MarkSnapshotStalled mirrors the real query's guard exactly: only a running,
// not-yet-stalled row is stamped; anything else is a silent no-op.
func (r *watchdogFakeRepo) MarkSnapshotStalled(_ context.Context, _, snapshotID uuid.UUID) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.snapshots[snapshotID]
	if !ok || s.Status != StatusRunning || s.StalledAt != nil {
		return false, nil
	}
	now := time.Now().UTC()
	s.StalledAt = &now
	r.snapshots[snapshotID] = s
	return true, nil
}

// ClearSnapshotStalled mirrors the real query's guard exactly: only a
// running, currently-stalled row is cleared — the anti-resurrection
// predicate this whole feature depends on.
func (r *watchdogFakeRepo) ClearSnapshotStalled(_ context.Context, _, snapshotID uuid.UUID) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.snapshots[snapshotID]
	if !ok || s.Status != StatusRunning || s.StalledAt == nil {
		return false, nil
	}
	s.StalledAt = nil
	r.snapshots[snapshotID] = s
	return true, nil
}

// GetBackupSettings overrides the embedded fakeRepo's NotFound default so the
// Fix 1 notification-gating tests below can observe whether sendBackupEmail
// actually enqueues a notification. Every watchdog test that never wires a
// mailer (svc.SetMailer is never called) is unaffected: sendBackupEmail's
// first check is `s.mailer == nil`, which short-circuits before this method
// is ever reached.
func (r *watchdogFakeRepo) GetBackupSettings(_ context.Context, _, _ uuid.UUID) (SiteBackupSettings, error) {
	return SiteBackupSettings{
		NotifyOnCompletion: "on_failure",
		NotifyRecipients:   []string{"ops@example.test"},
	}, nil
}

// fakeMailer is a minimal BackupMailer test double that records every Enqueue
// call so a test can assert whether (and how many times) a notification
// fired.
type fakeMailer struct {
	mu    sync.Mutex
	calls []string // recorded templates, in call order
}

func (m *fakeMailer) Enqueue(_ context.Context, _ uuid.UUID, _ []string, template string, _ map[string]any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, template)
	return nil
}

func (m *fakeMailer) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

// mustGet returns the current in-memory snapshot row, failing the test if
// absent.
func (r *watchdogFakeRepo) mustGet(t *testing.T, id uuid.UUID) Snapshot {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.snapshots[id]
	if !ok {
		t.Fatalf("snapshot %s not found in fake repo", id)
	}
	return s
}

// newWatchdogTestService builds a Service wired to repo and hub, reusing the
// package-level fakeSiteLookup / fakePresigner / fakeClock test doubles
// (defined in restore_chain_test.go / incremental_service_test.go).
func newWatchdogTestService(repo *watchdogFakeRepo, hub *Hub) *Service {
	svc := NewService(repo, &fakeSiteLookup{}, nil, &fakePresigner{}, fakeClock{t: time.Now()}, Config{})
	svc.SetHub(hub)
	return svc
}

// drainEvents collects every event currently buffered on ch without blocking.
func drainEvents(ch <-chan BackupEvent) []BackupEvent {
	var out []BackupEvent
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
		default:
			return out
		}
	}
}

// ---------------------------------------------------------------------------
// Watchdog: two-tier Work() pass.
// ---------------------------------------------------------------------------

// TestProgressWatchdog_SoftStallKeepsRunning verifies the GH #279 fix at the
// worker level: a row past the soft threshold but not the hard one is
// stamped stalled_at, status stays 'running' (never failed), and exactly one
// 'stalled' SSE hint is published.
func TestProgressWatchdog_SoftStallKeepsRunning(t *testing.T) {
	repo := newWatchdogFakeRepo()
	tenantID, siteID, snapID := uuid.New(), uuid.New(), uuid.New()
	repo.setSnapshot(Snapshot{ID: snapID, TenantID: tenantID, SiteID: siteID, Status: StatusRunning})
	repo.stalledFeed = []StalledSnapshot{{ID: snapID, TenantID: tenantID, SiteID: siteID, Hard: false, StalledAt: nil}}

	hub := NewHub()
	svc := newWatchdogTestService(repo, hub)
	ch, unsub := hub.Subscribe(snapID)
	defer unsub()

	worker := NewProgressWatchdogWorker(svc, time.Minute, time.Hour, nil)
	if err := worker.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work: %v", err)
	}

	got := repo.mustGet(t, snapID)
	if got.Status != StatusRunning {
		t.Fatalf("status = %q, want running — a soft stall must never fail the run", got.Status)
	}
	if got.StalledAt == nil {
		t.Fatal("expected stalled_at to be stamped")
	}

	events := drainEvents(ch)
	if len(events) != 1 || events[0].Phase != "stalled" {
		t.Fatalf("events = %+v, want exactly one 'stalled' event", events)
	}
	if events[0].Status != StatusRunning {
		t.Fatalf("event status = %q, want running", events[0].Status)
	}
}

// TestProgressWatchdog_HardFailAfterDeadline verifies a row past the hard
// threshold is failed with the distinct stallTimeoutMsg reason.
func TestProgressWatchdog_HardFailAfterDeadline(t *testing.T) {
	repo := newWatchdogFakeRepo()
	tenantID, siteID, snapID := uuid.New(), uuid.New(), uuid.New()
	repo.setSnapshot(Snapshot{ID: snapID, TenantID: tenantID, SiteID: siteID, Status: StatusRunning})
	repo.stalledFeed = []StalledSnapshot{{ID: snapID, TenantID: tenantID, SiteID: siteID, Hard: true}}

	hub := NewHub()
	svc := newWatchdogTestService(repo, hub)
	ch, unsub := hub.Subscribe(snapID)
	defer unsub()

	worker := NewProgressWatchdogWorker(svc, time.Minute, time.Hour, nil)
	if err := worker.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work: %v", err)
	}

	got := repo.mustGet(t, snapID)
	if got.Status != StatusFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if got.Error != stallTimeoutMsg {
		t.Fatalf("error = %q, want %q", got.Error, stallTimeoutMsg)
	}

	events := drainEvents(ch)
	if len(events) != 1 || events[0].Phase != "failed" {
		t.Fatalf("events = %+v, want exactly one 'failed' event", events)
	}
}

// TestProgressWatchdog_HardFailSkipsAlreadyTerminalRow is the Fix 1 TOCTOU
// security regression lock: ListStalledRunningSnapshots commits its list in a
// SEPARATE transaction from the watchdog's per-row hard-fail, so between the
// two the row may have already completed (or been cancelled, or agent-
// failed, or resumed). The stalledFeed here still reports the row as Hard
// (the list was already committed before the race), but the row's actual
// current status is terminal by the time the hard-fail runs. The guarded
// FailStalledSnapshot query must report rowsAffected=0, and the watchdog must
// skip BOTH the 'failed' SSE publish and the failure notification — a blind
// UPDATE here would wrongly flip a completed backup to failed and fire a
// false alert.
func TestProgressWatchdog_HardFailSkipsAlreadyTerminalRow(t *testing.T) {
	repo := newWatchdogFakeRepo()
	tenantID, siteID, snapID := uuid.New(), uuid.New(), uuid.New()
	repo.setSnapshot(Snapshot{ID: snapID, TenantID: tenantID, SiteID: siteID, Status: StatusCompleted})
	repo.stalledFeed = []StalledSnapshot{{ID: snapID, TenantID: tenantID, SiteID: siteID, Hard: true}}

	mailer := &fakeMailer{}
	hub := NewHub()
	svc := newWatchdogTestService(repo, hub)
	svc.SetMailer(mailer)
	ch, unsub := hub.Subscribe(snapID)
	defer unsub()

	worker := NewProgressWatchdogWorker(svc, time.Minute, time.Hour, nil)
	if err := worker.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work: %v", err)
	}

	got := repo.mustGet(t, snapID)
	if got.Status != StatusCompleted {
		t.Fatalf("status = %q, want completed — the watchdog must never touch an already-terminal row", got.Status)
	}
	if got.Error != "" {
		t.Fatalf("error = %q, want empty — a completed row must not be overwritten with the stall reason", got.Error)
	}

	if events := drainEvents(ch); len(events) != 0 {
		t.Fatalf("events = %+v, want none — a no-op hard-fail must not publish 'failed'", events)
	}
	if n := mailer.callCount(); n != 0 {
		t.Fatalf("mailer calls = %d, want 0 — a no-op hard-fail must not send a failure notification", n)
	}
}

// TestProgressWatchdog_HardFailStillFailsGenuinelyRunningRow is the Fix 1
// non-regression lock: a row that IS still 'running' when the watchdog's
// hard-fail transaction executes must be transitioned exactly as before —
// one 'failed' SSE event and one failure notification.
func TestProgressWatchdog_HardFailStillFailsGenuinelyRunningRow(t *testing.T) {
	repo := newWatchdogFakeRepo()
	tenantID, siteID, snapID := uuid.New(), uuid.New(), uuid.New()
	repo.setSnapshot(Snapshot{ID: snapID, TenantID: tenantID, SiteID: siteID, Status: StatusRunning})
	repo.stalledFeed = []StalledSnapshot{{ID: snapID, TenantID: tenantID, SiteID: siteID, Hard: true}}

	mailer := &fakeMailer{}
	hub := NewHub()
	svc := newWatchdogTestService(repo, hub)
	svc.SetMailer(mailer)
	ch, unsub := hub.Subscribe(snapID)
	defer unsub()

	worker := NewProgressWatchdogWorker(svc, time.Minute, time.Hour, nil)
	if err := worker.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work: %v", err)
	}

	got := repo.mustGet(t, snapID)
	if got.Status != StatusFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if got.Error != stallTimeoutMsg {
		t.Fatalf("error = %q, want %q", got.Error, stallTimeoutMsg)
	}

	events := drainEvents(ch)
	if len(events) != 1 || events[0].Phase != "failed" {
		t.Fatalf("events = %+v, want exactly one 'failed' event", events)
	}
	if n := mailer.callCount(); n != 1 {
		t.Fatalf("mailer calls = %d, want exactly 1 — a genuine hard-fail transition must send the failure notification", n)
	}
}

// TestProgressWatchdog_SoftStallIdempotent verifies the repo-level guard
// (status='running' AND stalled_at IS NULL) makes a re-tick on an
// already-stalled row a silent no-op: the timestamp does not move and no
// duplicate 'stalled' event is published.
func TestProgressWatchdog_SoftStallIdempotent(t *testing.T) {
	repo := newWatchdogFakeRepo()
	tenantID, siteID, snapID := uuid.New(), uuid.New(), uuid.New()
	repo.setSnapshot(Snapshot{ID: snapID, TenantID: tenantID, SiteID: siteID, Status: StatusRunning})

	hub := NewHub()
	svc := newWatchdogTestService(repo, hub)
	ch, unsub := hub.Subscribe(snapID)
	defer unsub()

	if err := svc.MarkSnapshotStalled(context.Background(), tenantID, snapID); err != nil {
		t.Fatalf("first MarkSnapshotStalled: %v", err)
	}
	first := repo.mustGet(t, snapID).StalledAt
	if first == nil {
		t.Fatal("expected stalled_at to be set")
	}

	if err := svc.MarkSnapshotStalled(context.Background(), tenantID, snapID); err != nil {
		t.Fatalf("second MarkSnapshotStalled: %v", err)
	}
	second := repo.mustGet(t, snapID).StalledAt
	if second == nil || !second.Equal(*first) {
		t.Fatalf("stalled_at changed on re-tick: first=%v second=%v", first, second)
	}

	stalledCount := 0
	for _, ev := range drainEvents(ch) {
		if ev.Phase == "stalled" {
			stalledCount++
		}
	}
	if stalledCount != 1 {
		t.Fatalf("stalled events = %d, want exactly 1 — the idempotent guard must suppress the second publish", stalledCount)
	}
}

// ---------------------------------------------------------------------------
// Proof of life: presign / manifest / progress all clear a soft stall.
// ---------------------------------------------------------------------------

// TestPresignChunks_AfterSoftStall_Succeeds is the GH #279 regression lock: a
// running-but-soft-stalled snapshot must still accept a presign (200, not
// 422), and the presign must clear the stall and publish 'resumed'.
func TestPresignChunks_AfterSoftStall_Succeeds(t *testing.T) {
	repo := newWatchdogFakeRepo()
	tenantID, siteID, snapID := uuid.New(), uuid.New(), uuid.New()
	stalledAt := time.Now().Add(-4 * time.Minute)
	repo.setSnapshot(Snapshot{ID: snapID, TenantID: tenantID, SiteID: siteID, Status: StatusRunning, StalledAt: &stalledAt})

	hub := NewHub()
	svc := newWatchdogTestService(repo, hub)
	ch, unsub := hub.Subscribe(snapID)
	defer unsub()

	uploads, err := svc.PresignChunks(context.Background(), tenantID, snapID, []string{"deadbeef"})
	if err != nil {
		t.Fatalf("PresignChunks: %v (a soft-stalled-but-alive snapshot must still accept work)", err)
	}
	if len(uploads) != 1 {
		t.Fatalf("uploads = %v, want 1 presigned URL", uploads)
	}

	got := repo.mustGet(t, snapID)
	if got.StalledAt != nil {
		t.Fatal("expected stalled_at to be cleared by the proof-of-life presign")
	}

	events := drainEvents(ch)
	if len(events) != 1 || events[0].Phase != "resumed" {
		t.Fatalf("events = %+v, want exactly one 'resumed' event", events)
	}
	if events[0].Status != StatusRunning {
		t.Fatalf("resumed event status = %q, want running", events[0].Status)
	}
}

// TestRecordProgress_ClearsStalled verifies a progress POST clears a soft
// stall and publishes 'resumed' BEFORE the regular progress event.
func TestRecordProgress_ClearsStalled(t *testing.T) {
	repo := newWatchdogFakeRepo()
	tenantID, siteID, snapID := uuid.New(), uuid.New(), uuid.New()
	stalledAt := time.Now().Add(-4 * time.Minute)
	repo.setSnapshot(Snapshot{ID: snapID, TenantID: tenantID, SiteID: siteID, Status: StatusRunning, StalledAt: &stalledAt})

	hub := NewHub()
	svc := newWatchdogTestService(repo, hub)
	ch, unsub := hub.Subscribe(snapID)
	defer unsub()

	if _, err := svc.RecordProgress(context.Background(), tenantID, snapID, "archiving_files", map[string]any{"files_done": 10}); err != nil {
		t.Fatalf("RecordProgress: %v", err)
	}

	got := repo.mustGet(t, snapID)
	if got.StalledAt != nil {
		t.Fatal("expected stalled_at to be cleared by the proof-of-life progress POST")
	}

	events := drainEvents(ch)
	if len(events) != 2 {
		t.Fatalf("events = %+v, want 2 (resumed + progress)", events)
	}
	if events[0].Phase != "resumed" {
		t.Fatalf("events[0].Phase = %q, want resumed", events[0].Phase)
	}
	if events[1].Phase != "archiving_files" {
		t.Fatalf("events[1].Phase = %q, want archiving_files", events[1].Phase)
	}
}

// TestRecordProgress_FailedPhaseEmptyReason_NeverResumes is the Fix 2
// correctness regression lock: a phase=="failed" progress POST with an empty
// reason skips the agentFailReason fast-fail branch (reason == "") and falls
// through the rest of RecordProgress — that fallthrough must NEVER clear the
// stall or publish 'resumed', because the agent is reporting a failure, not
// a resume. Pre-fix this wrongly cleared stalled_at and published 'resumed'
// for a run the agent itself says has failed.
func TestRecordProgress_FailedPhaseEmptyReason_NeverResumes(t *testing.T) {
	repo := newWatchdogFakeRepo()
	tenantID, siteID, snapID := uuid.New(), uuid.New(), uuid.New()
	stalledAt := time.Now().Add(-4 * time.Minute)
	repo.setSnapshot(Snapshot{ID: snapID, TenantID: tenantID, SiteID: siteID, Status: StatusRunning, StalledAt: &stalledAt})

	hub := NewHub()
	svc := newWatchdogTestService(repo, hub)
	ch, unsub := hub.Subscribe(snapID)
	defer unsub()

	// phase_detail carries neither "message" nor "error" — agentFailReason
	// returns "" and the fast-fail FailSnapshot branch is skipped entirely.
	if _, err := svc.RecordProgress(context.Background(), tenantID, snapID, "failed", map[string]any{}); err != nil {
		t.Fatalf("RecordProgress: %v", err)
	}

	got := repo.mustGet(t, snapID)
	if got.StalledAt == nil {
		t.Fatal("expected stalled_at to remain set — a 'failed' phase POST must never clear the stall")
	}
	if got.Status != StatusRunning {
		t.Fatalf("status = %q, want running — an empty-reason 'failed' phase with no fast-fail must not change status either", got.Status)
	}

	for _, ev := range drainEvents(ch) {
		if ev.Phase == "resumed" {
			t.Fatalf("events contain a 'resumed' event: %+v — a 'failed' phase POST must never publish resumed", ev)
		}
	}
}

// TestSubmitManifest_ClearsStalled verifies a manifest submission clears a
// soft stall (in addition to completing the snapshot) and publishes
// 'resumed'.
func TestSubmitManifest_ClearsStalled(t *testing.T) {
	repo := newWatchdogFakeRepo()
	tenantID, siteID, snapID := uuid.New(), uuid.New(), uuid.New()
	stalledAt := time.Now().Add(-4 * time.Minute)
	repo.setSnapshot(Snapshot{ID: snapID, TenantID: tenantID, SiteID: siteID, Status: StatusRunning, StalledAt: &stalledAt})

	hub := NewHub()
	svc := newWatchdogTestService(repo, hub)
	ch, unsub := hub.Subscribe(snapID)
	defer unsub()

	if _, _, err := svc.SubmitManifest(context.Background(), tenantID, snapID, agentcmd.SubmitManifestRequest{}); err != nil {
		t.Fatalf("SubmitManifest: %v", err)
	}

	foundResumed := false
	for _, ev := range drainEvents(ch) {
		if ev.Phase == "resumed" {
			foundResumed = true
		}
	}
	if !foundResumed {
		t.Fatal("expected a 'resumed' event from the proof-of-life clear")
	}
}

// ---------------------------------------------------------------------------
// A genuinely terminal (failed/cancelled) snapshot is never revived.
// ---------------------------------------------------------------------------

// TestPresignChunks_FailedSnapshot_Still422 verifies the pre-existing
// terminal-status guard still rejects a presign for a failed snapshot with
// 422 — the proof-of-life clear must never run (or matter) for a snapshot
// the watchdog already hard-failed.
func TestPresignChunks_FailedSnapshot_Still422(t *testing.T) {
	repo := newWatchdogFakeRepo()
	tenantID, siteID, snapID := uuid.New(), uuid.New(), uuid.New()
	repo.setSnapshot(Snapshot{ID: snapID, TenantID: tenantID, SiteID: siteID, Status: StatusFailed, Error: stallTimeoutMsg})

	svc := newWatchdogTestService(repo, NewHub())

	_, err := svc.PresignChunks(context.Background(), tenantID, snapID, []string{"deadbeef"})
	if err == nil {
		t.Fatal("expected an error for a terminal (failed) snapshot")
	}
	if got := domain.HTTPStatus(err); got != http.StatusUnprocessableEntity {
		t.Fatalf("HTTPStatus = %d, want 422", got)
	}
	de, ok := domain.AsDomain(err)
	if !ok || de.Code != "snapshot_not_in_progress" {
		t.Fatalf("error = %v, want code snapshot_not_in_progress", err)
	}
}

// TestSubmitManifest_Cancelled_Still409 verifies the pre-existing cancel
// guard still rejects a manifest submit for an operator-cancelled snapshot
// with 409 — the anti-resurrection guarantee for the cancel path.
func TestSubmitManifest_Cancelled_Still409(t *testing.T) {
	repo := newWatchdogFakeRepo()
	tenantID, siteID, snapID := uuid.New(), uuid.New(), uuid.New()
	repo.setSnapshot(Snapshot{ID: snapID, TenantID: tenantID, SiteID: siteID, Status: StatusFailed, Error: cancelByOperatorMsg})

	svc := newWatchdogTestService(repo, NewHub())

	_, _, err := svc.SubmitManifest(context.Background(), tenantID, snapID, agentcmd.SubmitManifestRequest{})
	if err == nil {
		t.Fatal("expected an error for a cancelled snapshot")
	}
	if got := domain.HTTPStatus(err); got != http.StatusConflict {
		t.Fatalf("HTTPStatus = %d, want 409", got)
	}
	de, ok := domain.AsDomain(err)
	if !ok || de.Code != "snapshot_canceled" {
		t.Fatalf("error = %v, want code snapshot_canceled", err)
	}
}

// TestWatchdogStallTimeoutMsgDistinctFromCancel locks in that a watchdog
// hard-fail and an operator cancel stamp different error strings — the only
// way ops/notifications/the UI can tell them apart (there is no separate
// status enum value for either).
func TestWatchdogStallTimeoutMsgDistinctFromCancel(t *testing.T) {
	if stallTimeoutMsg == cancelByOperatorMsg {
		t.Fatal("stallTimeoutMsg must be distinct from cancelByOperatorMsg")
	}
	if stallTimeoutMsg == "" {
		t.Fatal("stallTimeoutMsg must not be empty")
	}
}
