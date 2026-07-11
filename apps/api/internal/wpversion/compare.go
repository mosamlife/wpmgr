// Package wpversion implements WordPress version-comparison semantics,
// replicating the behaviour of PHP's version_compare() function as documented
// in the PHP manual and as used by the WordPress plugin ecosystem.
//
// WordPress version strings differ from semver in several important ways:
//   - They may have two, three, or four numeric segments (e.g. "6.5", "6.5.0",
//     "6.5.0.1").
//   - They may carry pre-release suffixes separated by ".", "_", "-", or "+"
//     (e.g. "1.2-beta1", "1.2.0-RC2", "1.2.0_alpha").
//   - Pre-release tokens are ordered: dev < alpha|a < beta|b < RC|rc < (no
//     suffix / release) < pl|p (patch-level).
//   - Digit/non-digit transitions inside a token are treated as separators
//     ("beta1" → ["beta", "1"]).
//
// AffectedVersions / IsVulnerable implement the range test used by the
// Wordfence Intelligence V3 feed. Each range is an object with four fields:
//
//	from_version      string — lower bound ("*" = unbounded)
//	from_inclusive    bool   — whether the lower bound is inclusive
//	to_version        string — upper bound ("*" = unbounded)
//	to_inclusive      bool   — whether the upper bound is inclusive
//
// A version is considered vulnerable when it falls within at least one of the
// affected_versions ranges. A nil / empty slice means no match (not vulnerable).
package wpversion

import (
	"strings"
	"unicode"
)

// pre-release token order — lower index = older / less-stable.
// A token not in this map is treated as a release (rank 5).
// "#" is the internal sentinel for empty sub-parts (double separators); it
// ranks between RC and release so that a bare separator compares correctly.
var preReleaseRank = map[string]int{
	"dev":   0,
	"alpha": 1,
	"a":     1,
	"beta":  2,
	"b":     2,
	"rc":    3,
	"#":     4, // empty/separator sentinel: between RC and release
	"pl":    6,
	"p":     6,
}

// tokenize splits a version string into comparable tokens using PHP's
// version_compare algorithm:
//  1. Replace ".", "_", "-", "+" with "." as canonical separators.
//  2. Split on ".".
//  3. Within each sub-token, split on digit/non-digit transitions so that
//     "beta1" becomes ["beta", "1"].
//  4. Empty tokens are replaced with the release sentinel "#".
func tokenize(v string) []string {
	if v == "" {
		return []string{"#"}
	}
	// Normalise separators to ".".
	v = strings.Map(func(r rune) rune {
		switch r {
		case '_', '-', '+':
			return '.'
		}
		return r
	}, v)

	var tokens []string
	for _, part := range strings.Split(v, ".") {
		if part == "" {
			tokens = append(tokens, "#")
			continue
		}
		// Split on digit/non-digit boundary within part.
		tokens = append(tokens, splitBoundary(part)...)
	}
	return tokens
}

// splitBoundary splits a string on digit↔non-digit transitions, e.g.
// "beta1" → ["beta", "1"], "1RC2" → ["1", "RC", "2"].
func splitBoundary(s string) []string {
	if s == "" {
		return nil
	}
	var parts []string
	start := 0
	isDigit := unicode.IsDigit(rune(s[0]))
	for i := 1; i < len(s); i++ {
		d := unicode.IsDigit(rune(s[i]))
		if d != isDigit {
			parts = append(parts, s[start:i])
			start = i
			isDigit = d
		}
	}
	parts = append(parts, s[start:])
	return parts
}

// compareToken compares two single version tokens (one segment after
// tokenisation).  Returns -1, 0, or 1 following the PHP pre-release ordering.
func compareToken(a, b string) int {
	// Normalise to lowercase for pre-release matching.
	aLow := strings.ToLower(a)
	bLow := strings.ToLower(b)

	aNum := isNumeric(a)
	bNum := isNumeric(b)

	switch {
	case aNum && bNum:
		return compareNumeric(a, b)
	case aNum:
		// A numeric token is always > a non-numeric pre-release token, but < a
		// non-numeric post-release token ("p"/"pl"). Use the release sentinel "#"
		// to represent a bare numeric segment in ordering comparisons against a
		// pre-release string.
		bRank := rankOf(bLow)
		aRank := 5 // numeric = "release" rank
		return cmp(aRank, bRank)
	case bNum:
		aRank := rankOf(aLow)
		bRank := 5
		return cmp(aRank, bRank)
	default:
		aRank := rankOf(aLow)
		bRank := rankOf(bLow)
		if aRank != bRank {
			return cmp(aRank, bRank)
		}
		// Same rank class: aliases (alpha/a, beta/b, pl/p, rc/RC) compare as equal.
		// PHP version_compare treats "alpha" == "a", "beta" == "b", "pl" == "p".
		return 0
	}
}

func rankOf(tok string) int {
	if r, ok := preReleaseRank[tok]; ok {
		return r
	}
	return 5 // release
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

// compareNumeric compares two strings that are purely numeric.  Leading zeroes
// are handled by comparing lengths first, then lexicographically (equivalent to
// integer comparison for reasonable version component widths).
func compareNumeric(a, b string) int {
	// Strip leading zeros for comparison.
	a = strings.TrimLeft(a, "0")
	b = strings.TrimLeft(b, "0")
	if len(a) != len(b) {
		return cmp(len(a), len(b))
	}
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

func cmp(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// Compare returns -1, 0, or 1 for a < b, a == b, a > b under PHP
// version_compare semantics.  It is safe to call with empty strings or
// wildcard "*".  "*" compares as the empty string (lowest possible value) so
// callers should check for wildcard before calling Compare.
func Compare(a, b string) int {
	if a == "*" {
		a = ""
	}
	if b == "*" {
		b = ""
	}
	tokensA := tokenize(a)
	tokensB := tokenize(b)

	maxLen := len(tokensA)
	if len(tokensB) > maxLen {
		maxLen = len(tokensB)
	}

	for i := range maxLen {
		ta := tokenAt(tokensA, i)
		tb := tokenAt(tokensB, i)
		if c := compareToken(ta, tb); c != 0 {
			return c
		}
	}
	return 0
}

// tokenAt returns the i-th token or "0" when i is beyond the end of the
// slice.  PHP's version_compare treats a missing segment as numeric zero,
// so "1.0.0" == "1.0" (both have a numeric "0" at position 2).
func tokenAt(tokens []string, i int) string {
	if i < len(tokens) {
		return tokens[i]
	}
	return "0"
}

// AffectedVersionRange is one entry in a vulnerability record's
// affected_versions array.  Fields mirror the Wordfence Intelligence V3 feed
// JSON shape.
type AffectedVersionRange struct {
	// FromVersion is the lower bound version string, or "*" for unbounded.
	FromVersion string `json:"from_version"`
	// FromInclusive controls whether the lower bound is inclusive (>=) or
	// exclusive (>).
	FromInclusive bool `json:"from_inclusive"`
	// ToVersion is the upper bound version string, or "*" for unbounded.
	ToVersion string `json:"to_version"`
	// ToInclusive controls whether the upper bound is inclusive (<=) or
	// exclusive (<).
	ToInclusive bool `json:"to_inclusive"`
}

// inRange reports whether installed falls within the range described by r.
func inRange(installed string, r AffectedVersionRange) bool {
	// Lower bound check.
	if r.FromVersion != "" && r.FromVersion != "*" {
		c := Compare(installed, r.FromVersion)
		if r.FromInclusive {
			if c < 0 { // installed < from → not vulnerable
				return false
			}
		} else {
			if c <= 0 { // installed <= from → not vulnerable
				return false
			}
		}
	}
	// Upper bound check.
	if r.ToVersion != "" && r.ToVersion != "*" {
		c := Compare(installed, r.ToVersion)
		if r.ToInclusive {
			if c > 0 { // installed > to → not vulnerable
				return false
			}
		} else {
			if c >= 0 { // installed >= to → not vulnerable
				return false
			}
		}
	}
	return true
}

// IsVulnerable reports whether the installed version string falls within at
// least one of the provided affected version ranges.  A nil or empty ranges
// slice returns false (not vulnerable).
//
// Versions reported as "unknown" (the agent fallback for unresolvable items)
// are treated as not vulnerable to avoid false positives.
func IsVulnerable(installed string, ranges []AffectedVersionRange) bool {
	if installed == "" || installed == "unknown" {
		return false
	}
	for _, r := range ranges {
		if inRange(installed, r) {
			return true
		}
	}
	return false
}

// BestFixedVersion returns the minimum version from the patched list that is
// strictly greater than installed. Returns "" when:
//   - patched is empty, or
//   - the installed version is already equal to a patched version (already fixed), or
//   - no patched version exists that is greater than installed.
//
// Exception: when all patched versions are strictly OLDER than installed (unusual;
// e.g. vuln was backported to an older branch), we surface the highest known fix
// so the operator can see what branch was patched.
func BestFixedVersion(installed string, patched []string) string {
	// Find the minimum patched version that is strictly greater than installed.
	best := ""
	for _, v := range patched {
		if v == "" {
			continue
		}
		if Compare(v, installed) > 0 {
			if best == "" || Compare(v, best) < 0 {
				best = v
			}
		}
	}
	if best != "" {
		return best
	}
	// No strictly-newer fix found.
	// If any patched version equals installed, the site is already on the fix.
	for _, v := range patched {
		if v == "" {
			continue
		}
		if Compare(v, installed) == 0 {
			return ""
		}
	}
	// All patched versions are strictly older than installed (unusual state, e.g.
	// the vulnerability was backported to an old release branch, and the operator
	// is already past the fix). Surface the highest known fix anyway.
	for _, v := range patched {
		if v == "" {
			continue
		}
		if best == "" || Compare(v, best) > 0 {
			best = v
		}
	}
	return best
}

// SameVersion reports whether installed and available refer to the same
// release after light, conservative normalization: both sides are
// TrimSpace'd, then a single leading "v"/"V" prefix is stripped from each
// (some update-transient sources report a "v"-prefixed version while the
// installed Version string carries none, or vice versa).
//
// This is intentionally an EQUALITY-only comparison. Go has no
// version_compare() equivalent safe enough to use for ORDERING an arbitrary
// plugin/theme version string, and a hand-rolled ordering check risks
// misclassifying a genuinely newer update as already-installed — far worse
// than the phantom-update symptom this guards against (GH #211: a WordPress
// update transient occasionally reports new_version == the already-installed
// Version, e.g. a Kadence "1.5.1 -> 1.5.1" advisory). Compare() (PHP
// version_compare semantics) is deliberately NOT used here.
//
// Either side empty returns false — fail OPEN, so a missing/unknown
// installed version never suppresses a reported update.
func SameVersion(installed, available string) bool {
	installed = strings.TrimSpace(installed)
	available = strings.TrimSpace(available)
	if installed == "" || available == "" {
		return false
	}
	return stripLeadingV(installed) == stripLeadingV(available)
}

// stripLeadingV removes a single leading "v"/"V" prefix, if present.
func stripLeadingV(v string) string {
	if len(v) > 0 && (v[0] == 'v' || v[0] == 'V') {
		return v[1:]
	}
	return v
}
