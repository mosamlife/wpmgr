package agentrelease_test

// mirror_capability_integration_test.go: agent_mirror.can_check_now proven
// against a REAL Postgres, through the REAL /fleet/agents route, with real
// tenants, users and memberships (GH #322).
//
// mirror_capability_test.go proves the field is wired to the shared decision
// and fails closed, but with a fake store "owner of one of two live
// organisations" and "non-owner member of the only organisation" are the same
// input. Only real rows can tell those apart, because the difference lives
// inside admingate's SQL and inside the RLS policies it runs under. That is
// what this file is for.
//
// The fleet ROLLUP is still faked here (a fake site lister and version reader):
// this file is about the permission, and seeding the whole sites/agent
// inventory would test something else. The gate store is the real
// admingate.PoolStore over the real pool, connected as the non-superuser
// wpmgr_app role, so the membership read genuinely depends on the
// memberships_self_read policy and the app.user_id GUC that InUserTx sets. On
// the bare pool that EXISTS silently sees nothing and every answer here would
// be false: wrong in the refusing direction, which is a button that never
// appears for the one person this was built for.
//
// Docker-backed (testcontainers), mirroring internal/admin/gate_integration_
// test.go's harness. Skips when Docker is unavailable.
//
// AGAINST THE PRE-CHANGE CODE every case here fails, including the ones
// expecting false: can_check_now did not exist on the wire, and
// assertCanCheckNow refuses an absent field before it ever compares a value.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/mosamlife/wpmgr/apps/api/internal/admingate"
	"github.com/mosamlife/wpmgr/apps/api/internal/agentrelease"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
)

// startCapabilityPostgres returns (appPool, adminPool). appPool connects as the
// non-superuser wpmgr_app role, so RLS is actually in force.
func startCapabilityPostgres(t *testing.T) (*db.Pool, *db.Pool) {
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

// capabilitySeed is the per-case fixture helper. Every case starts from empty
// tenants/users tables, because the decision's whole question is an
// install-wide count and leftover rows from a previous case would answer it.
type capabilitySeed struct {
	t     *testing.T
	admin *db.Pool
	ctx   context.Context
}

func newCapabilitySeed(t *testing.T, adminPool *db.Pool) *capabilitySeed {
	t.Helper()
	ctx := context.Background()
	for _, stmt := range []string{`DELETE FROM tenants`, `DELETE FROM users`} {
		if _, err := adminPool.Exec(ctx, stmt); err != nil {
			t.Fatalf("reset (%q): %v", stmt, err)
		}
	}
	return &capabilitySeed{t: t, admin: adminPool, ctx: ctx}
}

func (s *capabilitySeed) tenant(name string) uuid.UUID {
	s.t.Helper()
	var id uuid.UUID
	slug := name + "-" + uuid.NewString()[:8]
	if err := s.admin.QueryRow(s.ctx, `INSERT INTO tenants (name, slug) VALUES ($1,$2) RETURNING id`,
		name, slug).Scan(&id); err != nil {
		s.t.Fatalf("seed tenant %s: %v", name, err)
	}
	return id
}

func (s *capabilitySeed) user(superadmin bool) uuid.UUID {
	s.t.Helper()
	var id uuid.UUID
	email := "cap-" + uuid.NewString()[:8] + "@example.com"
	if err := s.admin.QueryRow(s.ctx,
		`INSERT INTO users (email, is_superadmin) VALUES ($1,$2) RETURNING id`,
		email, superadmin).Scan(&id); err != nil {
		s.t.Fatalf("seed user: %v", err)
	}
	return id
}

func (s *capabilitySeed) member(tenantID, userID uuid.UUID, role string) {
	s.t.Helper()
	if _, err := s.admin.Exec(s.ctx,
		`INSERT INTO memberships (tenant_id, user_id, role) VALUES ($1,$2,$3)`,
		tenantID, userID, role); err != nil {
		s.t.Fatalf("seed membership (%s): %v", role, err)
	}
}

func (s *capabilitySeed) softDelete(tenantID uuid.UUID) {
	s.t.Helper()
	if _, err := s.admin.Exec(s.ctx, `UPDATE tenants SET deleted_at = now() WHERE id = $1`, tenantID); err != nil {
		s.t.Fatalf("soft-delete tenant: %v", err)
	}
}

// realCapabilityEngine mounts the REAL /fleet/agents route with the REAL
// admingate.PoolStore, so the answer comes from soleLiveTenantOwnerSQL under
// InUserTx exactly as it does in production.
func realCapabilityEngine(t *testing.T, pool *db.Pool, mirrorEnabled bool) *gin.Engine {
	t.Helper()
	h := agentrelease.NewHandler(
		agentrelease.NewService(&fakeSiteLister{}, &fakeVersionReader{version: "0.61.120"}),
		false,
	)
	h.SetMirror(&fakeMirrorStateReader{}, mirrorEnabled)
	h.SetMirrorCheckGate(admingate.NewPoolStore(pool))
	return newTestEngine(h)
}

// TestCanCheckNow_AgainstRealRows is the GH #322 capability decision table run
// against real tenants, memberships and RLS, read off the real wire field.
func TestCanCheckNow_AgainstRealRows(t *testing.T) {
	pool, adminPool := startCapabilityPostgres(t)
	engine := realCapabilityEngine(t, pool, true)

	// T1: a superadmin gets the capability on any install, here one with two
	// live organisations, which is the case where nobody else does.
	t.Run("superadmin is true regardless of the organisation count", func(t *testing.T) {
		s := newCapabilitySeed(t, adminPool)
		first := s.tenant("first")
		s.tenant("second")
		sa := s.user(true)
		assertCanCheckNow(t, engine, orgUser(first, sa), true, "superadmin on a two-organisation install")
	})

	// T2: the owner of the only live organisation. The whole point.
	t.Run("owner of the only live organisation is true", func(t *testing.T) {
		s := newCapabilitySeed(t, adminPool)
		only := s.tenant("only")
		owner := s.user(false)
		s.member(only, owner, "owner")
		assertCanCheckNow(t, engine, orgUser(only, owner), true, "owner of the sole organisation")
	})

	// T3: a SECOND live organisation exists, so there IS another tenant's
	// budget to protect and the path closes. This is the hosted multi-tenant
	// case, and the only thing separating it from T2 is the row count.
	t.Run("owner is false once a second live organisation exists", func(t *testing.T) {
		s := newCapabilitySeed(t, adminPool)
		first := s.tenant("first")
		owner := s.user(false)
		s.member(first, owner, "owner")
		assertCanCheckNow(t, engine, orgUser(first, owner), true,
			"control: the same owner is true while theirs is the only organisation")

		s.tenant("second")
		assertCanCheckNow(t, engine, orgUser(first, owner), false,
			"the same owner immediately after a second organisation appeared")
	})

	// T4: membership is not enough. Only role='owner' passes; the constraint
	// vocabulary is ('owner','admin','operator','viewer'). This is the case a
	// fake store cannot tell apart from T3.
	t.Run("non-owner member of the only organisation is false", func(t *testing.T) {
		for _, role := range []string{"admin", "operator", "viewer"} {
			t.Run(role, func(t *testing.T) {
				s := newCapabilitySeed(t, adminPool)
				only := s.tenant("only")
				s.member(only, s.user(false), "owner") // a real owner exists; the caller is not them
				member := s.user(false)
				s.member(only, member, role)
				assertCanCheckNow(t, engine, orgUser(only, member), false,
					role+" of the sole organisation")
			})
		}
	})

	// The soft-delete rule, which is what makes T3 a question about LIVE
	// organisations rather than rows: an organisation inside its restore window
	// has no principal able to act as it, so it has no budget share to protect.
	t.Run("a soft-deleted second organisation does not close the path", func(t *testing.T) {
		s := newCapabilitySeed(t, adminPool)
		live := s.tenant("live")
		gone := s.tenant("gone")
		owner := s.user(false)
		s.member(live, owner, "owner")
		s.member(gone, owner, "owner")
		assertCanCheckNow(t, engine, orgUser(live, owner), false,
			"control: two live organisations")

		s.softDelete(gone)
		assertCanCheckNow(t, engine, orgUser(live, owner), true,
			"one live and one soft-deleted organisation")
	})

	// T5, against real rows: with the mirror switched off nobody may trigger a
	// run, so the capability is false even for the caller who would otherwise
	// have it. Same rows, same principal, only WPMGR_UPDATE_AGENT_MIRROR_ENABLED
	// differs.
	t.Run("false for everyone when the mirror is disabled", func(t *testing.T) {
		s := newCapabilitySeed(t, adminPool)
		only := s.tenant("only")
		owner := s.user(false)
		s.member(only, owner, "owner")
		sa := s.user(true)
		s.member(only, sa, "viewer")

		assertCanCheckNow(t, engine, orgUser(only, owner), true,
			"control: sole owner with the mirror enabled")
		assertCanCheckNow(t, engine, orgUser(only, sa), true,
			"control: superadmin with the mirror enabled")

		off := realCapabilityEngine(t, pool, false)
		assertCanCheckNow(t, off, orgUser(only, owner), false,
			"sole owner with the mirror disabled")
		assertCanCheckNow(t, off, orgUser(only, sa), false,
			"superadmin with the mirror disabled")
	})
}
