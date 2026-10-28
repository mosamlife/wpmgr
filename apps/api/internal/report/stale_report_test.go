package report

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// A report row nothing has touched for longer than every attempt of its job
// could have run is dead when no job for it is still queued or running: the job
// died without finishing the row. The in-flight guard in GenerateNow used to
// return it for ever, so "Generate now" was refused for that client until
// someone deleted the row by hand. A job still waiting in the queue has touched
// nothing either, and its report is not dead.

type staleGuardResult struct {
	got       GeneratedReport
	err       error
	failedIDs []uuid.UUID
	created   int
	enqueued  int
	newID     uuid.UUID
	tenantID  uuid.UUID
	// asked is every report GenerateNow asked the queue about.
	asked []activeJobQuery
}

// runGenerateNowWithActive runs GenerateNow over an in-flight report that has no
// job in the queue.
func runGenerateNowWithActive(t *testing.T, active GeneratedReport) staleGuardResult {
	t.Helper()
	return runGenerateNowWithActiveJob(t, active, &fakeEnqueuer{})
}

// runGenerateNowWithActiveJob runs GenerateNow over an in-flight report, with
// enq standing for the queue.
func runGenerateNowWithActiveJob(t *testing.T, active GeneratedReport, enq *fakeEnqueuer) staleGuardResult {
	t.Helper()
	var res staleGuardResult
	res.newID = uuid.New()
	res.tenantID = uuid.New()
	repo := &stubRepo{
		getActiveReport: func(context.Context, uuid.UUID, uuid.UUID) (GeneratedReport, bool, error) {
			return active, true, nil
		},
		failReport: func(_ context.Context, _, id uuid.UUID, _ string) (GeneratedReport, error) {
			res.failedIDs = append(res.failedIDs, id)
			return GeneratedReport{ID: id, Status: StatusFailed}, nil
		},
		createReport: func(_ context.Context, in CreateReportInput) (GeneratedReport, error) {
			res.created++
			return GeneratedReport{ID: res.newID, TenantID: in.TenantID, ClientID: in.ClientID, Status: StatusPending}, nil
		},
	}
	svc := NewService(repo, &noopBlobStorer{})
	svc.SetEnqueuer(enq)
	res.got, res.err = svc.GenerateNow(context.Background(), res.tenantID, uuid.New(), GenerateNowInput{})
	res.enqueued = len(enq.calls)
	res.asked = enq.asked
	return res
}

func TestGenerateNowTreatsAStaleReportWithNoJobLeftAsFailed(t *testing.T) {
	for _, status := range []string{StatusGenerating, StatusPending} {
		t.Run(status, func(t *testing.T) {
			dead := GeneratedReport{
				ID:        uuid.New(),
				Status:    status,
				UpdatedAt: time.Now().Add(-(staleReportAfter + time.Minute)),
			}
			res := runGenerateNowWithActive(t, dead)
			if res.err != nil {
				t.Fatalf("GenerateNow: %v", res.err)
			}
			if res.got.ID == dead.ID {
				t.Fatal("GenerateNow returned the dead report, so a new request is blocked for ever")
			}
			if res.got.ID != res.newID || res.created != 1 || res.enqueued != 1 {
				t.Errorf("new report id=%s created=%d enqueued=%d, want the new report created and enqueued once", res.got.ID, res.created, res.enqueued)
			}
			if len(res.failedIDs) != 1 || res.failedIDs[0] != dead.ID {
				t.Errorf("failed ids %v, want the dead report %s marked failed", res.failedIDs, dead.ID)
			}
			// The queue was asked about this tenant's report, not about any other.
			if want := (activeJobQuery{tenantID: res.tenantID, reportID: dead.ID}); len(res.asked) != 1 || res.asked[0] != want {
				t.Errorf("the queue was asked about %v, want %v", res.asked, want)
			}
		})
	}
}

// A job still waiting in a busy queue has touched nothing, so its row looks as
// untouched as a dead job's. While any job for the report is queued or running
// it is not dead: it is returned like a fresh one, and nothing is failed,
// created or queued. Failing it would leave a scheduled report's client without
// the email, because the replacement does not notify.
func TestGenerateNowKeepsAStaleReportWhoseJobIsStillQueued(t *testing.T) {
	for _, status := range []string{StatusPending, StatusGenerating} {
		t.Run(status, func(t *testing.T) {
			queued := GeneratedReport{
				ID:        uuid.New(),
				Status:    status,
				UpdatedAt: time.Now().Add(-(staleReportAfter + time.Minute)),
			}
			// Another report's job is queued as well: only this report's counts.
			enq := &fakeEnqueuer{active: map[uuid.UUID]bool{queued.ID: true, uuid.New(): true}}
			res := runGenerateNowWithActiveJob(t, queued, enq)
			if res.err != nil {
				t.Fatalf("GenerateNow: %v", res.err)
			}
			if res.got.ID != queued.ID {
				t.Errorf("GenerateNow returned %s, want the report %s whose job is still queued", res.got.ID, queued.ID)
			}
			if res.created != 0 || res.enqueued != 0 || len(res.failedIDs) != 0 {
				t.Errorf("created=%d enqueued=%d failed=%v; a report whose job is queued must be left alone", res.created, res.enqueued, res.failedIDs)
			}
			if want := (activeJobQuery{tenantID: res.tenantID, reportID: queued.ID}); len(res.asked) != 1 || res.asked[0] != want {
				t.Errorf("the queue was asked about %v, want %v", res.asked, want)
			}
		})
	}
}

// When the queue cannot be asked, a job that is only waiting cannot be told from
// one that died. The row is left as it is and the request fails, so a retry can
// decide; failing the row on a guess is how a client loses a report.
func TestGenerateNowLeavesAStaleReportAloneWhenTheQueueCannotBeAsked(t *testing.T) {
	stale := GeneratedReport{
		ID:        uuid.New(),
		Status:    StatusPending,
		UpdatedAt: time.Now().Add(-(staleReportAfter + time.Minute)),
	}
	cause := errors.New("river: connection refused")
	res := runGenerateNowWithActiveJob(t, stale, &fakeEnqueuer{activeErr: cause})
	if res.err == nil {
		t.Fatalf("GenerateNow returned %s and no error, want the request to fail", res.got.ID)
	}
	de, ok := domain.AsDomain(res.err)
	if !ok || de.Kind != domain.KindInternal || de.Code != "report_job_check_failed" {
		t.Errorf("error = %v, want a domain internal error with code report_job_check_failed", res.err)
	}
	if !errors.Is(res.err, cause) {
		t.Errorf("error %v does not carry the cause %v", res.err, cause)
	}
	if res.created != 0 || res.enqueued != 0 || len(res.failedIDs) != 0 {
		t.Errorf("created=%d enqueued=%d failed=%v; a row whose job could not be checked must be left alone", res.created, res.enqueued, res.failedIDs)
	}
}

// With no queue wired, nothing has been queued, so a stale row has no job.
func TestGenerateNowReplacesAStaleReportWhenNoQueueIsWired(t *testing.T) {
	stale := GeneratedReport{
		ID:        uuid.New(),
		Status:    StatusPending,
		UpdatedAt: time.Now().Add(-(staleReportAfter + time.Minute)),
	}
	var failed []uuid.UUID
	repo := &stubRepo{
		getActiveReport: func(context.Context, uuid.UUID, uuid.UUID) (GeneratedReport, bool, error) {
			return stale, true, nil
		},
		failReport: func(_ context.Context, _, id uuid.UUID, _ string) (GeneratedReport, error) {
			failed = append(failed, id)
			return GeneratedReport{ID: id, Status: StatusFailed}, nil
		},
	}
	got, err := NewService(repo, &noopBlobStorer{}).GenerateNow(context.Background(), uuid.New(), uuid.New(), GenerateNowInput{})
	if err != nil {
		t.Fatalf("GenerateNow: %v", err)
	}
	if got.ID == stale.ID || len(failed) != 1 || failed[0] != stale.ID {
		t.Errorf("returned %s, failed %v; want the stale report %s failed and a new one returned", got.ID, failed, stale.ID)
	}
}

// The other side: a report whose job is still within its attempts is returned
// as before, nothing is failed or created, and the queue is not even asked.
func TestGenerateNowKeepsReturningAReportWhoseJobCanStillBeRunning(t *testing.T) {
	cases := map[string]time.Duration{
		"just touched":         time.Second,
		"one attempt ago":      generateJobTimeout,
		"almost every attempt": staleReportAfter - 10*time.Second,
	}
	for name, age := range cases {
		t.Run(name, func(t *testing.T) {
			live := GeneratedReport{ID: uuid.New(), Status: StatusGenerating, UpdatedAt: time.Now().Add(-age)}
			res := runGenerateNowWithActive(t, live)
			if res.err != nil {
				t.Fatalf("GenerateNow: %v", res.err)
			}
			if res.got.ID != live.ID {
				t.Errorf("GenerateNow returned %s, want the running report %s", res.got.ID, live.ID)
			}
			if res.created != 0 || res.enqueued != 0 || len(res.failedIDs) != 0 {
				t.Errorf("created=%d enqueued=%d failed=%v; a running report must be left alone", res.created, res.enqueued, res.failedIDs)
			}
			if len(res.asked) != 0 {
				t.Errorf("the queue was asked about %v for a report that is not stale", res.asked)
			}
		})
	}
}

// A row with no recorded update time is not evidence of anything, so it is
// treated as running rather than failed.
func TestGenerateNowDoesNotFailAReportWithoutAnUpdateTime(t *testing.T) {
	res := runGenerateNowWithActive(t, GeneratedReport{ID: uuid.New(), Status: StatusGenerating})
	if res.created != 0 || len(res.failedIDs) != 0 {
		t.Errorf("created=%d failed=%v for a row with a zero update time", res.created, res.failedIDs)
	}
	if len(res.asked) != 0 {
		t.Errorf("the queue was asked about %v for a row that is not stale", res.asked)
	}
}

// The window is the job's own limits multiplied out, so a change to either
// moves it.
func TestStaleReportWindowIsTimeoutTimesAttempts(t *testing.T) {
	if want := generateJobTimeout * time.Duration(generateMaxAttempts); staleReportAfter != want {
		t.Errorf("staleReportAfter = %s, want %s", staleReportAfter, want)
	}
	w := &GenerateWorker{}
	if got := w.Timeout(nil); got != generateJobTimeout {
		t.Errorf("worker timeout %s differs from generateJobTimeout %s", got, generateJobTimeout)
	}
	if got := (GenerateArgs{}).InsertOpts().MaxAttempts; got != generateMaxAttempts {
		t.Errorf("job MaxAttempts %d differs from generateMaxAttempts %d", got, generateMaxAttempts)
	}
}
