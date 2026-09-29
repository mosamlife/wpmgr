package siteaddr

import "testing"

// TestPlan_OneHostKeyUpgradeIsSchemeOnly: an http address whose agent reports
// https with the host spelt another way that has the same HostKey is a
// scheme-only upgrade, adopted with the stored host's spelling (its ASCII
// letters lowercased, as every stored host is); a host that only Unicode
// lowercasing makes equal has another key and is still a mismatch; and the
// "www." toggle is decided as before.
func TestPlan_OneHostKeyUpgradeIsSchemeOnly(t *testing.T) {
	cases := []struct {
		name, stored, reported string
		want                   Decision
		to                     string
	}{
		{"non-ASCII letter case", "http://BÜCHER.de", "https://bücher.de", Adopt, "https://bÜcher.de"},
		{"non-ASCII letter case, reversed", "http://bücher.de", "https://BÜCHER.de", Adopt, "https://bücher.de"},
		{"Punycode spelling", "http://bücher.de", "https://xn--bcher-kva.de", Adopt, "https://bücher.de"},
		{"Unicode spelling of a Punycode host", "http://xn--bcher-kva.de/blog/", "https://bücher.de/blog", Adopt, "https://xn--bcher-kva.de/blog/"},
		{"dotted capital I vs its Unicode lowercase", "http://İstanbul.test", "https://istanbul.test", Mismatch, ""},
		{"capital sharp s vs its Unicode lowercase", "http://STRAẞE.test", "https://straße.test", Mismatch, ""},
		{"one key, another port", "http://BÜCHER.de", "https://bücher.de:8443", Mismatch, ""},
		{"one key, another path", "http://BÜCHER.de", "https://bücher.de/blog", Mismatch, ""},
		{"www toggle", "http://bücher.de", "https://www.bücher.de", Adopt, "https://www.bücher.de"},
		{"www toggle, ASCII", "https://example.com", "https://www.example.com", Adopt, "https://www.example.com"},
		{"www removed", "https://www.example.com/", "https://example.com", Adopt, "https://example.com/"},
		{"www toggle with a one-key spelling is not a sibling", "http://BÜCHER.de", "https://www.bücher.de", Mismatch, ""},
	}
	for _, c := range cases {
		for _, p := range planners {
			got := p.plan(c.stored, c.reported)
			if got.Decision != c.want || got.To != c.to {
				t.Errorf("%s: %s(%q, %q) = %+v, want %v %q", c.name, p.name, c.stored, c.reported, got, c.want, c.to)
			}
		}
	}
}
