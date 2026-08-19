// gh463_phase1_dispatch_integration_test.go — GH #463 Phase 1, the runtime half.
//
// Phase 0's suite proved the cross-tenant scan can SEE and CLAIM a due run.
// This one proves the dispatcher built on top of it does the right thing, and
// it is the first execution any of #473's nine statements has ever had.
//
// EVERY ASSERTION REACHES THE DATABASE THROUGH update.NewRepo(pool), the same
// repository production uses, on the pool startPostgres hands back — which
// connects as the NON-superuser, NOBYPASSRLS wpmgr_app role that every real
// install runs as, and whose transactions go through the real InAgentTx /
// InTenantTx wrappers. That is not ceremony. A test that opened its own
// connection, or connected as the container superuser, leaves every RLS policy
// inert and passes against a database where the feature is broken; that is
// exactly how m112's proofs passed while the email domain was cross-site
// readable. The few raw SQL statements below are SEEDING and INSPECTION only,
// and each still runs inside the same wrappers.
package tests

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/update"
)

// ---------------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------------

// countingTxEnqueuer is the stand-in for River inside the dispatch
// transaction. It counts inserts, and — when failOn is set — fails on the Nth,
// which is how the all-or-nothing property is forced.
//
// Counting enqueues is how "contacts no site" is asserted throughout this file.
// It is not a proxy for the real thing: update.Worker.Work is reachable ONLY
// through a TaskArgs job, so zero jobs means no signed command can reach any
// site, whatever any other layer does.
type countingTxEnqueuer struct {
	mu     sync.Mutex
	n      int
	failOn int // 1-based; 0 disables
}

func (e *countingTxEnqueuer) EnqueueTaskTx(_ context.Context, _ pgx.Tx, _, _, _ uuid.UUID, _ bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.n++
	if e.failOn != 0 && e.n == e.failOn {
		return errors.New("simulated River insert failure")
	}
	return nil
}

func (e *countingTxEnqueuer) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.n
}

// seedScheduledRunWithTask creates a deferred run through the REAL repo method,
// so the row is born exactly as Service.CreateRun would create it.
func seedScheduledRunWithTask(t *testing.T, repo update.Repo, tenant, siteID uuid.UUID, at time.Time, slugs ...string) (update.Run, []update.Task) {
	t.Helper()
	tasks := make([]update.NewTask, 0, len(slugs))
	for _, s := range slugs {
		tasks = append(tasks, update.NewTask{
			SiteID: siteID, TargetType: update.TargetPlugin, TargetSlug: s,
			DesiredVersion: "latest", FromVersion: "1.0.0",
		})
	}
	run, created, err := repo.CreateScheduledRunWithTasks(context.Background(), update.CreateRunInput{
		TenantID: tenant, ScheduledAt: &at,
	}, tasks)
	if err != nil {
		t.Fatalf("create scheduled run: %v", err)
	}
	return run, created
}

// runStatus reads one run's status through the tenant wrapper.
func runStatus(t *testing.T, pool *db.Pool, tenant, runID uuid.UUID) string {
	t.Helper()
	var s string
	if err := pool.InTenantTx(context.Background(), tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT status FROM update_runs WHERE id = $1`, runID).Scan(&s)
	}); err != nil {
		t.Fatalf("read run status: %v", err)
	}
	return s
}

// taskStatuses maps target_slug -> status for one run.
func taskStatuses(t *testing.T, pool *db.Pool, tenant, runID uuid.UUID) map[string]string {
	t.Helper()
	out := map[string]string{}
	if err := pool.InTenantTx(context.Background(), tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(context.Background(),
			`SELECT target_slug, status FROM update_tasks WHERE run_id = $1`, runID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var slug, st string
			if err := rows.Scan(&slug, &st); err != nil {
				return err
			}
			out[slug] = st
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("read task statuses: %v", err)
	}
	return out
}

// ---------------------------------------------------------------------------
// 1. The end-to-end behaviour. This is the feature.
// ---------------------------------------------------------------------------

// TestGH463_Phase1_ScheduledRunContactsNoSiteUntilItsTime is the issue's
// headline acceptance criterion, both halves of it.
//
// RED WITHOUT THE FEATURE: revert ListDueUpdateRuns' "scheduled_at <= now()"
// and the first half fails — the not-yet-due run is returned and dispatched
// early, which IS the reported defect (an operator picks 02:00, the fleet
// updates now).
func TestGH463_Phase1_ScheduledRunContactsNoSiteUntilItsTime(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	repo := update.NewRepo(pool)

	tenant := seedTenant(t, pool, "gh463-p1-e2e")
	siteID := seedSite(t, pool, tenant, "")

	future := time.Now().Add(10 * time.Minute)
	run, tasks := seedScheduledRunWithTask(t, repo, tenant, siteID, future, "akismet")
	if run.Status != update.RunScheduled {
		t.Fatalf("run born %q, want %q", run.Status, update.RunScheduled)
	}
	if len(tasks) != 1 || tasks[0].Status != update.TaskScheduled {
		t.Fatalf("tasks = %+v, want one 'scheduled'", tasks)
	}

	// --- before its time: invisible to the dispatcher ---------------------
	due, err := repo.ListDueRuns(ctx, 100)
	if err != nil {
		t.Fatalf("ListDueRuns: %v", err)
	}
	for _, r := range due {
		if r.ID == run.ID {
			t.Fatal("a run scheduled 10 minutes out was returned by the due scan: it would be dispatched now, which is the defect")
		}
	}
	enq := &countingTxEnqueuer{}
	if enq.count() != 0 {
		t.Fatal("impossible")
	}
	if got := taskStatuses(t, pool, tenant, run.ID)["akismet"]; got != update.TaskScheduled {
		t.Errorf("task status before its time = %q, want %q", got, update.TaskScheduled)
	}

	// --- at its time: dispatched ------------------------------------------
	// Move the clock forward by moving the row, which is the honest way round:
	// due-ness is decided by the DATABASE clock (now()), so the test must not
	// be able to disagree with it.
	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE update_runs SET scheduled_at = now() - interval '1 minute' WHERE id = $1`, run.ID)
		return err
	}); err != nil {
		t.Fatalf("advance schedule: %v", err)
	}

	due, err = repo.ListDueRuns(ctx, 100)
	if err != nil {
		t.Fatalf("ListDueRuns after due: %v", err)
	}
	var found *update.Run
	for i := range due {
		if due[i].ID == run.ID {
			found = &due[i]
		}
	}
	if found == nil {
		t.Fatal("a due run was NOT returned by the cross-tenant scan; the dispatcher would log '0 due runs' forever")
	}

	out, err := repo.DispatchDueRun(ctx, enq, *found)
	if err != nil {
		t.Fatalf("DispatchDueRun: %v", err)
	}
	if !out.Claimed || out.Dispatched != 1 || out.Skipped != 0 {
		t.Errorf("outcome = %+v, want claimed with 1 dispatched, 0 skipped", out)
	}
	if enq.count() != 1 {
		t.Errorf("enqueued %d task jobs, want 1", enq.count())
	}
	if got := runStatus(t, pool, tenant, run.ID); got != update.RunRunning {
		t.Errorf("run status after dispatch = %q, want %q", got, update.RunRunning)
	}
	if got := taskStatuses(t, pool, tenant, run.ID)["akismet"]; got != update.TaskPending {
		t.Errorf("task status after dispatch = %q, want %q", got, update.TaskPending)
	}
}

// TestGH463_Phase1_ScheduledTaskDoesNotBlockAnImmediateUpdate is the payoff of
// the whole design, and the coordinator is right that a failure here would mean
// the DESIGN is wrong rather than the code.
//
// A scheduled task must NOT hold its (tenant, site, target) slot in
// update_tasks_inflight_target_idx, or scheduling tomorrow's update of a plugin
// would reject today's urgent one with 409 targets_in_flight. The exclusion is
// by construction — the index is partial on ('pending','running') and
// InFlightTaskStatuses (#472) is that same pair — so this test is what stops
// anyone "tidying" 'scheduled' into either copy.
//
// RED WITHOUT THE FEATURE: make CreateScheduledUpdateTask insert 'pending'
// instead of 'scheduled' and both assertions below fail.
func TestGH463_Phase1_ScheduledTaskDoesNotBlockAnImmediateUpdate(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	repo := update.NewRepo(pool)

	tenant := seedTenant(t, pool, "gh463-p1-noblock")
	siteID := seedSite(t, pool, tenant, "")

	// Tomorrow's scheduled update of akismet.
	_, _ = seedScheduledRunWithTask(t, repo, tenant, siteID, time.Now().Add(20*time.Hour), "akismet")

	// The dedup pre-check must not see it.
	inFlight, err := repo.ListInFlightTargets(ctx, tenant, []uuid.UUID{siteID})
	if err != nil {
		t.Fatalf("ListInFlightTargets: %v", err)
	}
	key := update.InFlightKey{SiteID: siteID, TargetType: update.TargetPlugin, TargetSlug: "akismet"}
	if _, blocked := inFlight[key]; blocked {
		t.Error("a scheduled task claimed the in-flight slot: the operator's urgent update of this plugin would be rejected 409 targets_in_flight")
	}

	// And the authoritative guard — the partial unique index itself, via
	// CreateUpdateTask's ON CONFLICT arbiter — must let the immediate run
	// through. This is the assertion that would still catch the bug if the
	// pre-check above were merely wrong.
	_, immediateTasks, err := repo.CreateRunWithTasks(ctx, update.CreateRunInput{TenantID: tenant}, []update.NewTask{{
		SiteID: siteID, TargetType: update.TargetPlugin, TargetSlug: "akismet",
		DesiredVersion: "latest", FromVersion: "1.0.0",
	}})
	if err != nil {
		t.Fatalf("an immediate update of a plugin with a SCHEDULED task was rejected: %v", err)
	}
	if len(immediateTasks) != 1 || immediateTasks[0].Status != update.TaskPending {
		t.Fatalf("immediate tasks = %+v, want one 'pending'", immediateTasks)
	}
}

// TestGH463_Phase1_ReaperLeavesScheduledTasksAlone proves the other exclusion,
// and proves it does not over-fire: the SAME row in 'pending' is still reaped.
//
// Without it, deferring anything more than 45 minutes out would have the
// stale-task reaper mark it failed — with RunOnStart: true, fleet-wide, within
// seconds of the next deploy — reported to the operator as "stale: task
// exceeded max runtime". That is the risk the issue calls out as destroying
// scheduled work fleet-wide.
func TestGH463_Phase1_ReaperLeavesScheduledTasksAlone(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	repo := update.NewRepo(pool)

	tenant := seedTenant(t, pool, "gh463-p1-reaper")
	siteID := seedSite(t, pool, tenant, "")

	_, tasks := seedScheduledRunWithTask(t, repo, tenant, siteID, time.Now().Add(6*time.Hour), "akismet")
	taskID := tasks[0].ID

	// Age it three hours — four times staleTaskThreshold (45m).
	backdate := func(status string) {
		t.Helper()
		if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				`UPDATE update_tasks SET status = $2, updated_at = now() - interval '3 hours' WHERE id = $1`,
				taskID, status)
			return err
		}); err != nil {
			t.Fatalf("backdate task: %v", err)
		}
	}

	backdate(update.TaskScheduled)
	stale, err := repo.ListStaleUpdateTasks(ctx, 45*time.Minute, 500)
	if err != nil {
		t.Fatalf("ListStaleUpdateTasks: %v", err)
	}
	for _, s := range stale {
		if s.ID == taskID {
			t.Fatal("the reaper swept a three-hour-old SCHEDULED task: an operator's overnight run would be failed before it ever fired")
		}
	}

	// The control. A guard that never fires on anything is not known to guard
	// the right thing — the same row, in 'pending', must still be reaped.
	backdate(update.TaskPending)
	stale, err = repo.ListStaleUpdateTasks(ctx, 45*time.Minute, 500)
	if err != nil {
		t.Fatalf("ListStaleUpdateTasks (pending): %v", err)
	}
	var swept bool
	for _, s := range stale {
		if s.ID == taskID {
			swept = true
		}
	}
	if !swept {
		t.Error("the reaper did NOT sweep a three-hour-old PENDING task; the exclusion above proves nothing if the sweep is simply broken")
	}
}

// ---------------------------------------------------------------------------
// 2. The dispatch transaction.
// ---------------------------------------------------------------------------

// TestGH463_Phase1_ConcurrentClaimsExactlyOneWins is the cross-replica
// idempotency guarantee. Two control planes tick at the same instant; the row's
// status under its own lock decides who dispatches.
//
// RED WITHOUT THE FEATURE: drop "AND status = 'scheduled'" from
// ClaimUpdateRunForDispatch and both callers claim, both enqueue, and the site
// gets the same update twice concurrently — which races the agent's own
// rollback-snapshot pruning.
func TestGH463_Phase1_ConcurrentClaimsExactlyOneWins(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	repo := update.NewRepo(pool)

	tenant := seedTenant(t, pool, "gh463-p1-claim")
	siteID := seedSite(t, pool, tenant, "")
	run, _ := seedScheduledRunWithTask(t, repo, tenant, siteID, time.Now().Add(-time.Minute), "akismet")

	due, err := repo.ListDueRuns(ctx, 100)
	if err != nil || len(due) == 0 {
		t.Fatalf("ListDueRuns: %v (len %d)", err, len(due))
	}
	var target update.Run
	for _, r := range due {
		if r.ID == run.ID {
			target = r
		}
	}

	const replicas = 4
	enq := &countingTxEnqueuer{}
	outs := make([]update.DispatchOutcome, replicas)
	errs := make([]error, replicas)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < replicas; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			outs[i], errs[i] = repo.DispatchDueRun(ctx, enq, target)
		}(i)
	}
	close(start)
	wg.Wait()

	winners := 0
	for i, o := range outs {
		if errs[i] != nil {
			t.Fatalf("replica %d errored (a lost claim must be a benign zero-row result, never an error): %v", i, errs[i])
		}
		if o.Claimed {
			winners++
		}
	}
	if winners != 1 {
		t.Errorf("%d of %d concurrent replicas claimed the same run, want exactly 1", winners, replicas)
	}
	if enq.count() != 1 {
		t.Errorf("enqueued %d task jobs across %d replicas, want 1: the site would be updated more than once", enq.count(), replicas)
	}
}

// TestGH463_Phase1_DispatchIsAllOrNothing forces the River insert to fail and
// proves the claim and the task transition roll back with it.
//
// THIS IS THE PROPERTY THAT MAKES 'dispatching' SAFE. It is absent from
// update_runs_due_idx, so a run left there is never scanned again and no reaper
// anywhere in this system would find it. If the claim committed independently
// of the enqueue, a failure in the gap would strand the run permanently and
// silently. After the failure the run must still be 'scheduled', its task still
// 'scheduled', and the next tick must still find it.
//
// RED WITHOUT THE FEATURE: enqueue after the transaction commits instead of
// inside it (via the non-transactional Insert) and the run is left
// 'dispatching' with a 'pending' task and no job.
func TestGH463_Phase1_DispatchIsAllOrNothing(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	repo := update.NewRepo(pool)

	tenant := seedTenant(t, pool, "gh463-p1-atomic")
	siteID := seedSite(t, pool, tenant, "")
	run, _ := seedScheduledRunWithTask(t, repo, tenant, siteID, time.Now().Add(-time.Minute), "akismet", "jetpack")

	due, _ := repo.ListDueRuns(ctx, 100)
	var target update.Run
	for _, r := range due {
		if r.ID == run.ID {
			target = r
		}
	}

	// Fail the SECOND insert, so the first task has already moved and been
	// enqueued when the failure lands — the case where a partial commit would
	// be most tempting and most damaging.
	failing := &countingTxEnqueuer{failOn: 2}
	if _, err := repo.DispatchDueRun(ctx, failing, target); err == nil {
		t.Fatal("DispatchDueRun swallowed an enqueue failure; the run would be claimed with tasks that have no jobs")
	}

	if got := runStatus(t, pool, tenant, run.ID); got != update.RunScheduled {
		t.Errorf("run status after a failed dispatch = %q, want %q: it is stranded outside the due index", got, update.RunScheduled)
	}
	for slug, st := range taskStatuses(t, pool, tenant, run.ID) {
		if st != update.TaskScheduled {
			t.Errorf("task %s = %q after a failed dispatch, want %q", slug, st, update.TaskScheduled)
		}
	}

	// And it must still be claimable: the next tick recovers it.
	due2, err := repo.ListDueRuns(ctx, 100)
	if err != nil {
		t.Fatalf("ListDueRuns after rollback: %v", err)
	}
	var again bool
	for _, r := range due2 {
		if r.ID == run.ID {
			again = true
		}
	}
	if !again {
		t.Fatal("the rolled-back run is no longer due-scannable; it is lost")
	}
	healthy := &countingTxEnqueuer{}
	out, err := repo.DispatchDueRun(ctx, healthy, target)
	if err != nil {
		t.Fatalf("re-dispatch: %v", err)
	}
	if !out.Claimed || out.Dispatched != 2 {
		t.Errorf("re-dispatch outcome = %+v, want claimed with 2 dispatched", out)
	}
}

// TestGH463_Phase1_ABusyTargetIsSkippedAndTheRunSurvives is the savepoint's
// reason for existing, observed from the outside.
//
// One task of a two-task run has its (site, target) taken by an immediate run
// while the schedule waited. The whole point is that this costs ONE TASK and
// not the run: without per-task containment, the collision aborts the
// dispatcher's single transaction and one operator's manual update of one
// plugin kills an entire scheduled fleet run.
//
// Note on coverage, stated plainly: this exercises
// MarkScheduledUpdateTaskPending's NOT EXISTS arm, which is the common path.
// The residual race — a conflicting row inserted between that predicate and the
// commit, raising a true 23505 — is what the SAVEPOINT in dispatchOneTask
// handles, and forcing it deterministically needs a statement-level injection
// point this suite does not have. The savepoint is exercised on every task
// here; the 23505 branch specifically is not.
//
// RED WITHOUT THE FEATURE: drop the NOT EXISTS arm and the 23505 aborts the
// transaction, the run stays 'dispatching', and NEITHER task is dispatched.
func TestGH463_Phase1_ABusyTargetIsSkippedAndTheRunSurvives(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	repo := update.NewRepo(pool)

	tenant := seedTenant(t, pool, "gh463-p1-skip")
	siteID := seedSite(t, pool, tenant, "")
	run, _ := seedScheduledRunWithTask(t, repo, tenant, siteID, time.Now().Add(-time.Minute), "akismet", "jetpack")

	// The operator's urgent update of akismet, created while the run waited.
	if _, _, err := repo.CreateRunWithTasks(ctx, update.CreateRunInput{TenantID: tenant}, []update.NewTask{{
		SiteID: siteID, TargetType: update.TargetPlugin, TargetSlug: "akismet",
		DesiredVersion: "latest", FromVersion: "1.0.0",
	}}); err != nil {
		t.Fatalf("seed the competing immediate run: %v", err)
	}

	due, _ := repo.ListDueRuns(ctx, 100)
	var target update.Run
	for _, r := range due {
		if r.ID == run.ID {
			target = r
		}
	}
	enq := &countingTxEnqueuer{}
	out, err := repo.DispatchDueRun(ctx, enq, target)
	if err != nil {
		t.Fatalf("a busy target failed the whole dispatch: %v", err)
	}
	if !out.Claimed || out.Dispatched != 1 || out.Skipped != 1 {
		t.Errorf("outcome = %+v, want claimed with 1 dispatched and 1 skipped", out)
	}
	if enq.count() != 1 {
		t.Errorf("enqueued %d jobs, want 1 (the free target only)", enq.count())
	}

	got := taskStatuses(t, pool, tenant, run.ID)
	// Skip and failure must be distinguishable, and 'skipped' is the one that
	// says "nothing was sent to this site" rather than "this site broke".
	if got["akismet"] != update.TaskSkipped {
		t.Errorf("busy target = %q, want %q", got["akismet"], update.TaskSkipped)
	}
	if got["akismet"] == update.TaskFailed {
		t.Error("a busy target was recorded as FAILED; an operator would go hunting for a broken site")
	}
	if got["jetpack"] != update.TaskPending {
		t.Errorf("free target = %q, want %q: one busy plugin took the whole run down", got["jetpack"], update.TaskPending)
	}
	if s := runStatus(t, pool, tenant, run.ID); s != update.RunRunning {
		t.Errorf("run status = %q, want %q", s, update.RunRunning)
	}
}

// TestGH463_Phase1_ARunWithLiveWorkIsNotMarkedCompleted is the stranded-run
// defect inverted: not a run nobody would ever finish, but a run declared
// finished while it is still working.
//
// The scenario is an ordinary partial dispatch. An earlier pass moved one task
// to 'pending' and its command is out on a real site; this pass finds the
// remaining task's target busy and dispatches nothing. Deciding completion from
// this pass's own counters ("Dispatched == 0") reaches RunCompleted — because
// the loop skips non-'scheduled' rows WITHOUT counting them, so the live
// 'pending' task contributes to neither counter. The operator is told their
// fleet update finished while commands are still in flight.
//
// The fix is not a different sum, it is a different question:
// CountUnfinishedTasksForRun, which counts pending, running AND scheduled.
//
// RED WITHOUT THE FIX: restore `if out.Dispatched == 0 { out.Status =
// RunCompleted }` and this test reports the run 'completed' with a 'pending'
// task beneath it.
func TestGH463_Phase1_ARunWithLiveWorkIsNotMarkedCompleted(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	repo := update.NewRepo(pool)

	tenant := seedTenant(t, pool, "gh463-p1-livework")
	siteID := seedSite(t, pool, tenant, "")
	run, tasks := seedScheduledRunWithTask(t, repo, tenant, siteID,
		time.Now().Add(-time.Minute), "akismet", "jetpack")

	// An earlier partial pass: move ONE task to 'pending' by hand, exactly as a
	// previous dispatch would have left it. Its command is notionally out on
	// the site right now.
	var live update.Task
	for _, tk := range tasks {
		if tk.TargetSlug == "jetpack" {
			live = tk
		}
	}
	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE update_tasks SET status = 'pending' WHERE id = $1`, live.ID)
		return err
	}); err != nil {
		t.Fatalf("simulate the earlier partial pass: %v", err)
	}

	// And the remaining scheduled task's target is taken by an immediate run,
	// so this pass dispatches nothing at all.
	if _, _, err := repo.CreateRunWithTasks(ctx, update.CreateRunInput{TenantID: tenant}, []update.NewTask{{
		SiteID: siteID, TargetType: update.TargetPlugin, TargetSlug: "akismet",
		DesiredVersion: "latest", FromVersion: "1.0.0",
	}}); err != nil {
		t.Fatalf("seed the competing immediate run: %v", err)
	}

	due, _ := repo.ListDueRuns(ctx, 100)
	var target update.Run
	for _, r := range due {
		if r.ID == run.ID {
			target = r
		}
	}
	if target.ID == uuid.Nil {
		t.Fatal("the run left the due scan")
	}

	enq := &countingTxEnqueuer{}
	out, err := repo.DispatchDueRun(ctx, enq, target)
	if err != nil {
		t.Fatalf("DispatchDueRun: %v", err)
	}
	if !out.Claimed {
		t.Fatal("the run was not claimed")
	}
	if out.Dispatched != 0 {
		t.Fatalf("this pass dispatched %d tasks; the scenario requires it to dispatch none", out.Dispatched)
	}

	// The assertion. A run with a live 'pending' task is NOT finished.
	got := runStatus(t, pool, tenant, run.ID)
	if got == update.RunCompleted {
		t.Errorf("run marked %q while a task is still 'pending' and its command is out on the site: the operator is told their fleet update finished while it is still going out", got)
	}
	if got != update.RunRunning {
		t.Errorf("run status = %q, want %q", got, update.RunRunning)
	}

	// And the control, so the fix is not simply "never complete anything":
	// once the live task reaches a terminal state, a later pass over a run that
	// owes nothing does still complete it. Proven directly against the counter
	// the fix now consults.
	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE update_tasks SET status = 'succeeded', finished_at = now() WHERE id = $1`, live.ID)
		return err
	}); err != nil {
		t.Fatalf("terminalize the live task: %v", err)
	}
	remaining, err := repo.CountUnfinishedTasks(ctx, tenant, run.ID)
	if err != nil {
		t.Fatalf("CountUnfinishedTasks: %v", err)
	}
	if remaining != 0 {
		t.Errorf("unfinished count = %d once every task is terminal, want 0; the completion branch would now be unreachable for a genuinely finished run", remaining)
	}
}

// ---------------------------------------------------------------------------
// 3. Expiry.
// ---------------------------------------------------------------------------

// TestGH463_Phase1_ExpiryPastTheGraceWindow proves both arms of the window, and
// the clause an operator actually cares about: NO SITE IS CONTACTED.
//
// RED WITHOUT THE FEATURE: drop "AND scheduled_at < @expire_before" and the
// in-window run is expired too — the fail-open inversion the query's own
// comment spends a paragraph on, where the fleet's entire scheduled backlog is
// terminalized on schedule and the failure looks like the feature working.
func TestGH463_Phase1_ExpiryPastTheGraceWindow(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	repo := update.NewRepo(pool)

	tenant := seedTenant(t, pool, "gh463-p1-expire")
	siteID := seedSite(t, pool, tenant, "")

	// Came due three hours ago; the grace window is two.
	stale, _ := seedScheduledRunWithTask(t, repo, tenant, siteID, time.Now().Add(-3*time.Hour), "akismet")
	// Came due one minute ago; comfortably inside it.
	fresh, _ := seedScheduledRunWithTask(t, repo, tenant, siteID, time.Now().Add(-time.Minute), "jetpack")

	cutoff := time.Now().Add(-2 * time.Hour)

	expired, nTasks, err := repo.ExpireDueRun(ctx, tenant, stale.ID, cutoff,
		"not attempted: the run passed its dispatch window")
	if err != nil {
		t.Fatalf("ExpireDueRun: %v", err)
	}
	if !expired || nTasks != 1 {
		t.Fatalf("expired=%v tasks=%d, want true and 1", expired, nTasks)
	}
	if s := runStatus(t, pool, tenant, stale.ID); s != update.RunExpired {
		t.Errorf("stale run = %q, want %q", s, update.RunExpired)
	}
	// The ruling: 'expired', not 'cancelled'. Nobody cancelled this run.
	if s := taskStatuses(t, pool, tenant, stale.ID)["akismet"]; s != update.TaskExpired {
		t.Errorf("expired run's task = %q, want %q (not %q — that would tell the operator somebody stopped their run)",
			s, update.TaskExpired, update.TaskCancelled)
	}

	// The in-window run must NOT be expirable by the same cutoff...
	expired2, _, err := repo.ExpireDueRun(ctx, tenant, fresh.ID, cutoff, "should not happen")
	if err != nil {
		t.Fatalf("ExpireDueRun (fresh): %v", err)
	}
	if expired2 {
		t.Fatal("a run one minute past its start was expired by a two-hour window: the whole scheduled backlog would be terminalized on schedule")
	}
	// ...and must still dispatch normally.
	due, _ := repo.ListDueRuns(ctx, 100)
	var target update.Run
	for _, r := range due {
		if r.ID == fresh.ID {
			target = r
		}
	}
	if target.ID == uuid.Nil {
		t.Fatal("the in-window run left the due scan")
	}
	enq := &countingTxEnqueuer{}
	out, err := repo.DispatchDueRun(ctx, enq, target)
	if err != nil {
		t.Fatalf("dispatch the in-window run: %v", err)
	}
	if !out.Claimed || out.Dispatched != 1 || enq.count() != 1 {
		t.Errorf("in-window run outcome = %+v with %d enqueues, want claimed/1/1", out, enq.count())
	}

	// An expired run must be inert to every later trigger: not due-scannable,
	// and not claimable if something re-drives it from a stale scan result.
	due2, _ := repo.ListDueRuns(ctx, 100)
	for _, r := range due2 {
		if r.ID == stale.ID {
			t.Error("an expired run is still returned by the due scan; it would be resurrected")
		}
	}
	inert := &countingTxEnqueuer{}
	out2, err := repo.DispatchDueRun(ctx, inert, stale)
	if err != nil {
		t.Fatalf("re-drive an expired run: %v", err)
	}
	if out2.Claimed {
		t.Error("an expired run was claimed for dispatch; SetUpdateRunStatus's missing precondition would have allowed exactly this")
	}
	if inert.count() != 0 {
		t.Errorf("an expired run enqueued %d jobs, want 0: no site may be contacted", inert.count())
	}
}

// ---------------------------------------------------------------------------
// 4. The index.
// ---------------------------------------------------------------------------

// TestGH463_Phase1_DueScanUsesTheDueIndex is the specific evidence for why
// ListDueUpdateRuns restates "status = 'scheduled'" verbatim. Without this the
// restatement is folklore.
//
// Postgres uses a partial index only when it can prove the index predicate from
// the query's own WHERE clauses. Written any other way — status <> 'pending', a
// join against a status table — the proof fails, the index is discarded, and
// the dispatcher's every tick becomes a sequential scan over every run the
// install has ever created. It would still return the right rows, which is why
// nothing would ever fail; it would simply get slower forever.
//
// enable_seqscan is turned off for the assertion because a seed-sized table is
// always faster to scan than to index, so the planner would choose a seq scan
// on cost alone and tell us nothing about whether the index is USABLE. That
// usability is the property under test.
func TestGH463_Phase1_DueScanUsesTheDueIndex(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	repo := update.NewRepo(pool)

	tenant := seedTenant(t, pool, "gh463-p1-explain")
	siteID := seedSite(t, pool, tenant, "")
	for i := 0; i < 25; i++ {
		seedScheduledRunWithTask(t, repo, tenant, siteID,
			time.Now().Add(time.Duration(i-30)*time.Minute), fmt.Sprintf("p%d", i))
	}

	var plan strings.Builder
	if err := pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL enable_seqscan = off"); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `EXPLAIN
			SELECT * FROM update_runs
			WHERE status = 'scheduled' AND scheduled_at <= now()
			ORDER BY scheduled_at ASC, id ASC
			LIMIT 200`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				return err
			}
			plan.WriteString(line)
			plan.WriteString("\n")
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}

	t.Logf("due-scan plan:\n%s", plan.String())
	if !strings.Contains(plan.String(), "update_runs_due_idx") {
		t.Errorf("the due scan does NOT use update_runs_due_idx; every dispatcher tick is a sequential scan over every run the install has ever created.\nplan:\n%s", plan.String())
	}
}

// ---------------------------------------------------------------------------
// 5. The role.
// ---------------------------------------------------------------------------

// TestGH463_Phase1_EverythingRanAsTheApplicationRole is the check that makes
// every other test in this file mean something.
//
// A proof that runs as a superuser, or as a role with BYPASSRLS, leaves every
// policy inert and passes against a database where the tenancy boundary is
// simply absent. That is not hypothetical here: it is how m112's proofs passed
// while the email domain was cross-site readable, and a documented recovery
// statement worked as superuser and failed as wpmgr_app.
func TestGH463_Phase1_EverythingRanAsTheApplicationRole(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()

	var user string
	var super, bypass bool
	if err := pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT current_user, rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).
			Scan(&user, &super, &bypass)
	}); err != nil {
		t.Fatalf("read current role: %v", err)
	}

	if user != "wpmgr_app" {
		t.Errorf("connected as %q, want wpmgr_app (the role every install runs as)", user)
	}
	if super {
		t.Error("connected as a SUPERUSER: RLS is ignored entirely and every policy proof in this file is vacuous")
	}
	if bypass {
		t.Error("connected as a BYPASSRLS role: same problem")
	}
}
