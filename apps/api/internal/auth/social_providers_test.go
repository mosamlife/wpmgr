package auth

import "testing"

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
			// GitHub's noreply addresses arrive as ordinary entries. They are
			// verified and usable; nothing here should special-case them.
			name:         "noreply primary is accepted like any other",
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
