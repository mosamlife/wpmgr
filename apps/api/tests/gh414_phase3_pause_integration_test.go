// gh414_phase3_pause_integration_test.go — GH #414 phase 3, the half that needs
// a real database.
//
// Phase 3 pauses exactly two things: the weekly screenshot fanout, and the
// vuln rescan fan-out together with the vuln alert dispatch (they must pause
// together, or findings arrive that nobody is told about).
//
// Every read goes through the production type — capture.NewDBSiteIDLister(pool)
// and vuln.NewRepo(pool) — i.e. through the SAME InAgentTx / InTenantTx helpers
// the jobs use, as the non-superuser wpmgr_app role, so the RLS policies are
// live. A test that opened its own connection would leave them inert.
//
// The Go-side wiring (which job re-checks, which caller is exempt) is pinned in
// internal/vuln/monitoring_pause_test.go and
// internal/screenshot/capture/monitoring_pause_test.go.
package tests

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/screenshot/capture"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
	"github.com/mosamlife/wpmgr/apps/api/internal/uptime"
	"github.com/mosamlife/wpmgr/apps/api/internal/vuln"
)

// inScreenshotFanout reports whether the weekly fanout's enumeration returned
// this site. Goes through the production lister, not a hand-written query.
func inScreenshotFanout(t *testing.T, l *capture.DBSiteIDLister, id uuid.UUID) bool {
	t.Helper()
	rows, err := l.ListConnectedSiteIDs(context.Background())
	if err != nil {
		t.Fatalf("ListConnectedSiteIDs: %v", err)
	}
	for _, r := range rows {
		if r.SiteID == id {
			return true
		}
	}
	return false
}

// inRescanFanout reports whether the scheduled vuln rescan's enumeration
// returned this site.
func inRescanFanout(t *testing.T, r *vuln.Repo, tenant, id uuid.UUID) bool {
	t.Helper()
	ids, err := r.ListUnpausedSiteIDsForRescan(context.Background(), tenant)
	if err != nil {
		t.Fatalf("ListUnpausedSiteIDsForRescan: %v", err)
	}
	for _, got := range ids {
		if got == id {
			return true
		}
	}
	return false
}

// seedOpenFinding inserts one open, unnotified finding for a site so the alert
// dispatch has something to claim. Written through InTenantTx (RLS live).
func seedOpenFinding(t *testing.T, pool interface {
	InTenantTx(context.Context, uuid.UUID, func(pgx.Tx) error) error
}, tenant, siteID uuid.UUID, vulnID string) {
	t.Helper()
	err := pool.InTenantTx(context.Background(), tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `
			INSERT INTO site_vulnerabilities
			  (tenant_id, site_id, vuln_id, kind, slug, name, installed_version,
			   severity, title, status, first_seen, last_seen)
			VALUES ($1, $2, $3, 'plugin', 'gh414-slug', 'GH414 Plugin', '1.0.0',
			        'high', 'GH414 test finding', 'open', now(), now())`,
			tenant, siteID, vulnID)
		return err
	})
	if err != nil {
		t.Fatalf("seed finding: %v", err)
	}
}

// TestPhase3PauseSelection proves the three query predicates phase 3 adds, the
// over-fire control for each, that resuming puts the site back, and that the
// enumerations which must be UNAFFECTED still see the paused site.
//
// Mutations that redden the pause subtests, one each:
//   - drop "AND monitoring_paused_at IS NULL" from
//     listConnectedForScheduledScreenshotSQL (internal/screenshot/capture/sitelister.go)
//   - drop it from listUnpausedSiteIDsForRescanSQL (internal/vuln/repo.go)
//   - drop the EXISTS from ClaimUnnotifiedFindings / from
//     ListTenantsWithUnnotifiedFindings (internal/vuln/repo.go)
func TestPhase3PauseSelection(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()

	tenant := seedTenant(t, pool, "gh414-p3-"+uuid.NewString()[:8])
	actor := seedMonitoringUser(t, pool, "gh414-p3-"+uuid.NewString()[:8]+"@example.com")

	active := seedSite(t, pool, tenant, "https://gh414-p3-active-"+uuid.NewString()[:8]+".example.com")
	paused := seedSite(t, pool, tenant, "https://gh414-p3-paused-"+uuid.NewString()[:8]+".example.com")

	// The screenshot fanout only enumerates connection_state = 'connected'.
	admin := connectAdmin(t, pool)
	if _, err := admin.Exec(ctx,
		`UPDATE sites SET connection_state = 'connected' WHERE id = ANY($1)`,
		[]uuid.UUID{active, paused}); err != nil {
		admin.Close()
		t.Fatalf("stage connected: %v", err)
	}
	admin.Close()

	lister := capture.NewDBSiteIDLister(pool)
	vulnRepo := vuln.NewRepo(pool)
	siteRepo := site.NewRepo(pool)
	uptimeRepo := uptime.NewRepo(pool)

	// Baseline. Without this the pause subtests would pass over a query that
	// never returned either site.
	if !inScreenshotFanout(t, lister, active) || !inScreenshotFanout(t, lister, paused) {
		t.Fatalf("baseline: both seeded sites must be screenshot-fanout candidates before any pause")
	}
	if !inRescanFanout(t, vulnRepo, tenant, active) || !inRescanFanout(t, vulnRepo, tenant, paused) {
		t.Fatalf("baseline: both seeded sites must be rescan-fanout candidates before any pause")
	}

	// Pause through the SAME repo the route uses (phase 1's write path).
	states, err := siteRepo.PauseMonitoring(ctx, site.PauseMonitoringInput{
		TenantID: tenant, ActorUserID: actor,
		Principal: monitoringPrincipal(tenant, actor),
		SiteIDs:   []uuid.UUID{paused}, Reason: "phase 3 selection",
	})
	if err != nil {
		t.Fatalf("pause: %v", err)
	}
	if len(states) != 1 || !states[0].Changed {
		t.Fatalf("pause did not take: %+v", states)
	}

	t.Run("a paused site drops out of the weekly screenshot fanout", func(t *testing.T) {
		if inScreenshotFanout(t, lister, paused) {
			t.Fatalf("the paused site must NOT appear in the screenshot fanout enumeration")
		}
	})

	t.Run("the active sibling is still screenshotted", func(t *testing.T) {
		if !inScreenshotFanout(t, lister, active) {
			t.Fatalf("the ACTIVE site must still be in the screenshot fanout — a pause that silences everyone is worse than no pause")
		}
	})

	t.Run("a paused site drops out of the scheduled vuln rescan fanout", func(t *testing.T) {
		if inRescanFanout(t, vulnRepo, tenant, paused) {
			t.Fatalf("the paused site must NOT appear in the scheduled rescan enumeration")
		}
	})

	t.Run("the active sibling is still rescanned", func(t *testing.T) {
		if !inRescanFanout(t, vulnRepo, tenant, active) {
			t.Fatalf("the ACTIVE site must still be in the rescan enumeration")
		}
	})

	t.Run("the point-of-action re-check agrees with the column", func(t *testing.T) {
		// This is what makes a job queued BEFORE the pause take no action
		// after it: nothing drains River on pause.
		got, err := vulnRepo.IsMonitoringPaused(ctx, paused)
		if err != nil {
			t.Fatalf("vuln IsMonitoringPaused(paused): %v", err)
		}
		if !got {
			t.Fatalf("the vuln re-check must see the pause")
		}
		got, err = vulnRepo.IsMonitoringPaused(ctx, active)
		if err != nil {
			t.Fatalf("vuln IsMonitoringPaused(active): %v", err)
		}
		if got {
			t.Fatalf("the vuln re-check must NOT see a pause on the active site")
		}
		got, err = lister.IsMonitoringPaused(ctx, paused)
		if err != nil {
			t.Fatalf("screenshot IsMonitoringPaused(paused): %v", err)
		}
		if !got {
			t.Fatalf("the screenshot re-check must see the pause")
		}
	})

	t.Run("a deleted site reads as NOT paused on both re-checks", func(t *testing.T) {
		missing := uuid.New()
		for name, got := range map[string]func() (bool, error){
			"vuln":       func() (bool, error) { return vulnRepo.IsMonitoringPaused(ctx, missing) },
			"screenshot": func() (bool, error) { return lister.IsMonitoringPaused(ctx, missing) },
		} {
			paused, err := got()
			if err != nil {
				t.Fatalf("%s IsMonitoringPaused(missing): %v", name, err)
			}
			if paused {
				t.Fatalf("%s: a missing site must read as not-paused, never as an invented pause", name)
			}
		}
	})

	// -------------------------------------------------------------------
	// The alert dispatch. Rescan and dispatch must pause TOGETHER: a rescan
	// that still ran (an operator clicked Rescan on a paused site) must not
	// produce an email.
	// -------------------------------------------------------------------

	t.Run("a paused site's findings are not claimed, and survive to be sent on resume", func(t *testing.T) {
		seedOpenFinding(t, pool, tenant, paused, "gh414-paused-"+uuid.NewString()[:8])

		tenants, err := vulnRepo.ListTenantsWithUnnotifiedFindings(ctx)
		if err != nil {
			t.Fatalf("ListTenantsWithUnnotifiedFindings: %v", err)
		}
		if containsTenant(tenants, tenant) {
			t.Fatalf("a tenant whose ONLY unnotified findings sit on paused sites must not be dispatched for")
		}

		var claimed []vuln.ClaimedFinding
		if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
			var cErr error
			claimed, cErr = vulnRepo.ClaimUnnotifiedFindings(ctx, tx, tenant)
			return cErr
		}); err != nil {
			t.Fatalf("ClaimUnnotifiedFindings: %v", err)
		}
		if len(claimed) != 0 {
			t.Fatalf("a paused site's findings must not be claimed, claimed %d", len(claimed))
		}

		// Not-claiming is what makes resume work: the row is still unnotified.
		var n int
		if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx,
				`SELECT count(*) FROM site_vulnerabilities
				  WHERE tenant_id = $1 AND site_id = $2 AND notified_at IS NULL`,
				tenant, paused).Scan(&n)
		}); err != nil {
			t.Fatalf("re-read finding: %v", err)
		}
		if n != 1 {
			t.Fatalf("the paused site's finding must still be unnotified after ClaimUnnotifiedFindings excluded it, got %d unnotified rows (want 1)", n)
		}
	})

	t.Run("the active sibling's findings are still claimed and sent", func(t *testing.T) {
		seedOpenFinding(t, pool, tenant, active, "gh414-active-"+uuid.NewString()[:8])

		tenants, err := vulnRepo.ListTenantsWithUnnotifiedFindings(ctx)
		if err != nil {
			t.Fatalf("ListTenantsWithUnnotifiedFindings: %v", err)
		}
		if !containsTenant(tenants, tenant) {
			t.Fatalf("a tenant with an unnotified finding on an ACTIVE site must be dispatched for")
		}

		var claimed []vuln.ClaimedFinding
		if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
			var cErr error
			claimed, cErr = vulnRepo.ClaimUnnotifiedFindings(ctx, tx, tenant)
			return cErr
		}); err != nil {
			t.Fatalf("ClaimUnnotifiedFindings: %v", err)
		}
		if len(claimed) != 1 {
			t.Fatalf("exactly the active site's finding must be claimed, claimed %d", len(claimed))
		}
		if claimed[0].Finding.SiteID != active {
			t.Fatalf("the claimed finding must belong to the ACTIVE site %s, got %s", active, claimed[0].Finding.SiteID)
		}
	})

	// -------------------------------------------------------------------
	// Resume.
	// -------------------------------------------------------------------

	t.Run("a resumed site resumes everywhere at once", func(t *testing.T) {
		states, err := siteRepo.ResumeMonitoring(ctx, site.ResumeMonitoringInput{
			TenantID: tenant, ActorUserID: actor,
			Principal: monitoringPrincipal(tenant, actor),
			SiteIDs:   []uuid.UUID{paused},
		})
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
		if len(states) != 1 || !states[0].Changed {
			t.Fatalf("resume did not take: %+v", states)
		}

		if !inScreenshotFanout(t, lister, paused) {
			t.Fatalf("a resumed site must be screenshotted again")
		}
		if !inRescanFanout(t, vulnRepo, tenant, paused) {
			t.Fatalf("a resumed site must be rescanned again")
		}
		if got, err := vulnRepo.IsMonitoringPaused(ctx, paused); err != nil || got {
			t.Fatalf("the re-check must clear on resume (paused=%v err=%v)", got, err)
		}

		// The finding withheld during the pause is still unnotified, so the
		// first dispatch after the resume sends it. This is the whole reason
		// the claim EXCLUDES rather than stamps.
		tenants, err := vulnRepo.ListTenantsWithUnnotifiedFindings(ctx)
		if err != nil {
			t.Fatalf("ListTenantsWithUnnotifiedFindings after resume: %v", err)
		}
		if !containsTenant(tenants, tenant) {
			t.Fatalf("the finding withheld during the pause must be dispatched after the resume, not eaten")
		}
		var claimed []vuln.ClaimedFinding
		if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
			var cErr error
			claimed, cErr = vulnRepo.ClaimUnnotifiedFindings(ctx, tx, tenant)
			return cErr
		}); err != nil {
			t.Fatalf("ClaimUnnotifiedFindings after resume: %v", err)
		}
		if len(claimed) != 1 || claimed[0].Finding.SiteID != paused {
			t.Fatalf("the resumed site's withheld finding must be the one claimed, got %d claimed", len(claimed))
		}
	})

	// -------------------------------------------------------------------
	// What phase 3 must NOT have touched. Each is a different subsystem
	// reading `sites`, and each would be a silent, unrecoverable failure.
	// -------------------------------------------------------------------

	t.Run("the never-stops enumerations still see a paused site", func(t *testing.T) {
		// Re-pause: the resume subtest above cleared it.
		if _, err := siteRepo.PauseMonitoring(ctx, site.PauseMonitoringInput{
			TenantID: tenant, ActorUserID: actor,
			Principal: monitoringPrincipal(tenant, actor),
			SiteIDs:   []uuid.UUID{paused}, Reason: "phase 3 never-stops",
		}); err != nil {
			t.Fatalf("re-pause: %v", err)
		}

		// THE CRON KICK. It boots WP-Cron, which runs the agent's scheduled
		// BACKUPS. Filtering paused sites here stops backups on every
		// page-cached paused site — the failure people do not recover from.
		// Mutation that reddens this: add "AND monitoring_paused_at IS NULL"
		// to ListEnrolledSitesForProbe in db/query/sites.sql.
		probe, err := uptimeRepo.ListEnrolledForProbe(ctx)
		if err != nil {
			t.Fatalf("ListEnrolledForProbe: %v", err)
		}
		if !containsEnrolledSite(probe, paused) {
			t.Fatalf("the CRON KICK's enumeration must still contain a paused site — it is what runs backups")
		}
		if !containsEnrolledSite(probe, active) {
			t.Fatalf("over-fire control: the active site must be in the cron-kick enumeration too")
		}

		// site.Repo.ListAllSiteIDs — the enumeration phase 3 deliberately did
		// NOT filter, because nine other callers (org's purge worker, perf's
		// operator RUM handler, the CLI adapters) depend on it returning ALL
		// sites. Mutation that reddens this: add the pause predicate to
		// ListAllSiteIDs in db/query/sites.sql.
		all, err := site.NewService(siteRepo, nil, nil).ListAllSiteIDs(ctx, tenant)
		if err != nil {
			t.Fatalf("ListAllSiteIDs: %v", err)
		}
		if !containsUUID(all, paused) {
			t.Fatalf("ListAllSiteIDs must still return a paused site: the delete path and the operator RUM handler both read it")
		}
	})
}

// containsTenant already exists in gh402_site_object_reclaim_integration_test.go
// (same package); containsUUID is this file's own, named apart from it.
func containsUUID(ids []uuid.UUID, want uuid.UUID) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

func containsEnrolledSite(sites []uptime.EnrolledSite, want uuid.UUID) bool {
	for _, s := range sites {
		if s.ID == want {
			return true
		}
	}
	return false
}
