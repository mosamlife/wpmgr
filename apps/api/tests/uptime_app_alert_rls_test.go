// uptime_app_alert_rls_test.go - GH #291 Phase 3: RLS coverage for the two
// new tenant-scoped tables (site_app_alert_state, tenant_app_alert_breaker)
// and the per-site app-health settings columns on `sites`. Mirrors
// TestUptimeRollupTablesRLS's pattern exactly (tenant_isolation read scoping
// + agent cross-tenant read + WITH CHECK on a cross-tenant write attempt).
package tests

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/uptime"
)

// TestUptimeAppAlertStateRLS proves site_app_alert_state enforces tenant
// isolation on reads, is fully visible under app.agent (the probe worker's
// cross-tenant transition path), and rejects a cross-tenant WITH CHECK
// write.
func TestUptimeAppAlertStateRLS(t *testing.T) {
	app := startPostgres(t)
	admin := connectAdmin(t, app)
	defer admin.Close()
	ctx := context.Background()

	tenantA := seedTenant(t, app, "app-alert-state-rls-a-"+uuid.NewString()[:8])
	tenantB := seedTenant(t, app, "app-alert-state-rls-b-"+uuid.NewString()[:8])
	siteA := seedSiteFor(t, admin, tenantA, "https://"+uuid.NewString()+".example.com")
	siteA2 := seedSiteFor(t, admin, tenantA, "https://"+uuid.NewString()+".example.com")

	if _, err := admin.Exec(ctx,
		`INSERT INTO site_app_alert_state (site_id, tenant_id, last_status, consecutive_down, in_incident, ever_app_up)
		 VALUES ($1, $2, 'down', 1, false, true)`,
		siteA, tenantA); err != nil {
		t.Fatalf("seed site_app_alert_state: %v", err)
	}

	count := func(run func(fn func(pgx.Tx) error) error) int {
		var n int
		if err := run(func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM site_app_alert_state WHERE site_id = $1`, siteA).Scan(&n)
		}); err != nil {
			t.Fatalf("count query: %v", err)
		}
		return n
	}

	if got := count(func(fn func(pgx.Tx) error) error { return app.InTenantTx(ctx, tenantA, fn) }); got != 1 {
		t.Fatalf("tenant A must see its own row, got %d", got)
	}
	if got := count(func(fn func(pgx.Tx) error) error { return app.InTenantTx(ctx, tenantB, fn) }); got != 0 {
		t.Fatalf("tenant B must NOT see tenant A's row (tenant_isolation), got %d", got)
	}
	if got := count(func(fn func(pgx.Tx) error) error { return app.InAgentTx(ctx, fn) }); got != 1 {
		t.Fatalf("agent scope must see the row cross-tenant (agent policy), got %d", got)
	}

	// WITH CHECK: tenant B cannot write a row carrying tenant A's id, even
	// for a real site of A's.
	err := app.InTenantTx(ctx, tenantB, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx,
			`INSERT INTO site_app_alert_state (site_id, tenant_id, last_status, consecutive_down, in_incident, ever_app_up)
			 VALUES ($1, $2, 'down', 1, false, true)`,
			siteA2, tenantA)
		return e
	})
	if err == nil {
		t.Fatal("tenant B must NOT be able to write a site_app_alert_state row for tenant A (WITH CHECK)")
	}

	var total int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM site_app_alert_state`).Scan(&total); err != nil {
		t.Fatalf("total count: %v", err)
	}
	if total != 1 {
		t.Fatalf("expected exactly 1 site_app_alert_state row overall, got %d", total)
	}
}

// TestTenantAppAlertBreakerRLS mirrors TestUptimeAppAlertStateRLS for the
// tenant-wide circuit-breaker table.
func TestTenantAppAlertBreakerRLS(t *testing.T) {
	app := startPostgres(t)
	admin := connectAdmin(t, app)
	defer admin.Close()
	ctx := context.Background()

	tenantA := seedTenant(t, app, "app-breaker-rls-a-"+uuid.NewString()[:8])
	tenantB := seedTenant(t, app, "app-breaker-rls-b-"+uuid.NewString()[:8])

	if _, err := admin.Exec(ctx,
		`INSERT INTO tenant_app_alert_breaker (tenant_id, tripped, tripped_at) VALUES ($1, true, now())`,
		tenantA); err != nil {
		t.Fatalf("seed tenant_app_alert_breaker: %v", err)
	}

	count := func(run func(fn func(pgx.Tx) error) error) int {
		var n int
		if err := run(func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM tenant_app_alert_breaker WHERE tenant_id = $1`, tenantA).Scan(&n)
		}); err != nil {
			t.Fatalf("count query: %v", err)
		}
		return n
	}

	if got := count(func(fn func(pgx.Tx) error) error { return app.InTenantTx(ctx, tenantA, fn) }); got != 1 {
		t.Fatalf("tenant A must see its own row, got %d", got)
	}
	if got := count(func(fn func(pgx.Tx) error) error { return app.InTenantTx(ctx, tenantB, fn) }); got != 0 {
		t.Fatalf("tenant B must NOT see tenant A's row (tenant_isolation), got %d", got)
	}
	if got := count(func(fn func(pgx.Tx) error) error { return app.InAgentTx(ctx, fn) }); got != 1 {
		t.Fatalf("agent scope must see the row cross-tenant (agent policy), got %d", got)
	}

	err := app.InTenantTx(ctx, tenantB, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx,
			`INSERT INTO tenant_app_alert_breaker (tenant_id, tripped) VALUES ($1, true)`, tenantA)
		return e
	})
	if err == nil {
		t.Fatal("tenant B must NOT be able to write a tenant_app_alert_breaker row for tenant A (WITH CHECK)")
	}
}

// TestAppHealthSettingsRepo_TenantIsolation proves uptime.Repo.
// GetAppHealthSettings/UpdateAppHealthSettings (InTenantTx, GH #291 Phase 3
// section 3) never read or write a foreign tenant's site, and that a
// cleared app_probe_path round-trips to empty (NULL) rather than the
// literal string that was previously set.
func TestAppHealthSettingsRepo_TenantIsolation(t *testing.T) {
	app := startPostgres(t)
	admin := connectAdmin(t, app)
	defer admin.Close()
	ctx := context.Background()

	tenantA := seedTenant(t, app, "app-health-settings-a-"+uuid.NewString()[:8])
	tenantB := seedTenant(t, app, "app-health-settings-b-"+uuid.NewString()[:8])
	siteA := seedSiteFor(t, admin, tenantA, "https://"+uuid.NewString()+".example.com")

	repo := uptime.NewRepo(app)

	saved, found, err := repo.UpdateAppHealthSettings(ctx, tenantA, siteA, "/healthz", true)
	if err != nil {
		t.Fatalf("update app health settings: %v", err)
	}
	if !found {
		t.Fatal("expected found=true for the owning tenant")
	}
	if saved.AppProbePath != "/healthz" || !saved.AppAlertsDisabled {
		t.Fatalf("unexpected saved settings: %+v", saved)
	}

	// Tenant B must not be able to read or write tenant A's site.
	if _, found, err := repo.GetAppHealthSettings(ctx, tenantB, siteA); err != nil {
		t.Fatalf("get as tenant B: %v", err)
	} else if found {
		t.Fatal("tenant B must NOT see tenant A's site (RLS + tenant_id predicate)")
	}
	if _, found, err := repo.UpdateAppHealthSettings(ctx, tenantB, siteA, "/other", false); err != nil {
		t.Fatalf("update as tenant B: %v", err)
	} else if found {
		t.Fatal("tenant B must NOT be able to update tenant A's site")
	}

	// Clearing the override round-trips to empty.
	cleared, found, err := repo.UpdateAppHealthSettings(ctx, tenantA, siteA, "", false)
	if err != nil || !found {
		t.Fatalf("clear override: found=%v err=%v", found, err)
	}
	if cleared.AppProbePath != "" || cleared.AppAlertsDisabled {
		t.Fatalf("expected a cleared/re-enabled state, got %+v", cleared)
	}
}
