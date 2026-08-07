package auth

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The linking rule stays as it is: a provider-verified address may attach
// itself to a locally verified account with nobody signed in. What must never
// go back to being true is that it happens SILENTLY. These tests hold the
// notification to the two things that make it worth sending: it reaches the
// address this install verified, and it names the provider and the time.

// enqueued is one captured Enqueue call.
type enqueued struct {
	tenantID   uuid.UUID
	recipients []string
	template   string
	data       map[string]any
}

// fakeEnqueuer is an auth.EmailEnqueuer double recording every call.
type fakeEnqueuer struct{ calls []enqueued }

func (f *fakeEnqueuer) Enqueue(_ context.Context, tenantID uuid.UUID, recipients []string, template string, data map[string]any) error {
	f.calls = append(f.calls, enqueued{tenantID, recipients, template, data})
	return nil
}

func notifyService(t *testing.T) (*Service, *fakeEnqueuer) {
	t.Helper()
	f := &fakeEnqueuer{}
	s := &Service{}
	s.SetMailer(f, "https://manage.example.com/", nil)
	return s, f
}

func TestSignInMethodAddedNotifiesTheLocalAddress(t *testing.T) {
	s, f := notifyService(t)
	local := User{ID: uuid.New(), Email: "sarah@acme.com", Name: "Sarah", Status: "active"}

	s.sendSignInMethodAdded(context.Background(), local, googleID(true))

	if len(f.calls) != 1 {
		t.Fatalf("attaching a sign-in method must send exactly one notification, sent %d", len(f.calls))
	}
	call := f.calls[0]
	if call.template != "sign_in_method_added" {
		t.Errorf("template = %q", call.template)
	}
	if len(call.recipients) != 1 || call.recipients[0] != "sarah@acme.com" {
		t.Errorf("recipients = %v, want the local account address", call.recipients)
	}
	if got := call.data["Provider"]; got != "Google" {
		t.Errorf("Provider = %v, want the provider named so the reader can compare it with what they did", got)
	}
	if when, _ := call.data["When"].(string); when == "" {
		t.Error("the notice must carry a time; without one the reader cannot tell it from something they did last year")
	}
	if url, _ := call.data["SecurityURL"].(string); url != "https://manage.example.com/settings/security" {
		t.Errorf("SecurityURL = %q, want the page that lists and removes sign-in methods", url)
	}
}

// THE RECIPIENT IS NOT NEGOTIABLE. The provider asserted an address, and that
// assertion is what triggered the link, so taking the recipient from it would
// let the party the notice is ABOUT choose who reads it. Only the local row was
// verified by this install.
func TestSignInMethodAddedIgnoresTheProviderAssertedAddress(t *testing.T) {
	s, f := notifyService(t)
	local := User{ID: uuid.New(), Email: "sarah@acme.com", Status: "active"}
	in := googleID(true)
	in.Email = "attacker@elsewhere.test"

	s.sendSignInMethodAdded(context.Background(), local, in)

	if len(f.calls) != 1 {
		t.Fatalf("want one notification, got %d", len(f.calls))
	}
	for _, to := range f.calls[0].recipients {
		if to != "sarah@acme.com" {
			t.Fatalf("notification addressed to %q; it must only ever go to the local account's address", to)
		}
	}
}

// An operator-configured issuer is a real, checkable name. providerLabel says
// "your identity provider" for it, which in a security notice tells the reader
// nothing they can act on.
func TestSignInMethodLabelNamesTheIssuerForGenericOIDC(t *testing.T) {
	cases := []struct {
		name string
		in   SocialIdentity
		want string
	}{
		{"google", SocialIdentity{Provider: "google"}, "Google"},
		{"github", SocialIdentity{Provider: "github"}, "GitHub"},
		{"oidc with an issuer", SocialIdentity{Provider: "oidc", Issuer: "https://idp.acme.com/realms/main"}, "idp.acme.com"},
		{"oidc with a port", SocialIdentity{Provider: "oidc", Issuer: "https://idp.acme.com:8443"}, "idp.acme.com"},
		{"oidc with no issuer", SocialIdentity{Provider: "oidc"}, "your identity provider"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := signInMethodLabel(tc.in); got != tc.want {
				t.Fatalf("signInMethodLabel = %q, want %q", got, tc.want)
			}
		})
	}
}

// A missing display name is normal on a social account. It must not produce a
// message that reads like a broken mail merge, on the one email whose job is to
// be believed.
func TestSignInMethodAddedGreetsAnAccountWithNoName(t *testing.T) {
	s, f := notifyService(t)
	s.sendSignInMethodAdded(context.Background(), User{Email: "sarah@acme.com"}, googleID(true))

	if len(f.calls) != 1 {
		t.Fatalf("want one notification, got %d", len(f.calls))
	}
	if name, _ := f.calls[0].data["Name"].(string); name == "" {
		t.Error("Name is empty, which renders as \"Hi ,\"")
	}
}

// Mail is best effort here and must never fail the sign-in in progress: an
// install with no SMTP configured has no enqueuer wired at all.
func TestSignInMethodAddedIsSafeWithoutAMailer(t *testing.T) {
	s := &Service{}
	s.sendSignInMethodAdded(context.Background(), User{Email: "sarah@acme.com"}, googleID(true))
}

// A time is only useful if it is unambiguous. "When" is stamped in UTC with the
// zone spelled out, matching the password-changed notice.
func TestSignInMethodAddedStampsAnUnambiguousTime(t *testing.T) {
	s, f := notifyService(t)
	s.sendSignInMethodAdded(context.Background(), User{Email: "sarah@acme.com"}, googleID(true))

	when, _ := f.calls[0].data["When"].(string)
	if _, err := time.Parse("2006-01-02 15:04 MST", when); err != nil {
		t.Fatalf("When = %q, want a parseable zoned timestamp: %v", when, err)
	}
	if !contains(when, "UTC") {
		t.Errorf("When = %q, want UTC so the reader is not comparing against an unknown zone", when)
	}
}
