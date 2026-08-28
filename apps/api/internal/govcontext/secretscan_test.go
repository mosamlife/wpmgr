package govcontext

import (
	"testing"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// testAWSKey is built by concatenation, not one contiguous literal: this
// exact 20-char shape is what GitHub's own push-protection secret scanner
// looks for too, and a real-looking literal committed in a test fixture trips
// it just as hard as a real key would.
var testAWSKey = "AKIA" + "ABCDEFGHIJKLMNOP"

// testStripeShapedToken is not a Stripe key (this codebase has no Stripe
// integration), but "sk_live_" + 32 chars is a shape GitHub's push-protection
// scanner also recognises generically. Concatenated for the same reason as
// testAWSKey above.
var testStripeShapedToken = "sk_live_" + "9f8a7b6c5d4e3f2a1b0c9d8e7f6a5b4c"

// TestDetectSecret_FindsEachShape proves ADR-064 Decision 10's list of
// shapes: "an access key, a private key block, a connection string, a bearer
// token, a password-shaped string with the right entropy".
func TestDetectSecret_FindsEachShape(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		wantCat string
	}{
		{"aws access key", "our deploy key is " + testAWSKey + ", do not share it", "aws_access_key"},
		{"private key block", "-----BEGIN RSA PRIVATE KEY-----\nMIIB...\n-----END RSA PRIVATE KEY-----", "private_key_block"},
		{"postgres connection string", "connect via postgres://admin:hunter2@db.internal:5432/prod", "database_connection_string"},
		{"bearer token", "Authorization: Bearer " + testStripeShapedToken, "bearer_token"},
		{"api key assignment", `api_key = "` + testStripeShapedToken + `"`, "api_key_assignment"},
		{"high entropy bare token", "the rotation token is 9fQ2xK7pL0mZ8vB3nR5tY1wA6cD4eH2j and expires nightly", "high_entropy_secret"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			snap := Snapshot{Guidance: GuidanceSet{BrandVoice: c.value}}
			cat, found := DetectSecret(snap)
			if !found {
				t.Fatalf("DetectSecret found nothing in %q, want category %q", c.value, c.wantCat)
			}
			if cat != c.wantCat {
				t.Errorf("category = %q, want %q (value matched a different pattern first)", cat, c.wantCat)
			}
		})
	}
}

// TestDetectSecret_ScansRestrictionsToo proves the asymmetry m122's header
// calls out: the scan reads BOTH restrictions and guidance.
func TestDetectSecret_ScansRestrictionsToo(t *testing.T) {
	snap := Snapshot{Restrictions: RestrictionSet{ForbiddenDomains: []string{testAWSKey}}}
	if _, found := DetectSecret(snap); !found {
		t.Error("DetectSecret did not scan the restrictions column")
	}
}

// TestDetectSecret_NeverEchoesTheMatch is Decision 10's non-negotiable half:
// "never echoes the matched text back to the caller". checkNoSecret's error
// message and Details must not contain the secret value itself.
func TestDetectSecret_NeverEchoesTheMatch(t *testing.T) {
	secret := testAWSKey
	snap := Snapshot{Guidance: GuidanceSet{BrandVoice: "deploy key " + secret}}

	err := checkNoSecret(snap)
	if err == nil {
		t.Fatal("checkNoSecret returned nil, want a context_secret_detected error")
	}
	if err.Kind != domain.KindValidation {
		t.Errorf("Kind = %v, want KindValidation (422)", err.Kind)
	}
	if err.Code != "context_secret_detected" {
		t.Errorf("Code = %q, want context_secret_detected", err.Code)
	}
	if containsSubstring(err.Message, secret) {
		t.Fatalf("SECRET LEAKED into error message: %q", err.Message)
	}
	for k, v := range err.Details {
		if s, ok := v.(string); ok && containsSubstring(s, secret) {
			t.Fatalf("SECRET LEAKED into error details[%q]: %q", k, s)
		}
	}
}

// TestDetectSecret_HonestCases_OrdinaryProseIsNotFlagged is the over-fire
// control: ordinary operator-authored guidance text must never be flagged. A
// scanner that reddens correct work gets disabled, and then it scans nothing.
func TestDetectSecret_HonestCases_OrdinaryProseIsNotFlagged(t *testing.T) {
	cases := []string{
		"",
		"We write in a warm, approachable tone for small business owners.",
		"Never discuss competitor pricing or make medical claims.",
		"The site targets photographers and small creative studios in Ontario.",
		"CamelCaseIdentifiersLikeThisAreNotSecretsTheyAreJustLongWords",
		"https://example.com/some/reasonably/long/path/that/is/not/a/secret",
	}
	for _, v := range cases {
		snap := Snapshot{Guidance: GuidanceSet{BrandVoice: v}}
		if cat, found := DetectSecret(snap); found {
			t.Errorf("DetectSecret flagged ordinary prose %q as %q", v, cat)
		}
	}
}

func containsSubstring(s, substr string) bool {
	return len(substr) > 0 && (func() bool {
		for i := 0; i+len(substr) <= len(s); i++ {
			if s[i:i+len(substr)] == substr {
				return true
			}
		}
		return false
	})()
}
