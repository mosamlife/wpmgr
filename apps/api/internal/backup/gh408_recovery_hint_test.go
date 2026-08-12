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
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
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
//
// wantKind is the kind the message must name, and it is checked as part of the
// whole command rather than separately: the site and tenant engines print the
// SAME command with their OWN kind, and a message that named the other table's
// kind would be a command that reopens nothing.
func requireSupportedHint(t *testing.T, what, msg string, taskID uuid.UUID, wantKind string) {
	t.Helper()
	if msg == "" {
		t.Fatalf("%s produced no operator-facing message at all", what)
	}
	want := "wpmgr-cli reclaim retry --task " + taskID.String() + " --kind " + wantKind
	if !strings.Contains(msg, want) {
		t.Errorf("%s does not name the supported correction with the task id an operator has to "+
			"paste and this engine's own kind (%q).\ngot: %s", what, want, msg)
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
		requireSupportedHint(t, "the site worker's recorded failure reason",
			state.lastError[task.ID], task.ID, ReclaimKindBackupManifest)

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
			strings.Join(logged[before:], "\n"), task.ID, ReclaimKindBackupManifest)
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
		requireSupportedHint(t, "the tenant worker's recorded failure reason",
			state.lastError[task.ID], task.ID, TenantReclaimKindStorage)

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
			strings.Join(logged[before:], "\n"), task.ID, TenantReclaimKindStorage)
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
	for _, msg := range []string{
		ReclaimRetryCommand(id), ReclaimRetryHint(id),
		TenantReclaimRetryCommand(id), TenantReclaimRetryHint(id),
	} {
		if sql := runnableSQLIn(msg); sql != "" {
			t.Errorf("the guard fires on the supported hint (%q): %s", sql, msg)
		}
		if !strings.Contains(msg, "wpmgr-cli reclaim retry --task "+id.String()) {
			t.Errorf("the supported hint does not name the command and the task id: %s", msg)
		}
	}
}

// TestGH408_EveryOperatorHintComesFromOneConstructor is the guard on the claim
// ReclaimRetryCommand's doc comment makes.
//
// That comment used to say the advice was a function rather than a literal "so
// the three places an operator meets [it] cannot drift apart". All three parts
// of that were false when it was written: the guard-1 reason called the
// function, the every-tick stuck report built the string inline, and the tenant
// worker had a second function of its own. A comment asserting an invariant
// nothing enforces is worth less than no comment, because it stops the next
// reader checking. This is the enforcement.
//
// It reads the module's own source rather than comparing outputs, because
// identical strings from two constructors is exactly the state being ruled out:
// the two would agree today and diverge the first time one is edited. Test files
// are excluded, since they legitimately spell the command out to assert on it.
func TestGH408_EveryOperatorHintComesFromOneConstructor(t *testing.T) {
	root := gh408ModuleRoot(t)
	const literal = "wpmgr-cli reclaim retry"

	// The generated sqlc tree is excluded. It carries the doc comments from
	// db/query/*.sql verbatim, where the command is named as prose describing
	// what a query is for. That is documentation of the command, not a
	// construction of the string an operator copies out of a log, and it is
	// never hand-edited anyway.
	generated := filepath.Join(root, "internal", "db", "sqlc")

	sites := map[string]int{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path == generated {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if n := strings.Count(string(b), literal); n > 0 {
			rel, _ := filepath.Rel(root, path)
			sites[rel] = n
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the module: %v", err)
	}

	const want = "internal/backup/reclaim_ops.go"
	if len(sites) != 1 || sites[want] != 1 {
		t.Fatalf("the operator recovery command is constructed in %v.\n"+
			"It must be built in exactly one place (%s, reclaimRetryCommand) and reached from "+
			"everywhere else, or the site hint, the tenant hint and the stuck reports drift apart "+
			"and one of them sends an operator to a command that does not exist", sites, want)
	}
}

// gh408ModuleRoot walks up from the test's working directory to the directory
// holding go.mod.
func gh408ModuleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("working directory: %v", err)
	}
	for {
		if _, serr := os.Stat(filepath.Join(dir, "go.mod")); serr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above the test's working directory")
		}
		dir = parent
	}
}

// kindFlag pulls the --kind argument out of a printed command.
var kindFlag = regexp.MustCompile(`--kind\s+([a-z_]+)`)

// TestGH408_PrintedHintsAreAcceptedByRetry closes the loop between what the
// workers print and what the command they name will accept.
//
// `reclaim retry` validated --kind against the SITE kind set alone, so
// --kind tenant_storage was refused before the tenant table was ever consulted.
// It stayed latent only because the hints omitted --kind entirely and rode the
// default. Now that each engine's hint names its own kind, a hint the retry path
// refuses is a hint that cannot work, and this fails on it.
func TestGH408_PrintedHintsAreAcceptedByRetry(t *testing.T) {
	id := uuid.New()
	for _, c := range []struct {
		what           string
		msg            string
		wantKind       string
		wantTenantOnly bool
	}{
		{"the site worker's command", ReclaimRetryCommand(id), ReclaimKindBackupManifest, false},
		{"the site worker's advice sentence", ReclaimRetryHint(id), ReclaimKindBackupManifest, false},
		{"the tenant worker's command", TenantReclaimRetryCommand(id), TenantReclaimKindStorage, true},
		{"the tenant worker's advice sentence", TenantReclaimRetryHint(id), TenantReclaimKindStorage, true},
	} {
		m := kindFlag.FindStringSubmatch(c.msg)
		if m == nil {
			t.Errorf("%s prints no --kind at all, so it leans on a default that is only right for "+
				"one of the two engines: %s", c.what, c.msg)
			continue
		}
		if m[1] != c.wantKind {
			t.Errorf("%s names --kind %q, want %q: each engine's hint must correct its OWN table",
				c.what, m[1], c.wantKind)
		}
		engine, err := classifyRetryKind(m[1])
		if err != nil {
			t.Errorf("%s prints --kind %q, which `wpmgr-cli reclaim retry` REFUSES: %v.\n"+
				"The one command family that exists to dig an operator out of a hole must accept "+
				"the command it printed them", c.what, m[1], err)
			continue
		}
		if engine.tenantOnly != c.wantTenantOnly {
			t.Errorf("%s prints --kind %q, which routes to tenantOnly=%v, want %v",
				c.what, m[1], engine.tenantOnly, c.wantTenantOnly)
		}
	}
}

// TestGH408_RetryAcceptsBothEnginesAndNamesTheValidKinds covers the validation
// itself, including the message a mistyped kind gets.
func TestGH408_RetryAcceptsBothEnginesAndNamesTheValidKinds(t *testing.T) {
	// An empty --kind is the site default, and a tenant task still reopens under
	// it because the tenant table is consulted first.
	engine, err := classifyRetryKind("")
	if err != nil || engine.siteKind != ReclaimKindBackupManifest || engine.tenantOnly {
		t.Errorf("the default kind resolved to %+v (err=%v), want the site engine's %q",
			engine, err, ReclaimKindBackupManifest)
	}

	for _, k := range ReclaimKinds {
		engine, err := classifyRetryKind(k)
		if err != nil || engine.siteKind != k || engine.tenantOnly {
			t.Errorf("site kind %q resolved to %+v (err=%v), want the site engine", k, engine, err)
		}
	}
	for _, k := range TenantReclaimKinds {
		engine, err := classifyRetryKind(k)
		if err != nil {
			t.Errorf("tenant kind %q is REFUSED by `reclaim retry`: %v. That is the operator path "+
				"rejecting its own tenant kind", k, err)
			continue
		}
		if !engine.tenantOnly || engine.siteKind != "" {
			t.Errorf("tenant kind %q resolved to %+v, want the tenant engine alone: falling through "+
				"to the site table would aim a site UPDATE at it carrying a kind that table cannot hold",
				k, engine)
		}
	}

	// A genuinely invalid kind is refused, and the refusal names what is valid.
	// "backup_manifests" is the realistic typo: it is the plural, one keystroke,
	// and the very thing that strands a task in the first place.
	_, kerr := classifyRetryKind("backup_manifests")
	if kerr == nil {
		t.Fatal("an unknown kind was accepted; a wrong kind is what makes a task unreclaimable")
	}
	for _, want := range []string{"backup_manifests", ReclaimKindBackupManifest, TenantReclaimKindStorage} {
		if !strings.Contains(kerr.Error(), want) {
			t.Errorf("the refusal does not mention %q, so an operator cannot see what to type "+
				"instead: %v", want, kerr)
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

	// The org lifecycle lock. lockHeldBy stands in for another lifecycle
	// operation already holding a tenant's lock; locked and released count the
	// calls, so a test can assert the lock is both taken and given back.
	lockHeldBy map[uuid.UUID]bool
	lockErr    error
	locked     int
	released   int
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
		lockHeldBy:   map[uuid.UUID]bool{},
	}
}

func (f *fakeTenantReclaimStore) LockTenantLifecycle(_ context.Context, tenantID uuid.UUID) (func(), bool, error) {
	if f.lockErr != nil {
		return nil, false, f.lockErr
	}
	if f.lockHeldBy[tenantID] {
		return nil, false, nil
	}
	f.locked++
	f.lockHeldBy[tenantID] = true
	return func() {
		f.released++
		f.lockHeldBy[tenantID] = false
	}, true, nil
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
