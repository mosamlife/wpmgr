package tenant

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// This file proves that authz.RequirePermission(authz.PermTenantManage) is
// still wired onto all three assistant kill-switch routes
// (internal/tenant/handler.go:60-65), driven through the REAL Gin router
// group the way server.go:615 mounts it (v1 := sessionAuthGroup.Group("/api/
// v1"); deps.TenantH.Register(v1)) — not through Service directly.
//
// Deliberately a SEPARATE file from assistant_test.go, and it does not extend
// that file: assistant_test.go's 14 tests all call Service methods directly
// and never build a router, which is exactly how a security review deleted
// the middleware from the resume route (m130) and the whole suite, including
// ./internal/authz/..., stayed green. Driving the service directly cannot
// catch a missing gin.HandlerFunc in Handler.Register; only building the
// group and sending an HTTP request through it can.

// killSwitchAuthzRepo is a Repo double scoped to THIS file only. Its only
// purpose is to let a request past Service.assertOwnTenant and return a
// serialisable AssistantState — it deliberately never invokes the onCommit
// callback PauseAssistant/ResumeAssistant are given, so it never reaches
// audit.Recorder.RecordInTx (which would dereference a nil pgx.Tx). That lets
// NewHandler here take a nil *audit.Recorder safely. Business correctness of
// the pause/resume/audit path is assistant_test.go's job; this file only
// proves the authz gate in front of the handler.
type killSwitchAuthzRepo struct{}

func (killSwitchAuthzRepo) Create(context.Context, CreateInput) (Tenant, error) {
	return Tenant{}, nil
}
func (killSwitchAuthzRepo) GetForUser(context.Context, uuid.UUID, uuid.UUID) (Tenant, error) {
	return Tenant{}, nil
}
func (killSwitchAuthzRepo) ListForUser(context.Context, uuid.UUID, ListInput) ([]Tenant, error) {
	return nil, nil
}
func (killSwitchAuthzRepo) GetByID(context.Context, uuid.UUID) (Tenant, error) {
	return Tenant{}, nil
}

func (killSwitchAuthzRepo) GetAssistantState(context.Context, uuid.UUID, uuid.UUID) (AssistantState, error) {
	return AssistantState{}, nil
}

func (killSwitchAuthzRepo) PauseAssistant(_ context.Context, _, _ uuid.UUID, _ *string, _ func(pgx.Tx) error) (AssistantState, error) {
	now := time.Now().UTC()
	return AssistantState{PausedAt: &now}, nil
}

func (killSwitchAuthzRepo) ResumeAssistant(_ context.Context, _, _ uuid.UUID, _ func(pgx.Tx) error) (AssistantState, error) {
	return AssistantState{}, nil
}

// buildAssistantAuthzEngine wires a Gin engine carrying ONLY the assistant
// routes, registered via the real Handler.Register (so the exact middleware
// chain handler.go declares is what runs), under /api/v1 the way server.go's
// v1 group is named. The principal is injected on the context by a leading
// middleware, mirroring RequireAuth/RequireTenant's effect without needing
// real session/tenant resolution — the thing under test is RequirePermission,
// not authentication.
func buildAssistantAuthzEngine(t *testing.T, p domain.Principal) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(domain.WithPrincipal(c.Request.Context(), p))
		c.Next()
	})
	svc := NewService(killSwitchAuthzRepo{}, domain.NewValidator(), domain.SystemClock{})
	h := NewHandler(svc, nil)
	h.Register(r.Group("/api/v1"))
	return r
}

func doAssistantAuthzReq(r *gin.Engine, method, tenantID string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	var path string
	switch method {
	case http.MethodGet:
		path = "/api/v1/tenants/" + tenantID + "/assistant"
	case "pause":
		method, path = http.MethodPost, "/api/v1/tenants/"+tenantID+"/assistant/pause"
	case "resume":
		method, path = http.MethodPost, "/api/v1/tenants/"+tenantID+"/assistant/resume"
	}
	req := httptest.NewRequest(method, path, nil)
	r.ServeHTTP(rec, req)
	return rec
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response body is not the error envelope: %v (body=%q)", err, rec.Body.String())
	}
	return body.Code
}

// TestAssistantKillSwitchRoutesRequirePermTenantManage is the regression test
// for the gap m130's security review left open: deleting
// authz.RequirePermission(authz.PermTenantManage) from any of the three
// assistant routes must turn this red. See the three sibling t.Run blocks:
// each drives a route that authz.RequirePermission gates BEFORE the handler
// runs, and distinguishes that from a handler-level failure by requiring the
// EXACT 403 codes RequirePermission itself produces
// ("insufficient_permission" from PrincipalAllows, "org_scope_required" from
// the orgLevelPerms guard, middleware.go:186-198) — codes the handler and
// Service never emit. The killSwitchAuthzRepo double never fails, so if the
// middleware is removed and the request reaches the handler instead, the
// non-owner and site-scoped cases get 200, not one of these codes, and the
// test reddens instead of passing for the wrong reason.
func TestAssistantKillSwitchRoutesRequirePermTenantManage(t *testing.T) {
	tenantID := uuid.New()

	owner := domain.Principal{
		Type:     domain.PrincipalUser,
		UserID:   uuid.New(),
		TenantID: tenantID,
		Role:     string(authz.RoleOwner),
		Scope:    domain.ScopeOrg,
	}
	// admin: the highest role short of owner. PermTenantManage's minimum role
	// is RoleOwner (authz/role.go:244), so this principal is "below the
	// required tier" but not a weak strawman — it can manage members, API
	// keys and read the audit log, and still must not reach the kill switch.
	belowTier := domain.Principal{
		Type:     domain.PrincipalUser,
		UserID:   uuid.New(),
		TenantID: tenantID,
		Role:     string(authz.RoleAdmin),
		Scope:    domain.ScopeOrg,
	}
	// Site-constrained, but role=owner: PermTenantManage is in orgLevelPerms,
	// so this must be refused REGARDLESS of role (middleware.go:186-191).
	siteConstrained := domain.Principal{
		Type:           domain.PrincipalUser,
		UserID:         uuid.New(),
		TenantID:       tenantID,
		Role:           string(authz.RoleOwner),
		Scope:          domain.ScopeSite,
		AllowedSiteIDs: []uuid.UUID{uuid.New()},
	}

	routes := []struct {
		name   string
		method string // "GET", "pause", "resume" (see doAssistantAuthzReq)
	}{
		{"GET /assistant", http.MethodGet},
		{"POST /assistant/pause", "pause"},
		{"POST /assistant/resume", "resume"},
	}

	for _, rt := range routes {
		t.Run(rt.name, func(t *testing.T) {
			t.Run("owner reaches the handler", func(t *testing.T) {
				rec := doAssistantAuthzReq(buildAssistantAuthzEngine(t, owner), rt.method, tenantID.String())
				if rec.Code != http.StatusOK {
					t.Fatalf("owner: want 200 (handler reached and succeeded), got %d body=%s", rec.Code, rec.Body.String())
				}
			})

			t.Run("below-tier org member is refused before the handler runs", func(t *testing.T) {
				rec := doAssistantAuthzReq(buildAssistantAuthzEngine(t, belowTier), rt.method, tenantID.String())
				if rec.Code != http.StatusForbidden {
					t.Fatalf("below-tier: want 403, got %d body=%s", rec.Code, rec.Body.String())
				}
				// killSwitchAuthzRepo/Service never produce this code — only
				// RequirePermission's PrincipalAllows branch does
				// (middleware.go:195-198). If the middleware were removed, this
				// principal would reach the handler and get 200 from the fake
				// repo, not this code, so the test would redden here.
				if got := errorCode(t, rec); got != "insufficient_permission" {
					t.Fatalf("below-tier: want code %q (proves RequirePermission fired, not the handler), got %q", "insufficient_permission", got)
				}
			})

			t.Run("site-constrained principal is refused regardless of role", func(t *testing.T) {
				rec := doAssistantAuthzReq(buildAssistantAuthzEngine(t, siteConstrained), rt.method, tenantID.String())
				if rec.Code != http.StatusForbidden {
					t.Fatalf("site-constrained: want 403, got %d body=%s", rec.Code, rec.Body.String())
				}
				// Only the orgLevelPerms guard inside RequirePermission emits
				// this code (middleware.go:186-191), before PrincipalAllows is
				// even consulted. Neither the handler nor Service ever produce
				// it, so seeing it here proves the request never reached
				// h.assistantState/h.pauseAssistant/h.resumeAssistant.
				if got := errorCode(t, rec); got != "org_scope_required" {
					t.Fatalf("site-constrained: want code %q (proves RequirePermission fired, not the handler), got %q", "org_scope_required", got)
				}
			})
		})
	}
}
