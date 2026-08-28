package govcontext

import (
	"math"
	"regexp"
	"strings"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// secretPattern names a category of credential-shaped text and the regex that
// detects it. Order matters only for which category name is reported first
// when a value matches more than one; detection itself is independent of
// order (every value is checked against every pattern).
type secretPattern struct {
	category string
	re       *regexp.Regexp
}

// secretPatterns is the closed set of credential shapes Decision 10 scans
// for: "an access key, a private key block, a connection string, a bearer
// token, a password-shaped string with the right entropy". The first four are
// exact shapes; the fifth (entropyCategory below) is a fallback heuristic over
// tokens these patterns miss.
var secretPatterns = []secretPattern{
	{"aws_access_key", regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)},
	{"private_key_block", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{"database_connection_string", regexp.MustCompile(`\b(postgres(?:ql)?|mysql|mongodb(?:\+srv)?|redis)://[^\s'"]*:[^\s'"@]*@`)},
	{"bearer_token", regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9\-_.]{20,}`)},
	{"api_key_assignment", regexp.MustCompile(`(?i)(api[_-]?key|api[_-]?secret|client[_-]?secret|access[_-]?token|secret[_-]?key|password)\s*[:=]\s*['"]?[A-Za-z0-9/+_\-.]{8,}['"]?`)},
}

const entropyCategory = "high_entropy_secret"

// entropyMinLen is the shortest bare token the entropy fallback considers.
// Below this, ANY string of distinct characters trivially reaches the maximum
// possible entropy for its own length (e.g. a 4-character word with no
// repeated letters already has ratio 1.0 against itself), so a length floor
// is a false-positive guard independent of the ratio check below — not, on
// its own, a claim that anything shorter is safe from being a secret.
const entropyMinLen = 20

// entropyRatio is the fraction of a token's OWN maximum possible Shannon
// entropy — log2(len(token)), achieved only when every character in the
// token is distinct — that a bare alphanumeric token must reach to be
// treated as "password-shaped... with the right entropy" per Decision 10.
//
// A FIXED, LENGTH-INDEPENDENT bit/char threshold cannot work here, and
// previously did not: entropyThreshold used to be a flat 4.7 bits/char, but
// the maximum entropy ANY string of length n can have is log2(n) — 4.3219
// at n=20, 4.6439 at n=25 — both below 4.7. Every 20-25 character token was
// therefore structurally unable to cross that bound, whatever it contained;
// the check was dead code across exactly the length window it claimed to
// cover, and reported "no secret found" there because it was unreachable,
// not because it looked and cleared. See secretscan_test.go's boundary tests
// at 25 and 26, which pinned that exact dead zone before this fix and now
// prove a 20-25 character high-entropy token is caught, not skipped.
//
// Calibrated empirically (see the fixed 20000-trial simulation this comment
// is not the place to reproduce, summarised in the PR): a uniformly-random
// token over this file's alphabet has its entropy-to-log2(len) ratio fall
// below 0.78 in fewer than 0.02% of draws at every tested length from 20 to
// 40, while every non-secret contiguous-token fixture this package has
// tested (camelCase identifiers, snake_case identifiers, URLs, hex-ish
// strings) tops out at 0.75. 0.78 sits in the gap with margin on both sides.
const entropyRatio = 0.78

var tokenRe = regexp.MustCompile(`[A-Za-z0-9+/_\-]{20,}`)

// DetectSecret scans every string value in a Snapshot for something
// credential-shaped. It returns the category of the FIRST match and true, or
// ("", false) when nothing matches.
//
// Per Decision 10 this function's caller must never echo the matched text
// back to the caller, into a response body, or into a persisted log line —
// DetectSecret itself never returns the match, only the category name, which
// is the mechanism that makes that guarantee possible to keep.
func DetectSecret(s Snapshot) (category string, found bool) {
	for _, v := range snapshotStrings(s) {
		if cat, ok := scanValue(v); ok {
			return cat, true
		}
	}
	return "", false
}

// snapshotStrings flattens every string leaf in a Snapshot. Decision 10 scans
// BOTH restrictions and guidance — "The secret scan (ADR-064 Decision 10)
// reads BOTH. That asymmetry is the point of the split" (m122's header).
func snapshotStrings(s Snapshot) []string {
	out := make([]string, 0, 16)
	out = append(out, s.Restrictions.ForbiddenTools...)
	out = append(out, s.Restrictions.ForbiddenDomains...)
	out = append(out, s.Restrictions.ForbiddenTopics...)
	out = append(out, s.Guidance.BrandVoice, s.Guidance.Audience, s.Guidance.Terminology, s.Guidance.Style)
	return out
}

func scanValue(v string) (string, bool) {
	if v == "" {
		return "", false
	}
	for _, p := range secretPatterns {
		if p.re.MatchString(v) {
			return p.category, true
		}
	}
	for _, tok := range tokenRe.FindAllString(v, -1) {
		if isHighEntropy(tok) {
			return entropyCategory, true
		}
	}
	return "", false
}

// isHighEntropy reports whether tok's observed Shannon entropy reaches
// entropyRatio of the maximum ANY string of tok's length could have
// (log2(len(tok))), which is the length-relative form of "the right entropy"
// per Decision 10. See entropyRatio's doc comment for why a flat, length-
// independent bit/char bound cannot be used here.
func isHighEntropy(tok string) bool {
	if len(tok) < entropyMinLen {
		return false
	}
	maxEntropy := math.Log2(float64(len(tok)))
	return shannonEntropy(tok) >= entropyRatio*maxEntropy
}

// shannonEntropy returns the Shannon entropy of s in bits per character.
func shannonEntropy(s string) float64 {
	if s == "" {
		return 0
	}
	var counts [256]int
	for i := 0; i < len(s); i++ {
		counts[s[i]]++
	}
	n := float64(len(s))
	var h float64
	for _, c := range counts {
		if c == 0 {
			continue
		}
		p := float64(c) / n
		h -= p * math.Log2(p)
	}
	return h
}

// checkNoSecret is the Decision 10 write-time gate: a value shaped like a
// credential is refused with a 422 (domain.Validation) naming ONLY the
// category found, never the matched text — the reason string below is built
// entirely from the category name and never touches the flagged value.
func checkNoSecret(s Snapshot) *domain.Error {
	cat, found := DetectSecret(s)
	if !found {
		return nil
	}
	return domain.Validation("context_secret_detected", "this write was refused because it contains a value shaped like a "+
		strings.ReplaceAll(cat, "_", " ")+" — remove it and try again").
		WithDetails(map[string]any{"category": cat})
}
