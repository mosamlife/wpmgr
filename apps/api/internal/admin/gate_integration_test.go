package admin

// gate_integration_test.go: the GH #322 single-tenant allow-path proven against
// a REAL Postgres, through the REAL mounted route, with real rows.
//
// gate_test.go proves which route carries the wider gate and that it fails
// closed. It cannot prove the part that actually decides who gets in: how many
// organisations this install has, what "owner" means, and what a soft-deleted
// organisation counts as. Those live in soleLiveTenantOwnerSQL and in the RLS
// policies it runs under, so they are tested here against rows.
//
// Docker-backed (testcontainers), mirroring
// internal/middleware/auth_soft_delete_test.go's harness. Skips when Docker is
// unavailable.
//
// Against the PRE-change gate every allow case here fails (403), because there
// was no path for a non-superadmin at all; the refuse cases pass either way.

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// startGateTestPostgres returns (appPool, adminPool). appPool connects as the
// non-superuser wpmgr_app role, so RLS is actually in force: reading the
// caller's own memberships depends on the memberships_self_read policy and the
// app.user_id GUC that InUserTx sets, exactly as in production.
func startGateTestPostgres(t *testing.T) (*db.Pool, *db.Pool) {
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
				WithStartupTimeout(90*time.Second),
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

// gateSeed is the per-case fixture helper. Every case starts from an empty
// tenants/users table, because the gate's whole question is an install-wide
// count and leftover rows from a previous case would answer it.
type gateSeed struct {
	t     *testing.T
	admin *db.Pool
	ctx   context.Context
}

func newGateSeed(t *testing.T, adminPool *db.Pool) *gateSeed {
	t.Helper()
	ctx := context.Background()
	// tenants cascades to memberships; users cascades to memberships too.
	for _, stmt := range []string{`DELETE FROM tenants`, `DELETE FROM users`} {
		if _, err := adminPool.Exec(ctx, stmt); err != nil {
			t.Fatalf("reset (%q): %v", stmt, err)
		}
	}
	return &gateSeed{t: t, admin: adminPool, ctx: ctx}
}

func (s *gateSeed) tenant(name string) uuid.UUID {
	s.t.Helper()
	var id uuid.UUID
	slug := name + "-" + uuid.NewString()[:8]
	if err := s.admin.QueryRow(s.ctx, `INSERT INTO tenants (name, slug) VALUES ($1,$2) RETURNING id`,
		name, slug).Scan(&id); err != nil {
		s.t.Fatalf("seed tenant %s: %v", name, err)
	}
	return id
}

func (s *gateSeed) softDelete(tenantID uuid.UUID) {
	s.t.Helper()
	if _, err := s.admin.Exec(s.ctx, `UPDATE tenants SET deleted_at = now() WHERE id = $1`, tenantID); err != nil {
		s.t.Fatalf("soft-delete tenant: %v", err)
	}
}

func (s *gateSeed) restore(tenantID uuid.UUID) {
	s.t.Helper()
	if _, err := s.admin.Exec(s.ctx, `UPDATE tenants SET deleted_at = NULL WHERE id = $1`, tenantID); err != nil {
		s.t.Fatalf("restore tenant: %v", err)
	}
}

func (s *gateSeed) user(superadmin bool) uuid.UUID {
	s.t.Helper()
	var id uuid.UUID
	email := "gate-" + uuid.NewString()[:8] + "@example.com"
	if err := s.admin.QueryRow(s.ctx,
		`INSERT INTO users (email, is_superadmin) VALUES ($1,$2) RETURNING id`,
		email, superadmin).Scan(&id); err != nil {
		s.t.Fatalf("seed user: %v", err)
	}
	return id
}

func (s *gateSeed) member(tenantID, userID uuid.UUID, role string) {
	s.t.Helper()
	if _, err := s.admin.Exec(s.ctx,
		`INSERT INTO memberships (tenant_id, user_id, role) VALUES ($1,$2,$3)`,
		tenantID, userID, role); err != nil {
		s.t.Fatalf("seed membership (%s): %v", role, err)
	}
}

// realGatedEngine mounts the REAL admin routes against the REAL pool, so the
// gate runs newPoolGateStore and therefore soleLiveTenantOwnerSQL under InUserTx.
func realGatedEngine(t *testing.T, pool *db.Pool) *gin.Engine {
	t.Helper()
	h := NewHandler(nil, pool)
	h.SetAgentMirror(NewAgentMirrorCheckService(false, false, nil, nil))
	r := gin.New()
	h.Register(r.Group("/api/v1"))
	return r
}

// assertMirrorCheck drives POST /admin/agent-mirror/check as the given user and
// asserts whether the gate let it through. Past the gate is pastTheGateCode
// (the handler's own "mirroring is switched off" refusal); refused at the gate
// is 403 gateRefusedCode.
func assertMirrorCheck(t *testing.T, engine *gin.Engine, userID uuid.UUID, wantAllowed bool, why string) {
	t.Helper()
	p := &domain.Principal{Type: domain.PrincipalUser, UserID: userID}
	status, code := callAs(t, engine, http.MethodPost, mirrorCheckPath, p)

	if wantAllowed {
		if status == http.StatusForbidden || code != pastTheGateCode {
			t.Fatalf("%s: got %d %q, want the request to pass the gate (%q)", why, status, code, pastTheGateCode)
		}
		return
	}
	if status != http.StatusForbidden || code != gateRefusedCode {
		t.Fatalf("%s: got %d %q, want 403 %q", why, status, code, gateRefusedCode)
	}
}

// TestSoleTenantOwnerGate_AgainstRealRows is the whole GH #322 decision table,
// run against real tenants, memberships and RLS.
func TestSoleTenantOwnerGate_AgainstRealRows(t *testing.T) {
	pool, adminPool := startGateTestPostgres(t)
	engine := realGatedEngine(t, pool)

	// T2: the owner of the only organisation on the install is allowed.
	// FAILS against the pre-change gate.
	t.Run("owner of the only organisation is allowed", func(t *testing.T) {
		s := newGateSeed(t, adminPool)
		only := s.tenant("only")
		owner := s.user(false)
		s.member(only, owner, "owner")
		assertMirrorCheck(t, engine, owner, true, "owner of the sole organisation")
	})

	// T3: two live organisations means there IS another tenant's budget to
	// protect, so the reason for the gate is back and the path closes.
	t.Run("owner of one of two organisations is refused", func(t *testing.T) {
		s := newGateSeed(t, adminPool)
		first := s.tenant("first")
		s.tenant("second")
		owner := s.user(false)
		s.member(first, owner, "owner")
		assertMirrorCheck(t, engine, owner, false, "owner of one of two organisations")
	})

	// T4: membership is not enough. Only role='owner' passes; the constraint
	// vocabulary is ('owner','admin','operator','viewer').
	t.Run("non-owner member of the only organisation is refused", func(t *testing.T) {
		for _, role := range []string{"admin", "operator", "viewer"} {
			t.Run(role, func(t *testing.T) {
				s := newGateSeed(t, adminPool)
				only := s.tenant("only")
				owner := s.user(false)
				s.member(only, owner, "owner") // a real owner exists; the caller is not them
				member := s.user(false)
				s.member(only, member, role)
				assertMirrorCheck(t, engine, member, false, role+" of the sole organisation")
			})
		}
	})

	// A user with no membership at all, on a single-organisation install.
	t.Run("non-member is refused on a single-organisation install", func(t *testing.T) {
		s := newGateSeed(t, adminPool)
		only := s.tenant("only")
		s.member(only, s.user(false), "owner")
		stranger := s.user(false)
		assertMirrorCheck(t, engine, stranger, false, "non-member")
	})

	// T6: the R3 decision, asserted explicitly. A soft-deleted second
	// organisation does NOT count: nobody can act as it, so it has no budget
	// share to protect, and every other tenant-resolving read in this repo
	// already treats it as gone.
	t.Run("soft-deleted second organisation does not count", func(t *testing.T) {
		s := newGateSeed(t, adminPool)
		live := s.tenant("live")
		gone := s.tenant("gone")
		owner := s.user(false)
		s.member(live, owner, "owner")
		s.member(gone, owner, "owner")
		s.softDelete(gone)
		assertMirrorCheck(t, engine, owner, true, "owner with one live and one soft-deleted organisation")

		// R2, the other half of the same decision: restoring it closes the path
		// again on the very NEXT request. Nothing is cached, so there is nothing
		// to invalidate.
		s.restore(gone)
		assertMirrorCheck(t, engine, owner, false, "same owner immediately after the second organisation was restored")
	})

	// The inverse of T6: owning only a soft-deleted organisation grants
	// nothing, even though the live count is exactly 1. The caller must own
	// the LIVE one.
	t.Run("owner of only a soft-deleted organisation is refused", func(t *testing.T) {
		s := newGateSeed(t, adminPool)
		s.tenant("live-owned-by-someone-else")
		gone := s.tenant("gone")
		owner := s.user(false)
		s.member(gone, owner, "owner")
		s.softDelete(gone)
		assertMirrorCheck(t, engine, owner, false, "owner of a soft-deleted organisation only")
	})

	// T1: superadmin is unchanged, and is independent of the tenant count.
	t.Run("superadmin is allowed regardless of the organisation count", func(t *testing.T) {
		s := newGateSeed(t, adminPool)
		s.tenant("first")
		s.tenant("second")
		sa := s.user(true)
		assertMirrorCheck(t, engine, sa, true, "superadmin on a two-organisation install")
	})

	// T5: an API key that maps to the owner of the only organisation is still
	// refused. The principal type is checked before any lookup.
	t.Run("api-key principal is refused even when it maps to the sole owner", func(t *testing.T) {
		s := newGateSeed(t, adminPool)
		only := s.tenant("only")
		owner := s.user(false)
		s.member(only, owner, "owner")

		p := &domain.Principal{
			Type:     domain.PrincipalAPIKey,
			APIKeyID: uuid.New(),
			TenantID: only,
			UserID:   owner, // even carrying the owner's user id, it must not pass
		}
		status, code := callAs(t, engine, http.MethodPost, mirrorCheckPath, p)
		if status != http.StatusForbidden || code != gateRefusedCode {
			t.Fatalf("api-key principal: got %d %q, want 403 %q", status, code, gateRefusedCode)
		}
	})

	// T7: fail closed on a broken read. Revoking the app role's SELECT on
	// tenants makes soleLiveTenantOwnerSQL error; the gate must refuse rather
	// than treat the failure as an allow. Restored immediately afterwards.
	t.Run("a database error on the count path refuses", func(t *testing.T) {
		s := newGateSeed(t, adminPool)
		only := s.tenant("only")
		owner := s.user(false)
		s.member(only, owner, "owner")
		assertMirrorCheck(t, engine, owner, true, "control: the same owner is allowed before the read is broken")

		if _, err := adminPool.Exec(s.ctx, `REVOKE SELECT ON tenants FROM wpmgr_app`); err != nil {
			t.Fatalf("revoke select on tenants: %v", err)
		}
		defer func() {
			if _, err := adminPool.Exec(s.ctx, `GRANT SELECT ON tenants TO wpmgr_app`); err != nil {
				t.Fatalf("restore select on tenants: %v", err)
			}
		}()
		assertMirrorCheck(t, engine, owner, false, "the same owner once the tenant count cannot be read")
	})

	// T8: containment, against real rows. The owner who IS allowed through
	// agent-mirror/check is still refused by other admin routes. 403 rather
	// than "not 200" also proves those routes are still mounted.
	t.Run("no other admin route admits the sole-organisation owner", func(t *testing.T) {
		s := newGateSeed(t, adminPool)
		only := s.tenant("only")
		owner := s.user(false)
		s.member(only, owner, "owner")
		assertMirrorCheck(t, engine, owner, true, "control: the owner passes the widened route")

		p := &domain.Principal{Type: domain.PrincipalUser, UserID: owner}
		for _, path := range []string{
			"/api/v1/admin/stats",
			"/api/v1/admin/users",
			"/api/v1/admin/accounts-tenancy",
			"/api/v1/admin/accounts",
			"/api/v1/admin/revenue",
		} {
			status, code := callAs(t, engine, http.MethodGet, path, p)
			if status != http.StatusForbidden || code != gateRefusedCode {
				t.Fatalf("GET %s: got %d %q, want 403 %q: only agent-mirror/check may admit this principal",
					path, status, code, gateRefusedCode)
			}
		}
	})
}
