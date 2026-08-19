// gh493_pause_digest_and_dbclean_integration_test.go — GH #493, the half that
// needs a real database.
//
// Two scheduled paths ignored monitoring pause. One of them, scheduled database
// cleaning, sends a signed db_clean command that DELETES ROWS FROM THE
// CUSTOMER'S LIVE WORDPRESS DATABASE.
//
// Every read here goes through the production repository — perf.NewRepo(pool),
// vuln.NewRepo(pool) — i.e. through the same InAgentTx / InTenantTx helpers the
// jobs use, as the non-superuser wpmgr_app role, so the RLS policies are live.
// A test that opened its own connection would leave them inert, which is
// exactly how m112's proofs passed over a cross-site-readable email domain.
//
// The Go-side wiring (which worker gates, which caller is exempt, and that a
// manual clean is never filtered) is pinned in
// internal/perf/monitoring_pause_test.go.
package tests

import (
	"context"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/perf"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
	"github.com/mosamlife/wpmgr/apps/api/internal/vuln"
)

// namesSite reports whether a fleet findings list names this site at all.
// "Names it" is the question the digest turns on: alerts.sql:236 records that
// naming a paused site in an alert is the same broken promise as alerting on it.
func namesSite(rows []vuln.FleetFindingRow, id uuid.UUID) bool {
	for _, r := range rows {
		if r.Finding.SiteID == id {
			return true
		}
	}
	return false
}

// namesSiteInSummary is namesSite for FleetSummary.Findings — the Service-level
// return shape (GetFleetSummary / GetFleetSummaryForDigest), which carries
// SiteID as a direct field rather than nested under Finding.
func namesSiteInSummary(rows []vuln.FleetFinding, id uuid.UUID) bool {
	for _, r := range rows {
		if r.SiteID == id {
			return true
		}
	}
	return false
}

// TestGH493PauseIsHonouredByTheDigestAndTheScheduledClean proves both halves in
// ONE run, against one paused site and one active sibling in the same tenant.
//
// Mutations that redden it, one each:
//   - drop unpausedOnlySQL from FleetOpenCountsExcludingPaused
//     (internal/vuln/repo.go) — reddens the digest count subtest
//   - drop it from FleetOpenFindingsExcludingPaused — reddens the naming subtest
//   - point GetFleetSummaryForDigest back at the unfiltered repo methods
//     (internal/vuln/service.go) — reddens both
//   - drop the monitoring_paused_at predicate from perf.Repo.PausedSiteIDs or
//     IsMonitoringPaused (internal/perf/repo.go) — reddens the clean subtests
func TestGH493PauseIsHonouredByTheDigestAndTheScheduledClean(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()

	tenant := seedTenant(t, pool, "gh493-"+uuid.NewString()[:8])
	actor := seedMonitoringUser(t, pool, "gh493-"+uuid.NewString()[:8]+"@example.com")

	active := seedSite(t, pool, tenant, "https://gh493-active-"+uuid.NewString()[:8]+".example.com")
	paused := seedSite(t, pool, tenant, "https://gh493-paused-"+uuid.NewString()[:8]+".example.com")

	// One open finding each, so both sites have something the digest could name.
	seedOpenFinding(t, pool, tenant, active, "gh493-active-"+uuid.NewString()[:8])
	seedOpenFinding(t, pool, tenant, paused, "gh493-paused-"+uuid.NewString()[:8])

	vulnRepo := vuln.NewRepo(pool)
	perfRepo := perf.NewRepo(pool)
	siteRepo := site.NewRepo(pool)
	// The production caller (cmd/wpmgr/vuln_alert_adapter.go) never touches
	// vulnRepo directly — it calls vuln.Service.GetFleetSummaryForDigest. The
	// subtest below goes through this same Service, over the same pool, so a
	// service that routed back to the unfiltered repo methods reddens it even
	// though the two repo-level subtests above it would stay green.
	vulnSvc := vuln.NewService(vulnRepo, pool, nil, nil, nil, slog.Default())

	// Baseline BEFORE any pause. Without it every subtest below would pass over
	// a query that simply returned nothing.
	baseFindings, err := vulnRepo.FleetOpenFindings(ctx, tenant, 100)
	if err != nil {
		t.Fatalf("baseline FleetOpenFindings: %v", err)
	}
	if !namesSite(baseFindings, active) || !namesSite(baseFindings, paused) {
		t.Fatalf("baseline: both seeded sites must appear in the fleet findings before any pause")
	}
	_, baseHigh, _, _, _, err := vulnRepo.FleetOpenCounts(ctx, tenant)
	if err != nil {
		t.Fatalf("baseline FleetOpenCounts: %v", err)
	}
	if baseHigh != 2 {
		t.Fatalf("baseline: expected 2 open high findings before any pause, got %d", baseHigh)
	}

	// Pause through the SAME repo the route uses.
	states, err := siteRepo.PauseMonitoring(ctx, site.PauseMonitoringInput{
		TenantID: tenant, ActorUserID: actor,
		Principal: monitoringPrincipal(tenant, actor),
		SiteIDs:   []uuid.UUID{paused}, Reason: "gh493 proof",
	})
	if err != nil {
		t.Fatalf("pause: %v", err)
	}
	if len(states) != 1 || !states[0].Changed {
		t.Fatalf("pause did not take: %+v", states)
	}

	// ---------------------------------------------------------------- part 2

	t.Run("the digest does not COUNT a paused site's findings", func(t *testing.T) {
		_, high, _, _, _, err := vulnRepo.FleetOpenCountsExcludingPaused(ctx, tenant)
		if err != nil {
			t.Fatalf("FleetOpenCountsExcludingPaused: %v", err)
		}
		if high != 1 {
			t.Fatalf("the digest must count only the active site's finding, expected 1, got %d", high)
		}
	})

	t.Run("the digest does not NAME a paused site", func(t *testing.T) {
		rows, err := vulnRepo.FleetOpenFindingsExcludingPaused(ctx, tenant, 100)
		if err != nil {
			t.Fatalf("FleetOpenFindingsExcludingPaused: %v", err)
		}
		if namesSite(rows, paused) {
			t.Fatalf("the digest must not name the paused site")
		}
		if !namesSite(rows, active) {
			t.Fatalf("the digest must still name the ACTIVE site — a digest that names nobody is not a fix")
		}
	})

	t.Run("the digest ENTRY POINT (GetFleetSummaryForDigest) excludes the paused site", func(t *testing.T) {
		// This is the caller routing GH #493 actually requires: the repo-level
		// subtests above call FleetOpenCountsExcludingPaused /
		// FleetOpenFindingsExcludingPaused directly, which stays green even if
		// the service forgot to call them. Going through the Service here is
		// what the production adapter does, so a regression that re-points
		// GetFleetSummaryForDigest at the unfiltered repo methods reddens THIS
		// subtest specifically.
		fleet, _, err := vulnSvc.GetFleetSummaryForDigest(ctx, tenant, 100)
		if err != nil {
			t.Fatalf("GetFleetSummaryForDigest: %v", err)
		}
		if fleet.High != 1 {
			t.Fatalf("the digest entry point must count only the active site's finding, expected 1, got %d", fleet.High)
		}
		if namesSiteInSummary(fleet.Findings, paused) {
			t.Fatalf("the digest entry point must not name the paused site")
		}
		if !namesSiteInSummary(fleet.Findings, active) {
			t.Fatalf("the digest entry point must still name the ACTIVE site — a digest that names nobody is not a fix")
		}
	})

	t.Run("the DASHBOARD still shows the paused site and its findings", func(t *testing.T) {
		// The trap in the issue: these two methods are shared, and filtering
		// them would be the opposite defect. An operator looking at their own
		// fleet must see a paused site's findings.
		rows, err := vulnRepo.FleetOpenFindings(ctx, tenant, 100)
		if err != nil {
			t.Fatalf("FleetOpenFindings: %v", err)
		}
		if !namesSite(rows, paused) {
			t.Fatalf("the dashboard must STILL show the paused site's findings")
		}
		if !namesSite(rows, active) {
			t.Fatalf("the dashboard must still show the active site's findings")
		}
		_, high, _, _, _, err := vulnRepo.FleetOpenCounts(ctx, tenant)
		if err != nil {
			t.Fatalf("FleetOpenCounts: %v", err)
		}
		if high != 2 {
			t.Fatalf("the dashboard count must still include the paused site, expected 2, got %d", high)
		}
	})

	// ---------------------------------------------------------------- part 1

	t.Run("the db-clean sweep's pause lookup sees the pause", func(t *testing.T) {
		got, err := perfRepo.PausedSiteIDs(ctx, []uuid.UUID{active, paused})
		if err != nil {
			t.Fatalf("PausedSiteIDs: %v", err)
		}
		if !got[paused] {
			t.Fatalf("the scheduled clean's selection filter must see the pause on %s", paused)
		}
		if got[active] {
			t.Fatalf("the ACTIVE sibling must not be filtered out of the sweep — it is still cleaned")
		}
	})

	t.Run("the db-clean point-of-action re-check agrees with the column", func(t *testing.T) {
		// This is what makes a job enqueued BEFORE the pause send no db_clean
		// after it: nothing drains River on pause.
		got, err := perfRepo.IsMonitoringPaused(ctx, paused)
		if err != nil {
			t.Fatalf("perf IsMonitoringPaused(paused): %v", err)
		}
		if !got {
			t.Fatalf("the db-clean re-check must see the pause")
		}
		got, err = perfRepo.IsMonitoringPaused(ctx, active)
		if err != nil {
			t.Fatalf("perf IsMonitoringPaused(active): %v", err)
		}
		if got {
			t.Fatalf("the db-clean re-check must NOT see a pause on the active site")
		}
	})

	// ---------------------------------------------------------------- resume

	t.Run("resuming puts the site back in both", func(t *testing.T) {
		if _, err := siteRepo.ResumeMonitoring(ctx, site.ResumeMonitoringInput{
			TenantID: tenant, ActorUserID: actor,
			Principal: monitoringPrincipal(tenant, actor),
			SiteIDs:   []uuid.UUID{paused},
		}); err != nil {
			t.Fatalf("resume: %v", err)
		}

		rows, err := vulnRepo.FleetOpenFindingsExcludingPaused(ctx, tenant, 100)
		if err != nil {
			t.Fatalf("FleetOpenFindingsExcludingPaused after resume: %v", err)
		}
		if !namesSite(rows, paused) {
			t.Fatalf("a resumed site must be named by the digest again")
		}

		got, err := perfRepo.PausedSiteIDs(ctx, []uuid.UUID{paused})
		if err != nil {
			t.Fatalf("PausedSiteIDs after resume: %v", err)
		}
		if got[paused] {
			t.Fatalf("a resumed site must be cleaned again")
		}
	})
}
