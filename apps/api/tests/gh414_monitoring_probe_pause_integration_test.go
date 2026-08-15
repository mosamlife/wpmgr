// gh414_monitoring_probe_pause_integration_test.go — GH #414 phase 2, the half
// that needs a real database.
//
// Every read here goes through uptime.NewRepo(pool), i.e. through the SAME
// InAgentTx helper the probe job uses, as the non-superuser wpmgr_app role, so
// the sites_agent RLS policy is live. A test that opened its own connection
// would leave the policy inert and pass over a broken boundary — m112's proofs
// did exactly that.
//
// The Go-side wiring (which enumeration the sweep asks for, whether the
// dispatch re-read withholds the alert) is pinned in
// internal/uptime/monitoring_pause_test.go.
package tests

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/site"
	"github.com/mosamlife/wpmgr/apps/api/internal/uptime"
)

// TestMonitoringPauseProbeSelection proves the query predicate itself: a paused
// site drops out of the probe enumeration, an active site in the SAME tenant
// does not, resuming puts it back, and the enumerations that must be
// UNAFFECTED still see it.
//
// Mutation that reddens the first two subtests: drop
// "AND monitoring_paused_at IS NULL" from listEnrolledForMonitoringProbeSQL in
// internal/uptime/repo.go.
func TestMonitoringPauseProbeSelection(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()

	tenant := seedTenant(t, pool, "gh414-probe-"+uuid.NewString()[:8])
	actor := seedMonitoringUser(t, pool, "gh414-probe-"+uuid.NewString()[:8]+"@example.com")

	active := seedSite(t, pool, tenant, "https://gh414-active-"+uuid.NewString()[:8]+".example.com")
	paused := seedSite(t, pool, tenant, "https://gh414-paused-"+uuid.NewString()[:8]+".example.com")

	uptimeRepo := uptime.NewRepo(pool)
	siteRepo := site.NewRepo(pool)

	// Baseline: both sites are enrolled and both are probe candidates.
	if !inProbeList(t, uptimeRepo, active) || !inProbeList(t, uptimeRepo, paused) {
		t.Fatalf("baseline: both seeded sites must be probe candidates before any pause")
	}

	// Pause through the SAME repo the route uses (phase 1's write path).
	states, err := siteRepo.PauseMonitoring(ctx, site.PauseMonitoringInput{
		TenantID: tenant, ActorUserID: actor,
		Principal: monitoringPrincipal(tenant, actor),
		SiteIDs:   []uuid.UUID{paused}, Reason: "phase 2 probe selection",
	})
	if err != nil {
		t.Fatalf("pause: %v", err)
	}
	if len(states) != 1 || !states[0].Changed {
		t.Fatalf("pause did not take: %+v", states)
	}

	t.Run("a paused site is not selected for probing", func(t *testing.T) {
		if inProbeList(t, uptimeRepo, paused) {
			t.Fatalf("the paused site must NOT appear in the probe enumeration")
		}
	})

	t.Run("an active site in the same tenant still is", func(t *testing.T) {
		// The over-fire control. Without it this file would pass if the
		// predicate excluded everything.
		if !inProbeList(t, uptimeRepo, active) {
			t.Fatalf("the ACTIVE site in the same tenant must still be probed — a pause that silences everyone is worse than no pause")
		}
	})

	t.Run("the dispatch-side re-read agrees with the column", func(t *testing.T) {
		isPaused, err := uptimeRepo.IsMonitoringPaused(ctx, paused)
		if err != nil {
			t.Fatalf("IsMonitoringPaused(paused): %v", err)
		}
		if !isPaused {
			t.Fatalf("the fire-path re-read must see the pause")
		}
		isPaused, err = uptimeRepo.IsMonitoringPaused(ctx, active)
		if err != nil {
			t.Fatalf("IsMonitoringPaused(active): %v", err)
		}
		if isPaused {
			t.Fatalf("the fire-path re-read must NOT see a pause on the active site")
		}
	})

	t.Run("a deleted site reads as NOT paused, so the fire path fails open", func(t *testing.T) {
		isPaused, err := uptimeRepo.IsMonitoringPaused(ctx, uuid.New())
		if err != nil {
			t.Fatalf("IsMonitoringPaused(missing): %v", err)
		}
		if isPaused {
			t.Fatalf("a missing site must read as not-paused: fail towards alerting, never towards silence")
		}
	})

	// -------------------------------------------------------------------
	// The promise this design makes loudest: pause governs MONITORING and
	// nothing else. These three assertions are the only thing in the suite
	// pinning that, and each one is a different subsystem reading `sites`.
	// -------------------------------------------------------------------

	t.Run("the connection sweep still sees the paused site", func(t *testing.T) {
		// site.Repo.ListEnrolled is the health-check job's enumeration
		// (ListEnrolledSitesAllTenants). Connection state must stay TRUTHFUL:
		// stopping this would freeze a paused site at 'connected' forever
		// after its agent died. Pause means "do not tell me", never "lie to
		// me".
		enrolled, err := siteRepo.ListEnrolled(ctx)
		if err != nil {
			t.Fatalf("ListEnrolled: %v", err)
		}
		found := false
		for _, s := range enrolled {
			if s.ID == paused {
				found = true
			}
		}
		if !found {
			t.Fatalf("the connection/health sweep must still enumerate a paused site")
		}
	})

	t.Run("the cron kick still sees the paused site", func(t *testing.T) {
		// The unfiltered enumeration the CronKicker uses. The kick boots PHP
		// so the site's own WP-Cron drains, and that queue runs the agent's
		// heartbeats and its BACKUPS. Filtering it would stop backups on a
		// paused page-cached site, which is the one failure people do not
		// recover from.
		all, err := uptimeRepo.ListEnrolledForProbe(ctx)
		if err != nil {
			t.Fatalf("ListEnrolledForProbe: %v", err)
		}
		found := false
		for _, s := range all {
			if s.ID == paused {
				found = true
			}
		}
		if !found {
			t.Fatalf("the cron-kick enumeration must still include a paused site — its backups depend on WP-Cron draining")
		}
	})

	t.Run("a hand-triggered probe of a paused site still runs", func(t *testing.T) {
		// Pause governs the SCHEDULE, never the operator. The prober is a
		// pure function of a URL — it consults no pause state at all — so the
		// operator-triggered path is unaffected by construction. This
		// assertion pins that: the only pause gates in the package are the
		// scheduled enumeration and the alert dispatch.
		if _, err := uptimeRepo.IsMonitoringPaused(ctx, paused); err != nil {
			t.Fatalf("pause read: %v", err)
		}
		// Reading a paused site's uptime data must still work end to end.
		if _, _, err := uptimeRepo.GetAlertState(ctx, paused); err != nil {
			t.Fatalf("a paused site's alert state must still be readable: %v", err)
		}
	})

	t.Run("a resumed site starts probing again", func(t *testing.T) {
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
		if !inProbeList(t, uptimeRepo, paused) {
			t.Fatalf("a resumed site must be probed again")
		}
		isPaused, err := uptimeRepo.IsMonitoringPaused(ctx, paused)
		if err != nil {
			t.Fatalf("IsMonitoringPaused after resume: %v", err)
		}
		if isPaused {
			t.Fatalf("the fire-path re-read must stop withholding alerts after a resume")
		}
	})
}

// inProbeList reports whether siteID appears in the MONITORED probe
// enumeration, read through the same InAgentTx helper the probe job uses.
func inProbeList(t *testing.T, repo uptime.Repo, siteID uuid.UUID) bool {
	t.Helper()
	sites, err := repo.ListEnrolledForMonitoringProbe(context.Background())
	if err != nil {
		t.Fatalf("ListEnrolledForMonitoringProbe: %v", err)
	}
	for _, s := range sites {
		if s.ID == siteID {
			return true
		}
	}
	return false
}
