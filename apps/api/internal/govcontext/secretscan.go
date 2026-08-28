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

// entropyRatio is the fraction of a token's ceiling entropy that its OBSERVED
// Shannon entropy must reach to be treated as "password-shaped... with the
// right entropy" per Decision 10. "Ceiling" is defined by tokenCeilingBits
// below — this constant has been wrong twice about what that ceiling is, and
// both mistakes are worth keeping on the record because the second one was
// only findable by fixing the first.
//
// MISTAKE 1 (fixed): a FIXED, length-independent bit/char threshold. This
// package used to compare against a flat 4.7 bits/char, but the maximum
// entropy ANY string of length n can have is log2(n) — 4.3219 at n=20,
// 4.6439 at n=25 — both below 4.7. Every 20-25 character token was
// structurally unable to cross that bound, whatever it contained: the check
// was dead code across exactly the length window it claimed to cover. See
// secretscan_test.go's boundary tests at 25 and 26.
//
// MISTAKE 2 (fixed): comparing against log2(len(token)) unconditionally, on
// the theory that a token could in principle use as many distinct symbols as
// it has characters. That is only true for an UNBOUNDED alphabet. A REAL
// token drawn from a FIXED, small alphabet — hex (16 symbols), base32 (32),
// base58 (58) — has a ceiling of log2(alphabet size) that does NOT grow with
// length, so log2(len(token)) eventually exceeds it: past 35 characters for
// hex, 86 for base32, 183 for base58, the threshold became unreachable by
// ANY content in that alphabet, whatever it contained — the identical dead-
// zone shape as mistake 1, just relocated from short tokens to long
// fixed-alphabet ones. A 40-character hex API key and a 64-character
// SHA-256 digest are common, not exotic, and both were undetectable.
//
// tokenCeilingBits (below) is the corrected ceiling: log2(min(len(token),
// alphabet size)), where alphabet size is either a detected narrow, known
// encoding (decimal/hex/base32/base58, via tokenAlphabetSize) or this
// package's own general token character class (66 symbols) for anything
// else. This is the mathematically correct bound for BOTH failure modes at
// once: for a token shorter than its alphabet, length is the binding
// constraint (mistake 1's regime); for a token at or past its alphabet's
// size, the alphabet is (mistake 2's regime) — and it never exceeds
// whichever is actually achievable, so there is no length past which a
// GENUINELY random token of ANY of these alphabets becomes undetectable.
// secretscan_test.go's calibration tests verify this holds from 20 characters
// up to 512 for decimal, hex, base32, base58 and this package's general
// alphabet.
//
// 0.78 itself is calibrated empirically against this corrected ceiling: a
// uniformly-random token has its entropy-to-ceiling ratio fall below 0.78 in
// under 10% of draws at every tested length and alphabet from 20 to 512
// characters (worst observed: hex at length 20, ~91% detection — short
// samples of a 16-symbol alphabet have the least room to look uniform, and
// that residual is the honest cost of also closing the dead zone at length
// 20 rather than only at length 512), while every non-secret fixture this
// package tests — camelCase and snake_case identifiers, URLs, long
// identifier-like strings well past 100 characters — tops out at 0.75.
const entropyRatio = 0.78

// generalAlphabetSize is the size of tokenRe's own character class
// (A-Za-z0-9+/_-, 66 symbols): the ceiling used for any token that is not
// entirely within one of narrowAlphabets below.
const generalAlphabetSize = 66

// narrowAlphabets lists well-known, low-cardinality credential/identifier
// encodings, ORDERED SMALLEST FIRST so a token consistent with more than one
// (e.g. a pure-digit token is also valid hex) gets the tightest, most
// accurate ceiling. A token using any character outside all of these falls
// back to generalAlphabetSize.
var narrowAlphabets = []string{
	"0123456789",                       // decimal, 10
	"0123456789abcdef",                 // hex, lowercase, 16
	"0123456789ABCDEF",                 // hex, uppercase, 16
	"0123456789abcdefABCDEF",           // hex, mixed case, 22 — unusual but not exotic
	"ABCDEFGHIJKLMNOPQRSTUVWXYZ234567", // base32 (RFC 4648), 32
	"123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz", // base58 (Bitcoin alphabet, excludes 0/O/I/l), 58
}

// tokenAlphabetSize returns the size of the smallest known alphabet in
// narrowAlphabets that every character of tok belongs to, or
// generalAlphabetSize if tok uses characters outside all of them.
func tokenAlphabetSize(tok string) int {
	for _, chars := range narrowAlphabets {
		if isSubsetOf(tok, chars) {
			return len(chars)
		}
	}
	return generalAlphabetSize
}

func isSubsetOf(tok, chars string) bool {
	for i := 0; i < len(tok); i++ {
		if strings.IndexByte(chars, tok[i]) < 0 {
			return false
		}
	}
	return true
}

// tokenCeilingBits returns the maximum Shannon entropy tok could possibly
// have, in bits per character: log2(min(len(tok), alphabet size)). See
// entropyRatio's doc comment for why both terms of this min() are load-
// bearing — dropping either one reopens one of this package's two prior dead
// zones.
func tokenCeilingBits(tok string) float64 {
	n := len(tok)
	if a := tokenAlphabetSize(tok); a < n {
		n = a
	}
	return math.Log2(float64(n))
}

var tokenRe = regexp.MustCompile(`[A-Za-z0-9+/_\-]{20,}`)

// hostnameRe matches a plausible fully-qualified hostname, taken as a whole
// value: one or more dot-separated labels (1-63 characters, lowercase
// alphanumeric, interior hyphens allowed, never leading/trailing one)
// followed by a lowercase-letters-only final label of 2-63 characters — a
// plausible TLD.
//
// Deliberately CASE-SENSITIVE, lowercase only — not the DNS spec's own
// case-insensitivity. A real hostname, as an operator actually types or
// pastes one, is essentially always lowercase by convention; a raw secret
// token is essentially always mixed-case, because token generators preserve
// case specifically to maximise the effective alphabet and hence the entropy
// per character. That is the one structural signal available here that
// mixed-case does NOT: "AKIAIOSFODNN7EXAMPLE.amazonaws.com" and
// "ghp-16CharTokenXyz.io" both satisfy hostname GRAMMAR (dot-separated
// labels, letters-only final label) but neither is lowercase-only, so
// neither exempts under this pattern — see secretscan_test.go's
// TestDetectSecret_HostnameExemptionRequiresLowercase for the four shapes
// this line was tightened against. A case-insensitive version of this regex
// was the first cut of this fix and over-corrected: requiring only "looks
// like hostname grammar" also exempted a mixed-case secret with a fake short
// TLD tacked on, which is exactly the accidental-paste shape Decision 10
// exists to catch.
var hostnameRe = regexp.MustCompile(`^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)

// looksLikeHostname reports whether v, taken as a whole, is a plausible,
// lowercase, fully-qualified domain name — exactly the shape
// RestrictionSet.ForbiddenDomains exists to hold.
//
// A real hostname routinely has borderline-to-high per-character entropy: a
// DNS label mixes letters, digits and hyphens with little natural
// repetition, which is a property of being a structured identifier, not
// evidence of being a randomly-generated secret. A single hyphenated
// LOWERCASE label with no dot at all — "extremely-long-subdomain-name", 29
// characters, ratio 0.7994 — crosses entropyRatio (0.78) on its own; the SAME
// label as part of "extremely-long-subdomain-name.example.com" is
// unambiguously a hostname and must not be refused as a credential. This
// check exempts the whole value from the entropy fallback when it looks like
// a lowercase hostname; the four exact-shape patterns above still run first
// and still catch a real secret that happens to be dotted, so this narrows
// only the fallback heuristic's blast radius, not Decision 10's actual
// coverage of known credential shapes.
//
// This is not adversarially airtight — a value could still be hand-crafted,
// entirely lowercase, to end in a short valid-looking TLD purely to dodge
// this check. Decision 10's scan defends against an operator ACCIDENTALLY
// pasting a live credential, not a deliberate attempt to disguise one, and
// the four exact-shape patterns above already catch any accidentally-pasted
// secret that carries its own recognisable prefix or structure regardless of
// this exemption; the lowercase requirement closes the specific realistic
// residual this function's history already found (a mixed-case token
// suffixed with a fake TLD), not every conceivable one.
func looksLikeHostname(v string) bool {
	return len(v) <= 253 && hostnameRe.MatchString(v)
}

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
	if looksLikeHostname(v) {
		return "", false
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
// isHighEntropy reports whether tok's observed Shannon entropy reaches
// entropyRatio of tokenCeilingBits(tok) — the length-AND-alphabet-aware
// ceiling that closes both dead zones documented on entropyRatio.
func isHighEntropy(tok string) bool {
	if len(tok) < entropyMinLen {
		return false
	}
	return shannonEntropy(tok) >= entropyRatio*tokenCeilingBits(tok)
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
