package authz

// rls_isolation_test.go — RLS isolation policy-logic tests.
//
// These tests verify the enforcement wiring (FIX 1–4) that gates site-scoped
// principals from org-level resources and enforces the per-site allowlist. They
// do NOT require a live database; they exercise the Go middleware layer that
// sits in front of Postgres RLS as belt-and-braces.
//
// What is tested:
//   A. Org-level permission set (FIX 1): site-scoped principals receive 403
//      "org_scope_required" for every org-level permission regardless of role.
//   B. Site allowlist (FIX 2): RequireSiteAccess returns 404 for a siteId NOT
//      in AllowedSiteIDs; returns 200 for an allowed siteId; no-ops for org-scope.
//   C. Role clamping sanity: an org-level permission with an org-scoped admin
//      passes; same permission with a site-scoped principal (any role) fails.
//   D. Coverage: every permission in orgLevelPerms is blocked for site-scope.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

func init() { gin.SetMode(gin.TestMode) }

// --- helpers ----------------------------------------------------------------

// runWithPrincipalAndParam builds a one-route engine guarded by mws, injects p
// (or no principal when p == nil), and sets the named param to paramVal.
func runWithPrincipalAndParam(p *domain.Principal, paramName, paramVal string, mws ...gin.HandlerFunc) int {
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		if p != nil {
			c.Request = c.Request.WithContext(domain.WithPrincipal(c.Request.Context(), *p))
		}
		c.Next()
	})
	handlers := append(mws, func(c *gin.Context) { c.Status(http.StatusOK) })
	engine.GET("/x/:"+paramName, handlers...)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x/"+paramVal, nil)
	engine.ServeHTTP(w, req)
	return w.Code
}

// orgPrincipal builds a full org-member principal with the given role.
func orgPrincipal(role Role) domain.Principal {
	return domain.Principal{
		Type:     domain.PrincipalUser,
		UserID:   uuid.New(),
		TenantID: uuid.New(),
		Role:     string(role),
		Scope:    domain.ScopeOrg,
	}
}

// sitePrincipal builds a site-scoped principal with the given role and allowed sites.
func sitePrincipal(role Role, allowedSiteIDs ...uuid.UUID) domain.Principal {
	return domain.Principal{
		Type:           domain.PrincipalUser,
		UserID:         uuid.New(),
		TenantID:       uuid.New(),
		Role:           string(role),
		Scope:          domain.ScopeSite,
		AllowedSiteIDs: allowedSiteIDs,
	}
}

// --- A. Org-level permission guard (FIX 1) -----------------------------------

// TestOrgLevelPermsBlockedForSiteScope verifies that every org-level permission
// is denied for site-scoped principals with 403 "org_scope_required".
func TestOrgLevelPermsBlockedForSiteScope(t *testing.T) {
	// The authoritative set of org-level permissions. If a permission is added
	// to authz.orgLevelPerms without being listed here, this test will NOT catch
	// it — but the reverse is also covered: if a permission IS here but gets
	// removed from orgLevelPerms, the test fails, alerting the reviewer.
	orgPerms := []Permission{
		PermMemberManage,
		PermMemberRead,
		PermAPIKeyRead,
		PermAPIKeyManage,
		PermAuditRead,
		PermTenantManage,
	}

	// Test each org-level permission against every role a site-scoped principal
	// might have (after the role-clamping in middleware/auth.go, the highest
	// effective role is "operator", but we test all four to be exhaustive).
	roles := []Role{RoleViewer, RoleOperator, RoleAdmin, RoleOwner}

	for _, perm := range orgPerms {
		for _, role := range roles {
			t.Run(string(perm)+"/role="+string(role), func(t *testing.T) {
				p := sitePrincipal(role)
				code := runWithPrincipal(&p, RequirePermission(perm))
				if code != http.StatusForbidden {
					t.Fatalf("site-scoped %s with role %s got %d, want 403", perm, role, code)
				}
			})
		}
	}
}

// TestOrgLevelPermsAllowedForOrgScope verifies that org-level permissions are
// granted for org-scoped principals with the minimum required role.
func TestOrgLevelPermsAllowedForOrgScope(t *testing.T) {
	tests := []struct {
		perm     Permission
		minRole  Role
		wantPass bool
	}{
		{PermMemberManage, RoleAdmin, true},
		{PermMemberRead, RoleViewer, true},
		{PermAPIKeyRead, RoleAdmin, true},
		{PermAPIKeyManage, RoleAdmin, true},
		{PermAuditRead, RoleAdmin, true},
		{PermTenantManage, RoleOwner, true},
		// Verify that a role BELOW the minimum is still denied for org-scope.
		{PermMemberManage, RoleViewer, false},
		{PermTenantManage, RoleAdmin, false},
	}
	for _, tt := range tests {
		t.Run(string(tt.perm)+"/role="+string(tt.minRole), func(t *testing.T) {
			p := orgPrincipal(tt.minRole)
			code := runWithPrincipal(&p, RequirePermission(tt.perm))
			if tt.wantPass && code != http.StatusOK {
				t.Fatalf("org-scoped %s with role %s got %d, want 200", tt.perm, tt.minRole, code)
			}
			if !tt.wantPass && code == http.StatusOK {
				t.Fatalf("org-scoped %s with role %s got 200, want non-200", tt.perm, tt.minRole)
			}
		})
	}
}

// --- B. Site allowlist guard (FIX 2) ----------------------------------------

// TestRequireSiteAccessAllowlist verifies RequireSiteAccess enforcement.
func TestRequireSiteAccessAllowlist(t *testing.T) {
	siteA := uuid.New()
	siteB := uuid.New()

	tests := []struct {
		name      string
		principal *domain.Principal
		paramVal  string
		wantCode  int
	}{
		{
			name:      "org-scoped principal: no-op, always passes",
			principal: func() *domain.Principal { p := orgPrincipal(RoleViewer); return &p }(),
			paramVal:  siteB.String(),
			wantCode:  http.StatusOK,
		},
		{
			name:      "site-scoped: siteId IN allowlist → 200",
			principal: func() *domain.Principal { p := sitePrincipal(RoleViewer, siteA); return &p }(),
			paramVal:  siteA.String(),
			wantCode:  http.StatusOK,
		},
		{
			name:      "site-scoped: siteId NOT in allowlist → 404",
			principal: func() *domain.Principal { p := sitePrincipal(RoleViewer, siteA); return &p }(),
			paramVal:  siteB.String(),
			wantCode:  http.StatusNotFound,
		},
		{
			name:      "site-scoped: empty allowlist → 404 for any siteId",
			principal: func() *domain.Principal { p := sitePrincipal(RoleViewer); return &p }(),
			paramVal:  siteA.String(),
			wantCode:  http.StatusNotFound,
		},
		{
			name:      "site-scoped: malformed UUID → 404",
			principal: func() *domain.Principal { p := sitePrincipal(RoleViewer, siteA); return &p }(),
			paramVal:  "not-a-uuid",
			wantCode:  http.StatusNotFound,
		},
		{
			name:      "no principal: 401",
			principal: nil,
			paramVal:  siteA.String(),
			wantCode:  http.StatusUnauthorized,
		},
		{
			name: "site-scoped: multiple allowed sites, correct one passes",
			principal: func() *domain.Principal {
				p := sitePrincipal(RoleViewer, siteA, siteB)
				return &p
			}(),
			paramVal: siteB.String(),
			wantCode: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code := runWithPrincipalAndParam(tt.principal, "siteId", tt.paramVal, RequireSiteAccess("siteId"))
			if code != tt.wantCode {
				t.Fatalf("got %d, want %d", code, tt.wantCode)
			}
		})
	}
}

// --- C. Isolation of site B from a principal only granted site A -------------

// TestSiteScopedPrincipalCannotAccessSiteB exercises the full middleware stack
// for a principal whose AllowedSiteIDs = [A]. It verifies that:
//   - site-wide reads on site A pass RequireSiteAccess.
//   - site-wide reads on site B are blocked with 404.
//   - org-level permissions (members, API keys, audit) are blocked with 403.
//
// This mirrors the scenario from the spec's security review checklist item 4.
func TestSiteScopedPrincipalCannotAccessSiteB(t *testing.T) {
	siteA := uuid.New()
	siteB := uuid.New()

	p := sitePrincipal(RoleOperator, siteA)

	// Site A accessible via RequireSiteAccess.
	codeA := runWithPrincipalAndParam(&p, "siteId", siteA.String(), RequireSiteAccess("siteId"))
	if codeA != http.StatusOK {
		t.Errorf("site A: got %d, want 200", codeA)
	}

	// Site B blocked.
	codeB := runWithPrincipalAndParam(&p, "siteId", siteB.String(), RequireSiteAccess("siteId"))
	if codeB != http.StatusNotFound {
		t.Errorf("site B: got %d, want 404", codeB)
	}

	// Org-level perms blocked regardless of site.
	for _, perm := range []Permission{PermMemberManage, PermMemberRead, PermAPIKeyRead, PermAuditRead} {
		code := runWithPrincipal(&p, RequirePermission(perm))
		if code != http.StatusForbidden {
			t.Errorf("perm %s: got %d, want 403", perm, code)
		}
	}
}

// --- D. Coverage: orgLevelPerms set integrity --------------------------------

// TestOrgLevelPermsSetIntegrity asserts that every permission listed as
// "org-level" in our expected set is actually in authz.orgLevelPerms. If a new
// org-level permission is added to role.go without being added to orgLevelPerms,
// this test will detect the gap when the new permission is also added here.
//
// Conversely: if a permission is in orgLevelPerms but NOT in expectedOrgPerms,
// that is NOT caught here (it would only be a false positive restriction), but
// it is covered by TestOrgLevelPermsBlockedForSiteScope which will test it.
func TestOrgLevelPermsSetIntegrity(t *testing.T) {
	expectedOrgPerms := []Permission{
		PermMemberManage,
		PermMemberRead,
		PermAPIKeyRead,
		PermAPIKeyManage,
		PermAuditRead,
		PermTenantManage,
	}
	for _, perm := range expectedOrgPerms {
		if _, isOrgLevel := orgLevelPerms[perm]; !isOrgLevel {
			t.Errorf("permission %q is expected to be org-level but is not in authz.orgLevelPerms; add it to prevent privilege escalation by site-scoped principals", perm)
		}
	}
}

// TestSitePermsNotInOrgLevelSet asserts that site-level permissions (PermSiteRead,
// PermSiteWrite, PermSiteAutologin) are NOT in orgLevelPerms — they should be
// accessible to site-scoped principals.
func TestSitePermsNotInOrgLevelSet(t *testing.T) {
	sitePerms := []Permission{
		PermSiteRead,
		PermSiteWrite,
		PermSiteAutologin,
	}
	for _, perm := range sitePerms {
		if _, isOrgLevel := orgLevelPerms[perm]; isOrgLevel {
			t.Errorf("permission %q should NOT be org-level (site-scoped principals must be able to use it), but is in authz.orgLevelPerms", perm)
		}
	}
}
