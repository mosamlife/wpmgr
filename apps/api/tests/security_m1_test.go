package tests

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/auth"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/tenant"
)

// newTenantStack builds a tenant service over the real pool (membership-scoped
// reads use InUserTx + the memberships_self_read policy).
func newTenantStack(pool *db.Pool) *tenant.Service {
	return tenant.NewService(tenant.NewRepo(pool), domain.NewValidator(), domain.SystemClock{})
}

// seedUserMembership creates a user and grants them the given role in tenantID.
func seedUserMembership(t *testing.T, repo *auth.Repo, email string, tenantID uuid.UUID, role authz.Role) auth.User {
	t.Helper()
	ctx := context.Background()
	u, err := repo.CreateUser(ctx, email, "", email, "", "")
	if err != nil {
		t.Fatalf("create user %s: %v", email, err)
	}
	if _, err := repo.CreateMembership(ctx, u.ID, tenantID, role); err != nil {
		t.Fatalf("create membership %s: %v", email, err)
	}
	return u
}

// userPrincipal builds a user Principal for the tenant-scoping service calls.
func userPrincipal(userID, tenantID uuid.UUID, role authz.Role) domain.Principal {
	return domain.Principal{Type: domain.PrincipalUser, UserID: userID, TenantID: tenantID, Role: string(role)}
}

// TestTenantReadScopedToMemberships is the FIX 1 regression: a member of tenant
// A must NOT be able to read tenant B by id (404) and must NOT see tenant B in
// the list. Exercised through the real service + repo + Postgres RLS.
func TestTenantReadScopedToMemberships(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()

	tenantA := seedTenant(t, pool, "scope-a")
	tenantB := seedTenant(t, pool, "scope-b")

	authRepo := auth.NewRepo(pool)
	userA := seedUserMembership(t, authRepo, "scope-a@example.com", tenantA, authz.RoleOwner)
	_ = seedUserMembership(t, authRepo, "scope-b@example.com", tenantB, authz.RoleOwner)

	svc := newTenantStack(pool)
	pA := userPrincipal(userA.ID, tenantA, authz.RoleOwner)

	// Get-by-id of tenant B must be 404 (NotFound), not 200.
	if _, err := svc.GetForPrincipal(ctx, pA, tenantB); err == nil {
		t.Fatal("user A was able to read tenant B by id")
	} else if de, ok := domain.AsDomain(err); !ok || de.Kind != domain.KindNotFound {
		t.Fatalf("want NotFound reading tenant B, got %v", err)
	}

	// Get-by-id of tenant A (own) must succeed.
	gotA, err := svc.GetForPrincipal(ctx, pA, tenantA)
	if err != nil {
		t.Fatalf("user A reading own tenant A: %v", err)
	}
	if gotA.ID != tenantA {
		t.Fatalf("got wrong tenant for A: %+v", gotA)
	}

	// List must contain only tenant A, never tenant B.
	list, err := svc.ListForPrincipal(ctx, pA, tenant.ListInput{Limit: 100})
	if err != nil {
		t.Fatalf("list for user A: %v", err)
	}
	if len(list) != 1 || list[0].ID != tenantA {
		t.Fatalf("user A list leaked tenants (want only A): %+v", list)
	}
	for _, tn := range list {
		if tn.ID == tenantB {
			t.Fatal("tenant B leaked into user A's tenant list")
		}
	}
}

// TestInviteRoleCeiling is the FIX 2 regression: an admin inviting an owner is
// rejected (Forbidden), while an owner inviting an owner succeeds.
func TestInviteRoleCeiling(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()

	tenantID := seedTenant(t, pool, "ceiling")
	authRepo := auth.NewRepo(pool)
	svc, _ := newAuthStack(pool)

	admin := seedUserMembership(t, authRepo, "admin@example.com", tenantID, authz.RoleAdmin)
	owner := seedUserMembership(t, authRepo, "owner@example.com", tenantID, authz.RoleOwner)

	// Admin granting owner: must be rejected with Forbidden.
	_, _, err := svc.Invite(ctx, tenantID, admin.ID, authz.RoleAdmin, auth.InviteInput{
		Email:    "escalated@example.com",
		Password: "a-very-strong-password",
		Name:     "Escalated",
		Role:     authz.RoleOwner,
	})
	if err == nil {
		t.Fatal("admin was able to grant owner (privilege escalation)")
	} else if de, ok := domain.AsDomain(err); !ok || de.Kind != domain.KindForbidden {
		t.Fatalf("want Forbidden for admin->owner, got %v", err)
	}

	// Admin granting admin (equal to own role): allowed.
	if _, _, err := svc.Invite(ctx, tenantID, admin.ID, authz.RoleAdmin, auth.InviteInput{
		Email:    "peer-admin@example.com",
		Password: "a-very-strong-password",
		Name:     "Peer Admin",
		Role:     authz.RoleAdmin,
	}); err != nil {
		t.Fatalf("admin granting admin should succeed: %v", err)
	}

	// Owner granting owner: allowed.
	if _, _, err := svc.Invite(ctx, tenantID, owner.ID, authz.RoleOwner, auth.InviteInput{
		Email:    "new-owner@example.com",
		Password: "a-very-strong-password",
		Name:     "New Owner",
		Role:     authz.RoleOwner,
	}); err != nil {
		t.Fatalf("owner granting owner should succeed: %v", err)
	}
}

// TestOIDCLinkingRequiresBothSidesVerified covers the linking rules end to end
// against a real database, through the generic OIDC entry point.
//
// THE EXPECTATION HERE CHANGED, DELIBERATELY. The original version of this test
// asserted that a verified provider email SHOULD link to a pre-existing
// password account, and seeded that account without ever verifying its address.
// A security review of the social sign-in work established that this is the
// account takeover: registration does not require proving control of an
// address, so an attacker can register a victim's address, and linking the
// victim's later verified sign-in onto that row hands them an account whose
// password the attacker chose. Linking now requires the LOCAL account to have
// been verified too, and this test asserts that stronger rule.
func TestOIDCLinkingRequiresBothSidesVerified(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()

	tenantID := seedTenant(t, pool, "oidc-link")
	authRepo := auth.NewRepo(pool)
	svc, _ := newAuthStack(pool)

	// Pre-existing password account, address never verified. This is what
	// self-serve registration leaves behind, and what an attacker can create
	// for somebody else's address.
	existing, err := authRepo.CreateUser(ctx, "victim@example.com", "hash", "Victim", "", "")
	if err != nil {
		t.Fatalf("seed existing user: %v", err)
	}
	if _, err := authRepo.CreateMembership(ctx, existing.ID, tenantID, authz.RoleOwner); err != nil {
		t.Fatalf("seed membership: %v", err)
	}

	createTenant := func(ctx context.Context, name, slug string) (uuid.UUID, error) {
		return seedTenant(t, pool, "oidc-bootstrap-"+slug), nil
	}

	// 1. Unverified provider email must never link.
	_, err = svc.UpsertOIDCUser(ctx, "https://evil-idp.example", "evil-subject",
		"victim@example.com", false /* emailVerified */, "Attacker", createTenant)
	if err != nil {
		if de, ok := domain.AsDomain(err); !ok || (de.Kind != domain.KindConflict && de.Kind != domain.KindForbidden) {
			t.Fatalf("want a refusal or a unique-email conflict, got %v", err)
		}
	}
	assertIdentityNotBoundTo(t, ctx, authRepo, "evil-subject", existing.ID)

	// 2. THE TAKEOVER. Verified provider email, but the local account has never
	//    been verified by this install. Must be refused.
	_, err = svc.UpsertOIDCUser(ctx, "https://trusted-idp.example", "trusted-subject",
		"victim@example.com", true /* emailVerified */, "Victim", createTenant)
	if err == nil {
		t.Fatal("linking a verified identity onto a never-verified local account is an account takeover; it must be refused")
	}
	if de, ok := domain.AsDomain(err); !ok || de.Code != "social_link_requires_verification" {
		t.Fatalf("want social_link_requires_verification, got %v", err)
	}
	assertIdentityNotBoundTo(t, ctx, authRepo, "trusted-subject", existing.ID)

	// 3. Once the address IS verified on this install, the same sign-in links.
	//    This is the recovery path the refusal above mails a link for.
	if err := authRepo.MarkUserEmailVerified(ctx, existing.ID); err != nil {
		t.Fatalf("verify the seeded account: %v", err)
	}
	res, err := svc.UpsertOIDCUser(ctx, "https://trusted-idp.example", "trusted-subject",
		"victim@example.com", true /* emailVerified */, "Victim", createTenant)
	if err != nil {
		t.Fatalf("with both sides verified, linking should succeed: %v", err)
	}
	if res.User.ID != existing.ID {
		t.Fatalf("should resolve to the existing account, got %s", res.User.ID)
	}
}

// assertIdentityNotBoundTo fails when the given external identity resolves to
// the account it must not have been attached to.
func assertIdentityNotBoundTo(t *testing.T, ctx context.Context, repo *auth.Repo, subject string, mustNotBe uuid.UUID) {
	t.Helper()
	// Issuer is no longer part of the identity key (m111), so the strongest form
	// of this assertion is now subject-wide: the identity must not resolve to
	// the protected account under ANY issuer.
	linked, err := repo.GetUserByIdentity(ctx, "oidc", subject)
	if err != nil {
		return // no such identity at all, which is the strongest pass
	}
	if linked.ID == mustNotBe {
		t.Fatalf("identity %q was linked to the pre-existing account", subject)
	}
}
