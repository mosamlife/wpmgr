package tests

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/billing"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// TestBillingRoutes_404WhenUnhosted proves the routes-contract guarantee:
// when billing.Handler is never mounted (mirrors cmd/wpmgr/main.go's
// cfg.Hosted.Enabled==false wiring, which leaves deps.BillingH nil), every
// billing path simply 404s — there is no special-cased "hosted disabled"
// response, just an absent route.
func TestBillingRoutes_404WhenUnhosted(t *testing.T) {
	engine := gin.New()
	engine.Group("/api/v1") // no billing routes registered on it

	for _, req := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/billing"},
		{http.MethodPost, "/api/v1/billing/checkout"},
		{http.MethodPost, "/api/v1/billing/checkout/verify"},
		{http.MethodPost, "/api/v1/billing/portal"},
		{http.MethodPost, "/api/v1/billing/cancel"},
	} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(req.method, req.path, nil)
		engine.ServeHTTP(w, r)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404 when billing routes are not mounted", req.method, req.path, w.Code)
		}
	}
}

// TestBillingRoutes_200WhenHosted proves the other half of the contract: once
// billing.Handler IS mounted (hosted enabled) an authenticated owner
// principal reaches GET /api/v1/billing successfully.
func TestBillingRoutes_200WhenHosted(t *testing.T) {
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "billing-routes-hosted")

	fp := newFakeProvider("fake")
	svc := newTestBillingService(pool, fp)
	h := billing.NewHandler(svc, nil, "https://cp.example.com")

	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		p := domain.Principal{
			Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: tenant,
			Role: "owner", Scope: domain.ScopeOrg,
		}
		c.Request = c.Request.WithContext(domain.WithPrincipal(c.Request.Context(), p))
		c.Next()
	})
	v1 := engine.Group("/api/v1")
	h.Register(v1)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/billing", nil)
	engine.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/billing = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// TestBillingRoutes_NonOwnerForbidden proves PermBillingManage's owner-only
// gate (mirrors PermAuditManage): an admin (one rung below owner) is
// rejected with 403, never silently allowed through.
func TestBillingRoutes_NonOwnerForbidden(t *testing.T) {
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "billing-routes-nonowner")

	fp := newFakeProvider("fake")
	svc := newTestBillingService(pool, fp)
	h := billing.NewHandler(svc, nil, "https://cp.example.com")

	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		p := domain.Principal{
			Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: tenant,
			Role: "admin", Scope: domain.ScopeOrg,
		}
		c.Request = c.Request.WithContext(domain.WithPrincipal(c.Request.Context(), p))
		c.Next()
	})
	v1 := engine.Group("/api/v1")
	h.Register(v1)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/billing", nil)
	engine.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("GET /api/v1/billing (admin role) = %d, want 403", w.Code)
	}
}
