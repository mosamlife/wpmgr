package tests

// members_role_gh406_integration_test.go — GH #406: "an admin in an
// organization can remove the owner or change their role".
//
// A previous session closed this as frontend-only, on the belief that the
// backend already answered 403. It does not. The 403 that belief rested on was
// the LAST-OWNER COUNT guard firing in a single-owner test org, not an
// authorization check: members_handler.patchRole compared the actor only to
// the role being GRANTED, and removeMember never read the actor's role at all.
// In an org holding TWO owners the refusal evaporated — an admin got 200 on
// demote-an-owner and 204 on remove-an-owner.
//
// THE PREMISE THIS FILE RESTS ON IS ASSERTED, NOT ASSUMED:
// gh406RequireTwoOwners queries the owner count through the production path
// before any admin acts. Without it these tests would pass for exactly the
// reason the previous session's belief did, and would reproduce the defect
// they exist to catch.
//
// Everything under test is reached through the REAL gin engine, the REAL
// middleware.Authenticate -> authz.RequireAuth/RequireTenant/RequirePermission
// chain, over HTTP, against the pool startPostgres returns — wpmgr_app,
// NOSUPERUSER, NOBYPASSRLS. The superuser pool is used for seeding and for
// out-of-band observation only, never to perform the action under test.
//
// NOT RUN BY CI (apps/api/tests is excluded from the fast lane by owner
// decision). Run with `make test-integration` from the repository root.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alexedwards/scs/v2"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/apikey"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/auth"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/middleware"
)

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

// gh406RequireApplicationRole fails unless the pool really is a role RLS
// applies to. Kept local (file-self-contained) rather than shared.
func gh406RequireApplicationRole(t *testing.T, pool *db.Pool) {
	t.Helper()
	var name string
	var super, bypass bool
	if err := pool.QueryRow(context.Background(),
		`SELECT current_user, rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).
		Scan(&name, &super, &bypass); err != nil {
		t.Fatalf("read the connected role: %v", err)
	}
	if super || bypass {
		t.Fatalf("connected as %q (rolsuper=%v, rolbypassrls=%v): RLS does not apply to this role, "+
			"so nothing in this file proves anything", name, super, bypass)
	}
	t.Logf("acting connection is %q (rolsuper=%v, rolbypassrls=%v)", name, super, bypass)
}

// gh406Engine wires the members + api-key routes behind the SAME middleware
// chain internal/server/server.go mounts them behind.
func gh406Engine(t *testing.T, pool *db.Pool, sessions *auth.SessionManager, authSvc *auth.Service, rec *audit.Recorder) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	keys := apikey.NewService(pool)
	authn := middleware.NewAuthenticator(sessions, authSvc, keys, pool)

	engine := gin.New()
	engine.Use(authn.Authenticate())
	v1 := engine.Group("/api/v1")
	v1.Use(authz.RequireAuth(), authz.RequireTenant())
	auth.NewMembersHandler(authSvc, nil).Register(v1)
	apikey.NewHandler(keys, rec).Register(v1)
	return engine
}

// gh406Session returns a request context carrying a real logged-in session for
// (userID, tenantID). The role is NOT supplied — middleware.Authenticate
// resolves it from the memberships table, as in production.
func gh406Session(t *testing.T, sessions *auth.SessionManager, userID, tenantID uuid.UUID) context.Context {
	t.Helper()
	ctx, err := sessions.SCS().Load(context.Background(), "")
	if err != nil {
		t.Fatalf("load session: %v", err)
	}
	if err := sessions.Login(ctx, userID, tenantID); err != nil {
		t.Fatalf("login: %v", err)
	}
	return ctx
}

func gh406Do(engine *gin.Engine, ctx context.Context, method, path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(w, req)
	return w
}

// gh406Code pulls the machine-readable error code out of the httpx envelope.
func gh406Code(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var env struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope %q: %v", w.Body.String(), err)
	}
	return env.Code
}

// gh406RoleInDB reads the stored membership role out of band (superuser pool):
// a refusal that returns 403 while still writing the row is not a refusal.
func gh406RoleInDB(t *testing.T, admin *db.Pool, tenant, user uuid.UUID) string {
	t.Helper()
	var role string
	err := admin.QueryRow(context.Background(),
		`SELECT role FROM memberships WHERE tenant_id = $1 AND user_id = $2`, tenant, user).Scan(&role)
	if err != nil {
		t.Fatalf("read membership role: %v", err)
	}
	return role
}

func gh406MembershipExists(t *testing.T, admin *db.Pool, tenant, user uuid.UUID) bool {
	t.Helper()
	var n int
	if err := admin.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM memberships WHERE tenant_id = $1 AND user_id = $2`, tenant, user).Scan(&n); err != nil {
		t.Fatalf("count membership: %v", err)
	}
	return n > 0
}

// gh406RequireTwoOwners asserts the premise the whole file rests on, through
// the production path (auth.Service.CountOwners -> InTenantTx as wpmgr_app).
// With one owner the last-owner guard answers 403 for reasons that have
// nothing to do with authorization, and every assertion below goes vacuous.
func gh406RequireTwoOwners(t *testing.T, authSvc *auth.Service, tenant uuid.UUID) {
	t.Helper()
	n, err := authSvc.CountOwners(context.Background(), tenant)
	if err != nil {
		t.Fatalf("count owners: %v", err)
	}
	if n != 2 {
		t.Fatalf("PREMISE FAILED: org has %d owners, want exactly 2. With one owner the "+
			"last-owner COUNT guard supplies the 403 and this file proves nothing "+
			"about authorization — that is precisely how GH #406 was mis-closed", n)
	}
	t.Logf("premise asserted: org holds %d owners", n)
}

// ---------------------------------------------------------------------------
// The cardinal test
// ---------------------------------------------------------------------------

func TestGH406_AdminCannotActOnAnOwner(t *testing.T) {
	pool := startPostgres(t) // wpmgr_app: NOSUPERUSER, NOBYPASSRLS
	adminDB := connectAdmin(t, pool)
	defer adminDB.Close()
	gh406RequireApplicationRole(t, pool)

	rec := audit.NewRecorder(pool, domain.SystemClock{})
	authRepo := auth.NewRepo(pool)
	authSvc := auth.NewService(authRepo, rec, domain.NewValidator())
	sessions := auth.NewSessionManagerWithStore(scs.New(), false)
	engine := gh406Engine(t, pool, sessions, authSvc, rec)

	tenant := seedTenant(t, pool, "gh406-"+uuid.NewString()[:8])
	sfx := uuid.NewString()[:8]
	ownerA := seedUserMembership(t, authRepo, "gh406-ownerA-"+sfx+"@example.com", tenant, authz.RoleOwner)
	ownerB := seedUserMembership(t, authRepo, "gh406-ownerB-"+sfx+"@example.com", tenant, authz.RoleOwner)
	adminU := seedUserMembership(t, authRepo, "gh406-admin-"+sfx+"@example.com", tenant, authz.RoleAdmin)
	adminB := seedUserMembership(t, authRepo, "gh406-adminB-"+sfx+"@example.com", tenant, authz.RoleAdmin)
	viewerU := seedUserMembership(t, authRepo, "gh406-viewer-"+sfx+"@example.com", tenant, authz.RoleViewer)
	promoteU := seedUserMembership(t, authRepo, "gh406-promote-"+sfx+"@example.com", tenant, authz.RoleViewer)

	// THE PREMISE. Two owners: the last-owner guard cannot fire, so any 403
	// below is an authorization refusal and nothing else.
	gh406RequireTwoOwners(t, authSvc, tenant)

	adminCtx := gh406Session(t, sessions, adminU.ID, tenant)
	ownerCtx := gh406Session(t, sessions, ownerA.ID, tenant)

	// Sanity: the session really resolves to the role we think it does, via the
	// real middleware. If adminU authenticated as anything else the refusals
	// below would prove the wrong thing.
	t.Run("premise_admin_session_resolves_to_admin", func(t *testing.T) {
		w := gh406Do(engine, adminCtx, http.MethodGet, "/api/v1/members", "")
		if w.Code != http.StatusOK {
			t.Fatalf("admin GET /members = %d, want 200 (admin holds member:read); body = %s", w.Code, w.Body.String())
		}
		if got := gh406RoleInDB(t, adminDB, tenant, adminU.ID); got != string(authz.RoleAdmin) {
			t.Fatalf("acting user's stored role = %q, want admin", got)
		}
	})

	// ---- #406 proper -------------------------------------------------------

	t.Run("admin_demotes_owner_403", func(t *testing.T) {
		w := gh406Do(engine, adminCtx, http.MethodPatch,
			"/api/v1/members/"+ownerB.ID.String(), `{"role":"admin"}`)
		if w.Code != http.StatusForbidden {
			t.Fatalf("admin demoting an owner = %d, want 403 (GH #406); body = %s", w.Code, w.Body.String())
		}
		if code := gh406Code(t, w); code != "target_role_exceeds_actor" {
			t.Fatalf("refusal code = %q, want target_role_exceeds_actor (a last_owner here would mean "+
				"the COUNT guard answered, not authorization)", code)
		}
		if got := gh406RoleInDB(t, adminDB, tenant, ownerB.ID); got != string(authz.RoleOwner) {
			t.Fatalf("target's stored role = %q after a refused demote, want owner", got)
		}
	})

	t.Run("admin_removes_owner_403", func(t *testing.T) {
		w := gh406Do(engine, adminCtx, http.MethodDelete,
			"/api/v1/members/"+ownerB.ID.String(), "")
		if w.Code != http.StatusForbidden {
			t.Fatalf("admin removing an owner = %d, want 403 (GH #406); body = %s", w.Code, w.Body.String())
		}
		if code := gh406Code(t, w); code != "target_role_exceeds_actor" {
			t.Fatalf("refusal code = %q, want target_role_exceeds_actor", code)
		}
		if !gh406MembershipExists(t, adminDB, tenant, ownerB.ID) {
			t.Fatal("the owner's membership row is gone after a refused remove")
		}
	})

	t.Run("admin_escalates_self_403", func(t *testing.T) {
		// Already refused today by the grant ceiling. Pinned so it cannot regress.
		w := gh406Do(engine, adminCtx, http.MethodPatch,
			"/api/v1/members/"+adminU.ID.String(), `{"role":"owner"}`)
		if w.Code != http.StatusForbidden {
			t.Fatalf("admin escalating self to owner = %d, want 403; body = %s", w.Code, w.Body.String())
		}
		if code := gh406Code(t, w); code != "role_grant_exceeds_actor" {
			t.Fatalf("refusal code = %q, want role_grant_exceeds_actor", code)
		}
		if got := gh406RoleInDB(t, adminDB, tenant, adminU.ID); got != string(authz.RoleAdmin) {
			t.Fatalf("self role = %q after a refused escalation, want admin", got)
		}
	})

	t.Run("admin_mints_owner_api_key_403", func(t *testing.T) {
		// The second, independent escalation: role came off the request body
		// with no ceiling, so this alone was #406 end to end without ever
		// touching the members endpoint.
		w := gh406Do(engine, adminCtx, http.MethodPost, "/api/v1/api-keys",
			`{"name":"gh406-escalation","role":"owner"}`)
		if w.Code != http.StatusForbidden {
			t.Fatalf("admin minting an owner-role API key = %d, want 403; body = %s", w.Code, w.Body.String())
		}
		if code := gh406Code(t, w); code != "apikey_role_exceeds_actor" {
			t.Fatalf("refusal code = %q, want apikey_role_exceeds_actor", code)
		}
		var n int
		if err := adminDB.QueryRow(context.Background(),
			`SELECT COUNT(*) FROM api_keys WHERE tenant_id = $1 AND role = 'owner'`, tenant).Scan(&n); err != nil {
			t.Fatalf("count owner keys: %v", err)
		}
		if n != 0 {
			t.Fatalf("%d owner-role API key(s) exist after a refused mint, want 0", n)
		}
	})

	// ---- the guard must not over-fire --------------------------------------

	t.Run("admin_mints_operator_api_key_201", func(t *testing.T) {
		w := gh406Do(engine, adminCtx, http.MethodPost, "/api/v1/api-keys",
			`{"name":"gh406-ordinary","role":"operator"}`)
		if w.Code != http.StatusCreated {
			t.Fatalf("admin minting an operator key = %d, want 201; body = %s", w.Code, w.Body.String())
		}
	})

	t.Run("admin_demotes_admin_allowed", func(t *testing.T) {
		w := gh406Do(engine, adminCtx, http.MethodPatch,
			"/api/v1/members/"+adminB.ID.String(), `{"role":"operator"}`)
		if w.Code != http.StatusOK {
			t.Fatalf("admin acting on a peer admin = %d, want 200 (out of scope, must not tighten); body = %s",
				w.Code, w.Body.String())
		}
		if got := gh406RoleInDB(t, adminDB, tenant, adminB.ID); got != string(authz.RoleOperator) {
			t.Fatalf("peer admin's stored role = %q, want operator", got)
		}
	})

	t.Run("admin_removes_viewer_allowed", func(t *testing.T) {
		w := gh406Do(engine, adminCtx, http.MethodDelete,
			"/api/v1/members/"+viewerU.ID.String(), "")
		if w.Code != http.StatusNoContent {
			t.Fatalf("admin removing a viewer = %d, want 204; body = %s", w.Code, w.Body.String())
		}
		if gh406MembershipExists(t, adminDB, tenant, viewerU.ID) {
			t.Fatal("viewer's membership row survived an allowed remove")
		}
	})

	t.Run("owner_grants_owner_allowed", func(t *testing.T) {
		// Ownership transfer is a supported capability and the issue explicitly
		// asks to preserve it (see security_m1_test.go's owner-grants-owner).
		w := gh406Do(engine, ownerCtx, http.MethodPatch,
			"/api/v1/members/"+promoteU.ID.String(), `{"role":"owner"}`)
		if w.Code != http.StatusOK {
			t.Fatalf("owner granting owner = %d, want 200; body = %s", w.Code, w.Body.String())
		}
		if got := gh406RoleInDB(t, adminDB, tenant, promoteU.ID); got != string(authz.RoleOwner) {
			t.Fatalf("promoted user's stored role = %q, want owner", got)
		}
	})

	t.Run("owner_demotes_other_owner_allowed", func(t *testing.T) {
		n, err := authSvc.CountOwners(context.Background(), tenant)
		if err != nil {
			t.Fatalf("count owners: %v", err)
		}
		if n < 2 {
			t.Fatalf("PREMISE FAILED: %d owner(s) before an owner-on-owner demote; the last-owner "+
				"guard would supply the answer instead of the role comparison", n)
		}
		w := gh406Do(engine, ownerCtx, http.MethodPatch,
			"/api/v1/members/"+ownerB.ID.String(), `{"role":"admin"}`)
		if w.Code != http.StatusOK {
			t.Fatalf("owner demoting another owner = %d, want 200 (owner outranks nobody but ties, "+
				"and AtLeast is inclusive); body = %s", w.Code, w.Body.String())
		}
		if got := gh406RoleInDB(t, adminDB, tenant, ownerB.ID); got != string(authz.RoleAdmin) {
			t.Fatalf("demoted owner's stored role = %q, want admin", got)
		}
	})
}

// TestGH406_LastOwnerGuardStillFires keeps the pre-existing single-owner
// behaviour honest: in a one-owner org the refusal is last_owner, and it is
// NOT the answer the new target ceiling gives. Separate tenant so it cannot
// perturb the two-owner premise above.
func TestGH406_LastOwnerGuardStillFires(t *testing.T) {
	pool := startPostgres(t)
	adminDB := connectAdmin(t, pool)
	defer adminDB.Close()
	gh406RequireApplicationRole(t, pool)

	rec := audit.NewRecorder(pool, domain.SystemClock{})
	authRepo := auth.NewRepo(pool)
	authSvc := auth.NewService(authRepo, rec, domain.NewValidator())
	sessions := auth.NewSessionManagerWithStore(scs.New(), false)
	engine := gh406Engine(t, pool, sessions, authSvc, rec)

	tenant := seedTenant(t, pool, "gh406-solo-"+uuid.NewString()[:8])
	sfx := uuid.NewString()[:8]
	solo := seedUserMembership(t, authRepo, "gh406-solo-"+sfx+"@example.com", tenant, authz.RoleOwner)

	n, err := authSvc.CountOwners(context.Background(), tenant)
	if err != nil {
		t.Fatalf("count owners: %v", err)
	}
	if n != 1 {
		t.Fatalf("PREMISE FAILED: org has %d owners, want exactly 1", n)
	}

	soloCtx := gh406Session(t, sessions, solo.ID, tenant)
	w := gh406Do(engine, soloCtx, http.MethodPatch,
		"/api/v1/members/"+solo.ID.String(), `{"role":"admin"}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("last owner demoting themselves = %d, want 403; body = %s", w.Code, w.Body.String())
	}
	if code := gh406Code(t, w); code != "last_owner" {
		t.Fatalf("refusal code = %q, want last_owner (the new target ceiling must not swallow "+
			"this case: an owner does outrank an owner)", code)
	}
	if got := gh406RoleInDB(t, adminDB, tenant, solo.ID); got != string(authz.RoleOwner) {
		t.Fatalf("last owner's stored role = %q after a refused demote, want owner", got)
	}
}
