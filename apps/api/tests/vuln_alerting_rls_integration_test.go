// vuln_alerting_rls_integration_test.go — m103 (GH #247) RLS regression:
// proves the new alert_configs vulnerability-alerting columns and the new
// site_vulnerabilities.notified_at column are covered by the SAME
// tenant_isolation + agent (cross-tenant) policies as every pre-existing
// column on those tables — a foreign tenant cannot read another tenant's
// findings/config, and the app.agent sweep used by the dispatch job's
// ListTenantsWithUnnotifiedFindings/ClaimUnnotifiedFindings can enumerate
// across tenants. Mirrors tests/sitetag_rls_integration_test.go's pattern.
// Requires Docker; skips when unavailable (via startPostgres).
package tests

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/uptime"
	"github.com/mosamlife/wpmgr/apps/api/internal/vuln"
)

// TestVulnAlertConfigColumns_RLS proves alert_configs' new notify_vulns/
// vuln_min_severity/vuln_include_in_digest columns round-trip through the
// SAME whole-row RLS policies as every pre-existing column: tenant isolation
// on a tenant-scoped read, and cross-tenant visibility under the app.agent
// evaluator policy (the dispatch job's AlertConfigReader path).
func TestVulnAlertConfigColumns_RLS(t *testing.T) {
	app := startPostgres(t)
	ctx := context.Background()
	tenantA := seedTenant(t, app, "vuln-cfg-rls-a-"+uuid.NewString()[:8])
	tenantB := seedTenant(t, app, "vuln-cfg-rls-b-"+uuid.NewString()[:8])

	repo := uptime.NewRepo(app)
	if _, err := repo.UpsertAlertConfig(ctx, uptime.AlertConfig{
		TenantID:            tenantA,
		EmailRecipients:     []string{"a@example.com"},
		Enabled:             true,
		NotifyVulns:         true,
		VulnMinSeverity:     uptime.VulnSeverityCritical,
		VulnIncludeInDigest: false,
	}); err != nil {
		t.Fatalf("upsert config A: %v", err)
	}

	// Tenant A reads its own config with the new fields intact.
	cfgA, foundA, err := repo.GetAlertConfig(ctx, tenantA)
	if err != nil || !foundA {
		t.Fatalf("tenant A get config: found=%v err=%v", foundA, err)
	}
	if !cfgA.NotifyVulns || cfgA.VulnMinSeverity != uptime.VulnSeverityCritical || cfgA.VulnIncludeInDigest {
		t.Fatalf("tenant A config vuln fields wrong: %+v", cfgA)
	}

	// Tenant B sees no config (tenant_isolation covers the new columns too —
	// there is no separate policy per column, but this proves the row itself
	// including the new columns is invisible).
	_, foundB, err := repo.GetAlertConfig(ctx, tenantB)
	if err != nil {
		t.Fatalf("tenant B get config: %v", err)
	}
	if foundB {
		t.Fatal("tenant B must NOT see tenant A's alert config (RLS leak)")
	}

	// The cross-tenant evaluator (app.agent) sees it, with the new columns.
	all, err := repo.ListAlertConfigsAllTenants(ctx)
	if err != nil {
		t.Fatalf("list all configs (agent scope): %v", err)
	}
	var sawA bool
	for _, c := range all {
		if c.TenantID == tenantA {
			sawA = true
			if !c.NotifyVulns || c.VulnMinSeverity != uptime.VulnSeverityCritical {
				t.Errorf("agent-scope read of tenant A's config missing vuln fields: %+v", c)
			}
		}
	}
	if !sawA {
		t.Fatal("agent scope must see tenant A's config (evaluator policy)")
	}
}

// TestVulnFindingsNotifiedAt_RLS proves site_vulnerabilities.notified_at (and
// the row it lives on) is tenant-isolated, and that the agent scope used by
// ListTenantsWithUnnotifiedFindings/ClaimUnnotifiedFindings can enumerate
// across tenants.
func TestVulnFindingsNotifiedAt_RLS(t *testing.T) {
	app := startPostgres(t)
	admin := connectAdmin(t, app)
	defer admin.Close()
	ctx := context.Background()
	tenantA := seedTenant(t, app, "vuln-find-rls-a-"+uuid.NewString()[:8])
	tenantB := seedTenant(t, app, "vuln-find-rls-b-"+uuid.NewString()[:8])
	siteA := seedSiteFor(t, admin, tenantA, "https://vuln-find-rls-a.example.com")

	seedFindingAdmin(t, admin, tenantA, siteA, "v-rls", "critical", "open")

	countUnder := func(run func(fn func(pgx.Tx) error) error) int {
		var n int
		if err := run(func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM site_vulnerabilities WHERE tenant_id = $1`, tenantA).Scan(&n)
		}); err != nil {
			t.Fatalf("count query: %v", err)
		}
		return n
	}

	// 1. Tenant A sees its own finding.
	if got := countUnder(func(fn func(pgx.Tx) error) error { return app.InTenantTx(ctx, tenantA, fn) }); got != 1 {
		t.Fatalf("tenant A must see its own finding, got %d", got)
	}
	// 2. Tenant B does NOT see tenant A's finding — even though the WHERE
	// clause explicitly targets tenantA's id, proving RLS (not just the
	// app-level WHERE) enforces isolation.
	if got := countUnder(func(fn func(pgx.Tx) error) error { return app.InTenantTx(ctx, tenantB, fn) }); got != 0 {
		t.Fatalf("tenant B must NOT see tenant A's finding (tenant_isolation), got %d", got)
	}
	// 3. The agent scope (the dispatch job's ListTenantsWithUnnotifiedFindings
	// / ClaimUnnotifiedFindings path) sees it cross-tenant.
	if got := countUnder(func(fn func(pgx.Tx) error) error { return app.InAgentTx(ctx, fn) }); got != 1 {
		t.Fatalf("agent scope must see the finding (agent policy), got %d", got)
	}

	// 4. Repo-level: ListTenantsWithUnnotifiedFindings enumerates tenantA
	// (unnotified) and never tenantB (no rows at all).
	repo := vuln.NewRepo(app)
	tenants, err := repo.ListTenantsWithUnnotifiedFindings(ctx)
	if err != nil {
		t.Fatalf("ListTenantsWithUnnotifiedFindings: %v", err)
	}
	var sawA, sawB bool
	for _, id := range tenants {
		if id == tenantA {
			sawA = true
		}
		if id == tenantB {
			sawB = true
		}
	}
	if !sawA {
		t.Fatal("expected tenant A to be listed as having unnotified findings")
	}
	if sawB {
		t.Fatal("tenant B has no findings at all and must not be listed")
	}

	// 5. WITH CHECK: tenant B cannot INSERT a finding row carrying tenant A's id.
	errWrite := app.InTenantTx(ctx, tenantB, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `
			INSERT INTO site_vulnerabilities
				(tenant_id, site_id, vuln_id, kind, slug, name, installed_version, severity, title, status)
			VALUES ($1, $2, 'cross-tenant', 'plugin', 'cross-tenant', 'x', '1.0.0', 'high', 'x', 'open')`,
			tenantA, siteA)
		return e
	})
	if errWrite == nil {
		t.Fatal("tenant B must NOT be able to write a site_vulnerabilities row for tenant A (WITH CHECK)")
	}
}
