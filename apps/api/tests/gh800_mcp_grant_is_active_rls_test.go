// gh800_mcp_grant_is_active_rls_test.go: m152's mcp_grant_is_active, executed
// against the real schema as wpmgr_app, inside the transaction production
// uses for an AI request's creation: db.Pool.RunTenantTx dispatching a
// site-constrained principal to InScopedTenantTx, so app.site_scope is 'on'
// and mcp_grants_site_scope_select is live. No GUC is hand-set by this file.
//
// Three questions, none decidable by reading the migration:
//
//  1. Inside that site-scoped transaction, does the function see through the
//     site-scope gate for exactly its own question (active -> true, revoked ->
//     false), while the gate stays shut for everything else before and after
//     the call?
//  2. Does it refuse another tenant's grant, including on an install whose
//     migrator is a superuser, so the SECURITY DEFINER body runs with row
//     security off and the explicit tenant check is the only boundary?
//  3. Is it closed to PUBLIC?
package tests

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

// gh800SitePrincipal is the site-constrained principal a connection resolves
// to: the shape runConnectionTx requires, dispatched to InScopedTenantTx.
func gh800SitePrincipal(t *testing.T, pool *db.Pool, tenantID uuid.UUID) domain.Principal {
	t.Helper()
	s, err := site.NewRepo(pool).Create(context.Background(), site.CreateInput{
		TenantID: tenantID, URL: "https://gh800-" + uuid.NewString()[:8] + ".example.com", Name: "gh800"})
	if err != nil {
		t.Fatalf("create site: %v", err)
	}
	return domain.Principal{
		Type:           domain.PrincipalUser,
		UserID:         uuid.New(),
		TenantID:       tenantID,
		Scope:          domain.ScopeSite,
		AllowedSiteIDs: []uuid.UUID{s.ID},
	}
}

// gh800Ask runs the generated query in the production site-scoped dispatch and
// returns its verdict. It also proves, in the same transaction, that the
// site-scope gate is live before the call and still live after it: a direct
// read of the grant returns nothing either side, and app.site_scope is 'on'.
func gh800Ask(t *testing.T, pool *db.Pool, p domain.Principal, tenantArg, grantID uuid.UUID) bool {
	t.Helper()
	ctx := context.Background()
	var active bool
	err := pool.RunTenantTx(ctx, p, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "gh800 site-scoped tx")
		gateShut := func(when string) {
			var scope string
			if err := tx.QueryRow(ctx, "SELECT current_setting('app.site_scope', true)").Scan(&scope); err != nil {
				t.Fatalf("%s: read app.site_scope: %v", when, err)
			}
			if scope != "on" {
				t.Fatalf("%s: app.site_scope=%q, want 'on'; the tx is not site-scoped or the function leaked its SET", when, scope)
			}
			var n int
			if err := tx.QueryRow(ctx, "SELECT count(*) FROM mcp_grants WHERE id = $1", grantID).Scan(&n); err != nil {
				t.Fatalf("%s: direct grant read: %v", when, err)
			}
			if n != 0 {
				t.Fatalf("%s: a direct read of mcp_grants saw %d row(s) in a site-scoped tx; the gate is open", when, n)
			}
		}
		gateShut("before the call")
		v, err := sqlc.New(tx).MCPGrantIsActiveInScopedTx(ctx, sqlc.MCPGrantIsActiveInScopedTxParams{
			TenantID: tenantArg, GrantID: grantID})
		if err != nil {
			return err
		}
		active = v
		gateShut("after the call")
		return nil
	})
	if err != nil {
		t.Fatalf("site-scoped ask: %v", err)
	}
	return active
}

// TestGH800GrantIsActiveInSiteScopedTx is question 1.
func TestGH800GrantIsActiveInSiteScopedTx(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	repo := mcp.NewRepo(pool)

	tenant := seedTenant(t, pool, "gh800-a-"+uuid.NewString()[:8])
	grant := mcpSeedGrant(t, repo, tenant, "all", nil)
	p := gh800SitePrincipal(t, pool, tenant)

	if got := gh800Ask(t, pool, p, tenant, grant.ID); !got {
		t.Fatalf("active grant: got false, want true")
	}
	if got := gh800Ask(t, pool, p, tenant, uuid.New()); got {
		t.Fatalf("nonexistent grant: got true, want false")
	}

	org := domain.Principal{Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: tenant, Scope: domain.ScopeOrg}
	if _, err := repo.RevokeGrantWithTokens(ctx, org, grant.ID, nil); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if got := gh800Ask(t, pool, p, tenant, grant.ID); got {
		t.Fatalf("revoked grant: got true, want false")
	}
}

// TestGH800GrantIsActiveRefusesAnotherTenant is question 2.
func TestGH800GrantIsActiveRefusesAnotherTenant(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	repo := mcp.NewRepo(pool)

	tenantA := seedTenant(t, pool, "gh800-x-a-"+uuid.NewString()[:8])
	tenantB := seedTenant(t, pool, "gh800-x-b-"+uuid.NewString()[:8])
	grantB := mcpSeedGrant(t, repo, tenantB, "all", nil)
	pA := gh800SitePrincipal(t, pool, tenantA)
	pB := gh800SitePrincipal(t, pool, tenantB)

	// Positive control: the same grant IS active when asked from its own tenant,
	// so every false below is the boundary and not a broken fixture.
	if !gh800Ask(t, pool, pB, tenantB, grantB.ID) {
		t.Fatalf("positive control: tenant B's own active grant read false")
	}

	check := func(label string) {
		t.Helper()
		// A's tx, naming B as the tenant: must be refused by the argument check.
		if gh800Ask(t, pool, pA, tenantB, grantB.ID) {
			t.Fatalf("%s: tenant A's tx asking with tenant=B about B's grant got true", label)
		}
		// A's tx, naming itself, about B's grant: no such grant in A.
		if gh800Ask(t, pool, pA, tenantA, grantB.ID) {
			t.Fatalf("%s: tenant A's tx asking with tenant=A about B's grant got true", label)
		}
	}
	check("owner wpmgr_owner (NOBYPASSRLS)")

	// A superuser-migrated install: the definer body then runs with row security
	// not applied, so the explicit p_tenant = app.tenant_id check is the only
	// thing between tenant A and tenant B's grant.
	admin := connectAdmin(t, pool)
	defer admin.Close()
	var super bool
	if err := admin.QueryRow(ctx, "SELECT rolsuper FROM pg_roles WHERE rolname = current_user").Scan(&super); err != nil || !super {
		t.Fatalf("admin connection is not a superuser (super=%t err=%v); this half would test nothing", super, err)
	}
	if _, err := admin.Exec(ctx, "ALTER FUNCTION mcp_grant_is_active(uuid, uuid) OWNER TO CURRENT_USER"); err != nil {
		t.Fatalf("re-own function to superuser: %v", err)
	}
	check("owner superuser (row security bypassed in the body)")
}

// TestGH800GrantIsActiveNotExecutableByPublic is question 3.
func TestGH800GrantIsActiveNotExecutableByPublic(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()

	var publicHas bool
	if err := admin.QueryRow(ctx, `
		SELECT EXISTS (
		  SELECT 1 FROM pg_proc p, aclexplode(coalesce(p.proacl, acldefault('f', p.proowner))) a
		   WHERE p.proname = 'mcp_grant_is_active' AND a.grantee = 0 AND a.privilege_type = 'EXECUTE')`).
		Scan(&publicHas); err != nil {
		t.Fatalf("read acl: %v", err)
	}
	if publicHas {
		t.Fatalf("PUBLIC holds EXECUTE on mcp_grant_is_active")
	}

	var appHas bool
	if err := admin.QueryRow(ctx,
		"SELECT has_function_privilege('wpmgr_app', 'mcp_grant_is_active(uuid, uuid)', 'EXECUTE')").
		Scan(&appHas); err != nil || !appHas {
		t.Fatalf("positive control: wpmgr_app EXECUTE=%t err=%v, want true", appHas, err)
	}

	// Execute it as a role holding nothing but PUBLIC's privileges.
	probe := "gh800_probe_" + uuid.NewString()[:8]
	if _, err := admin.Exec(ctx, "CREATE ROLE "+probe+" NOLOGIN"); err != nil {
		t.Fatalf("create probe role: %v", err)
	}
	tx, err := admin.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+probe); err != nil {
		t.Fatalf("set role: %v", err)
	}
	var v bool
	err = tx.QueryRow(ctx, "SELECT mcp_grant_is_active($1, $2)", uuid.New(), uuid.New()).Scan(&v)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("a PUBLIC-only role calling the function: err=%v, want 42501 insufficient_privilege", err)
	}
	t.Logf("PUBLIC-only role refused: %s %s", pgErr.Code, pgErr.Message)
}
