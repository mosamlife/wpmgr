package tests

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/auth"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// testClaim is the provisioning claim these tests configure. It stands in for
// the value an installer mints and hands to whoever is entitled to own the
// install.
const testClaim = "test-provisioning-claim-value"

// TestFirstRunOwnership_RequiresTheProvisioningClaim is the fires half: every
// way of arriving without the claim is refused, and refused identically.
//
// To watch it go red: delete the `if !s.bootstrapClaimAccepted(claim)` guard at
// the top of Service.Bootstrap in internal/auth/service.go.
func TestFirstRunOwnership_RequiresTheProvisioningClaim(t *testing.T) {
	t.Run("no claim configured on the install", func(t *testing.T) {
		pool := startPostgres(t)
		ctx := context.Background()
		svc, _ := newAuthStack(pool)
		// Deliberately NOT calling SetBootstrapClaimSecret.

		// Even presenting the "right" secret cannot work when the install has
		// none: an unconfigured claim is "nobody may claim this", not "anybody".
		for _, presented := range []string{"", testClaim, "anything"} {
			_, err := svc.Bootstrap(ctx, auth.RegisterInput{
				Email:    "attacker@example.com",
				Password: "a-very-strong-password",
				Name:     "Attacker",
			}, presented)
			assertRegistrationClosed(t, err)
		}

		assertUserCount(t, pool, 0)
		assertTenantCount(t, pool, 0)
	})

	t.Run("wrong claim presented", func(t *testing.T) {
		pool := startPostgres(t)
		ctx := context.Background()
		svc, _ := newAuthStack(pool)
		svc.SetBootstrapClaimSecret(testClaim)

		for _, presented := range []string{
			"",                                 // none at all
			"wrong",                            // shorter
			testClaim + "x",                    // longer
			"test-provisioning-claim-valuX",    // same length, last byte differs
			"Test-Provisioning-Claim-Value",    // case differs
			" test-provisioning-claim-value  ", // trimmed to the right value: this one MUST succeed, see below
		} {
			if presented == " test-provisioning-claim-value  " {
				continue // covered by the does-not-over-fire test
			}
			_, err := svc.Bootstrap(ctx, auth.RegisterInput{
				Email:    "attacker@example.com",
				Password: "a-very-strong-password",
				Name:     "Attacker",
			}, presented)
			assertRegistrationClosed(t, err)
		}

		assertUserCount(t, pool, 0)
		assertTenantCount(t, pool, 0)
	})
}

// TestFirstRunOwnership_RefusalIsIndistinguishable proves the refusal an
// unclaimed install returns is byte-identical to the one an already-owned
// install returns, so the endpoint cannot be used to sort installs into
// "waiting to be claimed" and "already claimed".
//
// To watch it go red: give the no-claim-configured branch its own error code in
// internal/auth/handler.go / service.go (e.g. domain.Forbidden("bootstrap_not_configured", ...)).
func TestFirstRunOwnership_RefusalIsIndistinguishable(t *testing.T) {
	ctx := context.Background()

	// An install with no claim configured, still unowned.
	unclaimedPool := startPostgres(t)
	unclaimedSvc, _ := newAuthStack(unclaimedPool)
	_, unclaimedErr := unclaimedSvc.Bootstrap(ctx, auth.RegisterInput{
		Email: "probe@example.com", Password: "a-very-strong-password",
	}, "some-guess")

	// An install that has already been claimed, probed with the WRONG claim.
	claimedPool := startPostgres(t)
	claimedSvc, _ := newAuthStack(claimedPool)
	claimedSvc.SetBootstrapClaimSecret(testClaim)
	if _, err := claimedSvc.Bootstrap(ctx, auth.RegisterInput{
		Email: "owner@example.com", Password: "a-very-strong-password", Name: "Owner",
	}, testClaim); err != nil {
		t.Fatalf("legitimate bootstrap: %v", err)
	}
	_, claimedErr := claimedSvc.Bootstrap(ctx, auth.RegisterInput{
		Email: "probe@example.com", Password: "a-very-strong-password",
	}, "some-guess")

	// And the same install probed with the RIGHT claim, which is now spent.
	_, spentErr := claimedSvc.Bootstrap(ctx, auth.RegisterInput{
		Email: "probe@example.com", Password: "a-very-strong-password",
	}, testClaim)

	for _, pair := range []struct {
		name string
		err  error
	}{
		{"unclaimed install, wrong claim", unclaimedErr},
		{"claimed install, wrong claim", claimedErr},
		{"claimed install, correct claim", spentErr},
	} {
		de, ok := domain.AsDomain(pair.err)
		if !ok {
			t.Fatalf("%s: want a domain error, got %v", pair.name, pair.err)
		}
		if de.Kind != domain.KindForbidden || de.Code != "registration_closed" {
			t.Fatalf("%s: want forbidden/registration_closed, got %v/%s", pair.name, de.Kind, de.Code)
		}
	}

	// The messages must match too — a differing message is as good a probe as a
	// differing code.
	a, _ := domain.AsDomain(unclaimedErr)
	b, _ := domain.AsDomain(claimedErr)
	c, _ := domain.AsDomain(spentErr)
	if a.Message != b.Message || b.Message != c.Message {
		t.Fatalf("refusal messages differ:\n unclaimed: %q\n claimed:   %q\n spent:     %q", a.Message, b.Message, c.Message)
	}

	// Neither refusal may echo the configured claim.
	for _, m := range []string{a.Message, b.Message, c.Message} {
		if containsSecret(m) {
			t.Fatalf("refusal message echoes the provisioning claim: %q", m)
		}
	}
}

// TestFirstRunOwnership_CorrectClaimStillWorks is the does-not-over-fire half.
// A guard that reddens correct work gets switched off, and then it guards
// nothing.
//
// To watch it go red: make bootstrapClaimAccepted return false unconditionally
// in internal/auth/bootstrap_claim.go.
func TestFirstRunOwnership_CorrectClaimStillWorks(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	svc, _ := newAuthStack(pool)
	svc.SetBootstrapClaimSecret(testClaim)

	// Presented with the surrounding whitespace a shell, a .env file or a
	// compose file routinely attaches. Trimming is not laxity: the operator set
	// the right value and cannot see the newline.
	res, err := svc.Bootstrap(ctx, auth.RegisterInput{
		Email:    "owner@example.com",
		Password: "a-very-strong-password",
		Name:     "Owner",
	}, "  "+testClaim+"\n")
	if err != nil {
		t.Fatalf("bootstrap with the correct claim: %v", err)
	}
	if len(res.Memberships) != 1 || res.Memberships[0].Role != "owner" {
		t.Fatalf("want exactly one owner membership, got %+v", res.Memberships)
	}
	if res.ActiveTenant == uuid.Nil {
		t.Fatal("bootstrap did not set an active tenant")
	}
	if res.User.ID == uuid.Nil {
		t.Fatal("bootstrap did not create a user")
	}
	assertUserCount(t, pool, 1)
	assertTenantCount(t, pool, 1)

	// The session-issuing half of the existing behaviour: the owner can log in
	// immediately, with no verification step.
	if _, err := svc.Login(ctx, "owner@example.com", "a-very-strong-password"); err != nil {
		t.Fatalf("owner login after bootstrap: %v", err)
	}

	// An already-owned install is unaffected: a second bootstrap is refused
	// even with the correct claim.
	_, err = svc.Bootstrap(ctx, auth.RegisterInput{
		Email: "second@example.com", Password: "another-strong-password",
	}, testClaim)
	assertRegistrationClosed(t, err)
	assertUserCount(t, pool, 1)
	assertTenantCount(t, pool, 1)
}

// TestFirstRunOwnership_ConcurrentClaimsMintExactlyOneOwner is the lock proof.
// Two callers presenting the correct claim at the same instant must produce one
// owner and one organisation, not two.
//
// To watch it go red: in internal/auth/bootstrap_repo.go, move the CountUsers
// read out of the InInstallLockTx callback (do it on r.q before the call), or
// drop the pg_advisory_xact_lock line from Pool.InInstallLockTx. Either restores
// the check-then-act and this test reports two owners.
func TestFirstRunOwnership_ConcurrentClaimsMintExactlyOneOwner(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	svc, _ := newAuthStack(pool)
	svc.SetBootstrapClaimSecret(testClaim)

	const racers = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		wins    int
		refused int
		other   []error
	)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := svc.Bootstrap(ctx, auth.RegisterInput{
				Email:    uuid.NewString() + "@example.com",
				Password: "a-very-strong-password",
				Name:     "Racer",
			}, testClaim)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
			case isRegistrationClosed(err):
				refused++
			default:
				other = append(other, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if len(other) > 0 {
		t.Fatalf("unexpected errors from concurrent bootstraps: %v", other)
	}
	if wins != 1 {
		t.Fatalf("concurrent bootstraps: %d succeeded, want exactly 1 (refused: %d)", wins, refused)
	}
	if refused != racers-1 {
		t.Fatalf("concurrent bootstraps: %d refused, want %d", refused, racers-1)
	}

	assertUserCount(t, pool, 1)
	assertTenantCount(t, pool, 1)
	assertOwnerMembershipCount(t, pool, 1)
}

// TestFirstRunOwnership_SocialSignInNeverMints proves the social path cannot
// grant first-run ownership. It has nowhere to carry the provisioning claim, so
// the only answer that keeps the two paths agreeing is that it never mints.
//
// To watch it go red: restore the `if len(memberships) == 0 && ...` branch in
// finishSocialLogin (internal/auth/social.go) that created a tenant and an
// owner membership.
func TestFirstRunOwnership_SocialSignInNeverMints(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	svc, _ := newAuthStack(pool)
	svc.SetBootstrapClaimSecret(testClaim)

	// A completely unclaimed install: zero users, zero tenants.
	assertUserCount(t, pool, 0)

	res, err := svc.SignInWithSocial(ctx, auth.SocialIdentity{
		Provider:      "google",
		Subject:       "google-subject-1",
		Email:         "first@example.com",
		EmailVerified: true,
		Name:          "First",
	}, makeCreateTenant(t, pool))
	if err != nil {
		t.Fatalf("social sign-in: %v", err)
	}
	if len(res.Memberships) != 0 {
		t.Fatalf("social sign-in on an unclaimed install minted %d membership(s): %+v", len(res.Memberships), res.Memberships)
	}
	if res.ActiveTenant != uuid.Nil {
		t.Fatalf("social sign-in on an unclaimed install set an active tenant: %s", res.ActiveTenant)
	}
	assertTenantCount(t, pool, 0)
	assertOwnerMembershipCount(t, pool, 0)
}

// TestFirstRunOwnership_SocialSignInStillWorksOnAClaimedInstall is the
// does-not-over-fire half of the social change: once the install has an owner,
// social sign-in resolves memberships exactly as before.
//
// To watch it go red: make finishSocialLogin return no memberships (drop the
// ListMembershipsForUser call).
func TestFirstRunOwnership_SocialSignInStillWorksOnAClaimedInstall(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	svc, _ := newAuthStack(pool)
	svc.SetBootstrapClaimSecret(testClaim)

	owner, err := svc.Bootstrap(ctx, auth.RegisterInput{
		Email:    "owner@example.com",
		Password: "a-very-strong-password",
		Name:     "Owner",
	}, testClaim)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	// The same person signs in with Google against the address they already own
	// and verified through the claim-bearing bootstrap.
	res, err := svc.SignInWithSocial(ctx, auth.SocialIdentity{
		Provider:      "google",
		Subject:       "google-subject-owner",
		Email:         "owner@example.com",
		EmailVerified: true,
		Name:          "Owner",
	}, makeCreateTenant(t, pool))
	if err != nil {
		t.Fatalf("social sign-in on a claimed install: %v", err)
	}
	if res.ActiveTenant != owner.ActiveTenant {
		t.Fatalf("social sign-in resolved tenant %s, want the owner's %s", res.ActiveTenant, owner.ActiveTenant)
	}
	if len(res.Memberships) != 1 {
		t.Fatalf("want the owner's single membership, got %+v", res.Memberships)
	}
	// Still exactly one organisation: signing in socially created nothing.
	assertTenantCount(t, pool, 1)
	assertOwnerMembershipCount(t, pool, 1)
}

// TestFirstRunOwnership_SelfServeCannotBurnTheOwnershipSlot proves an
// unauthenticated self-serve registration on an unclaimed install writes
// nothing. Taking the first-account slot and merely blocking it are the same
// loss to the operator: either way the person holding the claim can never
// bootstrap.
//
// To watch it go red: delete the zero-user early return at the top of
// RegisterSelfServe in internal/auth/register.go.
func TestFirstRunOwnership_SelfServeCannotBurnTheOwnershipSlot(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	svc, _ := newAuthStack(pool)
	svc.SetBootstrapClaimSecret(testClaim)

	// The generic response is returned (nil error), exactly as on a claimed
	// install, so the caller cannot tell the two apart.
	if err := svc.RegisterSelfServe(ctx, auth.RegisterInput{
		Email:    "squatter@example.com",
		Password: "a-very-strong-password",
		Name:     "Squatter",
	}, makeCreateTenant(t, pool)); err != nil {
		t.Fatalf("self-serve on an unclaimed install returned an error: %v", err)
	}
	assertUserCount(t, pool, 0)
	assertTenantCount(t, pool, 0)

	// The operator can still claim the install afterwards.
	if _, err := svc.Bootstrap(ctx, auth.RegisterInput{
		Email:    "owner@example.com",
		Password: "a-very-strong-password",
		Name:     "Owner",
	}, testClaim); err != nil {
		t.Fatalf("bootstrap after a self-serve attempt: %v", err)
	}
	assertUserCount(t, pool, 1)

	// And self-serve is open again now that the install has an owner: this is
	// the does-not-over-fire half.
	if err := svc.RegisterSelfServe(ctx, auth.RegisterInput{
		Email:    "later@example.com",
		Password: "a-very-strong-password",
		Name:     "Later",
	}, makeCreateTenant(t, pool)); err != nil {
		t.Fatalf("self-serve on a claimed install: %v", err)
	}
	assertUserCount(t, pool, 2)
}

// --- helpers ---------------------------------------------------------------

func isRegistrationClosed(err error) bool {
	de, ok := domain.AsDomain(err)
	return ok && de.Kind == domain.KindForbidden && de.Code == "registration_closed"
}

func assertRegistrationClosed(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("want a refusal, got success")
	}
	if !isRegistrationClosed(err) {
		t.Fatalf("want forbidden/registration_closed, got %v", err)
	}
}

func containsSecret(s string) bool { return strings.Contains(s, testClaim) }

// assertUserCount and assertTenantCount read through the app pool. Neither
// table is RLS-scoped, and the pool is connected as the non-superuser
// wpmgr_app role that every install runs as, so this is the same view the
// request path has.
func assertUserCount(t *testing.T, pool *db.Pool, want int) {
	t.Helper()
	assertCount(t, pool, "SELECT count(*) FROM users", want, "users")
}

func assertTenantCount(t *testing.T, pool *db.Pool, want int) {
	t.Helper()
	assertCount(t, pool, "SELECT count(*) FROM tenants", want, "tenants")
}

// assertOwnerMembershipCount counts owner memberships ACROSS every tenant,
// which no tenant-scoped connection can do: memberships is FORCE ROW LEVEL
// SECURITY and every policy on it is keyed on a scope this assertion
// deliberately declines to assume. connectAdmin is the documented way to
// observe from outside the app's RLS constraints; it is used only to check the
// outcome, never to produce it — every write under test went through the
// wpmgr_app role via the same tx helper the request path uses.
func assertOwnerMembershipCount(t *testing.T, pool *db.Pool, want int) {
	t.Helper()
	admin := connectAdmin(t, pool)
	defer admin.Close()
	assertCount(t, admin, "SELECT count(*) FROM memberships WHERE role = 'owner'", want, "owner memberships")
}

func assertCount(t *testing.T, pool *db.Pool, query string, want int, label string) {
	t.Helper()
	var got int
	if err := pool.QueryRow(context.Background(), query).Scan(&got); err != nil {
		t.Fatalf("count %s: %v", label, err)
	}
	if got != want {
		t.Fatalf("%s = %d, want %d", label, got, want)
	}
}
