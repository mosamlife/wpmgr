package report

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// A report row nothing has touched for longer than every attempt of its job
// could have run is dead: the job died without finishing the row. The
// in-flight guard in GenerateNow used to return it for ever, so "Generate now"
// was refused for that client until someone deleted the row by hand.

type staleGuardResult struct {
	got       GeneratedReport
	err       error
	failedIDs []uuid.UUID
	created   int
	enqueued  int
	newID     uuid.UUID
}

func runGenerateNowWithActive(t *testing.T, active GeneratedReport) staleGuardResult {
	t.Helper()
	var res staleGuardResult
	res.newID = uuid.New()
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
	enq := &fakeEnqueuer{}
	svc.SetEnqueuer(enq)
	res.got, res.err = svc.GenerateNow(context.Background(), uuid.New(), uuid.New(), GenerateNowInput{})
	res.enqueued = len(enq.calls)
	return res
}

func TestGenerateNowTreatsAReportNothingHasTouchedForEveryAttemptAsFailed(t *testing.T) {
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
		})
	}
}

// The other side: a report whose job is still within its attempts is returned
// as before and nothing is failed or created.
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
