package autologin

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

func init() { gin.SetMode(gin.TestMode) }

// ---------------------------------------------------------------------------
// GH #286: mint-time default injection (Service.Mint).
// ---------------------------------------------------------------------------

// TestServiceMintEmptyBodyUsesStoredDefault proves an empty
// target_wp_user_login in the mint request is filled in from the site's
// stored policy default, and that the injected value flows into both the
// persisted token AND the audit.requested metadata (the reporter's audit gap
// this feature closes).
func TestServiceMintEmptyBodyUsesStoredDefault(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()
	userID := uuid.New()
	repo := newFakeRepo()
	repo.policies[siteID] = Policy{
		SiteID: siteID, TenantID: tenantID, Enabled: true,
		AllowedWPRoles: DefaultAllowedWPRoles, MaxSessionAgeMinutes: 30,
		DefaultWPUserLogin: "editor.alice",
	}
	store := newFakeStore()
	signer := &fakeSigner{}
	sites := &fakeSiteLookup{urls: map[uuid.UUID]string{siteID: "https://wp.example.com"}}
	rec := &nopRecorder{}
	svc := NewService(repo, store, signer, sites, NewMemoryLimiter(), rec, domain.SystemClock{}, Config{})

	tok, err := svc.Mint(t.Context(), MintRequest{TenantID: tenantID, SiteID: siteID, InitiatorID: userID})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	stored, ok := repo.tokens[tok.NonceID]
	if !ok {
		t.Fatal("token not persisted")
	}
	if stored.TargetWPUserLogin != "editor.alice" {
		t.Fatalf("persisted target = %q, want the stored policy default %q", stored.TargetWPUserLogin, "editor.alice")
	}

	if len(rec.entries) != 1 || rec.entries[0].Action != audit.ActionAutologinRequested {
		t.Fatalf("expected single autologin.requested audit, got %+v", rec.entries)
	}
	if got := rec.entries[0].Metadata["target_wp_user_login"]; got != "editor.alice" {
		t.Fatalf("audit target_wp_user_login = %v, want editor.alice", got)
	}

	// The Redis payload must also carry the injected default (the consume path
	// reads TargetWPUserLogin straight from the persisted/Redis payload).
	if payload, ok := store.values[tok.NonceID]; !ok || payload.TargetWPUserLogin != "editor.alice" {
		t.Fatalf("redis payload target = %+v, want editor.alice", payload)
	}
}

// TestServiceMintExplicitUserOverridesStoredDefault proves the picker's
// explicit choice always wins over a configured site default.
func TestServiceMintExplicitUserOverridesStoredDefault(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()
	userID := uuid.New()
	repo := newFakeRepo()
	repo.policies[siteID] = Policy{
		SiteID: siteID, TenantID: tenantID, Enabled: true,
		AllowedWPRoles: DefaultAllowedWPRoles, MaxSessionAgeMinutes: 30,
		DefaultWPUserLogin: "editor.alice",
	}
	store := newFakeStore()
	signer := &fakeSigner{}
	sites := &fakeSiteLookup{urls: map[uuid.UUID]string{siteID: "https://wp.example.com"}}
	rec := &nopRecorder{}
	svc := NewService(repo, store, signer, sites, NewMemoryLimiter(), rec, domain.SystemClock{}, Config{})

	tok, err := svc.Mint(t.Context(), MintRequest{
		TenantID: tenantID, SiteID: siteID, InitiatorID: userID, TargetWPUser: "bob.support",
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	if got := repo.tokens[tok.NonceID].TargetWPUserLogin; got != "bob.support" {
		t.Fatalf("persisted target = %q, want the explicit picker choice bob.support", got)
	}
	if got := rec.entries[0].Metadata["target_wp_user_login"]; got != "bob.support" {
		t.Fatalf("audit target_wp_user_login = %v, want bob.support", got)
	}
}

// TestServiceMintNoStoredDefaultLeavesTargetEmpty proves the pre-existing
// "agent picks the first administrator" fallback is unchanged when a site has
// never configured a default (default_wp_user_login == "").
func TestServiceMintNoStoredDefaultLeavesTargetEmpty(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()
	userID := uuid.New()
	repo := newFakeRepo()
	repo.policies[siteID] = Policy{
		SiteID: siteID, TenantID: tenantID, Enabled: true,
		AllowedWPRoles: DefaultAllowedWPRoles, MaxSessionAgeMinutes: 30,
		// DefaultWPUserLogin intentionally left "".
	}
	store := newFakeStore()
	signer := &fakeSigner{}
	sites := &fakeSiteLookup{urls: map[uuid.UUID]string{siteID: "https://wp.example.com"}}
	rec := &nopRecorder{}
	svc := NewService(repo, store, signer, sites, NewMemoryLimiter(), rec, domain.SystemClock{}, Config{})

	tok, err := svc.Mint(t.Context(), MintRequest{TenantID: tenantID, SiteID: siteID, InitiatorID: userID})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	if got := repo.tokens[tok.NonceID].TargetWPUserLogin; got != "" {
		t.Fatalf("persisted target = %q, want empty (agent fallback preserved)", got)
	}
	if got := rec.entries[0].Metadata["target_wp_user_login"]; got != "" {
		t.Fatalf("audit target_wp_user_login = %v, want empty", got)
	}
}

// ---------------------------------------------------------------------------
// GH #286: Service.UpdatePolicy validation + upsert-seeding contract.
// ---------------------------------------------------------------------------

func TestServiceUpdatePolicyValidation(t *testing.T) {
	tests := []struct {
		name    string
		login   string
		wantErr bool
		wantCod string
	}{
		{name: "61 chars rejected (WP cap is 60)", login: strings.Repeat("a", 61), wantErr: true, wantCod: "default_wp_user_login_too_long"},
		{name: "60 chars accepted (at the cap)", login: strings.Repeat("a", 60), wantErr: false},
		{name: "bad charset rejected", login: "bob smith!", wantErr: true, wantCod: "default_wp_user_login_invalid"},
		{name: "email-shaped login accepted (WP allows @)", login: "bob@example.com", wantErr: false},
		{name: "empty clears the default", login: "", wantErr: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tenantID := uuid.New()
			siteID := uuid.New()
			repo := newFakeRepo()
			repo.policies[siteID] = Policy{SiteID: siteID, TenantID: tenantID, DefaultWPUserLogin: "old.value", AllowedWPRoles: DefaultAllowedWPRoles}
			sites := &fakeSiteLookup{urls: map[uuid.UUID]string{siteID: "https://wp.example.com"}}
			svc := newSvc(t, repo, newFakeStore(), &fakeSigner{}, sites)

			saved, err := svc.UpdatePolicy(t.Context(), tenantID, siteID, domain.Principal{Type: domain.PrincipalUser, UserID: uuid.New()}, PolicyInput{
				Enabled: true, DefaultWPUserLogin: tt.login,
			})
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected a validation error")
				}
				de, ok := domain.AsDomain(err)
				if !ok || de.Code != tt.wantCod {
					t.Fatalf("want code %q, got %v", tt.wantCod, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if saved.DefaultWPUserLogin != tt.login {
				t.Fatalf("saved default = %q, want %q", saved.DefaultWPUserLogin, tt.login)
			}
		})
	}
}

// TestServiceUpdatePolicySeedsRowWithoutDisturbingAllowedRoles proves a PUT
// against a site with no existing policy row seeds the other columns'
// defaults (mirroring UpsertAutologinPolicyDefault) and never widens
// allowed_wp_roles.
func TestServiceUpdatePolicySeedsRowWithoutDisturbingAllowedRoles(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()
	repo := newFakeRepo() // no pre-seeded policy: the row does not exist yet.
	sites := &fakeSiteLookup{urls: map[uuid.UUID]string{siteID: "https://wp.example.com"}}
	svc := newSvc(t, repo, newFakeStore(), &fakeSigner{}, sites)

	saved, err := svc.UpdatePolicy(t.Context(), tenantID, siteID, domain.Principal{Type: domain.PrincipalUser, UserID: uuid.New()}, PolicyInput{
		Enabled: true, DefaultWPUserLogin: "first.write",
	})
	if err != nil {
		t.Fatalf("update policy: %v", err)
	}
	if len(saved.AllowedWPRoles) != 1 || saved.AllowedWPRoles[0] != "administrator" {
		t.Fatalf("allowed_wp_roles = %v, want the seeded default [administrator]", saved.AllowedWPRoles)
	}
	if saved.MaxSessionAgeMinutes != 30 {
		t.Fatalf("max_session_age_minutes = %d, want the seeded default 30", saved.MaxSessionAgeMinutes)
	}

	// A second write with a different default must NOT touch allowed_wp_roles.
	saved2, err := svc.UpdatePolicy(t.Context(), tenantID, siteID, domain.Principal{Type: domain.PrincipalUser, UserID: uuid.New()}, PolicyInput{
		Enabled: false, DefaultWPUserLogin: "second.write",
	})
	if err != nil {
		t.Fatalf("second update: %v", err)
	}
	if saved2.DefaultWPUserLogin != "second.write" || saved2.Enabled != false {
		t.Fatalf("second write did not persist enabled/default: %+v", saved2)
	}
	if len(saved2.AllowedWPRoles) != 1 || saved2.AllowedWPRoles[0] != "administrator" {
		t.Fatalf("allowed_wp_roles changed on an update path: %v", saved2.AllowedWPRoles)
	}
}

// TestServiceUpdatePolicyRecordsAudit proves a successful PUT records
// autologin.policy_updated with the saved settings.
func TestServiceUpdatePolicyRecordsAudit(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()
	repo := newFakeRepo()
	rec := &nopRecorder{}
	sites := &fakeSiteLookup{urls: map[uuid.UUID]string{siteID: "https://wp.example.com"}}
	svc := NewService(repo, newFakeStore(), &fakeSigner{}, sites, NewMemoryLimiter(), rec, domain.SystemClock{}, Config{})

	if _, err := svc.UpdatePolicy(t.Context(), tenantID, siteID, domain.Principal{Type: domain.PrincipalUser, UserID: uuid.New()}, PolicyInput{
		Enabled: true, DefaultWPUserLogin: "audited.user",
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if len(rec.entries) != 1 || rec.entries[0].Action != audit.ActionAutologinPolicyUpdated {
		t.Fatalf("expected single autologin.policy_updated audit entry, got %+v", rec.entries)
	}
	if got := rec.entries[0].Metadata["default_wp_user_login"]; got != "audited.user" {
		t.Fatalf("audit metadata default_wp_user_login = %v, want audited.user", got)
	}
}

// ---------------------------------------------------------------------------
// GH #286: PolicyHandler HTTP layer: RBAC + allowed_wp_roles not writable.
// ---------------------------------------------------------------------------

// buildPolicyEngine mounts a real PolicyHandler on a /api/v1 group, injecting
// the given principal ahead of the route (mirrors server.go's real wiring).
func buildPolicyEngine(svc *Service, p domain.Principal) *gin.Engine {
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(domain.WithPrincipal(c.Request.Context(), p))
		c.Next()
	})
	v1 := engine.Group("/api/v1")
	NewPolicyHandler(svc).Register(v1)
	return engine
}

// TestPolicyHandlerRBAC proves the Admin floor (same permission as mint):
// viewer and operator are denied on both GET and PUT; admin and owner succeed.
func TestPolicyHandlerRBAC(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()

	tests := []struct {
		role     authz.Role
		wantCode int
	}{
		{authz.RoleViewer, http.StatusForbidden},
		{authz.RoleOperator, http.StatusForbidden},
		{authz.RoleAdmin, http.StatusOK},
		{authz.RoleOwner, http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(string(tt.role)+"/GET", func(t *testing.T) {
			repo := newFakeRepo()
			sites := &fakeSiteLookup{urls: map[uuid.UUID]string{siteID: "https://wp.example.com"}}
			svc := newSvc(t, repo, newFakeStore(), &fakeSigner{}, sites)
			engine := buildPolicyEngine(svc, domain.Principal{
				Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: tenantID, Role: string(tt.role),
			})
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/v1/sites/"+siteID.String()+"/autologin-policy", nil)
			engine.ServeHTTP(rec, req)
			if rec.Code != tt.wantCode {
				t.Fatalf("GET role=%s status = %d, want %d (body=%s)", tt.role, rec.Code, tt.wantCode, rec.Body.String())
			}
		})
		t.Run(string(tt.role)+"/PUT", func(t *testing.T) {
			repo := newFakeRepo()
			sites := &fakeSiteLookup{urls: map[uuid.UUID]string{siteID: "https://wp.example.com"}}
			svc := newSvc(t, repo, newFakeStore(), &fakeSigner{}, sites)
			engine := buildPolicyEngine(svc, domain.Principal{
				Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: tenantID, Role: string(tt.role),
			})
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPut, "/api/v1/sites/"+siteID.String()+"/autologin-policy",
				bytes.NewReader([]byte(`{"enabled":true,"default_wp_user_login":"alice"}`)))
			req.Header.Set("Content-Type", "application/json")
			engine.ServeHTTP(rec, req)
			if rec.Code != tt.wantCode {
				t.Fatalf("PUT role=%s status = %d, want %d (body=%s)", tt.role, rec.Code, tt.wantCode, rec.Body.String())
			}
		})
	}
}

// TestPolicyHandlerPutRejectsAllowedWPRoles proves allowed_wp_roles is NOT
// writable via the PUT body: DisallowUnknownFields rejects the attempt with
// 422 rather than silently ignoring the field.
func TestPolicyHandlerPutRejectsAllowedWPRoles(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()
	repo := newFakeRepo()
	sites := &fakeSiteLookup{urls: map[uuid.UUID]string{siteID: "https://wp.example.com"}}
	svc := newSvc(t, repo, newFakeStore(), &fakeSigner{}, sites)
	engine := buildPolicyEngine(svc, domain.Principal{
		Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: tenantID, Role: string(authz.RoleAdmin),
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/sites/"+siteID.String()+"/autologin-policy",
		bytes.NewReader([]byte(`{"enabled":true,"default_wp_user_login":"alice","allowed_wp_roles":["editor"]}`)))
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body=%s)", rec.Code, rec.Body.String())
	}
	// The role ceiling in the fake repo must remain untouched.
	if p, ok := repo.policies[siteID]; ok && len(p.AllowedWPRoles) > 0 && p.AllowedWPRoles[0] == "editor" {
		t.Fatal("allowed_wp_roles was widened via the PUT body")
	}
}

// TestPolicyHandlerGetPutRoundTrip proves a full GET -> PUT -> GET cycle
// through the HTTP layer returns the saved settings and never emits
// allowed_wp_roles as writable (informational only).
func TestPolicyHandlerGetPutRoundTrip(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()
	repo := newFakeRepo()
	sites := &fakeSiteLookup{urls: map[uuid.UUID]string{siteID: "https://wp.example.com"}}
	svc := newSvc(t, repo, newFakeStore(), &fakeSigner{}, sites)
	engine := buildPolicyEngine(svc, domain.Principal{
		Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: tenantID, Role: string(authz.RoleOwner),
	})

	// GET before any policy exists auto-creates the default row.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sites/"+siteID.String()+"/autologin-policy", nil)
	engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("initial GET status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"default_wp_user_login":""`) {
		t.Fatalf("initial GET body missing empty default: %s", rec.Body.String())
	}

	// PUT a new default.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPut, "/api/v1/sites/"+siteID.String()+"/autologin-policy",
		bytes.NewReader([]byte(`{"enabled":true,"default_wp_user_login":"final.user"}`)))
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"default_wp_user_login":"final.user"`) {
		t.Fatalf("PUT response missing saved default: %s", rec.Body.String())
	}

	// GET again reflects the saved default.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/sites/"+siteID.String()+"/autologin-policy", nil)
	engine.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), `"default_wp_user_login":"final.user"`) {
		t.Fatalf("follow-up GET missing saved default: %s", rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Security fix: cross-tenant site-ownership guard on GetPolicy/UpdatePolicy.
//
// autologin_policies.site_id is the table PK, and its RLS WITH CHECK only
// validates tenant_id == app.tenant_id (never that site_id actually belongs
// to tenant_id). Without resolving the site within the caller's tenant
// first, a caller in tenant A supplying tenant B's siteId could create or
// overwrite a phantom (siteB, tenantA) row, breaking tenant B's own
// GetOrCreatePolicy/mint. These tests use a SiteLookup that resolves no
// sites for any tenant, so they fail against the unguarded code (the old
// GetPolicy/UpdatePolicy never consulted SiteLookup at all).
// ---------------------------------------------------------------------------

// TestServiceGetPolicyForeignTenantSite404sWithoutCreatingRow proves GetPolicy
// resolves the site within the caller's tenant before touching the policy
// row: a site the caller's tenant does not own 404s the same way Mint does,
// and no phantom policy row is auto-created for it.
func TestServiceGetPolicyForeignTenantSite404sWithoutCreatingRow(t *testing.T) {
	tenantA := uuid.New()
	foreignSiteID := uuid.New() // belongs to some other tenant; absent from SiteLookup
	repo := newFakeRepo()
	sites := &fakeSiteLookup{} // resolves no site for any tenant
	svc := newSvc(t, repo, newFakeStore(), &fakeSigner{}, sites)

	_, err := svc.GetPolicy(t.Context(), tenantA, foreignSiteID)
	if err == nil {
		t.Fatal("expected site_not_found")
	}
	de, ok := domain.AsDomain(err)
	if !ok || de.Kind != domain.KindNotFound {
		t.Fatalf("want NotFound, got %v", err)
	}
	if _, ok := repo.policies[foreignSiteID]; ok {
		t.Fatal("GetPolicy created a phantom policy row for a foreign-tenant site")
	}
}

// TestServiceUpdatePolicyForeignTenantSite404sWithoutWriting proves
// UpdatePolicy applies the same guard: no phantom (foreignSite, tenantA) row
// is written when the site does not belong to the caller's tenant.
func TestServiceUpdatePolicyForeignTenantSite404sWithoutWriting(t *testing.T) {
	tenantA := uuid.New()
	foreignSiteID := uuid.New()
	repo := newFakeRepo()
	sites := &fakeSiteLookup{}
	svc := newSvc(t, repo, newFakeStore(), &fakeSigner{}, sites)

	_, err := svc.UpdatePolicy(t.Context(), tenantA, foreignSiteID, domain.Principal{Type: domain.PrincipalUser, UserID: uuid.New()}, PolicyInput{
		Enabled: true, DefaultWPUserLogin: "attacker.user",
	})
	if err == nil {
		t.Fatal("expected site_not_found")
	}
	de, ok := domain.AsDomain(err)
	if !ok || de.Kind != domain.KindNotFound {
		t.Fatalf("want NotFound, got %v", err)
	}
	if _, ok := repo.policies[foreignSiteID]; ok {
		t.Fatal("UpdatePolicy created a phantom policy row for a foreign-tenant site")
	}
}

// TestPolicyHandlerForeignTenantSite404sWithoutCreatingOrWritingRow proves the
// HTTP layer surfaces 404 for both GET and PUT when siteId in the URL does
// not belong to the caller's tenant, and that neither request creates or
// overwrites a policy row (the cross-tenant write/DoS this guard closes).
func TestPolicyHandlerForeignTenantSite404sWithoutCreatingOrWritingRow(t *testing.T) {
	tenantA := uuid.New()
	foreignSiteID := uuid.New()
	repo := newFakeRepo()
	sites := &fakeSiteLookup{} // foreignSiteID resolves for no tenant
	svc := newSvc(t, repo, newFakeStore(), &fakeSigner{}, sites)
	engine := buildPolicyEngine(svc, domain.Principal{
		Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: tenantA, Role: string(authz.RoleOwner),
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sites/"+foreignSiteID.String()+"/autologin-policy", nil)
	engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET status = %d, want 404 (body=%s)", rec.Code, rec.Body.String())
	}
	if _, ok := repo.policies[foreignSiteID]; ok {
		t.Fatal("GET created a phantom policy row for a foreign-tenant site")
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPut, "/api/v1/sites/"+foreignSiteID.String()+"/autologin-policy",
		bytes.NewReader([]byte(`{"enabled":true,"default_wp_user_login":"attacker.user"}`)))
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("PUT status = %d, want 404 (body=%s)", rec.Code, rec.Body.String())
	}
	if _, ok := repo.policies[foreignSiteID]; ok {
		t.Fatal("PUT created a phantom policy row for a foreign-tenant site")
	}
}
