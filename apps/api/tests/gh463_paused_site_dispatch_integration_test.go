// gh463_paused_site_dispatch_integration_test.go — GH #463: a scheduled run
// must not dispatch to a site whose monitoring is paused.
//
// THE POLICY THESE TESTS ENCODE is m117's, quoted from schema.sql: "Pause
// governs the SCHEDULE, never the operator", and pause must never stop
// "anything a person clicks". A deferred run IS on the schedule — the click
// happened hours earlier, to put it there, and at fire time nobody is present —
// so pause governs it. Skipping is therefore existing policy applied to a path
// that had not yet consulted it, not new policy.
//
// The practical case is the one that cannot be undone: people pause a site
// because something is wrong with it, and firing an update into a site somebody
// deliberately froze mid-incident is not recoverable. A skip is: they see it and
// re-run with one click.
//
// EVERY ASSERTION REACHES THE DATABASE THROUGH update.NewRepo(pool) and
// site.NewRepo(pool), the same repositories production uses, on the pool
// startPostgres hands back — the NON-superuser, NOBYPASSRLS wpmgr_app role every
// real install runs as, through the real InAgentTx / InTenantTx wrappers. A test
// that opened its own connection would leave every RLS policy inert and pass
// against a database where this is broken.
package tests

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
	"github.com/mosamlife/wpmgr/apps/api/internal/update"
)

// ---------------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------------

// recordingTxEnqueuer records WHICH tasks were enqueued, not merely how many.
//
// That distinction is the whole point of this file. "No command reaches the
// paused site" is asserted as the ABSENCE OF THAT SITE'S TASK from this list,
// which is a real proof rather than an inference from a count: update.Worker.Work
// — the only code that can sign and send a command to a site — is reachable
// ONLY through a TaskArgs job, so a task with no job here can contact nothing,
// whatever any other layer does.
type recordingTxEnqueuer struct {
	mu      sync.Mutex
	taskIDs []uuid.UUID
}

func (e *recordingTxEnqueuer) EnqueueTaskTx(_ context.Context, _ pgx.Tx, _, _ uuid.UUID, taskID uuid.UUID, _ bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.taskIDs = append(e.taskIDs, taskID)
	return nil
}

func (e *recordingTxEnqueuer) enqueued(id uuid.UUID) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, got := range e.taskIDs {
		if got == id {
			return true
		}
	}
	return false
}

func (e *recordingTxEnqueuer) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.taskIDs)
}

// seedRunAcrossSites creates ONE deferred run spanning several sites, through
// the real repo method, so the rows are born exactly as Service.CreateRun makes
// them. Each entry is one site and the single plugin slug targeted on it, so a
// slug identifies a site in the assertions below.
func seedRunAcrossSites(t *testing.T, repo update.Repo, tenant uuid.UUID, at time.Time, bySlug map[string]uuid.UUID) (update.Run, map[string]update.Task) {
	t.Helper()
	tasks := make([]update.NewTask, 0, len(bySlug))
	for slug, siteID := range bySlug {
		tasks = append(tasks, update.NewTask{
			SiteID: siteID, TargetType: update.TargetPlugin, TargetSlug: slug,
			DesiredVersion: "latest", FromVersion: "1.0.0",
		})
	}
	run, created, err := repo.CreateScheduledRunWithTasks(context.Background(), update.CreateRunInput{
		TenantID: tenant, ScheduledAt: &at,
	}, tasks)
	if err != nil {
		t.Fatalf("create scheduled run: %v", err)
	}
	out := make(map[string]update.Task, len(created))
	for _, task := range created {
		out[task.TargetSlug] = task
	}
	return run, out
}

// pauseSiteNow pauses one site through site.Repo.PauseMonitoring — the SAME
// call the operator's "pause monitoring" button makes. Nothing here writes
// monitoring_paused_at by hand: a test that set the column directly would prove
// the dispatcher reads a column, not that it honours the operator's pause.
func pauseSiteNow(t *testing.T, repo site.Repo, tenantID, siteID uuid.UUID, reason string) {
	t.Helper()
	states, err := repo.PauseMonitoring(context.Background(), site.PauseMonitoringInput{
		TenantID:  tenantID,
		Principal: autoResumePrincipal{tenantID: tenantID},
		SiteIDs:   []uuid.UUID{siteID},
		Reason:    reason,
	})
	if err != nil {
		t.Fatalf("pause monitoring: %v", err)
	}
	if len(states) != 1 || !states[0].Paused() {
		t.Fatalf("pause did not take: %+v", states)
	}
}

// resumeSiteNow clears the pause through the operator's own resume call.
func resumeSiteNow(t *testing.T, repo site.Repo, tenantID, siteID uuid.UUID) {
	t.Helper()
	states, err := repo.ResumeMonitoring(context.Background(), site.ResumeMonitoringInput{
		TenantID:  tenantID,
		Principal: autoResumePrincipal{tenantID: tenantID},
		SiteIDs:   []uuid.UUID{siteID},
	})
	if err != nil {
		t.Fatalf("resume monitoring: %v", err)
	}
	if len(states) != 1 || states[0].Paused() {
		t.Fatalf("resume did not take: %+v", states)
	}
}

// taskDetails maps target_slug -> detail for one run, read through the tenant
// wrapper. The detail is what carries the REASON to the operator.
func taskDetails(t *testing.T, pool *db.Pool, tenant, runID uuid.UUID) map[string]string {
	t.Helper()
	out := map[string]string{}
	if err := pool.InTenantTx(context.Background(), tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(context.Background(),
			`SELECT target_slug, COALESCE(detail, '') FROM update_tasks WHERE run_id = $1`, runID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var slug, detail string
			if err := rows.Scan(&slug, &detail); err != nil {
				return err
			}
			out[slug] = detail
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("read task details: %v", err)
	}
	return out
}

// dueRun re-reads one run through the cross-tenant due scan, so the dispatch
// under test receives exactly the Run the production dispatcher would.
func dueRun(t *testing.T, repo update.Repo, runID uuid.UUID) update.Run {
	t.Helper()
	due, err := repo.ListDueRuns(context.Background(), 100)
	if err != nil {
		t.Fatalf("list due runs: %v", err)
	}
	for _, r := range due {
		if r.ID == runID {
			return r
		}
	}
	t.Fatalf("run %s was not returned by the due scan", runID)
	return update.Run{}
}

// ---------------------------------------------------------------------------
// 1. The feature, and the case it exists for.
// ---------------------------------------------------------------------------

// TestGH463_PausedSiteIsSkippedAndTheUnpausedSiteStillDispatches is the whole
// decision in one test, and the ORDER OF ITS STEPS IS THE POINT.
//
// The run is created FIRST, while both sites are active, and one site is paused
// AFTERWARDS. That is the scenario the check exists for and the one a create-time
// read cannot see: an operator defers a fleet update to 02:00, something goes
// wrong on one site at 23:00, they pause it, and at 02:00 the run fires. A
// dispatcher that captured pause state when the run was created would fire into
// the frozen site regardless.
//
// The second assertion is the one that actually matters. A fix that skipped the
// WHOLE RUN because one site was paused would pass a naive "the paused site was
// not updated" test while being its own, worse defect: nine healthy sites left
// unpatched because of one frozen one.
//
// RED WITHOUT THE CHANGE: remove the `paused` lookup and its `continue` from
// pgRepo.DispatchDueRun and the paused site's task comes back 'pending' with a
// job enqueued.
func TestGH463_PausedSiteIsSkippedAndTheUnpausedSiteStillDispatches(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	repo := update.NewRepo(pool)
	siteRepo := site.NewRepo(pool)

	tenant := seedTenant(t, pool, "gh463-paused-skip")
	frozen := seedSite(t, pool, tenant, "")
	healthy := seedSite(t, pool, tenant, "")

	// STEP 1 — schedule, while BOTH sites are active.
	run, tasks := seedRunAcrossSites(t, repo, tenant, time.Now().Add(-time.Minute), map[string]uuid.UUID{
		"akismet": frozen,
		"jetpack": healthy,
	})

	// STEP 2 — the incident. The operator pauses the one site, AFTER the run
	// was scheduled and BEFORE it fires.
	pauseSiteNow(t, siteRepo, tenant, frozen, "investigating a 500 on checkout")

	// STEP 3 — the schedule fires.
	enq := &recordingTxEnqueuer{}
	out, err := repo.DispatchDueRun(ctx, enq, dueRun(t, repo, run.ID))
	if err != nil {
		t.Fatalf("a paused site failed the whole dispatch: %v", err)
	}

	// NO COMMAND REACHED THE PAUSED SITE. Asserted as the absence of that
	// task's job, not as a status.
	if enq.enqueued(tasks["akismet"].ID) {
		t.Error("a job was enqueued for the PAUSED site: the update will reach a site the operator deliberately froze")
	}
	if !enq.enqueued(tasks["jetpack"].ID) {
		t.Error("no job was enqueued for the ACTIVE site: one paused site took the whole run down")
	}
	if enq.count() != 1 {
		t.Errorf("enqueued %d jobs, want exactly 1 (the active site only)", enq.count())
	}

	if !out.Claimed || out.Dispatched != 1 || out.Skipped != 1 || out.PausedSkipped != 1 {
		t.Errorf("outcome = %+v, want claimed with 1 dispatched, 1 skipped, 1 of them paused", out)
	}

	got := taskStatuses(t, pool, tenant, run.ID)
	if got["akismet"] != update.TaskSkipped {
		t.Errorf("paused site's task = %q, want %q", got["akismet"], update.TaskSkipped)
	}
	// Nothing was attempted and nothing is broken, so this must not read as a
	// failure: an operator seeing 'failed' goes hunting for a broken site.
	if got["akismet"] == update.TaskFailed {
		t.Error("a paused site's task was recorded FAILED; nothing was attempted and nothing is broken")
	}
	if got["jetpack"] != update.TaskPending {
		t.Errorf("active site's task = %q, want %q: one paused site took the whole run down", got["jetpack"], update.TaskPending)
	}

	// THE SKIP MUST BE VISIBLE. A run reporting "1 of 2 applied" with no
	// explanation reads as broken; the reason travels on the task's detail,
	// which the task DTO carries to the dashboard.
	details := taskDetails(t, pool, tenant, run.ID)
	if !strings.Contains(strings.ToLower(details["akismet"]), "paused") {
		t.Errorf("skip detail = %q, want it to name the pause: an unexplained skip reads as a broken run", details["akismet"])
	}

	if s := runStatus(t, pool, tenant, run.ID); s != update.RunRunning {
		t.Errorf("run status = %q, want %q: the active site's work is still outstanding", s, update.RunRunning)
	}
}

// ---------------------------------------------------------------------------
// 2. The check is at FIRE time. This is the one most likely to be got wrong.
// ---------------------------------------------------------------------------

// TestGH463_ResumingBeforeTheRunFiresDispatchesNormally is the inverse of the
// test above and is what makes it a proof of a FIRE-TIME read rather than of any
// read at all.
//
// Pause and then resume, both after scheduling. If the dispatcher captured pause
// state at create time it would see "active" and dispatch — passing this test for
// the wrong reason. If it cached the state from the pause it would see "paused"
// and skip, failing. Only a read taken at the moment of firing gets this right,
// and it is also the operator's own expectation: they un-paused the site
// precisely so their scheduled work would run.
func TestGH463_ResumingBeforeTheRunFiresDispatchesNormally(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	repo := update.NewRepo(pool)
	siteRepo := site.NewRepo(pool)

	tenant := seedTenant(t, pool, "gh463-resume-fires")
	siteID := seedSite(t, pool, tenant, "")

	run, tasks := seedRunAcrossSites(t, repo, tenant, time.Now().Add(-time.Minute), map[string]uuid.UUID{
		"akismet": siteID,
	})

	// Paused during the incident, then resumed once it was over — all of it
	// after the run was scheduled and before it fired.
	pauseSiteNow(t, siteRepo, tenant, siteID, "incident")
	resumeSiteNow(t, siteRepo, tenant, siteID)

	enq := &recordingTxEnqueuer{}
	out, err := repo.DispatchDueRun(ctx, enq, dueRun(t, repo, run.ID))
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if !enq.enqueued(tasks["akismet"].ID) {
		t.Error("a RESUMED site was not dispatched to: the pause outlived the resume")
	}
	if out.Dispatched != 1 || out.PausedSkipped != 0 {
		t.Errorf("outcome = %+v, want 1 dispatched and 0 paused-skips", out)
	}
	if got := taskStatuses(t, pool, tenant, run.ID); got["akismet"] != update.TaskPending {
		t.Errorf("resumed site's task = %q, want %q", got["akismet"], update.TaskPending)
	}
}

// ---------------------------------------------------------------------------
// 3. The all-paused run's terminal status — a deliberate choice.
// ---------------------------------------------------------------------------

// TestGH463_ARunWhoseSitesAreAllPausedCompletesWithoutContactingAnySite pins the
// decision made in DispatchDueRun, so a later reader changes it on purpose
// rather than by accident.
//
// The run lands 'completed'. That is NOT "the updates were applied": 'completed'
// is defined in schema.sql as "All tasks reached a terminal state", and every
// task here did. It is not 'failed' — nothing was attempted and nothing is
// broken. It is not 'expired', which is about the control plane missing its own
// window. And it is the SAME status an all-targets-busy run has always taken,
// which is the same shape; giving one of two identical situations a different
// status would report the same fact two ways.
//
// What stops that from overstating the outcome is asserted here too: every task
// says why, and out.PausedSkipped carries the count to the dispatch audit entry.
func TestGH463_ARunWhoseSitesAreAllPausedCompletesWithoutContactingAnySite(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	repo := update.NewRepo(pool)
	siteRepo := site.NewRepo(pool)

	tenant := seedTenant(t, pool, "gh463-all-paused")
	siteA := seedSite(t, pool, tenant, "")
	siteB := seedSite(t, pool, tenant, "")

	run, _ := seedRunAcrossSites(t, repo, tenant, time.Now().Add(-time.Minute), map[string]uuid.UUID{
		"akismet": siteA,
		"jetpack": siteB,
	})
	pauseSiteNow(t, siteRepo, tenant, siteA, "incident")
	pauseSiteNow(t, siteRepo, tenant, siteB, "incident")

	enq := &recordingTxEnqueuer{}
	out, err := repo.DispatchDueRun(ctx, enq, dueRun(t, repo, run.ID))
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if enq.count() != 0 {
		t.Errorf("enqueued %d jobs, want 0: every site in this run is paused", enq.count())
	}
	if out.Dispatched != 0 || out.Skipped != 2 || out.PausedSkipped != 2 {
		t.Errorf("outcome = %+v, want 0 dispatched and 2 skipped, both for pause", out)
	}

	// The run is finished, and every task says why it did nothing.
	if s := runStatus(t, pool, tenant, run.ID); s != update.RunCompleted {
		t.Errorf("run status = %q, want %q (every task reached a terminal state)", s, update.RunCompleted)
	}
	for slug, st := range taskStatuses(t, pool, tenant, run.ID) {
		if st != update.TaskSkipped {
			t.Errorf("task %q = %q, want %q", slug, st, update.TaskSkipped)
		}
	}
	for slug, detail := range taskDetails(t, pool, tenant, run.ID) {
		if !strings.Contains(strings.ToLower(detail), "paused") {
			t.Errorf("task %q detail = %q, want it to name the pause", slug, detail)
		}
	}
}

// ---------------------------------------------------------------------------
// 4. The role. Same guard the Phase 1 suite carries.
// ---------------------------------------------------------------------------

// TestGH463_PausedSkipRanAsTheApplicationRole proves the tests above were not
// quietly running as a superuser, which would leave every RLS policy inert and
// pass against a database where this is broken. It is the m112 lesson, kept as
// an executable check rather than a comment.
func TestGH463_PausedSkipRanAsTheApplicationRole(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()

	var role string
	var super, bypass bool
	if err := pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT current_user, rolsuper, rolbypassrls
			   FROM pg_roles WHERE rolname = current_user`).Scan(&role, &super, &bypass)
	}); err != nil {
		t.Fatalf("read current role: %v", err)
	}
	if super {
		t.Errorf("connected as %q, which is a SUPERUSER: every RLS policy is inert and these proofs mean nothing", role)
	}
	if bypass {
		t.Errorf("connected as %q, which has BYPASSRLS: every RLS policy is inert and these proofs mean nothing", role)
	}
}
