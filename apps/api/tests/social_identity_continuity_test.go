// social_identity_continuity_test.go: does a returning person land on the
// account they already had, and does everyone else stay out of it?
//
// The end-to-end half of the rule unit-tested in internal/auth (which is where
// the policy itself is exercised, because this package sits out the unit CI
// lane). Here the questions are the ones only a real database answers: that the
// row is actually moved, that the repair is idempotent, that a REFUSED sign-in
// leaves no trace, and that a subject collision between two issuers cannot
// resolve to somebody else's account.
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
// organisation creation.
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

func identityIssuer(t *testing.T, pool *db.Pool, userID uuid.UUID) string {
	t.Helper()
	var issuer string
	if err := pool.QueryRow(context.Background(),
		`SELECT issuer FROM user_identities WHERE user_id = $1`, userID).Scan(&issuer); err != nil {
		t.Fatalf("read issuer: %v", err)
	}
	return issuer
}

func userCount(t *testing.T, pool *db.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM users`).Scan(&n); err != nil {
		t.Fatalf("count users: %v", err)
	}
	return n
}

// An operator moves the corporate IdP to a new hostname and DECLARES where it
// moved from. The subject is unchanged and the human is unchanged, so the
// sign-in must land on the same account, and the row must actually move, so the
// declaration can be withdrawn afterwards.
func TestSocialIdentity_DeclaredIssuerChangeKeepsTheSameAccount(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	svc, _ := newAuthStack(pool)
	mk := noTenant(t, pool)

	first, err := svc.UpsertOIDCUser(ctx,
		"https://idp.acme.com", "corp-sub-1", "sarah@acme.com", true, "Sarah", mk)
	if err != nil {
		t.Fatalf("first sign-in: %v", err)
	}

	// The declaration is what authorises the move. Without it the stored
	// identity belongs to an issuer this install no longer knows anything about;
	// see TestSocialIdentity_UndeclaredIssuerChangeDoesNotAdoptALegacyRow and
	// the collision test below for the strictness that buys.
	svc.SetPreviousOIDCIssuer("https://idp.acme.com")
	second, err := svc.UpsertOIDCUser(ctx,
		"https://login.acme.com", "corp-sub-1", "sarah@acme.com", true, "Sarah", mk)
	if err != nil {
		t.Fatalf("sign-in after a declared issuer change must work: %v", err)
	}
	if second.User.ID != first.User.ID {
		t.Fatalf("a declared issuer change forked the account: was %s, now %s", first.User.ID, second.User.ID)
	}
	if n := identityRowCount(t, pool, first.User.ID); n != 1 {
		t.Fatalf("expected exactly 1 identity row, got %d", n)
	}
	// MOVED, not merely matched: the declaration is meant to be temporary, so
	// the row has to carry the new issuer once the person has signed in.
	if got := identityIssuer(t, pool, first.User.ID); got != "https://login.acme.com" {
		t.Fatalf("identity issuer = %q, want the new one; the row was not migrated", got)
	}

	// AND IT IS ON THE RECORD. An identity changing issuer changes what a stored
	// credential means, so a move that nobody can see afterwards is not a
	// migration, it is a silent relaxation. Read through a superuser connection
	// because audit_log is tenant-scoped by RLS.
	admin := connectAdmin(t, pool)
	defer admin.Close()
	var moves int
	if err := admin.QueryRow(ctx,
		`SELECT count(*) FROM audit_log
		 WHERE metadata->>'event' = 'identity_issuer_migrated'
		   AND metadata->>'from_issuer' = 'https://idp.acme.com'
		   AND metadata->>'to_issuer' = 'https://login.acme.com'`).Scan(&moves); err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	if moves != 1 {
		t.Fatalf("expected exactly 1 audited issuer migration, got %d", moves)
	}

	// With the row moved, the declaration is no longer load-bearing.
	svc.SetPreviousOIDCIssuer("")
	third, err := svc.UpsertOIDCUser(ctx,
		"https://login.acme.com", "corp-sub-1", "sarah@acme.com", true, "Sarah", mk)
	if err != nil || third.User.ID != first.User.ID {
		t.Fatalf("after the move the sign-in must be an ordinary exact hit: %v", err)
	}
}

// A trailing slash is not a change of issuer and needs no declaration. This is
// the edit most likely to be made by accident, and the one whose blast radius
// is every SSO user on the install at once.
func TestSocialIdentity_CosmeticIssuerEditNeedsNoDeclaration(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	svc, _ := newAuthStack(pool)
	mk := noTenant(t, pool)

	first, err := svc.UpsertOIDCUser(ctx,
		"https://idp.acme.com", "corp-sub-1", "sarah@acme.com", true, "Sarah", mk)
	if err != nil {
		t.Fatalf("first sign-in: %v", err)
	}
	second, err := svc.UpsertOIDCUser(ctx,
		"https://idp.acme.com/", "corp-sub-1", "sarah@acme.com", true, "Sarah", mk)
	if err != nil {
		t.Fatalf("a trailing slash must not break sign-in: %v", err)
	}
	if second.User.ID != first.User.ID {
		t.Fatalf("a trailing slash forked the account: %s then %s", first.User.ID, second.User.ID)
	}
	if n := userCount(t, pool); n != 1 {
		t.Fatalf("expected 1 user, got %d", n)
	}
}

// THE TAKEOVER. Two issuers mint the same opaque subject for two different
// people. Nothing, declaration or not, may let the second one sign into the
// first one's account.
func TestSocialIdentity_SubjectCollisionAcrossIssuersIsNeverTheSameAccount(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	svc, _ := newAuthStack(pool)
	mk := noTenant(t, pool)

	alice, err := svc.UpsertOIDCUser(ctx,
		"https://idp.acme.com", "1000", "alice@acme.com", true, "Alice", mk)
	if err != nil {
		t.Fatalf("alice: %v", err)
	}

	// A different IdP, the same subject string, a different human with a
	// different verified address.
	bob, err := svc.UpsertOIDCUser(ctx,
		"https://idp.other-company.example", "1000", "bob@other-company.example", true, "Bob", mk)
	if err != nil {
		t.Fatalf("bob: %v", err)
	}
	if bob.User.ID == alice.User.ID {
		t.Fatal("a subject collision between two issuers signed one person into another's account")
	}
}

// The rollback window. m110 backfilled once; anything the previous release
// wrote afterwards has legacy columns and no identity row. The new policy
// refuses to link onto a never-verified account, which is correct in general and
// catastrophic here: the account is never-verified only because the OLD SSO path
// never wrote email_verified_at.
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

// The same rollback window for an IdP that returns no email claim: there is no
// address to reconcile on, so the identity is the only thing that can connect
// the person to their account.
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

// A REFUSED SIGN-IN MUST LEAVE THE DATABASE AS IT FOUND IT. The repair writes a
// permanent identity binding, so running it while loading the facts meant a
// disabled account still acquired one on its way out of the door.
func TestSocialIdentity_RefusedSignInWritesNoIdentity(t *testing.T) {
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
	if n := identityRowCount(t, pool, legacyID); n != 0 {
		t.Fatalf("a refused sign-in wrote %d identity rows; authentication state must not move on a refusal", n)
	}
}

// The same, for the issuer migration: a declared move must not be applied to an
// account the policy then refuses.
func TestSocialIdentity_RefusedSignInDoesNotMigrateTheIssuer(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	svc, _ := newAuthStack(pool)
	mk := noTenant(t, pool)

	first, err := svc.UpsertOIDCUser(ctx,
		"https://idp.acme.com", "corp-sub-1", "sarah@acme.com", true, "Sarah", mk)
	if err != nil {
		t.Fatalf("first sign-in: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET status = 'disabled' WHERE id = $1`, first.User.ID); err != nil {
		t.Fatalf("disable user: %v", err)
	}

	svc.SetPreviousOIDCIssuer("https://idp.acme.com")
	if _, err := svc.UpsertOIDCUser(ctx,
		"https://login.acme.com", "corp-sub-1", "sarah@acme.com", true, "Sarah", mk); err == nil {
		t.Fatal("a disabled user must not sign in across a declared issuer change")
	}
	if got := identityIssuer(t, pool, first.User.ID); got != "https://idp.acme.com" {
		t.Fatalf("identity issuer = %q: a refused sign-in migrated the row", got)
	}
}

// An undeclared issuer change must not adopt a legacy row either. The repair
// path has to be exactly as strict as the authenticating lookup.
func TestSocialIdentity_UndeclaredIssuerChangeDoesNotAdoptALegacyRow(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	svc, _ := newAuthStack(pool)
	mk := noTenant(t, pool)

	legacyID := seedLegacyOIDCUser(t, pool, "corp-sub-1@oidc.local", "https://idp.acme.com", "corp-sub-1")

	// No email claim, so nothing else can connect this sign-in to that account.
	res, err := svc.UpsertOIDCUser(ctx, "https://idp.other-company.example", "corp-sub-1", "", false, "", mk)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.User.ID == legacyID {
		t.Fatal("a legacy row was adopted across an undeclared issuer change")
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

// A provider that returns no subject is broken, not anonymous: an empty subject
// is a shared key that everybody would sign into.
func TestSocialIdentity_EmptySubjectIsRefused(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	svc, _ := newAuthStack(pool)
	mk := noTenant(t, pool)

	_, err := svc.UpsertOIDCUser(ctx, "https://idp.acme.com", "", "sarah@acme.com", true, "Sarah", mk)
	if err == nil {
		t.Fatal("an empty subject must never create or resolve an account")
	}
	if de, ok := domain.AsDomain(err); !ok || de.Code != "social_subject_missing" {
		t.Fatalf("want social_subject_missing, got %v", err)
	}
	if n := userCount(t, pool); n != 0 {
		t.Fatalf("expected no user to be created, got %d", n)
	}
}
