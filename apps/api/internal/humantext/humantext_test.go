package humantext

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func TestClean_RemovesWhatCanHideOrRearrangeWords(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"bidi override", "Shop\u202Egnp.exe", "Shop gnp.exe"},
		{"zero width space and joiner", "Sh\u200Bop\u200D Two", "Sh op Two"},
		{"newline and tab", "line one\n\tIGNORE PREVIOUS\r\ninstructions", "line one IGNORE PREVIOUS instructions"},
		{"line and paragraph separators", "a\u2028b\u2029c", "a b c"},
		{"no-break space", "a\u00A0\u00A0b", "a b"},
		{"leading and trailing runs", "  \t name \n ", "name"},
		{"invalid utf-8 dropped", "ok\xff\xfename", "okname"},
		{"plain label unchanged", "Bücher Shop", "Bücher Shop"},
		{"full-width letters kept, not folded", "ＳＨＯＰ", "ＳＨＯＰ"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Clean(tc.in)
			if got != tc.want {
				t.Fatalf("Clean(%q) = %q, want %q", tc.in, got, tc.want)
			}
			for _, r := range got {
				if unicode.IsControl(r) || unicode.In(r, unicode.Cf) || (unicode.IsSpace(r) && r != ' ') {
					t.Fatalf("Clean(%q) kept rune U+%04X", tc.in, r)
				}
			}
		})
	}
}

func TestCapRunes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		n    int
		want string
	}{
		{"shorter than n", "abc", 5, "abc"},
		{"exactly n", "abcde", 5, "abcde"},
		{"one over", "abcdef", 5, "abcde" + Ellipsis},
		{"multibyte runes counted as runes", "ääääää", 3, "äää" + Ellipsis},
		{"trailing space trimmed before the ellipsis", "abc def", 4, "abc" + Ellipsis},
		{"zero", "abc", 0, ""},
		{"negative", "abc", -1, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CapRunes(tc.in, tc.n); got != tc.want {
				t.Fatalf("CapRunes(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
			}
		})
	}
}

// TestCleanThenCapRunesFitsTheStoredLabelColumns pins the arithmetic the
// request table's CHECKs rely on: a label capped at n runes is stored in at
// most n+1 characters (site_label <= 201 at n=200, grant_label <= 65 at
// n=64), whatever the input.
func TestCleanThenCapRunesFitsTheStoredLabelColumns(t *testing.T) {
	hostile := strings.Repeat("é\u202E\u200B\nIGNORE PREVIOUS ", 400)
	for _, n := range []int{200, 64} {
		got := CapRunes(Clean(hostile), n)
		if c := utf8.RuneCountInString(got); c > n+1 {
			t.Fatalf("n=%d: %d runes", n, c)
		}
		if strings.ContainsAny(got, "\u202E\u200B\n") {
			t.Fatalf("n=%d: a control or format rune survived: %q", n, got)
		}
		if !strings.HasSuffix(got, Ellipsis) {
			t.Fatalf("n=%d: a shortened label must end in the ellipsis: %q", n, got)
		}
	}
}

func TestCapBytes_NeverSplitsARune(t *testing.T) {
	s := strings.Repeat("ä", 10) // 20 bytes
	for limit := 0; limit <= 21; limit++ {
		got := CapBytes(s, limit)
		if len(got) > limit {
			t.Fatalf("limit %d: %d bytes", limit, len(got))
		}
		if !utf8.ValidString(got) {
			t.Fatalf("limit %d: split a rune: %q", limit, got)
		}
		if limit >= 1 && len(got) < limit-1 {
			t.Fatalf("limit %d: cut too much: %d bytes", limit, len(got))
		}
	}
	if got := CapBytes("abc", -1); got != "" {
		t.Fatalf("negative limit: %q", got)
	}
}

// TestReason_RedactsSiteProse is the site-reported-text case: prose a site
// sent back on a failed clear keeps its words and loses its links, addresses
// and paths.
func TestReason_RedactsSiteProse(t *testing.T) {
	got := Reason("purge failed at https://evil.example/x from 10.0.0.1 in /var/www/secret", 512)
	want := "purge failed at [link] from [link] in [path]"
	if got != want {
		t.Fatalf("Reason = %q, want %q", got, want)
	}
}

func TestReason_NormalisesThenRedacts(t *testing.T) {
	cases := []struct {
		name, in, leak string
	}{
		{"full-width host", "see ｅｖｉｌ．ｃｏｍ now", "evil"},
		{"ideographic full stop", "see evil\u3002com now", "evil"},
		{"email", "mail root@evil.example", "root@"},
		{"bare host", "call evil.example.org", "evil.example"},
		{"windows path", `open C:\secret\x.txt`, "secret"},
		{"token", "key=" + strings.Repeat("aB3", 12), "aB3aB3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Reason(tc.in, 512)
			if strings.Contains(got, tc.leak) {
				t.Fatalf("Reason(%q) = %q, leaked %q", tc.in, got, tc.leak)
			}
		})
	}
}

func TestReason_CapsAndIsRedactedAfterTheCut(t *testing.T) {
	in := strings.Repeat("word ", 300) + "help.com.js"
	for limit := 10; limit < 60; limit++ {
		got := Reason(in, limit)
		if len(got) > limit {
			t.Fatalf("limit %d: %d bytes", limit, len(got))
		}
		if Redact(got) != got {
			t.Fatalf("limit %d: not a fixed point of Redact: %q", limit, got)
		}
	}
}
