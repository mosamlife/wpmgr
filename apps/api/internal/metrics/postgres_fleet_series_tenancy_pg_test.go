package metrics

// postgres_fleet_series_tenancy_pg_test.go — tenancy and retention-seam
// proofs for QueryFleetDailySeries (GH #460), established BY EXECUTION as
// wpmgr_app (NOSUPERUSER NOBYPASSRLS).
//
// Originally written during the security review of PR #478, which found the
// branch had no cross-tenant case; lifted in rather than restated, because a
// proof written by someone trying to break the code is worth more than one
// written by the author who believed it already worked.
//
// Both tests keep a POSITIVE CONTROL. Without one, "the foreign tenant got
// nothing" passes just as convincingly when the seed never landed, and an
// isolation proof that cannot tell those apart is proving nothing.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestQueryFleetDailySeries_CannotCrossTenant passes a FOREIGN site id in
// the batch and asserts nothing comes back for it, with a positive control
// proving that same site DOES have days when asked for as its own tenant.
func TestQueryFleetDailySeries_CannotCrossTenant(t *testing.T) {
	app, admin := startMetricsTestPostgres(t)
	ctx := context.Background()
	store := NewPostgres(app, nil)

	const window = 30 * 24 * time.Hour
	_, today, _, _, _, _, _ := fleetUptimeParams(time.Now(), window)

	// Assert the role really is the unprivileged one, so this proof cannot
	// pass because it silently ran as superuser.
	var role string
	var super, bypass bool
	if err := app.QueryRow(ctx,
		`SELECT current_user, rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).
		Scan(&role, &super, &bypass); err != nil {
		t.Fatalf("role introspection: %v", err)
	}
	t.Logf("connected as %s (superuser=%v bypassrls=%v)", role, super, bypass)
	if role != "wpmgr_app" || super || bypass {
		t.Fatalf("proof must run as wpmgr_app NOSUPERUSER NOBYPASSRLS, got %s super=%v bypass=%v", role, super, bypass)
	}

	tenantA := metricsSeedTenant(t, admin, "fleetseries-xt-a-"+uuid.NewString()[:8])
	tenantB := metricsSeedTenant(t, admin, "fleetseries-xt-b-"+uuid.NewString()[:8])
	siteA := metricsSeedSite(t, admin, tenantA, "https://"+uuid.NewString()+".example.com")
	siteB := metricsSeedSite(t, admin, tenantB, "https://"+uuid.NewString()+".example.com")

	// Both sites measured on the SAME days, so any leak is unambiguous, and
	// across all three decomposition parts: the rollup middle (d=20),
	// the raw boundary day (d=29, the oldest labelled day) and today (d=0).
	var checks []Check
	for _, d := range []int{29, 20, 0} {
		day := today.AddDate(0, 0, -d)
		at := day.Add(6 * time.Hour)
		if d == 0 {
			at = time.Now().UTC().Add(-time.Minute)
		}
		checks = append(checks,
			Check{TenantID: tenantA, SiteID: siteA, CheckedAt: at, Up: true, TotalMs: 100},
			Check{TenantID: tenantB, SiteID: siteB, CheckedAt: at, Up: true, TotalMs: 100},
		)
	}
	seedWithRollup(t, store, checks)

	// POSITIVE CONTROL: tenant B really does have days. Without this the
	// cross-tenant assertion below could pass because the seed never landed.
	ctrl, err := store.QueryFleetDailySeries(ctx, tenantB, []uuid.UUID{siteB}, window)
	if err != nil {
		t.Fatalf("control QueryFleetDailySeries(tenantB): %v", err)
	}
	if len(ctrl[siteB]) != 3 {
		t.Fatalf("control: tenant B site has %d days, want 3 — the seed did not land, so the leak assertion below would be vacuous", len(ctrl[siteB]))
	}
	t.Logf("control: tenant B site %s has %d measured days", siteB, len(ctrl[siteB]))

	// THE PROOF: tenant A asks for its own site AND tenant B's site.
	got, err := store.QueryFleetDailySeries(ctx, tenantA, []uuid.UUID{siteA, siteB}, window)
	if err != nil {
		t.Fatalf("QueryFleetDailySeries(tenantA, [siteA, siteB]): %v", err)
	}
	if pts, present := got[siteB]; present {
		t.Errorf("CROSS-TENANT LEAK: tenant A received %d days for tenant B's site %s", len(pts), siteB)
		for _, p := range pts {
			t.Errorf("  leaked day %s checks=%d up=%d", p.Bucket.Format("2006-01-02"), p.Checks, p.UpChecks)
		}
	}
	if len(got[siteA]) != 3 {
		t.Errorf("tenant A got %d days for its own site, want 3 — tenant scoping must not over-fire", len(got[siteA]))
	}
	if len(got) != 1 {
		t.Errorf("result map has %d sites, want exactly 1 (siteA)", len(got))
	}
	for _, p := range got[siteA] {
		if p.Checks != 1 {
			t.Errorf("day %s has checks=%d, want 1 — a value of 2 means tenant B's probe was folded into tenant A's day",
				p.Bucket.Format("2006-01-02"), p.Checks)
		}
	}
	t.Logf("tenant A result: %d site(s), %d days for its own site, 0 for the foreign site", len(got), len(got[siteA]))
}

// TestQueryFleetDailySeries_OldestDayIsServedFromRawProbesAlone pins the seam
// the review is most worried about: the OLDEST labelled day of a 90d window
// is excluded from the rollup middle (day > boundaryDay) and served only from
// site_uptime_probes, whose GC retention is exactly 90 days.
func TestQueryFleetDailySeries_OldestDayIsServedFromRawProbesAlone(t *testing.T) {
	app, admin := startMetricsTestPostgres(t)
	ctx := context.Background()
	store := NewPostgres(app, nil)

	const days = 90
	now := time.Now().UTC()
	today := now.Truncate(24 * time.Hour)
	start := today.AddDate(0, 0, -(days - 1))
	window := now.Sub(start)

	boundaryDay, _, _, _, retentionCutoff, _, _ := fleetUptimeParams(now, window)
	t.Logf("start(oldest labelled day)=%s boundaryDay=%s retentionCutoff=%s margin=%s",
		start.Format(time.RFC3339), boundaryDay.Format(time.RFC3339),
		retentionCutoff.Format(time.RFC3339), start.Sub(retentionCutoff))
	if !boundaryDay.Equal(start) {
		t.Fatalf("boundaryDay %s != oldest labelled day %s — the review's model of the seam is wrong", boundaryDay, start)
	}

	tenant := metricsSeedTenant(t, admin, "fleetseries-seam-"+uuid.NewString()[:8])
	site := metricsSeedSite(t, admin, tenant, "https://"+uuid.NewString()+".example.com")

	oldest := start.Add(6 * time.Hour)
	checks := []Check{{TenantID: tenant, SiteID: site, CheckedAt: oldest, Up: true, TotalMs: 100}}
	seedWithRollup(t, store, checks)

	got, err := store.QueryFleetDailySeries(ctx, tenant, []uuid.UUID{site}, window)
	if err != nil {
		t.Fatalf("QueryFleetDailySeries: %v", err)
	}
	if len(got[site]) != 1 {
		t.Fatalf("oldest day: got %d points, want 1", len(got[site]))
	}
	if b := got[site][0].Bucket.Format("2006-01-02"); b != start.Format("2006-01-02") {
		t.Fatalf("oldest point bucketed at %s, want %s", b, start.Format("2006-01-02"))
	}
	t.Logf("oldest day %s served, checks=%d", got[site][0].Bucket.Format("2006-01-02"), got[site][0].Checks)

	// Now delete ONLY the raw probes for that day, leaving the rollup row
	// intact — exactly what UptimeProbeGCWorker does when the day tips past
	// probeRetention (it prunes site_uptime_daily on a LATER, day-truncated
	// cutoff). If the day then vanishes, the oldest cell of a 90d strip goes
	// null while the rollup still holds the answer.
	if _, err := admin.Exec(ctx, `DELETE FROM site_uptime_probes WHERE site_id = $1`, site); err != nil {
		t.Fatalf("delete raw probes: %v", err)
	}
	var rollupRows int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM site_uptime_daily WHERE site_id = $1`, site).Scan(&rollupRows); err != nil {
		t.Fatalf("count rollup: %v", err)
	}
	t.Logf("after deleting raw probes: %d rollup row(s) remain", rollupRows)
	if rollupRows == 0 {
		t.Fatalf("rollup row absent — cannot distinguish the seam from a missing seed")
	}

	after, err := store.QueryFleetDailySeries(ctx, tenant, []uuid.UUID{site}, window)
	if err != nil {
		t.Fatalf("QueryFleetDailySeries after prune: %v", err)
	}
	t.Logf("SEAM RESULT: with the rollup row present but raw probes pruned, the oldest day returns %d point(s)", len(after[site]))

	// ASSERT the seam rather than merely reporting it. As written, this test
	// ended at the log line above, which cannot fail and therefore tests
	// nothing — the exact shape this project treats as a defect.
	//
	// The expected value is 0: the boundary day is excluded from the rollup
	// middle (`day > $3::date`), so pruning its raw probes blanks the cell
	// even though site_uptime_daily still holds the answer. That is the
	// behaviour probeRetention's comment and
	// TestProbeRetentionCoversTheLongestHistoryWindow exist to keep
	// unreachable in production, where retention always outruns the window.
	//
	// If someone later teaches part 1 to serve a fully-in-window boundary day
	// from the rollup, this goes red and should be updated to 1 — deliberately,
	// with the retention comment updated in the same change.
	if got := len(after[site]); got != 0 {
		t.Errorf("after pruning raw probes the oldest day returned %d point(s), want 0 — "+
			"the seam has moved; update probeRetention's comment and the margin test together", got)
	}
}
