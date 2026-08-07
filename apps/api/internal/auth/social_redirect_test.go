package auth

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestSocialRedirectURL pins the derivation itself, including the two ways an
// operator's value differs from the canonical one.
func TestSocialRedirectURL(t *testing.T) {
	cases := []struct {
		name string
		base string
		want string
	}{
		{"plain origin", "https://manage.example.com", "https://manage.example.com/auth/social/google/callback"},
		{"trailing slash", "https://manage.example.com/", "https://manage.example.com/auth/social/google/callback"},
		{"surrounding whitespace", "  https://manage.example.com  ", "https://manage.example.com/auth/social/google/callback"},
		{"path prefix", "https://example.com/wpmgr", "https://example.com/wpmgr/auth/social/google/callback"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SocialRedirectURL(tc.base, "google"); got != tc.want {
				t.Errorf("SocialRedirectURL(%q) = %q, want %q", tc.base, got, tc.want)
			}
		})
	}
}

// envExampleCallbackLine matches a documented callback URL in .env.example:
// a comment line holding nothing but an absolute /auth/social/<provider>/callback
// URL. That is the shape an operator copies into a provider's console.
var envExampleCallbackLine = regexp.MustCompile(`^#\s+(https?://\S+/auth/social/([a-z]+)/callback)\s*$`)

// TestEnvExampleDocumentsTheDerivedSocialCallback checks the callback URLs
// .env.example tells operators to register against the URL this code actually
// produces, from that same file's own WPMGR_PUBLIC_BASE_URL.
//
// This exists because the two were once written independently, and drifted: the
// file documented a callback on the web container's port while the code derived
// one on the API's, so an operator who followed the instructions to the letter
// got redirect_uri_mismatch from the provider. That failure has no symptom to
// search for, since both URLs look plausible and neither the log nor the
// dashboard mentions either.
//
// So the documentation is not allowed to be a second, hand-maintained copy of
// the rule. It is checked against SocialRedirectURL, the one function the
// running server uses.
func TestEnvExampleDocumentsTheDerivedSocialCallback(t *testing.T) {
	// internal/auth -> internal -> apps/api -> apps -> repo root.
	path := filepath.Join("..", "..", "..", "..", ".env.example")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read .env.example: %v", err)
	}

	var base string
	var documented []string
	for _, line := range strings.Split(string(raw), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "WPMGR_PUBLIC_BASE_URL="); ok {
			base = v
			continue
		}
		if m := envExampleCallbackLine.FindStringSubmatch(line); m != nil {
			documented = append(documented, m[1])
		}
	}

	if base == "" {
		t.Fatal(".env.example must set WPMGR_PUBLIC_BASE_URL: the social callback is derived from it, so a file that documents a callback without setting it documents nothing")
	}
	if len(documented) == 0 {
		t.Fatal(".env.example must document the callback URLs to register at the providers: deriving the redirect_uri means the operator cannot work it out from a provider console")
	}

	for _, got := range documented {
		provider := envExampleCallbackLine.FindStringSubmatch("# " + got)[2]
		want := SocialRedirectURL(base, provider)
		if got != want {
			t.Errorf("\n.env.example tells operators to register:\n  %s\nbut with its own WPMGR_PUBLIC_BASE_URL=%s this code derives:\n  %s\nRegistering the documented URL would fail with redirect_uri_mismatch.", got, base, want)
		}
	}
}
