package authz

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

func init() { gin.SetMode(gin.TestMode) }

// runWithPrincipal builds a one-route engine guarded by mw, injecting p (or no
// principal when p == nil), and returns the resulting status code.
func runWithPrincipal(p *domain.Principal, mw gin.HandlerFunc) int {
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		if p != nil {
			c.Request = c.Request.WithContext(domain.WithPrincipal(c.Request.Context(), *p))
		}
		c.Next()
	})
	engine.GET("/x", mw, func(c *gin.Context) { c.Status(http.StatusOK) })

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	engine.ServeHTTP(w, req)
	return w.Code
}

func TestRequirePermissionRBAC(t *testing.T) {
	tenant := uuid.New()
	user := uuid.New()

	tests := []struct {
		name     string
		role     Role
		perm     Permission
		wantCode int
	}{
		{"viewer denied site write", RoleViewer, PermSiteWrite, http.StatusForbidden},
		{"operator allowed site write", RoleOperator, PermSiteWrite, http.StatusOK},
		{"viewer allowed site read", RoleViewer, PermSiteRead, http.StatusOK},
		{"operator denied apikey manage", RoleOperator, PermAPIKeyManage, http.StatusForbidden},
		{"admin allowed apikey manage", RoleAdmin, PermAPIKeyManage, http.StatusOK},
		{"admin denied tenant manage", RoleAdmin, PermTenantManage, http.StatusForbidden},
		{"owner allowed tenant manage", RoleOwner, PermTenantManage, http.StatusOK},
		{"admin denied audit manage (rebaseline)", RoleAdmin, PermAuditManage, http.StatusForbidden},
		{"owner allowed audit manage (rebaseline)", RoleOwner, PermAuditManage, http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// org-scoped principals (existing behaviour unchanged)
			p := &domain.Principal{Type: domain.PrincipalUser, UserID: user, TenantID: tenant, Role: string(tt.role), Scope: domain.ScopeOrg}
			if code := runWithPrincipal(p, RequirePermission(tt.perm)); code != tt.wantCode {
				t.Fatalf("code = %d, want %d", code, tt.wantCode)
			}
		})
	}
}

// TestRequirePermissionOrgScopeGuard verifies FIX 1: site-scoped principals get
// 403 "org_scope_required" for org-level permissions regardless of their role.
func TestRequirePermissionOrgScopeGuard(t *testing.T) {
	tenant := uuid.New()
	user := uuid.New()
	site := uuid.New()

	orgLevelTests := []struct {
		perm Permission
		role Role
	}{
		{PermMemberManage, RoleAdmin},
		{PermMemberManage, RoleOwner},
		{PermMemberRead, RoleViewer},
		{PermAPIKeyRead, RoleAdmin},
		{PermAPIKeyManage, RoleAdmin},
		{PermAuditRead, RoleAdmin},
		{PermAuditManage, RoleOwner},
		{PermTenantManage, RoleOwner},
	}
	for _, tt := range orgLevelTests {
		t.Run("site_scope/"+string(tt.perm)+"/"+string(tt.role), func(t *testing.T) {
			p := &domain.Principal{
				Type:           domain.PrincipalUser,
				UserID:         user,
				TenantID:       tenant,
				Role:           string(tt.role),
				Scope:          domain.ScopeSite,
				AllowedSiteIDs: []uuid.UUID{site},
			}
			code := runWithPrincipal(p, RequirePermission(tt.perm))
			if code != http.StatusForbidden {
				t.Fatalf("site-scoped %s with role %s: got %d, want 403", tt.perm, tt.role, code)
			}
		})
	}
}

// TestRequirePermissionSitePermsForSiteScope verifies that site-level permissions
// are NOT blocked for site-scoped principals (they need these to do their work).
func TestRequirePermissionSitePermsForSiteScope(t *testing.T) {
	tenant := uuid.New()
	user := uuid.New()
	site := uuid.New()

	tests := []struct {
		perm Permission
		role Role
		want int
	}{
		{PermSiteRead, RoleViewer, http.StatusOK},
		{PermSiteWrite, RoleOperator, http.StatusOK},
		{PermSiteWrite, RoleViewer, http.StatusForbidden}, // viewer cannot write
	}
	for _, tt := range tests {
		t.Run("site_scope/"+string(tt.perm)+"/"+string(tt.role), func(t *testing.T) {
			p := &domain.Principal{
				Type:           domain.PrincipalUser,
				UserID:         user,
				TenantID:       tenant,
				Role:           string(tt.role),
				Scope:          domain.ScopeSite,
				AllowedSiteIDs: []uuid.UUID{site},
			}
			code := runWithPrincipal(p, RequirePermission(tt.perm))
			if code != tt.want {
				t.Fatalf("site-scoped %s with role %s: got %d, want %d", tt.perm, tt.role, code, tt.want)
			}
		})
	}
}

// TestPortalPrincipalRejectedFromSSEGates pins the m66 portal/SSE boundary:
// every SSE stream route (/sites/events, /backups/:id/events,
// /restores/:id/events, /updates/:id/events) is gated by
// RequirePermission(PermSiteRead), and a portal principal must fail that gate.
// Portal users get curated /portal DTOs only; the event streams carry raw
// operational payloads (real connection states, backup lifecycle) that the
// portal deliberately excludes. If RoleClient is ever granted PermSiteRead,
// this test fails and the SSE routes need their own explicit portal check.
func TestPortalPrincipalRejectedFromSSEGates(t *testing.T) {
	p := &domain.Principal{
		Type:           domain.PrincipalUser,
		UserID:         uuid.New(),
		TenantID:       uuid.New(),
		Role:           string(RoleClient),
		Scope:          domain.ScopeSite,
		AllowedSiteIDs: []uuid.UUID{uuid.New()},
		ClientIDs:      []uuid.UUID{uuid.New()},
	}
	if code := runWithPrincipal(p, RequirePermission(PermSiteRead)); code != http.StatusForbidden {
		t.Fatalf("portal principal vs PermSiteRead (the SSE gate) = %d, want 403", code)
	}
}

func TestRequireAuthAndTenant(t *testing.T) {
	if code := runWithPrincipal(nil, RequireAuth()); code != http.StatusUnauthorized {
		t.Fatalf("anonymous RequireAuth = %d, want 401", code)
	}
	noTenant := &domain.Principal{Type: domain.PrincipalUser, UserID: uuid.New()}
	if code := runWithPrincipal(noTenant, RequireTenant()); code != http.StatusForbidden {
		t.Fatalf("no-tenant RequireTenant = %d, want 403", code)
	}
	withTenant := &domain.Principal{Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: uuid.New(), Role: string(RoleViewer)}
	if code := runWithPrincipal(withTenant, RequireTenant()); code != http.StatusOK {
		t.Fatalf("tenant RequireTenant = %d, want 200", code)
	}
}

func TestRequireRole(t *testing.T) {
	tenant := uuid.New()
	viewer := &domain.Principal{Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: tenant, Role: string(RoleViewer)}
	if code := runWithPrincipal(viewer, RequireRole(RoleAdmin)); code != http.StatusForbidden {
		t.Fatalf("viewer vs admin min = %d, want 403", code)
	}
	admin := &domain.Principal{Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: tenant, Role: string(RoleAdmin)}
	if code := runWithPrincipal(admin, RequireRole(RoleAdmin)); code != http.StatusOK {
		t.Fatalf("admin vs admin min = %d, want 200", code)
	}
}

// TestRequireClientPortal verifies the m66 portal gate:
//   - A portal principal (User, ScopeSite, RoleClient, non-empty ClientIDs) is admitted.
//   - Every other principal shape is rejected with 403.
func TestRequireClientPortal(t *testing.T) {
	tenant := uuid.New()
	user := uuid.New()
	site1 := uuid.New()
	client1 := uuid.New()

	portalP := &domain.Principal{
		Type:           domain.PrincipalUser,
		UserID:         user,
		TenantID:       tenant,
		Role:           string(RoleClient),
		Scope:          domain.ScopeSite,
		AllowedSiteIDs: []uuid.UUID{site1},
		ClientIDs:      []uuid.UUID{client1},
	}

	tests := []struct {
		name     string
		p        *domain.Principal
		wantCode int
	}{
		{
			name:     "portal principal admitted",
			p:        portalP,
			wantCode: http.StatusOK,
		},
		{
			name:     "unauthenticated (no principal)",
			p:        nil,
			wantCode: http.StatusUnauthorized,
		},
		{
			name: "org member (role=viewer) rejected",
			p: &domain.Principal{
				Type:     domain.PrincipalUser,
				UserID:   uuid.New(),
				TenantID: tenant,
				Role:     string(RoleViewer),
				Scope:    domain.ScopeOrg,
			},
			wantCode: http.StatusForbidden,
		},
		{
			name: "org member (role=admin) rejected",
			p: &domain.Principal{
				Type:     domain.PrincipalUser,
				UserID:   uuid.New(),
				TenantID: tenant,
				Role:     string(RoleAdmin),
				Scope:    domain.ScopeOrg,
			},
			wantCode: http.StatusForbidden,
		},
		{
			name: "site-share collaborator (role=operator, no ClientIDs) rejected",
			p: &domain.Principal{
				Type:           domain.PrincipalUser,
				UserID:         uuid.New(),
				TenantID:       tenant,
				Role:           string(RoleOperator),
				Scope:          domain.ScopeSite,
				AllowedSiteIDs: []uuid.UUID{site1},
			},
			wantCode: http.StatusForbidden,
		},
		{
			name: "portal principal with empty ClientIDs rejected",
			p: &domain.Principal{
				Type:           domain.PrincipalUser,
				UserID:         uuid.New(),
				TenantID:       tenant,
				Role:           string(RoleClient),
				Scope:          domain.ScopeSite,
				AllowedSiteIDs: []uuid.UUID{site1},
				ClientIDs:      []uuid.UUID{}, // empty
			},
			wantCode: http.StatusForbidden,
		},
		{
			name: "API key (not PrincipalUser) rejected",
			p: &domain.Principal{
				Type:     domain.PrincipalAPIKey,
				TenantID: tenant,
				Role:     string(RoleAdmin),
				Scope:    domain.ScopeOrg,
			},
			wantCode: http.StatusForbidden,
		},
	}

	mw := RequireClientPortal()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := runWithPrincipal(tt.p, mw)
			if got != tt.wantCode {
				t.Fatalf("RequireClientPortal: got %d, want %d", got, tt.wantCode)
			}
		})
	}
}
