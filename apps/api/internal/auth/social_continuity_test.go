package auth

import (
	"testing"

	"github.com/google/uuid"
)

// The pure half of identity continuity. adoptableLegacyUser and
// syntheticEmailDomain decide, between them, whether a returning person lands on
// the account they already had, so both are kept free of database work for the
// same reason decideSocial is: this is where the account is won or lost.

// An account's own address must not be a function of an environment variable.
// Deriving the placeholder domain from the issuer host meant repointing
// WPMGR_OIDC_ISSUER changed the address a given subject would be minted with, so
// the same human returned and silently got a second account with nothing to
// reconcile the two on.
func TestSyntheticEmailDomain_DoesNotMoveWithTheIssuer(t *testing.T) {
	base := SocialIdentity{Provider: "oidc", Subject: "corp-1"}

	var seen string
	for _, issuer := range []string{
		"https://idp.acme.com",
		"https://idp.acme.com/",      // a trailing slash
		"https://login.acme.com",     // the IdP moved hostname
		"http://idp.acme.com:8443/x", // scheme and port edits
		"",                           // never configured
	} {
		in := base
		in.Issuer = issuer
		got := syntheticEmailDomain(in)
		if seen == "" {
			seen = got
			continue
		}
		if got != seen {
			t.Fatalf("issuer %q produced domain %q, want %q: an issuer edit must not change the address", issuer, got, seen)
		}
	}
}

// The placeholder must stay obviously non-routable, so it can never collide
// with, or squat, an address somebody actually owns.
func TestSyntheticEmailDomain_StaysUnroutableAndProviderScoped(t *testing.T) {
	for _, tc := range []struct{ provider, want string }{
		{"oidc", "oidc.invalid"},
		{"google", "google.invalid"},
		{"", "sso.invalid"},
	} {
		got := syntheticEmailDomain(SocialIdentity{Provider: tc.provider, Issuer: "https://idp.acme.com"})
		if got != tc.want {
			t.Errorf("provider %q: got %q, want %q", tc.provider, got, tc.want)
		}
	}
}

// Two providers cannot be given the same placeholder for the same subject,
// which is the collision the issuer-scoped version was reaching for.
func TestSyntheticEmailDomain_SeparatesProviders(t *testing.T) {
	a := syntheticEmailDomain(SocialIdentity{Provider: "oidc"})
	b := syntheticEmailDomain(SocialIdentity{Provider: "github"})
	if a == b {
		t.Fatalf("two providers share the placeholder domain %q", a)
	}
}

func legacyUser(issuer string) User {
	return User{ID: uuid.New(), Email: "sarah@acme.com", Status: "active", OIDCIssuer: issuer, OIDCSubject: "corp-1"}
}

// Exactly one holder, or nobody. There is no safe tiebreak when two people
// share a subject, so ambiguity must resolve to "no match" rather than to a
// guess, which would hand one of them the other's account.
func TestAdoptableLegacyUser_RefusesToGuess(t *testing.T) {
	if _, ok := adoptableLegacyUser(nil); ok {
		t.Fatal("no holders must not adopt")
	}

	one := legacyUser("https://idp.acme.com")
	got, ok := adoptableLegacyUser([]User{one})
	if !ok || got.ID != one.ID {
		t.Fatalf("a single unambiguous holder must be adopted: ok=%v", ok)
	}

	two := []User{legacyUser("https://idp-a.acme.com"), legacyUser("https://idp-b.acme.com")}
	if _, ok := adoptableLegacyUser(two); ok {
		t.Fatal("a subject held by two users identifies neither; adopting one is a coin flip between two humans")
	}
}

// The legacy users.oidc_* mirror is a courtesy to a rollback that may never
// happen. Writing it blind made that courtesy fatal: the column pair is unique,
// so a taken slot failed the whole sign-in with a duplicate-key error instead of
// simply declining to mirror.
func TestLegacySlotTaken_OnlyWhenTheExactPairIsHeld(t *testing.T) {
	in := SocialIdentity{Provider: "oidc", Subject: "corp-1", Issuer: "https://idp-a.acme.com"}

	if legacySlotTaken(in, nil) {
		t.Fatal("no holders means the slot is free")
	}
	if !legacySlotTaken(in, []User{legacyUser("https://idp-a.acme.com")}) {
		t.Fatal("the exact (issuer, subject) pair is held, so the mirror must be skipped")
	}
	// A holder under a DIFFERENT issuer does not collide on the unique index, so
	// the mirror is still safe to write and still useful on a rollback.
	if legacySlotTaken(in, []User{legacyUser("https://idp-b.acme.com")}) {
		t.Fatal("a different issuer does not occupy this slot")
	}
}
