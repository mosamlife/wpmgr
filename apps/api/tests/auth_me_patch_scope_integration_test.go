package tests

// PATCH /auth/me must return the SAME Me resource GET /auth/me returns.
//
// Both responses are cached by apps/web under one key — useUpdateProfile does
// setQueryData(authKeys.me, <the patch response>) — so a field present on the
// GET and absent from the PATCH does not produce a smaller Me, it produces a
// WRONG one. scope and role are set in exactly one place, enrichMePortal, and
// updateProfile did not call it. A site-scoped collaborator who saved their
// display name lost me.scope, canWriteSiteContext flipped to false, and the
// governed-context editor went read-only until a hard reload.
//
// The test drives the REAL middleware.Authenticator against a REAL site_shares
// row, so scope/role are resolved the way production resolves them rather than
// injected by the test. The GET is the in-run control: it proves this principal
// genuinely is site-scoped, so a green PATCH cannot be green because the
// fixture forgot to make the user a collaborator.
//
// To watch it go red: drop the enrichMePortal call from updateProfile in
// internal/auth/handler.go. The GET assertions stay green and the PATCH
// assertions fail, which is exactly the shipped defect.

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
	"github.com/mosamlife/wpmgr/apps/api/internal/auth"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/middleware"
)

// meWire is the slice of the Me response this test cares about.
type meWire struct {
	Scope string `json:"scope"`
	Role  string `json:"role"`
	User  struct {
		ID   uuid.UUID `json:"id"`
		Name string    `json:"name"`
	} `json:"user"`
}

// seedSiteShare grants userID collaborator access to siteID at the given role.
// No membership row is created: site_shares are EXCLUSIVE in
// middleware.Authenticate, and it is the absence of a membership that sends the
// principal down the ScopeSite branch.
func seedSiteShare(t *testing.T, admin *db.Pool, tenant, site, user uuid.UUID, role string) {
	t.Helper()
	if _, err := admin.Exec(context.Background(),
		`INSERT INTO site_shares (tenant_id, site_id, user_id, role) VALUES ($1, $2, $3, $4)`,
		tenant, site, user, role); err != nil {
		t.Fatalf("seed site_share: %v", err)
	}
}

// authMeEngine mounts the auth routes behind the same Authenticator
// internal/server/server.go puts in front of them.
func authMeEngine(t *testing.T, pool *db.Pool, sessions *auth.SessionManager, authSvc *auth.Service) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	authn := middleware.NewAuthenticator(sessions, authSvc, apikey.NewService(pool), pool)

	engine := gin.New()
	engine.Use(sessions.LoadAndSave())
	engine.Use(authn.Authenticate())
	auth.NewHandler(authSvc, sessions, nil, makeCreateTenant(t, pool)).Register(engine)
	return engine
}

func authMeDo(engine *gin.Engine, ctx context.Context, method, path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(w, req)
	return w
}

func decodeMe(t *testing.T, w *httptest.ResponseRecorder, what string) meWire {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("%s: status %d, body %s", what, w.Code, w.Body.String())
	}
	var me meWire
	if err := json.Unmarshal(w.Body.Bytes(), &me); err != nil {
		t.Fatalf("%s: decode %q: %v", what, w.Body.String(), err)
	}
	return me
}

func TestAuthMe_PatchCarriesScopeAndRoleLikeGet(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()

	tenant := seedTenant(t, pool, "patchme-"+uuid.NewString()[:8])
	site := adr064SeedSite(t, admin, tenant)
	user := seedUserRow(t, admin, "collaborator-"+uuid.NewString()[:8]+"@example.com")
	seedSiteShare(t, admin, tenant, site, user, "operator")

	sessions := auth.NewSessionManagerWithStore(scs.New(), false)
	authSvc, _ := newAuthStack(pool)
	engine := authMeEngine(t, pool, sessions, authSvc)

	ctx, err := sessions.SCS().Load(context.Background(), "")
	if err != nil {
		t.Fatalf("load session: %v", err)
	}
	if err := sessions.Login(ctx, user, tenant); err != nil {
		t.Fatalf("login: %v", err)
	}

	// CONTROL. GET /auth/me already enriched before this change, so this half
	// says what the principal really is. If these fail the fixture is wrong and
	// nothing below means anything.
	got := decodeMe(t, authMeDo(engine, ctx, http.MethodGet, "/auth/me", ""), "GET /auth/me")
	if got.Scope != "site" {
		t.Fatalf("fixture is not site-scoped: GET /auth/me scope = %q, want \"site\". "+
			"The site_shares row did not take effect, so the PATCH assertions below would be vacuous", got.Scope)
	}
	if got.Role == "" {
		t.Fatal("fixture is broken: GET /auth/me carries no role")
	}

	// THE ASSERTION. Same principal, same resource, one field changed.
	patched := decodeMe(t,
		authMeDo(engine, ctx, http.MethodPatch, "/auth/me", `{"name":"Renamed Collaborator"}`),
		"PATCH /auth/me")

	if patched.User.Name != "Renamed Collaborator" {
		t.Fatalf("PATCH did not apply the name: got %q", patched.User.Name)
	}
	if patched.Scope != got.Scope {
		t.Fatalf("PATCH /auth/me dropped scope: got %q, want %q (as GET returns).\n"+
			"apps/web caches this response under authKeys.me, so this collaborator's "+
			"canWriteSiteContext flips to false and the context editor goes read-only "+
			"until a hard reload. An absent field must not read as a denied permission",
			patched.Scope, got.Scope)
	}
	if patched.Role != got.Role {
		t.Fatalf("PATCH /auth/me dropped role: got %q, want %q (as GET returns).\n"+
			"routes/login.tsx branches on role === \"client\" to pick the portal",
			patched.Role, got.Role)
	}

	// The name really changed, so this was a real write and not a no-op that
	// happened to echo the GET back.
	refetched := decodeMe(t, authMeDo(engine, ctx, http.MethodGet, "/auth/me", ""), "GET /auth/me after PATCH")
	if refetched.User.Name != "Renamed Collaborator" {
		t.Fatalf("the PATCH did not persist: refetched name %q", refetched.User.Name)
	}
	t.Logf("PATCH /auth/me returned scope=%q role=%q, matching GET", patched.Scope, patched.Role)
}
