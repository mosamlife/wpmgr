package backup

// gh402_reclaim_guard_test.go: the guards that stand between a reclaim task
// and an irreversible object delete, plus the crash/give-up behaviour.
//
// Every test in here is about REFUSING to delete. That is deliberate: leaking
// an object costs money, and deleting a live one costs somebody their backups,
// so every ambiguous case must resolve to "leave it behind".

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// fakeReclaimStore: an in-memory ReclaimStore.
// ---------------------------------------------------------------------------

type fakeReclaimStore struct {
	tasks []ReclaimTask
	// siteExists reports what the raw sites table would say. The real query
	// deliberately does NOT filter archived sites, so an archived (LIVE) site
	// answers true here, which is the whole point of the archived test below.
	siteExists map[uuid.UUID]bool
	// tenantState is the three-way lifecycle answer. The zero value is
	// TenantLive, so a test only names the tenants it wants to be unusual.
	// TenantPendingPurge means skip; TenantGone means reclaim.
	tenantState map[uuid.UUID]TenantReclaimState

	completed map[uuid.UUID]bool
	cancelled map[uuid.UUID]string
	failures  map[uuid.UUID]int
	lastError map[uuid.UUID]string
	backoffs  map[uuid.UUID]time.Duration

	siteExistsErr error
	tenantErr     error
}

func newFakeReclaimStore() *fakeReclaimStore {
	return &fakeReclaimStore{
		siteExists:  map[uuid.UUID]bool{},
		tenantState: map[uuid.UUID]TenantReclaimState{},
		completed:   map[uuid.UUID]bool{},
		cancelled:   map[uuid.UUID]string{},
		failures:    map[uuid.UUID]int{},
		lastError:   map[uuid.UUID]string{},
		backoffs:    map[uuid.UUID]time.Duration{},
	}
}

func (f *fakeReclaimStore) ListDue(_ context.Context, maxAttempts, limit int32) ([]ReclaimTask, error) {
	var out []ReclaimTask
	for _, t := range f.tasks {
		if f.completed[t.ID] || f.cancelled[t.ID] != "" {
			continue
		}
		// The give-up cap: past it a task drops out of the due query but the
		// row is still present in f.tasks, which is what the give-up test
		// asserts on.
		if int32(f.failures[t.ID]) >= maxAttempts {
			continue
		}
		t.Attempts = int32(f.failures[t.ID])
		out = append(out, t)
		if int32(len(out)) >= limit {
			break
		}
	}
	return out, nil
}

func (f *fakeReclaimStore) SiteExists(_ context.Context, _, siteID uuid.UUID) (bool, error) {
	if f.siteExistsErr != nil {
		return false, f.siteExistsErr
	}
	return f.siteExists[siteID], nil
}

func (f *fakeReclaimStore) ListStuck(_ context.Context, maxAttempts, limit int32) ([]ReclaimTask, error) {
	var out []ReclaimTask
	for _, t := range f.tasks {
		if f.completed[t.ID] || f.cancelled[t.ID] != "" {
			continue
		}
		if int32(f.failures[t.ID]) < maxAttempts {
			continue
		}
		t.Attempts = int32(f.failures[t.ID])
		t.LastError = f.lastError[t.ID]
		out = append(out, t)
		if int32(len(out)) >= limit {
			break
		}
	}
	return out, nil
}

func (f *fakeReclaimStore) TenantState(_ context.Context, tenantID uuid.UUID) (TenantReclaimState, error) {
	if f.tenantErr != nil {
		return TenantLive, f.tenantErr
	}
	return f.tenantState[tenantID], nil
}

func (f *fakeReclaimStore) Complete(_ context.Context, id uuid.UUID) error {
	f.completed[id] = true
	return nil
}

func (f *fakeReclaimStore) Cancel(_ context.Context, id uuid.UUID, reason string) error {
	f.cancelled[id] = reason
	return nil
}

func (f *fakeReclaimStore) Fail(_ context.Context, id uuid.UUID, backoff time.Duration, reason string) error {
	f.failures[id]++
	f.lastError[id] = reason
	f.backoffs[id] = backoff
	return nil
}

// find returns the task row as it still stands in the table.
func (f *fakeReclaimStore) find(id uuid.UUID) (ReclaimTask, bool) {
	for _, t := range f.tasks {
		if t.ID == id {
			return t, true
		}
	}
	return ReclaimTask{}, false
}

func newReclaimTask(tenantID, siteID uuid.UUID) ReclaimTask {
	return ReclaimTask{
		ID: uuid.New(), TenantID: tenantID, SiteID: siteID,
		Kind: ReclaimKindBackupManifest, DestinationKind: "cp",
	}
}

// ---------------------------------------------------------------------------
// An ARCHIVED site is LIVE and restorable. Its manifests must survive.
//
// This is the test that fails if anyone reaches for site.ListAllSiteIDs, which
// is the obvious-looking helper, is already used by the org purge worker for a
// different purpose, and filters connection_state <> 'archived'. Under that
// helper an archived site looks deleted and its backups' manifests get wiped.
// ---------------------------------------------------------------------------

func TestGH402_ReclaimWorker_ArchivedSiteIsNotOrphaned(t *testing.T) {
	tenantID := uuid.New()
	archived := uuid.New()

	store := newGCStore()
	key := manifestIndexKey(tenantID, archived, uuid.New())
	store.put(key)

	state := newFakeReclaimStore()
	task := newReclaimTask(tenantID, archived)
	state.tasks = []ReclaimTask{task}
	// The raw sites table still holds the archived row: archived is a
	// connection state, not a deletion.
	state.siteExists[archived] = true

	w := NewReclaimWorker(state, store, nil)
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work: %v", err)
	}

	if !store.has(key) {
		t.Fatal("an ARCHIVED (live, restorable) site's manifest was deleted")
	}
	if len(store.deleted) != 0 {
		t.Fatalf("expected zero deletes, got %d", len(store.deleted))
	}
	if state.cancelled[task.ID] == "" {
		t.Error("the task should have been cancelled, not left to retry forever")
	}
}

// ---------------------------------------------------------------------------
// The restored-dump / staging-pointed-at-production valve.
// ---------------------------------------------------------------------------

func TestGH402_ReclaimWorker_RefusesWhenSiteRowStillExists(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()

	store := newGCStore()
	live := manifestIndexKey(tenantID, siteID, uuid.New())
	store.put(live)

	state := newFakeReclaimStore()
	task := newReclaimTask(tenantID, siteID)
	state.tasks = []ReclaimTask{task}
	state.siteExists[siteID] = true

	w := NewReclaimWorker(state, store, nil)
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work: %v", err)
	}

	if !store.has(live) {
		t.Error("a live site's manifest was deleted")
	}
	if len(store.deleted) != 0 {
		t.Errorf("expected zero deletes, got %d", len(store.deleted))
	}
	if got := state.cancelled[task.ID]; got == "" {
		t.Error("expected the task to be cancelled with a reason")
	}
	if state.completed[task.ID] {
		t.Error("a refused task must not be recorded as completed work")
	}
}

// A guard that cannot be EVALUATED is not a guard. An error reading the site
// state must fail the task, never fall through to the delete.
func TestGH402_ReclaimWorker_GuardErrorDeletesNothing(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()

	store := newGCStore()
	key := manifestIndexKey(tenantID, siteID, uuid.New())
	store.put(key)

	state := newFakeReclaimStore()
	task := newReclaimTask(tenantID, siteID)
	state.tasks = []ReclaimTask{task}
	state.siteExistsErr = errors.New("simulated database failure")

	w := NewReclaimWorker(state, store, nil)
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work: %v", err)
	}

	if !store.has(key) {
		t.Error("an object was deleted while the site-existence guard was unevaluable")
	}
	if state.failures[task.ID] != 1 {
		t.Errorf("expected the attempt to be recorded as a failure, got %d", state.failures[task.ID])
	}
	if state.completed[task.ID] {
		t.Error("task marked complete despite an unevaluable guard")
	}
}

// A tenant that is itself soft-deleted belongs to the org purge worker, which
// owns the whole tenant/<id>/ root and locks in a different namespace. Skip
// rather than race it, and skip WITHOUT burning an attempt.
func TestGH402_ReclaimWorker_SkipsSoftDeletedTenant(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()

	store := newGCStore()
	key := manifestIndexKey(tenantID, siteID, uuid.New())
	store.put(key)

	state := newFakeReclaimStore()
	task := newReclaimTask(tenantID, siteID)
	state.tasks = []ReclaimTask{task}
	state.tenantState[tenantID] = TenantPendingPurge

	w := NewReclaimWorker(state, store, nil)
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work: %v", err)
	}

	if len(store.deleted) != 0 {
		t.Errorf("expected zero deletes while the org purge owns the root, got %d", len(store.deleted))
	}
	if state.completed[task.ID] || state.cancelled[task.ID] != "" {
		t.Error("a skipped task must stay open for a later tick")
	}
	if state.failures[task.ID] != 0 {
		t.Error("a skip must not burn a retry attempt")
	}
}

// A tenant that is GONE is the opposite case, and the distinction is the whole
// blocking finding of the second review round.
//
// admin_delete_empty_tenant (org delete Lane A, and the superadmin orphan
// cleanup) hard-deletes a tenant row and sweeps NO object storage. m113
// therefore carries no tenant foreign key, so the task survives that delete, and
// it is then the only thing in the database that names those objects. An earlier
// version of this worker read "no tenant row" as "already purged, skip", which
// would have left every one of those prefixes in the bucket forever: GH #402
// relocated one level up rather than fixed.
func TestGH402_ReclaimWorker_ReclaimsWhenTenantIsHardDeleted(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()

	store := newGCStore()
	key := manifestIndexKey(tenantID, siteID, uuid.New())
	store.put(key)
	// A neighbouring tenant's object, to prove the sweep stays inside its own
	// derived prefix even with no tenant row to check against.
	other := manifestIndexKey(uuid.New(), uuid.New(), uuid.New())
	store.put(other)

	state := newFakeReclaimStore()
	task := newReclaimTask(tenantID, siteID)
	state.tasks = []ReclaimTask{task}
	state.tenantState[tenantID] = TenantGone

	w := NewReclaimWorker(state, store, nil)
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work: %v", err)
	}

	if store.has(key) {
		t.Error("the manifest of a site whose TENANT was hard-deleted was not reclaimed. " +
			"Lane A (admin_delete_empty_tenant) frees no storage at all, so nothing else ever will")
	}
	if !store.has(other) {
		t.Error("the sweep reached outside its derived prefix")
	}
	if !state.completed[task.ID] {
		t.Error("the task did not close after a clean drain")
	}
	if state.failures[task.ID] != 0 {
		t.Errorf("expected no failed attempts, got %d", state.failures[task.ID])
	}
}

// Past the cap a task stops being retried but must not go quiet. reportStuck
// re-reads and re-logs it every tick, so a permanently stuck prefix stays
// visible for as long as it is stuck rather than producing one line, once.
func TestGH402_ReclaimWorker_StuckTasksAreReportedEveryTick(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()

	store := newGCStore()
	store.put(manifestIndexKey(tenantID, siteID, uuid.New()))
	store.failEvery = 1 // every delete fails, forever

	state := newFakeReclaimStore()
	task := newReclaimTask(tenantID, siteID)
	state.tasks = []ReclaimTask{task}

	var logged []string
	w := NewReclaimWorker(state, store, slog.New(slog.NewTextHandler(&recordingWriter{lines: &logged},
		&slog.HandlerOptions{Level: slog.LevelError})))

	for i := 0; i < reclaimMaxAttempts; i++ {
		if err := w.Work(context.Background(), nil); err != nil {
			t.Fatalf("Work tick %d: %v", i, err)
		}
	}
	// The task is now past the cap and out of the due query entirely.
	before := len(logged)
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("post-cap Work: %v", err)
	}
	after := len(logged)
	if after <= before {
		t.Fatal("a task past the attempt cap produced no log output at all on a later tick; " +
			"it is retained but invisible, which is how a permanently stuck prefix gets forgotten")
	}
	joined := strings.Join(logged[before:], "\n")
	if !strings.Contains(joined, siteID.String()) {
		t.Errorf("the stuck-task report does not name the site: %s", joined)
	}
	if !strings.Contains(joined, "tenant/"+tenantID.String()+"/site/"+siteID.String()+"/") {
		t.Errorf("the stuck-task report does not carry the exact storage prefix an operator needs: %s", joined)
	}
}

// recordingWriter captures slog output line by line.
type recordingWriter struct{ lines *[]string }

func (w *recordingWriter) Write(p []byte) (int, error) {
	*w.lines = append(*w.lines, string(p))
	return len(p), nil
}

// ---------------------------------------------------------------------------
// Crash resume. A storage fault partway through a prefix leaves the task
// incomplete; the next tick drains the remainder and closes it.
// ---------------------------------------------------------------------------

func TestGH402_ReclaimWorker_ResumesAfterPartialFailure(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()

	store := newGCStore()
	var keys []string
	for i := 0; i < 6; i++ {
		k := manifestIndexKey(tenantID, siteID, uuid.New())
		store.put(k)
		keys = append(keys, k)
	}
	// Fail the 3rd delete of the run.
	store.failEvery = 3

	state := newFakeReclaimStore()
	task := newReclaimTask(tenantID, siteID)
	state.tasks = []ReclaimTask{task}

	w := NewReclaimWorker(state, store, nil)
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("first Work: %v", err)
	}

	if state.completed[task.ID] {
		t.Fatal("a partially drained prefix must NOT be marked complete")
	}
	if state.failures[task.ID] != 1 {
		t.Fatalf("expected 1 recorded failure, got %d", state.failures[task.ID])
	}
	if state.lastError[task.ID] == "" {
		t.Error("expected the failure reason to be recorded on the task")
	}
	if state.backoffs[task.ID] <= 0 {
		t.Error("expected a positive retry backoff")
	}
	remaining := store.list("tenant/" + tenantID.String() + "/site/" + siteID.String() + "/")
	if len(remaining) == 0 {
		t.Fatal("the fault should have left keys behind for the next tick")
	}

	// Second tick: storage healthy again. The re-list is shorter and finishes.
	store.failEvery = 0
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("second Work: %v", err)
	}
	if !state.completed[task.ID] {
		t.Error("the resumed sweep should have completed the task")
	}
	if got := store.list("tenant/" + tenantID.String() + "/site/" + siteID.String() + "/"); len(got) != 0 {
		t.Errorf("prefix not drained after resume, %d keys remain: %v", len(got), got)
	}
	for _, k := range keys {
		if store.has(k) {
			t.Errorf("key never reclaimed: %s", k)
		}
	}
}

// ---------------------------------------------------------------------------
// GIVE UP WITHOUT FORGETTING. Past the retry cap the task stops being retried
// but the ROW REMAINS, because it is the last record that those objects exist.
// Deleting it on give-up would recreate the exact bug this feature fixes.
// ---------------------------------------------------------------------------

func TestGH402_ReclaimWorker_GiveUpLeavesTaskVisible(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()

	store := newGCStore()
	store.put(manifestIndexKey(tenantID, siteID, uuid.New()))
	store.failEvery = 1 // every delete fails, forever

	state := newFakeReclaimStore()
	task := newReclaimTask(tenantID, siteID)
	state.tasks = []ReclaimTask{task}

	w := NewReclaimWorker(state, store, nil)
	for i := 0; i < reclaimMaxAttempts+3; i++ {
		if err := w.Work(context.Background(), nil); err != nil {
			t.Fatalf("Work tick %d: %v", i, err)
		}
	}

	if got := state.failures[task.ID]; got != reclaimMaxAttempts {
		t.Errorf("expected exactly %d attempts before the task drops out of the due query, got %d",
			reclaimMaxAttempts, got)
	}
	if _, ok := state.find(task.ID); !ok {
		t.Fatal("the task row was deleted on give-up; it is the only record those objects exist")
	}
	if state.completed[task.ID] {
		t.Error("a task that never drained must not be recorded as completed")
	}
	if state.lastError[task.ID] == "" {
		t.Error("the give-up reason must be readable on the row")
	}
}

// ---------------------------------------------------------------------------
// AN UNKNOWN KIND IS A RETRYABLE FAILURE, NEVER A CANCEL.
//
// kind used to be free text and the worker used to CANCEL a task whose prefix it
// could not derive. Cancel closes the row, so the task left the due query and
// the stuck report at once and nothing ever mentioned it again, while the
// objects it named sat in the bucket with no other record anywhere.
//
// That is not a theoretical row. The m113 header and the CHANGELOG both tell an
// operator to hand a known-deleted site to the sweep with a hand-written INSERT,
// because it is the only route to the objects orphaned before m113 existed, and
// the reporting account has 90 of those. One mistyped kind in that statement
// stranded them permanently: GH #402 delivered by the remedy for GH #402.
//
// The database now refuses the bad insert outright (site_object_reclaim_kind_check,
// m113 for fresh databases and m115 for existing ones). This test covers what
// the constraint cannot: a row written before it existed. The rule the worker
// follows is that Cancel is reserved for a PROOF that there is nothing to
// reclaim, which is the live-site guard alone.
// ---------------------------------------------------------------------------

func TestGH402_ReclaimWorker_UnknownKindIsRetriedAndStaysVisible(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()

	store := newGCStore()
	key := manifestIndexKey(tenantID, siteID, uuid.New())
	store.put(key)

	state := newFakeReclaimStore()
	task := newReclaimTask(tenantID, siteID)
	task.Kind = "backup_manifests" // the plural: one keystroke
	state.tasks = []ReclaimTask{task}

	var logged []string
	w := NewReclaimWorker(state, store, slog.New(slog.NewTextHandler(&recordingWriter{lines: &logged},
		&slog.HandlerOptions{Level: slog.LevelError})))

	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work: %v", err)
	}

	// It must not have been closed. A closed task is invisible to every query the
	// worker has, so the objects would never be reclaimed and nobody would know.
	if state.cancelled[task.ID] != "" {
		t.Fatal("a task with an unknown kind was CANCELLED. That closes the row, which is the " +
			"only record naming those objects, so they are stranded permanently: GH #402 " +
			"reintroduced by the remedy for it")
	}
	if state.completed[task.ID] {
		t.Fatal("a task with an unknown kind was marked complete despite reclaiming nothing")
	}
	if state.failures[task.ID] != 1 {
		t.Errorf("expected the attempt to be recorded as a retryable failure, got %d",
			state.failures[task.ID])
	}
	if state.backoffs[task.ID] <= 0 {
		t.Error("expected a positive retry backoff")
	}
	// Nothing was deleted: the worker could not name a prefix, so it must not have
	// guessed one.
	if !store.has(key) {
		t.Error("an object was deleted for a task whose prefix could not be derived")
	}
	if len(store.deleted) != 0 {
		t.Errorf("expected zero deletes, got %d", len(store.deleted))
	}
	// The recorded reason has to be usable: the bad value, and how to fix it.
	reason := state.lastError[task.ID]
	if !strings.Contains(reason, "backup_manifests") {
		t.Errorf("the recorded reason does not quote the bad kind, so an operator cannot see the "+
			"typo: %q", reason)
	}
	if !strings.Contains(reason, "UPDATE site_object_reclaim") {
		t.Errorf("the recorded reason does not tell the operator how to correct the row: %q", reason)
	}

	// Past the cap it must keep showing up, every tick, with the bad value in it.
	for i := 0; i < reclaimMaxAttempts+2; i++ {
		if err := w.Work(context.Background(), nil); err != nil {
			t.Fatalf("Work tick %d: %v", i, err)
		}
	}
	if _, ok := state.find(task.ID); !ok {
		t.Fatal("the row was deleted; it is the only record those objects exist")
	}
	before := len(logged)
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("post-cap Work: %v", err)
	}
	joined := strings.Join(logged[before:], "\n")
	if joined == "" {
		t.Fatal("a stranded task went silent after the attempt cap")
	}
	if !strings.Contains(joined, siteID.String()) {
		t.Errorf("the stuck report does not name the site: %s", joined)
	}
	if !strings.Contains(joined, "backup_manifests") {
		t.Errorf("the stuck report does not carry the bad kind, which is the thing an operator "+
			"has to see in order to fix it: %s", joined)
	}
}

// Correcting the kind makes the task run and drain, so the failure above is
// genuinely recoverable rather than merely noisy.
func TestGH402_ReclaimWorker_CorrectingTheKindResumesTheTask(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()

	store := newGCStore()
	key := manifestIndexKey(tenantID, siteID, uuid.New())
	store.put(key)

	state := newFakeReclaimStore()
	task := newReclaimTask(tenantID, siteID)
	task.Kind = "backup_manifests"
	state.tasks = []ReclaimTask{task}

	w := NewReclaimWorker(state, store, nil)
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if !store.has(key) {
		t.Fatal("the object was deleted before the kind was corrected")
	}

	// The operator's UPDATE: fix the kind, reset the attempt counter.
	state.tasks[0].Kind = ReclaimKindBackupManifest
	state.failures[task.ID] = 0

	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work after correction: %v", err)
	}
	if store.has(key) {
		t.Error("the corrected task did not reclaim the object")
	}
	if !state.completed[task.ID] {
		t.Error("the corrected task did not close after a clean drain")
	}
}

// Backoff grows and is capped, so a permanently broken bucket does not spin.
func TestGH402_ReclaimBackoff_GrowsAndCaps(t *testing.T) {
	if got := reclaimBackoff(0); got != reclaimBaseBackoff {
		t.Errorf("attempt 0 backoff = %v, want %v", got, reclaimBaseBackoff)
	}
	if got := reclaimBackoff(1); got != 2*reclaimBaseBackoff {
		t.Errorf("attempt 1 backoff = %v, want %v", got, 2*reclaimBaseBackoff)
	}
	if got := reclaimBackoff(50); got != reclaimMaxBackoff {
		t.Errorf("attempt 50 backoff = %v, want the %v cap", got, reclaimMaxBackoff)
	}
}

// A nil object store (no object storage configured) must be a clean no-op that
// leaves the task OPEN. Closing it would throw away the only record that those
// objects exist, so if storage is configured later the work would be lost:
// the same bug, one layer up.
func TestGH402_ReclaimWorker_NoObjectStoreLeavesTaskOpen(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()

	state := newFakeReclaimStore()
	task := newReclaimTask(tenantID, siteID)
	state.tasks = []ReclaimTask{task}

	w := NewReclaimWorker(state, nil, nil)
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if state.completed[task.ID] {
		t.Error("task closed with no object storage configured; the work would be lost forever")
	}
	if state.cancelled[task.ID] != "" {
		t.Error("task cancelled with no object storage configured")
	}
	if state.failures[task.ID] != 0 {
		t.Error("a no-op sweep must not burn retry attempts")
	}
}
