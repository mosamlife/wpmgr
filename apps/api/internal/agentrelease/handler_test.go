package agentrelease_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentrelease"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

func init() { gin.SetMode(gin.TestMode) }

// fakeSiteLister is a Service.SiteLister test double.
type fakeSiteLister struct {
	byTenant map[uuid.UUID][]agentrelease.SiteAgentVersion
}

func (f *fakeSiteLister) ListSiteAgentVersions(_ context.Context, tenantID uuid.UUID) ([]agentrelease.SiteAgentVersion, error) {
	return f.byTenant[tenantID], nil
}

// fakeVersionReader is a Service.VersionReader test double.
type fakeVersionReader struct{ version string }

func (f *fakeVersionReader) LatestVersion(context.Context) string { return f.version }

func newTestEngine(h *agentrelease.Handler) *gin.Engine {
	engine := gin.New()
	engine.Use(gin.Recovery())
	v1 := engine.Group("/api/v1")
	v1.Use(authz.RequireAuth(), authz.RequireTenant())
	h.Register(v1)
	return engine
}

func doRequest(engine *gin.Engine, method, path string, p domain.Principal) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	req = req.WithContext(domain.WithPrincipal(req.Context(), p))
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	return w
}

// TestFleetAgents_RequiresOrgScope mirrors vuln's fleet-rollup precedent
// (TestFleetSummaryRequiresOrgScope): a site-scoped collaborator is blocked
// with 403 before reaching the handler; an org-scoped member reaches it.
func TestFleetAgents_RequiresOrgScope(t *testing.T) {
	tenantID := uuid.New()
	h := agentrelease.NewHandler(nil, false)
	engine := newTestEngine(h)

	sitePrincipal := domain.Principal{
		TenantID:       tenantID,
		Type:           domain.PrincipalUser,
		UserID:         uuid.New(),
		Scope:          domain.ScopeSite,
		AllowedSiteIDs: []uuid.UUID{uuid.New()},
		Role:           string(authz.RoleViewer),
	}
	orgPrincipal := domain.Principal{
		TenantID: tenantID,
		Type:     domain.PrincipalUser,
		UserID:   uuid.New(),
		Scope:    "",
		Role:     string(authz.RoleViewer),
	}

	if w := doRequest(engine, http.MethodGet, "/api/v1/fleet/agents", sitePrincipal); w.Code != http.StatusForbidden {
		t.Errorf("site-scoped GET /fleet/agents = %d; want 403", w.Code)
	}
	// nil service (Handler.svc == nil) degrades to 503, but crucially NOT 403;
	// proving the org-scoped request passed RequireOrgScope and reached the handler.
	if w := doRequest(engine, http.MethodGet, "/api/v1/fleet/agents", orgPrincipal); w.Code == http.StatusForbidden {
		t.Errorf("org-scoped GET /fleet/agents = %d; must not be blocked by RequireOrgScope", w.Code)
	}
}

// TestCrossTenantIsolation_FleetAgents proves that a tenant's fleet rollup
// never includes another tenant's sites, at the handler+service boundary:
// two tenants share one fake repo keyed by tenant, and each principal's
// request must see only its own tenant's rows. Repo-level RLS isolation
// (the DB layer under this) is covered separately.
func TestCrossTenantIsolation_FleetAgents(t *testing.T) {
	tenantA := uuid.New()
	tenantB := uuid.New()
	siteA := uuid.New()
	siteB := uuid.New()

	repo := &fakeSiteLister{byTenant: map[uuid.UUID][]agentrelease.SiteAgentVersion{
		tenantA: {{SiteID: siteA, SiteName: "tenant-a-site", AgentVersion: "0.61.90"}},
		tenantB: {{SiteID: siteB, SiteName: "tenant-b-site", AgentVersion: "0.61.95"}},
	}}
	svc := agentrelease.NewService(repo, &fakeVersionReader{version: "0.61.95"})
	h := agentrelease.NewHandler(svc, false)
	engine := newTestEngine(h)

	principalFor := func(tenantID uuid.UUID) domain.Principal {
		return domain.Principal{
			TenantID: tenantID,
			Type:     domain.PrincipalUser,
			UserID:   uuid.New(),
			Scope:    "",
			Role:     string(authz.RoleViewer),
		}
	}

	type fleetResponse struct {
		Sites []struct {
			SiteID   string `json:"site_id"`
			SiteName string `json:"site_name"`
		} `json:"sites"`
	}

	w := doRequest(engine, http.MethodGet, "/api/v1/fleet/agents", principalFor(tenantA))
	if w.Code != http.StatusOK {
		t.Fatalf("tenant A request = %d; want 200, body=%s", w.Code, w.Body.String())
	}
	var respA fleetResponse
	if err := json.Unmarshal(w.Body.Bytes(), &respA); err != nil {
		t.Fatalf("decode tenant A response: %v", err)
	}
	if len(respA.Sites) != 1 || respA.Sites[0].SiteID != siteA.String() {
		t.Fatalf("tenant A must see exactly its own site (%s), got %+v", siteA, respA.Sites)
	}
	for _, s := range respA.Sites {
		if s.SiteID == siteB.String() {
			t.Fatalf("tenant A response leaked tenant B's site %s", siteB)
		}
	}

	w = doRequest(engine, http.MethodGet, "/api/v1/fleet/agents", principalFor(tenantB))
	if w.Code != http.StatusOK {
		t.Fatalf("tenant B request = %d; want 200, body=%s", w.Code, w.Body.String())
	}
	var respB fleetResponse
	if err := json.Unmarshal(w.Body.Bytes(), &respB); err != nil {
		t.Fatalf("decode tenant B response: %v", err)
	}
	if len(respB.Sites) != 1 || respB.Sites[0].SiteID != siteB.String() {
		t.Fatalf("tenant B must see exactly its own site (%s), got %+v", siteB, respB.Sites)
	}
	for _, s := range respB.Sites {
		if s.SiteID == siteA.String() {
			t.Fatalf("tenant B response leaked tenant A's site %s", siteA)
		}
	}
}

// TestFleetAgents_EmitsSelfUpdateEnabled pins the Phase 2 feature flag on the
// wire. The field is specified and the UI gates the "Update WPMgr agent" bulk
// action on it, so until the control plane actually emits it the action stays
// hidden even on an instance where WPMGR_UPDATE_AGENT_SELF_UPDATE_ENABLED is
// on, i.e. the channel could not be turned on at all. It must be present in
// BOTH states and track the same config the update worker gates dispatch on.
func TestFleetAgents_EmitsSelfUpdateEnabled(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "enabled"}[enabled], func(t *testing.T) {
			tenantID := uuid.New()
			repo := &fakeSiteLister{byTenant: map[uuid.UUID][]agentrelease.SiteAgentVersion{
				tenantID: {{SiteID: uuid.New(), SiteName: "s", AgentVersion: "0.61.90"}},
			}}
			svc := agentrelease.NewService(repo, &fakeVersionReader{version: "0.61.95"})
			engine := newTestEngine(agentrelease.NewHandler(svc, enabled))
			p := domain.Principal{TenantID: tenantID, Type: domain.PrincipalUser, UserID: uuid.New(), Role: string(authz.RoleViewer)}

			w := doRequest(engine, http.MethodGet, "/api/v1/fleet/agents", p)
			if w.Code != http.StatusOK {
				t.Fatalf("GET /fleet/agents = %d; want 200, body=%s", w.Code, w.Body.String())
			}
			// Decoded into a pointer so "absent" is distinguishable from
			// "false": absent is exactly the bug this test exists to catch.
			var resp struct {
				SelfUpdateEnabled *bool `json:"self_update_enabled"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if resp.SelfUpdateEnabled == nil {
				t.Fatalf("self_update_enabled absent from the response; the UI cannot reveal the action without it. body=%s", w.Body.String())
			}
			if *resp.SelfUpdateEnabled != enabled {
				t.Errorf("self_update_enabled = %v; want %v (must track the config the worker checks)", *resp.SelfUpdateEnabled, enabled)
			}
		})
	}
}

// TestAgentLatest_DegradesToUnknown proves GET /agent/latest never errors:
// a nil service (object storage unconfigured / never wired) still returns
// 200 with version="unknown".
func TestAgentLatest_DegradesToUnknown(t *testing.T) {
	h := agentrelease.NewHandler(nil, false)
	engine := newTestEngine(h)
	p := domain.Principal{TenantID: uuid.New(), Type: domain.PrincipalUser, UserID: uuid.New(), Role: string(authz.RoleViewer)}

	w := doRequest(engine, http.MethodGet, "/api/v1/agent/latest", p)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /agent/latest with nil service = %d; want 200, body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Version != "unknown" {
		t.Errorf("version = %q; want %q", resp.Version, "unknown")
	}
}
