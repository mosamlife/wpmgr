package config

import (
	"strings"
	"testing"
)

// validSecret is a session secret that passes check 1, so these tests see only
// the issues they are about.
const validSecret = "0123456789abcdef0123456789abcdef"

// socialIssues returns the issues Validate reports for a development-env config
// carrying the given social settings, with every unrelated check satisfied.
func socialIssues(t *testing.T, cfg Config) []Issue {
	t.Helper()
	cfg.Env = "development"
	cfg.Auth.SessionSecret = validSecret
	cfg.River.MediaSchema = "media"
	return Validate(cfg)
}

// hasIssue reports whether an issue with the given env-var name was raised.
func hasIssue(issues []Issue, name string) bool {
	for _, iss := range issues {
		if iss.Name == name {
			return true
		}
	}
	return false
}

func issueNames(issues []Issue) string {
	names := make([]string, 0, len(issues))
	for _, iss := range issues {
		names = append(names, iss.Name)
	}
	return strings.Join(names, ", ")
}

// TestValidateRefusesSocialSignInWithoutPublicBaseURL is the regression for the
// defect where an install could enable social sign-in with no public base URL.
//
// The OAuth redirect_uri is derived, not configured: it is built as
// <public base URL>/auth/social/<provider>/callback. With the base URL unset
// that string is the relative path /auth/social/google/callback, which every
// provider rejects. Nothing refused it at boot, so the only symptom was a
// provider error page that gave no hint the fault was in the operator's own
// environment.
func TestValidateRefusesSocialSignInWithoutPublicBaseURL(t *testing.T) {
	cfg := Config{}
	cfg.Social.Google.ClientID = "123.apps.googleusercontent.com"
	cfg.Social.Google.ClientSecret = "google-secret"
	cfg.PublicBaseURL = ""

	issues := socialIssues(t, cfg)
	if !hasIssue(issues, "WPMGR_PUBLIC_BASE_URL") {
		t.Fatalf("an unset public base URL with social sign-in configured must be refused at boot; got issues: [%s]", issueNames(issues))
	}
}

// TestValidateRefusesNonAbsolutePublicBaseURL covers the values that look set
// but still cannot produce an absolute redirect_uri.
func TestValidateRefusesNonAbsolutePublicBaseURL(t *testing.T) {
	cases := []struct {
		name  string
		value string
	}{
		{"host with no scheme", "manage.example.com"},
		{"path only", "/wpmgr"},
		{"whitespace only", "   "},
		{"scheme with no host", "https://"},
		{"unsupported scheme", "ftp://manage.example.com"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{}
			cfg.Social.GitHub.ClientID = "gh-id"
			cfg.Social.GitHub.ClientSecret = "gh-secret"
			cfg.PublicBaseURL = tc.value

			issues := socialIssues(t, cfg)
			if !hasIssue(issues, "WPMGR_PUBLIC_BASE_URL") {
				t.Fatalf("public base URL %q cannot yield an absolute redirect_uri and must be refused; got issues: [%s]", tc.value, issueNames(issues))
			}
		})
	}
}

// TestValidateAcceptsAbsolutePublicBaseURL keeps the check from being a blanket
// refusal: a correctly configured install must boot, including a self-hosted
// operator on plain http, which no provider forces us to judge here.
func TestValidateAcceptsAbsolutePublicBaseURL(t *testing.T) {
	for _, base := range []string{"https://manage.example.com", "https://example.com/wpmgr/", "http://localhost:8088"} {
		cfg := Config{}
		cfg.Social.Google.ClientID = "id"
		cfg.Social.Google.ClientSecret = "secret"
		cfg.PublicBaseURL = base

		if issues := socialIssues(t, cfg); len(issues) != 0 {
			t.Fatalf("public base URL %q is absolute and must be accepted; got issues: [%s]", base, issueNames(issues))
		}
	}
}

// TestValidateIgnoresPublicBaseURLWithoutSocialSignIn guards the blast radius.
// The base URL requirement exists only because the social redirect_uri is
// derived from it, so an install that never configured a social provider must
// not be parked in degraded boot over a variable it does not need.
func TestValidateIgnoresPublicBaseURLWithoutSocialSignIn(t *testing.T) {
	cfg := Config{}
	cfg.PublicBaseURL = ""

	if issues := socialIssues(t, cfg); len(issues) != 0 {
		t.Fatalf("no social provider is configured, so the public base URL must not be demanded; got issues: [%s]", issueNames(issues))
	}
}

// TestValidateRefusesHalfConfiguredSocialProvider is the regression for a
// provider configured with one half of its credential.
//
// The provider correctly stays switched off, since a button that fails at the
// provider is worse than no button. What was wrong is that it stayed off in
// total silence: it vanished from /auth/social/providers with no log line, no
// failed check, and validate-env reporting the configuration as fine.
func TestValidateRefusesHalfConfiguredSocialProvider(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(*Config)
		wantName string
	}{
		{
			name:     "google client id without its secret",
			mutate:   func(c *Config) { c.Social.Google.ClientID = "123.apps.googleusercontent.com" },
			wantName: "WPMGR_SOCIAL_GOOGLE_CLIENT_SECRET",
		},
		{
			name:     "google client secret without its id",
			mutate:   func(c *Config) { c.Social.Google.ClientSecret = "google-secret" },
			wantName: "WPMGR_SOCIAL_GOOGLE_CLIENT_ID",
		},
		{
			name:     "github client id without its secret",
			mutate:   func(c *Config) { c.Social.GitHub.ClientID = "gh-id" },
			wantName: "WPMGR_SOCIAL_GITHUB_CLIENT_SECRET",
		},
		{
			name:     "github client secret without its id",
			mutate:   func(c *Config) { c.Social.GitHub.ClientSecret = "gh-secret" },
			wantName: "WPMGR_SOCIAL_GITHUB_CLIENT_ID",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{PublicBaseURL: "https://manage.example.com"}
			tc.mutate(&cfg)

			issues := socialIssues(t, cfg)
			if !hasIssue(issues, tc.wantName) {
				t.Fatalf("want issue %s for a half-configured provider; got issues: [%s]", tc.wantName, issueNames(issues))
			}
		})
	}
}

// TestValidateAcceptsWholeOrAbsentSocialConfig states the rule the check
// enforces: an optional subsystem is either off or whole, never half wired.
func TestValidateAcceptsWholeOrAbsentSocialConfig(t *testing.T) {
	t.Run("both providers absent", func(t *testing.T) {
		if issues := socialIssues(t, Config{}); len(issues) != 0 {
			t.Fatalf("no social configuration at all is legal; got issues: [%s]", issueNames(issues))
		}
	})
	t.Run("both providers whole", func(t *testing.T) {
		cfg := Config{PublicBaseURL: "https://manage.example.com"}
		cfg.Social.Google.ClientID = "google-id"
		cfg.Social.Google.ClientSecret = "google-secret"
		cfg.Social.GitHub.ClientID = "gh-id"
		cfg.Social.GitHub.ClientSecret = "gh-secret"

		if issues := socialIssues(t, cfg); len(issues) != 0 {
			t.Fatalf("a complete social configuration is legal; got issues: [%s]", issueNames(issues))
		}
	})
}

// TestSocialConfigured separates the two questions the config answers: Enabled
// decides whether a button renders, Configured decides whether the operator
// meant to switch this on and therefore deserves to hear about a mistake.
func TestSocialConfigured(t *testing.T) {
	var half SocialConfig
	half.Google.ClientID = "id-only"
	if half.Google.Enabled() {
		t.Error("a half-entered credential must not enable the provider")
	}
	if !half.Configured() {
		t.Error("a half-entered credential must still count as configured, or the mistake goes unreported")
	}

	var none SocialConfig
	if none.Configured() {
		t.Error("an install with no social credentials at all must not count as configured")
	}
}

// TestLoadPublicBaseURLFromEnv pins the binding the boot check depends on: if
// WPMGR_PUBLIC_BASE_URL does not reach cfg.PublicBaseURL, the new check would
// refuse every install that had actually set it.
func TestLoadPublicBaseURLFromEnv(t *testing.T) {
	t.Setenv("WPMGR_PUBLIC_BASE_URL", "https://manage.example.com")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PublicBaseURL != "https://manage.example.com" {
		t.Errorf("PublicBaseURL = %q, want the value from the environment", cfg.PublicBaseURL)
	}
}
