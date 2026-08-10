package email

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// THE DOOR THIS FILE WATCHES (GH #380).
//
// GetConfig falls back to the organisation-wide row for a site that has no
// config of its own, and the fallback carries the ORG row's ID. Three per-site
// writes take their config ID straight from it, and PermEmailManage is a
// per-site permission, so somebody invited to one site could reach the org row
// through their own site's URL:
//
//   - PUT    /sites/:siteId/email/connections/:connKey
//   - DELETE /sites/:siteId/email/connections/:connKey
//   - PUT    /sites/:siteId/email/webhook-config
//
// On the registry that is the credential escalation the audience check closes,
// arriving by another route: repoint an org connection at your own mail server,
// send no secret so the upsert preserves the org credential underneath it, then
// let the next push hand that credential to your site's agent. On the webhook
// fields it hands the same collaborator the org's route token, which
// authenticates inbound provider events for every site in the organisation.

// principalCtx builds a request carrying an authenticated principal and the
// :siteId path parameter, the way the router would.
func principalCtx(t *testing.T, method, body string, p domain.Principal, siteID uuid.UUID) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(method, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req.WithContext(domain.WithPrincipal(context.Background(), p))
	c.Params = gin.Params{{Key: "siteId", Value: siteID.String()}, {Key: "connKey", Value: "billing"}}
	return c, rec
}

// orgAndSite wires a service whose tenant has an organisation-wide config and
// one site inheriting it.
func orgAndSite(t *testing.T) (*Handler, *fakeRepo, uuid.UUID, uuid.UUID, uuid.UUID) {
	t.Helper()
	tenantID, siteID, orgConfigID := uuid.New(), uuid.New(), uuid.New()
	repo := newFakeRepo()
	repo.org[tenantID] = Config{
		ID: orgConfigID, TenantID: tenantID, Provider: "smtp",
		Config: map[string]any{"host": "smtp.org-relay.example", "port": float64(587)},
	}
	svc := NewService(&Repo{}, &fakeEncryptor{}, nil)
	svc.repo = repo
	return &Handler{svc: svc}, repo, tenantID, siteID, orgConfigID
}

func TestHandler_PutConnection_SiteScopedCollaboratorCannotReachTheOrgRegistry(t *testing.T) {
	h, repo, tenantID, siteID, _ := orgAndSite(t)

	c, rec := principalCtx(t, http.MethodPut,
		`{"provider":"smtp","config":{"host":"collector.attacker.example","port":587}}`,
		domain.Principal{
			Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: tenantID,
			Role: "member", Scope: domain.ScopeSite, AllowedSiteIDs: []uuid.UUID{siteID},
		}, siteID)
	h.putConnection(c)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("a site-scoped collaborator edited the organisation's connection registry: status %d", rec.Code)
	}
	if repo.connUpserts != 0 {
		t.Errorf("the refusal has to happen before the write, %d upserts reached the repository", repo.connUpserts)
	}
	if !strings.Contains(rec.Body.String(), "email_org_row_requires_org_scope") {
		t.Errorf("expected the org-scope refusal code, got %s", rec.Body.String())
	}
}

func TestHandler_DeleteConnection_SiteScopedCollaboratorCannotReachTheOrgRegistry(t *testing.T) {
	h, _, tenantID, siteID, orgConfigID := orgAndSite(t)
	h.svc.repo.(*fakeRepo).addConnection(Connection{
		TenantID: tenantID, ConfigID: orgConfigID, ConnectionKey: "billing", Provider: "smtp",
	}, "org-relay-password")

	c, rec := principalCtx(t, http.MethodDelete, "", domain.Principal{
		Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: tenantID,
		Role: "member", Scope: domain.ScopeSite, AllowedSiteIDs: []uuid.UUID{siteID},
	}, siteID)
	h.deleteConnection(c)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("a site-scoped collaborator deleted an organisation connection: status %d", rec.Code)
	}
	if _, ok := h.svc.repo.(*fakeRepo).conns[connRowKey(orgConfigID, "billing")]; !ok {
		t.Error("the organisation's connection was deleted anyway")
	}
}

func TestHandler_PutWebhookConfig_SiteScopedCollaboratorCannotRotateTheOrgRouteToken(t *testing.T) {
	h, repo, tenantID, siteID, _ := orgAndSite(t)

	c, rec := principalCtx(t, http.MethodPut, `{"rotate_token":true}`, domain.Principal{
		Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: tenantID,
		Role: "member", Scope: domain.ScopeSite, AllowedSiteIDs: []uuid.UUID{siteID},
	}, siteID)
	h.putWebhookConfig(c)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("a site-scoped collaborator rotated the organisation's webhook route token: status %d", rec.Code)
	}
	if len(repo.webhookWrites) != 0 {
		t.Errorf("the organisation row was written anyway: %v", repo.webhookWrites)
	}
}

// The guard is about WHOSE row is being written, not about refusing the feature.
// An organisation member editing the organisation registry through a site that
// inherits it keeps working exactly as before.
func TestHandler_PutConnection_OrgMemberStillEditsTheInheritedRegistry(t *testing.T) {
	h, repo, tenantID, siteID, orgConfigID := orgAndSite(t)

	c, rec := principalCtx(t, http.MethodPut,
		`{"provider":"smtp","config":{"host":"smtp.org-relay.example","port":587},"secret":"new-password"}`,
		domain.Principal{
			Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: tenantID,
			Role: "admin", Scope: domain.ScopeOrg,
		}, siteID)
	h.putConnection(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("an organisation member was refused: status %d body %s", rec.Code, rec.Body.String())
	}
	if _, ok := repo.conns[connRowKey(orgConfigID, "billing")]; !ok {
		t.Error("the connection was not written to the organisation row")
	}
}

// And a site-scoped collaborator on a site that HAS a config of its own is
// untouched: the write lands on that site's row, which is theirs to edit.
func TestHandler_PutConnection_SiteScopedCollaboratorKeepsTheirOwnSiteRegistry(t *testing.T) {
	h, repo, tenantID, siteID, orgConfigID := orgAndSite(t)
	siteConfigID := uuid.New()
	repo.site[siteKey(tenantID, siteID)] = Config{
		ID: siteConfigID, TenantID: tenantID, SiteID: &siteID, Provider: "smtp",
		Config: map[string]any{"host": "smtp.site.example"},
	}

	c, rec := principalCtx(t, http.MethodPut,
		`{"provider":"smtp","config":{"host":"smtp.site.example"},"secret":"site-password"}`,
		domain.Principal{
			Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: tenantID,
			Role: "member", Scope: domain.ScopeSite, AllowedSiteIDs: []uuid.UUID{siteID},
		}, siteID)
	h.putConnection(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("a collaborator was refused on their own site's registry: status %d body %s", rec.Code, rec.Body.String())
	}
	if _, ok := repo.conns[connRowKey(siteConfigID, "billing")]; !ok {
		t.Error("the connection did not land on the site's own config row")
	}
	if _, ok := repo.conns[connRowKey(orgConfigID, "billing")]; ok {
		t.Error("the connection landed on the organisation row instead")
	}
}
