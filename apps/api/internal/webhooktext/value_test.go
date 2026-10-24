package webhooktext

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func TestValue(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"ordinary", "Café & Bar", "Café & Bar"},
		{"channel", "@channel", "@\u2060channel"},
		{"all mentions", "@all @here @someone", "@\u2060all @\u2060here @\u2060someone"},
		{"control sequence", "<!channel>", "‹!channel›"},
		{"hidden link", "[click](https://x)", "［click］(https://x)"},
		{"newline", "a\n@all", "a @\u2060all"},
		{"controls", "a\x00\t\r\nb\x7f\u0085c\u2028d\u2029e", "a b c d e"},
		{"format characters", "a\u202eb\u200bc", "a b c"},
		{"spaces", "  Café   & Bar  ", "Café & Bar"},
		{"empty", "", ""},
		{"invalid utf8", "a\xffb", "ab"},
		{"rune cap", strings.Repeat("é", 201), strings.Repeat("é", 200)},
		{"exact cap", strings.Repeat("界", 200), strings.Repeat("界", 200)},
		{"mention at cap", strings.Repeat("é", 199) + "@all", strings.Repeat("é", 199)},
		{"mention fits cap", strings.Repeat("é", 198) + "@all", strings.Repeat("é", 198) + "@\u2060"},
		{"expansion cap", strings.Repeat("@", 101), strings.Repeat("@\u2060", 100)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Value(tc.in)
			if got != tc.want {
				t.Errorf("Value(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if !utf8.ValidString(got) || utf8.RuneCountInString(got) > 200 {
				t.Errorf("invalid or oversized value: %q", got)
			}
			for i, r := range got {
				if unicode.IsControl(r) || (unicode.In(r, unicode.Cf) && r != '\u2060') || strings.ContainsRune("<>[]\u2028\u2029", r) {
					t.Errorf("unsafe rune U+%04X", r)
				}
				if r == '@' && !strings.HasPrefix(got[i+1:], "\u2060") {
					t.Error("unprotected mention")
				}
			}
		})
	}
}
