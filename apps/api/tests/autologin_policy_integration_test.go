package tests

import (
	"context"
	"strings"
	"testing"

	"github.com/mosamlife/wpmgr/apps/api/internal/autologin"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

// TestAutologinPolicySettingsRealSQLSeedsAndPreservesRoles exercises the real
// m105 UpdateAutologinPolicySettings query (GH #286) against a live Postgres:
// the first write on a site with no policy row seeds allowed_wp_roles +
// max_session_age_minutes with their table defaults, and a second write never
// widens allowed_wp_roles.
func TestAutologinPolicySettingsRealSQLSeedsAndPreservesRoles(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "al-policy-seed")
	s := enrollFakeSite(t, pool, tenant, "https://wp-policy-seed.example.com")

	repo := autologin.NewRepo(pool)

	first, err := repo.UpdatePolicySettings(ctx, tenant, s.ID, true, "first.write")
	if err != nil {
		t.Fatalf("first write: %v", err)
	}
	if len(first.AllowedWPRoles) != 1 || first.AllowedWPRoles[0] != "administrator" {
		t.Fatalf("allowed_wp_roles = %v, want seeded default [administrator]", first.AllowedWPRoles)
	}
	if first.MaxSessionAgeMinutes != 30 {
		t.Fatalf("max_session_age_minutes = %d, want seeded default 30", first.MaxSessionAgeMinutes)
	}
	if first.DefaultWPUserLogin != "first.write" {
		t.Fatalf("default_wp_user_login = %q, want first.write", first.DefaultWPUserLogin)
	}

	second, err := repo.UpdatePolicySettings(ctx, tenant, s.ID, false, "second.write")
	if err != nil {
		t.Fatalf("second write: %v", err)
	}
	if second.DefaultWPUserLogin != "second.write" || second.Enabled {
		t.Fatalf("second write did not persist: %+v", second)
	}
	if len(second.AllowedWPRoles) != 1 || second.AllowedWPRoles[0] != "administrator" {
		t.Fatalf("allowed_wp_roles changed on an UPDATE path: %v", second.AllowedWPRoles)
	}

	// GetOrCreatePolicy (the GET path) reflects the same row.
	fetched, err := repo.GetOrCreatePolicy(ctx, tenant, s.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if fetched.DefaultWPUserLogin != "second.write" {
		t.Fatalf("GET default_wp_user_login = %q, want second.write", fetched.DefaultWPUserLogin)
	}
}

// TestAutologinMintInjectsRealStoredDefault proves the mint-time injection
// (Service.Mint) works end to end against a live Postgres: a policy saved via
// UpdatePolicySettings is picked up on the next mint when the operator's
// request omits target_wp_user_login, and the persisted token + audit row
// both carry the real username.
func TestAutologinMintInjectsRealStoredDefault(t *testing.T) {
	pool := startPostgres(t)
	redisPool, _ := startRedis(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "al-mint-default")
	siteSvc := site.NewService(site.NewRepo(pool), domain.NewValidator(), domain.SystemClock{})
	s := enrollFakeSite(t, pool, tenant, "https://wp-mint-default.example.com")
	userID := seedAutologinUser(t, pool, "mint-default@example.com")

	repo := autologin.NewRepo(pool)
	if _, err := repo.UpdatePolicySettings(ctx, tenant, s.ID, true, "site.default.user"); err != nil {
		t.Fatalf("seed policy default: %v", err)
	}

	svc, _ := buildAutologinService(t, pool, redisPool, siteSvc, autologin.Config{})
	tok, err := svc.Mint(ctx, autologin.MintRequest{TenantID: tenant, SiteID: s.ID, InitiatorID: userID})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	admin := connectAdmin(t, pool)
	defer admin.Close()
	var target string
	if err := admin.QueryRow(ctx, "SELECT target_wp_user_login FROM autologin_tokens WHERE id=$1", tok.NonceID).Scan(&target); err != nil {
		t.Fatalf("read token: %v", err)
	}
	if target != "site.default.user" {
		t.Fatalf("persisted target_wp_user_login = %q, want site.default.user", target)
	}

	var auditMeta []byte
	if err := admin.QueryRow(ctx,
		"SELECT metadata FROM audit_log WHERE tenant_id=$1 AND action='autologin.requested' ORDER BY created_at DESC LIMIT 1",
		tenant).Scan(&auditMeta); err != nil {
		t.Fatalf("read audit row: %v", err)
	}
	if !containsJSONValue(auditMeta, "target_wp_user_login", "site.default.user") {
		t.Fatalf("audit metadata missing the injected default: %s", auditMeta)
	}

	// Consume must also see the injected default (Redis payload path).
	res, err := svc.Consume(ctx, s.ID, s.ID, tok.NonceID, "203.0.113.99")
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if res.TargetWPUser != "site.default.user" {
		t.Fatalf("consume target = %q, want site.default.user", res.TargetWPUser)
	}
}

// TestAutologinPolicyCrossTenantSiteGuardedAgainstPhantomRow proves the
// site-ownership guard end to end against a live Postgres: autologin_policies
// has no FK from site_id to a tenant-scoped site, and its RLS WITH CHECK only
// validates tenant_id == app.tenant_id (never that site_id actually belongs
// to tenant_id). Without the guard, tenant A calling GetPolicy/UpdatePolicy
// with tenant B's site UUID could create or overwrite a phantom (siteB,
// tenantA) row keyed by siteB's PK, and GetOrCreatePolicy's ON CONFLICT
// (site_id) target has no tenant_id WHERE clause, so a stray row would then
// leak into tenant B's own reads. This test asserts: (1) both calls 404 the
// same way Mint does, (2) no (siteB, tenantA) row is ever written, and (3)
// tenant B's own GetOrCreatePolicy/mint still work afterward.
func TestAutologinPolicyCrossTenantSiteGuardedAgainstPhantomRow(t *testing.T) {
	pool := startPostgres(t)
	redisPool, _ := startRedis(t)
	ctx := context.Background()
	tenantA := seedTenant(t, pool, "al-policy-xtenant-a")
	tenantB := seedTenant(t, pool, "al-policy-xtenant-b")
	siteSvc := site.NewService(site.NewRepo(pool), domain.NewValidator(), domain.SystemClock{})
	siteB := enrollFakeSite(t, pool, tenantB, "https://wp-xtenant-b.example.com")
	userA := seedAutologinUser(t, pool, "xtenant-a@example.com")

	svc, _ := buildAutologinService(t, pool, redisPool, siteSvc, autologin.Config{})

	// Tenant A's UpdatePolicy against tenant B's site must 404 and must not
	// write a row.
	if _, err := svc.UpdatePolicy(ctx, tenantA, siteB.ID, domain.Principal{Type: domain.PrincipalUser, UserID: userA}, autologin.PolicyInput{
		Enabled: true, DefaultWPUserLogin: "attacker.user",
	}); err == nil {
		t.Fatal("expected site_not_found for a cross-tenant UpdatePolicy")
	} else if de, ok := domain.AsDomain(err); !ok || de.Kind != domain.KindNotFound {
		t.Fatalf("want NotFound, got %v", err)
	}

	// Tenant A's GetPolicy against tenant B's site must also 404 and must not
	// auto-create a row.
	if _, err := svc.GetPolicy(ctx, tenantA, siteB.ID); err == nil {
		t.Fatal("expected site_not_found for a cross-tenant GetPolicy")
	} else if de, ok := domain.AsDomain(err); !ok || de.Kind != domain.KindNotFound {
		t.Fatalf("want NotFound, got %v", err)
	}

	// No (siteB, tenantA) row exists in autologin_policies.
	admin := connectAdmin(t, pool)
	defer admin.Close()
	var count int
	if err := admin.QueryRow(ctx,
		"SELECT count(*) FROM autologin_policies WHERE site_id=$1 AND tenant_id=$2",
		siteB.ID, tenantA).Scan(&count); err != nil {
		t.Fatalf("count phantom rows: %v", err)
	}
	if count != 0 {
		t.Fatalf("phantom (siteB, tenantA) autologin_policies row was created: count=%d", count)
	}

	// Tenant B's own GetOrCreatePolicy must still see a row scoped to itself
	// (no cross-tenant squatter on the site_id PK).
	repo := autologin.NewRepo(pool)
	policy, err := repo.GetOrCreatePolicy(ctx, tenantB, siteB.ID)
	if err != nil {
		t.Fatalf("tenant B GetOrCreatePolicy: %v", err)
	}
	if policy.TenantID != tenantB {
		t.Fatalf("tenant B policy tenant_id = %s, want %s (cross-tenant row leaked)", policy.TenantID, tenantB)
	}

	// Tenant B's own mint (which also calls GetOrCreatePolicy) still works.
	userB := seedAutologinUser(t, pool, "xtenant-b@example.com")
	if _, err := svc.Mint(ctx, autologin.MintRequest{TenantID: tenantB, SiteID: siteB.ID, InitiatorID: userB}); err != nil {
		t.Fatalf("tenant B mint after cross-tenant guard attempt: %v", err)
	}
}

// containsJSONValue asserts one string field is present in a jsonb blob.
// Postgres's jsonb output normalizes to `"key": "value"` (space after the
// colon), so both spacing variants are accepted rather than pulling in a
// JSON path library for a single-field check.
func containsJSONValue(raw []byte, key, want string) bool {
	s := string(raw)
	quotedKey := "\"" + key + "\""
	quotedVal := "\"" + want + "\""
	return strings.Contains(s, quotedKey+":"+quotedVal) || strings.Contains(s, quotedKey+": "+quotedVal)
}
