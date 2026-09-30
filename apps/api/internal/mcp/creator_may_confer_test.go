package mcp

// requireCreatorMayConfer, on both creation paths: the connection-token mint
// and the OAuth consent Approve. A connection holding mcp.cache.purge asks
// people to clear site caches, so only a principal that may clear caches
// (authz.PermSiteCachePurge) may create one. Each refusal must write nothing,
// and each positive case must reach the store with the capability stored.

import (
	"context"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// conferCase is one creator. Role and capability set are the whole of what
// authz.PrincipalAllows reads.
type conferCase struct {
	name     string
	p        func(tenant uuid.UUID) domain.Principal
	mayPurge bool
}

func conferCreators() []conferCase {
	userWithRole := func(role string) func(uuid.UUID) domain.Principal {
		return func(tenant uuid.UUID) domain.Principal {
			return domain.Principal{
				Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: tenant,
				Role: role, Scope: domain.ScopeOrg, AuthModel: domain.AuthModelRole,
			}
		}
	}
	// A capability-scoped key is judged on its explicit set alone. The role
	// is set to owner on purpose: a gate that fell back to the role compare
	// would admit it, and the refusal case would go green for the wrong
	// reason if the role were low.
	capabilityKey := func(caps ...authz.Permission) func(uuid.UUID) domain.Principal {
		return func(tenant uuid.UUID) domain.Principal {
			names := make([]string, 0, len(caps))
			for _, c := range caps {
				names = append(names, string(c))
			}
			return domain.Principal{
				Type: domain.PrincipalAPIKey, APIKeyID: uuid.New(), TenantID: tenant,
				Role: "owner", Scope: domain.ScopeOrg,
				AuthModel: domain.AuthModelCapability, Capabilities: names,
			}
		}
	}
	return []conferCase{
		{"user without site.cache.purge (viewer)", userWithRole("viewer"), false},
		{"capability-scoped api key without site.cache.purge",
			capabilityKey(authz.PermAPIKeyManage, authz.PermSiteRead, authz.PermSiteCacheManage), false},
		{"user with site.cache.purge (operator)", userWithRole("operator"), true},
		{"capability-scoped api key with site.cache.purge",
			capabilityKey(authz.PermAPIKeyManage, authz.PermSiteRead, authz.PermSiteCachePurge), true},
	}
}

// fixtureAgreesWithAuthz fails when a case's expectation disagrees with
// authz.PrincipalAllows, so a change to the role matrix breaks this fixture
// loudly rather than turning a refusal case into a vacuous pass.
func fixtureAgreesWithAuthz(t *testing.T, tc conferCase, p domain.Principal) {
	t.Helper()
	if got := authz.PrincipalAllows(p, authz.PermSiteCachePurge); got != tc.mayPurge {
		t.Fatalf("fixture %q: authz.PrincipalAllows(%s) = %t, want %t",
			tc.name, authz.PermSiteCachePurge, got, tc.mayPurge)
	}
}

func wantCreatorMayNotConfer(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("a principal without site.cache.purge created a connection holding mcp.cache.purge")
	}
	de, ok := domain.AsDomain(err)
	if !ok || de.Code != ErrCodeCreatorMayNotConfer {
		t.Fatalf("refusal = %v, want a domain error coded %q", err, ErrCodeCreatorMayNotConfer)
	}
	if de.Kind != domain.KindForbidden {
		t.Fatalf("refusal kind = %v, want Forbidden", de.Kind)
	}
}

// TestMintRequiresTheCreatorToHoldCachePurgeToConferIt is the token path.
func TestMintRequiresTheCreatorToHoldCachePurgeToConferIt(t *testing.T) {
	for _, tc := range conferCreators() {
		t.Run(tc.name, func(t *testing.T) {
			tenant := uuid.New()
			p := tc.p(tenant)
			fixtureAgreesWithAuthz(t, tc, p)
			store := &fakeStore{}
			svc := mintService(store)

			_, err := svc.MintConnection(context.Background(), MintConnectionRequest{
				Principal:    p,
				Name:         "ci",
				SiteScope:    SiteScopeRequest{Mode: SiteScopeModeAll},
				Capabilities: &[]Capability{CapSitesRead, CapCachePurge},
			})
			if !tc.mayPurge {
				wantCreatorMayNotConfer(t, err)
				if len(store.minted) != 0 {
					t.Fatalf("a refused mint wrote %d grants", len(store.minted))
				}
				return
			}
			if err != nil {
				t.Fatalf("a principal holding site.cache.purge was refused: %v", err)
			}
			if len(store.minted) != 1 {
				t.Fatalf("want 1 grant written, got %d", len(store.minted))
			}
			if got := store.minted[0].Grant.Capabilities; !slices.Contains(got, string(CapCachePurge)) {
				t.Fatalf("stored capabilities %v do not hold %s", got, CapCachePurge)
			}
		})
	}
}

// TestMintWithoutARequestCapabilityIsNotGated is the over-fire check: the same
// principals that are refused mcp.cache.purge may still mint a read-only
// connection.
func TestMintWithoutARequestCapabilityIsNotGated(t *testing.T) {
	for _, tc := range conferCreators() {
		if tc.mayPurge {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeStore{}
			if _, err := mintService(store).MintConnection(context.Background(), MintConnectionRequest{
				Principal:    tc.p(uuid.New()),
				Name:         "ci",
				SiteScope:    SiteScopeRequest{Mode: SiteScopeModeAll},
				Capabilities: &[]Capability{CapSitesRead},
			}); err != nil {
				t.Fatalf("a read-only mint was refused: %v", err)
			}
			if len(store.minted) != 1 {
				t.Fatalf("want 1 grant written, got %d", len(store.minted))
			}
		})
	}
}

// cacheConsent is an approval whose authorize call asked for both scopes, from
// a client registered for both, so the capability ceiling holds
// mcp.cache.purge and the only thing left to refuse it is the creator gate.
func cacheConsent(t *testing.T, store *fakeStore, svc *Service, p domain.Principal, caps []Capability) ApprovalRequest {
	t.Helper()
	store.client.RegisteredScopes = SupportedScopes()
	req := approvalFor(authorizeForTest(t, svc, string(ScopeRead)+" "+string(ScopeCache)))
	req.Principal = p
	req.Capabilities = &caps
	return req
}

// TestApproveRequiresTheApproverToHoldCachePurgeToConferIt is the consent path.
func TestApproveRequiresTheApproverToHoldCachePurgeToConferIt(t *testing.T) {
	for _, tc := range conferCreators() {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.p(uuid.New())
			fixtureAgreesWithAuthz(t, tc, p)
			store := approvalStore()
			svc := consentSvc(store)
			req := cacheConsent(t, store, svc, p, []Capability{CapSitesRead, CapCachePurge})

			got, err := svc.Approve(context.Background(), req)
			if !tc.mayPurge {
				wantCreatorMayNotConfer(t, err)
				if got.Code != "" {
					t.Fatal("a refused approval returned a code")
				}
				mustNotHaveCreatedAGrant(t, store)
				return
			}
			if err != nil {
				t.Fatalf("a principal holding site.cache.purge was refused: %v", err)
			}
			if !slices.Contains(storedCapabilities(t, store), string(CapCachePurge)) {
				t.Fatalf("stored capabilities %v do not hold %s", storedCapabilities(t, store), CapCachePurge)
			}
		})
	}
}

// TestApproveWithoutARequestCapabilityIsNotGated: the refused principals may
// still approve a read-only connection on the same consent.
func TestApproveWithoutARequestCapabilityIsNotGated(t *testing.T) {
	for _, tc := range conferCreators() {
		if tc.mayPurge {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			store := approvalStore()
			svc := consentSvc(store)
			req := cacheConsent(t, store, svc, tc.p(uuid.New()), []Capability{CapSitesRead})

			if _, err := svc.Approve(context.Background(), req); err != nil {
				t.Fatalf("a read-only approval was refused: %v", err)
			}
			if got := storedCapabilities(t, store); slices.Contains(got, string(CapCachePurge)) {
				t.Fatalf("a read-only approval stored %v", got)
			}
		})
	}
}
