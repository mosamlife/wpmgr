// gh414_app_alert_ratio_sql_pause_test.go — GH #414 phase 2, finding 1.
//
// The two shipped SQL predicates that stop a paused site from silencing an
// UNPAUSED one had nothing pinning them: deleting
// `AND s.monitoring_paused_at IS NULL` from GetTenantAppAlertRatio (and the
// identical line in ListTenantAppDownSites) left the entire suite green,
// because every committed test for that behaviour drives a Go fake of the
// query rather than the query. A fake cannot regress when the SQL does.
//
// Everything here therefore runs the REAL generated query, through the REAL
// uptime.Repo (which wraps each call in db.Pool.InAgentTx and sets app.agent),
// against a REAL Postgres with every migration applied, connected as the
// non-superuser, non-BYPASSRLS wpmgr_app role that startPostgres provisions.
// No test-local SQL restates the predicate; if the predicate leaves the query,
// these tests are the thing that notices.
//
// The pause itself is written through db.Pool.InTenantTx — the same helper and
// the same role the request path uses, so sites_tenant_isolation is live for
// the write — rather than through site.PauseMonitoring, because the subject
// under test is what the ratio query does with a paused row, not how the row
// came to be paused (gh414_monitoring_pause_routes_test.go covers that).
package tests

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
	"github.com/mosamlife/wpmgr/apps/api/internal/uptime"
)

// appAlertFixtureSite is one row of the fleet each test below builds.
type appAlertFixtureSite struct {
	name       string
	everAppUp  bool
	inIncident bool
	paused     bool
}

// seedAppAlertFleet creates the sites and their site_app_alert_state rows,
// pausing the ones the fixture marks paused, and returns the site ids by name.
func seedAppAlertFleet(t *testing.T, pool *db.Pool, tenantID uuid.UUID, fleet []appAlertFixtureSite) map[string]uuid.UUID {
	t.Helper()
	ctx := context.Background()
	repo := site.NewRepo(pool)
	ids := make(map[string]uuid.UUID, len(fleet))

	for _, f := range fleet {
		created, err := repo.Create(ctx, site.CreateInput{
			TenantID: tenantID,
			URL:      "https://" + f.name + ".example.com",
			Name:     f.name,
		})
		if err != nil {
			t.Fatalf("create site %s: %v", f.name, err)
		}
		ids[f.name] = created.ID

		// site_app_alert_state is written by the probe worker under the tenant
		// GUC; InTenantTx is that path, and the RLS policy on the table is
		// live for this insert.
		err = pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				INSERT INTO site_app_alert_state
				    (site_id, tenant_id, last_status, consecutive_down, in_incident, ever_app_up)
				VALUES ($1, $2, $3, $4, $5, $6)`,
				created.ID, tenantID,
				map[bool]string{true: "down", false: "up"}[f.inIncident],
				map[bool]int{true: 3, false: 0}[f.inIncident],
				f.inIncident, f.everAppUp)
			return err
		})
		if err != nil {
			t.Fatalf("seed app alert state for %s: %v", f.name, err)
		}

		if f.paused {
			err = pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `
					UPDATE sites
					   SET monitoring_paused_at = now(),
					       monitoring_paused_reason = 'gh414 finding 1 fixture'
					 WHERE id = $1 AND tenant_id = $2`, created.ID, tenantID)
				return err
			})
			if err != nil {
				t.Fatalf("pause %s: %v", f.name, err)
			}
		}
	}
	return ids
}

// TestGH414_AppAlertRatioSQL_ExcludesPausedSitesFromBothSides is the pin for
// GetTenantAppAlertRatio's pause predicate.
//
// Fleet: two unpaused sites in an open app incident, one unpaused and healthy,
// and two paused sites frozen mid-incident — the ordinary reason to pause a
// site is that it is broken, so the frozen in_incident=true rows are the
// realistic case, not a contrived one.
//
// The truth the breaker must be told is 2 down of 3 eligible. A paused site
// must leave the NUMERATOR (or its frozen `true` counts as down forever and
// permanently trips the breaker, silencing every unpaused site in the tenant)
// and the DENOMINATOR in the same predicate (or the ratio is understated and
// the breaker trips later than it should).
//
// RED: delete `AND s.monitoring_paused_at IS NULL` from GetTenantAppAlertRatio
// in apps/api/db/query/alerts.sql and regenerate — the query answers 4 down of
// 5 eligible and this test names itself.
func TestGH414_AppAlertRatioSQL_ExcludesPausedSitesFromBothSides(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenantID := seedTenant(t, pool, "gh414-ratio-sql")

	seedAppAlertFleet(t, pool, tenantID, []appAlertFixtureSite{
		{name: "ratio-down-a", everAppUp: true, inIncident: true},
		{name: "ratio-down-b", everAppUp: true, inIncident: true},
		{name: "ratio-healthy-c", everAppUp: true},
		{name: "ratio-paused-d", everAppUp: true, inIncident: true, paused: true},
		{name: "ratio-paused-e", everAppUp: true, inIncident: true, paused: true},
	})

	eligible, down, err := uptime.NewRepo(pool).GetTenantAppAlertRatio(ctx, tenantID)
	if err != nil {
		t.Fatalf("GetTenantAppAlertRatio: %v", err)
	}
	if eligible != 3 {
		t.Fatalf("eligible must exclude the two paused sites: want 3, got %d (down=%d)", eligible, down)
	}
	if down != 2 {
		t.Fatalf("down must exclude the two paused sites frozen mid-incident: want 2, got %d (eligible=%d)", down, eligible)
	}
}

// TestGH414_AppAlertRatioSQL_PausingEveryDownSiteEmptiesTheNumerator is the
// severe half stated on its own: pause every site that is currently down and
// the tenant's down count must fall to zero, so wantTrip goes false and the
// breaker RECOVERS rather than staying tripped forever over frozen rows. This
// is the exact state a fully-paused-mid-incident tenant lands in.
//
// RED: same deletion — down comes back 2 and the breaker never recovers.
func TestGH414_AppAlertRatioSQL_PausingEveryDownSiteEmptiesTheNumerator(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenantID := seedTenant(t, pool, "gh414-ratio-frozen")

	seedAppAlertFleet(t, pool, tenantID, []appAlertFixtureSite{
		{name: "frozen-a", everAppUp: true, inIncident: true, paused: true},
		{name: "frozen-b", everAppUp: true, inIncident: true, paused: true},
		{name: "frozen-c", everAppUp: true},
	})

	eligible, down, err := uptime.NewRepo(pool).GetTenantAppAlertRatio(ctx, tenantID)
	if err != nil {
		t.Fatalf("GetTenantAppAlertRatio: %v", err)
	}
	if down != 0 {
		t.Fatalf("every down site is paused, so down must be 0, got %d (eligible=%d)", down, eligible)
	}
	if eligible != 1 {
		t.Fatalf("only the unpaused healthy site is eligible: want 1, got %d", eligible)
	}
}

// TestGH414_AppDownSitesSQL_NeverNamesAPausedSite is the pin for
// ListTenantAppDownSites's pause predicate — the query whose contract is that
// it names EXACTLY the rows GetTenantAppAlertRatio counted as down. The two
// predicates must move together or the aggregate mail's prose contradicts its
// own numbers, which is the originally reported symptom.
//
// RED: delete `AND s.monitoring_paused_at IS NULL` from ListTenantAppDownSites
// in apps/api/db/query/alerts.sql and regenerate — the paused site is named.
func TestGH414_AppDownSitesSQL_NeverNamesAPausedSite(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenantID := seedTenant(t, pool, "gh414-downlist-sql")

	seedAppAlertFleet(t, pool, tenantID, []appAlertFixtureSite{
		{name: "list-down-a", everAppUp: true, inIncident: true},
		{name: "list-down-b", everAppUp: true, inIncident: true},
		{name: "list-healthy-c", everAppUp: true},
		{name: "list-paused-d", everAppUp: true, inIncident: true, paused: true},
	})

	repo := uptime.NewRepo(pool)
	names, err := repo.ListTenantAppDownSites(ctx, tenantID, 100)
	if err != nil {
		t.Fatalf("ListTenantAppDownSites: %v", err)
	}
	for _, n := range names {
		if n == "list-paused-d" {
			t.Fatalf("a paused site must never be named in the aggregate's affected list, got %v", names)
		}
		if n == "list-healthy-c" {
			t.Fatalf("a site that is not in an incident must not be named, got %v", names)
		}
	}
	if len(names) != 2 {
		t.Fatalf("exactly the two unpaused down sites must be named: want 2, got %d (%v)", len(names), names)
	}

	// The two queries must agree, which is the whole contract: the list length
	// equals the ratio's down count for a tenant small enough that row_limit
	// cannot be what truncated it.
	_, down, err := repo.GetTenantAppAlertRatio(ctx, tenantID)
	if err != nil {
		t.Fatalf("GetTenantAppAlertRatio: %v", err)
	}
	if down != len(names) {
		t.Fatalf("the down count and the named list must agree: down=%d, named=%d (%v)", down, len(names), names)
	}
}

// TestGH414_AppAlertRatioSQL_UnpausedFleetIsUnchanged is the over-fire
// control. A guard that reddens correct work gets switched off, and then it
// guards nothing: with NOTHING paused, both queries must answer exactly what
// they answered before m117 existed — the pause predicate must be a filter on
// paused rows, never a general silencer.
//
// RED: replace the predicate with `AND s.monitoring_paused_at IS NOT NULL`, or
// widen it in any way that drops an active site.
func TestGH414_AppAlertRatioSQL_UnpausedFleetIsUnchanged(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenantID := seedTenant(t, pool, "gh414-ratio-control")

	seedAppAlertFleet(t, pool, tenantID, []appAlertFixtureSite{
		{name: "control-down-a", everAppUp: true, inIncident: true},
		{name: "control-down-b", everAppUp: true, inIncident: true},
		{name: "control-healthy-c", everAppUp: true},
		{name: "control-healthy-d", everAppUp: true},
		// Never conclusively observed healthy: excluded from BOTH sides by the
		// pre-existing ever_app_up gate, with no help from the pause predicate.
		{name: "control-never-up-e", inIncident: true},
	})

	repo := uptime.NewRepo(pool)
	eligible, down, err := repo.GetTenantAppAlertRatio(ctx, tenantID)
	if err != nil {
		t.Fatalf("GetTenantAppAlertRatio: %v", err)
	}
	if eligible != 4 || down != 2 {
		t.Fatalf("an unpaused fleet must be counted exactly as before: want eligible=4 down=2, got eligible=%d down=%d", eligible, down)
	}
	names, err := repo.ListTenantAppDownSites(ctx, tenantID, 100)
	if err != nil {
		t.Fatalf("ListTenantAppDownSites: %v", err)
	}
	if len(names) != 2 {
		t.Fatalf("an unpaused fleet's down list must be unchanged: want 2 names, got %d (%v)", len(names), names)
	}
}
