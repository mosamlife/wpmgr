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
