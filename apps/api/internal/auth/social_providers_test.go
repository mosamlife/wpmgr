package auth

import (
	"context"
	"testing"

	"github.com/mosamlife/wpmgr/apps/api/internal/config"
)

// GitHub is not an OpenID Connect provider and has no email_verified claim, so
// primaryVerifiedEmail is where the verified address is established for that
// provider. Everything the policy does for a GitHub sign-in follows from this
// function's answer, which makes it worth pinning hard.
func TestPrimaryVerifiedEmail(t *testing.T) {
	cases := []struct {
		name         string
		in           []githubEmail
		wantEmail    string
		wantVerified bool
	}{
		{
			name:         "primary and verified",
			in:           []githubEmail{{Email: "sarah@acme.com", Primary: true, Verified: true}},
			wantEmail:    "sarah@acme.com",
			wantVerified: true,
		},
		{
			// The account exists but the address was never confirmed. Under this
			// policy that is not a sign-in, so the caller must see false.
			name:         "primary but unverified",
			in:           []githubEmail{{Email: "sarah@acme.com", Primary: true, Verified: false}},
			wantEmail:    "",
			wantVerified: false,
		},
		{
			// A verified address that is NOT the user's primary must not be
			// substituted. Signing somebody in under an address they do not
			// consider theirs creates an account they will not recognise or find
			// again, and on a shared work address could put them in the wrong org.
			name: "verified but not primary is refused",
			in: []githubEmail{
				{Email: "old@personal.test", Primary: false, Verified: true},
				{Email: "sarah@acme.com", Primary: true, Verified: false},
			},
			wantEmail:    "",
			wantVerified: false,
		},
		{
			name: "picks the primary from several",
			in: []githubEmail{
				{Email: "alt@personal.test", Primary: false, Verified: true},
				{Email: "sarah@acme.com", Primary: true, Verified: true},
				{Email: "third@personal.test", Primary: false, Verified: true},
			},
			wantEmail:    "sarah@acme.com",
			wantVerified: true,
		},
		{
			// GitHub's privacy addresses arrive as ordinary entries and this
			// function reports them as it finds them: primary, verified, and
			// exactly what GitHub said. Whether an address that GitHub will
			// never DELIVER to may become an account's contact address is a
			// separate question with a separate answer, one layer up in
			// githubAdapter.identity. See TestGitHubPrivateEmailIsMarkedUnreachable.
			name:         "noreply primary is reported as the verified primary it is",
			in:           []githubEmail{{Email: "1234+sarah@users.noreply.github.com", Primary: true, Verified: true}},
			wantEmail:    "1234+sarah@users.noreply.github.com",
			wantVerified: true,
		},
		{
			name:         "normalises case and surrounding space",
			in:           []githubEmail{{Email: "  Sarah@ACME.com ", Primary: true, Verified: true}},
			wantEmail:    "sarah@acme.com",
			wantVerified: true,
		},
		{
			// An account with the email scope granted but no addresses at all,
			// or a response we could not read. Must never be a sign-in.
			name:         "empty list",
			in:           nil,
			wantEmail:    "",
			wantVerified: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			email, verified := primaryVerifiedEmail(tc.in)
			if email != tc.wantEmail || verified != tc.wantVerified {
				t.Fatalf("got (%q, %v), want (%q, %v)", email, verified, tc.wantEmail, tc.wantVerified)
			}
		})
	}
}

// An unconfigured provider must not appear anywhere, because a rendered button
// that leads to a provider error page is worse than no button.
func TestSocialProvidersAbsentWhenUnconfigured(t *testing.T) {
	var sp *SocialProviders
	if got := sp.Enabled(); len(got) != 0 {
		t.Fatalf("a nil provider set must report nothing enabled, got %v", got)
	}
	if sp.Get("google") != nil {
		t.Fatal("a nil provider set must not return an adapter")
	}

	empty := &SocialProviders{byKey: map[string]SocialProviderAdapter{}}
	if len(empty.Enabled()) != 0 {
		t.Fatal("an empty provider set must report nothing enabled")
	}
	if empty.Get("github") != nil {
		t.Fatal("an unconfigured provider must not resolve to an adapter")
	}
}

// Constructing the provider set must do no network I/O and must not be able to
// fail. It used to call oidc.NewProvider inline and main returned the error, so
// an unreachable or merely slow accounts.google.com stopped the whole control
// plane from booting. This asserts the constructor is now pure: it returns
// immediately with Google listed as enabled, having contacted nobody.
func TestNewSocialProvidersDoesNoNetworkIO(t *testing.T) {
	sp := NewSocialProviders(config.SocialConfig{
		Google: config.GoogleConfig{ClientID: "id", ClientSecret: "secret"},
		GitHub: config.GitHubConfig{ClientID: "id", ClientSecret: "secret"},
	})

	got := sp.Enabled()
	if len(got) != 2 {
		t.Fatalf("Enabled() = %v, want both providers listed without any discovery call", got)
	}
	if sp.Get("google") == nil || sp.Get("github") == nil {
		t.Fatal("both adapters must exist before any network call is attempted")
	}

	// Discovery has not run, so the Google adapter is not yet ready. That is the
	// point: it becomes ready on first use, not at boot.
	g, ok := sp.Get("google").(*googleAdapter)
	if !ok {
		t.Fatal("expected the google adapter type")
	}
	if g.ready {
		t.Error("google adapter reports ready before any discovery call; construction is doing I/O")
	}
}

// A discovery failure must surface as a failed sign-in, not a failed boot, and
// must not be cached as permanent: the next attempt retries.
func TestGoogleDiscoveryFailureIsNotPermanent(t *testing.T) {
	g := newGoogleAdapter(config.GoogleConfig{ClientID: "id", ClientSecret: "secret"})

	// A context already past its deadline stands in for an unreachable issuer.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, _, _, _, err := g.AuthCodeURL(ctx, "https://example.test/cb"); err == nil {
		t.Fatal("a failed discovery must surface as an error from AuthCodeURL")
	}
	if g.ready {
		t.Error("a failed discovery must not mark the adapter ready")
	}
	// Nothing was cached, so a later attempt is free to succeed.
	if g.verifier != nil {
		t.Error("a failed discovery must leave no half-built verifier behind")
	}
}
