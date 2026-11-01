// report_generate_now_live_job_integration_test.go -- "Generate now" against the
// database and River's own job table, on the pool startPostgres returns
// (wpmgr_app, NOSUPERUSER, NOBYPASSRLS). The service is the production one on
// the production repo and the production River enqueuer, and the jobs are real
// River jobs inserted by a River client over the same pool.
//
// GenerateNow takes a pending or generating report row that nothing has touched
// for the length of every attempt of its job to belong to a job that died, and
// replaces it with a report of its own. A job that is only waiting in a busy
// queue touches nothing either, so a scheduled report queued behind a backlog
// was replaced by a request that does not notify the client, and the client
// never got that period's email. A report is dead only when no job for it is
// still queued or running.
package tests

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
	"github.com/riverqueue/river/rivertype"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/report"
	"github.com/mosamlife/wpmgr/apps/api/internal/report/render/html"
	"github.com/mosamlife/wpmgr/apps/api/internal/report/render/pdf"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

// rptLiveMailer records every email the report worker queues.
type rptLiveMailer struct{ sends []string }

func (m *rptLiveMailer) Enqueue(_ context.Context, _ uuid.UUID, to []string, template string, _ map[string]any) error {
	for _, r := range to {
		m.sends = append(m.sends, template+"->"+r)
	}
	return nil
}

// rptLiveMailerOn says the instance mailer is configured.
type rptLiveMailerOn struct{}

func (rptLiveMailerOn) Enabled(context.Context) bool { return true }

// rptLiveRiver migrates River's schema as the owner, the way cmd/wpmgr does, and
// returns an insert-only River client on the application pool: the client the
// scanner and the enqueuer insert report jobs with.
func rptLiveRiver(t *testing.T, pool *db.Pool) *river.Client[pgx.Tx] {
	t.Helper()
	ctx := context.Background()
	owner := connectOwner(t, pool)
	defer owner.Close()
	migrator, err := rivermigrate.New(riverpgxv5.New(owner.Pool), nil)
	if err != nil {
		t.Fatalf("river migrator: %v", err)
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		t.Fatalf("river migrate: %v", err)
	}
	client, err := river.NewClient[pgx.Tx](riverpgxv5.New(pool.Pool), &river.Config{})
	if err != nil {
		t.Fatalf("river client: %v", err)
	}
	return client
}

// rptLiveWorker is the production generate worker on the production repo.
func rptLiveWorker(t *testing.T, repo report.Repo, svc *report.Service) *report.GenerateWorker {
	t.Helper()
	htmlR, err := html.NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	src := report.Sources{
		ListClientSites: func(context.Context, uuid.UUID, uuid.UUID) ([]site.Site, error) {
			return []site.Site{{ID: uuid.New(), Name: "site-a", URL: "https://a.example"}}, nil
		},
	}
	return report.NewGenerateWorker(repo, svc, src, htmlR, pdf.NewFpdfRenderer(), nil, nil)
}

// rptBackdateReport sets the time a report row was last touched.
func rptBackdateReport(t *testing.T, pool *db.Pool, tenant, id uuid.UUID, age time.Duration) {
	t.Helper()
	ctx := context.Background()
	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "UPDATE generated_reports SET updated_at = $3 WHERE id = $1 AND tenant_id = $2",
			id, tenant, time.Now().UTC().Add(-age))
		return err
	}); err != nil {
		t.Fatalf("backdate report %s: %v", id, err)
	}
}

// rptForceJobState puts a River job in the given state, the way River itself
// would leave it. The jobs are inserted through River's client, which can only
// make them available; there is no client call that finishes one without
// running a worker, so the state is moved out of band on the owner's side of
// the table. Each state carries the columns River sets with it.
func rptForceJobState(t *testing.T, pool *db.Pool, jobID int64, state rivertype.JobState) {
	t.Helper()
	extra := map[rivertype.JobState]string{
		rivertype.JobStateAvailable: "",
		rivertype.JobStatePending:   "",
		rivertype.JobStateScheduled: ", scheduled_at = now() + interval '1 hour'",
		rivertype.JobStateRetryable: ", attempt = 1, attempted_at = now(), scheduled_at = now() + interval '1 minute'",
		rivertype.JobStateRunning:   ", attempt = 1, attempted_at = now()",
		rivertype.JobStateCompleted: ", attempt = 1, attempted_at = now(), finalized_at = now()",
		rivertype.JobStateDiscarded: ", attempt = 3, attempted_at = now(), finalized_at = now()",
		rivertype.JobStateCancelled: ", finalized_at = now()",
	}
	set, ok := extra[state]
	if !ok {
		t.Fatalf("no recipe for job state %q", state)
	}
	admin := connectAdmin(t, pool)
	defer admin.Close()
	tag, err := admin.Exec(context.Background(),
		"UPDATE river_job SET state = $2::river_job_state"+set+" WHERE id = $1", jobID, string(state))
	if err != nil {
		t.Fatalf("force job %d to %s: %v", jobID, state, err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("force job %d to %s: %d rows changed, want 1", jobID, state, tag.RowsAffected())
	}
}

// rptJobCount counts the report_generate jobs whose args carry key = id, read as
// the application role.
func rptJobCount(t *testing.T, pool *db.Pool, key string, id uuid.UUID) (n int) {
	t.Helper()
	frag, err := json.Marshal(map[string]string{key: id.String()})
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM river_job WHERE kind = 'report_generate' AND args @> $1::jsonb`, string(frag)).Scan(&n); err != nil {
		t.Fatalf("count report jobs by %s: %v", key, err)
	}
	return n
}

// A scheduled report waits in a busy queue for longer than the stale window and
// the dashboard's "Generate now" arrives. The report is returned as in flight,
// nothing is failed or created, and when the job finally runs it completes the
// scheduled report and emails the client once.
func TestReportGenerateNowLeavesAScheduledReportWhoseJobIsStillQueued(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "rpt-live-job-scheduled")
	client := gh970SeedClient(t, pool, tenant, "Scheduled Client")
	rc := rptLiveRiver(t, pool)

	repo := report.NewRepo(pool)
	blob := &rptCountingBlob{}
	mail := &rptLiveMailer{}
	svc := report.NewService(repo, blob)
	svc.SetMailer(mail)
	svc.SetMailerStatus(rptLiveMailerOn{})
	svc.SetEnqueuer(report.NewRiverEnqueuer(rc))

	sched, err := svc.UpsertSchedule(ctx, report.UpsertScheduleInput{
		TenantID: tenant, ClientID: client, Enabled: true, Cadence: "weekly",
		SendDay: 1, SendHour: 8, Recipients: []string{"client@example.com"},
	})
	if err != nil {
		t.Fatalf("save schedule: %v", err)
	}

	// The scanner's run for this period, as the scanner makes it: the row, then
	// the job that notifies the client. The row was made seven minutes ago and
	// the queue has not reached the job.
	periodEnd := time.Now().UTC().Truncate(time.Second)
	periodStart := periodEnd.Add(-7 * 24 * time.Hour)
	scheduled, err := repo.CreateReportAgent(ctx, report.CreateReportInput{
		TenantID: tenant, ClientID: client, ScheduleID: &sched.ID,
		PeriodStart: periodStart, PeriodEnd: periodEnd,
	})
	if err != nil {
		t.Fatalf("create scheduled report row: %v", err)
	}
	queued, err := rc.Insert(ctx, report.GenerateArgs{
		TenantID: tenant, ClientID: client, ReportID: scheduled.ID, ScheduleID: &sched.ID,
		PeriodStart: periodStart, PeriodEnd: periodEnd, Notify: true,
	}, nil)
	if err != nil {
		t.Fatalf("queue scheduled job: %v", err)
	}
	rptBackdateReport(t, pool, tenant, scheduled.ID, 7*time.Minute)
	if queued.Job.State != rivertype.JobStateAvailable {
		t.Fatalf("premise: the scheduled job is %q, want it waiting in the queue", queued.Job.State)
	}

	// The dashboard's Generate now sends {}.
	got, err := svc.GenerateNow(ctx, tenant, client, report.GenerateNowInput{})
	if err != nil {
		t.Fatalf("GenerateNow: %v", err)
	}
	status, errMsg := rptReportState(t, pool, tenant, scheduled.ID)
	t.Logf("after Generate now: returned %s (scheduled %s); scheduled row %q (%q); %d report rows; %d report jobs",
		got.ID, scheduled.ID, status, errMsg, rptCountReports(t, pool, tenant, client), rptJobCount(t, pool, "client_id", client))
	if got.ID != scheduled.ID {
		t.Fatalf("GenerateNow returned %s, want the scheduled report %s that is still queued", got.ID, scheduled.ID)
	}
	if status != report.StatusPending || errMsg != "" {
		t.Errorf("scheduled report is %q with error %q, want it left pending", status, errMsg)
	}
	if n := rptCountReports(t, pool, tenant, client); n != 1 {
		t.Errorf("%d report rows for the client, want the scheduled one only", n)
	}
	if n := rptJobCount(t, pool, "client_id", client); n != 1 {
		t.Errorf("%d report jobs for the client, want the scheduled job only", n)
	}

	// The queue gets to the job. It runs with the args River stored.
	var stored report.GenerateArgs
	if err := json.Unmarshal(queued.Job.EncodedArgs, &stored); err != nil {
		t.Fatalf("decode stored job args: %v", err)
	}
	row := *queued.Job
	row.Attempt = 1
	job := &river.Job[report.GenerateArgs]{JobRow: &row, Args: stored}
	if err := rptLiveWorker(t, repo, svc).Work(ctx, job); err != nil {
		t.Fatalf("scheduled job: %v", err)
	}
	status, _ = rptReportState(t, pool, tenant, scheduled.ID)
	if status != report.StatusCompleted {
		t.Errorf("scheduled report is %q after its job ran, want completed", status)
	}
	if len(mail.sends) != 1 || mail.sends[0] != "report_ready->client@example.com" {
		t.Errorf("emails queued %v, want exactly one report_ready to the client", mail.sends)
	}
	if blob.puts != 2 {
		t.Errorf("%d objects uploaded, want the html and the pdf of one report", blob.puts)
	}
	if n := rptCountReports(t, pool, tenant, client); n != 1 {
		t.Errorf("%d report rows for the client at the end, want the scheduled one only", n)
	}
}

// What a stale pending or generating row is taken to be follows the state of the
// River job for that report: queued, waiting for a retry, parked or running means
// the report is in flight whatever its age; completed, discarded, cancelled or
// not there at all means the job will never touch the row again and the report
// is dead. Another report's job, or a job of another tenant that names this
// report's id, keeps nothing alive.
func TestReportGenerateNowFollowsTheStateOfTheReportsJob(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "rpt-live-job-states")
	otherTenant := seedTenant(t, pool, "rpt-live-job-states-other")
	rc := rptLiveRiver(t, pool)
	repo := report.NewRepo(pool)

	const (
		noJob       = "none"
		ownJob      = "own"
		otherReport = "another report's"
		otherOrg    = "another tenant's"
	)
	justPast := 7 * time.Minute
	cases := []struct {
		name     string
		row      string
		age      time.Duration
		job      string
		state    rivertype.JobState
		inFlight bool
	}{
		{"pending row, job available", report.StatusPending, justPast, ownJob, rivertype.JobStateAvailable, true},
		{"pending row, job scheduled for later", report.StatusPending, justPast, ownJob, rivertype.JobStateScheduled, true},
		{"pending row, job waiting to retry", report.StatusPending, justPast, ownJob, rivertype.JobStateRetryable, true},
		{"pending row, job parked as pending", report.StatusPending, justPast, ownJob, rivertype.JobStatePending, true},
		{"pending row, job running", report.StatusPending, justPast, ownJob, rivertype.JobStateRunning, true},
		{"generating row, job running", report.StatusGenerating, justPast, ownJob, rivertype.JobStateRunning, true},
		{"generating row, job waiting to retry", report.StatusGenerating, justPast, ownJob, rivertype.JobStateRetryable, true},
		{"pending row queued for two days", report.StatusPending, 48 * time.Hour, ownJob, rivertype.JobStateAvailable, true},

		{"pending row, job completed", report.StatusPending, justPast, ownJob, rivertype.JobStateCompleted, false},
		{"generating row, job discarded", report.StatusGenerating, justPast, ownJob, rivertype.JobStateDiscarded, false},
		{"pending row, job cancelled", report.StatusPending, justPast, ownJob, rivertype.JobStateCancelled, false},
		{"pending row, no job at all", report.StatusPending, justPast, noJob, "", false},
		{"generating row, no job at all", report.StatusGenerating, justPast, noJob, "", false},
		{"pending row, only another report's job is queued", report.StatusPending, justPast, otherReport, rivertype.JobStateAvailable, false},
		{"pending row, only another tenant's job names it", report.StatusPending, justPast, otherOrg, rivertype.JobStateAvailable, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := gh970SeedClient(t, pool, tenant, tc.name)
			existing := rptSeedReport(t, pool, tenant, client, tc.row, time.Now().UTC().Add(-tc.age))

			fixtureJobs := 0
			if tc.job != noJob {
				args := report.GenerateArgs{
					TenantID: tenant, ClientID: client, ReportID: existing,
					PeriodStart: time.Now().UTC().Add(-7 * 24 * time.Hour), PeriodEnd: time.Now().UTC(),
					Notify: true,
				}
				switch tc.job {
				case otherReport:
					args.ReportID = uuid.New()
				case otherOrg:
					args.TenantID = otherTenant
				}
				res, err := rc.Insert(ctx, args, nil)
				if err != nil {
					t.Fatalf("queue the fixture job: %v", err)
				}
				if tc.state != rivertype.JobStateAvailable {
					rptForceJobState(t, pool, res.Job.ID, tc.state)
				}
				fixtureJobs = 1
			}

			svc := report.NewService(repo, &rptCountingBlob{})
			svc.SetEnqueuer(report.NewRiverEnqueuer(rc))
			got, err := svc.GenerateNow(ctx, tenant, client, report.GenerateNowInput{})
			if err != nil {
				t.Fatalf("GenerateNow: %v", err)
			}
			status, errMsg := rptReportState(t, pool, tenant, existing)
			clientJobs := rptJobCount(t, pool, "client_id", client)
			t.Logf("returned %s (existing %s); existing row %q (%q); %d report rows; %d report jobs",
				got.ID, existing, status, errMsg, rptCountReports(t, pool, tenant, client), clientJobs)

			if tc.inFlight {
				if got.ID != existing {
					t.Fatalf("GenerateNow returned %s, want the report %s whose job is %s", got.ID, existing, tc.state)
				}
				if status != tc.row || errMsg != "" {
					t.Errorf("the report is %q with error %q, want it left %q", status, errMsg, tc.row)
				}
				if n := rptCountReports(t, pool, tenant, client); n != 1 {
					t.Errorf("%d report rows for the client, want the one in flight", n)
				}
				if clientJobs != fixtureJobs {
					t.Errorf("%d report jobs for the client, want the %d that was queued", clientJobs, fixtureJobs)
				}
				return
			}
			if got.ID == existing {
				t.Fatalf("GenerateNow returned the dead report %s (job: %s, state %q)", existing, tc.job, tc.state)
			}
			if got.Status != report.StatusPending {
				t.Errorf("new report status = %q, want pending", got.Status)
			}
			if status != report.StatusFailed || errMsg == "" {
				t.Errorf("the dead report is %q with error %q, want failed with a reason", status, errMsg)
			}
			if n := rptCountReports(t, pool, tenant, client); n != 2 {
				t.Errorf("%d report rows for the client, want the dead one and the new one", n)
			}
			if n := rptJobCount(t, pool, "report_id", got.ID); n != 1 {
				t.Errorf("%d jobs queued for the new report, want 1", n)
			}
			if clientJobs != fixtureJobs+1 {
				t.Errorf("%d report jobs for the client, want the fixture's %d and the new one", clientJobs, fixtureJobs)
			}
		})
	}
}
