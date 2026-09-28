package tests

// settings_smtp_instance_authority_integration_test.go: the instance SMTP
// settings routes (GET, PUT and POST /api/v1/settings/smtp[/test]) admit only
// instance-level authority, proven against real rows.
//
// The contract: the relay is one row for the whole install, so a principal
// qualifies only as a superadmin, or as the owner of the only live organisation
// on the install. A role inside one organisation of several does not qualify.
//
// Everything under test is reached through the REAL gin engine and the REAL
// middleware.Authenticate -> authz.RequireAuth/RequireTenant chain that
// internal/server/server.go mounts the settings routes behind, then the real
// settings.Handler.Register (RequireOrgScope + the instance-authority gate),
// over the pool startPostgres returns: wpmgr_app, NOSUPERUSER, NOBYPASSRLS. The
// gate store is the production admingate.PoolStore over that same pool.
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
	"github.com/mosamlife/wpmgr/apps/api/internal/cryptbox"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/middleware"
	"github.com/mosamlife/wpmgr/apps/api/internal/settings"
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

// engine mounts the settings routes behind the same chain server.go uses for
// the v1 group, with the given gate store.
func (s *smtpIAStack) engine(t *testing.T, gate admingate.Store) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	keys := apikey.NewService(s.pool)
	authn := middleware.NewAuthenticator(s.sessions, s.authSvc, keys, s.pool)
	e := gin.New()
	e.Use(authn.Authenticate())
	v1 := e.Group("/api/v1")
	v1.Use(authz.RequireAuth(), authz.RequireTenant())
	settings.NewHandler(s.svc, s.rec, gate).Register(v1)
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
	superU := seedUserMembership(t, s.authRepo, "smtp-ia-super-"+sfx+"@example.com", tenantB, authz.RoleViewer)
	s.makeSuperadmin(t, superU.ID)
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
		requireAllRefused(t, e, s.session(t, ownerA.ID, tenantA))
		after, _ := s.smtpRow(t)
		if after != before {
			t.Errorf("smtp_settings changed under refused requests:\nbefore %s\nafter  %s", before, after)
		}
	})

	t.Run("superadmin is admitted on GET, PUT and POST /test", func(t *testing.T) {
		requireAllAdmitted(t, s, e, s.session(t, superU.ID, tenantB))
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
		requireAllAdmitted(t, s, e, s.session(t, owner.ID, tenant))
	})

	t.Run("admin of the sole organisation is refused", func(t *testing.T) {
		before, _ := s.smtpRow(t)
		requireAllRefused(t, e, s.session(t, adminU.ID, tenant))
		if after, _ := s.smtpRow(t); after != before {
			t.Errorf("smtp_settings changed under refused requests:\nbefore %s\nafter  %s", before, after)
		}
	})

	// Fail closed. failing is the production PoolStore over a pool that has
	// been closed, so each read returns a real error from the real store.
	failing := admingate.NewPoolStore(closedAppPool(t, s.pool))
	healthy := admingate.NewPoolStore(s.pool)

	t.Run("organisation count read failing refuses the sole owner", func(t *testing.T) {
		before, _ := s.smtpRow(t)
		ee := s.engine(t, splitGateStore{superadmin: healthy, owner: failing})
		requireAllRefused(t, ee, s.session(t, owner.ID, tenant))
		if after, _ := s.smtpRow(t); after != before {
			t.Errorf("smtp_settings changed under refused requests")
		}
	})

	t.Run("superadmin read failing refuses and does not fall through to the owner arm", func(t *testing.T) {
		before, _ := s.smtpRow(t)
		ee := s.engine(t, splitGateStore{superadmin: failing, owner: healthy})
		requireAllRefused(t, ee, s.session(t, owner.ID, tenant))
		if after, _ := s.smtpRow(t); after != before {
			t.Errorf("smtp_settings changed under refused requests")
		}
	})

	// Runs last: it creates the second organisation.
	t.Run("a second organisation refuses the same owner on the next request", func(t *testing.T) {
		seedTenant(t, s.pool, "smtp-ia-second-"+sfx)
		if n := s.liveTenantCount(t, tenant); n != 2 {
			t.Fatalf("PREMISE FAILED: %d live organisations after seeding a second, want 2", n)
		}
		before, _ := s.smtpRow(t)
		requireAllRefused(t, e, s.session(t, owner.ID, tenant))
		if after, _ := s.smtpRow(t); after != before {
			t.Errorf("smtp_settings changed under refused requests")
		}
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
