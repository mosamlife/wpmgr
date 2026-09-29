// gh755_scan_redirect_integration_test.go: GH #755 F4 — the scan worker's
// redirect branch (internal/scan/worker.go, the agentcmd.AsRedirect check
// right before the "status 404" -> agent_too_old check) drives the
// PRODUCTION scan.ScanRunWorker.Work directly, against a real Postgres 16
// (testcontainers) reached through scan.NewRepo(pool) — the same repo the
// production worker uses, under RLS, as wpmgr_app.
//
// This exists because internal/scan/worker_test.go's TestWorkerLoop_* suite
// drives a hand-rolled copy of Work() (testableWorker), never the real
// ScanRunWorker.Work, so a regression in the production method's redirect
// branch is invisible to that package's fast unit tests. See this file's
// test for the actual coverage; the package unit tests remain valuable for
// everything else they cover, just not this branch.
package tests

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/scan"
)

// gh755ScanRedirect is the redirect gh755ScanRedirectClient answers with.
func gh755ScanRedirect() *agentcmd.RedirectError {
	return &agentcmd.RedirectError{
		Command:          "scan",
		Status:           301,
		From:             "https://example.com/wp-json/wpmgr/v1/command/scan",
		To:               "https://www.example.com/wp-json/wpmgr/v1/command/scan",
		SuggestedSiteURL: "https://www.example.com",
	}
}

// gh755ScanRedirectClient answers Scan the way agentcmd does when the site's
// saved command address redirects, wrapped the way a transport error reaches
// the worker. The wrapping text carries "status 404" on purpose: the error
// then matches the worker's old-agent check as well as its redirect check,
// so the failure text the run records shows which of the two ran first.
// GetFile is never called on this branch and stubbed only to satisfy
// scan.AgentScanClient.
type gh755ScanRedirectClient struct{ calls int }

func (c *gh755ScanRedirectClient) Scan(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.ScanRequest) (agentcmd.ScanResponse, error) {
	c.calls++
	return agentcmd.ScanResponse{}, fmt.Errorf("agent command failed after a status 404 hop: %w", gh755ScanRedirect())
}

func (c *gh755ScanRedirectClient) GetFile(context.Context, uuid.UUID, string, agentcmd.GetFileRequest) (agentcmd.GetFileResponse, error) {
	return agentcmd.GetFileResponse{}, fmt.Errorf("GetFile not used by this test")
}

// gh755ScanRedirectSiteLookup answers scan.SiteLookup with a fixed, enrolled
// site — the redirect happens at the agent transport, not at site
// resolution.
type gh755ScanRedirectSiteLookup struct{ url string }

func (s gh755ScanRedirectSiteLookup) GetScanSiteInfo(context.Context, uuid.UUID, uuid.UUID) (scan.ScanSiteInfo, error) {
	return scan.ScanSiteInfo{URL: s.url, Enrolled: true}, nil
}

// TestGH755Push_ScanRedirectFailsRunNotAgentTooOld drives the PRODUCTION
// scan.ScanRunWorker.Work end to end against a real scan_runs row: a site
// whose saved command address redirects fails the run on the first attempt
// with the operator redirect message (agentcmd.RedirectError.OperatorMessage),
// and NEVER with "agent_too_old" — the sibling branch keyed on a bare
// "status 404" substring match that a redirect must not fall into.
func TestGH755Push_ScanRedirectFailsRunNotAgentTooOld(t *testing.T) {
	t.Parallel()
	pool := startPostgres(t)
	ctx := context.Background()

	tenantID := seedTenant(t, pool, "gh755-scan-redirect")
	siteID := seedSite(t, pool, tenantID, "https://example.com")

	repo := scan.NewRepo(pool)
	run, err := repo.InsertRun(ctx, tenantID, siteID, scan.KindCore)
	if err != nil {
		t.Fatalf("seed scan run: %v", err)
	}

	cmd := &gh755ScanRedirectClient{}
	sites := gh755ScanRedirectSiteLookup{url: "https://example.com"}
	worker := scan.NewScanRunWorker(repo, nil, cmd, sites, nil, nil, nil)

	job := &river.Job[scan.ScanRunArgs]{Args: scan.ScanRunArgs{
		TenantID: tenantID,
		SiteID:   siteID,
		RunID:    run.ID,
	}}
	if err := worker.Work(ctx, job); err != nil {
		t.Fatalf("Work() returned %v; a redirect must be a terminal failure (nil, run marked failed), not a retry", err)
	}
	if cmd.calls != 1 {
		t.Errorf("scan command sent %d times, want 1", cmd.calls)
	}

	got, err := repo.GetRun(ctx, tenantID, run.ID)
	if err != nil {
		t.Fatalf("GetRun after Work(): %v", err)
	}
	if got.Status != scan.StatusFailed {
		t.Fatalf("run status = %q, want %q after one redirect attempt", got.Status, scan.StatusFailed)
	}
	// The recorded failure is the redirect message, exactly, and not the
	// old-agent text: the error above matches both checks, so this fails if
	// the "status 404" check runs first.
	const agentTooOld = "agent_too_old"
	if got.Error == agentTooOld {
		t.Fatalf("run error = %q: the redirect was read as an old agent; the redirect check must run before the \"status 404\" check", got.Error)
	}
	if want := gh755ScanRedirect().OperatorMessage("Scan"); got.Error != want {
		t.Errorf("run error = %q, want the redirect message %q", got.Error, want)
	}
	for _, want := range []string{"Scan not started.", "https://www.example.com", "updates to https://www.example.com automatically"} {
		if !strings.Contains(got.Error, want) {
			t.Errorf("run error %q does not contain %q", got.Error, want)
		}
	}
}
