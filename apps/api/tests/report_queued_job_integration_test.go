// report_queued_job_integration_test.go -- the generate worker against the
// database, on the pool startPostgres returns (wpmgr_app, NOSUPERUSER,
// NOBYPASSRLS). The worker is the production one on the production repo: its
// first step reads the report row through the tenant path and its writes go
// through the agent path, so this is what shows the read is admitted by RLS for
// a job, which a fake repo cannot.
//
// A job queued behind a backlog can start after GenerateNow failed its row. It
// must leave that row, and the client, alone.
package tests

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/mosamlife/wpmgr/apps/api/internal/report"
	"github.com/mosamlife/wpmgr/apps/api/internal/report/render/html"
	"github.com/mosamlife/wpmgr/apps/api/internal/report/render/pdf"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

// rptCountingBlob counts the objects uploaded.
type rptCountingBlob struct{ puts int }

func (b *rptCountingBlob) Put(_ context.Context, _ string, body io.Reader, _ int64) error {
	_, _ = io.Copy(io.Discard, body)
	b.puts++
	return nil
}
func (b *rptCountingBlob) Delete(context.Context, string) error { return nil }
func (b *rptCountingBlob) PresignGet(context.Context, string, time.Duration) (string, error) {
	return "", nil
}

// rptEventLog keeps the status of every report.completed event.
type rptEventLog struct{ statuses []string }

func (l *rptEventLog) Publish(_ context.Context, ev site.ConnectionEvent) error {
	if s, ok := ev.Data["status"].(string); ok {
		l.statuses = append(l.statuses, s)
	}
	return nil
}

func TestReportQueuedJobLeavesAReportThatIsNoLongerInFlight(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "rpt-queued-job")
	longAgo := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond)

	cases := []struct {
		status     string
		wantWork   bool
		wantStatus string
	}{
		// The row GenerateNow failed, and a report that is already finished.
		{report.StatusFailed, false, report.StatusFailed},
		{report.StatusCompleted, false, report.StatusCompleted},
		// A job that is still wanted: its row is waiting, or an earlier attempt
		// of the same job left it generating.
		{report.StatusPending, true, report.StatusCompleted},
		{report.StatusGenerating, true, report.StatusCompleted},
	}
	for _, tc := range cases {
		t.Run(tc.status+" row", func(t *testing.T) {
			client := gh970SeedClient(t, pool, tenant, tc.status)
			reportID := rptSeedReport(t, pool, tenant, client, tc.status, longAgo)

			blob := &rptCountingBlob{}
			events := &rptEventLog{}
			repo := report.NewRepo(pool)
			svc := report.NewService(repo, blob)
			svc.SetPublisher(events)
			htmlR, err := html.NewRenderer()
			if err != nil {
				t.Fatal(err)
			}
			src := report.Sources{
				ListClientSites: func(context.Context, uuid.UUID, uuid.UUID) ([]site.Site, error) {
					return []site.Site{{ID: uuid.New(), Name: "site-a", URL: "https://a.example"}}, nil
				},
			}
			w := report.NewGenerateWorker(repo, svc, src, htmlR, pdf.NewFpdfRenderer(), nil, nil)

			job := &river.Job[report.GenerateArgs]{
				JobRow: &rivertype.JobRow{Attempt: 1, MaxAttempts: 3},
				Args: report.GenerateArgs{
					TenantID:    tenant,
					ClientID:    client,
					ReportID:    reportID,
					PeriodStart: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
					PeriodEnd:   time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
				},
			}
			if err := w.Work(ctx, job); err != nil {
				t.Fatalf("Work: %v", err)
			}

			status, _ := rptReportState(t, pool, tenant, reportID)
			if status != tc.wantStatus {
				t.Errorf("report row is %q after the job, want %q", status, tc.wantStatus)
			}
			if tc.wantWork {
				if blob.puts != 2 || len(events.statuses) != 1 || events.statuses[0] != report.StatusCompleted {
					t.Errorf("a job whose report is in flight did not run: %d uploads, events %v", blob.puts, events.statuses)
				}
			} else if blob.puts != 0 || len(events.statuses) != 0 {
				t.Errorf("a job whose report is %s did work: %d uploads, events %v", tc.status, blob.puts, events.statuses)
			}
		})
	}
}
