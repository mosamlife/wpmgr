package update

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// These tests cover the CALLER half of the update-task claim compare-and-swap
// (MarkUpdateTaskRunning in db/query/updates.sql). The SQL half is pinned by
// tests/update_task_claim_cas_test.go against a real database; what is pinned
// here is what Worker.Work does with each of the CAS's two outcomes, because
// the failure mode this change exists to close is a caller that compiles fine
// and silently does the wrong thing with them.

// claimTestWorker wires a Worker with the minimum needed to drive Work as far
// as the claim: a site the lookup can resolve, a commander that records
// whether anything was ever dispatched, and a repo whose hooks the caller sets.
func claimTestWorker(repo *probeFakeRepo, task Task) (*Worker, *recordingCommander) {
	cmd := &recordingCommander{}
	sites := &fakeSiteLookup{sites: map[uuid.UUID]SiteInfo{
		task.SiteID: {ID: task.SiteID, URL: "https://example.test", Name: "example", Enrolled: true},
	}}
	w := NewWorker(repo, sites, cmd, &scriptedProber{script: []probeStep{healthyStep()}}, nil /* hub */, nil /* audit */, nil, 5, 0)
	// Deterministic snooze: the jittered production backoff would make the
	// assertions below flaky about the duration.
	w.busyBackoff = func(Task) time.Duration { return 7 * time.Second }
	return w, cmd
}

// recordingCommander records every dispatch. A dispatch that must not happen
// is proved by updateCalls staying 0; the responses are shaped so a dispatch
// that DOES happen completes cleanly rather than dragging the test into the
// rollback ladder.
type recordingCommander struct {
	updateCalls int
}

func (c *recordingCommander) Update(_ context.Context, _ uuid.UUID, _ string, req agentcmd.UpdateRequest) (agentcmd.UpdateResponse, error) {
	c.updateCalls++
	item := req.Items[0]
	return agentcmd.UpdateResponse{OK: true, Results: []agentcmd.ItemResult{{
		Type: item.Type, Slug: item.Slug, Status: agentcmd.ItemSucceeded, ToVersion: item.Version,
	}}}, nil
}

func (c *recordingCommander) Rollback(context.Context, uuid.UUID, string, agentcmd.RollbackRequest) (agentcmd.RollbackResponse, error) {
	panic("no rollback expected in a claim test")
}

// claimTestTask is a plain non-agent task in the state Work expects to find it
// in when a job starts: pending, nothing dispatched yet.
func claimTestTask() Task {
	t := testTask()
	t.Status = TaskPending
	return t
}

// claimRepo returns a probeFakeRepo wired to let Work reach the claim: the row
// reads back as `task`, no other task is running for the tenant, and the run
// is already running so ensureRunRunning is a no-op.
func claimRepo(task Task) *probeFakeRepo {
	return &probeFakeRepo{
		getTask:      func(uuid.UUID, uuid.UUID) (Task, error) { return task, nil },
		countRunning: func(uuid.UUID) (int64, error) { return 0, nil },
		getRun:       func(uuid.UUID, uuid.UUID) (Run, error) { return Run{Status: RunRunning}, nil },
	}
}

func runClaimWork(t *testing.T, w *Worker, task Task) error {
	t.Helper()
	return w.Work(context.Background(), &river.Job[TaskArgs]{
		Args: TaskArgs{TenantID: task.TenantID, RunID: task.RunID, TaskID: task.ID},
	})
}

// TestClaim_PassesRealStaleAfter is the regression test for the defect this
// change closes. Both call sites used keyed struct literals, so when the
// generated params struct grew StaleAfter they kept compiling while sending a
// zero pgtype.Interval — SQL NULL — which makes the CAS's reclaim branch match
// nothing. Nothing errored and nothing logged; the feature was simply inert.
// Assert on the VALUE the worker hands the repo, not merely that the claim
// succeeded, because a zero value succeeds too.
//
// RED: revert worker.go's call to MarkTaskRunning(ctx, a.TenantID, a.TaskID, 0).
func TestClaim_PassesRealStaleAfter(t *testing.T) {
	task := claimTestTask()
	repo := claimRepo(task)
	repo.markRunning = func(_, _ uuid.UUID, _ time.Duration) (Task, error) {
		running := task
		running.Status = TaskRunning
		return running, nil
	}
	w, _ := claimTestWorker(repo, task)

	if err := runClaimWork(t, w, task); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if repo.markStaleAfter != w.claimStaleAfter {
		t.Fatalf("the claim must pass the worker's own derived staleness bound, got %v want %v (a zero duration is a NULL interval, which silently disables abandoned-task reclaim)",
			repo.markStaleAfter, w.claimStaleAfter)
	}
	if repo.markStaleAfter <= 0 {
		t.Fatal("the claim's staleness bound must be positive")
	}
}

// TestClaimStaleAfter_TracksTheConfiguredApplyBudget is the F1 regression. The
// bound the claim uses must EXCEED this install's worst-case apply budget, or
// a second worker reclaims a row whose holder is still applying and both
// dispatch. That budget is config-driven (DeriveApplyJobTimeout over
// cfg.Update.ApplyHTTPTimeout and cfg.Update.HTTPTimeout, both env-settable),
// so a fixed constant stops exceeding it once the timeout is raised. The
// existing ordering test pins the DEFAULT configuration only; this one sweeps
// configurations a real operator can set.
//
// RED: make ClaimStaleAfter return the constant siteWriterHoldMax again, and
// every row from 10m upward fails.
func TestClaimStaleAfter_TracksTheConfiguredApplyBudget(t *testing.T) {
	// applyHTTPTimeout values an operator can set via
	// WPMGR_UPDATE_APPLY_HTTP_TIMEOUT, spanning the default and well past the
	// point the old constant stopped covering the budget.
	for _, applyHTTP := range []time.Duration{
		1 * time.Minute, 8 * time.Minute, 14 * time.Minute, 20 * time.Minute, 30 * time.Minute,
	} {
		budget := DeriveApplyJobTimeout(applyHTTP, 30*time.Second)
		bound := ClaimStaleAfter(budget)
		if bound <= budget {
			t.Errorf("apply_http_timeout=%v: claim bound %v must exceed the apply budget %v, "+
				"or a worker reclaims a task another worker is still applying", applyHTTP, bound, budget)
		}
	}
}

// TestValidateClaimTimings_FiresAndDoesNotOverFire covers the boot assertion
// both ways: it must refuse a configuration that puts the claim bound at or
// past the reaper threshold, and it must NOT refuse configurations an operator
// can legitimately run. A boot check that reddens valid config gets deleted,
// and then it guards nothing.
//
// RED: return nil unconditionally from ValidateClaimTimings and the invalid
// rows fail; clamp the bound below staleTaskThreshold and the valid rows fail.
func TestValidateClaimTimings_FiresAndDoesNotOverFire(t *testing.T) {
	// Valid: the default, and everything up to the point the bound would
	// reach the reaper threshold.
	for _, applyHTTP := range []time.Duration{
		0, 1 * time.Minute, 8 * time.Minute, 20 * time.Minute,
	} {
		budget := DeriveApplyJobTimeout(applyHTTP, 30*time.Second)
		if err := ValidateClaimTimings(budget); err != nil {
			t.Errorf("apply_http_timeout=%v is a legitimate configuration and must boot, got %v", applyHTTP, err)
		}
	}
	// Invalid: the bound would land at or past staleTaskThreshold, so the
	// reaper could terminalize a row the claim still treats as live.
	for _, applyHTTP := range []time.Duration{40 * time.Minute, 90 * time.Minute} {
		budget := DeriveApplyJobTimeout(applyHTTP, 30*time.Second)
		err := ValidateClaimTimings(budget)
		if err == nil {
			t.Errorf("apply_http_timeout=%v pushes the claim bound to %v, at or past the reaper threshold %v: boot must fail",
				applyHTTP, ClaimStaleAfter(budget), staleTaskThreshold)
			continue
		}
		// The operator who raised the timeout has to be able to act on this.
		if !strings.Contains(err.Error(), "apply_http_timeout") {
			t.Errorf("the boot failure must name the knob to change, got %q", err)
		}
	}
}

// TestYieldContendedClaim_IsWallClockBounded is the F2 regression. Every other
// waiting path in this worker is bounded by siteBusyMaxWait from CreatedAt and
// terminalizes as TaskSkipped; before this, the contended-claim path was the
// only one that could snooze forever, and its unreadable-row branch did so
// with nothing but a Warn — no error, no attempt consumed, no terminal state.
//
// RED: drop the busyBoundExceeded check from yieldContendedClaim and both
// subtests snooze instead of terminalizing.
func TestYieldContendedClaim_IsWallClockBounded(t *testing.T) {
	cases := map[string]func(*probeFakeRepo){
		"row still held": func(r *probeFakeRepo) {
			held := claimTestTask()
			held.Status = TaskRunning
			r.getTask = func(uuid.UUID, uuid.UUID) (Task, error) { return held, nil }
		},
		// The branch that was genuinely unbounded: the re-read itself fails,
		// so the bound must not depend on it.
		"row unreadable": func(r *probeFakeRepo) {
			r.getTask = func(uuid.UUID, uuid.UUID) (Task, error) {
				return Task{}, errors.New("connection reset")
			}
		},
	}
	for name, wire := range cases {
		t.Run(name, func(t *testing.T) {
			task := claimTestTask()
			task.CreatedAt = time.Now().Add(-siteBusyMaxWait - time.Minute)
			repo := claimRepo(task)
			wire(repo)
			w, cmd := claimTestWorker(repo, task)

			err := w.yieldContendedClaim(context.Background(), TaskArgs{
				TenantID: task.TenantID, RunID: task.RunID, TaskID: task.ID,
			}, task)
			if err != nil {
				t.Fatalf("a task past its wall-clock bound must terminalize, not snooze; got %v", err)
			}
			if len(repo.finished) != 1 {
				t.Fatalf("expected exactly one terminal write, got %+v", repo.finished)
			}
			if got := repo.finished[0].Status; got != TaskSkipped {
				t.Fatalf("status = %q, want %q: this worker never got the claim, so nothing was sent to the site",
					got, TaskSkipped)
			}
			if cmd.updateCalls != 0 {
				t.Fatalf("a task that never won the claim must never dispatch, got %d", cmd.updateCalls)
			}
		})
	}
}

// TestYieldContendedClaim_WithinBoundStillSnoozes is the does-not-over-fire
// companion: the bound must not terminalize an ordinary contended task that
// has only been waiting a moment.
//
// RED: drop the `bound != ""` condition so the check always terminalizes.
func TestYieldContendedClaim_WithinBoundStillSnoozes(t *testing.T) {
	task := claimTestTask()
	task.CreatedAt = time.Now() // nowhere near siteBusyMaxWait
	repo := claimRepo(task)
	held := task
	held.Status = TaskRunning
	repo.getTask = func(uuid.UUID, uuid.UUID) (Task, error) { return held, nil }
	w, _ := claimTestWorker(repo, task)

	err := w.yieldContendedClaim(context.Background(), TaskArgs{
		TenantID: task.TenantID, RunID: task.RunID, TaskID: task.ID,
	}, task)
	if _, ok := asSnooze(err); !ok {
		t.Fatalf("a freshly contended task must still snooze, got %v (%T)", err, err)
	}
	if len(repo.finished) != 0 {
		t.Fatalf("a task within its bound must not be terminalized, got %+v", repo.finished)
	}
}

// TestClaim_Succeeds_Dispatches is the does-not-over-fire case: an ordinary
// uncontended claim must still go all the way through to a dispatch. A guard
// that blocks correct work gets switched off, and then it guards nothing.
//
// RED: make yieldContendedClaim run unconditionally (drop the errors.Is check
// in Work) and the dispatch never happens.
func TestClaim_Succeeds_Dispatches(t *testing.T) {
	task := claimTestTask()
	repo := claimRepo(task)
	repo.markRunning = func(_, _ uuid.UUID, _ time.Duration) (Task, error) {
		running := task
		running.Status = TaskRunning
		return running, nil
	}
	w, cmd := claimTestWorker(repo, task)

	if err := runClaimWork(t, w, task); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if cmd.updateCalls != 1 {
		t.Fatalf("a successful claim must dispatch exactly once, got %d", cmd.updateCalls)
	}
}

// TestClaim_Reclaimed_AbandonedTaskDispatches is the behaviour that is INERT
// today: a row left 'running' by a worker that died is re-claimable once it
// ages past siteWriterHoldMax, and the retrying job must dispatch rather than
// give up. The repo hook here stands in for the SQL reclaim arm (proved
// against a real DB in tests/update_task_claim_cas_test.go): it grants the
// claim only when handed a staleAfter that would actually match the row.
//
// RED: pass 0 (or any duration greater than the row's age) at the call site
// and the hook refuses, so the task snoozes forever instead of dispatching.
func TestClaim_Reclaimed_AbandonedTaskDispatches(t *testing.T) {
	task := claimTestTask()
	// A row whose command went out well past the point a live worker could
	// still be behind it: River has long since cancelled that job's context.
	task.Status = TaskRunning
	age := siteWriterHoldMax + 5*time.Minute

	repo := claimRepo(task)
	repo.markRunning = func(_, _ uuid.UUID, staleAfter time.Duration) (Task, error) {
		// Mirrors the CAS: the 'running' arm matches only when the row is
		// older than staleAfter. A NULL/zero interval matches nothing.
		if staleAfter <= 0 || age <= staleAfter {
			return Task{}, ErrTaskNotClaimed
		}
		running := task
		running.Status = TaskRunning
		return running, nil
	}
	w, cmd := claimTestWorker(repo, task)

	if err := runClaimWork(t, w, task); err != nil {
		t.Fatalf("an abandoned task must be reclaimed and dispatched, got %v", err)
	}
	if cmd.updateCalls != 1 {
		t.Fatalf("the reclaiming worker must dispatch exactly once, got %d", cmd.updateCalls)
	}
}

// TestClaim_Contended_SnoozesWithoutDispatching is the fires case. A losing
// claimant must snooze: not an error (that spends a retry attempt and
// eventually dead-letters work that never failed), not nil (that drops the
// task while its row stays pending forever, holding the in-flight dedup slot),
// and above all not domain.NotFound, which is what this path reported before
// and which sends an operator hunting for a row that is still there.
//
// RED: return the error instead of snoozing in yieldContendedClaim, or restore
// the pgx.ErrNoRows -> domain.NotFound mapping in repo.go.
func TestClaim_Contended_SnoozesWithoutDispatching(t *testing.T) {
	task := claimTestTask()
	repo := claimRepo(task)
	repo.markRunning = func(_, _ uuid.UUID, _ time.Duration) (Task, error) {
		return Task{}, ErrTaskNotClaimed
	}
	// The re-read sees the row alive and held by the winner.
	held := task
	held.Status = TaskRunning
	repo.getTask = func(uuid.UUID, uuid.UUID) (Task, error) { return held, nil }

	w, cmd := claimTestWorker(repo, task)
	err := runClaimWork(t, w, task)

	snooze, ok := asSnooze(err)
	if !ok {
		t.Fatalf("a losing claimant must snooze without consuming a retry attempt, got %v (%T)", err, err)
	}
	if snooze.Duration != 7*time.Second {
		t.Fatalf("snooze duration = %v, want the configured backoff 7s", snooze.Duration)
	}
	if cmd.updateCalls != 0 {
		t.Fatalf("a losing claimant must never dispatch, got %d dispatches", cmd.updateCalls)
	}
	if len(repo.finished) != 0 {
		t.Fatalf("a losing claimant must not terminalize the winner's task, got %+v", repo.finished)
	}
	// The re-read is the whole mechanism: Work's opening read plus
	// yieldContendedClaim's.
	if repo.getTaskCalls != 2 {
		t.Fatalf("the contended branch must re-read the row before deciding, got %d GetTask calls", repo.getTaskCalls)
	}
}

// TestClaim_Contended_TerminalRowStops proves the other half of the re-read:
// when the winner already recorded a terminal outcome, there is nothing left
// to do and snoozing would loop on a finished row. Return nil.
//
// RED: drop the terminal(current.Status) branch and this snoozes instead.
func TestClaim_Contended_TerminalRowStops(t *testing.T) {
	for _, status := range []string{TaskSucceeded, TaskFailed, TaskRolledBack, TaskSkipped, TaskCancelled} {
		t.Run(status, func(t *testing.T) {
			task := claimTestTask()
			repo := claimRepo(task)
			repo.markRunning = func(_, _ uuid.UUID, _ time.Duration) (Task, error) {
				return Task{}, ErrTaskNotClaimed
			}
			// First read: still open, so Work proceeds to the claim. Second
			// read (the re-read): the winner has finished it.
			repo.getTask = func(uuid.UUID, uuid.UUID) (Task, error) {
				if repo.getTaskCalls > 1 {
					done := task
					done.Status = status
					return done, nil
				}
				return task, nil
			}
			w, cmd := claimTestWorker(repo, task)

			if err := runClaimWork(t, w, task); err != nil {
				t.Fatalf("a task the winner already finished must stop cleanly, got %v", err)
			}
			if cmd.updateCalls != 0 {
				t.Fatalf("a terminal task must never be dispatched, got %d", cmd.updateCalls)
			}
			if len(repo.finished) != 0 {
				t.Fatalf("a terminal task must not be re-terminalized, got %+v", repo.finished)
			}
		})
	}
}

// TestClaim_Contended_UnreadableRowSnoozes covers the branch where the re-read
// itself fails: terminal and contended are indistinguishable, so take the safe
// reading (someone may hold it) and snooze rather than spend a retry attempt
// on a control-plane DB hiccup.
//
// RED: return the GetTask error and this fails on the snooze assertion.
func TestClaim_Contended_UnreadableRowSnoozes(t *testing.T) {
	task := claimTestTask()
	repo := claimRepo(task)
	repo.markRunning = func(_, _ uuid.UUID, _ time.Duration) (Task, error) {
		return Task{}, ErrTaskNotClaimed
	}
	boom := errors.New("connection reset")
	repo.getTask = func(uuid.UUID, uuid.UUID) (Task, error) {
		if repo.getTaskCalls > 1 {
			return Task{}, boom
		}
		return task, nil
	}
	w, cmd := claimTestWorker(repo, task)

	err := runClaimWork(t, w, task)
	if _, ok := asSnooze(err); !ok {
		t.Fatalf("an unreadable row after a refused claim must snooze, got %v (%T)", err, err)
	}
	if cmd.updateCalls != 0 {
		t.Fatalf("a worker that did not get the claim must never dispatch, got %d", cmd.updateCalls)
	}
}

// TestErrTaskNotClaimed_IsNotNotFoundOrNotOpen pins the three-way distinction
// the callers branch on. ErrTaskNotClaimed carries NO verdict about the task;
// conflating it with ErrTaskNotOpen would make a losing claimant return nil
// and drop live work, and the domain.NotFound it used to be reported an absent
// row that is in fact present and held.
//
// RED: define ErrTaskNotClaimed = ErrTaskNotOpen.
func TestErrTaskNotClaimed_IsNotNotFoundOrNotOpen(t *testing.T) {
	if errors.Is(ErrTaskNotClaimed, ErrTaskNotOpen) || errors.Is(ErrTaskNotOpen, ErrTaskNotClaimed) {
		t.Fatal("ErrTaskNotClaimed (no verdict; another worker holds the row) must stay distinct from ErrTaskNotOpen (a terminal outcome is recorded)")
	}
	if _, ok := domain.AsDomain(ErrTaskNotClaimed); ok {
		t.Fatal("ErrTaskNotClaimed must not be a domain error: reporting the losing claimant as NotFound sends operators hunting for a row that still exists")
	}
}
