package auth

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// These tests are the whole security argument for social sign-in. decideSocial
// is a pure function precisely so every cell of the matrix can be asserted
// here, including the ones that must REFUSE, which are the cells an
// implementation is most likely to get wrong by being helpful.

func verifiedUser() *User {
	t := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return &User{ID: uuid.New(), Email: "sarah@acme.com", Status: "active", EmailVerifiedAt: &t}
}

func unverifiedUser() *User {
	// What registration leaves behind: a row exists, nobody has proven they own
	// the address, and the account is parked in 'pending'.
	return &User{ID: uuid.New(), Email: "sarah@acme.com", Status: "pending"}
}

func googleID(verified bool) SocialIdentity {
	return SocialIdentity{
		Provider: "google", Subject: "google-sub-1",
		Email: "sarah@acme.com", EmailVerified: verified, Name: "Sarah",
	}
}

func codeOf(t *testing.T, err error) string {
	t.Helper()
	de, ok := domain.AsDomain(err)
	if !ok {
		t.Fatalf("expected a domain error, got %v", err)
	}
	return de.Code
}

func TestDecideSocial_KnownIdentitySignsIn(t *testing.T) {
	got, err := decideSocial(googleID(true), verifiedUser(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != socialSignIn {
		t.Fatalf("got action %v, want socialSignIn", got)
	}
}

// A known identity authenticates on (provider, subject) alone. If a provider
// later reports a different, even unverified, address for the SAME identity,
// that must not change who is signing in and must not lock them out.
func TestDecideSocial_KnownIdentityIgnoresEmailEntirely(t *testing.T) {
	in := googleID(false)
	in.Email = "someone-else@elsewhere.test"
	got, err := decideSocial(in, verifiedUser(), nil)
	if err != nil {
		t.Fatalf("a known identity must sign in regardless of the email reported: %v", err)
	}
	if got != socialSignIn {
		t.Fatalf("got %v, want socialSignIn", got)
	}
}

func TestDecideSocial_NewIdentityNewEmailCreates(t *testing.T) {
	got, err := decideSocial(googleID(true), nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != socialCreate {
		t.Fatalf("got %v, want socialCreate", got)
	}
}

func TestDecideSocial_BothVerifiedLinks(t *testing.T) {
	got, err := decideSocial(googleID(true), nil, verifiedUser())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != socialLinkExisting {
		t.Fatalf("got %v, want socialLinkExisting", got)
	}
}

// THE TAKEOVER. An attacker registers the victim's address with a password;
// registration does not require proving control, so the row sits unverified.
// The victim later signs in with Google, which truthfully reports the address
// as verified. Linking here would bind the victim's identity to an account
// whose password the attacker chose.
func TestDecideSocial_RefusesLinkingToUnverifiedLocalAccount(t *testing.T) {
	_, err := decideSocial(googleID(true), nil, unverifiedUser())
	if err == nil {
		t.Fatal("linking a verified provider identity to an UNVERIFIED local account is an account takeover; it must be refused")
	}
	// 'pending' is caught by the status gate, which is the correct refusal and
	// the one a real registration produces.
	if c := codeOf(t, err); c != "email_not_verified" && c != "social_link_requires_verification" {
		t.Fatalf("unexpected refusal code %q", c)
	}
}

// The same shape with an ACTIVE but never-verified account, which the
// first-user bootstrap path can produce. The status gate does not catch this
// one, so the explicit EmailVerified() check is what has to.
func TestDecideSocial_RefusesLinkingToActiveButNeverVerifiedAccount(t *testing.T) {
	u := unverifiedUser()
	u.Status = "active"
	u.EmailVerifiedAt = nil

	_, err := decideSocial(googleID(true), nil, u)
	if err == nil {
		t.Fatal("an active account that was never email-verified must not absorb a social identity")
	}
	if c := codeOf(t, err); c != "social_link_requires_verification" {
		t.Fatalf("got code %q, want social_link_requires_verification", c)
	}
}

// An unverified provider email is worthless for linking AND for creating. If it
// could create, somebody could squat an address they do not own and collect the
// real owner's later sign-in.
func TestDecideSocial_RefusesUnverifiedProviderEmail(t *testing.T) {
	for _, tc := range []struct {
		name      string
		emailUser *User
	}{
		{"with no local account", nil},
		{"with a verified local account", verifiedUser()},
		{"with an unverified local account", unverifiedUser()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decideSocial(googleID(false), nil, tc.emailUser)
			if err == nil {
				t.Fatal("an unverified provider email must never link or create")
			}
			if c := codeOf(t, err); c != "social_email_unverified" {
				t.Fatalf("got code %q, want social_email_unverified", c)
			}
		})
	}
}

func TestDecideSocial_RefusesEmptyProviderEmail(t *testing.T) {
	in := googleID(true)
	in.Email = ""
	if _, err := decideSocial(in, nil, nil); err == nil {
		t.Fatal("an empty email must not create an account")
	}
}

// Gap A. The password path has always refused these two states. This path
// refused neither, so an administrator could disable a user and that user could
// still sign in through SSO.
func TestDecideSocial_StatusGateAppliesToKnownIdentities(t *testing.T) {
	for _, status := range []string{"disabled", "pending"} {
		t.Run(status, func(t *testing.T) {
			u := verifiedUser()
			u.Status = status
			if _, err := decideSocial(googleID(true), u, nil); err == nil {
				t.Fatalf("a %q account must not be able to sign in via a linked identity", status)
			}
		})
	}
}

func TestDecideSocial_StatusGateAppliesBeforeLinking(t *testing.T) {
	u := verifiedUser()
	u.Status = "disabled"
	_, err := decideSocial(googleID(true), nil, u)
	if err == nil {
		t.Fatal("a disabled account must not absorb a new identity")
	}
	if c := codeOf(t, err); c != "account_disabled" {
		t.Fatalf("got code %q, want account_disabled", c)
	}
}

// GitHub has no email_verified claim, so its adapter derives the flag from
// /user/emails. The policy must treat a false from that derivation exactly as
// it treats a false from Google's claim: this asserts the policy is
// provider-agnostic and cannot drift per provider.
func TestDecideSocial_IsProviderAgnostic(t *testing.T) {
	gh := SocialIdentity{Provider: "github", Subject: "gh-1", Email: "sarah@acme.com"}
	if _, err := decideSocial(gh, nil, verifiedUser()); err == nil {
		t.Fatal("an unverified GitHub email must be refused exactly like an unverified Google one")
	}
	gh.EmailVerified = true
	got, err := decideSocial(gh, nil, verifiedUser())
	if err != nil || got != socialLinkExisting {
		t.Fatalf("a verified GitHub email against a verified account should link: action=%v err=%v", got, err)
	}
}

// Refusal messages reach a person mid sign-in, so they must say what to do
// next. A bare "forbidden" leaves someone stuck with no path forward.
func TestDecideSocial_RefusalsAreActionable(t *testing.T) {
	_, err := decideSocial(googleID(true), nil, &User{
		ID: uuid.New(), Email: "sarah@acme.com", Status: "active",
	})
	de, _ := domain.AsDomain(err)
	if de == nil || de.Message == "" {
		t.Fatal("refusal must carry a message")
	}
	for _, want := range []string{"password", "Google"} {
		if !contains(de.Message, want) {
			t.Errorf("refusal message should mention %q so the user knows what to do: %q", want, de.Message)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
