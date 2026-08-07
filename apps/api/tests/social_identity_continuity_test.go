// social_identity_continuity_test.go: does (provider, subject, issuer) still
// resolve to the account the person already had?
//
// Every case here is the same question asked from a different side: an issuer
// string the operator edited, an identity row a migration never wrote, and a
// synthesised placeholder address that used to be derived from the issuer. All
// three used to end at a NEW account or a hard refusal, where the release
// before social sign-in silently signed the person straight in.
package tests

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/auth"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// noTenant is the createTenant callback for cases where the bootstrap is
// irrelevant: these tests are about WHICH user a sign-in resolves to, not about
// organisation creation, and the install always already has a user.
func noTenant(t *testing.T, pool *db.Pool) func(context.Context, string, string) (uuid.UUID, error) {
	return func(_ context.Context, _, slug string) (uuid.UUID, error) {
		return seedTenant(t, pool, slug+"-"+uuid.NewString()[:8]), nil
	}
}

// seedLegacyOIDCUser writes the exact row shape the release BEFORE social
// sign-in left behind: users.oidc_issuer / users.oidc_subject populated, no
// user_identities row at all, and email_verified_at NULL because that path
// never wrote it. This is what an install has for anyone who signed in through
// SSO during a rollback window, when m110's one-shot backfill had already run
// and will never run again.
func seedLegacyOIDCUser(t *testing.T, pool *db.Pool, email, issuer, subject string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pool.QueryRow(context.Background(),
		`INSERT INTO users (email, oidc_issuer, oidc_subject, name)
		 VALUES ($1, $2, $3, 'Legacy') RETURNING id`,
		email, issuer, subject).Scan(&id)
	if err != nil {
		t.Fatalf("seed legacy OIDC user: %v", err)
	}
	return id
}

func identityRowCount(t *testing.T, pool *db.Pool, userID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM user_identities WHERE user_id = $1`, userID).Scan(&n); err != nil {
		t.Fatalf("count identities: %v", err)
	}
	return n
}

func userCount(t *testing.T, pool *db.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM users`).Scan(&n); err != nil {
		t.Fatalf("count users: %v", err)
	}
	return n
}

// [2.9] An operator edits WPMGR_OIDC_ISSUER, because the company moved its IdP
// to a new hostname or someone added a trailing slash. The subject is unchanged and
// the human is unchanged, so the sign-in must land on the same account. With
// issuer in the identity key it landed on a brand new one, for EVERY SSO user
// at once, which is an install-wide lockout triggered by a config edit.
func TestSocialIdentity_IssuerChangeKeepsTheSameAccount(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	svc, _ := newAuthStack(pool)
	mk := noTenant(t, pool)

	first, err := svc.UpsertOIDCUser(ctx,
		"https://idp.acme.com", "corp-sub-1", "sarah@acme.com", true, "Sarah", mk)
	if err != nil {
		t.Fatalf("first sign-in: %v", err)
	}

	// Same IdP, same person, same subject, new URL.
	second, err := svc.UpsertOIDCUser(ctx,
		"https://login.acme.com", "corp-sub-1", "sarah@acme.com", true, "Sarah", mk)
	if err != nil {
		t.Fatalf("sign-in after the issuer URL changed must still work: %v", err)
	}

	if second.User.ID != first.User.ID {
		t.Fatalf("editing the issuer forked the account: was %s, now %s", first.User.ID, second.User.ID)
	}
	if n := userCount(t, pool); n != 1 {
		t.Fatalf("expected exactly 1 user after an issuer change, got %d", n)
	}
	if n := identityRowCount(t, pool, first.User.ID); n != 1 {
		t.Fatalf("expected exactly 1 identity row, got %d", n)
	}

	// The recorded issuer follows the current configuration, so the row is a
	// truthful record of where the person last came from.
	var issuer string
	if err := pool.QueryRow(ctx,
		`SELECT issuer FROM user_identities WHERE user_id = $1`, first.User.ID).Scan(&issuer); err != nil {
		t.Fatalf("read issuer: %v", err)
	}
	if issuer != "https://login.acme.com" {
		t.Fatalf("issuer = %q, want the issuer seen on the most recent sign-in", issuer)
	}
}

// [2.8] The rollback window. m110 backfilled once; anything the previous
// release wrote afterwards has legacy columns and no identity row. The new
// policy refuses to link onto a never-verified account, which is correct in
// general and catastrophic here: the account is never-verified only because the
// OLD SSO path never wrote email_verified_at. Every pre-existing SSO user on
// such an install is refused at the door.
func TestSocialIdentity_LegacyRowWithoutIdentitySignsInAndHeals(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	svc, _ := newAuthStack(pool)
	mk := noTenant(t, pool)

	legacyID := seedLegacyOIDCUser(t, pool, "sarah@acme.com", "https://idp.acme.com", "corp-sub-1")
	if n := identityRowCount(t, pool, legacyID); n != 0 {
		t.Fatalf("precondition: legacy user must start with no identity row, got %d", n)
	}

	res, err := svc.UpsertOIDCUser(ctx,
		"https://idp.acme.com", "corp-sub-1", "sarah@acme.com", true, "Sarah", mk)
	if err != nil {
		t.Fatalf("a pre-existing SSO user must still be able to sign in: %v", err)
	}
	if res.User.ID != legacyID {
		t.Fatalf("signed in as %s, want the pre-existing account %s", res.User.ID, legacyID)
	}
	if n := userCount(t, pool); n != 1 {
		t.Fatalf("expected 1 user, got %d (the sign-in forked a duplicate)", n)
	}

	// Healed forward: the missing row is written, so the next sign-in is an
	// ordinary identity hit and does not depend on the legacy columns at all.
	if n := identityRowCount(t, pool, legacyID); n != 1 {
		t.Fatalf("expected the missing identity row to be written, got %d", n)
	}

	again, err := svc.UpsertOIDCUser(ctx,
		"https://idp.acme.com", "corp-sub-1", "sarah@acme.com", true, "Sarah", mk)
	if err != nil {
		t.Fatalf("second sign-in: %v", err)
	}
	if again.User.ID != legacyID {
		t.Fatalf("second sign-in resolved to %s, want %s", again.User.ID, legacyID)
	}
	if n := identityRowCount(t, pool, legacyID); n != 1 {
		t.Fatalf("healing must be idempotent, got %d identity rows", n)
	}
}

// [2.8] + [2.26] The same rollback window for an IdP that returns no email
// claim. The old release synthesised <subject>@oidc.local; this one synthesised
// <subject>@<issuer-host>.oidc.invalid. With no email to reconcile on and no
// identity row, nothing connected the two and the person silently got a second
// account: the duplicate-account fork, with no way for them to notice.
func TestSocialIdentity_NoEmailClaimDoesNotForkALegacyAccount(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	svc, _ := newAuthStack(pool)
	mk := noTenant(t, pool)

	// The address the previous release minted for a no-email IdP.
	legacyID := seedLegacyOIDCUser(t, pool, "corp-sub-1@oidc.local", "https://idp.acme.com", "corp-sub-1")

	res, err := svc.UpsertOIDCUser(ctx, "https://idp.acme.com", "corp-sub-1", "", false, "", mk)
	if err != nil {
		t.Fatalf("a no-email IdP sign-in must resolve to the existing account: %v", err)
	}
	if res.User.ID != legacyID {
		t.Fatalf("signed in as %s, want %s", res.User.ID, legacyID)
	}
	if n := userCount(t, pool); n != 1 {
		t.Fatalf("expected 1 user, got %d (a synthetic address forked a duplicate account)", n)
	}
}

// [2.26] A no-email IdP creates a genuinely new account, and then the operator
// edits the issuer. The placeholder address must not move with the issuer, or
// the same person forks a second account on the next sign-in.
func TestSocialIdentity_SyntheticAddressSurvivesAnIssuerChange(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	svc, _ := newAuthStack(pool)
	mk := noTenant(t, pool)

	first, err := svc.UpsertOIDCUser(ctx, "https://idp.acme.com", "corp-sub-9", "", false, "", mk)
	if err != nil {
		t.Fatalf("first sign-in: %v", err)
	}
	second, err := svc.UpsertOIDCUser(ctx, "https://login.acme.com", "corp-sub-9", "", false, "", mk)
	if err != nil {
		t.Fatalf("sign-in after the issuer changed: %v", err)
	}
	if second.User.ID != first.User.ID {
		t.Fatalf("a no-email account forked on an issuer change: %s then %s", first.User.ID, second.User.ID)
	}
	if n := userCount(t, pool); n != 1 {
		t.Fatalf("expected 1 user, got %d", n)
	}
}

// The narrow case that must still REFUSE to guess. Two legacy rows share a
// subject under different issuers, which the old (oidc_issuer, oidc_subject)
// unique index allowed. Subject alone no longer identifies anyone here, so
// adopting either row would be a coin flip between two humans. The sign-in must
// fall through to the ordinary policy rather than pick one.
func TestSocialIdentity_AmbiguousLegacySubjectIsNeverAdopted(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	svc, _ := newAuthStack(pool)
	mk := noTenant(t, pool)

	a := seedLegacyOIDCUser(t, pool, "a@acme.com", "https://idp-a.acme.com", "shared-sub")
	b := seedLegacyOIDCUser(t, pool, "b@acme.com", "https://idp-b.acme.com", "shared-sub")

	// No email claim, so the policy cannot link either; it creates a distinct
	// account instead of silently adopting one of the two.
	res, err := svc.UpsertOIDCUser(ctx, "https://idp-a.acme.com", "shared-sub", "", false, "", mk)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.User.ID == a || res.User.ID == b {
		t.Fatalf("an ambiguous legacy subject must never be adopted, got %s", res.User.ID)
	}
}

// The status gate still applies to a healed legacy identity. Adopting a legacy
// row must not become a way around the check that a disabled user cannot sign
// in, which is the whole reason the gate was added to this path.
func TestSocialIdentity_HealedLegacyIdentityStillObeysTheStatusGate(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	svc, _ := newAuthStack(pool)
	mk := noTenant(t, pool)

	legacyID := seedLegacyOIDCUser(t, pool, "sarah@acme.com", "https://idp.acme.com", "corp-sub-1")
	if _, err := pool.Exec(ctx, `UPDATE users SET status = 'disabled' WHERE id = $1`, legacyID); err != nil {
		t.Fatalf("disable user: %v", err)
	}

	_, err := svc.UpsertOIDCUser(ctx,
		"https://idp.acme.com", "corp-sub-1", "sarah@acme.com", true, "Sarah", mk)
	if err == nil {
		t.Fatal("a disabled user must not sign in through a healed legacy identity")
	}
	if de, ok := domain.AsDomain(err); !ok || de.Code != "account_disabled" {
		t.Fatalf("want account_disabled, got %v", err)
	}
}

// Consumer providers never wrote the legacy columns, so they must never read
// them. A Google subject that happens to equal some corporate OIDC subject must
// not adopt that account.
func TestSocialIdentity_ConsumerProviderNeverAdoptsALegacyRow(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	repo := auth.NewRepo(pool)
	svc, _ := newAuthStack(pool)
	mk := noTenant(t, pool)

	legacyID := seedLegacyOIDCUser(t, pool, "sarah@acme.com", "https://idp.acme.com", "collide-1")

	res, err := svc.SignInWithSocial(ctx, auth.SocialIdentity{
		Provider: "google", Subject: "collide-1", Issuer: "https://accounts.google.com",
		Email: "someone@gmail.test", EmailVerified: true, Name: "Someone",
	}, mk)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.User.ID == legacyID {
		t.Fatal("a Google subject must never adopt a generic-OIDC legacy row")
	}

	// And the legacy user is untouched.
	if _, err := repo.GetUserByID(ctx, legacyID); err != nil {
		t.Fatalf("legacy user should still exist: %v", err)
	}
}
