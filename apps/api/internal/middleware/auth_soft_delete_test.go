package middleware

// auth_soft_delete_test.go — GH #152 adversarial-review fast-follow M3
// (test-per-security-fix discipline). Proves the fail-closed ordering in
// Authenticate(): the tenantSoftDeleted check in the "no membership" branch
// must pre-empt BOTH the site_shares AND client_members fallback resolutions,
// not just one of them — otherwise a user who is a FORMER org member of a
// now-soft-deleted tenant, but who ALSO happens to hold an unrelated
// site_shares grant and/or client_members grant into that SAME tenant, could
// still resolve access via whichever fallback runs first.
//
// Docker-backed (testcontainers), mirroring tests/rls_integration_test.go's
// startPostgres — trimmed and duplicated here since internal/middleware
// cannot import the separate `tests` package.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"net/http"
	"net/http/httptest"

	"github.com/mosamlife/wpmgr/apps/api/internal/apikey"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/auth"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

func init() { gin.SetMode(gin.TestMode) }

func startMiddlewareTestPostgres(t *testing.T) (*db.Pool, *db.Pool) {
	t.Helper()
	ctx := context.Background()

	container, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("wpmgr"),
		tcpostgres.WithUsername("wpmgr"),
		tcpostgres.WithPassword("wpmgr"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		t.Skipf("skipping: cannot start postgres container (docker unavailable?): %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	adminDSN, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	adminPool, err := db.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	if err := adminPool.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for _, stmt := range []string{
		"ALTER ROLE wpmgr_app LOGIN PASSWORD 'app'",
		"GRANT USAGE ON SCHEMA public TO wpmgr_app",
		"GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO wpmgr_app",
		"REVOKE UPDATE, DELETE, TRUNCATE ON audit_log FROM wpmgr_app",
	} {
		if _, err := adminPool.Exec(ctx, stmt); err != nil {
			t.Fatalf("provision app role (%q): %v", stmt, err)
		}
	}
	t.Cleanup(adminPool.Close)

	appDSN := strings.Replace(adminDSN, "wpmgr:wpmgr@", "wpmgr_app:app@", 1)
	appPool, err := db.Connect(ctx, appDSN)
	if err != nil {
		t.Fatalf("connect app: %v", err)
	}
	t.Cleanup(appPool.Close)
	return appPool, adminPool
}

// TestAuthenticate_FailsClosedForFormerMemberWithShareAndPortalGrants proves
// the fail-closed ordering: a user who is (1) a FORMER member of a
// soft-deleted tenant, AND (2) holds a site_shares grant into that SAME
// tenant, AND (3) holds a client_members portal grant into that SAME tenant,
// resolves TenantID == uuid.Nil (RequireTenant -> 403) — the tenantSoftDeleted
// check must pre-empt BOTH fallbacks, not just whichever runs first.
func TestAuthenticate_FailsClosedForFormerMemberWithShareAndPortalGrants(t *testing.T) {
	pool, admin := startMiddlewareTestPostgres(t)
	ctx := context.Background()

	var tenant, user, site, client uuid.UUID
	if err := admin.QueryRow(ctx, `INSERT INTO tenants (name, slug) VALUES ($1,$1) RETURNING id`,
		"mw-soft-delete-"+uuid.NewString()[:8]).Scan(&tenant); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	if err := admin.QueryRow(ctx, `INSERT INTO users (email) VALUES ($1) RETURNING id`,
		"mw-user-"+uuid.NewString()[:8]+"@example.com").Scan(&user); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	// (1) A former org membership row (still physically present — the whole
	// point is that ListMembershipsForUser's own JOIN filters it out once the
	// tenant is soft-deleted, per db/query/memberships.sql).
	if _, err := admin.Exec(ctx, `INSERT INTO memberships (tenant_id, user_id, role) VALUES ($1,$2,'owner')`,
		tenant, user); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
	// (2) A site + a non-expired site_shares grant into the same tenant.
	if err := admin.QueryRow(ctx, `INSERT INTO sites (tenant_id, url, name) VALUES ($1,$2,'s') RETURNING id`,
		tenant, "https://mw-soft-delete-"+uuid.NewString()[:8]+".example.com").Scan(&site); err != nil {
		t.Fatalf("seed site: %v", err)
	}
	if _, err := admin.Exec(ctx, `INSERT INTO site_shares (tenant_id, site_id, user_id, role) VALUES ($1,$2,$3,'admin')`,
		tenant, site, user); err != nil {
		t.Fatalf("seed site_share: %v", err)
	}
	// (3) A client + a client_members portal grant into the same tenant.
	if err := admin.QueryRow(ctx, `INSERT INTO clients (tenant_id, name) VALUES ($1,'c') RETURNING id`,
		tenant).Scan(&client); err != nil {
		t.Fatalf("seed client: %v", err)
	}
	if _, err := admin.Exec(ctx, `INSERT INTO client_members (tenant_id, client_id, user_id) VALUES ($1,$2,$3)`,
		tenant, client, user); err != nil {
		t.Fatalf("seed client_member: %v", err)
	}

	authRepo := auth.NewRepo(pool)
	auditRec := audit.NewRecorder(pool, domain.SystemClock{})
	authSvc := auth.NewService(authRepo, auditRec, domain.NewValidator())
	keys := apikey.NewService(pool)
	sessions := auth.NewSessionManagerWithStore(scs.New(), false)
	authn := NewAuthenticator(sessions, authSvc, keys, pool)

	engine := gin.New()
	engine.Use(authn.Authenticate())
	var gotPrincipal domain.Principal
	var gotOK bool
	engine.GET("/whoami", func(c *gin.Context) {
		gotPrincipal, gotOK = domain.PrincipalFromContext(c.Request.Context())
		c.Status(http.StatusOK)
	})
	engineGated := gin.New()
	engineGated.Use(authn.Authenticate(), authz.RequireTenant())
	engineGated.GET("/gated", func(c *gin.Context) { c.Status(http.StatusOK) })

	sessCtx, err := sessions.SCS().Load(ctx, "")
	if err != nil {
		t.Fatalf("load session: %v", err)
	}
	if err := sessions.Login(sessCtx, user, tenant); err != nil {
		t.Fatalf("login: %v", err)
	}

	// Sanity: before soft-delete, the full membership resolves normally AND
	// RequireTenant passes.
	req := httptest.NewRequest(http.MethodGet, "/whoami", nil).WithContext(sessCtx)
	engine.ServeHTTP(httptest.NewRecorder(), req)
	if !gotOK || gotPrincipal.TenantID != tenant || gotPrincipal.Scope != domain.ScopeOrg {
		t.Fatalf("pre-delete principal = %+v (ok=%v), want TenantID=%s Scope=org", gotPrincipal, gotOK, tenant)
	}
	wGatedPre := httptest.NewRecorder()
	engineGated.ServeHTTP(wGatedPre, httptest.NewRequest(http.MethodGet, "/gated", nil).WithContext(sessCtx))
	if wGatedPre.Code != http.StatusOK {
		t.Fatalf("pre-delete gated status = %d, want 200", wGatedPre.Code)
	}

	// Soft-delete the tenant. The former-member membership row, the
	// site_shares grant, and the client_members grant ALL still physically
	// exist — only the read-path filters (and this fail-closed check) hide them.
	if _, err := admin.Exec(ctx, `UPDATE tenants SET deleted_at = now() WHERE id = $1`, tenant); err != nil {
		t.Fatalf("soft-delete: %v", err)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/whoami", nil).WithContext(sessCtx)
	engine.ServeHTTP(httptest.NewRecorder(), req2)
	if !gotOK {
		t.Fatal("principal should still be attached (UserID must survive so /auth/me works)")
	}
	if gotPrincipal.TenantID != uuid.Nil {
		t.Fatalf("expected TenantID=uuid.Nil for a former member who ALSO holds a site_share AND a "+
			"client_member grant into a soft-deleted tenant (fail-closed must pre-empt BOTH fallbacks), got %s "+
			"(scope=%q role=%q)", gotPrincipal.TenantID, gotPrincipal.Scope, gotPrincipal.Role)
	}
	if gotPrincipal.Scope == domain.ScopeSite {
		t.Fatal("must not resolve ScopeSite via the site_shares fallback for a soft-deleted tenant")
	}
	if len(gotPrincipal.AllowedSiteIDs) != 0 || len(gotPrincipal.ClientIDs) != 0 {
		t.Fatalf("must not carry any allowed sites / client ids for a soft-deleted tenant, got AllowedSiteIDs=%v ClientIDs=%v",
			gotPrincipal.AllowedSiteIDs, gotPrincipal.ClientIDs)
	}

	wGatedPost := httptest.NewRecorder()
	engineGated.ServeHTTP(wGatedPost, httptest.NewRequest(http.MethodGet, "/gated", nil).WithContext(sessCtx))
	if wGatedPost.Code != http.StatusForbidden {
		t.Fatalf("post-delete gated status = %d, want 403", wGatedPost.Code)
	}
}
