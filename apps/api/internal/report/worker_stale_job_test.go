package report

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/report/render/html"
	"github.com/mosamlife/wpmgr/apps/api/internal/report/render/pdf"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

// A report job can start long after GenerateNow gave up on it. A queue that was
// only backed up leaves the row 'pending' with no update for longer than
// staleReportAfter, so the next "Generate now" fails that row and queues a new
// report; the first job then runs when the queue drains. It must find its row
// is no longer in flight and stop. With nothing checking, the attempt marked the
// failed row 'generating' again, completed it, and told the client a second
// time.

// lifecycleRepo serves the report row an attempt reads and counts every write
// the attempt makes to it.
type lifecycleRepo struct {
	*stubRepo
	row GeneratedReport

	reads, marked, completed, failed int
}

func (r *lifecycleRepo) GetReport(_ context.Context, tenantID, clientID, id uuid.UUID) (GeneratedReport, error) {
	r.reads++
	row := r.row
	row.ID, row.TenantID, row.ClientID = id, tenantID, clientID
	return row, nil
}

func (r *lifecycleRepo) MarkGenerating(context.Context, uuid.UUID, uuid.UUID) (GeneratedReport, error) {
	r.marked++
	return GeneratedReport{}, nil
}

func (r *lifecycleRepo) CompleteReport(context.Context, uuid.UUID, uuid.UUID, string, string, []byte) (GeneratedReport, error) {
	r.completed++
	return GeneratedReport{}, nil
}

func (r *lifecycleRepo) FailReport(_ context.Context, _, id uuid.UUID, _ string) (GeneratedReport, error) {
	r.failed++
	return GeneratedReport{ID: id, Status: StatusFailed}, nil
}

// recordingMailer keeps the template of every email the report service queues.
type recordingMailer struct{ templates []string }

func (m *recordingMailer) Enqueue(_ context.Context, _ uuid.UUID, _ []string, template string, _ map[string]any) error {
	m.templates = append(m.templates, template)
	return nil
}

// mailerEnabled is a configured instance mailer.
type mailerEnabled struct{}

func (mailerEnabled) Enabled(context.Context) bool { return true }

type attemptOutcome struct {
	err       error
	repo      *lifecycleRepo
	events    []string // status of every report.completed event published
	emails    []string // template of every email queued
	uploads   int
	siteReads int
	logs      string
}

// runAttemptAgainstRow runs one attempt of a notifying report job against a
// report row in the given status.
func runAttemptAgainstRow(t *testing.T, status string, attempt int) attemptOutcome {
	t.Helper()
	repo := &lifecycleRepo{
		stubRepo: &stubRepo{
			getSchedule: func(context.Context, uuid.UUID, uuid.UUID) (Schedule, bool, error) {
				return Schedule{Recipients: []string{"client@example.com"}}, true, nil
			},
			getTenantName: func(context.Context, uuid.UUID) (string, error) { return "Agency", nil },
		},
		row: GeneratedReport{Status: status},
	}
	blob := &capturingBlob{}
	svc := NewService(repo, blob)
	pub := &recordingPublisher{}
	svc.SetPublisher(pub)
	mail := &recordingMailer{}
	svc.SetMailer(mail)
	svc.SetMailerStatus(mailerEnabled{})

	htmlR, err := html.NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	var siteReads int
	src := oneSiteSources()
	listSites := src.ListClientSites
	src.ListClientSites = func(ctx context.Context, tenantID, clientID uuid.UUID) ([]site.Site, error) {
		siteReads++
		return listSites(ctx, tenantID, clientID)
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	w := NewGenerateWorker(repo, svc, src, htmlR, pdf.NewFpdfRenderer(), nil, logger)

	job := generateJob(attempt)
	job.Args.Notify = true
	err, p := workCatching(w, context.Background(), job)
	if p != nil {
		t.Fatalf("Work panicked: %v", p)
	}
	return attemptOutcome{
		err:       err,
		repo:      repo,
		events:    pub.statuses,
		emails:    mail.templates,
		uploads:   len(blob.objects),
		siteReads: siteReads,
		logs:      logs.String(),
	}
}

func TestWorkEndsWithoutWorkWhenTheReportIsNoLongerInFlight(t *testing.T) {
	// failed: GenerateNow gave up on the row. completed: the report was already
	// finished (a job River runs again after a crash).
	for _, status := range []string{StatusFailed, StatusCompleted} {
		for _, attempt := range []int{1, 2, 3} {
			t.Run(fmt.Sprintf("%s row, attempt %d", status, attempt), func(t *testing.T) {
				o := runAttemptAgainstRow(t, status, attempt)

				if o.err != nil {
					t.Fatalf("Work returned %v; a job with nothing to do ends cleanly so River does not retry it", o.err)
				}
				if o.repo.reads != 1 {
					t.Errorf("the attempt read its report row %d times, want once", o.repo.reads)
				}
				if o.repo.marked != 0 || o.repo.completed != 0 || o.repo.failed != 0 {
					t.Errorf("the attempt wrote to a %s row: marked generating %d, completed %d, failed %d; want none",
						status, o.repo.marked, o.repo.completed, o.repo.failed)
				}
				if len(o.events) != 0 {
					t.Errorf("published report events %v for a report that is no longer in flight", o.events)
				}
				if len(o.emails) != 0 {
					t.Errorf("queued emails %v for a report that is no longer in flight", o.emails)
				}
				if o.uploads != 0 || o.siteReads != 0 {
					t.Errorf("rendered anyway: %d uploads, %d site reads", o.uploads, o.siteReads)
				}
				// Logged once: one record, naming the report and the status it found.
				lines := strings.Split(strings.TrimSpace(o.logs), "\n")
				if len(lines) != 1 || lines[0] == "" {
					t.Fatalf("want exactly one log record, got %d:\n%s", len(lines), o.logs)
				}
				if !strings.Contains(lines[0], `"report_id"`) || !strings.Contains(lines[0], `"status":"`+status+`"`) {
					t.Errorf("the log record does not name the report and its status: %s", lines[0])
				}
			})
		}
	}
}

// The other side: a job whose row is still in flight is not turned away. The
// first attempt finds it pending; a retry finds the generating row the earlier
// attempt left.
func TestWorkStillCompletesAReportThatIsInFlight(t *testing.T) {
	cases := []struct {
		status  string
		attempt int
	}{
		{StatusPending, 1},
		{StatusGenerating, 2},
		{StatusGenerating, 3},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("%s row, attempt %d", c.status, c.attempt), func(t *testing.T) {
			o := runAttemptAgainstRow(t, c.status, c.attempt)

			if o.err != nil {
				t.Fatalf("Work: %v", o.err)
			}
			if o.repo.marked != 1 || o.repo.completed != 1 || o.repo.failed != 0 {
				t.Errorf("marked generating %d, completed %d, failed %d; want 1, 1, 0", o.repo.marked, o.repo.completed, o.repo.failed)
			}
			if got := strings.Join(o.events, ","); got != StatusCompleted {
				t.Errorf("published events %q, want one %q", got, StatusCompleted)
			}
			if got := strings.Join(o.emails, ","); got != "report_ready" {
				t.Errorf("queued emails %q, want one report_ready", got)
			}
			if o.uploads != 2 {
				t.Errorf("uploaded %d objects, want the HTML and the PDF", o.uploads)
			}
		})
	}
}

// A row that cannot be read is not the same as a row that is finished: the
// attempt fails so River retries it, and the final attempt marks the report
// failed as it always has.
func TestWorkFailsTheAttemptWhenTheReportRowCannotBeRead(t *testing.T) {
	boom := fmt.Errorf("db down")
	repo := &readFailureRepo{lifecycleRepo: &lifecycleRepo{stubRepo: &stubRepo{}}, readErr: boom}
	svc := NewService(repo, &capturingBlob{})
	htmlR, err := html.NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	w := NewGenerateWorker(repo, svc, oneSiteSources(), htmlR, pdf.NewFpdfRenderer(), nil, nil)

	for _, attempt := range []int{1, 3} {
		repo.lifecycleRepo.failed = 0
		err, p := workCatching(w, context.Background(), generateJob(attempt))
		if p != nil {
			t.Fatalf("attempt %d: Work panicked: %v", attempt, p)
		}
		if err == nil {
			t.Fatalf("attempt %d: Work returned nil for a report row it could not read, so River would not retry", attempt)
		}
		wantFailed := 0
		if attempt == 3 {
			wantFailed = 1
		}
		if repo.lifecycleRepo.failed != wantFailed {
			t.Errorf("attempt %d of 3: FailReport called %d times, want %d", attempt, repo.lifecycleRepo.failed, wantFailed)
		}
		if repo.lifecycleRepo.marked != 0 {
			t.Errorf("attempt %d: marked the report generating without having read it", attempt)
		}
	}
}

// readFailureRepo is a lifecycleRepo whose row read fails.
type readFailureRepo struct {
	*lifecycleRepo
	readErr error
}

func (r *readFailureRepo) GetReport(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (GeneratedReport, error) {
	return GeneratedReport{}, r.readErr
}
