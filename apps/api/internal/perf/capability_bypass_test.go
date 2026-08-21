package perf

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

func capCtx(p domain.Principal) *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	req := httptest.NewRequest("POST", "/api/v1/sites/x/perf/cache/purge", nil)
	c.Request = req.WithContext(domain.WithPrincipal(req.Context(), p))
	return c
}

// TestCapabilityKeyDeniedRoleOnlyPermissionThroughPerfHandler is the perf-side
// half of proof 1. (*Handler).allows here gates cache purge-all and the media
// clean delete — both destructive — and it read the role only until #510.
func TestCapabilityKeyDeniedRoleOnlyPermissionThroughPerfHandler(t *testing.T) {
	h := &Handler{}

	capKey := domain.Principal{
		Type:      domain.PrincipalAPIKey,
		APIKeyID:  uuid.New(),
		TenantID:  uuid.New(),
		Role:      string(authz.RoleOwner),
		Scope:     domain.ScopeOrg,
		AuthModel: domain.AuthModelCapability,
		// Read-only capability: it must not reach either destructive branch.
		Capabilities: []string{string(authz.PermSiteFilesRead)},
	}

	for _, perm := range []authz.Permission{
		authz.PermSiteCacheDeleteAll,
		authz.PermMediaCleanDelete,
	} {
		if !authz.Allows(authz.RoleOwner, perm) {
			t.Fatalf("precondition failed: role owner does not grant %q, so the denial below proves nothing", perm)
		}
	}

	c := capCtx(capKey)
	for _, perm := range []authz.Permission{
		authz.PermSiteCacheDeleteAll,
		authz.PermMediaCleanDelete,
	} {
		if h.allows(c, perm) {
			t.Errorf("BYPASS: capability key (caps=%v, role=owner) was granted %q through the perf handler",
				capKey.Capabilities, perm)
		}
	}
}

// TestRoleBehaviourUnchangedThroughPerfHandler is the perf-side over-fire
// control: legacy role principals must be answered exactly as the role matrix
// answers, across the whole vocabulary.
func TestRoleBehaviourUnchangedThroughPerfHandler(t *testing.T) {
	h := &Handler{}
	perms := authz.AllPermissions()
	if len(perms) == 0 {
		t.Fatal("authz.AllPermissions() returned an empty vocabulary — this control proves nothing")
	}
	roles := []authz.Role{authz.RoleOwner, authz.RoleAdmin, authz.RoleOperator, authz.RoleViewer, authz.RoleClient}

	n := 0
	for _, role := range roles {
		p := domain.Principal{
			Type:     domain.PrincipalAPIKey,
			APIKeyID: uuid.New(),
			TenantID: uuid.New(),
			Role:     string(role),
		}
		c := capCtx(p)
		for _, perm := range perms {
			want := authz.Allows(role, perm)
			if got := h.allows(c, perm); got != want {
				t.Errorf("REGRESSION: role=%s perm=%q: handler allows=%v, role matrix=%v", role, perm, got, want)
			}
			n++
		}
	}
	t.Logf("%d role-model assertions, all unchanged", n)
}
