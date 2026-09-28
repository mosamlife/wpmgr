package tests

// settings_smtp_instance_authority_integration_test.go: the instance SMTP
// settings routes (GET, PUT and POST /api/v1/settings/smtp[/test]) admit only
// instance-level authority, proven against real rows.
//
// The contract: the relay is one row for the whole install, so a principal
// qualifies only as a superadmin, or as the owner of the only live organisation
// on the install. A role inside one organisation of several does not qualify.
//
// Everything under test is reached through the REAL router: server.New builds
// the engine, so the settings routes sit on whichever group server.go mounts
// them on, behind the session load and middleware.Authenticate it puts in
// front of every group, then the real settings.Handler.Register
// (RequireOrgScope + the instance-authority gate). GET /auth/me is served by the
// same engine, so the can_manage_instance_email capability is read through the
// real auth handler. All of it runs over the pool startPostgres returns:
// wpmgr_app, NOSUPERUSER, NOBYPASSRLS. The gate store is the production
// admingate.PoolStore over that same pool, handed to both the settings handler
// and the auth handler exactly as cmd/wpmgr does.
//
// Reads that observe the database go through the production transaction
// helpers as wpmgr_app, and each one asserts the connected role inside its own
// transaction:
//   - the premise reads (membership role, live organisation count) use
//     InTenantTx, the helper the tenant-scoped request path uses;
//   - the smtp_settings snapshot uses InAgentTx, because that row's only RLS
//     policy keys on app.agent and settings.Repo reads it through InAgentTx.
//     Under InTenantTx the row is invisible, and "unchanged" would compare
//     nothing with nothing. The snapshot therefore also asserts it found a row.
//
// The superuser pool is used only for seeding the superadmin flag, which is
// how production seeds it (WPMGR_SUPERADMIN_EMAILS runs on the owner DSN), and
// to derive a connection string for the fail-closed case.
//
// NOT RUN BY CI. Run with `make test-integration` from the repository root.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alexedwards/scs/v2"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/admingate"
	"github.com/mosamlife/wpmgr/apps/api/internal/apikey"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/auth"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/config"
	"github.com/mosamlife/wpmgr/apps/api/internal/cryptbox"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/middleware"
	"github.com/mosamlife/wpmgr/apps/api/internal/server"
	"github.com/mosamlife/wpmgr/apps/api/internal/settings"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
	"github.com/mosamlife/wpmgr/apps/api/internal/tenant"
)

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

type smtpIAStack struct {
	pool     *db.Pool
	adminDB  *db.Pool
	authRepo *auth.Repo
	authSvc  *auth.Service
	rec      *audit.Recorder
	sessions *auth.SessionManager
	svc      *settings.Service
}

func newSMTPIAStack(t *testing.T) *smtpIAStack {
	t.Helper()
	pool := startPostgres(t) // wpmgr_app: NOSUPERUSER, NOBYPASSRLS
	adminDB := connectAdmin(t, pool)
	t.Cleanup(adminDB.Close)

	age, err := cryptbox.NewAgeIdentity("")
	if err != nil {
		t.Fatalf("age identity: %v", err)
	}
	rec := audit.NewRecorder(pool, domain.SystemClock{})
	authRepo := auth.NewRepo(pool)
	return &smtpIAStack{
		pool:     pool,
		adminDB:  adminDB,
		authRepo: authRepo,
		authSvc:  auth.NewService(authRepo, rec, domain.NewValidator()),
		rec:      rec,
		sessions: auth.NewSessionManagerWithStore(scs.New(), false),
		// nil mailer: every POST /test in this file runs against a disabled
		// stored config, so the service answers before it would send.
		svc: settings.NewService(settings.NewRepo(pool), age, nil, nil),
	}
}

// engine builds the production router with server.New. Deps carries the
// handlers server.New registers unconditionally, plus the settings handler and
// an auth handler wired with the given gate store, the way cmd/wpmgr wires
// both. Every optional handler is left nil, which server.New skips.
func (s *smtpIAStack) engine(t *testing.T, gate admingate.Store) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	clock := domain.SystemClock{}
	validator := domain.NewValidator()
	keys := apikey.NewService(s.pool)
	authH := auth.NewHandler(s.authSvc, s.sessions, nil, nil)
	authH.SetInstanceAuthorityGate(gate)
	srv := server.New(server.Deps{
		Config:    config.Config{},
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Pool:      s.pool,
		Sessions:  s.sessions,
		Auth:      middleware.NewAuthenticator(s.sessions, s.authSvc, keys, s.pool),
		AuthH:     authH,
		MembersH:  auth.NewMembersHandler(s.authSvc, nil),
		APIKeyH:   apikey.NewHandler(keys, s.rec),
		AuditH:    audit.NewHandler(s.rec),
		TenantH:   tenant.NewHandler(tenant.NewService(tenant.NewRepo(s.pool), validator, clock), s.rec),
		SiteH:     site.NewHandler(site.NewService(site.NewRepo(s.pool), validator, clock), s.rec, ""),
		SettingsH: settings.NewHandler(s.svc, s.rec, gate),
	})
	e, ok := srv.Handler().(*gin.Engine)
	if !ok {
		t.Fatalf("server.Handler() is %T, want *gin.Engine", srv.Handler())
	}
	return e
}

func (s *smtpIAStack) session(t *testing.T, userID, tenantID uuid.UUID) context.Context {
	t.Helper()
	ctx, err := s.sessions.SCS().Load(context.Background(), "")
	if err != nil {
		t.Fatalf("load session: %v", err)
	}
	if err := s.sessions.Login(ctx, userID, tenantID); err != nil {
		t.Fatalf("login: %v", err)
	}
	return ctx
}

// requireAppRoleInTx asserts, inside the caller's transaction, that RLS applies
// to the connected role.
func requireAppRoleInTx(t *testing.T, ctx context.Context, tx pgx.Tx) {
	t.Helper()
	var name string
	var super, bypass bool
	if err := tx.QueryRow(ctx,
		`SELECT current_user, rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).
		Scan(&name, &super, &bypass); err != nil {
		t.Fatalf("read the connected role: %v", err)
	}
	if name != "wpmgr_app" || super || bypass {
		t.Fatalf("transaction runs as %q (rolsuper=%v, rolbypassrls=%v); want wpmgr_app with neither",
			name, super, bypass)
	}
}

// liveTenantCount reads the live organisation count through InTenantTx.
func (s *smtpIAStack) liveTenantCount(t *testing.T, tenantID uuid.UUID) int {
	t.Helper()
	ctx := context.Background()
	var n int
	err := s.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		requireAppRoleInTx(t, ctx, tx)
		return tx.QueryRow(ctx, `SELECT count(*) FROM tenants WHERE deleted_at IS NULL`).Scan(&n)
	})
	if err != nil {
		t.Fatalf("count live tenants: %v", err)
	}
	return n
}

// membershipRole reads the caller's stored role through InTenantTx.
func (s *smtpIAStack) membershipRole(t *testing.T, tenantID, userID uuid.UUID) string {
	t.Helper()
	ctx := context.Background()
	var role string
	err := s.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		requireAppRoleInTx(t, ctx, tx)
		return tx.QueryRow(ctx,
			`SELECT role FROM memberships WHERE tenant_id = $1 AND user_id = $2`, tenantID, userID).Scan(&role)
	})
	if err != nil {
		t.Fatalf("read membership role: %v", err)
	}
	return role
}

// smtpRow snapshots the whole smtp_settings row as JSON through InAgentTx.
// found=false means no row exists.
func (s *smtpIAStack) smtpRow(t *testing.T) (row string, found bool) {
	t.Helper()
	ctx := context.Background()
	err := s.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		requireAppRoleInTx(t, ctx, tx)
		qerr := tx.QueryRow(ctx, `SELECT row_to_json(s)::text FROM smtp_settings s`).Scan(&row)
		if errors.Is(qerr, pgx.ErrNoRows) {
			return nil
		}
		if qerr != nil {
			return qerr
		}
		found = true
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot smtp_settings: %v", err)
	}
	return row, found
}

// seedRelay writes a disabled relay with a stored password through the
// production service, so the row under test has every column populated.
func (s *smtpIAStack) seedRelay(t *testing.T, by uuid.UUID) {
	t.Helper()
	pw := "seeded-relay-secret"
	if _, err := s.svc.Update(context.Background(), settings.SMTPUpdate{
		Enabled:     false,
		Host:        "relay-a.example.test",
		Port:        587,
		Username:    "relay-a-user",
		Password:    &pw,
		FromAddress: "noreply@relay-a.example.test",
		FromName:    "Relay A",
		TLSMode:     "starttls",
	}, by); err != nil {
		t.Fatalf("seed relay: %v", err)
	}
}

func (s *smtpIAStack) makeSuperadmin(t *testing.T, userID uuid.UUID) {
	t.Helper()
	tag, err := s.adminDB.Exec(context.Background(), `UPDATE users SET is_superadmin = true WHERE id = $1`, userID)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("seed superadmin: rows=%d err=%v", tag.RowsAffected(), err)
	}
}

type smtpIAResp struct {
	status int
	code   string
	body   string
}

func smtpIADo(e *gin.Engine, ctx context.Context, method, path, body string) smtpIAResp {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	e.ServeHTTP(w, req)
	var env struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	return smtpIAResp{status: w.Code, code: env.Code, body: w.Body.String()}
}

const (
	smtpPath     = "/api/v1/settings/smtp"
	smtpTestPath = "/api/v1/settings/smtp/test"
	// A disabled config keeps POST /test from reaching the (nil) mailer when a
	// request is admitted.
	smtpPutBody  = `{"enabled":false,"host":"relay-b.example.test","port":2525,"username":"relay-b-user","password":"relay-b-secret","from_address":"noreply@relay-b.example.test","from_name":"Relay B","tls_mode":"tls","allow_insecure_tls":false}`
	smtpTestBody = `{"to_address":"inbox@example.test"}`
)

// requireAllRefused asserts GET, PUT and POST /test all answer the gate's 403.
func requireAllRefused(t *testing.T, e *gin.Engine, ctx context.Context) {
	t.Helper()
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, smtpPath, ""},
		{http.MethodPut, smtpPath, smtpPutBody},
		{http.MethodPost, smtpTestPath, smtpTestBody},
	} {
		r := smtpIADo(e, ctx, c.method, c.path, c.body)
		if r.status != http.StatusForbidden || r.code != settings.InstanceAuthorityRequiredCode {
			t.Errorf("%s %s: got %d %q (%s); want 403 %q",
				c.method, c.path, r.status, r.code, r.body, settings.InstanceAuthorityRequiredCode)
		}
	}
}

// requireAllAdmitted asserts GET, PUT and POST /test all reach the handler and
// succeed, and that the PUT really wrote the row.
func requireAllAdmitted(t *testing.T, s *smtpIAStack, e *gin.Engine, ctx context.Context) {
	t.Helper()
	before, _ := s.smtpRow(t)

	if r := smtpIADo(e, ctx, http.MethodGet, smtpPath, ""); r.status != http.StatusOK {
		t.Errorf("GET: got %d %q (%s); want 200", r.status, r.code, r.body)
	}
	r := smtpIADo(e, ctx, http.MethodPut, smtpPath, smtpPutBody)
	if r.status != http.StatusOK {
		t.Errorf("PUT: got %d %q (%s); want 200", r.status, r.code, r.body)
	}
	after, found := s.smtpRow(t)
	if !found || after == before || !strings.Contains(after, "relay-b.example.test") {
		t.Errorf("PUT answered %d but the row was not written: found=%v row=%s", r.status, found, after)
	}
	// The handler answers 200 with {ok, message}; the stored config is disabled,
	// so ok is false and nothing is sent. Seeing that shape proves the request
	// reached the handler.
	tr := smtpIADo(e, ctx, http.MethodPost, smtpTestPath, smtpTestBody)
	var res struct {
		OK      *bool  `json:"ok"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal([]byte(tr.body), &res)
	if tr.status != http.StatusOK || res.OK == nil || !strings.Contains(res.Message, "enabled SMTP configuration") {
		t.Errorf("POST /test: got %d (%s); want 200 from the handler", tr.status, tr.body)
	}
}

// requireCapability asserts that GET /auth/me, served by the same engine,
// reports can_manage_instance_email and that it equals want. An absent field
// fails: absence must never be what a test reads as false.
func requireCapability(t *testing.T, e *gin.Engine, ctx context.Context, want bool) {
	t.Helper()
	r := smtpIADo(e, ctx, http.MethodGet, "/auth/me", "")
	if r.status != http.StatusOK {
		t.Fatalf("GET /auth/me: got %d (%s); want 200", r.status, r.body)
	}
	var me struct {
		CanManageInstanceEmail *bool `json:"can_manage_instance_email"`
	}
	if err := json.Unmarshal([]byte(r.body), &me); err != nil {
		t.Fatalf("decode /auth/me: %v (%s)", err, r.body)
	}
	if me.CanManageInstanceEmail == nil {
		t.Fatalf("GET /auth/me omitted can_manage_instance_email: %s", r.body)
	}
	if *me.CanManageInstanceEmail != want {
		t.Errorf("can_manage_instance_email = %v, want %v (it must equal the route gate's decision)",
			*me.CanManageInstanceEmail, want)
	}
}

// smtpIADoKey sends a request authenticated by an API key, with no session.
func smtpIADoKey(e *gin.Engine, token, method, path, body string) smtpIAResp {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	e.ServeHTTP(w, req)
	var env struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	return smtpIAResp{status: w.Code, code: env.Code, body: w.Body.String()}
}

// systemAuditUpdates counts system_audit_log rows recording an SMTP settings
// update by actorID with the nil tenant id. Read through InUserTx as the actor,
// the helper the writer uses.
func (s *smtpIAStack) systemAuditUpdates(t *testing.T, actorID uuid.UUID) int {
	t.Helper()
	ctx := context.Background()
	var n int
	err := s.pool.InUserTx(ctx, actorID, func(tx pgx.Tx) error {
		requireAppRoleInTx(t, ctx, tx)
		return tx.QueryRow(ctx, `
			SELECT count(*) FROM system_audit_log
			WHERE action = $1 AND actor_type = 'user' AND actor_id = $2
			  AND tenant_id = '00000000-0000-0000-0000-000000000000'
			  AND metadata->>'host' = 'relay-b.example.test'
			  AND metadata->>'target_type' = 'smtp_settings'`,
			settings.AuditActionUpdate, actorID).Scan(&n)
	})
	if err != nil {
		t.Fatalf("read system_audit_log: %v", err)
	}
	return n
}

// tenantAuditUpdates counts audit_log rows in tenantID recording an SMTP
// settings update by actorID, read through InTenantTx.
func (s *smtpIAStack) tenantAuditUpdates(t *testing.T, tenantID, actorID uuid.UUID) int {
	t.Helper()
	ctx := context.Background()
	var n int
	err := s.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		requireAppRoleInTx(t, ctx, tx)
		return tx.QueryRow(ctx, `
			SELECT count(*) FROM audit_log
			WHERE tenant_id = $1 AND action = $2 AND actor_type = 'user' AND actor_id = $3`,
			tenantID, settings.AuditActionUpdate, actorID.String()).Scan(&n)
	})
	if err != nil {
		t.Fatalf("read audit_log: %v", err)
	}
	return n
}

// seedUserOnly creates a user with no membership anywhere.
func seedUserOnly(t *testing.T, repo *auth.Repo, email string) auth.User {
	t.Helper()
	u, err := repo.CreateUser(context.Background(), email, "", email, "", "")
	if err != nil {
		t.Fatalf("create user %s: %v", email, err)
	}
	return u
}

// ---------------------------------------------------------------------------
// Multi-organisation install
// ---------------------------------------------------------------------------

func TestSMTPSettings_MultiTenantInstall_InstanceAuthority(t *testing.T) {
	s := newSMTPIAStack(t)
	e := s.engine(t, admingate.NewPoolStore(s.pool))

	sfx := uuid.NewString()[:8]
	tenantA := seedTenant(t, s.pool, "smtp-ia-a-"+sfx)
	tenantB := seedTenant(t, s.pool, "smtp-ia-b-"+sfx)
	ownerA := seedUserMembership(t, s.authRepo, "smtp-ia-owner-a-"+sfx+"@example.com", tenantA, authz.RoleOwner)
	adminA := seedUserMembership(t, s.authRepo, "smtp-ia-admin-a-"+sfx+"@example.com", tenantA, authz.RoleAdmin)
	superU := seedUserMembership(t, s.authRepo, "smtp-ia-super-"+sfx+"@example.com", tenantB, authz.RoleViewer)
	s.makeSuperadmin(t, superU.ID)
	// The operator of a multi-organisation install: a superadmin who belongs
	// to no organisation, so every request carries no active tenant.
	loneSuper := seedUserOnly(t, s.authRepo, "smtp-ia-lone-super-"+sfx+"@example.com")
	s.makeSuperadmin(t, loneSuper.ID)
	// A superadmin whose active session is a site collaboration in tenantA:
	// site-scoped, with no membership there.
	siteSuper := seedUserOnly(t, s.authRepo, "smtp-ia-site-super-"+sfx+"@example.com")
	s.makeSuperadmin(t, siteSuper.ID)
	seedSiteShare(t, s.adminDB, tenantA, seedSite(t, s.pool, tenantA, "https://smtp-ia-"+sfx+".example.test"), siteSuper.ID, "viewer")
	s.seedRelay(t, superU.ID)

	// THE PREMISE, asserted rather than assumed: more than one live
	// organisation, the caller holds 'owner' in theirs, and is not a superadmin.
	if n := s.liveTenantCount(t, tenantA); n < 2 {
		t.Fatalf("PREMISE FAILED: %d live organisations, want at least 2", n)
	}
	if role := s.membershipRole(t, tenantA, ownerA.ID); role != string(authz.RoleOwner) {
		t.Fatalf("PREMISE FAILED: caller's role is %q, want owner", role)
	}
	store := admingate.NewPoolStore(s.pool)
	if isSA, err := store.IsSuperadmin(context.Background(), ownerA.ID); err != nil || isSA {
		t.Fatalf("PREMISE FAILED: IsSuperadmin(owner) = %v, %v; want false, nil", isSA, err)
	}

	t.Run("organisation owner who is not a superadmin is refused and the row is unchanged", func(t *testing.T) {
		before, found := s.smtpRow(t)
		if !found {
			t.Fatal("PREMISE FAILED: no smtp_settings row to protect; an empty-vs-empty comparison proves nothing")
		}
		ctx := s.session(t, ownerA.ID, tenantA)
		requireAllRefused(t, e, ctx)
		after, _ := s.smtpRow(t)
		if after != before {
			t.Errorf("smtp_settings changed under refused requests:\nbefore %s\nafter  %s", before, after)
		}
		requireCapability(t, e, ctx, false)
	})

	t.Run("organisation admin is refused", func(t *testing.T) {
		before, _ := s.smtpRow(t)
		ctx := s.session(t, adminA.ID, tenantA)
		requireAllRefused(t, e, ctx)
		if after, _ := s.smtpRow(t); after != before {
			t.Errorf("smtp_settings changed under refused requests")
		}
		requireCapability(t, e, ctx, false)
	})

	t.Run("API key is refused, even an owner-role key", func(t *testing.T) {
		created, err := apikey.NewService(s.pool).Create(context.Background(), tenantA, "smtp-ia-key", authz.RoleOwner)
		if err != nil {
			t.Fatalf("create API key: %v", err)
		}
		before, _ := s.smtpRow(t)
		for _, c := range []struct{ method, path, body string }{
			{http.MethodGet, smtpPath, ""},
			{http.MethodPut, smtpPath, smtpPutBody},
			{http.MethodPost, smtpTestPath, smtpTestBody},
		} {
			r := smtpIADoKey(e, created.Token, c.method, c.path, c.body)
			if r.status != http.StatusForbidden || r.code != settings.InstanceAuthorityRequiredCode {
				t.Errorf("%s %s with an API key: got %d %q (%s); want 403 %q",
					c.method, c.path, r.status, r.code, r.body, settings.InstanceAuthorityRequiredCode)
			}
		}
		if after, _ := s.smtpRow(t); after != before {
			t.Errorf("smtp_settings changed under refused requests")
		}
		// Me is a session resource: a key is never offered the capability.
		if r := smtpIADoKey(e, created.Token, http.MethodGet, "/auth/me", ""); r.status != http.StatusUnauthorized {
			t.Errorf("GET /auth/me with an API key: got %d (%s); want 401", r.status, r.body)
		}
	})

	t.Run("site-scoped principal is refused, even a superadmin", func(t *testing.T) {
		before, _ := s.smtpRow(t)
		ctx := s.session(t, siteSuper.ID, tenantA)
		for _, c := range []struct{ method, path, body string }{
			{http.MethodGet, smtpPath, ""},
			{http.MethodPut, smtpPath, smtpPutBody},
			{http.MethodPost, smtpTestPath, smtpTestBody},
		} {
			r := smtpIADo(e, ctx, c.method, c.path, c.body)
			if r.status != http.StatusForbidden || r.code != "org_scope_required" {
				t.Errorf("%s %s site-scoped: got %d %q (%s); want 403 org_scope_required",
					c.method, c.path, r.status, r.code, r.body)
			}
		}
		if after, _ := s.smtpRow(t); after != before {
			t.Errorf("smtp_settings changed under refused requests")
		}
		requireCapability(t, e, ctx, false)
	})

	t.Run("superadmin with a membership is admitted on GET, PUT and POST /test", func(t *testing.T) {
		ctx := s.session(t, superU.ID, tenantB)
		requireCapability(t, e, ctx, true)
		requireAllAdmitted(t, s, e, ctx)
		if n := s.tenantAuditUpdates(t, tenantB, superU.ID); n < 1 {
			t.Errorf("PUT with an active organisation left %d audit_log rows in it; want at least 1", n)
		}
	})

	t.Run("superadmin with no membership and no active organisation is admitted and audited", func(t *testing.T) {
		ctx := s.session(t, loneSuper.ID, uuid.Nil)
		requireCapability(t, e, ctx, true)
		if n := s.systemAuditUpdates(t, loneSuper.ID); n != 0 {
			t.Fatalf("PREMISE FAILED: %d system_audit_log rows for this actor before any PUT", n)
		}
		requireAllAdmitted(t, s, e, ctx)
		if n := s.systemAuditUpdates(t, loneSuper.ID); n != 1 {
			t.Errorf("PUT with no active organisation left %d system_audit_log rows naming the actor; want 1", n)
		}
	})
}

// ---------------------------------------------------------------------------
// Single-organisation install (the self-hosted case)
// ---------------------------------------------------------------------------

func TestSMTPSettings_SingleTenantInstall_InstanceAuthority(t *testing.T) {
	s := newSMTPIAStack(t)
	e := s.engine(t, admingate.NewPoolStore(s.pool))

	sfx := uuid.NewString()[:8]
	tenant := seedTenant(t, s.pool, "smtp-ia-solo-"+sfx)
	owner := seedUserMembership(t, s.authRepo, "smtp-ia-solo-owner-"+sfx+"@example.com", tenant, authz.RoleOwner)
	adminU := seedUserMembership(t, s.authRepo, "smtp-ia-solo-admin-"+sfx+"@example.com", tenant, authz.RoleAdmin)
	s.seedRelay(t, owner.ID)

	if n := s.liveTenantCount(t, tenant); n != 1 {
		t.Fatalf("PREMISE FAILED: %d live organisations, want exactly 1", n)
	}
	if role := s.membershipRole(t, tenant, owner.ID); role != string(authz.RoleOwner) {
		t.Fatalf("PREMISE FAILED: owner's role is %q", role)
	}
	if isSA, err := admingate.NewPoolStore(s.pool).IsSuperadmin(context.Background(), owner.ID); err != nil || isSA {
		t.Fatalf("PREMISE FAILED: IsSuperadmin(owner) = %v, %v; want false, nil", isSA, err)
	}

	t.Run("sole owner is admitted on GET, PUT and POST /test with no superadmin configured", func(t *testing.T) {
		ctx := s.session(t, owner.ID, tenant)
		requireCapability(t, e, ctx, true)
		requireAllAdmitted(t, s, e, ctx)
		if n := s.tenantAuditUpdates(t, tenant, owner.ID); n < 1 {
			t.Errorf("PUT by the sole owner left %d audit_log rows in their organisation; want at least 1", n)
		}
	})

	t.Run("admin of the sole organisation is refused", func(t *testing.T) {
		before, _ := s.smtpRow(t)
		ctx := s.session(t, adminU.ID, tenant)
		requireAllRefused(t, e, ctx)
		if after, _ := s.smtpRow(t); after != before {
			t.Errorf("smtp_settings changed under refused requests:\nbefore %s\nafter  %s", before, after)
		}
		requireCapability(t, e, ctx, false)
	})

	t.Run("owner-role API key of the sole organisation is refused", func(t *testing.T) {
		created, err := apikey.NewService(s.pool).Create(context.Background(), tenant, "smtp-ia-solo-key", authz.RoleOwner)
		if err != nil {
			t.Fatalf("create API key: %v", err)
		}
		before, _ := s.smtpRow(t)
		for _, c := range []struct{ method, path, body string }{
			{http.MethodGet, smtpPath, ""},
			{http.MethodPut, smtpPath, smtpPutBody},
			{http.MethodPost, smtpTestPath, smtpTestBody},
		} {
			r := smtpIADoKey(e, created.Token, c.method, c.path, c.body)
			if r.status != http.StatusForbidden || r.code != settings.InstanceAuthorityRequiredCode {
				t.Errorf("%s %s with an API key: got %d %q (%s); want 403 %q",
					c.method, c.path, r.status, r.code, r.body, settings.InstanceAuthorityRequiredCode)
			}
		}
		if after, _ := s.smtpRow(t); after != before {
			t.Errorf("smtp_settings changed under refused requests")
		}
	})

	t.Run("site collaborator in the sole organisation is refused", func(t *testing.T) {
		collab := seedUserOnly(t, s.authRepo, "smtp-ia-solo-collab-"+sfx+"@example.com")
		seedSiteShare(t, s.adminDB, tenant, seedSite(t, s.pool, tenant, "https://smtp-ia-solo-"+sfx+".example.test"), collab.ID, "viewer")
		before, _ := s.smtpRow(t)
		ctx := s.session(t, collab.ID, tenant)
		for _, c := range []struct{ method, path, body string }{
			{http.MethodGet, smtpPath, ""},
			{http.MethodPut, smtpPath, smtpPutBody},
			{http.MethodPost, smtpTestPath, smtpTestBody},
		} {
			r := smtpIADo(e, ctx, c.method, c.path, c.body)
			if r.status != http.StatusForbidden || r.code != "org_scope_required" {
				t.Errorf("%s %s site-scoped: got %d %q (%s); want 403 org_scope_required",
					c.method, c.path, r.status, r.code, r.body)
			}
		}
		if after, _ := s.smtpRow(t); after != before {
			t.Errorf("smtp_settings changed under refused requests")
		}
		requireCapability(t, e, ctx, false)
	})

	// Fail closed. failing is the production PoolStore over a pool that has
	// been closed, so each read returns a real error from the real store.
	failing := admingate.NewPoolStore(closedAppPool(t, s.pool))
	healthy := admingate.NewPoolStore(s.pool)

	t.Run("organisation count read failing refuses the sole owner", func(t *testing.T) {
		before, _ := s.smtpRow(t)
		ee := s.engine(t, splitGateStore{superadmin: healthy, owner: failing})
		ctx := s.session(t, owner.ID, tenant)
		requireAllRefused(t, ee, ctx)
		if after, _ := s.smtpRow(t); after != before {
			t.Errorf("smtp_settings changed under refused requests")
		}
		requireCapability(t, ee, ctx, false)
	})

	t.Run("superadmin read failing refuses and does not fall through to the owner arm", func(t *testing.T) {
		before, _ := s.smtpRow(t)
		ee := s.engine(t, splitGateStore{superadmin: failing, owner: healthy})
		ctx := s.session(t, owner.ID, tenant)
		requireAllRefused(t, ee, ctx)
		if after, _ := s.smtpRow(t); after != before {
			t.Errorf("smtp_settings changed under refused requests")
		}
		requireCapability(t, ee, ctx, false)
	})

	// Runs last: it creates the second organisation.
	t.Run("a second organisation refuses the same owner on the next request", func(t *testing.T) {
		seedTenant(t, s.pool, "smtp-ia-second-"+sfx)
		if n := s.liveTenantCount(t, tenant); n != 2 {
			t.Fatalf("PREMISE FAILED: %d live organisations after seeding a second, want 2", n)
		}
		before, _ := s.smtpRow(t)
		ctx := s.session(t, owner.ID, tenant)
		requireAllRefused(t, e, ctx)
		if after, _ := s.smtpRow(t); after != before {
			t.Errorf("smtp_settings changed under refused requests")
		}
		requireCapability(t, e, ctx, false)
	})
}

// splitGateStore answers each admingate.Store read from a different store, so
// one read can fail while the other is healthy.
type splitGateStore struct {
	superadmin admingate.Store
	owner      admingate.Store
}

func (s splitGateStore) IsSuperadmin(ctx context.Context, id uuid.UUID) (bool, error) {
	return s.superadmin.IsSuperadmin(ctx, id)
}

func (s splitGateStore) IsSoleLiveTenantOwner(ctx context.Context, id uuid.UUID) (bool, error) {
	return s.owner.IsSoleLiveTenantOwner(ctx, id)
}

// closedAppPool opens a second wpmgr_app pool on the same container and closes
// it, so every query on it fails. It is used only as a failing gate store; it
// never reads or writes data under test.
func closedAppPool(t *testing.T, app *db.Pool) *db.Pool {
	t.Helper()
	adminDSNsMu.Lock()
	dsn, ok := adminDSNs[app]
	adminDSNsMu.Unlock()
	if !ok {
		t.Fatal("SETUP FAILURE (test helper misuse, not the test's own assertion): no DSN for this pool")
	}
	p, err := db.Connect(context.Background(), strings.Replace(dsn, "wpmgr:wpmgr@", "wpmgr_app:app@", 1))
	if err != nil {
		setupFatalf(t, err, "postgres: connect the pool to be closed")
	}
	p.Close()
	return p
}
