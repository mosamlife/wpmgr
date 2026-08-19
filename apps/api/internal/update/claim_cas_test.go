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
	// A NON-DEFAULT job timeout, deliberately. Constructing with 0 puts
	// claimStaleAfter on its siteWriterHoldMax floor, and then asserting that
	// the call site passes w.claimStaleAfter cannot tell the derived bound
	// apart from the flat constant — the F1 defect could be reintroduced at
	// the call site with nothing going red. This value makes the two differ
	// (apply budget 25m52s -> bound 31m52s, against a 20m floor), so the
	// assertion binds the seam and not just the arithmetic.
	jobTimeout := DeriveApplyJobTimeout(20*time.Minute, 30*time.Second)
	w := NewWorker(repo, sites, cmd, &scriptedProber{script: []probeStep{healthyStep()}}, nil /* hub */, nil /* audit */, nil, 5, jobTimeout)
	if w.claimStaleAfter == siteWriterHoldMax {
		panic("claim test fixture must construct a worker whose derived bound differs from the gate constant")
	}
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
		t.Fatalf("the claim must pass the worker's own derived staleness bound, got %v want %v "+
			"(a flat constant stops exceeding the apply budget once apply_http_timeout is raised, "+
			"and a non-positive bound would make every running row instantly reclaimable)",
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
	// The TRANSITION itself, derived from the constants rather than guessed.
	// The loops above bracket it at 20m valid and 40m invalid, and the real
	// boundary sits between them; a change to claimStaleMargin,
	// probeRetryDelays or agentVerifyTimeout moves it anywhere inside that gap
	// with both loops still green. Pin the edge instead: the largest budget
	// that still boots, and the smallest that must not.
	//
	// RED: change claimStaleMargin without re-deriving, and these two fail
	// while every fixed-value row above keeps passing.
	lastValid := staleTaskThreshold - claimStaleMargin - time.Second
	if err := ValidateClaimTimings(lastValid); err != nil {
		t.Errorf("an apply budget of %v leaves the bound just under the reaper threshold %v and must boot, got %v",
			lastValid, staleTaskThreshold, err)
	}
	firstInvalid := staleTaskThreshold - claimStaleMargin
	if err := ValidateClaimTimings(firstInvalid); err == nil {
		t.Errorf("an apply budget of %v puts the bound exactly at the reaper threshold %v and must be refused",
			firstInvalid, staleTaskThreshold)
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

// TestYieldContendedClaim_PastBound_IsLoudAndNeverWrites pins BOTH halves of
// the contended path's wall-clock bound.
//
// It must be bounded: before the bound existed, a contended claim could snooze
// forever with no error, no attempt consumed and no terminal state, and the
// unreadable-row branch could do it on nothing but a Warn.
//
// It must ALSO never write. The first attempt at the bound terminalized the
// task as TaskSkipped from the pre-claim snapshot. FinishUpdateTask accepts
// 'pending' and 'running' alike, so that skip lands on the row the WINNER is
// actively dispatching, the winner's real result is then rejected as
// ErrTaskNotOpen, and the permanent record says "nothing was sent to the site"
// about a site that was updated. A worker that never got the claim has no
// outcome to record.
//
// RED: restore the terminalizing form (w.finish(..., TaskSkipped, ...) ahead of
// the re-read) and both subtests fail on the write assertions.
func TestYieldContendedClaim_PastBound_IsLoudAndNeverWrites(t *testing.T) {
	cases := map[string]func(*probeFakeRepo){
		// The winner holds the row and is mid-dispatch right now.
		"row held by the winner": func(r *probeFakeRepo) {
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

			// Loud: an error, which consumes an attempt and eventually
			// dead-letters. Not nil (silently dropped) and not a snooze
			// (invisible forever).
			if err == nil {
				t.Fatal("a task past its wall-clock bound must fail loudly, got nil")
			}
			if _, ok := asSnooze(err); ok {
				t.Fatalf("past the bound this must stop snoozing, got %v", err)
			}
			// And never a write of any kind.
			if len(repo.finished) != 0 {
				t.Fatalf("a worker that never got the claim must never record an outcome; "+
					"this would overwrite the winner's live row and report the update as not attempted, got %+v",
					repo.finished)
			}
			if cmd.updateCalls != 0 {
				t.Fatalf("a task that never won the claim must never dispatch, got %d", cmd.updateCalls)
			}
		})
	}
}

// TestContendedClaim_LoserNeverOverwritesTheWinnersOutcome is the race stated
// end to end, on one shared row, past siteBusyMaxWait: a winner that claims and
// finishes, and a loser that is refused. What must be recorded is the WINNER's
// result. The loser writing its own verdict is the defect, and it is worse than
// a lost update: the site IS changed and the record says it was not.
//
// RED: restore the terminalizing form and the recorded status is "skipped".
func TestContendedClaim_LoserNeverOverwritesTheWinnersOutcome(t *testing.T) {
	task := claimTestTask()
	task.CreatedAt = time.Now().Add(-siteBusyMaxWait - time.Minute) // past the bound
	repo := claimRepo(task)

	// One shared row both workers act on.
	row := task
	var claimed bool
	repo.getTask = func(uuid.UUID, uuid.UUID) (Task, error) { return row, nil }
	repo.markRunning = func(_, _ uuid.UUID, _ time.Duration) (Task, error) {
		if claimed {
			return Task{}, ErrTaskNotClaimed // the loser
		}
		claimed = true
		row.Status = TaskRunning // the winner now holds it
		return row, nil
	}
	// FinishTask on this fake mirrors FinishUpdateTask's precondition: it
	// writes while the row is OPEN (pending|running) and refuses afterwards.
	// That precondition is exactly why the loser must not call it at all.
	repo.finishTask = func(in FinishTaskInput) (Task, error) {
		if terminal(row.Status) {
			return row, ErrTaskNotOpen
		}
		row.Status = in.Status
		row.Detail = in.Detail
		return row, nil
	}

	w, _ := claimTestWorker(repo, task)
	args := TaskArgs{TenantID: task.TenantID, RunID: task.RunID, TaskID: task.ID}

	// The winner claims.
	if _, err := repo.MarkTaskRunning(context.Background(), task.TenantID, task.ID, w.claimStaleAfter); err != nil {
		t.Fatalf("the winner must get the claim: %v", err)
	}
	// The loser is refused and yields.
	loserErr := w.yieldContendedClaim(context.Background(), args, task)
	// The winner then records its real outcome.
	winnerErr := w.finish(context.Background(), row, TaskSucceeded, "1.9.9", "2.0.0", "updated", "")

	if winnerErr != nil {
		t.Fatalf("the winner's outcome must be recordable after the loser yields, got %v", winnerErr)
	}
	if row.Status != TaskSucceeded {
		t.Fatalf("recorded status = %q, want %q: the loser overwrote the winner's live row, "+
			"so the site was updated while the record says it was not", row.Status, TaskSucceeded)
	}
	if strings.Contains(row.Detail, "Nothing was sent to the site") {
		t.Fatalf("the record claims nothing was sent to the site, but the winner updated it: %q", row.Detail)
	}
	if loserErr == nil {
		t.Fatal("past the bound the loser must still fail loudly rather than return nil")
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

	repo := claimRepo(task)
	w, cmd := claimTestWorker(repo, task)
	// Age the row relative to the worker's OWN derived bound, not a constant,
	// so this test keeps meaning what it says under any configuration.
	age := w.claimStaleAfter + 5*time.Minute
	repo.markRunning = func(_, _ uuid.UUID, staleAfter time.Duration) (Task, error) {
		// Mirrors the real statement's 'running' arm:
		// coalesce(started_at, updated_at) < now() - staleAfter.
		//
		// Note what a NON-POSITIVE bound does here — it matches EVERYTHING,
		// because durationToInterval yields interval '0' and never NULL. That
		// is the fail-OPEN direction. An earlier version of this fake encoded
		// `staleAfter <= 0` as "matches nothing", which modelled the opposite
		// of the real statement in the direction that hides the danger.
		// pgRepo.MarkTaskRunning refuses a non-positive bound outright, so
		// production cannot reach this, but the double must not lie about it.
		if age <= staleAfter {
			return Task{}, ErrTaskNotClaimed
		}
		running := task
		running.Status = TaskRunning
		return running, nil
	}

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
