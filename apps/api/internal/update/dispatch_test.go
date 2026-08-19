package update

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// GH #463 — the create-time half of deferred dispatch.
//
// Every test here fails on the pre-#463 code for the same reason: CreateRun
// never read ScheduledAt, so the schedule branch did not exist, the run came
// back 'pending', and one per-task job went out immediately. Deleting the
// `if deferred` block from Service.CreateRun reddens all four.

func scheduleFixture(t *testing.T) (uuid.UUID, uuid.UUID, *fakeSiteLookup) {
	t.Helper()
	tenant := uuid.New()
	site := uuid.New()
	lookup := &fakeSiteLookup{sites: map[uuid.UUID]SiteInfo{
		site: {
			ID:       site,
			Enrolled: true,
			Components: []Component{
				{Type: TargetPlugin, Slug: "akismet", Version: "5.0.0", UpdateAvailable: true, NewVersion: "5.1.0"},
			},
		},
	}}
	return tenant, site, lookup
}

// TestCreateRunDefersFutureSchedule is the issue's headline acceptance
// criterion at the unit level: a run scheduled 10 minutes out contacts no site
// until its time.
//
// "Contacts no site" is asserted as "zero per-task jobs were enqueued", which
// is the only place the control plane could have started talking to a site
// from: Worker.Work is reached exclusively through a TaskArgs job, so no job
// means no command, no matter what any other layer does.
func TestCreateRunDefersFutureSchedule(t *testing.T) {
	tenant, site, lookup := scheduleFixture(t)
	repo := &fakeCreateRepo{tenantID: tenant}
	tasksEnq := &countingEnqueuer{}
	dispatchEnq := &fakeDispatchEnqueuer{}

	svc := NewService(repo, lookup, tasksEnq, domain.NewValidator(), domain.SystemClock{})
	svc.SetDispatchEnqueuer(dispatchEnq)

	at := time.Now().Add(10 * time.Minute)
	run, tasks, err := svc.CreateRun(context.Background(), CreateRunInput{
		TenantID:    tenant,
		SiteIDs:     []uuid.UUID{site},
		Items:       []Item{{Type: TargetPlugin, Slug: "akismet", Version: "latest"}},
		ScheduledAt: &at,
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	if run.Status != RunScheduled {
		t.Errorf("run status = %q, want %q", run.Status, RunScheduled)
	}
	if len(tasks) != 1 {
		t.Fatalf("got %d tasks, want 1", len(tasks))
	}
	if tasks[0].Status != TaskScheduled {
		t.Errorf("task status = %q, want %q", tasks[0].Status, TaskScheduled)
	}

	// The load-bearing assertion. A non-zero count here IS the reported defect.
	if tasksEnq.n != 0 {
		t.Errorf("enqueued %d task jobs for a future-scheduled run, want 0: the fleet would update now", tasksEnq.n)
	}

	if len(dispatchEnq.calls) != 1 {
		t.Fatalf("got %d dispatch jobs, want exactly 1", len(dispatchEnq.calls))
	}
	got := dispatchEnq.calls[0]
	if got.runID != run.ID || got.tenantID != tenant {
		t.Errorf("dispatch job targets run %s tenant %s, want %s / %s", got.runID, got.tenantID, run.ID, tenant)
	}
	if !got.at.Equal(at) {
		t.Errorf("dispatch job scheduled at %s, want the run's own scheduled_at %s", got.at, at)
	}
}

// TestCreateRunImmediateWhenScheduleAbsentOrNow proves the branch did not
// capture the paths it was not meant to: a nil schedule, and a schedule that is
// effectively now, both take the original immediate path with a task job out
// per task.
//
// This is the over-fire half. A deferral that also swallowed ordinary
// immediate runs would stop the fleet updating at all, which is a worse bug
// than the one being fixed.
func TestCreateRunImmediateWhenScheduleAbsentOrNow(t *testing.T) {
	now := time.Now()
	past := now.Add(-30 * time.Second) // inside scheduleSkewGrace: a browser clock behind ours.
	soon := now.Add(20 * time.Second)  // inside scheduleMinLead: deferral would buy nothing.

	cases := []struct {
		name string
		at   *time.Time
	}{
		{"nil schedule", nil},
		{"a few seconds in the past (clock skew)", &past},
		{"under the minimum lead", &soon},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tenant, site, lookup := scheduleFixture(t)
			repo := &fakeCreateRepo{tenantID: tenant}
			tasksEnq := &countingEnqueuer{}
			dispatchEnq := &fakeDispatchEnqueuer{}

			svc := NewService(repo, lookup, tasksEnq, domain.NewValidator(), fixedClock{t: now})
			svc.SetDispatchEnqueuer(dispatchEnq)

			run, tasks, err := svc.CreateRun(context.Background(), CreateRunInput{
				TenantID:    tenant,
				SiteIDs:     []uuid.UUID{site},
				Items:       []Item{{Type: TargetPlugin, Slug: "akismet", Version: "latest"}},
				ScheduledAt: tc.at,
			})
			if err != nil {
				t.Fatalf("CreateRun: %v", err)
			}
			if run.Status != RunPending {
				t.Errorf("run status = %q, want %q", run.Status, RunPending)
			}
			if tasksEnq.n != len(tasks) {
				t.Errorf("enqueued %d task jobs for %d tasks, want one each", tasksEnq.n, len(tasks))
			}
			if len(dispatchEnq.calls) != 0 {
				t.Errorf("enqueued %d dispatch jobs for an immediate run, want 0", len(dispatchEnq.calls))
			}
		})
	}
}

// TestCreateRunRefusesUnhonourableSchedules proves both bounds are refused at
// create time, while the operator is still there to fix them, rather than
// asynchronously as a run that quietly expired.
func TestCreateRunRefusesUnhonourableSchedules(t *testing.T) {
	now := time.Now()
	longPast := now.Add(-24 * time.Hour)
	tooFar := now.Add(31 * 24 * time.Hour)

	cases := []struct {
		name string
		at   time.Time
		code string
	}{
		{"a time in the past", longPast, "schedule_in_past"},
		{"more than 30 days out", tooFar, "schedule_too_far"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tenant, site, lookup := scheduleFixture(t)
			tasksEnq := &countingEnqueuer{}
			dispatchEnq := &fakeDispatchEnqueuer{}
			svc := NewService(&fakeCreateRepo{tenantID: tenant}, lookup, tasksEnq, domain.NewValidator(), fixedClock{t: now})
			svc.SetDispatchEnqueuer(dispatchEnq)

			at := tc.at
			_, _, err := svc.CreateRun(context.Background(), CreateRunInput{
				TenantID:    tenant,
				SiteIDs:     []uuid.UUID{site},
				Items:       []Item{{Type: TargetPlugin, Slug: "akismet", Version: "latest"}},
				ScheduledAt: &at,
			})
			if err == nil {
				t.Fatal("CreateRun accepted a schedule it cannot honour")
			}
			de, ok := domain.AsDomain(err)
			if !ok {
				t.Fatalf("error is not a domain error: %v", err)
			}
			if de.Code != tc.code {
				t.Errorf("error code = %q, want %q", de.Code, tc.code)
			}
			// Nothing may have gone out on a refused schedule.
			if tasksEnq.n != 0 || len(dispatchEnq.calls) != 0 {
				t.Errorf("a refused schedule enqueued %d task jobs and %d dispatch jobs, want 0 and 0",
					tasksEnq.n, len(dispatchEnq.calls))
			}
		})
	}
}

// TestScheduledTaskIsNotTerminalButExpiredIs pins the asymmetry terminal()
// rests on. Getting it backwards is silent in both directions: calling
// 'scheduled' terminal makes Worker.Work discard a task whose run is still
// ahead of it, and leaving 'expired' non-terminal lets a worker try to run a
// task belonging to a run that was abandoned hours ago.
func TestScheduledTaskIsNotTerminalButExpiredIs(t *testing.T) {
	if terminal(TaskScheduled) {
		t.Error("terminal(TaskScheduled) = true; a scheduled task has not been attempted YET")
	}
	if !terminal(TaskExpired) {
		t.Error("terminal(TaskExpired) = false; an expired task will never be attempted")
	}
	if TaskExpired == TaskCancelled {
		t.Error("TaskExpired and TaskCancelled are the same string; they record different facts")
	}
}

// TestExpiredTaskIsRetryableAsNeverRan pins the retry contract for the status
// this branch introduced.
//
// Making a status terminal silently changes retryClassify, because its default
// arm is TaskSucceeded's: "retrying is not meaningful, the work is done". For an
// expired task that is exactly backwards — nothing was sent to the site, nothing
// on the site changed, and the only reason it did not run is that the window
// passed while the control plane was down. Inheriting the default would have
// offered no retry on the one outcome an operator most wants back.
//
// TaskCancelled is asserted alongside it as the control: the two share a class
// because both were never attempted, and the test would catch either drifting
// away from it.
func TestExpiredTaskIsRetryableAsNeverRan(t *testing.T) {
	retryable, class := retryClassify(TaskExpired)
	if !retryable {
		t.Error("an expired task offers no retry; it was never attempted, so it is the lowest-risk thing a retry can pick up")
	}
	if class != RetryClassNeverRan {
		t.Errorf("retry class for %q = %q, want %q", TaskExpired, class, RetryClassNeverRan)
	}
	if class == RetryClassNotApplicable {
		t.Error("expired fell through to the default arm, which exists for 'succeeded' — the opposite case")
	}

	// The control: cancelled shares the class for the same reason.
	if cRetryable, cClass := retryClassify(TaskCancelled); !cRetryable || cClass != RetryClassNeverRan {
		t.Errorf("retryClassify(%q) = (%v, %q), want (true, %q)", TaskCancelled, cRetryable, cClass, RetryClassNeverRan)
	}
	// And the anti-control: succeeded must NOT have become retryable.
	if sRetryable, _ := retryClassify(TaskSucceeded); sRetryable {
		t.Error("a succeeded task became retryable; the default arm has been widened past its purpose")
	}
}
