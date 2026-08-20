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

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
)

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

// startHaltContractPostgres returns (appPool, adminPool). appPool connects as
// wpmgr_app — non-superuser, NOBYPASSRLS, the role every real install runs as —
// because the repo's InTenantTx wrappers are only meaningful against a role the
// policies actually apply to. adminPool is the superuser and seeds fixtures
// only. Mirrors internal/email/repo_site_scope_integration_test.go.
func startHaltContractPostgres(t *testing.T) (*db.Pool, *db.Pool) {
	t.Helper()
	ctx := context.Background()

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
	if err != nil {
		t.Skipf("skipping: cannot start postgres container (docker unavailable?): %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	adminDSN, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	adminPool, err := db.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	if err := adminPool.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for _, stmt := range []string{
		"ALTER ROLE wpmgr_app LOGIN PASSWORD 'app'",
		"GRANT USAGE ON SCHEMA public TO wpmgr_app",
		"GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO wpmgr_app",
		"REVOKE UPDATE, DELETE, TRUNCATE ON audit_log FROM wpmgr_app",
	} {
		if _, err := adminPool.Exec(ctx, stmt); err != nil {
			t.Fatalf("provision app role (%q): %v", stmt, err)
		}
	}
	t.Cleanup(adminPool.Close)

	appDSN := strings.Replace(adminDSN, "wpmgr:wpmgr@", "wpmgr_app:app@", 1)
	appPool, err := db.Connect(ctx, appDSN)
	if err != nil {
		t.Fatalf("connect app: %v", err)
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
