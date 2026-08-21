package authz_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// TestAuthorizeSitesRefusesSiteOutsideAllowlist is proof 2: a key allowlisted
// to site A, reaching a route that fans out over {A, B} inside the handler,
// gets A and only A. No middleware runs on this path — there is no :siteId in
// a bulk route — so the chokepoint is the entire boundary.
func TestAuthorizeSitesRefusesSiteOutsideAllowlist(t *testing.T) {
	siteA, siteB := uuid.New(), uuid.New()

	capKey := domain.Principal{
		Type:           domain.PrincipalAPIKey,
		APIKeyID:       uuid.New(),
		TenantID:       uuid.New(),
		Role:           string(authz.RoleOwner),
		Scope:          domain.ScopeSite,
		AllowedSiteIDs: []uuid.UUID{siteA},
		AuthModel:      domain.AuthModelCapability,
		Capabilities:   []string{string(authz.PermSiteFilesRead)},
	}

	if !capKey.IsSiteConstrained() {
		t.Fatal("precondition failed: the key under test is not site-constrained, so the filter below proves nothing")
	}

	tok, denied := authz.AuthorizeSites(context.Background(), capKey, []uuid.UUID{siteA, siteB})

	if !tok.Contains(siteA) {
		t.Errorf("site A (%s) is on the allowlist but was refused", siteA)
	}
	if tok.Contains(siteB) {
		t.Errorf("BYPASS: site B (%s) is NOT on the allowlist but the fan-out was authorized for it", siteB)
	}
	if tok.Len() != 1 {
		t.Errorf("authorized set = %d sites, want 1 (%v)", tok.Len(), tok.IDs())
	}
	if len(denied) != 1 || denied[0] != siteB {
		t.Errorf("denied = %v, want exactly [%s]", denied, siteB)
	}
}

// TestZeroAuthorizedSitesAuthorizesNothing is what makes the chokepoint hard to
// skip: a handler that declares the token instead of calling AuthorizeSites
// gets a value that admits nothing, so the omission is an empty fan-out rather
// than a tenant-wide one.
func TestZeroAuthorizedSitesAuthorizesNothing(t *testing.T) {
	var zero authz.AuthorizedSites
	if zero.Authorized() {
		t.Error("zero AuthorizedSites reports Authorized() == true")
	}
	if zero.Len() != 0 {
		t.Errorf("zero AuthorizedSites Len() = %d, want 0", zero.Len())
	}
	if zero.IDs() != nil {
		t.Errorf("zero AuthorizedSites IDs() = %v, want nil", zero.IDs())
	}
	for _, id := range []uuid.UUID{uuid.New(), uuid.Nil} {
		if zero.Contains(id) {
			t.Errorf("zero AuthorizedSites Contains(%s) = true", id)
		}
	}
	// A real but empty authorization is distinguishable from the zero value.
	empty, _ := authz.AuthorizeSites(context.Background(), domain.Principal{
		Scope: domain.ScopeSite,
	}, []uuid.UUID{uuid.New()})
	if !empty.Authorized() {
		t.Error("a real AuthorizeSites result reports Authorized() == false")
	}
	if empty.Len() != 0 {
		t.Errorf("site-scoped principal with an empty allowlist authorized %d sites, want 0", empty.Len())
	}
}

// TestEmptyRequestDoesNotMeanEverything: silence must not widen.
func TestEmptyRequestDoesNotMeanEverything(t *testing.T) {
	orgUser := domain.Principal{
		Type:     domain.PrincipalUser,
		UserID:   uuid.New(),
		TenantID: uuid.New(),
		Role:     string(authz.RoleOwner),
		Scope:    domain.ScopeOrg,
	}
	tok, denied := authz.AuthorizeSites(context.Background(), orgUser, nil)
	if tok.Len() != 0 {
		t.Errorf("an empty request authorized %d sites — it was read as 'all'", tok.Len())
	}
	if len(denied) != 0 {
		t.Errorf("an empty request denied %v, want nothing", denied)
	}
}

// TestOrgPrincipalsUnchanged is proof 3 for the site gate: every principal
// shape that exists today and is NOT site-scoped keeps tenant-wide site reach,
// through both CanAccessSite and the chokepoint. This is the over-fire control
// for finding 2 — it must be green before and after the change.
func TestOrgPrincipalsUnchanged(t *testing.T) {
	siteX, siteY := uuid.New(), uuid.New()

	cases := []struct {
		name string
		p    domain.Principal
	}{
		{"org-scoped session user", domain.Principal{
			Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: uuid.New(),
			Role: string(authz.RoleOwner), Scope: domain.ScopeOrg,
		}},
		{"legacy zero-value scope session user", domain.Principal{
			Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: uuid.New(),
			Role: string(authz.RoleOperator), // Scope deliberately unset
		}},
		{"legacy pre-m120 API key", domain.Principal{
			Type: domain.PrincipalAPIKey, APIKeyID: uuid.New(), TenantID: uuid.New(),
			Role: string(authz.RoleAdmin), // Scope and AuthModel both unset
		}},
		{"m120 org-scoped capability key", domain.Principal{
			Type: domain.PrincipalAPIKey, APIKeyID: uuid.New(), TenantID: uuid.New(),
			Role: string(authz.RoleClient), Scope: domain.ScopeOrg,
			AuthModel: domain.AuthModelCapability, Capabilities: []string{string(authz.PermSiteFilesRead)},
		}},
		{"org-scoped viewer", domain.Principal{
			Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: uuid.New(),
			Role: string(authz.RoleViewer), Scope: domain.ScopeOrg,
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.p.IsSiteConstrained() {
				t.Fatalf("REGRESSION: %s is now site-constrained; it was tenant-wide before #510", tc.name)
			}
			for _, id := range []uuid.UUID{siteX, siteY, uuid.New()} {
				if !tc.p.CanAccessSite(id) {
					t.Errorf("REGRESSION: %s was refused site %s", tc.name, id)
				}
			}
			tok, denied := authz.AuthorizeSites(context.Background(), tc.p, []uuid.UUID{siteX, siteY})
			if tok.Len() != 2 {
				t.Errorf("REGRESSION: %s authorized for %d of 2 requested sites", tc.name, tok.Len())
			}
			if len(denied) != 0 {
				t.Errorf("REGRESSION: %s was denied %v", tc.name, denied)
			}
		})
	}
}

// TestSiteScopedCollaboratorUnchanged: the existing site-share collaborator —
// the only principal that WAS constrained before #510 — behaves identically.
func TestSiteScopedCollaboratorUnchanged(t *testing.T) {
	siteA, siteB := uuid.New(), uuid.New()
	collab := domain.Principal{
		Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: uuid.New(),
		Role: string(authz.RoleOperator), Scope: domain.ScopeSite,
		AllowedSiteIDs: []uuid.UUID{siteA},
	}
	if !collab.CanAccessSite(siteA) {
		t.Errorf("collaborator was refused its allowlisted site %s", siteA)
	}
	if collab.CanAccessSite(siteB) {
		t.Errorf("collaborator reached non-allowlisted site %s", siteB)
	}
}

// TestRunTenantTxDispatchMatchesDomainPredicate cross-checks the predicate that
// internal/db/db.go restates inline (it cannot import domain — domain imports
// db) against domain.Principal.IsSiteConstrained, over every combination that
// matters. If the two ever disagree, a principal is constrained at the HTTP
// gate and tenant-wide inside the transaction, or the reverse.
func TestRunTenantTxDispatchMatchesDomainPredicate(t *testing.T) {
	site := uuid.New()
	cases := []struct {
		name    string
		scope   string
		allowed []uuid.UUID
	}{
		{"unset scope, no allowlist", "", nil},
		{"org scope, no allowlist", domain.ScopeOrg, nil},
		{"site scope, no allowlist", domain.ScopeSite, nil},
		{"site scope, with allowlist", domain.ScopeSite, []uuid.UUID{site}},
		{"unset scope, with allowlist", "", []uuid.UUID{site}},
		{"org scope, with allowlist", domain.ScopeOrg, []uuid.UUID{site}},
	}
	for _, tc := range cases {
		p := domain.Principal{Scope: tc.scope, AllowedSiteIDs: tc.allowed}
		// This expression is the literal condition in db.RunTenantTx.
		dbDispatchesScoped := tc.scope == "site" || len(p.GetAllowedSiteIDs()) > 0
		if got := p.IsSiteConstrained(); got != dbDispatchesScoped {
			t.Errorf("DRIFT (%s): domain.IsSiteConstrained=%v but db.RunTenantTx would dispatch scoped=%v",
				tc.name, got, dbDispatchesScoped)
		}
	}
}
