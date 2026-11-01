package report

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/mosamlife/wpmgr/apps/api/internal/report/render/html"
	"github.com/mosamlife/wpmgr/apps/api/internal/report/render/pdf"
	"github.com/mosamlife/wpmgr/apps/api/internal/report/reportdata"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

// The final attempt of a report job must leave the report row finished. A
// panic skipped every FailReport branch of Work, so the row stayed
// 'generating', and GenerateNow's in-flight guard then refused every later
// "Generate now" for the client.

// panicRenderer is a PDF renderer that panics, standing in for any bug in the
// renderer or the library under it.
type panicRenderer struct{}

func (panicRenderer) Render(reportdata.ReportData, []byte) ([]byte, error) {
	panic("renderer exploded")
}

// recordingPublisher keeps the status of every report.completed event.
type recordingPublisher struct{ statuses []string }

func (p *recordingPublisher) Publish(_ context.Context, ev site.ConnectionEvent) error {
	if s, ok := ev.Data["status"].(string); ok {
		p.statuses = append(p.statuses, s)
	}
	return nil
}

// failRecorder is the repo the worker fails reports through, remembering what
// the context looked like when it did.
type failRecorder struct {
	*snapshotRepo
	failCalls  int
	failCtxErr error
}

func (r *failRecorder) FailReport(ctx context.Context, tenantID, id uuid.UUID, msg string) (GeneratedReport, error) {
	r.failCalls++
	r.failCtxErr = ctx.Err()
	return r.snapshotRepo.FailReport(ctx, tenantID, id, msg)
}

func finalAttemptWorker(t *testing.T, repo *failRecorder, src Sources, pdfR pdf.Renderer) (*GenerateWorker, *recordingPublisher) {
	t.Helper()
	svc := NewService(repo, &capturingBlob{})
	pub := &recordingPublisher{}
	svc.SetPublisher(pub)
	htmlR, err := html.NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	return NewGenerateWorker(repo, svc, src, htmlR, pdfR, nil, nil), pub
}

func newFailRecorder(getSchedule func(context.Context, uuid.UUID, uuid.UUID) (Schedule, bool, error)) *failRecorder {
	return &failRecorder{snapshotRepo: &snapshotRepo{stubRepo: &stubRepo{
		getSchedule:   getSchedule,
		getTenantName: func(context.Context, uuid.UUID) (string, error) { return "Agency", nil },
	}}}
}

func oneSiteSources() Sources {
	return Sources{
		ListClientSites: func(context.Context, uuid.UUID, uuid.UUID) ([]site.Site, error) {
			return []site.Site{{ID: uuid.New(), Name: "site-a", URL: "https://a.example"}}, nil
		},
	}
}

func generateJob(attempt int) *river.Job[GenerateArgs] {
	return &river.Job[GenerateArgs]{
		JobRow: &rivertype.JobRow{Attempt: attempt, MaxAttempts: 3},
		Args: GenerateArgs{
			TenantID: uuid.New(), ClientID: uuid.New(), ReportID: uuid.New(),
			PeriodStart: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
			PeriodEnd:   time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		},
	}
}

// workCatching runs Work and reports a panic instead of crashing the test
// binary.
func workCatching(w *GenerateWorker, ctx context.Context, job *river.Job[GenerateArgs]) (err error, panicked any) {
	defer func() { panicked = recover() }()
	err = w.Work(ctx, job)
	return err, nil
}

func TestWorkFinalAttemptPanicFailsTheReport(t *testing.T) {
	panicSites := oneSiteSources()
	panicSites.ListClientSites = func(context.Context, uuid.UUID, uuid.UUID) ([]site.Site, error) {
		panic("sites exploded")
	}
	cases := map[string]struct {
		src  Sources
		pdfR pdf.Renderer
	}{
		"renderer panics": {src: oneSiteSources(), pdfR: panicRenderer{}},
		"source panics":   {src: panicSites, pdfR: panicRenderer{}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			repo := newFailRecorder(nil)
			w, pub := finalAttemptWorker(t, repo, c.src, c.pdfR)
			err, p := workCatching(w, context.Background(), generateJob(3))
			if p != nil {
				t.Fatalf("Work let a panic escape: %v", p)
			}
			if err == nil {
				t.Fatal("Work returned nil after a panic")
			}
			if repo.failCalls != 1 {
				t.Fatalf("FailReport called %d times on the final attempt, want 1", repo.failCalls)
			}
			if !strings.Contains(repo.failed, "exploded") {
				t.Errorf("the failed row's error %q does not say what happened", repo.failed)
			}
			if got := strings.Join(pub.statuses, ","); got != StatusFailed {
				t.Errorf("published statuses %q, want one %q", got, StatusFailed)
			}
			if repo.snapshot != nil {
				t.Error("a panicked job completed the report")
			}
		})
	}
}

// An earlier attempt that panics is retried by River; it must not mark the
// report failed while attempts remain.
func TestWorkEarlierAttemptPanicIsRetriedNotFailed(t *testing.T) {
	for _, attempt := range []int{1, 2} {
		repo := newFailRecorder(nil)
		w, pub := finalAttemptWorker(t, repo, oneSiteSources(), panicRenderer{})
		err, p := workCatching(w, context.Background(), generateJob(attempt))
		if p != nil {
			t.Fatalf("attempt %d: Work let a panic escape: %v", attempt, p)
		}
		if err == nil {
			t.Fatalf("attempt %d: Work returned nil after a panic, so River would not retry", attempt)
		}
		if repo.failCalls != 0 || len(pub.statuses) != 0 {
			t.Errorf("attempt %d of 3 marked the report failed (calls=%d, events=%v)", attempt, repo.failCalls, pub.statuses)
		}
	}
}

// A job that ran out of time has a cancelled context, and the update that
// fails the row used the same one, so it failed too and the row stayed
// 'generating'.
func TestWorkFinalAttemptFailsTheReportAfterTheJobTimedOut(t *testing.T) {
	repo := newFailRecorder(func(ctx context.Context, _, _ uuid.UUID) (Schedule, bool, error) {
		<-ctx.Done()
		return Schedule{}, false, ctx.Err()
	})
	w, pub := finalAttemptWorker(t, repo, oneSiteSources(), panicRenderer{})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err, p := workCatching(w, ctx, generateJob(3))
	if p != nil {
		t.Fatalf("Work panicked: %v", p)
	}
	if err == nil {
		t.Fatal("Work returned nil for a job that timed out")
	}
	if repo.failCalls != 1 {
		t.Fatalf("FailReport called %d times, want 1", repo.failCalls)
	}
	if repo.failCtxErr != nil {
		t.Errorf("FailReport ran on a finished context (%v), so the update would not have been written", repo.failCtxErr)
	}
	if got := strings.Join(pub.statuses, ","); got != StatusFailed {
		t.Errorf("published statuses %q, want one %q", got, StatusFailed)
	}
}

// Every other way a final attempt fails still marks the report failed exactly
// once.
func TestWorkFinalAttemptErrorFailsTheReportOnce(t *testing.T) {
	repo := newFailRecorder(func(context.Context, uuid.UUID, uuid.UUID) (Schedule, bool, error) {
		return Schedule{}, false, context.DeadlineExceeded
	})
	w, pub := finalAttemptWorker(t, repo, oneSiteSources(), panicRenderer{})
	err, p := workCatching(w, context.Background(), generateJob(3))
	if p != nil || err == nil {
		t.Fatalf("Work: err=%v panic=%v", err, p)
	}
	if repo.failCalls != 1 || len(pub.statuses) != 1 {
		t.Errorf("FailReport calls=%d, events=%v; want one of each", repo.failCalls, pub.statuses)
	}
}
