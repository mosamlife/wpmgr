// Package humantext is the one sanitiser for text a person reads that
// something other than WPMgr could have written: a site's name, a
// connection's name, or a message a site's agent reported.
//
// It is a leaf package. It imports nothing from this module, so any package
// may call it and none has to carry its own copy.
//
// Two levels:
//
//   - Clean keeps the words and removes what could hide or rearrange them:
//     invalid UTF-8, control and format characters (bidi overrides,
//     zero-width characters) and every kind of line break or space become one
//     plain space, and runs of spaces collapse. Use it on a label an operator
//     chose, such as a site name, where the words themselves are the point.
//
//   - Reason does all of that and more, for free-form prose that came from a
//     site: it NFKC-normalises, then replaces links, email addresses,
//     hostnames, IP addresses, absolute paths and long encoded runs with
//     neutral placeholders, and caps the result on a rune boundary, redacting
//     again after the cut.
//
// Neither makes text safe to interpret. The output is still plain text, for a
// text node, never markup and never a link.
package humantext

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Ellipsis is what CapRunes appends when it shortens a string.
const Ellipsis = "…"

// Clean returns s as valid UTF-8 with every control character (Cc), format
// character (Cf) and Unicode space or separator replaced by a single ASCII
// space, runs of whitespace collapsed to one space, and both ends trimmed.
// It does not shorten s; cap it afterwards with CapRunes or CapBytes.
func Clean(s string) string {
	s = strings.ToValidUTF8(s, "")
	s = replaceControlAndFormatChars(s)
	return collapseWhitespace(s)
}

// CapBytes truncates s to at most limit bytes, backing off until the result
// is valid UTF-8, so a multi-byte rune is never split. It appends nothing.
func CapBytes(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(s) <= limit {
		return s
	}
	b := []byte(s)[:limit]
	for len(b) > 0 && !utf8.Valid(b) {
		b = b[:len(b)-1]
	}
	return string(b)
}

// CapRunes returns s unchanged when it holds at most n runes. Otherwise it
// keeps the first n runes and appends Ellipsis, so the result holds n+1
// runes. A cut that leaves trailing whitespace is trimmed before the
// ellipsis. n <= 0 returns "".
func CapRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	i, count := 0, 0
	for i < len(s) && count < n {
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
		count++
	}
	return strings.TrimRightFunc(s[:i], unicode.IsSpace) + Ellipsis
}

// Reason turns free-form prose a site reported into a short, safe sentence
// fragment of at most limit bytes: valid UTF-8, NFKC-normalised (so
// full-width letters, "@", "/" and dots become their ASCII forms, and an
// ideographic full stop becomes "."), with control, format and space
// characters replaced and whitespace collapsed, and with links, addresses,
// hostnames, IP addresses, absolute paths and long encoded runs replaced by
// "[link]", "[address]", "[path]" or "[redacted]". The cap is applied on a
// rune boundary and the capped text is redacted again, so a cut cannot leave
// a bare hostname behind.
func Reason(s string, limit int) string {
	s = strings.ToValidUTF8(s, "")
	s = norm.NFKC.String(s)
	s = strings.Map(asciiFullStop, s)
	s = replaceControlAndFormatChars(s)
	s = collapseWhitespace(s)
	s = Redact(s)
	return capAndRedact(s, limit)
}

// Redact replaces every link, email address, absolute path, hostname, IP
// address and long encoded run in s with its placeholder, and collapses
// whitespace. It is idempotent: no placeholder matches any pattern.
func Redact(s string) string {
	// Links first, so a token in a query string is swallowed by "[link]"
	// rather than fragmented by a later pass, and so the "//host/path" of a
	// URL is never mistaken for a POSIX path.
	s = schemeURLPattern.ReplaceAllString(s, "[link]")
	s = emailPattern.ReplaceAllString(s, "[address]")
	s = uncPathPattern.ReplaceAllString(s, "[path]")
	s = windowsPathPattern.ReplaceAllString(s, "[path]")
	s = posixPathPattern.ReplaceAllString(s, "${1}[path]")
	s = ipv4Pattern.ReplaceAllString(s, "${1}[link]")
	s = redactHosts(s)
	s = encodedRunPattern.ReplaceAllStringFunc(s, redactEncoded)
	return collapseWhitespace(s)
}

var (
	// schemeURLPattern matches anything with a "//" authority, with or
	// without a scheme in front of it: http, https, ftp, a defanged "hxxps",
	// or a protocol-relative "//host".
	schemeURLPattern = regexp.MustCompile(`(?i)(?:\b[a-z][a-z0-9+.-]*:)?//\S*`)
	// emailPattern matches anything shaped like user@host.
	emailPattern = regexp.MustCompile(`\S+@\S+`)
	// uncPathPattern matches a Windows UNC path (\\server\share\...).
	uncPathPattern = regexp.MustCompile(`\\\\\S+`)
	// windowsPathPattern matches a drive-letter path with either separator.
	windowsPathPattern = regexp.MustCompile(`(?i)\b[a-z]:[\\/]\S*`)
	// posixPathPattern matches an ABSOLUTE POSIX path: a "/" at the start of
	// the text or after a character that cannot be part of a relative path.
	// The preceding character is captured and put back, so a relative path
	// such as "includes/commands/x.php" is left intact.
	posixPathPattern = regexp.MustCompile(`(^|[^A-Za-z0-9._/\-])/[A-Za-z0-9._-]+(?:/[A-Za-z0-9._-]*)*`)
	// hostPattern matches a dotted name whose last label starts with a
	// letter (a hostname shape, in any script, so an internationalised name
	// is covered as well as its punycode form), optionally followed by a
	// path. Group 1 is the character before the name (or the start of the
	// text), which is put back: Go's \b is ASCII-only, so the boundary is
	// spelled out. Any character that cannot be part of a name is a
	// boundary, "_" included, so "_evil.com" loses its host.
	// Group 2 is the name and its path.
	hostPattern = regexp.MustCompile(`(^|[^\p{L}\p{M}\p{N}])((?:[\p{L}\p{M}\p{N}](?:[\p{L}\p{M}\p{N}-]*[\p{L}\p{M}\p{N}])?\.)+\p{L}[\p{L}\p{M}\p{N}-]*[\p{L}\p{M}\p{N}](?:/\S*)?)`)
	// ipv4Pattern matches a dotted-quad address, with an optional port and
	// path. Like hostPattern, group 1 is the boundary in front of it (the
	// start of the text, or a character that is not an ASCII letter or
	// digit, "_" included) and is put back; group 2 is the address. A
	// letter directly in front ("v1.2.3.4") keeps the text as it is.
	ipv4Pattern = regexp.MustCompile(`(^|[^0-9A-Za-z])(\d{1,3}(?:\.\d{1,3}){3}\b(?::\d{1,5})?(?:/\S*)?)`)
	// encodedRunPattern matches a long token-shaped run; redactEncoded
	// decides whether it is one.
	encodedRunPattern = regexp.MustCompile(`[A-Za-z0-9+/=_-]{32,}`)
	whitespaceRun     = regexp.MustCompile(`\s+`)
)

// fileExtensions are the final labels that make a dotted name a file name,
// not a hostname, when nothing else about it looks like a link. None of them
// is a top-level domain.
var fileExtensions = map[string]struct{}{
	"php": {}, "phar": {}, "js": {}, "json": {}, "css": {}, "htm": {}, "html": {},
	"xml": {}, "sql": {}, "txt": {}, "log": {}, "ini": {}, "lock": {}, "yml": {},
	"yaml": {}, "gz": {}, "tar": {}, "bak": {}, "tmp": {}, "csv": {},
}

// redactHosts applies redactHost to every hostPattern match, keeping the
// boundary character each match captured in front of the name.
func redactHosts(s string) string {
	matches := hostPattern.FindAllStringSubmatchIndex(s, -1)
	if matches == nil {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	prev := 0
	for _, m := range matches {
		nameStart, nameEnd := m[4], m[5]
		b.WriteString(s[prev:nameStart])
		b.WriteString(redactHost(s[nameStart:nameEnd]))
		prev = nameEnd
	}
	b.WriteString(s[prev:])
	return b.String()
}

// redactHost replaces a hostname-shaped match with "[link]" unless it is a
// plain file name: no "www." in front, no path after it, and a final label
// that is a known file extension.
func redactHost(m string) string {
	if strings.Contains(m, "/") || strings.HasPrefix(strings.ToLower(m), "www.") {
		return "[link]"
	}
	ext := strings.ToLower(m[strings.LastIndexByte(m, '.')+1:])
	if _, ok := fileExtensions[ext]; ok {
		return m
	}
	return "[link]"
}

// redactEncoded replaces a long run with "[redacted]" when it carries a digit
// or a base64 symbol, the marks of a token, key or hash. A run of letters,
// "-", "_" and "/" alone is a relative path or a slug, and is kept.
func redactEncoded(m string) string {
	if strings.ContainsAny(m, "0123456789+=") {
		return "[redacted]"
	}
	return m
}

// capAndRedact caps an already-redacted s at limit bytes. Cutting can turn a
// kept file name into a bare hostname ("help.com.js" cut to "help.com"), so
// the capped text is redacted again. A placeholder can be a few bytes longer
// than what it replaced, so this repeats until the text fits; only the tail
// the cut created can change on each pass, and a cut placeholder matches
// nothing.
func capAndRedact(s string, limit int) string {
	for i := 0; i < 4; i++ {
		if len(s) <= limit {
			return s
		}
		s = Redact(CapBytes(s, limit))
	}
	if len(s) <= limit {
		return s
	}
	// Did not settle: drop the partial last word rather than show it.
	s = CapBytes(s, limit)
	if k := strings.LastIndexByte(s, ' '); k > 0 {
		return s[:k]
	}
	return ""
}

// asciiFullStop maps the ideographic full stop (U+3002, which NFKC also
// produces from the halfwidth form) to ".", as hostname parsers do. NFKC
// already maps the full-width and small full stops.
func asciiFullStop(r rune) rune {
	if r == '。' {
		return '.'
	}
	return r
}

// replaceControlAndFormatChars replaces every Unicode control character (Cc),
// format character (Cf: bidi overrides, zero-width characters) and space
// character (unicode.IsSpace, which includes U+2028 LINE SEPARATOR, U+2029
// PARAGRAPH SEPARATOR and U+00A0) with a single ASCII space, so the ASCII-only
// whitespace collapse that follows sees every one of them.
func replaceControlAndFormatChars(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf) || unicode.IsSpace(r) {
			b.WriteByte(' ')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// collapseWhitespace replaces every run of whitespace with a single space and
// trims the ends.
func collapseWhitespace(s string) string {
	return strings.TrimSpace(whitespaceRun.ReplaceAllString(s, " "))
}
