package config

import (
	"strings"
	"testing"
)

// validSecret is a session secret that passes check 1, so these tests see only
// the issues they are about.
const validSecret = "0123456789abcdef0123456789abcdef"

// soundBase fills in the checks these tests are not about, so a non-empty
// result means the social checks produced it.
func soundBase(cfg Config) Config {
	if cfg.Env == "" {
		cfg.Env = "development"
	}
	cfg.Auth.SessionSecret = validSecret
	cfg.River.MediaSchema = "media"
	return cfg
}

// satisfyProductionSecrets fills the production-only secret these tests are
// not about (WPMGR_SITE_DEST_AGE_SECRET is read from the environment by
// Validate), so a production-env case reports only what it is testing.
func satisfyProductionSecrets(t *testing.T) {
	t.Helper()
	t.Setenv("WPMGR_SITE_DEST_AGE_SECRET", "AGE-SECRET-KEY-TEST")
}

// socialAdvisories returns the ADVISORIES for a config carrying the given
// social settings. Advisories degrade a feature; they never stop the server.
func socialAdvisories(t *testing.T, cfg Config) []Issue {
	t.Helper()
	return Advisories(soundBase(cfg))
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

// TestSocialMisconfigurationNeverBlocksBoot is the invariant the rest of this
// file is written around, and the regression for having put these checks on the
// boot gate in the first place.
//
// config.Validate is not a list of complaints: every issue it returns parks the
// process in serveDegraded, where /readyz 503s and NOTHING else answers. Putting
// a social sign-in mistake there turns a cosmetic misconfiguration into a total
// control-plane outage for every tenant on the install, and it does it on the
// upgrade path, where the operator changed nothing and has no reason to suspect
// a variable they set months ago. One sign-in button is never worth the backups,
// the updates, the monitoring and the dashboard.
func TestSocialMisconfigurationNeverBlocksBoot(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"half-entered google credential", func(c *Config) {
			c.Social.Google.ClientID = "123.apps.googleusercontent.com"
		}},
		{"half-entered github credential", func(c *Config) {
			c.Social.GitHub.ClientSecret = "gh-secret"
		}},
		{"configured provider with no public base URL", func(c *Config) {
			c.Social.Google.ClientID = "id"
			c.Social.Google.ClientSecret = "secret"
			c.PublicBaseURL = ""
		}},
		{"configured provider with a relative public base URL", func(c *Config) {
			c.Social.GitHub.ClientID = "gh-id"
			c.Social.GitHub.ClientSecret = "gh-secret"
			c.PublicBaseURL = "/wpmgr"
		}},
		{"plain-http public base URL in production", func(c *Config) {
			c.Env = "production"
			c.PublicBaseURL = "http://manage.example.com"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Unrelated production-only check; see satisfyProductionSecrets.
			satisfyProductionSecrets(t)
			cfg := Config{}
			tc.mutate(&cfg)
			cfg = soundBase(cfg)

			if issues := Validate(cfg); len(issues) != 0 {
				t.Fatalf("this configuration must not park the control plane in degraded boot; Validate returned: [%s]", issueNames(issues))
			}
			if len(Advisories(cfg)) == 0 {
				t.Fatalf("...but it must still be reported as an advisory, or the operator is left with silence")
			}
		})
	}
}

// TestAdvisoriesReportSocialSignInWithoutPublicBaseURL is the regression for an
// install that enables social sign-in with no public base URL.
//
// The OAuth redirect_uri is derived, not configured: it is built as
// <public base URL>/auth/social/<provider>/callback. With the base URL unset
// that string is the relative path /auth/social/google/callback, which every
// provider rejects. Nothing said so, so the only symptom was a provider error
// page that gave no hint the fault was in the operator's own environment.
func TestAdvisoriesReportSocialSignInWithoutPublicBaseURL(t *testing.T) {
	cfg := Config{}
	cfg.Social.Google.ClientID = "123.apps.googleusercontent.com"
	cfg.Social.Google.ClientSecret = "google-secret"
	cfg.PublicBaseURL = ""

	issues := socialAdvisories(t, cfg)
	if !hasIssue(issues, "WPMGR_PUBLIC_BASE_URL") {
		t.Fatalf("an unset public base URL with social sign-in configured must be reported; got advisories: [%s]", issueNames(issues))
	}
}

// TestSocialSignInUsable covers the switch the server actually flips: with an
// unusable base URL the feature is turned off, which is why no button renders
// rather than one that dead-ends at the provider.
func TestSocialSignInUsable(t *testing.T) {
	cases := []struct {
		name     string
		value    string
		wantable bool
	}{
		{"empty", "", false},
		{"host with no scheme", "manage.example.com", false},
		{"path only", "/wpmgr", false},
		{"whitespace only", "   ", false},
		{"scheme with no host", "https://", false},
		{"unsupported scheme", "ftp://manage.example.com", false},
		{"https origin", "https://manage.example.com", true},
		{"http localhost", "http://localhost:8088", true},
		{"origin with a path prefix", "https://example.com/wpmgr", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{PublicBaseURL: tc.value}
			if got := cfg.SocialSignInUsable(); got != tc.wantable {
				t.Fatalf("SocialSignInUsable(%q) = %v, want %v", tc.value, got, tc.wantable)
			}

			// And the advisory has to agree with the switch, or the operator is
			// told about a feature that is on, or not told about one that is off.
			cfg.Social.GitHub.ClientID = "gh-id"
			cfg.Social.GitHub.ClientSecret = "gh-secret"
			reported := hasIssue(socialAdvisories(t, cfg), "WPMGR_PUBLIC_BASE_URL")
			if reported == tc.wantable {
				t.Fatalf("advisory for %q = %v, but the feature switch says usable=%v: the two must never disagree", tc.value, reported, tc.wantable)
			}
		})
	}
}

// TestAdvisoriesIgnorePublicBaseURLWithoutSocialSignIn guards the blast radius.
// The base URL requirement exists only because the social redirect_uri is
// derived from it, so an install that never configured a social provider must
// not be nagged about a variable it does not need for this.
func TestAdvisoriesIgnorePublicBaseURLWithoutSocialSignIn(t *testing.T) {
	cfg := Config{}
	cfg.PublicBaseURL = ""

	if issues := socialAdvisories(t, cfg); len(issues) != 0 {
		t.Fatalf("no social provider is configured, so the public base URL must not be demanded; got advisories: [%s]", issueNames(issues))
	}
}

// TestAdvisoriesReportHalfConfiguredSocialProvider is the regression for a
// provider configured with one half of its credential.
//
// The provider correctly stays switched off, since a button that fails at the
// provider is worse than no button. What was wrong is that it stayed off in
// total silence: it vanished from /auth/social/providers with no log line, no
// warning, and validate-env reporting the configuration as fine.
func TestAdvisoriesReportHalfConfiguredSocialProvider(t *testing.T) {
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

			issues := socialAdvisories(t, cfg)
			if !hasIssue(issues, tc.wantName) {
				t.Fatalf("want advisory %s for a half-configured provider; got: [%s]", tc.wantName, issueNames(issues))
			}
		})
	}
}

// TestAdvisoriesAcceptWholeOrAbsentSocialConfig states the rule: an optional
// subsystem is either off or whole, and both of those are quiet.
func TestAdvisoriesAcceptWholeOrAbsentSocialConfig(t *testing.T) {
	t.Run("both providers absent", func(t *testing.T) {
		if issues := socialAdvisories(t, Config{}); len(issues) != 0 {
			t.Fatalf("no social configuration at all is legal; got advisories: [%s]", issueNames(issues))
		}
	})
	t.Run("both providers whole", func(t *testing.T) {
		cfg := Config{PublicBaseURL: "https://manage.example.com"}
		cfg.Social.Google.ClientID = "google-id"
		cfg.Social.Google.ClientSecret = "google-secret"
		cfg.Social.GitHub.ClientID = "gh-id"
		cfg.Social.GitHub.ClientSecret = "gh-secret"

		if issues := socialAdvisories(t, cfg); len(issues) != 0 {
			t.Fatalf("a complete social configuration is legal; got advisories: [%s]", issueNames(issues))
		}
	})
}

// TestAdvisoriesReportPlainHTTPPublicBaseURLInProduction covers the reason this
// value deserves a word about its scheme even though no provider needs us to
// judge it: the same origin builds password-reset and invitation links, and
// those carry single-use tokens that an http:// link puts in clear text.
//
// Loopback and non-production are exempt, and it is an advisory rather than a
// refusal, because a plain-http origin behind a TLS-terminating proxy is a
// legitimate deployment this process cannot see from the inside.
func TestAdvisoriesReportPlainHTTPPublicBaseURLInProduction(t *testing.T) {
	cases := []struct {
		name       string
		env        string
		base       string
		wantIssue  bool
		wantReason string
	}{
		{name: "production http public host", env: "production", base: "http://manage.example.com", wantIssue: true},
		{name: "production https", env: "production", base: "https://manage.example.com"},
		{name: "production http loopback", env: "production", base: "http://localhost:8088"},
		{name: "production http 127.0.0.1", env: "production", base: "http://127.0.0.1:8088"},
		{name: "development http public host", env: "development", base: "http://manage.example.com"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			satisfyProductionSecrets(t)
			cfg := soundBase(Config{Env: tc.env, PublicBaseURL: tc.base})

			got := hasIssue(Advisories(cfg), "WPMGR_PUBLIC_BASE_URL")
			if got != tc.wantIssue {
				t.Fatalf("advisory for env=%s base=%s = %v, want %v", tc.env, tc.base, got, tc.wantIssue)
			}
			// Whatever the answer, it is never a reason to stop serving.
			if issues := Validate(cfg); len(issues) != 0 {
				t.Fatalf("Validate must not block on the base URL scheme; got: [%s]", issueNames(issues))
			}
		})
	}
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

// TestLoadNormalizesPublicBaseURL pins the two properties the checks depend on.
//
// The BINDING: if WPMGR_PUBLIC_BASE_URL does not reach cfg.PublicBaseURL, every
// consumer falls back to an empty origin and the checks judge a value nobody
// set.
//
// The NORMALIZATION: consumers concatenate a path onto this string, so the
// value they use has to be the value that was checked, byte for byte. A check
// that trimmed whitespace while consumers did not would approve
// " https://x " and then build " https://x /auth/social/google/callback".
func TestLoadNormalizesPublicBaseURL(t *testing.T) {
	cases := []struct {
		name string
		set  string
		want string
	}{
		{"plain", "https://manage.example.com", "https://manage.example.com"},
		{"trailing slash", "https://manage.example.com/", "https://manage.example.com"},
		{"surrounding whitespace", "  https://manage.example.com  ", "https://manage.example.com"},
		{"whitespace and trailing slash", " https://manage.example.com/ ", "https://manage.example.com"},
		{"whitespace only", "   ", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("WPMGR_PUBLIC_BASE_URL", tc.set)

			cfg, err := Load("")
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.PublicBaseURL != tc.want {
				t.Errorf("PublicBaseURL = %q, want %q", cfg.PublicBaseURL, tc.want)
			}
		})
	}
}

// TestValidatePublicBaseURLJudgesTheStringItIsGiven is the other half of
// normalization: the check must not quietly trim, or it would approve a value
// that differs from the one consumers concatenate.
func TestValidatePublicBaseURLJudgesTheStringItIsGiven(t *testing.T) {
	if validatePublicBaseURL(" https://manage.example.com") == nil {
		t.Error("an untrimmed value must not be approved: consumers would use it verbatim")
	}
	if issue := validatePublicBaseURL("https://manage.example.com"); issue != nil {
		t.Errorf("the normalized value must be approved; got: %s", issue.Reason)
	}
}
