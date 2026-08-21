package files

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// ctxWith builds a *gin.Context carrying p exactly the way the auth middleware
// does — on the request context, not on gin's key/value bag — so h.allows is
// exercised through the same retrieval path a live request uses.
func ctxWith(p domain.Principal) *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	req := httptest.NewRequest("GET", "/api/v1/sites/x/files", nil)
	c.Request = req.WithContext(domain.WithPrincipal(req.Context(), p))
	return c
}

// TestCapabilityKeyDeniedRoleOnlyPermissionThroughFilesHandler is proof 1 for
// #510 finding 1, taken at the real call site: (*Handler).allows, the private
// helper that gates every sensitive branch in this package — reading a
// sensitive file, writing code, deleting a file. Before the fix it read
// authz.Allows(authz.Role(p.Role), perm), which consults the role hierarchy and
// ignores the capability set entirely.
//
// The key under test holds exactly one capability, site.files.read, and carries
// role "owner". Owner outranks every permission in the matrix, so if the role
// is consulted at all every assertion below flips.
func TestCapabilityKeyDeniedRoleOnlyPermissionThroughFilesHandler(t *testing.T) {
	h := &Handler{}

	capKey := domain.Principal{
		Type:      domain.PrincipalAPIKey,
		APIKeyID:  uuid.New(),
		TenantID:  uuid.New(),
		Role:      string(authz.RoleOwner), // deliberately the highest role
		Scope:     domain.ScopeOrg,
		AuthModel: domain.AuthModelCapability,
		Capabilities: []string{
			string(authz.PermSiteFilesRead),
		},
	}

	// Sanity: the role really would grant these, so the denials below are the
	// capability model working and not an artefact of a weak role.
	for _, perm := range []authz.Permission{
		authz.PermSiteFilesReadSensitive,
		authz.PermSiteFilesWriteCode,
		authz.PermSiteFilesDelete,
	} {
		if !authz.Allows(authz.RoleOwner, perm) {
			t.Fatalf("precondition failed: role owner does not grant %q, so the denial below proves nothing", perm)
		}
	}

	c := ctxWith(capKey)

	// Granted: the one capability the key actually holds.
	if !h.allows(c, authz.PermSiteFilesRead) {
		t.Errorf("capability key holding %q was denied it through the files handler", authz.PermSiteFilesRead)
	}

	// Denied: everything the ROLE would have granted but the capability set does not.
	for _, perm := range []authz.Permission{
		authz.PermSiteFilesReadSensitive,
		authz.PermSiteFilesWriteCode,
		authz.PermSiteFilesDelete,
	} {
		if h.allows(c, perm) {
			t.Errorf("BYPASS: capability key (caps=%v, role=owner) was granted %q through the files handler — "+
				"the role was consulted", capKey.Capabilities, perm)
		}
	}
}

// TestRoleKeyBehaviourUnchangedThroughFilesHandler is the over-fire control for
// proof 3: a legacy role principal — a session user and a pre-m120 API key,
// both with the zero-value AuthModel — must get byte-identical answers from
// h.allows and from the role matrix, for every permission in the vocabulary.
// If the fix narrowed anything for existing principals, this reddens.
func TestRoleKeyBehaviourUnchangedThroughFilesHandler(t *testing.T) {
	perms := authz.AllPermissions()
	if len(perms) == 0 {
		t.Fatal("authz.AllPermissions() returned an empty vocabulary — this control proves nothing")
	}

	roles := []authz.Role{
		authz.RoleOwner, authz.RoleAdmin, authz.RoleOperator, authz.RoleViewer, authz.RoleClient,
	}

	for _, role := range roles {
		for _, principalType := range []domain.PrincipalType{domain.PrincipalUser, domain.PrincipalAPIKey} {
			p := domain.Principal{
				Type:     principalType,
				TenantID: uuid.New(),
				Role:     string(role),
				// AuthModel deliberately left at the zero value: this is
				// exactly how every principal built before m120 arrives.
			}
			if principalType == domain.PrincipalUser {
				p.UserID = uuid.New()
			} else {
				p.APIKeyID = uuid.New()
			}
			c := ctxWith(p)

			for _, perm := range perms {
				want := authz.Allows(role, perm)
				got := h0().allows(c, perm)
				if got != want {
					t.Errorf("REGRESSION: %s principal role=%s perm=%q: handler allows=%v, role matrix=%v — "+
						"existing behaviour moved", principalType, role, perm, got, want)
				}
			}
		}
	}
	t.Logf("checked %d permissions x %d roles x 2 principal types = %d assertions, all unchanged",
		len(perms), len(roles), len(perms)*len(roles)*2)
}

func h0() *Handler { return &Handler{} }
