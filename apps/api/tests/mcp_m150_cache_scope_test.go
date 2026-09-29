// m150 proofs: the `mcp:cache` scope is storable on a grant and on a client
// registration, a scope outside the vocabulary is still refused at the named
// constraint, both constraints were validated on add, and the new
// cache_purge_audit.initiator_grant_id column changes nothing for existing
// writers or for the RESTRICTIVE site-scope policy the AI clear path relies
// on.
//
// Every transaction under test is the production dispatch and asserts, from
// inside, that it is wpmgr_app with neither SUPERUSER nor BYPASSRLS.
package tests

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

// TestM150CacheScopeIsStorableAndTheVocabularyStillClosedAsAppRole proves both
// widened constraints: {mcp:read, mcp:cache} is stored on a grant (a direct
// insert, as m136's constraint test does) and on a client registration (the
// shipped RegisterMCPOAuthClient statement), 'mcp:write' is refused with 23514
// at each named constraint, and both are convalidated.
//
// Mutation, planted and watched: re-adding either constraint over
// ARRAY['mcp:read'] alone makes the matching `mcp:cache` insert fail.
func TestM150CacheScopeIsStorableAndTheVocabularyStillClosedAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "m150-scope-"+uuid.NewString()[:8])

	type subject struct{ table, constraint string }
	for _, s := range []subject{
		{"mcp_grants", "mcp_grants_oauth_scopes_vocabulary_check"},
		{"mcp_oauth_clients", "mcp_oauth_clients_registered_scopes_vocabulary_check"},
	} {
		var validated bool
		if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
			mcpAssertAndReportRole(t, tx, "InTenantTx (constraint probe)")
			return tx.QueryRow(ctx, `SELECT convalidated FROM pg_constraint
				WHERE conrelid = ('public.' || $1)::regclass AND contype = 'c' AND conname = $2`,
				s.table, s.constraint).Scan(&validated)
		}); err != nil {
			t.Fatalf("INDETERMINATE: %s carries no CHECK named %q (%v); nothing below can be attributed to it",
				s.table, s.constraint, err)
		}
		if !validated {
			t.Fatalf("%s.%s is NOT VALID; m150 re-adds it validated", s.table, s.constraint)
		}
		t.Logf("%s: %s present and convalidated", s.table, s.constraint)
	}

	grantInsert := func(scopes []string) error {
		return pool.RunTenantTx(ctx, acprOrgPrincipal(tenant), func(tx pgx.Tx) error {
			mcpAssertAndReportRole(t, tx, "RunTenantTx (grant scopes)")
			_, err := tx.Exec(ctx, `INSERT INTO mcp_grants
				    (tenant_id, name, status, site_scope_mode, capabilities, oauth_scopes, expires_at)
				VALUES ($1, $2, 'active', 'all', ARRAY['mcp.sites.read','mcp.cache.purge']::text[], $3::text[], $4)`,
				tenant, "m150 "+uuid.NewString()[:8], scopes, time.Now().UTC().Add(90*24*time.Hour))
			return err
		})
	}
	clientInsert := func(scopes []string) error {
		return pool.InMCPClientRegisterTx(ctx, func(tx pgx.Tx) error {
			mcpAssertAndReportRole(t, tx, "InMCPClientRegisterTx (client scopes)")
			_, err := sqlc.New(tx).RegisterMCPOAuthClient(ctx, sqlc.RegisterMCPOAuthClientParams{
				ClientID:                "m150-" + uuid.NewString(),
				TokenEndpointAuthMethod: "none",
				RedirectUris:            []string{"https://client.example/cb"},
				RegisteredScopes:        scopes,
			})
			return err
		})
	}

	if err := grantInsert([]string{"mcp:read", "mcp:cache"}); err != nil {
		t.Fatalf("a grant holding mcp:cache was refused: %v", err)
	}
	if err := clientInsert([]string{"mcp:read", "mcp:cache"}); err != nil {
		t.Fatalf("a client registering mcp:cache was refused: %v", err)
	}
	t.Log("mcp:cache stored on a grant and on a client registration")

	for _, c := range []struct {
		what, constraint string
		err              error
	}{
		{"grant with mcp:write", "mcp_grants_oauth_scopes_vocabulary_check", grantInsert([]string{"mcp:read", "mcp:write"})},
		{"client with mcp:write", "mcp_oauth_clients_registered_scopes_vocabulary_check", clientInsert([]string{"mcp:read", "mcp:write"})},
	} {
		var pgErr *pgconn.PgError
		if !errors.As(c.err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != c.constraint {
			t.Fatalf("%s: want 23514 at %s, got %v", c.what, c.constraint, c.err)
		}
		t.Logf("%s: refused 23514 at %s", c.what, c.constraint)
	}
}

// TestM150CachePurgeAuditSiteScopeAndOldShapeInsertAsAppRole proves B3 of the
// design: cache_purge_audit_site_scope exists and is RESTRICTIVE; the dashboard
// purge's own insert statement still works with the new nullable column; and
// under a single-site principal the cooldown read sees that site's dashboard
// row and not another site's, while the AI hourly count ignores dashboard rows.
//
// Mutation, planted and watched: DROP POLICY cache_purge_audit_site_scope makes
// the S2 cooldown read under the S1 principal return S2's row.
func TestM150CachePurgeAuditSiteScopeAndOldShapeInsertAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "m150-cpa-"+uuid.NewString()[:8])
	s1 := seedSite(t, pool, tenant, "")
	s2 := seedSite(t, pool, tenant, "")

	var permissive string
	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (policy probe)")
		return tx.QueryRow(ctx, `SELECT permissive FROM pg_policies
			WHERE schemaname = 'public' AND tablename = 'cache_purge_audit'
			  AND policyname = 'cache_purge_audit_site_scope'`).Scan(&permissive)
	}); err != nil {
		t.Fatalf("INDETERMINATE: cache_purge_audit_site_scope not found (%v)", err)
	}
	if permissive != "RESTRICTIVE" {
		t.Fatalf("cache_purge_audit_site_scope is %s, want RESTRICTIVE", permissive)
	}

	// The dashboard's insert, unchanged, on both sites (org principal).
	var dash1 sqlc.CachePurgeAudit
	if err := pool.RunTenantTx(ctx, acprOrgPrincipal(tenant), func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "RunTenantTx (dashboard purge insert)")
		q := sqlc.New(tx)
		var err error
		for _, s := range []uuid.UUID{s1, s2} {
			row, ierr := q.InsertCachePurgeAudit(ctx, sqlc.InsertCachePurgeAuditParams{
				TenantID: tenant, SiteID: s, Kind: "all", TargetUrls: []string{}, UrlsCount: 0})
			if ierr != nil {
				return ierr
			}
			if row.InitiatorGrantID.Valid {
				t.Fatalf("a dashboard purge row carries initiator_grant_id %v", row.InitiatorGrantID)
			}
			if s == s1 {
				dash1 = row
			}
		}
		return err
	}); err != nil {
		t.Fatalf("old-shape InsertCachePurgeAudit: %v", err)
	}

	if err := pool.RunTenantTx(ctx, acprSitePrincipal(tenant, s1), func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "RunTenantTx (single site S1)")
		q := sqlc.New(tx)
		// THE LEAK ASSERTION FIRST: S2's row is invisible.
		if _, err := q.GetLatestWholeSitePurgeOnSiteSince(ctx, sqlc.GetLatestWholeSitePurgeOnSiteSinceParams{
			TenantID: tenant, SiteID: s2, WindowSeconds: 300}); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("SITE-SCOPE LEAK: the S1 principal's cooldown read saw site S2's purge (err=%v)", err)
		}
		at, err := q.GetLatestWholeSitePurgeOnSiteSince(ctx, sqlc.GetLatestWholeSitePurgeOnSiteSinceParams{
			TenantID: tenant, SiteID: s1, WindowSeconds: 300})
		if err != nil || !at.Equal(dash1.CreatedAt) {
			t.Fatalf("OVER-FIRING: the S1 cooldown read did not see S1's dashboard purge (at=%v err=%v)", at, err)
		}
		n, err := q.CountAssistantCachePurgesOnSiteSince(ctx, sqlc.CountAssistantCachePurgesOnSiteSinceParams{
			TenantID: tenant, SiteID: s1, WindowSeconds: 3600})
		if err != nil || n != 0 {
			t.Fatalf("the AI hourly count included a dashboard purge: %d (err=%v)", n, err)
		}
		return nil
	}); err != nil {
		t.Fatalf("single-site cooldown read: %v", err)
	}
}
