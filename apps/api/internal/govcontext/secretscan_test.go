package govcontext

import (
	"fmt"
	"strings"
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

// ---------------------------------------------------------------------------
// entropyThreshold used to be a FLAT 4.7 bits/char bound, but the maximum
// Shannon entropy ANY string of length n can have is log2(n) — 4.3219 at
// n=20, 4.6439 at n=25. Every 20-25 character token was therefore
// STRUCTURALLY UNABLE to cross 4.7, whatever it contained: the entropy
// fallback reported "no secret found" across that entire window because the
// check was unreachable there, not because it looked and cleared. Fixed by
// making the threshold a fraction (entropyRatio) of the token's OWN maximum
// possible entropy (secretscan.go). These two tests pin exactly the boundary
// that fix must hold at.
// ---------------------------------------------------------------------------

// TestDetectSecret_EntropyBoundary_Length25IsNoLongerADeadZone: 25 characters
// is the LONGEST length for which the old flat 4.7 threshold was
// mathematically unreachable by ANY content (log2(25) = 4.6439 < 4.7). This
// token is maximally diverse (25 distinct characters, the highest entropy a
// 25-character string can have) and was therefore the best-case input the
// old scanner could ever be given at this length — and it still would have
// been reported clean.
//
// Confirmed RED restoring the OLD flat-threshold behaviour (entropyRatio's
// use replaced with the literal old check `shannonEntropy(tok) >= 4.7`):
//
//	$ go test ./internal/govcontext/... -run TestDetectSecret_EntropyBoundary_Length25IsNoLongerADeadZone -v
//	    secretscan_test.go:149: DetectSecret found nothing in a 25-char, maximally-diverse token (entropy 4.6439 bits/char) — the OLD flat-4.7 threshold is mathematically unreachable at this length regardless of content
//	--- FAIL: TestDetectSecret_EntropyBoundary_Length25IsNoLongerADeadZone
//
// Restored to the length-relative check, it is GREEN:
//
//	$ go test ./internal/govcontext/... -run TestDetectSecret_EntropyBoundary_Length25IsNoLongerADeadZone -v
//	--- PASS: TestDetectSecret_EntropyBoundary_Length25IsNoLongerADeadZone
func TestDetectSecret_EntropyBoundary_Length25IsNoLongerADeadZone(t *testing.T) {
	tok := "Xk9mQ2pL7vN4wZ8tR1yB6hJf3" // 25 chars, all distinct: H = log2(25) = 4.6439
	if len(tok) != 25 {
		t.Fatalf("test fixture is %d characters, want exactly 25", len(tok))
	}
	if h := shannonEntropy(tok); h < 4.6 || h > 4.65 {
		t.Fatalf("test fixture's entropy is %.4f, want ~4.6439 (recompute the fixture)", h)
	}
	snap := Snapshot{Guidance: GuidanceSet{BrandVoice: "rotation token: " + tok}}
	cat, found := DetectSecret(snap)
	if !found {
		t.Fatalf("DetectSecret found nothing in a 25-char, maximally-diverse token (entropy 4.6439 bits/char) — " +
			"the OLD flat-4.7 threshold is mathematically unreachable at this length regardless of content")
	}
	if cat != entropyCategory {
		t.Errorf("category = %q, want %q", cat, entropyCategory)
	}
}

// TestDetectSecret_EntropyBoundary_Length26RealisticTokenIsCaught uses a
// REALISTIC 26-character token — one repeated character, not the perfectly
// unique string that was the only content able to reach the old flat 4.7 at
// this exact length (log2(26) = 4.7004, so only an all-distinct 26-char
// string could ever have crossed the old bound). A real credential almost
// never has zero repeated characters; this fixture proves 26 characters is
// robustly covered now, not merely at its single unreachable-in-practice
// edge case.
func TestDetectSecret_EntropyBoundary_Length26RealisticTokenIsCaught(t *testing.T) {
	tok := "Xk9mQ2pL7vN4wZ8tR1yB6hJf3k" // 26 chars, ONE repeat ('k'): H = 4.6235 < 4.7 (old bound)
	if len(tok) != 26 {
		t.Fatalf("test fixture is %d characters, want exactly 26", len(tok))
	}
	h := shannonEntropy(tok)
	if h >= 4.7 {
		t.Fatalf("test fixture's entropy is %.4f, want < 4.7 (this fixture is supposed to fail the OLD flat "+
			"threshold so it proves the new one catches what the old one could not)", h)
	}
	snap := Snapshot{Guidance: GuidanceSet{BrandVoice: "rotation token: " + tok}}
	cat, found := DetectSecret(snap)
	if !found {
		t.Fatalf("DetectSecret found nothing in a realistic 26-char token (entropy %.4f bits/char, below the "+
			"old flat 4.7 bound but above the new length-relative one)", h)
	}
	if cat != entropyCategory {
		t.Errorf("category = %q, want %q", cat, entropyCategory)
	}
}

// ---------------------------------------------------------------------------
// The entropy fallback over-fires on ordinary hostnames. A single hyphenated
// DNS label with no dot at all can cross entropyRatio on its own —
// "extremely-long-subdomain-name" (29 chars) has ratio 0.7994, computed
// directly with shannonEntropy/log2 below — and that label is exactly what
// ForbiddenDomains is supposed to hold. Fixed by exempting a value that, as a
// WHOLE, looks like a fully-qualified hostname (looksLikeHostname,
// secretscan.go) from the entropy fallback specifically — the four
// exact-shape patterns still run unconditionally first.
// ---------------------------------------------------------------------------

// TestDetectSecret_HostnameNotFlaggedAsHighEntropy is the over-fire proof:
// realistic multi-label domains whose longest dot-free label independently
// crosses entropyRatio must not be flagged.
//
// Confirmed RED against secretscan.go with the `if looksLikeHostname(v) {
// return "", false }` line removed from scanValue:
//
//	$ go test ./internal/govcontext/... -run TestDetectSecret_HostnameNotFlaggedAsHighEntropy -v
//	    secretscan_test.go:228: DetectSecret flagged the ordinary hostname "extremely-long-subdomain-name.example.com" as "high_entropy_secret"
//	--- FAIL: TestDetectSecret_HostnameNotFlaggedAsHighEntropy
//
// Restored, it is GREEN.
func TestDetectSecret_HostnameNotFlaggedAsHighEntropy(t *testing.T) {
	domains := []string{
		// Each contains a single dot-free label that independently crosses
		// entropyRatio (0.78) — verified against isHighEntropy directly below
		// before asserting DetectSecret's behaviour over the whole domain.
		"extremely-long-subdomain-name.example.com",
		"wp-content-uploads-2026-08-archive.example.org",
		// The two domains reported directly: dotted, so tokenRe's dot-free
		// {20,} match never extracts a token 20+ characters long from either
		// (their longest label is under 20 chars) — pinned here so a future
		// change to tokenRe's character class (e.g. adding '.') cannot
		// silently reopen this without a test failing.
		"staging.client-portal.example.com",
		"some.very-long-subdomain.example.co.uk",
	}
	for _, d := range domains {
		t.Run(d, func(t *testing.T) {
			snap := Snapshot{Restrictions: RestrictionSet{ForbiddenDomains: []string{d}}}
			if cat, found := DetectSecret(snap); found {
				t.Errorf("DetectSecret flagged the ordinary hostname %q as %q", d, cat)
			}
		})
	}
}

// TestDetectSecret_HostnameLabelWouldHaveCrossedTheThreshold pins the actual
// numbers TestDetectSecret_HostnameNotFlaggedAsHighEntropy depends on staying
// true: that the fix is exempting a label that GENUINELY crosses
// entropyRatio, not one that was already safely below it. If this test ever
// fails, the over-fire proof above is no longer testing what it claims to.
func TestDetectSecret_HostnameLabelWouldHaveCrossedTheThreshold(t *testing.T) {
	cases := []struct {
		label string
		len   int
	}{
		{"extremely-long-subdomain-name", 29},
		{"wp-content-uploads-2026-08-archive", 34},
	}
	for _, c := range cases {
		if len(c.label) != c.len {
			t.Fatalf("fixture %q is %d characters, want %d", c.label, len(c.label), c.len)
		}
		if !isHighEntropy(c.label) {
			t.Errorf("isHighEntropy(%q) = false, want true — this fixture must independently cross "+
				"entropyRatio for the hostname-exemption test above to prove anything", c.label)
		}
	}
}

// TestDetectSecret_HostnameExemptionDoesNotReopenTheDeadZone re-confirms the
// entropy dead-zone fix (TestDetectSecret_EntropyBoundary_Length25/26) is
// unaffected by the hostname exemption: a bare, dot-free high-entropy token
// (no domain shape at all) must still be flagged. looksLikeHostname requires
// at least one dot, so a dotless value can never match it — this test proves
// that rather than asserting it from reading the regex.
func TestDetectSecret_HostnameExemptionDoesNotReopenTheDeadZone(t *testing.T) {
	tok := "Xk9mQ2pL7vN4wZ8tR1yB6hJf3" // the same 25-char fixture as the dead-zone test
	if looksLikeHostname(tok) {
		t.Fatalf("looksLikeHostname(%q) = true, want false (no dot at all)", tok)
	}
	snap := Snapshot{Guidance: GuidanceSet{BrandVoice: "rotation token: " + tok}}
	if _, found := DetectSecret(snap); !found {
		t.Error("DetectSecret found nothing in a bare 25-char high-entropy token — the hostname exemption must not affect dotless values")
	}
}

// ---------------------------------------------------------------------------
// Proportionality check: a case-INSENSITIVE hostname exemption swallows more
// than real hostnames. "AKIAIOSFODNN7EXAMPLE.amazonaws.com" and
// "ghp-16CharTokenXyz.io" both satisfy hostname grammar (dot-separated
// labels, letters-only final label) despite carrying a credential-shaped
// prefix, because grammar alone says nothing about case. hostnameRe is now
// lowercase-only — see its doc comment for why that is the available
// structural signal (real hostnames are conventionally lowercase; generated
// tokens are conventionally mixed-case to maximise entropy per character).
// ---------------------------------------------------------------------------

// TestDetectSecret_HostnameExemptionRequiresLowercase checks the four
// reported shapes against the actual end-to-end DetectSecret pipeline (not
// hostnameRe in isolation) and records, per row, WHY each lands where it
// does — two different rows are caught for two different reasons that are
// easy to conflate.
func TestDetectSecret_HostnameExemptionRequiresLowercase(t *testing.T) {
	cases := []struct {
		value   string
		found   bool
		explain string
	}{
		{
			"some.very-long-subdomain.example.co.uk", false,
			"real hostname, all lowercase — exempted, correctly",
		},
		{
			"sk-proj-A9fK2mQ7xR4tL8vN3wZ6yB1cD5eG", true,
			"bare mixed-case token, no dot at all — caught by entropy; hostnameRe never applies (no dot to match)",
		},
		{
			"AKIAIOSFODNN7EXAMPLE.amazonaws.com", true,
			"caught by the exact-shape aws_access_key PATTERN, which runs before the hostname check regardless of " +
				"case — this row was never actually exempted end-to-end, independent of this fix",
		},
		{
			"ghp-16CharTokenXyz.io", false,
			"NOT fixed by this change, and not caused by the hostname exemption either: the bare label " +
				"\"ghp-16CharTokenXyz\" is 18 characters, below entropyMinLen (20), so it would never be flagged " +
				"by entropy with or without any hostname involved, and this package has no GitHub-token-shaped " +
				"exact pattern. A separate, pre-existing, disclosed gap — not this fix's scope.",
		},
	}
	for _, c := range cases {
		t.Run(c.value, func(t *testing.T) {
			snap := Snapshot{Restrictions: RestrictionSet{ForbiddenDomains: []string{c.value}}}
			_, found := DetectSecret(snap)
			if found != c.found {
				t.Errorf("DetectSecret(%q) found=%v, want %v (%s)", c.value, found, c.found, c.explain)
			}
		})
	}
}

// TestDetectSecret_MixedCaseTokenWithFakeTLDIsCaught is the actual
// vulnerability TestDetectSecret_HostnameExemptionRequiresLowercase's row 3
// only LOOKS like it demonstrates: a bare, mixed-case, high-entropy token
// carrying NO recognisable prefix at all (not AWS-, PEM-, connection-string-,
// bearer-, or key=value-shaped) with a short lowercase TLD appended. Before
// the lowercase restriction, this satisfied hostname grammar case-
// insensitively and was exempted outright; the AWS example is a red herring
// for this specific class because it is caught by its own exact pattern
// regardless of the hostname check.
//
// Confirmed RED against a case-insensitive hostnameRe (the `(?i)` flag
// restored):
//
//	$ go test ./internal/govcontext/... -run TestDetectSecret_MixedCaseTokenWithFakeTLDIsCaught -v
//	    secretscan_test.go:354: looksLikeHostname("Xk9mQ2pL7vN4wZ8tR1yB6hJf3.io") = true, want false — it is mixed-case, not a real hostname
//	--- FAIL: TestDetectSecret_MixedCaseTokenWithFakeTLDIsCaught
//
// Restored (lowercase-only), it is GREEN.
func TestDetectSecret_MixedCaseTokenWithFakeTLDIsCaught(t *testing.T) {
	tok := "Xk9mQ2pL7vN4wZ8tR1yB6hJf3" // the dead-zone fixture: mixed case, ratio 1.0000, no recognisable prefix
	if !isHighEntropy(tok) {
		t.Fatalf("fixture %q must independently cross entropyRatio for this test to prove anything", tok)
	}
	v := tok + ".io"
	if looksLikeHostname(v) {
		t.Fatalf("looksLikeHostname(%q) = true, want false — it is mixed-case, not a real hostname", v)
	}
	snap := Snapshot{Restrictions: RestrictionSet{ForbiddenDomains: []string{v}}}
	if _, found := DetectSecret(snap); !found {
		t.Errorf("DetectSecret found nothing in %q — a bare mixed-case high-entropy token with no "+
			"recognisable prefix, hidden behind a fake TLD", v)
	}
}

// ---------------------------------------------------------------------------
// The length-relative ceiling (log2(len(token))) assumed an unbounded
// alphabet: a token could in principle use as many distinct symbols as it
// has characters. A real token drawn from a FIXED, small alphabet cannot —
// hex tops out at log2(16)=4.0 bits/char no matter how long it is — so past
// 35 characters for hex, 86 for base32, 183 for base58, the threshold became
// higher than that alphabet could ever produce: the identical dead-zone shape
// as the original bug, relocated from short tokens to long fixed-alphabet
// ones. A 40-character hex API key and a 64-character SHA-256 digest are
// common, not exotic, and both were undetectable. Fixed by capping the
// ceiling at log2(min(len(token), detected alphabet size)) — see
// entropyRatio's doc comment (secretscan.go) for the full account of both
// mistakes.
// ---------------------------------------------------------------------------

// TestDetectSecret_FixedAlphabetDigestsAreCaught uses real, reproducible hash
// digests — not synthetic strings — as the concrete case the dead zone
// missed. Each is hex, and each was previously undetectable purely because
// of its length, regardless of content.
//
// Confirmed RED against the pre-fix ceiling (tokenCeilingBits replaced with
// a bare `math.Log2(float64(len(tok)))`, ignoring alphabet entirely):
//
//	$ go test ./internal/govcontext/... -run TestDetectSecret_FixedAlphabetDigestsAreCaught -v
//	    secretscan_test.go:412: DetectSecret found nothing in the sha256("wpmgr") digest (64 hex characters) — this is exactly the fixed-alphabet dead zone
//	--- FAIL: TestDetectSecret_FixedAlphabetDigestsAreCaught
//
// Restored, it is GREEN.
func TestDetectSecret_FixedAlphabetDigestsAreCaught(t *testing.T) {
	cases := []struct {
		name string
		hex  string
	}{
		// sha1("wpmgr"), sha256("wpmgr"), md5("wpmgr") — reproducible with
		// any standard library: echo -n wpmgr | sha1sum / sha256sum / md5sum.
		{`sha1("wpmgr") digest (40 hex characters)`, "f432f2956ce15a76ee56bd8af04c1df25e2b13e5"},
		{`sha256("wpmgr") digest (64 hex characters)`, "6fe7e000d77661bc26207f7658a92908e3551bf11bfc82eac50d4e66e75acaee"},
		{`md5("wpmgr") digest (32 hex characters)`, "8955db4aaa57cdfb759681e466bc9caa"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := tokenAlphabetSize(c.hex); got != 16 {
				t.Fatalf("tokenAlphabetSize(%q) = %d, want 16 (hex) — fixture is not pure hex", c.hex, got)
			}
			snap := Snapshot{Guidance: GuidanceSet{BrandVoice: "deploy checksum: " + c.hex}}
			cat, found := DetectSecret(snap)
			if !found {
				t.Fatalf("DetectSecret found nothing in the %s — this is exactly the fixed-alphabet dead zone", c.name)
			}
			if cat != entropyCategory {
				t.Errorf("category = %q, want %q", cat, entropyCategory)
			}
		})
	}
}

// cyclicToken deterministically constructs a token of the given length by
// cycling through alphabet in order. For any length, every character of
// alphabet appears either floor(length/len(alphabet)) or one more time than
// that — as close to perfectly uniform as an integer-length string can be —
// so its Shannon entropy sits arbitrarily close to log2(len(alphabet)), that
// alphabet's true ceiling, regardless of how long the token is. This is
// deliberately NOT random sampling: a fixed construction removes sampling
// variance from the question "does detection degrade with length", which is
// exactly what the dead zone was about.
func cyclicToken(alphabet string, length int) string {
	var b strings.Builder
	for i := 0; i < length; i++ {
		b.WriteByte(alphabet[i%len(alphabet)])
	}
	return b.String()
}

// TestDetectSecret_NoDeadZoneAcrossAlphabetsAndLengths is the acceptance
// test named directly: for every alphabet a real credential encoding
// plausibly uses, and every tested length from entropyMinLen up to 512
// characters, a near-maximally-uniform token of that alphabet is detected.
// There is no (alphabet, length) pair in this table where the check cannot
// fire — which is the property the two prior dead zones each violated, in
// opposite directions (too-short tokens, then too-long fixed-alphabet ones).
func TestDetectSecret_NoDeadZoneAcrossAlphabetsAndLengths(t *testing.T) {
	alphabets := map[string]string{
		"decimal": "0123456789",
		"hex":     "0123456789abcdef",
		"base32":  "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567",
		"base58":  "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz",
	}
	lengths := []int{20, 24, 32, 40, 64, 96, 128, 256, 512}
	for name, alphabet := range alphabets {
		for _, length := range lengths {
			tok := cyclicToken(alphabet, length)
			t.Run(fmt.Sprintf("%s/len=%d", name, length), func(t *testing.T) {
				if !isHighEntropy(tok) {
					t.Errorf("isHighEntropy(%q) = false — dead zone at length %d for the %s alphabet", tok, length, name)
				}
			})
		}
	}
}

// TestTokenAlphabetSize_ClassifiesKnownEncodings is the unit-level proof
// tokenAlphabetSize picks the tightest (smallest) alphabet consistent with a
// token's actual characters, which is what makes the ceiling accurate rather
// than merely "large enough to never fire".
func TestTokenAlphabetSize_ClassifiesKnownEncodings(t *testing.T) {
	cases := []struct {
		tok  string
		want int
	}{
		{"0123456789", 10},
		{"0123456789abcdef", 16},
		{"0123456789ABCDEF", 16},
		{"deadBEEF0123", 22}, // mixed-case hex
		{"ABCDEFGHIJKLMNOPQRSTUVWXYZ234567", 32},
		{"123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz", 58},
		{"ThisIsALongCamelCaseIdentifier", generalAlphabetSize}, // outside every narrow alphabet
		{"api/key+value_pair-here", generalAlphabetSize},
	}
	for _, c := range cases {
		if got := tokenAlphabetSize(c.tok); got != c.want {
			t.Errorf("tokenAlphabetSize(%q) = %d, want %d", c.tok, got, c.want)
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
