// gh791_scan_command_failed_integration_test.go: GH #791 — an agent-reported
// command failure (agentcmd.CommandError.AgentFailed) fails a scan run on the
// first attempt with the sanitised operator message. Drives the PRODUCTION
// scan.ScanRunWorker.Work against a real Postgres 16 (testcontainers) through
// scan.NewRepo(pool), the repo the production worker uses, as wpmgr_app under
// RLS.
package tests

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/scan"
)

// gh791ScanCommandFailure is the typed error agentcmd returns for an HTTP 500
// wpmgr_command_failed body from the scan command.
func gh791ScanCommandFailure() *agentcmd.CommandError {
	return &agentcmd.CommandError{
		Command:     "scan",
		Status:      500,
		Code:        "wpmgr_command_failed",
		Message:     "Command execution failed: RuntimeException: cannot read the core checksum list, see www.evil-example.com/help",
		DataCommand: "scan",
		Exception:   "RuntimeException",
		At:          "includes/commands/class-scan-command.php:88",
		DataStatus:  500,
	}
}

// gh791ScanFailingClient answers Scan with the agent failure, wrapped the way
// a caller sees it. The wrapping text carries "status 404" on purpose: the
// error then also matches the worker's old-agent check, so the run's recorded
// reason shows which branch ran first.
type gh791ScanFailingClient struct{ calls int }

func (c *gh791ScanFailingClient) Scan(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.ScanRequest) (agentcmd.ScanResponse, error) {
	c.calls++
	return agentcmd.ScanResponse{}, fmt.Errorf("scan after a status 404 hop: %w", gh791ScanCommandFailure())
}

func (c *gh791ScanFailingClient) GetFile(context.Context, uuid.UUID, string, agentcmd.GetFileRequest) (agentcmd.GetFileResponse, error) {
	return agentcmd.GetFileResponse{}, fmt.Errorf("GetFile not used by this test")
}

type gh791ScanSiteLookup struct{ url string }

func (s gh791ScanSiteLookup) GetScanSiteInfo(context.Context, uuid.UUID, uuid.UUID) (scan.ScanSiteInfo, error) {
	return scan.ScanSiteInfo{URL: s.url, Enrolled: true}, nil
}

// TestGH791_ScanCommandFailedFailsRun: the production scan worker fails the
// run on the first attempt (Work returns nil, so River does not retry) with
// OperatorMessage("Scan"), never "agent_too_old" and never raw body text.
func TestGH791_ScanCommandFailedFailsRun(t *testing.T) {
	t.Parallel()
	pool := startPostgres(t)
	ctx := context.Background()

	tenantID := seedTenant(t, pool, "gh791-scan-failed")
	siteID := seedSite(t, pool, tenantID, "https://example.com")

	var role string
	var super, bypass bool
	if err := pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT current_user, rolsuper, rolbypassrls
			   FROM pg_roles WHERE rolname = current_user`).Scan(&role, &super, &bypass)
	}); err != nil {
		t.Fatalf("read current role: %v", err)
	}
	t.Logf("connected as %q rolsuper=%v rolbypassrls=%v", role, super, bypass)
	if role != "wpmgr_app" || super || bypass {
		t.Fatalf("this proof must run as wpmgr_app without superuser or BYPASSRLS; got %q super=%v bypass=%v", role, super, bypass)
	}

	repo := scan.NewRepo(pool)
	run, err := repo.InsertRun(ctx, tenantID, siteID, scan.KindCore)
	if err != nil {
		t.Fatalf("seed scan run: %v", err)
	}

	cmd := &gh791ScanFailingClient{}
	worker := scan.NewScanRunWorker(repo, nil, cmd, gh791ScanSiteLookup{url: "https://example.com"}, nil, nil, nil)
	job := &river.Job[scan.ScanRunArgs]{Args: scan.ScanRunArgs{
		TenantID: tenantID,
		SiteID:   siteID,
		RunID:    run.ID,
	}}
	if err := worker.Work(ctx, job); err != nil {
		t.Fatalf("Work() returned %v; an agent-reported failure must fail the run (nil), not be retried", err)
	}
	if cmd.calls != 1 {
		t.Errorf("scan command sent %d times, want 1", cmd.calls)
	}

	got, err := repo.GetRun(ctx, tenantID, run.ID)
	if err != nil {
		t.Fatalf("GetRun after Work(): %v", err)
	}
	if got.Status != scan.StatusFailed {
		t.Fatalf("run status = %q, want %q after one agent failure", got.Status, scan.StatusFailed)
	}
	if want := gh791ScanCommandFailure().OperatorMessage("Scan"); got.Error != want {
		t.Errorf("run error = %q, want OperatorMessage(\"Scan\") %q", got.Error, want)
	}
	for _, leak := range []string{"agent_too_old", "body=", "evil-example", "rejected by agent"} {
		if strings.Contains(got.Error, leak) {
			t.Errorf("run error %q contains %q", got.Error, leak)
		}
	}
	if !strings.Contains(got.Error, "RuntimeException at includes/commands/class-scan-command.php:88") {
		t.Errorf("run error %q lost the exception class and location", got.Error)
	}
}
