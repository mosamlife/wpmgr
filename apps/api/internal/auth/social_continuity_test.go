package auth

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// Identity continuity across an issuer change, and the takeover that a careless
// version of it would open.
//
// These live in internal/auth, next to the policy, because this is the package
// CI actually runs: apps/api/tests is container-based and sits out the unit
// lane, so behaviour asserted only there is compile-checked and nothing more.
// The rule this file exercises is the one that decides whose account a sign-in
// lands on, which is not a rule to leave unexecuted.

const (
	acme    = "https://idp.acme.com"
	acmeNew = "https://login.acme.com"
)

func oidcIn(issuer, subject string) SocialIdentity {
	return SocialIdentity{Provider: "oidc", Subject: subject, Issuer: issuer, Email: "sarah@acme.com", EmailVerified: true}
}

func storedIdentity(userID uuid.UUID, provider, subject, issuer string) Identity {
	return Identity{UserID: userID, Provider: provider, Subject: subject, Issuer: issuer}
}

// THE TAKEOVER THIS KEY EXISTS TO PREVENT. A subject is unique only inside the
// issuer that minted it, so two identity providers can legitimately mint the
// same opaque string for two different people. Matching on (provider, subject)
// alone turns that collision into a complete, silent sign-in as somebody else.
// Without an explicit declaration from the operator, a stored identity under a
// different issuer must not match at all.
func TestMatchStoredIdentity_DifferentIssuerNeverMatchesByDefault(t *testing.T) {
	alice := uuid.New()
	stored := []Identity{storedIdentity(alice, "oidc", "123", acme)}

	// Bob arrives from a DIFFERENT IdP carrying the same subject string.
	got, kind := matchStoredIdentity(oidcIn("https://idp.other-company.example", "123"), "", stored)
	if kind != matchNone {
		t.Fatalf("a subject minted by another issuer must not resolve to %s (kind=%d)", got.UserID, kind)
	}
}

// Even with a previous issuer declared, the declaration names ONE issuer. Any
// other issuer is still a stranger.
func TestMatchStoredIdentity_DeclarationAuthorisesOnlyTheDeclaredIssuer(t *testing.T) {
	stored := []Identity{storedIdentity(uuid.New(), "oidc", "123", acme)}

	if _, kind := matchStoredIdentity(oidcIn("https://attacker.example", "123"), acmeNew, stored); kind != matchNone {
		t.Fatal("an issuer that is neither the current nor the declared previous one must not match")
	}
}

// The ordinary path: the issuer that signed the token is the one on the row.
func TestMatchStoredIdentity_ExactIssuerMatches(t *testing.T) {
	alice := uuid.New()
	stored := []Identity{storedIdentity(alice, "oidc", "123", acme)}

	got, kind := matchStoredIdentity(oidcIn(acme, "123"), "", stored)
	if kind != matchExact || got.UserID != alice {
		t.Fatalf("exact issuer must match: kind=%d user=%s", kind, got.UserID)
	}
}

// A trailing slash or a change of case is not a change of issuer, and needs no
// operator involvement. Nobody runs two identity providers on one hostname
// distinguished only by whether the URL ends in "/".
func TestMatchStoredIdentity_CosmeticIssuerDifferenceIsNotAChange(t *testing.T) {
	alice := uuid.New()
	for _, stored := range []string{
		"https://idp.acme.com/",
		"https://IDP.Acme.com",
		"https://idp.acme.com",
	} {
		got, kind := matchStoredIdentity(oidcIn(acme, "123"), "",
			[]Identity{storedIdentity(alice, "oidc", "123", stored)})
		if kind != matchExact || got.UserID != alice {
			t.Errorf("stored issuer %q must still be the same issuer: kind=%d", stored, kind)
		}
	}
}

// A path difference is NOT cosmetic: Dex, Keycloak and Auth0 all put the realm
// in the path, so two paths on one host are two populations of users.
func TestMatchStoredIdentity_PathIsPartOfTheIssuer(t *testing.T) {
	stored := []Identity{storedIdentity(uuid.New(), "oidc", "123", "https://sso.acme.com/realms/staff")}

	if _, kind := matchStoredIdentity(oidcIn("https://sso.acme.com/realms/contractors", "123"), "", stored); kind != matchNone {
		t.Fatal("two realms on one host are two issuers")
	}
}

// The declared migration: the operator has said "identities under that issuer
// are the same people", so the single candidate is matched, and reported as a
// migration so the caller moves the row and writes an audit entry rather than
// relaxing the lookup forever.
func TestMatchStoredIdentity_DeclaredPreviousIssuerMigratesOnce(t *testing.T) {
	alice := uuid.New()
	stored := []Identity{storedIdentity(alice, "oidc", "123", acme)}

	got, kind := matchStoredIdentity(oidcIn(acmeNew, "123"), acme, stored)
	if kind != matchIssuerMigrated || got.UserID != alice {
		t.Fatalf("a declared issuer change must migrate the identity: kind=%d user=%s", kind, got.UserID)
	}
	if got.Issuer != acme {
		t.Fatalf("the caller needs the issuer it was stored under to move it, got %q", got.Issuer)
	}
}

// The relaxation is for the operator-configured issuer only. Google and GitHub
// mint a constant issuer, so a mismatch there is never a config change; it can
// only be two different people.
func TestMatchStoredIdentity_ConsumerProvidersNeverRelax(t *testing.T) {
	stored := []Identity{storedIdentity(uuid.New(), "google", "123", "https://accounts.google.com")}

	in := SocialIdentity{Provider: "google", Subject: "123", Issuer: "https://accounts.evil.example", EmailVerified: true}
	if _, kind := matchStoredIdentity(in, "https://accounts.google.com", stored); kind != matchNone {
		t.Fatal("a consumer provider must never match across issuers, declaration or not")
	}
}

// Ambiguity refuses to guess, and it must do so on the AUTHENTICATING lookup,
// not only in the legacy repair path. Two rows that fold onto the current
// issuer identify nobody, and picking one hands a person another's account.
func TestMatchStoredIdentity_AmbiguityRefusesToGuess(t *testing.T) {
	stored := []Identity{
		storedIdentity(uuid.New(), "oidc", "123", acme),
		storedIdentity(uuid.New(), "oidc", "123", acme+"/"),
	}
	if _, kind := matchStoredIdentity(oidcIn(acme, "123"), "", stored); kind != matchNone {
		t.Fatal("two candidate rows identify nobody; there is no safe tiebreak")
	}
}

// An exact hit always wins over a migration candidate, so a moved-but-not-yet-
// migrated row can never displace the identity that is actually current.
func TestMatchStoredIdentity_ExactWinsOverAMigrationCandidate(t *testing.T) {
	current, old := uuid.New(), uuid.New()
	stored := []Identity{
		storedIdentity(old, "oidc", "123", acme),
		storedIdentity(current, "oidc", "123", acmeNew),
	}
	got, kind := matchStoredIdentity(oidcIn(acmeNew, "123"), acme, stored)
	if kind != matchExact || got.UserID != current {
		t.Fatalf("the row under the current issuer must win: kind=%d user=%s", kind, got.UserID)
	}
}

// Rows for another subject or another provider are not candidates, whatever the
// query handed over.
func TestMatchStoredIdentity_IgnoresRowsThatAreNotThisIdentity(t *testing.T) {
	stored := []Identity{
		storedIdentity(uuid.New(), "oidc", "999", acme),
		storedIdentity(uuid.New(), "github", "123", acme),
	}
	if _, kind := matchStoredIdentity(oidcIn(acme, "123"), acme, stored); kind != matchNone {
		t.Fatal("only rows for this exact (provider, subject) are candidates")
	}
}

func legacyUser(issuer, subject string) User {
	return User{ID: uuid.New(), Email: "sarah@acme.com", Status: "active", OIDCIssuer: issuer, OIDCSubject: subject}
}

// The pre-m110 repair path must apply EXACTLY the rule above. A repair that
// matched more loosely than the authenticating lookup would just move the
// takeover one query along.
func TestMatchLegacyUser_AppliesTheSameIssuerRule(t *testing.T) {
	alice := legacyUser(acme, "123")

	if u, kind := matchLegacyUser(oidcIn(acme, "123"), "", []User{alice}); kind != matchExact || u.ID != alice.ID {
		t.Fatalf("the exact legacy pair must be adopted: kind=%d", kind)
	}
	if _, kind := matchLegacyUser(oidcIn(acmeNew, "123"), "", []User{alice}); kind != matchNone {
		t.Fatal("a legacy row under another issuer must not be adopted without a declaration")
	}
	if u, kind := matchLegacyUser(oidcIn(acmeNew, "123"), acme, []User{alice}); kind != matchIssuerMigrated || u.ID != alice.ID {
		t.Fatalf("a declared issuer change must adopt the legacy row: kind=%d", kind)
	}
}

// Two legacy users sharing a subject under two issuers, which the old
// (oidc_issuer, oidc_subject) unique index permitted. With a declaration in
// play they are both candidates and neither may be adopted.
func TestMatchLegacyUser_AmbiguousSubjectIsNeverAdopted(t *testing.T) {
	a := legacyUser("https://idp-a.acme.com", "shared")
	b := legacyUser("https://idp-b.acme.com", "shared")

	// Arriving from a third issuer with idp-a declared: only a is a candidate,
	// so this one resolves.
	if _, kind := matchLegacyUser(oidcIn(acmeNew, "shared"), "https://idp-a.acme.com", []User{a, b}); kind != matchIssuerMigrated {
		t.Fatalf("a single declared candidate is not ambiguous: kind=%d", kind)
	}
	// Both rows fold onto the current issuer: nobody is identified.
	c := legacyUser(acme, "shared")
	d := legacyUser(acme+"/", "shared")
	if _, kind := matchLegacyUser(oidcIn(acme, "shared"), "", []User{c, d}); kind != matchNone {
		t.Fatal("two legacy holders identify neither of them")
	}
}

// The legacy columns were only ever written by the generic OIDC path, so a
// consumer provider must never read them: a match could only be a subject
// collision between two different people.
func TestMatchLegacyUser_ConsumerProviderNeverReadsLegacyColumns(t *testing.T) {
	in := SocialIdentity{Provider: "google", Subject: "123", Issuer: "https://accounts.google.com", EmailVerified: true}
	if _, kind := matchLegacyUser(in, acme, []User{legacyUser("https://accounts.google.com", "123")}); kind != matchNone {
		t.Fatal("a Google sign-in must never adopt a generic-OIDC legacy row")
	}
}

// A REFUSED SIGN-IN MUST NOT MUTATE AUTHENTICATION STATE. The repair writes a
// permanent identity binding, so it may only run for a sign-in the policy
// actually allowed. The service here carries a nil repo on purpose: reaching
// the database at all would panic, which is the assertion.
func TestRepairIdentity_WritesNothingUnlessTheSignInWasAllowed(t *testing.T) {
	s := &Service{}
	user := legacyUser(acme, "123")
	facts := socialFacts{identityUser: &user, match: matchIssuerMigrated, storedIssuer: acme, fromLegacy: true}

	for _, action := range []socialAction{socialLinkExisting, socialCreate} {
		s.repairIdentity(context.Background(), action, oidcIn(acmeNew, "123"), facts)
	}
	// And with nothing matched there is nothing to repair either.
	s.repairIdentity(context.Background(), socialSignIn, oidcIn(acmeNew, "123"), socialFacts{})
}

// TestTouchIssuer_FindsTheRowTheStampMustUpdate is the other half of
// repairIdentity: once the repair has run, the login stamp has to look the row
// up under the issuer it now lives under, and the stamp is an EXACT match on
// (provider, subject, issuer).
//
// The case that was wrong is the fold. matchStoredIdentity treats a case or
// trailing-slash difference as no difference, so the sign-in is correctly
// recognised through the stored spelling and repairIdentity deliberately writes
// nothing, because there is nothing to repair. The stamp then went looking for
// the INBOUND spelling and matched no rows at all: last_login_at stayed null for
// the life of that identity, the provider's current address was never recorded,
// and every later sign-in repeated the same two-query miss.
//
// The two repair paths are the opposite case and must not be folded in with it.
// Both write the row under the issuer that just signed the token, so stamping
// those under the stored one would miss for the very same reason.
func TestTouchIssuer_FindsTheRowTheStampMustUpdate(t *testing.T) {
	cases := []struct {
		name  string
		facts socialFacts
		in    SocialIdentity
		want  string
	}{
		{
			// The ordinary path: every issuer in play is the same string.
			name:  "exact match stamps the issuer that signed",
			facts: socialFacts{match: matchExact, storedIssuer: acme},
			in:    oidcIn(acme, "123"),
			want:  acme,
		},
		{
			// THE ONE THAT WAS BROKEN. Same issuer, different spelling; the row
			// keeps the spelling it was stored with.
			name:  "cosmetic fold stamps the stored spelling",
			facts: socialFacts{match: matchExact, storedIssuer: "https://IDP.acme.com"},
			in:    oidcIn("https://idp.acme.com/", "123"),
			want:  "https://IDP.acme.com",
		},
		{
			// The row moved to the new issuer, so the stamp follows it.
			name:  "a migrated issuer stamps where the row moved to",
			facts: socialFacts{match: matchIssuerMigrated, storedIssuer: acme},
			in:    oidcIn(acmeNew, "123"),
			want:  acmeNew,
		},
		{
			// Adoption INSERTS under the current issuer, whatever the legacy
			// columns said.
			name:  "an adopted legacy row stamps the current issuer",
			facts: socialFacts{match: matchExact, storedIssuer: acme, fromLegacy: true},
			in:    oidcIn(acmeNew, "123"),
			want:  acmeNew,
		},
		{
			// Consumer providers mint a constant issuer and store nothing here.
			name:  "no stored issuer falls back to the inbound one",
			facts: socialFacts{match: matchExact},
			in:    SocialIdentity{Provider: "google", Subject: "123", Issuer: googleIssuer},
			want:  googleIssuer,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.facts.touchIssuer(tc.in); got != tc.want {
				t.Errorf("touchIssuer = %q, want %q; the stamp is an exact match, so a wrong issuer updates no rows at all", got, tc.want)
			}
		})
	}
}

// The legacy users.oidc_* mirror is a courtesy to a rollback that may never
// happen. Writing it blind makes that courtesy fatal: the column pair is
// unique, so a taken slot fails the whole sign-in with a duplicate-key error
// instead of simply declining to mirror.
func TestLegacySlotTaken_OnlyWhenTheExactPairIsHeld(t *testing.T) {
	in := oidcIn(acme, "123")

	if legacySlotTaken(in, nil) {
		t.Fatal("no holders means the slot is free")
	}
	if !legacySlotTaken(in, []User{legacyUser(acme, "123")}) {
		t.Fatal("the exact (issuer, subject) pair is held, so the mirror must be skipped")
	}
	if !legacySlotTaken(in, []User{legacyUser(acme+"/", "123")}) {
		t.Fatal("a cosmetic issuer difference still occupies the same unique slot")
	}
	if legacySlotTaken(in, []User{legacyUser(acmeNew, "123")}) {
		t.Fatal("a different issuer does not occupy this slot")
	}
	if legacySlotTaken(in, []User{legacyUser(acme, "999")}) {
		t.Fatal("a different subject does not occupy this slot")
	}
}

// The placeholder minted for an IdP that returns no email must stay obviously
// non-routable and must differ per issuer, so a subject collision between two
// IdPs cannot become a unique-email failure at account creation.
func TestSyntheticEmailDomain_UnroutableAndIssuerScoped(t *testing.T) {
	a := syntheticEmailDomain(SocialIdentity{Provider: "oidc", Issuer: acme})
	b := syntheticEmailDomain(SocialIdentity{Provider: "oidc", Issuer: "https://idp.other.example"})
	if a == b {
		t.Fatalf("two issuers share the placeholder domain %q", a)
	}
	for _, got := range []string{a, b, syntheticEmailDomain(SocialIdentity{Provider: "oidc"})} {
		if len(got) < len(".invalid") || got[len(got)-len(".invalid"):] != ".invalid" {
			t.Fatalf("placeholder domain %q must sit under the reserved .invalid TLD", got)
		}
	}
}
