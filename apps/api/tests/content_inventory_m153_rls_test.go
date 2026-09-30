// m153 proofs: site_content_inventory is invisible outside its tenant and its
// site scope, content_integrations is read-only for wpmgr_app, its one write
// path refuses a non-superadmin actor and audits a superadmin one, and the
// seed rows exist after migration.
//
// Every transaction goes through the production dispatch (RunTenantTx /
// InTenantTx / InAgentTx) and asserts, from inside, that it is wpmgr_app with
// neither SUPERUSER nor BYPASSRLS. Statements that exist in
// db/query/content_inventory.sql are called through the generated sqlc
// methods, so the statement proven is the statement shipped.
package tests

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

func m153Principal(tenant uuid.UUID, sites ...uuid.UUID) domain.Principal {
	if len(sites) == 0 {
		return domain.Principal{TenantID: tenant, Scope: domain.ScopeOrg}
	}
	return domain.Principal{TenantID: tenant, Scope: domain.ScopeSite, AllowedSiteIDs: sites}
}

// m153Refresh replaces one site's inventory the way the daily job will: the
// bulk upsert and the stale delete, one checked_at, one tenant transaction.
func m153Refresh(t *testing.T, pool *db.Pool, tenant, site uuid.UUID, postIDs ...int64) {
	t.Helper()
	n := len(postIDs)
	arg := sqlc.UpsertSiteContentInventoryParams{
		TenantID: tenant, SiteID: site, CheckedAt: time.Now().UTC(),
		PostIds: postIDs,
	}
	for i := 0; i < n; i++ {
		arg.PostTypes = append(arg.PostTypes, "page")
		arg.PostStatuses = append(arg.PostStatuses, "publish")
		arg.RouteNumbers = append(arg.RouteNumbers, 3)
		arg.Fingerprints = append(arg.Fingerprints, "")
		arg.Titles = append(arg.Titles, "Home")
		arg.OwnerDisplayNames = append(arg.OwnerDisplayNames, "")
		if i%2 == 0 {
			arg.Verdicts = append(arg.Verdicts, "builder")
			arg.RouteReasons = append(arg.RouteReasons, "builder_not_supported")
			arg.OwnerIntegrationIds = append(arg.OwnerIntegrationIds, "elementor")
			arg.OwnerVersions = append(arg.OwnerVersions, "3.21.0")
		} else {
			arg.Verdicts = append(arg.Verdicts, "block_document")
			arg.RouteReasons = append(arg.RouteReasons, "block_editor_unsupported")
			arg.OwnerIntegrationIds = append(arg.OwnerIntegrationIds, "")
			arg.OwnerVersions = append(arg.OwnerVersions, "")
		}
	}
	if err := pool.RunTenantTx(context.Background(), m153Principal(tenant), func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "RunTenantTx (m153 refresh)")
		q := sqlc.New(tx)
		got, err := q.UpsertSiteContentInventory(context.Background(), arg)
		if err != nil {
			return err
		}
		if got != int64(n) {
			t.Fatalf("upsert wrote %d rows, want %d", got, n)
		}
		_, err = q.DeleteStaleSiteContentInventory(context.Background(), sqlc.DeleteStaleSiteContentInventoryParams{
			TenantID: tenant, SiteID: site, CheckedAt: arg.CheckedAt,
		})
		return err
	}); err != nil {
		t.Fatalf("refresh inventory for site %s: %v", site, err)
	}
}

func m153List(t *testing.T, tx pgx.Tx, tenant, site uuid.UUID, owner *string) []sqlc.SiteContentInventory {
	t.Helper()
	rows, err := sqlc.New(tx).ListSiteContentInventory(context.Background(), sqlc.ListSiteContentInventoryParams{
		TenantID: tenant, SiteID: site, AfterPostID: 0, OwnerIntegrationID: owner, RowLimit: 50,
	})
	if err != nil {
		t.Fatalf("list inventory for site %s: %v", site, err)
	}
	return rows
}

func m153CountSite(t *testing.T, tx pgx.Tx, site uuid.UUID) int {
	t.Helper()
	var n int
	if err := tx.QueryRow(context.Background(),
		`SELECT count(*) FROM site_content_inventory WHERE site_id = $1`, site).Scan(&n); err != nil {
		t.Fatalf("count inventory rows: %v", err)
	}
	return n
}

// TestContentInventoryIsolationAsAppRole proves the permissive tenant policy,
// the RESTRICTIVE site-scope policy in both directions, the per-site replace,
// the editor filter, and the FOR SELECT agent policy behind the fleet report.
//
// Mutation, planted and watched: removing site_content_inventory_site_scope
// from m153 fires "SITE-SCOPE LEAK".
func TestContentInventoryIsolationAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)

	tenantA := seedTenant(t, pool, "m153-a-"+uuid.NewString()[:8])
	tenantB := seedTenant(t, pool, "m153-b-"+uuid.NewString()[:8])
	s1 := seedSite(t, pool, tenantA, "")
	s2 := seedSite(t, pool, tenantA, "")

	m153Refresh(t, pool, tenantA, s1, 10, 11, 12)
	m153Refresh(t, pool, tenantA, s2, 20)

	// Tenant B sees nothing of tenant A.
	if err := pool.InTenantTx(ctx, tenantB, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (foreign tenant)")
		if n := m153CountSite(t, tx, s1); n != 0 {
			t.Fatalf("TENANCY LEAK: tenant B sees %d inventory rows of tenant A's site %s; "+
				"site_content_inventory_tenant_isolation is missing or not enforced", n, s1)
		}
		if rows := m153List(t, tx, tenantA, s1, nil); len(rows) != 0 {
			t.Fatalf("TENANCY LEAK: ListSiteContentInventory named tenant A and returned %d rows to tenant B", len(rows))
		}
		return nil
	}); err != nil {
		t.Fatalf("foreign tenant read: %v", err)
	}

	// A principal scoped to S1 sees S1 and not S2, and cannot write S2.
	if err := pool.RunTenantTx(ctx, m153Principal(tenantA, s1), func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "RunTenantTx (single site S1)")
		if n := m153CountSite(t, tx, s2); n != 0 {
			t.Fatalf("SITE-SCOPE LEAK: a principal scoped to site %s sees %d rows of site %s; "+
				"site_content_inventory_site_scope is missing or not RESTRICTIVE", s1, n, s2)
		}
		if n := m153CountSite(t, tx, s1); n != 3 {
			t.Fatalf("the S1-scoped principal sees %d rows of its own site, want 3", n)
		}
		err := m133ExpectRefused(t, tx,
			`INSERT INTO site_content_inventory (tenant_id, site_id, post_id, post_type, post_status,
			   verdict, route_number, route_reason, checked_at)
			 VALUES ($1, $2, 99, 'page', 'draft', 'classic', 1, 'content_column', now())`, tenantA, s2)
		if err == nil {
			t.Fatalf("SITE-SCOPE LEAK: the S1-scoped principal inserted a row for site %s", s2)
		}
		return nil
	}); err != nil {
		t.Fatalf("site-scoped read: %v", err)
	}

	// Positive control for the org principal, and the editor filter.
	if err := pool.RunTenantTx(ctx, m153Principal(tenantA), func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "RunTenantTx (org)")
		if n := m153CountSite(t, tx, s2); n != 1 {
			t.Fatalf("the org principal sees %d rows of site %s, want 1", n, s2)
		}
		el := "elementor"
		if rows := m153List(t, tx, tenantA, s1, &el); len(rows) != 2 {
			t.Fatalf("filter elementor returned %d rows, want 2", len(rows))
		}
		classic := "classic"
		if rows := m153List(t, tx, tenantA, s1, &classic); len(rows) != 1 || rows[0].PostID != 11 {
			t.Fatalf("filter classic returned %+v, want post 11 only", rows)
		}
		return nil
	}); err != nil {
		t.Fatalf("org read: %v", err)
	}

	// Replace: a refresh that no longer reports post 10 and 12 drops them.
	m153Refresh(t, pool, tenantA, s1, 11)
	if err := pool.RunTenantTx(ctx, m153Principal(tenantA), func(tx pgx.Tx) error {
		rows := m153List(t, tx, tenantA, s1, nil)
		if len(rows) != 1 || rows[0].PostID != 11 {
			t.Fatalf("after replace the site holds %+v, want post 11 only", rows)
		}
		return nil
	}); err != nil {
		t.Fatalf("read after replace: %v", err)
	}

	// Fleet report reads across tenants under app.agent; the agent context
	// cannot write.
	if err := pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InAgentTx (fleet report)")
		rows, err := sqlc.New(tx).FleetContentShareByVerdict(ctx)
		if err != nil {
			return err
		}
		var pages int64
		for _, r := range rows {
			pages += r.Pages
		}
		if pages < 2 {
			t.Fatalf("fleet report counted %d pages, want at least the 2 this test wrote", pages)
		}
		tag, err := tx.Exec(ctx, `DELETE FROM site_content_inventory WHERE site_id = $1`, s2)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 0 {
			t.Fatalf("the agent context deleted %d rows; site_content_inventory_agent must be FOR SELECT", tag.RowsAffected())
		}
		return nil
	}); err != nil {
		t.Fatalf("agent read: %v", err)
	}
}

// TestContentIntegrationsReadOnlyForAppRole proves the seed rows exist after
// migration, that wpmgr_app can read and cannot INSERT, UPDATE, DELETE or
// TRUNCATE content_integrations, and that the definer refuses a
// non-superadmin actor and audits a superadmin one.
func TestContentIntegrationsReadOnlyForAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "m153-ci-"+uuid.NewString()[:8])

	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (allowlist read)")
		rows, err := sqlc.New(tx).ListContentIntegrations(ctx)
		if err != nil {
			return err
		}
		want := map[string]bool{"beaver-builder": true, "bricks": true, "divi": true, "elementor": true, "wpbakery": true}
		for _, r := range rows {
			if want[r.IntegrationID] {
				if r.Status != "detect_only" {
					t.Fatalf("seed row %s has status %q, want detect_only", r.IntegrationID, r.Status)
				}
				delete(want, r.IntegrationID)
			}
		}
		if len(want) != 0 {
			t.Fatalf("seed rows missing after migration: %v (read %d rows)", want, len(rows))
		}

		for what, stmt := range map[string]string{
			"INSERT":   `INSERT INTO content_integrations (integration_id, display_name, status) VALUES ('x-test', 'X', 'detect_only')`,
			"UPDATE":   `UPDATE content_integrations SET enabled = false WHERE integration_id = 'elementor'`,
			"DELETE":   `DELETE FROM content_integrations WHERE integration_id = 'elementor'`,
			"TRUNCATE": `TRUNCATE content_integrations`,
		} {
			err := m133ExpectRefused(t, tx, stmt)
			var pgErr *pgconn.PgError
			if err == nil || !errors.As(err, &pgErr) || pgErr.Code != "42501" {
				t.Fatalf("WRITE LEAK: wpmgr_app %s on content_integrations was not refused with 42501: %v", what, err)
			}
			t.Logf("wpmgr_app %s refused: %s", what, pgErr.Code)
		}

		_, err = sqlc.New(tx).AdminUpsertContentIntegration(ctx, sqlc.AdminUpsertContentIntegrationParams{
			ActorUserID: uuid.New(), IntegrationID: "elementor", DisplayName: "Elementor",
			Enabled: false, Status: "detect_only", Descriptor: []byte(`{}`),
		})
		var pgErr *pgconn.PgError
		if err == nil || !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Fatalf("the definer accepted a non-superadmin actor: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("allowlist checks: %v", err)
	}

	// A superadmin actor writes through the definer, and the audit records it.
	admin := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, email, is_superadmin) VALUES ($1, $2, true)`,
		admin, "m153-"+admin.String()[:8]+"@example.test"); err != nil {
		t.Fatalf("seed superadmin: %v", err)
	}
	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (definer write)")
		q := sqlc.New(tx)
		row, err := q.AdminUpsertContentIntegration(ctx, sqlc.AdminUpsertContentIntegrationParams{
			ActorUserID: admin, IntegrationID: "elementor", DisplayName: "Elementor",
			Enabled: false, Status: "detect_only", Descriptor: []byte(`{}`),
		})
		if err != nil {
			return err
		}
		if row.Enabled || !row.UpdatedByUserID.Valid || uuid.UUID(row.UpdatedByUserID.Bytes) != admin {
			t.Fatalf("definer returned %+v, want enabled=false by %s", row, admin)
		}
		audit, err := q.ListContentIntegrationAudit(ctx, sqlc.ListContentIntegrationAuditParams{IntegrationID: "elementor", RowLimit: 5})
		if err != nil {
			return err
		}
		if len(audit) != 1 || audit[0].ActorUserID != admin || audit[0].Action != "update" ||
			audit[0].BeforeSha256 == nil || *audit[0].BeforeSha256 == audit[0].AfterSha256 {
			t.Fatalf("audit after one superadmin update: %+v", audit)
		}
		return nil
	}); err != nil {
		t.Fatalf("superadmin write: %v", err)
	}
}
