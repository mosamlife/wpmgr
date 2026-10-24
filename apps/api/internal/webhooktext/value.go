// Package webhooktext prepares site-supplied values for alert webhook text.
package webhooktext

import (
	"strings"
	"unicode"
)

// Value returns a single display line of at most 200 runes. Mentions,
// character references, chat control sequences and Markdown link labels remain
// visible as plain text.
func Value(s string) string {
	s = strings.ToValidUTF8(s, "")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf) || unicode.IsSpace(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	var b strings.Builder
	count := 0
	for _, r := range s {
		size := 1
		if r == '@' || r == '&' {
			size = 2
		}
		if count+size > 200 {
			break
		}
		switch r {
		case '@', '&':
			b.WriteRune(r)
			b.WriteRune('\u2060')
		case '<':
			b.WriteRune('‹')
		case '>':
			b.WriteRune('›')
		case '[':
			b.WriteRune('［')
		case ']':
			b.WriteRune('］')
		default:
			b.WriteRune(r)
		}
		count += size
	}
	return strings.TrimSpace(b.String())
}
