package backup

// gh408_recovery_hint_test.go, GH #408 finding 3: what the reclaim workers hand
// an operator has to WORK on the connection that operator has.
//
// The site reclaim worker used to write a bare UPDATE into last_error and print
// it again in the every-tick stuck report. Run verbatim as wpmgr_app
// (NOSUPERUSER, NOBYPASSRLS) with no GUC set, that statement is err=nil, rows=0,
// and the row is byte-for-byte unchanged: the RLS USING clause hides it, so
// Postgres has nothing to complain about. The measured proof of that is in
// apps/api/tests/gh408_operator_role_integration_test.go, which needs a
// container. THIS file is the containerless half that runs in the CI lane every
// pull request pays for, and it is the test reclaim_worker.go names.
//
// It asserts two things about every operator-facing string both workers produce:
// the supported command is in it, with the task id an operator has to paste, and
// there is no pastable SQL statement in it at all. The second is the load-bearing
// one. A caveat next to a statement that cannot work was the shipped state, and
// it is why a longer caveat would have been a different fix from the right one.

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// runnableSQL matches a statement an operator could paste, aimed at either
// reclaim table. The old message was
//
//	UPDATE site_object_reclaim SET kind = '...', attempts = 0 WHERE id = '...'
//
// so the verbs are matched against the table names rather than on their own:
// these messages legitimately mention backup_chunks and sites in prose, and a
// guard that fails correct work gets switched off.
var runnableSQL = regexp.MustCompile(
	`(?is)\b(?:UPDATE|INSERT\s+INTO|DELETE\s+FROM|ALTER\s+TABLE)\s+"?(?:public"?\s*\.\s*"?)?(?:site|tenant)_object_reclaim\b`)

// pastableWhereClause is the backstop for a statement aimed at some other table,
// or one rewritten past the matcher above. Nothing that is genuinely advice
// carries a WHERE clause.
var pastableWhereClause = regexp.MustCompile(`(?is)\bWHERE\s+id\s*=`)

// runnableSQLIn returns the offending fragment, or "" when the string is free of
// pastable SQL.
func runnableSQLIn(s string) string {
	if m := runnableSQL.FindString(s); m != "" {
		return m
	}
	return pastableWhereClause.FindString(s)
}

// requireSupportedHint is the assertion pair, applied to one string.
func requireSupportedHint(t *testing.T, what, msg string, taskID uuid.UUID) {
	t.Helper()
	if msg == "" {
		t.Fatalf("%s produced no operator-facing message at all", what)
	}
	want := "wpmgr-cli reclaim retry --task " + taskID.String()
	if !strings.Contains(msg, want) {
		t.Errorf("%s does not name the supported correction with the task id an operator has to "+
			"paste (%q).\ngot: %s", what, want, msg)
	}
	if sql := runnableSQLIn(msg); sql != "" {
		t.Errorf("%s hands the operator SQL (%q). As the application role, RLS hides that row: the "+
			"statement reports success having changed nothing, which is the defect GH #408 finding 3 "+
			"removed.\ngot: %s", what, sql, msg)
	}
}

// TestGH408_WorkerPrintsNoRunnableSQL is the test reclaim_worker.go's guard-1
// comment names. It covers the site worker's failure reason and its stuck
// report, and the tenant worker's equivalents, because the string is written in
// four places and only one of them was ever the reported one.
func TestGH408_WorkerPrintsNoRunnableSQL(t *testing.T) {
	t.Run("site worker, failure reason and stuck report", func(t *testing.T) {
		tenantID, siteID := uuid.New(), uuid.New()
		store := newGCStore()
		store.put(manifestIndexKey(tenantID, siteID, uuid.New()))

		state := newFakeReclaimStore()
		task := newReclaimTask(tenantID, siteID)
		task.Kind = "backup_manifests" // the plural: one keystroke, and unreclaimable
		state.tasks = []ReclaimTask{task}

		var logged []string
		w := NewReclaimWorker(state, store, slog.New(slog.NewTextHandler(
			&recordingWriter{lines: &logged}, &slog.HandlerOptions{Level: slog.LevelError})))

		if err := w.Work(context.Background(), nil); err != nil {
			t.Fatalf("Work: %v", err)
		}
		requireSupportedHint(t, "the site worker's recorded failure reason", state.lastError[task.ID], task.ID)

		// Past the cap the row stops being retried and starts being re-reported
		// every tick. Those lines are what an operator actually alerts on.
		for i := 0; i < reclaimMaxAttempts+1; i++ {
			if err := w.Work(context.Background(), nil); err != nil {
				t.Fatalf("Work tick %d: %v", i, err)
			}
		}
		before := len(logged)
		if err := w.Work(context.Background(), nil); err != nil {
			t.Fatalf("post-cap Work: %v", err)
		}
		requireSupportedHint(t, "the site worker's stuck report",
			strings.Join(logged[before:], "\n"), task.ID)
	})

	t.Run("tenant worker, failure reason and stuck report", func(t *testing.T) {
		tenantID := uuid.New()
		state := newFakeTenantReclaimStore()
		task := TenantReclaimTask{ID: uuid.New(), TenantID: tenantID, Kind: "tenant_storages"}
		state.tasks = []TenantReclaimTask{task}

		var logged []string
		w := NewTenantReclaimWorker(state, newGCStore(), slog.New(slog.NewTextHandler(
			&recordingWriter{lines: &logged}, &slog.HandlerOptions{Level: slog.LevelError})))

		if err := w.Work(context.Background(), nil); err != nil {
			t.Fatalf("Work: %v", err)
		}
		requireSupportedHint(t, "the tenant worker's recorded failure reason", state.lastError[task.ID], task.ID)

		for i := 0; i < tenantReclaimMaxAttempts+1; i++ {
			if err := w.Work(context.Background(), nil); err != nil {
				t.Fatalf("Work tick %d: %v", i, err)
			}
		}
		before := len(logged)
		if err := w.Work(context.Background(), nil); err != nil {
			t.Fatalf("post-cap Work: %v", err)
		}
		requireSupportedHint(t, "the tenant worker's stuck report",
			strings.Join(logged[before:], "\n"), task.ID)
	})
}

// TestGH408_RecoveryHintGuardSeesThePreviousMessage is the other half of a guard
// worth having: it must fire on the string that shipped, and it must not fire on
// the one that replaced it. Without this the matchers could be quietly wrong in
// either direction and every assertion above would still be green.
func TestGH408_RecoveryHintGuardSeesThePreviousMessage(t *testing.T) {
	id := uuid.New()

	// The message as it shipped with GH #402, reconstructed from the migration
	// header and the worker's own git history.
	shipped := "site object reclaim: unknown kind (the task is kept and retried. Correct the row " +
		"and it resumes, ON A SUPERUSER CONNECTION: UPDATE site_object_reclaim SET kind = " +
		"'backup_manifest', attempts = 0, next_attempt_at = now() WHERE id = '" + id.String() + "')"
	if runnableSQLIn(shipped) == "" {
		t.Error("the guard does not recognise the very statement GH #408 finding 3 is about, so it " +
			"could go back in with this test green")
	}

	// And the replacements must pass it, or the guard fails correct work.
	for _, msg := range []string{ReclaimRetryHint(id), TenantReclaimRetryHint(id)} {
		if sql := runnableSQLIn(msg); sql != "" {
			t.Errorf("the guard fires on the supported hint (%q): %s", sql, msg)
		}
		if !strings.Contains(msg, "wpmgr-cli reclaim retry --task "+id.String()) {
			t.Errorf("the supported hint does not name the command and the task id: %s", msg)
		}
	}
}

// ---------------------------------------------------------------------------
// fakeTenantReclaimStore: an in-memory TenantReclaimStore.
//
// The tenant drain holds the ONLY chunk-delete authority in this codebase, and
// until now every proof it had needed a container and so ran in no lane at all.
// This fake is deliberately minimal: it exists for the message assertions above.
// The guards themselves are proved against real Postgres in
// apps/api/tests/gh408_tenant_object_reclaim_integration_test.go.
// ---------------------------------------------------------------------------

type fakeTenantReclaimStore struct {
	tasks []TenantReclaimTask

	tenantExists map[uuid.UUID]bool
	sitesExist   map[uuid.UUID]bool
	chunksExist  map[uuid.UUID]bool

	completed map[uuid.UUID]bool
	failures  map[uuid.UUID]int
	lastError map[uuid.UUID]string
	backoffs  map[uuid.UUID]time.Duration
}

func newFakeTenantReclaimStore() *fakeTenantReclaimStore {
	return &fakeTenantReclaimStore{
		tenantExists: map[uuid.UUID]bool{},
		sitesExist:   map[uuid.UUID]bool{},
		chunksExist:  map[uuid.UUID]bool{},
		completed:    map[uuid.UUID]bool{},
		failures:     map[uuid.UUID]int{},
		lastError:    map[uuid.UUID]string{},
		backoffs:     map[uuid.UUID]time.Duration{},
	}
}

func (f *fakeTenantReclaimStore) ListDue(_ context.Context, maxAttempts, limit int32) ([]TenantReclaimTask, error) {
	var out []TenantReclaimTask
	for _, t := range f.tasks {
		if f.completed[t.ID] || int32(f.failures[t.ID]) >= maxAttempts {
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

func (f *fakeTenantReclaimStore) ListStuck(_ context.Context, maxAttempts, limit int32) ([]TenantReclaimTask, error) {
	var out []TenantReclaimTask
	for _, t := range f.tasks {
		if f.completed[t.ID] || int32(f.failures[t.ID]) < maxAttempts {
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

func (f *fakeTenantReclaimStore) TenantExists(_ context.Context, tenantID uuid.UUID) (bool, error) {
	return f.tenantExists[tenantID], nil
}

func (f *fakeTenantReclaimStore) SitesExist(_ context.Context, tenantID uuid.UUID) (bool, error) {
	return f.sitesExist[tenantID], nil
}

func (f *fakeTenantReclaimStore) ChunkRowsExist(_ context.Context, tenantID uuid.UUID) (bool, error) {
	return f.chunksExist[tenantID], nil
}

func (f *fakeTenantReclaimStore) Complete(_ context.Context, id uuid.UUID) error {
	f.completed[id] = true
	return nil
}

func (f *fakeTenantReclaimStore) Fail(_ context.Context, id uuid.UUID, backoff time.Duration, reason string) error {
	f.failures[id]++
	f.lastError[id] = reason
	f.backoffs[id] = backoff
	return nil
}
