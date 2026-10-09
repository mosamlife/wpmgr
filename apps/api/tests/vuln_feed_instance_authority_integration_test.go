package tests

// vuln_feed_instance_authority_integration_test.go: the vulnerability-feed key
// routes (GET /api/v1/admin/vuln-feed/status, PUT and DELETE .../key, POST
// .../sync) admit exactly the instance-email authority, proven against real
// rows (GH #361).
//
// The contract (owner ruling, 2026-10-09): whoever may manage the instance
// email settings may manage the feed key. On an install with one live
// organisation its owner is admitted without being a superadmin; an admin of
// that organisation, an API key minted in it, and the owner of one of several
// organisations are refused with the admin family's one refusal.
//
// Everything is reached through the REAL router: server.New builds the engine,
// so the admin routes sit on the group server.go mounts them on, behind the
// session load and middleware.Authenticate, then admin.Handler.Register with
// the vuln-feed sub-handler wired as cmd/wpmgr wires it: the same
// admingate.InstanceEmailPoolStore handed to the feed routes and to the auth
// handler's Me capability. The pool is wpmgr_app (NOSUPERUSER, NOBYPASSRLS),
// and every observation goes through a production transaction helper with the
// connected role asserted inside it: the stored key through InAgentTx (the
// instance_settings policy keys on app.agent, as admin.InstanceSettingsRepo
// reads it), the instance trail through InUserTx, an organisation's audit_log
// through InTenantTx. No test here bootstraps, so install_owner stays empty and
// the install-owner arm admits nobody.
//
// NOT RUN BY CI. Run with `make test-integration` from the repository root.

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/admin"
	"github.com/mosamlife/wpmgr/apps/api/internal/admingate"
	"github.com/mosamlife/wpmgr/apps/api/internal/apikey"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/auth"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/config"
	"github.com/mosamlife/wpmgr/apps/api/internal/cryptbox"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/middleware"
	"github.com/mosamlife/wpmgr/apps/api/internal/server"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
	"github.com/mosamlife/wpmgr/apps/api/internal/tenant"
)

const (
	vfIAStatusPath = "/api/v1/admin/vuln-feed/status"
	vfIAKeyPath    = "/api/v1/admin/vuln-feed/key"
	vfIASyncPath   = "/api/v1/admin/vuln-feed/sync"
	vfIAKeyBody    = `{"key":"integration-feed-key-0361"}`
	// vfIASettingKey is the instance_settings row the feed key lives in.
	vfIASettingKey = "vuln_feed.wordfence_api_key"
	// vfIARefusal is the admin route family's one refusal code.
	vfIARefusal = "superadmin_required"
)

// vfIAEnqueuer counts feed refreshes instead of inserting River jobs.
type vfIAEnqueuer struct{ calls int }

func (e *vfIAEnqueuer) EnqueueFeedRefresh(context.Context) error {
	e.calls++
	return nil
}

type vfIARig struct {
	engine *gin.Engine
	keySvc *admin.VulnFeedKeyService
	enq    *vfIAEnqueuer
}

// vfIAEngine builds the production router with server.New, with the admin
// handler and the auth handler both wired with gate, the way cmd/wpmgr wires
// them. Every optional handler is left nil, which server.New skips.
func vfIAEngine(t *testing.T, s *smtpIAStack, gate admingate.InstanceEmailStore) vfIARig {
	t.Helper()
	gin.SetMode(gin.TestMode)
	clock := domain.SystemClock{}
	validator := domain.NewValidator()
	keys := apikey.NewService(s.pool)
	authH := auth.NewHandler(s.authSvc, s.sessions, nil, nil)
	authH.SetInstanceAuthorityGate(gate)

	age, err := cryptbox.NewAgeIdentity("")
	if err != nil {
		t.Fatalf("age identity: %v", err)
	}
	enq := &vfIAEnqueuer{}
	keySvc := admin.NewVulnFeedKeyService(admin.NewInstanceSettingsRepo(s.pool), age, "", enq, nil)
	adminH := admin.NewHandler(admin.NewService(admin.NewRepo(s.pool), nil), s.pool)
	adminH.SetAuditRecorder(s.rec)
	adminH.SetVulnFeed(nil, keySvc, gate)

	srv := server.New(server.Deps{
		Config:   config.Config{},
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Pool:     s.pool,
		Sessions: s.sessions,
		Auth:     middleware.NewAuthenticator(s.sessions, s.authSvc, keys, s.pool),
		AuthH:    authH,
		MembersH: auth.NewMembersHandler(s.authSvc, nil),
		APIKeyH:  apikey.NewHandler(keys, s.rec),
		AuditH:   audit.NewHandler(s.rec),
		TenantH:  tenant.NewHandler(tenant.NewService(tenant.NewRepo(s.pool), validator, clock), s.rec),
		SiteH:    site.NewHandler(site.NewService(site.NewRepo(s.pool), validator, clock), s.rec, ""),
		AdminH:   adminH,
	})
	e, ok := srv.Handler().(*gin.Engine)
	if !ok {
		t.Fatalf("server.Handler() is %T, want *gin.Engine", srv.Handler())
	}
	return vfIARig{engine: e, keySvc: keySvc, enq: enq}
}

// vfIAKeyStored reports whether the feed key row exists, read through InAgentTx
// as wpmgr_app.
func vfIAKeyStored(t *testing.T, s *smtpIAStack) bool {
	t.Helper()
	ctx := context.Background()
	var n int
	err := s.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		requireAppRoleInTx(t, ctx, tx)
		return tx.QueryRow(ctx, `SELECT count(*) FROM instance_settings WHERE key = $1`, vfIASettingKey).Scan(&n)
	})
	if err != nil {
		t.Fatalf("read instance_settings: %v", err)
	}
	return n == 1
}

// vfIASystemAudit counts instance-trail rows for action by actorID admitted
// under arm, read through InUserTx as the actor.
func vfIASystemAudit(t *testing.T, s *smtpIAStack, actorID uuid.UUID, action, arm string) int {
	t.Helper()
	ctx := context.Background()
	var n int
	err := s.pool.InUserTx(ctx, actorID, func(tx pgx.Tx) error {
		requireAppRoleInTx(t, ctx, tx)
		return tx.QueryRow(ctx, `
			SELECT count(*) FROM system_audit_log
			WHERE action = $1 AND actor_type = 'user' AND actor_id = $2
			  AND tenant_id = '00000000-0000-0000-0000-000000000000'
			  AND metadata->>'admitted_as' = $3
			  AND metadata->>'target_id' = $4`,
			action, actorID, arm, vfIASettingKey).Scan(&n)
	})
	if err != nil {
		t.Fatalf("read system_audit_log: %v", err)
	}
	return n
}

// vfIATenantAudit counts audit_log rows in tenantID for action by actorID,
// read through InTenantTx.
func vfIATenantAudit(t *testing.T, s *smtpIAStack, tenantID, actorID uuid.UUID, action string) int {
	t.Helper()
	ctx := context.Background()
	var n int
	err := s.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		requireAppRoleInTx(t, ctx, tx)
		return tx.QueryRow(ctx, `
			SELECT count(*) FROM audit_log
			WHERE tenant_id = $1 AND action = $2 AND actor_type = 'user' AND actor_id = $3
			  AND target_id = $4`,
			tenantID, action, actorID.String(), vfIASettingKey).Scan(&n)
	})
	if err != nil {
		t.Fatalf("read audit_log: %v", err)
	}
	return n
}

// vfIARequireRefused asserts all four routes answer the admin family's 403
// through send, and that the stored key and the refresh queue are untouched.
func vfIARequireRefused(t *testing.T, s *smtpIAStack, rig vfIARig, send func(method, path, body string) smtpIAResp) {
	t.Helper()
	storedBefore, queuedBefore := vfIAKeyStored(t, s), rig.enq.calls
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, vfIAStatusPath, ""},
		{http.MethodPut, vfIAKeyPath, vfIAKeyBody},
		{http.MethodDelete, vfIAKeyPath, ""},
		{http.MethodPost, vfIASyncPath, ""},
	} {
		r := send(c.method, c.path, c.body)
		if r.status != http.StatusForbidden || r.code != vfIARefusal {
			t.Errorf("%s %s: got %d %q (%s); want 403 %q", c.method, c.path, r.status, r.code, r.body, vfIARefusal)
		}
	}
	if stored := vfIAKeyStored(t, s); stored != storedBefore {
		t.Errorf("the stored feed key changed under refused requests: %v -> %v", storedBefore, stored)
	}
	if rig.enq.calls != queuedBefore {
		t.Errorf("feed refreshes queued under refused requests: %d -> %d", queuedBefore, rig.enq.calls)
	}
}

func TestVulnFeedKey_SingleTenantInstall_InstanceEmailAuthority(t *testing.T) {
	s := newSMTPIAStack(t)
	gate := admingate.NewInstanceEmailPoolStore(s.pool, false)
	rig := vfIAEngine(t, s, gate)

	sfx := uuid.NewString()[:8]
	org := seedTenant(t, s.pool, "vf-ia-solo-"+sfx)
	owner := seedUserMembership(t, s.authRepo, "vf-ia-owner-"+sfx+"@example.com", org, authz.RoleOwner)
	adminU := seedUserMembership(t, s.authRepo, "vf-ia-admin-"+sfx+"@example.com", org, authz.RoleAdmin)

	if n := s.liveTenantCount(t, org); n != 1 {
		t.Fatalf("PREMISE FAILED: %d live organisations, want exactly 1", n)
	}
	if role := s.membershipRole(t, org, adminU.ID); role != string(authz.RoleAdmin) {
		t.Fatalf("PREMISE FAILED: the member's role is %q, want admin", role)
	}
	for _, u := range []uuid.UUID{owner.ID, adminU.ID} {
		if isSA, err := admingate.NewPoolStore(s.pool).IsSuperadmin(context.Background(), u); err != nil || isSA {
			t.Fatalf("PREMISE FAILED: IsSuperadmin = %v, %v; want false, nil", isSA, err)
		}
	}

	t.Run("owner of the only organisation manages the key and is recorded in both trails", func(t *testing.T) {
		ctx := s.session(t, owner.ID, org)
		send := func(method, path, body string) smtpIAResp { return smtpIADo(rig.engine, ctx, method, path, body) }
		requireCapability(t, rig.engine, ctx, true)
		sysBefore := vfIASystemAudit(t, s, owner.ID, "admin.vuln_feed.key.set", admingate.ArmSoleLiveTenantOwner.String())
		orgBefore := vfIATenantAudit(t, s, org, owner.ID, "admin.vuln_feed.key.set")

		if r := send(http.MethodGet, vfIAStatusPath, ""); r.status != http.StatusOK {
			t.Errorf("GET status: got %d %q (%s); want 200", r.status, r.code, r.body)
		}
		if r := send(http.MethodPut, vfIAKeyPath, vfIAKeyBody); r.status != http.StatusOK {
			t.Errorf("PUT key: got %d %q (%s); want 200", r.status, r.code, r.body)
		}
		if !vfIAKeyStored(t, s) {
			t.Error("PUT key answered but no key row was written")
		}
		if r := send(http.MethodPost, vfIASyncPath, ""); r.status != http.StatusAccepted {
			t.Errorf("POST sync: got %d %q (%s); want 202", r.status, r.code, r.body)
		}
		if rig.enq.calls != 2 {
			t.Errorf("feed refreshes queued = %d, want 2 (PUT key, POST sync)", rig.enq.calls)
		}
		if n := vfIASystemAudit(t, s, owner.ID, "admin.vuln_feed.key.set", admingate.ArmSoleLiveTenantOwner.String()); n != sysBefore+1 {
			t.Errorf("instance trail rows for the key set went %d -> %d; want exactly one more", sysBefore, n)
		}
		if n := vfIATenantAudit(t, s, org, owner.ID, "admin.vuln_feed.key.set"); n != orgBefore+1 {
			t.Errorf("audit_log rows in the sole organisation went %d -> %d; want exactly one more", orgBefore, n)
		}
		if r := send(http.MethodDelete, vfIAKeyPath, ""); r.status != http.StatusOK {
			t.Errorf("DELETE key: got %d %q (%s); want 200", r.status, r.code, r.body)
		}
		if vfIAKeyStored(t, s) {
			t.Error("DELETE key answered but the key row is still there")
		}
	})

	// Seeded through the production service, so the refusals below have a
	// stored key that a refused DELETE must leave in place.
	if err := rig.keySvc.SetKey(context.Background(), "seeded-feed-key-0361"); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	if !vfIAKeyStored(t, s) {
		t.Fatal("PREMISE FAILED: the seeded key is not stored")
	}

	t.Run("admin of the only organisation is refused", func(t *testing.T) {
		ctx := s.session(t, adminU.ID, org)
		vfIARequireRefused(t, s, rig, func(method, path, body string) smtpIAResp {
			return smtpIADo(rig.engine, ctx, method, path, body)
		})
		requireCapability(t, rig.engine, ctx, false)
	})

	t.Run("owner-role API key of the only organisation is refused", func(t *testing.T) {
		created, err := apikey.NewService(s.pool).Create(context.Background(), org, "vf-ia-key", authz.RoleOwner)
		if err != nil {
			t.Fatalf("create API key: %v", err)
		}
		vfIARequireRefused(t, s, rig, func(method, path, body string) smtpIAResp {
			return smtpIADoKey(rig.engine, created.Token, method, path, body)
		})
	})

	// A second live organisation closes the sole-owner arm on the next
	// request, and with no install owner recorded nothing else admits.
	t.Run("owner of one of two organisations is refused", func(t *testing.T) {
		seedTenant(t, s.pool, "vf-ia-second-"+sfx)
		if n := s.liveTenantCount(t, org); n != 2 {
			t.Fatalf("PREMISE FAILED: %d live organisations after seeding a second, want 2", n)
		}
		ctx := s.session(t, owner.ID, org)
		vfIARequireRefused(t, s, rig, func(method, path, body string) smtpIAResp {
			return smtpIADo(rig.engine, ctx, method, path, body)
		})
		requireCapability(t, rig.engine, ctx, false)
	})
}
