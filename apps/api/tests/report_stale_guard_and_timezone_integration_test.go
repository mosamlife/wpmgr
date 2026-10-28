// report_stale_guard_and_timezone_integration_test.go — database proofs for two
// pieces of report SQL, run on the pool startPostgres returns, which connects as
// wpmgr_app (NOSUPERUSER, NOBYPASSRLS). Every row is seeded and read through
// InTenantTx, and the code under test is the production repo and service.
//
//   - getActiveReportQuery selects and scans updated_at, which GenerateNow reads
//     to tell a report whose job died from one that is still running. A row
//     that scans a zero updated_at is never stale, so "Generate now" was
//     refused for ever once a job died.
//   - GetClientWithTimezone gives the schedule GET and PUT the client's
//     timezone, which the send hour and next_run_at are in.
package tests

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/report"
)

type rptBlob struct{}

func (rptBlob) Put(context.Context, string, io.Reader, int64) error { return nil }
func (rptBlob) Delete(context.Context, string) error                { return nil }
func (rptBlob) PresignGet(context.Context, string, time.Duration) (string, error) {
	return "", nil
}

type rptEnqueuer struct{ calls []report.GenerateArgs }

func (e *rptEnqueuer) EnqueueGenerate(_ context.Context, a report.GenerateArgs) error {
	e.calls = append(e.calls, a)
	return nil
}

// rptSeedReport inserts a report row in the given status, last touched at
// updatedAt, and returns its id.
func rptSeedReport(t *testing.T, pool *db.Pool, tenant, client uuid.UUID, status string, updatedAt time.Time) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
INSERT INTO generated_reports (id, tenant_id, client_id, period_start, period_end, status, created_at, updated_at)
VALUES ($1, $2, $3, now() - interval '31 days', now() - interval '1 day', $4, $5, $5)`,
			id, tenant, client, status, updatedAt)
		return err
	}); err != nil {
		t.Fatalf("seed %s report: %v", status, err)
	}
	return id
}

func rptReportState(t *testing.T, pool *db.Pool, tenant, id uuid.UUID) (status, errMsg string) {
	t.Helper()
	ctx := context.Background()
	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT status, error FROM generated_reports WHERE id = $1 AND tenant_id = $2",
			id, tenant).Scan(&status, &errMsg)
	}); err != nil {
		t.Fatalf("read report %s: %v", id, err)
	}
	return status, errMsg
}

func rptCountReports(t *testing.T, pool *db.Pool, tenant, client uuid.UUID) (n int) {
	t.Helper()
	ctx := context.Background()
	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT count(*) FROM generated_reports WHERE tenant_id = $1 AND client_id = $2",
			tenant, client).Scan(&n)
	}); err != nil {
		t.Fatalf("count reports: %v", err)
	}
	return n
}

// The active-report query returns the last time the row was touched, as stored.
func TestReportActiveQueryReturnsUpdatedAt(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "rpt-active-query")
	client := gh970SeedClient(t, pool, tenant, "Client")

	touched := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond)
	id := rptSeedReport(t, pool, tenant, client, report.StatusGenerating, touched)

	got, found, err := report.NewRepo(pool).GetActiveReport(ctx, tenant, client)
	if err != nil || !found {
		t.Fatalf("GetActiveReport: found=%v err=%v", found, err)
	}
	if got.ID != id {
		t.Fatalf("GetActiveReport returned %s, want the seeded report %s", got.ID, id)
	}
	if !got.UpdatedAt.Equal(touched) {
		t.Errorf("UpdatedAt = %s, want the stored %s: the query does not select it", got.UpdatedAt, touched)
	}
}

// GenerateNow against the database: a report whose job died is failed and the
// request goes through; a report whose job is running is returned as it was.
func TestReportGenerateNowStaleGuard(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "rpt-stale-guard")

	// Far past any window the job's attempts could fill, and just now.
	longAgo := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond)
	justNow := time.Now().UTC().Truncate(time.Microsecond)

	cases := []struct {
		name      string
		status    string
		updatedAt time.Time
		wantStale bool
	}{
		{"stale generating", report.StatusGenerating, longAgo, true},
		{"stale pending", report.StatusPending, longAgo, true},
		{"fresh generating", report.StatusGenerating, justNow, false},
		{"fresh pending", report.StatusPending, justNow, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := gh970SeedClient(t, pool, tenant, tc.name)
			existing := rptSeedReport(t, pool, tenant, client, tc.status, tc.updatedAt)

			svc := report.NewService(report.NewRepo(pool), rptBlob{})
			enq := &rptEnqueuer{}
			svc.SetEnqueuer(enq)

			got, err := svc.GenerateNow(ctx, tenant, client, report.GenerateNowInput{})
			if err != nil {
				t.Fatalf("GenerateNow: %v", err)
			}
			status, errMsg := rptReportState(t, pool, tenant, existing)

			if tc.wantStale {
				if got.ID == existing {
					t.Fatal("GenerateNow returned the dead report, so a new request is refused for ever")
				}
				if got.Status != report.StatusPending {
					t.Errorf("new report status = %q, want pending", got.Status)
				}
				if status != report.StatusFailed || errMsg == "" {
					t.Errorf("dead report is %q with error %q, want failed with a reason", status, errMsg)
				}
				if n := rptCountReports(t, pool, tenant, client); n != 2 {
					t.Errorf("%d report rows for the client, want the dead one and the new one", n)
				}
				if len(enq.calls) != 1 || enq.calls[0].ReportID != got.ID {
					t.Errorf("enqueued %d jobs (%v), want one, for the new report %s", len(enq.calls), enq.calls, got.ID)
				}
				return
			}
			if got.ID != existing {
				t.Errorf("GenerateNow returned %s, want the running report %s", got.ID, existing)
			}
			if status != tc.status || errMsg != "" {
				t.Errorf("running report is now %q with error %q, want it left as %q", status, errMsg, tc.status)
			}
			if n := rptCountReports(t, pool, tenant, client); n != 1 {
				t.Errorf("%d report rows for the client, want the one that was running", n)
			}
			if len(enq.calls) != 0 {
				t.Errorf("enqueued %d jobs for a request that returned the running report", len(enq.calls))
			}
		})
	}
}

func rptSeedClientWithTimezone(t *testing.T, pool *db.Pool, tenant uuid.UUID, name, tz string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "INSERT INTO clients (id, tenant_id, name, timezone) VALUES ($1, $2, $3, $4)", id, tenant, name, tz)
		return err
	}); err != nil {
		t.Fatalf("seed client %s: %v", name, err)
	}
	return id
}

// GET and PUT of a client's report schedule carry the timezone on the client's
// record, and the first send time is computed in it.
func TestReportScheduleCarriesTheClientsTimezone(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "rpt-timezone")
	otherTenant := seedTenant(t, pool, "rpt-timezone-other")

	const zone = "Asia/Kolkata"
	client := rptSeedClientWithTimezone(t, pool, tenant, "Kolkata Client", zone)
	defaultClient := gh970SeedClient(t, pool, tenant, "Default Client")
	foreignClient := rptSeedClientWithTimezone(t, pool, otherTenant, "Foreign Client", "Pacific/Auckland")

	svc := report.NewService(report.NewRepo(pool), rptBlob{})

	// GET before any schedule is saved: the defaults, in the client's timezone.
	got, found, err := svc.GetSchedule(ctx, tenant, client)
	if err != nil {
		t.Fatalf("GetSchedule: %v", err)
	}
	if found {
		t.Fatal("premise: the client has a stored schedule before one was saved")
	}
	if got.Timezone != zone {
		t.Errorf("GET with no schedule: timezone = %q, want %q", got.Timezone, zone)
	}

	// PUT: the saved schedule comes back with the timezone, and the first run
	// is at the send hour in that timezone.
	saved, err := svc.UpsertSchedule(ctx, report.UpsertScheduleInput{
		TenantID: tenant, ClientID: client, Enabled: true, Cadence: "monthly", SendDay: 15, SendHour: 8,
		Recipients: []string{},
	})
	if err != nil {
		t.Fatalf("UpsertSchedule: %v", err)
	}
	if saved.Timezone != zone {
		t.Errorf("PUT: timezone = %q, want %q", saved.Timezone, zone)
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		t.Fatalf("load %s: %v", zone, err)
	}
	if saved.NextRunAt == nil {
		t.Fatal("PUT of an enabled schedule returned no next_run_at")
	}
	if local := saved.NextRunAt.In(loc); local.Hour() != 8 || local.Minute() != 0 || local.Day() != 15 {
		t.Errorf("next_run_at = %s in %s, want the 15th at 08:00", local, zone)
	}

	// GET after the schedule is stored.
	got, found, err = svc.GetSchedule(ctx, tenant, client)
	if err != nil || !found {
		t.Fatalf("GetSchedule after save: found=%v err=%v", found, err)
	}
	if got.Timezone != zone {
		t.Errorf("GET with a stored schedule: timezone = %q, want %q", got.Timezone, zone)
	}

	// Controls: a client left on the default is UTC; a client of another
	// tenant is not visible, so its timezone does not come back.
	if def, _, err := svc.GetSchedule(ctx, tenant, defaultClient); err != nil || def.Timezone != "UTC" {
		t.Errorf("default client: timezone = %q err=%v, want UTC", def.Timezone, err)
	}
	if foreign, _, err := svc.GetSchedule(ctx, tenant, foreignClient); err != nil || foreign.Timezone != "" {
		t.Errorf("another tenant's client: timezone = %q err=%v, want it empty", foreign.Timezone, err)
	}
}
