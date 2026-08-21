package tests

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/apikey"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// TestAPIKeyCreateSendsDiscriminatorColumns is the red-first proof for the
// #510 Go half. m120 added five columns to api_keys, three of them constrained
// by CHECKs that encode an authorization contract. apikey.Service.Create builds
// sqlc.CreateAPIKeyParams as a KEYED literal, so the three new string columns
// silently took the Go zero value "" once sqlc regenerated the params struct —
// which compiles, vets and unit-tests clean, and then fails every single key
// creation at runtime with SQLSTATE 23514.
//
// This is the hazard GH #458 predicted: a generated params struct gaining a
// field still compiles at every keyed-literal call site while sending nothing.
// The build being green is not evidence that the callers were updated.
//
// Before the fix this test fails with 23514 (api_keys_kind_check). After it,
// Create round-trips and the row carries the legacy-equivalent defaults.
func TestAPIKeyCreateSendsDiscriminatorColumns(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "apikey-cap-create")

	svc := apikey.NewService(pool)

	created, err := svc.Create(ctx, tenant, "legacy role key", authz.RoleOperator)
	if err != nil {
		t.Fatalf("Create failed — this is the m120 caller gap if the error is 23514: %v", err)
	}

	// The row must reproduce present-day behaviour exactly.
	if created.Key.Kind != apikey.KindIntegration {
		t.Errorf("kind = %q, want %q", created.Key.Kind, apikey.KindIntegration)
	}
	if created.Key.AuthModel != domain.AuthModelRole {
		t.Errorf("auth_model = %q, want %q", created.Key.AuthModel, domain.AuthModelRole)
	}
	if created.Key.Capabilities != nil {
		t.Errorf("capabilities = %v, want nil (SQL NULL) for a role key", created.Key.Capabilities)
	}
	if created.Key.SiteScope != domain.ScopeOrg {
		t.Errorf("site_scope = %q, want %q", created.Key.SiteScope, domain.ScopeOrg)
	}
	if len(created.Key.AllowedSiteIDs) != 0 {
		t.Errorf("allowed_site_ids = %v, want empty", created.Key.AllowedSiteIDs)
	}
	if created.Key.Role != authz.RoleOperator {
		t.Errorf("role = %q, want %q", created.Key.Role, authz.RoleOperator)
	}
}

// TestPreExistingRoleKeyAuthorityUnchanged is the over-fire control. A key
// created through the ordinary role path must resolve to EXACTLY the permission
// set it resolved to before m120 — not one permission more, not one fewer.
// The reference set is computed straight from authz.Allows over the stored
// role, which is the pre-m120 behaviour by construction.
//
// This test must be green both before and after the capability work. If it ever
// reddens, an existing key's authority moved, which the #510 brief forbids.
func TestPreExistingRoleKeyAuthorityUnchanged(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "apikey-cap-unchanged")

	svc := apikey.NewService(pool)

	for _, role := range []authz.Role{authz.RoleViewer, authz.RoleOperator, authz.RoleAdmin, authz.RoleOwner} {
		t.Run(string(role), func(t *testing.T) {
			created, err := svc.Create(ctx, tenant, "role key "+string(role), role)
			if err != nil {
				t.Fatalf("Create(%s): %v", role, err)
			}

			// Resolve through the real authentication path.
			key, err := svc.Authenticate(ctx, created.Token)
			if err != nil {
				t.Fatalf("Authenticate(%s): %v", role, err)
			}
			p := apikey.PrincipalFor(key)

			if p.AuthModel != domain.AuthModelRole {
				t.Fatalf("auth_model = %q, want %q — a legacy key must stay on the role model",
					p.AuthModel, domain.AuthModelRole)
			}

			// Every permission in the vocabulary, compared against pre-m120 semantics.
			for _, perm := range authz.AllPermissions() {
				want := authz.Allows(role, perm)
				got := authz.PrincipalAllows(p, perm)
				if got != want {
					t.Errorf("permission %q: PrincipalAllows = %v, authz.Allows(%s) = %v — authority moved",
						perm, got, role, want)
				}
			}
		})
	}
}

// TestCapabilityKeyDoesNotFallBackToRole is the assertion the whole issue
// exists for. api_keys.role is a rank in a totally ordered hierarchy, so a
// machine principal granted an admin-tier permission is granted every
// admin-and-below permission with it. site.files.read is admin-tier
// (authz.PermSiteFilesRead -> RoleAdmin), and member:manage is also admin-tier
// (authz.PermMemberManage -> RoleAdmin). Under the role model those two are
// inseparable. Under the capability model they must not be.
func TestCapabilityKeyDoesNotFallBackToRole(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "apikey-cap-nofallback")

	svc := apikey.NewService(pool)
	siteID := uuid.New()

	created, err := svc.CreateCapability(ctx, tenant, "files reader", apikey.CapabilitySpec{
		Kind:           apikey.KindAgent,
		Capabilities:   []string{string(authz.PermSiteFilesRead)},
		AllowedSiteIDs: []uuid.UUID{siteID},
	})
	if err != nil {
		t.Fatalf("CreateCapability: %v", err)
	}

	key, err := svc.Authenticate(ctx, created.Token)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	p := apikey.PrincipalFor(key)

	// The granted capability is held.
	if !authz.PrincipalAllows(p, authz.PermSiteFilesRead) {
		t.Fatalf("capability key does NOT hold its own granted capability %q", authz.PermSiteFilesRead)
	}

	// THE assertion: the same-rank permission it was never granted is denied.
	if authz.PrincipalAllows(p, authz.PermMemberManage) {
		t.Fatalf("capability key holding only %q was allowed %q — it fell back to role authority",
			authz.PermSiteFilesRead, authz.PermMemberManage)
	}

	// And it holds for every other permission in the vocabulary too: exactly
	// one capability means exactly one permission.
	for _, perm := range authz.AllPermissions() {
		want := perm == authz.PermSiteFilesRead
		if got := authz.PrincipalAllows(p, perm); got != want {
			t.Errorf("permission %q: got %v, want %v", perm, got, want)
		}
	}

	// The site allowlist reached the principal, so the site chokepoint has
	// something to enforce against.
	if p.Scope != domain.ScopeSite {
		t.Errorf("scope = %q, want %q", p.Scope, domain.ScopeSite)
	}
	if len(p.AllowedSiteIDs) != 1 || p.AllowedSiteIDs[0] != siteID {
		t.Errorf("allowed_site_ids = %v, want [%s]", p.AllowedSiteIDs, siteID)
	}
}

// TestUnknownCapabilityRefusedAtCreation proves the vocabulary guard fires.
// The database deliberately does NOT enumerate the permission vocabulary (it
// would fail new writes closed against a lagging database at boot), so this
// validation is Go's and only Go's. An unknown string must be refused, not
// silently stored and then silently ignored at check time.
func TestUnknownCapabilityRefusedAtCreation(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "apikey-cap-unknown")

	svc := apikey.NewService(pool)

	cases := []struct {
		name string
		caps []string
	}{
		{"not in vocabulary", []string{"site.files.read", "site:superuser"}},
		{"plausible but undeclared", []string{"site.files.read_all"}},
		{"malformed — uppercase", []string{"Site.Files.Read"}},
		{"malformed — whitespace", []string{"site.files.read "}},
		{"malformed — empty element", []string{""}},
		{"duplicate element", []string{"site.files.read", "site.files.read"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.CreateCapability(ctx, tenant, "bad caps", apikey.CapabilitySpec{
				Kind:         apikey.KindIntegration,
				Capabilities: tc.caps,
			})
			if err == nil {
				t.Fatalf("CreateCapability accepted %v — an unknown or malformed capability must be refused", tc.caps)
			}
			de, ok := domain.AsDomain(err)
			if !ok {
				t.Fatalf("error is not a domain error (so it is an infra failure, not a refusal): %v", err)
			}
			if !strings.HasPrefix(de.Code, "apikey_capabilit") {
				t.Fatalf("code = %q, want an apikey_capabilit* validation code", de.Code)
			}
		})
	}
}

// TestEmptyCapabilitySetHasZeroAuthority closes the fail-open that auth_model
// exists to prevent. sqlc generates Capabilities []string with no validity
// flag, so SQL NULL and '{}' both arrive as a zero-length slice: len(caps)==0
// cannot tell "no capability set, use role" from "zero capabilities, allow
// nothing". Collapsing them would hand a zero-capability key its full role
// authority, which is the worst possible direction for the failure.
func TestEmptyCapabilitySetHasZeroAuthority(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "apikey-cap-empty")

	svc := apikey.NewService(pool)

	created, err := svc.CreateCapability(ctx, tenant, "zero authority", apikey.CapabilitySpec{
		Kind:         apikey.KindIntegration,
		Capabilities: []string{}, // non-nil, empty: a real set with nothing in it
	})
	if err != nil {
		t.Fatalf("CreateCapability with empty set: %v", err)
	}

	key, err := svc.Authenticate(ctx, created.Token)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	p := apikey.PrincipalFor(key)

	// Distinguishable from a role key even though both have len(Capabilities)==0.
	if p.AuthModel != domain.AuthModelCapability {
		t.Fatalf("auth_model = %q, want %q — the discriminator collapsed and this key would fall back to role",
			p.AuthModel, domain.AuthModelCapability)
	}
	if len(p.Capabilities) != 0 {
		t.Fatalf("capabilities = %v, want empty", p.Capabilities)
	}

	// Zero authority, across the entire vocabulary.
	for _, perm := range authz.AllPermissions() {
		if authz.PrincipalAllows(p, perm) {
			t.Errorf("empty-capability key was allowed %q — zero capabilities must mean zero authority", perm)
		}
	}

	// And the contrast: a role key with the same zero-length Capabilities slice
	// keeps its full role authority. This is the pair that proves the two
	// zero-length slices are NOT being treated alike.
	roleCreated, err := svc.Create(ctx, tenant, "contrast role key", authz.RoleAdmin)
	if err != nil {
		t.Fatalf("Create role key: %v", err)
	}
	roleKey, err := svc.Authenticate(ctx, roleCreated.Token)
	if err != nil {
		t.Fatalf("Authenticate role key: %v", err)
	}
	rp := apikey.PrincipalFor(roleKey)
	if len(rp.Capabilities) != 0 {
		t.Fatalf("precondition: role key should also have a zero-length capability slice, got %v", rp.Capabilities)
	}
	if !authz.PrincipalAllows(rp, authz.PermMemberManage) {
		t.Fatal("role key lost its authority — the discriminator is being read backwards")
	}
}
