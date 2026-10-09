package aireadiness

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// These tests drive the route gates through the real Handler.Register, on a
// group named /api/v1 the way server.go mounts it. A leading middleware puts
// the principal on the context in place of RequireAuth and RequireTenant: the
// thing under test is what Register puts in front of each handler. The service
// tests call Service directly, so a middleware missing from Register is
// invisible to them.
//
// Service.Get and Service.RequestRefresh repeat the site check, so a
// collaborator refused by RequireSiteAccess and one refused by the service
// both answer 404 site_not_found for a well-formed id outside the allowlist.
// That request cannot tell the two layers apart. A malformed id can: the gate
// answers it with 404 site_not_found, the handler with 422 invalid_site_id,
// and an org member's request (which the gate waves through) pins the
// handler's answer.

const notAUUID = "not-a-uuid"

// routeRig is a refreshRig whose site would answer every route successfully,
// plus a second site of the same tenant that a collaborator is not given.
type routeRig struct {
	*refreshRig
	other uuid.UUID
}

func newRouteRig(t *testing.T) *routeRig {
	t.Helper()
	rig := newRefreshRig(t)
	other := rig.repo.add(rig.tenant, readyFacts())
	otherTarget := rig.target
	otherTarget.SiteID = other
	rig.repo.targets[other] = otherTarget
	rig.build()
	return &routeRig{refreshRig: rig, other: other}
}

func (r *routeRig) principal(role authz.Role, sites ...uuid.UUID) domain.Principal {
	p := domain.Principal{Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: r.tenant, Role: string(role), Scope: domain.ScopeOrg}
	if len(sites) > 0 {
		p.Scope, p.AllowedSiteIDs = domain.ScopeSite, sites
	}
	return p
}

// serve sends one request through a fresh engine carrying only the readiness
// routes. A POST carries an empty JSON object with the JSON media type, so
// RequireJSONBody never decides a test.
func (r *routeRig) serve(p domain.Principal, method, path string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	e := gin.New()
	e.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(domain.WithPrincipal(c.Request.Context(), p))
		c.Next()
	})
	NewHandler(r.svc).Register(e.Group("/api/v1"))

	var req *http.Request
	if method == http.MethodPost {
		req = httptest.NewRequest(method, path, strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

func readinessPath(site string) string { return "/api/v1/sites/" + site + "/ai/readiness" }
func refreshPath(site string) string   { return "/api/v1/sites/" + site + "/ai/readiness/refresh" }

const fleetPath = "/api/v1/fleet/ai-readiness"

func wantStatusCode(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d %s (body %s)", rec.Code, status, code, rec.Body.String())
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not the error envelope: %v (%s)", err, rec.Body.String())
	}
	if body.Code != code {
		t.Fatalf("code = %q, want %q (body %s)", body.Code, code, rec.Body.String())
	}
}

func wantSiteID(t *testing.T, rec *httptest.ResponseRecorder, site uuid.UUID) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var body struct {
		SiteID uuid.UUID `json:"site_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.SiteID != site {
		t.Fatalf("site_id = %s (%v), want %s (body %s)", body.SiteID, err, site, rec.Body.String())
	}
}

// nothingRan asserts that no repo read, enqueue or audit write happened.
func (r *routeRig) nothingRan(t *testing.T) {
	t.Helper()
	if len(r.repo.loads) != 0 || len(r.repo.targetReads) != 0 {
		t.Fatalf("the repo was read: loads %+v target reads %+v", r.repo.loads, r.repo.targetReads)
	}
	r.nothingQueued(t)
}

func TestReadinessSiteRoutesRequireSiteAccess(t *testing.T) {
	t.Run("a collaborator reaches the site it was given", func(t *testing.T) {
		// Also what catches a gate reading the wrong path parameter: it would
		// parse an empty id and refuse this collaborator too.
		rig := newRouteRig(t)
		collab := rig.principal(authz.RoleOperator, rig.site)
		wantSiteID(t, rig.serve(collab, http.MethodGet, readinessPath(rig.site.String())), rig.site)
		rec := rig.serve(collab, http.MethodPost, refreshPath(rig.site.String()))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("refresh status = %d, want 202 (body %s)", rec.Code, rec.Body.String())
		}
		if len(rig.meta.calls) != 1 || rig.meta.calls[0].site != rig.site {
			t.Fatalf("metadata refresh = %+v, want one for %s", rig.meta.calls, rig.site)
		}
	})

	t.Run("the handler itself answers a malformed id with 422", func(t *testing.T) {
		// The premise of the next subtest: if the handler ever answered a
		// malformed id with 404 too, the gate's 404 would prove nothing.
		rig := newRouteRig(t)
		member := rig.principal(authz.RoleOperator)
		wantStatusCode(t, rig.serve(member, http.MethodGet, readinessPath(notAUUID)), http.StatusUnprocessableEntity, "invalid_site_id")
		wantStatusCode(t, rig.serve(member, http.MethodPost, refreshPath(notAUUID)), http.StatusUnprocessableEntity, "invalid_site_id")
		rig.nothingRan(t)
	})

	t.Run("the gate answers a collaborator's malformed id before the handler", func(t *testing.T) {
		// Without RequireSiteAccess the request reaches the handler and gets
		// 422 invalid_site_id, as the subtest above shows.
		rig := newRouteRig(t)
		collab := rig.principal(authz.RoleOperator, rig.site)
		wantStatusCode(t, rig.serve(collab, http.MethodGet, readinessPath(notAUUID)), http.StatusNotFound, "site_not_found")
		wantStatusCode(t, rig.serve(collab, http.MethodPost, refreshPath(notAUUID)), http.StatusNotFound, "site_not_found")
		rig.nothingRan(t)
	})

	t.Run("a site outside the allowlist is not found and nothing runs", func(t *testing.T) {
		rig := newRouteRig(t)
		collab := rig.principal(authz.RoleOperator, rig.site)
		wantStatusCode(t, rig.serve(collab, http.MethodGet, readinessPath(rig.other.String())), http.StatusNotFound, "site_not_found")
		wantStatusCode(t, rig.serve(collab, http.MethodPost, refreshPath(rig.other.String())), http.StatusNotFound, "site_not_found")
		rig.nothingRan(t)

		// Over-firing control: an org member reaches the same site.
		member := rig.principal(authz.RoleOperator)
		wantSiteID(t, rig.serve(member, http.MethodGet, readinessPath(rig.other.String())), rig.other)
	})
}

func TestReadinessRefreshRequiresSiteContentRefresh(t *testing.T) {
	t.Run("an operator queues the refresh", func(t *testing.T) {
		// The positive control: the request the viewers below send succeeds
		// for an operator, so their 403 cannot come from the handler.
		for name, p := range map[string]func(*routeRig) domain.Principal{
			"org member":   func(r *routeRig) domain.Principal { return r.principal(authz.RoleOperator) },
			"collaborator": func(r *routeRig) domain.Principal { return r.principal(authz.RoleOperator, r.site) },
		} {
			t.Run(name, func(t *testing.T) {
				rig := newRouteRig(t)
				rec := rig.serve(p(rig), http.MethodPost, refreshPath(rig.site.String()))
				if rec.Code != http.StatusAccepted {
					t.Fatalf("status = %d, want 202 (body %s)", rec.Code, rec.Body.String())
				}
				if len(rig.meta.calls) != 1 || len(rig.inv.calls) != 1 || len(rig.audit.events) != 1 {
					t.Fatalf("want one metadata refresh, one tool-list read and one audit row: meta %+v inventory %+v audit %+v",
						rig.meta.calls, rig.inv.calls, rig.audit.events)
				}
			})
		}
	})

	t.Run("a viewer is refused before anything is queued", func(t *testing.T) {
		// insufficient_permission comes only from RequirePermission; the
		// service never emits it. Without the gate, or with the route back on
		// site:read, the viewer reaches the handler and gets 202.
		for name, p := range map[string]func(*routeRig) domain.Principal{
			"org viewer":          func(r *routeRig) domain.Principal { return r.principal(authz.RoleViewer) },
			"collaborator viewer": func(r *routeRig) domain.Principal { return r.principal(authz.RoleViewer, r.site) },
		} {
			t.Run(name, func(t *testing.T) {
				rig := newRouteRig(t)
				wantStatusCode(t, rig.serve(p(rig), http.MethodPost, refreshPath(rig.site.String())),
					http.StatusForbidden, "insufficient_permission")
				rig.nothingRan(t)
			})
		}
	})
}

func TestReadinessReadsStayOnSiteRead(t *testing.T) {
	t.Run("a viewer reads the site and the fleet", func(t *testing.T) {
		rig := newRouteRig(t)
		for _, p := range []domain.Principal{rig.principal(authz.RoleViewer), rig.principal(authz.RoleViewer, rig.site)} {
			wantSiteID(t, rig.serve(p, http.MethodGet, readinessPath(rig.site.String())), rig.site)
			if rec := rig.serve(p, http.MethodGet, fleetPath); rec.Code != http.StatusOK {
				t.Fatalf("fleet status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
			}
		}
	})

	t.Run("a role with no site:read is refused", func(t *testing.T) {
		rig := newRouteRig(t)
		client := rig.principal(authz.RoleClient)
		wantStatusCode(t, rig.serve(client, http.MethodGet, readinessPath(rig.site.String())), http.StatusForbidden, "insufficient_permission")
		wantStatusCode(t, rig.serve(client, http.MethodGet, fleetPath), http.StatusForbidden, "insufficient_permission")
		rig.nothingRan(t)
	})
}
