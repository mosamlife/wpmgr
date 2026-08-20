// halted_run_contract_integration_test.go — GH #482.
//
// A HALTED RUN MUST NEVER READ 'completed'. This file is the execution of that
// sentence, and it has to reach real Postgres to be worth anything: the
// guarantee lives in SetUpdateRunStatus's precondition (db/query/updates.sql),
// so every fake-repo test in this package would pass with the guard deleted.
//
// It also has to pick the RIGHT HALT. A halt the wave gate can re-derive from
// the task rows was already compensated for in Go — haltLocked re-asserts
// 'halted' after every agent-task terminal transition — so a test built on one
// would go green with no precondition at all and prove nothing. The halts with
// NO compensation are the ones DeriveAgentWaveState cannot see: the kill switch
// and a withdrawn release manifest, both routed through
// Worker.haltAgentRunWith. Those runs stayed 'completed' permanently, telling an
// operator a rollout they killed had finished normally. That is the fixture
// below.
//
// The run is driven through the REAL Worker methods (haltAgentRunWith, then
// finishAgentTask, which is finish plus the gate re-judgement) on a repo built
// over a pool that connects as the non-superuser wpmgr_app role, so nothing
// here can pass by going around the code path or the policies that production
// uses.
package update

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

// setupFatalf, setupSkipf, skipIfDockerUnavailable and
// setupFatalfOrSkipIfDaemonDied are local copies of the four helpers commit
// 54ad8ae4 (#503) standardised across the tests package. They are duplicated
// rather than imported because those live in package tests, which an internal
// package cannot import.
//
// What all four buy: A SKIPPED PROOF STILL PRINTS ok. This file is the only
// executed proof that a halted run cannot read 'completed', so a container that
// fails to start for any reason OTHER than "there is no reachable Docker on
// this machine" must redden the package, never disappear into a SKIP line under
// an ok. Only a positive health probe may resolve to a skip; "we could not
// tell" resolves to fatal.
func setupFatalf(t *testing.T, err error, stage string) {
	t.Helper()
	t.Fatalf("SETUP FAILURE (infrastructure, not the test's own assertion) at stage=%q: %v", stage, err)
}

// setupSkipf is setupFatalf's skip counterpart, reserved for the single case of
// "Docker is not available on this machine at all" — the one setup failure that
// is not a mid-run flake and that every container-backed test would hit
// identically, so skipping is the honest signal rather than a hidden pass.
func setupSkipf(t *testing.T, err error, stage string) {
	t.Helper()
	t.Skipf("SETUP SKIP (infrastructure, not the test's own assertion) at stage=%q: %v", stage, err)
}

// skipIfDockerUnavailable positively probes the provider testcontainers will
// use, BEFORE any container is asked to start, rather than inferring Docker's
// absence from whatever error a later start call happens to return.
func skipIfDockerUnavailable(t *testing.T, ctx context.Context, stage string) {
	t.Helper()
	provider, err := testcontainers.ProviderDocker.GetProvider()
	if err != nil {
		setupSkipf(t, err, stage+" (docker provider unavailable)")
		return
	}
	if err := provider.Health(ctx); err != nil {
		setupSkipf(t, err, stage+" (docker daemon unreachable)")
	}
}

// setupFatalfOrSkipIfDaemonDied handles an error from the container-START call
// itself. skipIfDockerUnavailable has already proved the daemon was reachable
// immediately beforehand, so an error here is ordinarily a real setup failure
// with Docker perfectly healthy — a bad image tag, no space left, a registry
// pull failure — and that MUST stay fatal.
//
// The one case that should still skip is the daemon dying inside the window
// this container's own startup spans. That is told apart with a SECOND positive
// Health() probe, never by pattern-matching the start error's text: an ordinary
// start failure re-probes healthy and falls straight through to setupFatalf.
func setupFatalfOrSkipIfDaemonDied(t *testing.T, ctx context.Context, startErr error, stage string) {
	t.Helper()
	provider, provErr := testcontainers.ProviderDocker.GetProvider()
	if provErr == nil {
		if healthErr := provider.Health(ctx); healthErr != nil {
			setupSkipf(t, fmt.Errorf("start error: %v; daemon health re-probe now fails: %w", startErr, healthErr),
				stage+" (docker daemon died mid-start)")
			return
		}
	}
	setupFatalf(t, startErr, stage)
}

// startHaltContractPostgres returns (appPool, adminPool). appPool connects as
// wpmgr_app — non-superuser, NOBYPASSRLS, the role every real install runs as —
// because the repo's InTenantTx wrappers are only meaningful against a role the
// policies actually apply to. adminPool is the superuser and seeds fixtures
// only. Mirrors internal/email/repo_site_scope_integration_test.go.
func startHaltContractPostgres(t *testing.T) (*db.Pool, *db.Pool) {
	t.Helper()
	ctx := context.Background()

	skipIfDockerUnavailable(t, ctx, "postgres")

	container, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("wpmgr"),
		tcpostgres.WithUsername("wpmgr"),
		tcpostgres.WithPassword("wpmgr"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(90*time.Second),
		),
	)
	// Registered BEFORE the error is handled: a start that fails partway can
	// still leave a container behind, and a cleanup registered after the check
	// is unreachable on exactly the paths that leak one.
	t.Cleanup(func() {
		if container != nil {
			_ = container.Terminate(context.Background())
		}
	})
	if err != nil {
		setupFatalfOrSkipIfDaemonDied(t, ctx, err, "postgres")
	}

	adminDSN, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		setupFatalf(t, err, "postgres connection string")
	}
	adminPool, err := db.Connect(ctx, adminDSN)
	if err != nil {
		setupFatalf(t, err, "connect admin")
	}
	if err := adminPool.Migrate(ctx); err != nil {
		setupFatalf(t, err, "migrate")
	}
	for _, stmt := range []string{
		"ALTER ROLE wpmgr_app LOGIN PASSWORD 'app'",
		"GRANT USAGE ON SCHEMA public TO wpmgr_app",
		"GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO wpmgr_app",
		"REVOKE UPDATE, DELETE, TRUNCATE ON audit_log FROM wpmgr_app",
	} {
		if _, err := adminPool.Exec(ctx, stmt); err != nil {
			setupFatalf(t, err, fmt.Sprintf("provision app role (%q)", stmt))
		}
	}
	t.Cleanup(adminPool.Close)

	appDSN := strings.Replace(adminDSN, "wpmgr:wpmgr@", "wpmgr_app:app@", 1)
	appPool, err := db.Connect(ctx, appDSN)
	if err != nil {
		setupFatalf(t, err, "connect app")
	}
	t.Cleanup(appPool.Close)
	return appPool, adminPool
}

func seedHaltContractTenant(t *testing.T, admin *db.Pool, slug string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := admin.QueryRow(context.Background(),
		"INSERT INTO tenants (name, slug) VALUES ($1, $1) RETURNING id", slug).Scan(&id); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	return id
}

func seedHaltContractSite(t *testing.T, admin *db.Pool, tenant uuid.UUID, url string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := admin.Exec(context.Background(),
		"INSERT INTO sites (id, tenant_id, url, name, status) VALUES ($1, $2, $3, $3, 'connected')",
		id, tenant, url); err != nil {
		t.Fatalf("seed site: %v", err)
	}
	return id
}

// readRunStatus reads ground truth through the same tenant wrapper the repo
// uses, so the read is subject to the same policies as the writes above it.
func readRunStatus(t *testing.T, pool *db.Pool, tenant, runID uuid.UUID) string {
	t.Helper()
	var s string
	if err := pool.InTenantTx(context.Background(), tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT status FROM update_runs WHERE id = $1 AND tenant_id = $2`, runID, tenant).Scan(&s)
	}); err != nil {
		t.Fatalf("read run status: %v", err)
	}
	return s
}

// haltContractWorker builds a Worker over the real repo with the agent wave
// gate wired to that same repo, which is what lets finishAgentTask actually
// re-judge the gate. Everything the finish path nil-guards (hub, audit,
// refresher, commander) is left nil: this test is about one run row.
func haltContractWorker(repo Repo) *Worker {
	w := NewWorker(repo, nil, nil, nil, nil, nil, nil, 5, 0)
	w.SetAgentSelfUpdate(AgentSelfUpdateDeps{Waves: repo.(AgentWaveRepo)})
	return w
}

// ---------------------------------------------------------------------------
// The regression
// ---------------------------------------------------------------------------

// TestKillSwitchHaltSurvivesTheLastRunningTaskFinishing is GH #482.
//
// THE SEQUENCE IT PINS:
//
//  1. An agent self-update rollout is killed by the kill switch. haltAgentRunWith
//     records the refused task as 'skipped' and halts the run. haltLocked
//     cancels every task nothing was sent for and DELIBERATELY LEAVES RUNNING
//     ONES ALONE — their command is already on the site, so the confirm poll
//     must survive to learn whether those sites upgraded or bricked.
//  2. That running task's confirmation resolves. finishAgentTask records it,
//     and Worker.finish runs its ordinary run-completion check.
//  3. CountUnfinishedTasks now returns 0 — the halt terminalized every sibling —
//     so the worker asks for 'completed' on a run that reads 'halted'.
//
// Before the precondition, step 3 won and the operator was told a rollout they
// killed finished normally, with commands out on real sites.
//
// RED WITHOUT THE GUARD: drop "AND (status = $3 OR status NOT IN ('halted',
// 'expired'))" from SetUpdateRunStatus in db/query/updates.sql, run sqlc
// generate, and the halted-run assertion reads "completed".
//
// The gate re-judgement in step 2 is NOT what saves this run, which is the
// whole reason the fixture looks the way it does: every wave confirms at least
// one site, so haltReasonFor finds nothing to halt on and DeriveAgentWaveState
// re-asserts nothing. That property is asserted, not assumed — see the Fatal on
// st.Halt below. Build this on a rollout that had NOT proved itself and the
// gate re-derives the halt for free, the Go compensation puts 'halted' back,
// and the test goes green against a database with no precondition at all.
func TestKillSwitchHaltSurvivesTheLastRunningTaskFinishing(t *testing.T) {
	app, admin := startHaltContractPostgres(t)
	ctx := context.Background()

	repo := NewRepo(app)
	w := haltContractWorker(repo)

	tenant := seedHaltContractTenant(t, admin, "gh482-halt")

	// SIX sites, because the fixture's whole value depends on the wave shape.
	// PlanWaves(6) is canary [0,1), pilot [1,4), rest [4,6) — a FINAL WAVE OF
	// TWO, which is what lets one task be in flight while another meets the
	// kill switch, inside the same wave, with every earlier wave already
	// confirmed. See the derivation assertion below for why that matters.
	seed := make([]NewTask, 0, 6)
	for i := 0; i < 6; i++ {
		seed = append(seed, NewTask{
			SiteID:     seedHaltContractSite(t, admin, tenant, fmt.Sprintf("https://s%d.gh482.example", i)),
			TargetType: TargetAgent, TargetSlug: AgentTargetSlug,
			DesiredVersion: "0.62.0", FromVersion: "0.61.80",
		})
	}

	run, tasks, err := repo.CreateRunWithTasks(ctx, CreateRunInput{TenantID: tenant}, seed)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if len(tasks) != 6 {
		t.Fatalf("seeded %d tasks, want 6", len(tasks))
	}

	// The rollout's own order, not insertion order: batch-inserted tasks share
	// a created_at, so waveOrder breaks the tie on id. Reading the plan from
	// the production functions keeps this fixture correct if the wave sizes are
	// ever retuned.
	order := waveOrder(tasks)
	waves := PlanWaves(len(order))
	last := waves[len(waves)-1]
	if last.End-last.Start < 2 {
		t.Fatalf("the final wave holds %d task(s); this fixture needs 2 (one in flight, one killed)", last.End-last.Start)
	}
	inFlight, killed := order[last.End-2], order[last.End-1]

	// Every wave before the last one CONFIRMS. This is what makes the halt
	// non-derivable: haltReasonFor halts any wave that confirmed no site, so a
	// rollout that had not proved itself would let the wave gate re-derive the
	// halt from the task rows and re-assert 'halted' all on its own — and the
	// test would pass with no SQL precondition at all.
	for i := 0; i < last.End-2; i++ {
		if _, err := repo.FinishTask(ctx, FinishTaskInput{
			TenantID: tenant, TaskID: order[i].ID, Status: TaskSucceeded,
			FromVersion: "0.61.80", ToVersion: "0.62.0", Detail: "confirmed",
		}); err != nil {
			t.Fatalf("confirm the earlier waves (task %d): %v", i, err)
		}
	}

	// The in-flight task's command is already out on its site.
	running, err := repo.MarkTaskRunning(ctx, tenant, inFlight.ID, 30*time.Minute)
	if err != nil {
		t.Fatalf("mark the in-flight task running: %v", err)
	}
	if running.Status != TaskRunning {
		t.Fatalf("in-flight task status = %q, want %q", running.Status, TaskRunning)
	}

	// 1. The kill switch fires on the other task. This is the halt flavour the
	//    wave gate cannot re-derive.
	if err := w.haltAgentRunWith(ctx, killed, TaskSkipped, killed.FromVersion, "", "refused: kill switch", "", "agent self-update disabled by the kill switch"); err != nil {
		t.Fatalf("halt the run: %v", err)
	}
	if got := readRunStatus(t, app, tenant, run.ID); got != RunHalted {
		t.Fatalf("run status after the halt = %q, want %q", got, RunHalted)
	}

	// The halt must NOT have cancelled the in-flight task: its command is on
	// the site and only its own confirmation may resolve it. If this breaks,
	// the rest of the test is testing nothing.
	stillRunning, err := repo.GetTask(ctx, tenant, inFlight.ID)
	if err != nil {
		t.Fatalf("re-read the in-flight task: %v", err)
	}
	if stillRunning.Status != TaskRunning {
		t.Fatalf("the halt terminalized the in-flight task (%q); it must be left to its own confirmation", stillRunning.Status)
	}

	// 2 & 3. The confirmation lands. This is the write that used to overwrite
	//        'halted' with 'completed'.
	if err := w.finishAgentTask(ctx, stillRunning, TaskSucceeded, "0.61.80", "0.62.0", "confirmed after the halt", ""); err != nil {
		t.Fatalf("finish the in-flight task: %v", err)
	}

	if n, err := repo.CountUnfinishedTasks(ctx, tenant, run.ID); err != nil || n != 0 {
		t.Fatalf("unfinished tasks = %d (err %v), want 0 — the completion check must have been reached for this test to mean anything", n, err)
	}

	// THE FIXTURE'S LOAD-BEARING PROPERTY, asserted rather than assumed: with
	// the rows in their final shape the wave gate derives NO halt, so nothing
	// in Go puts 'halted' back. finishAgentTask has just re-judged the gate and
	// found no reason to halt; whatever the run says next is what the database
	// refused, or allowed, on its own. If this ever flips, the assertions below
	// would pass even with the precondition deleted, and this Fatal is what
	// stops that going unnoticed.
	final, err := repo.ListTasks(ctx, tenant, run.ID)
	if err != nil {
		t.Fatalf("list the run's tasks: %v", err)
	}
	if st := DeriveAgentWaveState(final); st.Halt {
		t.Fatalf("the wave gate re-derives this halt (%q), so the Go compensation would mask a missing precondition; this fixture must use a halt it CANNOT derive", st.HaltReason)
	}

	if got := readRunStatus(t, app, tenant, run.ID); got != RunHalted {
		t.Errorf("run status = %q, want %q: a rollout stopped by the kill switch was reported to the operator as having finished normally (GH #482)", got, RunHalted)
	}

	// The status the operator's live view is told, not a guess. Before #482 this
	// published 'completed'; with the guard but no ErrRunNotOpen handling it
	// published 'running', which is equally untrue of a halted run.
	if got := w.maybeCompleteRun(ctx, tenant, run.ID); got != RunHalted {
		t.Errorf("published run status = %q, want %q", got, RunHalted)
	}

	// The refusal is a normal outcome and must be legible as one: the run comes
	// back UNCHANGED alongside ErrRunNotOpen, never domain.NotFound. A caller
	// that saw "update run not found" for a live row would log a false alarm on
	// every correct halt.
	back, err := repo.SetRunStatus(ctx, tenant, run.ID, RunCompleted)
	if !errors.Is(err, ErrRunNotOpen) {
		t.Fatalf("SetRunStatus on a halted run: err = %v, want ErrRunNotOpen", err)
	}
	if back.Status != RunHalted {
		t.Errorf("run returned alongside ErrRunNotOpen = %q, want the unchanged %q", back.Status, RunHalted)
	}

	// Idempotence: re-writing a terminal run's OWN status stays a no-op
	// success, because haltLocked re-asserts 'halted' on purpose and a bare
	// NOT IN would turn that honest caller into an error path.
	if _, err := repo.SetRunStatus(ctx, tenant, run.ID, RunHalted); err != nil {
		t.Errorf("re-asserting 'halted' on a halted run must stay a no-op success, got %v", err)
	}
}

// TestAnUnhaltedRunStillCompletes is the over-fire twin. The precondition must
// block exactly one transition and no other: an ordinary run whose last task
// finishes still reaches 'completed'. A guard that reddens correct work gets
// switched off, and then it guards nothing.
//
// This case is GREEN both with and without the guard, by design — it is the
// control, not the regression.
func TestAnUnhaltedRunStillCompletes(t *testing.T) {
	app, admin := startHaltContractPostgres(t)
	ctx := context.Background()

	repo := NewRepo(app)
	w := haltContractWorker(repo)

	tenant := seedHaltContractTenant(t, admin, "gh482-ok")
	site := seedHaltContractSite(t, admin, tenant, "https://ok.gh482.example")

	run, tasks, err := repo.CreateRunWithTasks(ctx, CreateRunInput{TenantID: tenant}, []NewTask{
		{SiteID: site, TargetType: TargetPlugin, TargetSlug: "akismet", DesiredVersion: "latest", FromVersion: "1.0.0"},
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	running, err := repo.MarkTaskRunning(ctx, tenant, tasks[0].ID, 30*time.Minute)
	if err != nil {
		t.Fatalf("mark running: %v", err)
	}

	if err := w.finish(ctx, running, TaskSucceeded, "1.0.0", "1.1.0", "updated", ""); err != nil {
		t.Fatalf("finish: %v", err)
	}

	if got := readRunStatus(t, app, tenant, run.ID); got != RunCompleted {
		t.Errorf("run status = %q, want %q: the precondition is over-firing and an ordinary run can no longer complete", got, RunCompleted)
	}
	if got := w.maybeCompleteRun(ctx, tenant, run.ID); got != RunCompleted {
		t.Errorf("published run status = %q, want %q", got, RunCompleted)
	}
}

// ---------------------------------------------------------------------------
// The refused halt
// ---------------------------------------------------------------------------

// countRunHaltedAudit counts update.run.halted entries for one run, read
// through the same tenant wrapper the recorder writes through.
func countRunHaltedAudit(t *testing.T, pool *db.Pool, tenant, runID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.InTenantTx(context.Background(), tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT count(*) FROM audit_log
			  WHERE tenant_id = $1 AND action = $2 AND target_id = $3`,
			tenant, ActionRunHalted, runID.String()).Scan(&n)
	}); err != nil {
		t.Fatalf("count halted audit records: %v", err)
	}
	return n
}

// readTaskStatus reads one task's ground truth through the same tenant wrapper
// the repo writes through, so the read is subject to the same policies.
func readTaskStatus(t *testing.T, pool *db.Pool, tenant, taskID uuid.UUID) string {
	t.Helper()
	var s string
	if err := pool.InTenantTx(context.Background(), tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT status FROM update_tasks WHERE id = $1 AND tenant_id = $2`, taskID, tenant).Scan(&s)
	}); err != nil {
		t.Fatalf("read task status: %v", err)
	}
	return s
}

// readHaltAuditCancelledTasks reads the cancelled_tasks number the halt audit
// record published for one run. The count of records says an event was booked;
// this says WHAT IT CLAIMED, which is the half that can be false while the
// count is right.
func readHaltAuditCancelledTasks(t *testing.T, pool *db.Pool, tenant, runID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.InTenantTx(context.Background(), tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT (metadata->>'cancelled_tasks')::int FROM audit_log
			  WHERE tenant_id = $1 AND action = $2 AND target_id = $3`,
			tenant, ActionRunHalted, runID.String()).Scan(&n)
	}); err != nil {
		t.Fatalf("read cancelled_tasks from the halt audit record: %v", err)
	}
	return n
}

// TestARefusedHaltRecordsNoHaltedAudit pins the other half of #482's
// precondition: what happens to the CALLER when the database says no.
//
// SetUpdateRunStatus reports its refusal as zero rows, which pgx surfaces as
// pgx.ErrNoRows — the same error a missing row produces. Before this change
// haltLocked swallowed it as "run not found", which was defensible while that
// was the only thing it could mean. The #482 precondition gave it a second
// meaning: "the run is terminal and you asked for a different status". At that
// point the swallow started reporting a REFUSED halt as a successful one, and
// AgentRunEvaluation.Changed had already been computed from the pre-write
// snapshot, so recordRunHalted wrote an update.run.halted event for a halt that
// the database never let land. Announcing success over your own error.
//
// The only run status that can refuse a halt is 'expired' ('halted' matches the
// statement's idempotent `status = $3` arm and returns its row). No live worker
// sequence reaches it today — an expired run never dispatched anything, so
// nothing of its finishes later to re-judge the gate — which is why this was a
// latent defect and not an incident. The fixture therefore seeds the terminal
// state directly through the admin pool rather than reshaping production code
// to manufacture a route into it; the unit under test is haltLocked's handling
// of the refusal and recordRunHalted's audit gate, and both are reached
// through the real repo, over the wpmgr_app role, exactly as production does.
//
// IT ALSO PINS THE ORDER haltLocked asks in, which is the second half of the
// same sentence. "Refused" is only honest if NOTHING happened, and while the
// cancel loop ran before the status write a refusal returned a nil error over a
// transaction that had already terminalized every pending task of the run: the
// caller got Halted=false alongside Cancelled=N, the operator's log line said
// the halt did not land, and N sites' tasks were cancelled anyway by a halt the
// database had refused. The status transition is attempted first now, so a
// declined halt cannot touch a task row at all.
//
// RED WITHOUT THE FIX, two independent plants:
//
//   - Restore `if !errors.Is(err, pgx.ErrNoRows) { return ... }` as the whole of
//     haltLocked's zero-rows branch (drop the ev assignments) and the refused
//     run books an update.run.halted event.
//   - Move haltLocked's SetUpdateRunStatus call back below the cancel loop and
//     the refused run's pending tasks come back 'cancelled' with ev.Cancelled=2.
func TestARefusedHaltRecordsNoHaltedAudit(t *testing.T) {
	app, admin := startHaltContractPostgres(t)
	ctx := context.Background()

	repo := NewRepo(app)
	waves := repo.(AgentWaveRepo)
	// A REAL recorder over the app pool, not a fake: the assertion below is a
	// count of zero, and a stubbed-out recorder would satisfy it while proving
	// nothing. The positive control at the end is what makes the zero mean
	// something.
	w := NewWorker(repo, nil, nil, nil, nil, audit.NewRecorder(app, domain.SystemClock{}), nil, 5, 0)
	w.SetAgentSelfUpdate(AgentSelfUpdateDeps{Waves: waves})

	tenant := seedHaltContractTenant(t, admin, "gh482-refused")

	// Every run here is created with MORE THAN ONE task on purpose. A halt's
	// effect on task rows is the thing this test now measures on both sides,
	// and a single-task run cannot tell "cancelled the pending ones" apart from
	// "cancelled the one task it was handed".
	newRun := func(hosts ...string) (Run, []Task) {
		t.Helper()
		specs := make([]NewTask, 0, len(hosts))
		for _, h := range hosts {
			specs = append(specs, NewTask{
				SiteID:     seedHaltContractSite(t, admin, tenant, h),
				TargetType: TargetAgent, TargetSlug: AgentTargetSlug,
				DesiredVersion: "0.62.0", FromVersion: "0.61.80",
			})
		}
		run, tasks, err := repo.CreateRunWithTasks(ctx, CreateRunInput{TenantID: tenant}, specs)
		if err != nil {
			t.Fatalf("create run (%v): %v", hosts, err)
		}
		if len(tasks) != len(hosts) {
			t.Fatalf("created %d task(s) for %d host(s): the fixture is not the shape the assertions below read",
				len(tasks), len(hosts))
		}
		return run, tasks
	}

	// --- The refusal ------------------------------------------------------
	refused, refusedTasks := newRun("https://refused-a.gh482.example", "https://refused-b.gh482.example")
	for _, task := range refusedTasks {
		if got := readTaskStatus(t, app, tenant, task.ID); got != TaskPending {
			t.Fatalf("fixture task status = %q, want %q: a refused halt cannot be shown to leave pending tasks alone if none are pending", got, TaskPending)
		}
	}
	if _, err := admin.Exec(ctx,
		`UPDATE update_runs SET status = $1 WHERE id = $2 AND tenant_id = $3`,
		RunExpired, refused.ID, tenant); err != nil {
		t.Fatalf("seed the run into %q: %v", RunExpired, err)
	}

	ev, err := waves.HaltAgentRun(ctx, tenant, refused.ID, "agent self-update disabled by the kill switch")
	if err != nil {
		t.Fatalf("halt an expired run: %v (a refusal is a normal outcome, not an error)", err)
	}
	if !ev.Refused {
		t.Errorf("Refused = false on a halt the database declined; the caller cannot tell a refusal from a halt")
	}
	if ev.Changed {
		t.Errorf("Changed = true on a refused halt: this is the flag recordRunHalted audits on")
	}
	if ev.Halted {
		t.Errorf("Halted = true on a run that is %q, not halted", RunExpired)
	}
	if got := readRunStatus(t, app, tenant, refused.ID); got != RunExpired {
		t.Fatalf("run status = %q, want %q: the halt landed after all and this test is testing nothing", got, RunExpired)
	}

	// A REFUSED HALT IS A NO-OP TRANSACTION, NOT A PARTIAL ONE. The status
	// write is asked for first precisely so the cancel loop is never reached,
	// and these two assertions are the difference between "the halt did not
	// land" and "the halt did not land, but it cancelled everything on its way
	// out and then reported that it had changed nothing".
	if ev.Cancelled != 0 {
		t.Errorf("Cancelled = %d on a refused halt, want 0: the evaluation reports work the refusal says did not happen", ev.Cancelled)
	}
	for i, task := range refusedTasks {
		if got := readTaskStatus(t, app, tenant, task.ID); got != TaskPending {
			t.Errorf("refused run's task %d status = %q, want %q: a halt the database declined terminalized a real site's task",
				i, got, TaskPending)
		}
	}

	// The exact worker step that used to announce the halt that never landed.
	w.recordRunHalted(ctx, tenant, refused.ID, ev)

	if n := countRunHaltedAudit(t, app, tenant, refused.ID); n != 0 {
		t.Errorf("audit_log holds %d %q record(s) for a halt the database refused, want 0: the log asserts something that did not happen",
			n, ActionRunHalted)
	}

	// --- The positive control ---------------------------------------------
	// Without this, a recorder that silently wrote nothing at all would pass
	// the assertion above, and a haltLocked that had simply stopped cancelling
	// tasks would pass the two above that. A halt that DOES land must still
	// audit exactly once AND still cancel every task nothing was sent for.
	//
	// Two tasks, deliberately in different states when the halt arrives: the
	// first is dispatched (haltAgentRunWith finishes it as 'skipped' before
	// halting), the second is still pending and is the one the halt must
	// cancel. That is the ordinary kill-switch shape.
	landed, tasks := newRun("https://landed.gh482.example", "https://landed-pending.gh482.example")
	dispatched, stillPending := tasks[0], tasks[1]
	running, err := repo.MarkTaskRunning(ctx, tenant, dispatched.ID, 30*time.Minute)
	if err != nil {
		t.Fatalf("mark running: %v", err)
	}
	if err := w.haltAgentRunWith(ctx, running, TaskSkipped, running.FromVersion, "",
		"refused: kill switch", "", "agent self-update disabled by the kill switch"); err != nil {
		t.Fatalf("halt the live run: %v", err)
	}
	if got := readRunStatus(t, app, tenant, landed.ID); got != RunHalted {
		t.Fatalf("control run status = %q, want %q", got, RunHalted)
	}
	if got := readTaskStatus(t, app, tenant, stillPending.ID); got != TaskCancelled {
		t.Errorf("pending task of a halt that DID land = %q, want %q: moving the status write ahead of the cancel loop must not stop the cancel loop running",
			got, TaskCancelled)
	}
	if n := countRunHaltedAudit(t, app, tenant, landed.ID); n != 1 {
		t.Errorf("audit_log holds %d %q record(s) for a halt that DID land, want 1: the zero above proves nothing if the recorder never writes",
			n, ActionRunHalted)
	}
	// The audit record's own number, not just the row count: this is where a
	// Cancelled that disagreed with the task rows would surface to an operator.
	if n := readHaltAuditCancelledTasks(t, app, tenant, landed.ID); n != 1 {
		t.Errorf("the halt audit record claims cancelled_tasks = %d, want 1: the operator's record disagrees with what happened to the task rows", n)
	}
}
