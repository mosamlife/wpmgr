package siteaddr

import (
	"strings"
	"testing"
)

// TestSameHost: hosts match by their IDNA lookup form, never by Unicode case
// folding, and a host that does not convert matches nothing.
func TestSameHost(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"Example.COM", "example.com", true},
		{"bücher.de", "BÜCHER.de", true},
		{"bücher.de", "xn--bcher-kva.de", true},
		{"::1", "::1", true},
		{"127.0.0.1", "127.0.0.1", true},
		{"straße.de", "straẞe.de", false}, // fold equal; xn--strae-oqa.de vs strasse.de
		{"σ.gr", "ς.gr", false},           // fold equal; xn--4xa.gr vs xn--3xa.gr
		{"my_site.test", "my_site.test", false},
		{"", "", false},
	}
	for _, c := range cases {
		if got := SameHost(c.a, c.b); got != c.want {
			t.Errorf("SameHost(%q, %q) = %v, want %v (EqualFold %v)", c.a, c.b, got, c.want, strings.EqualFold(c.a, c.b))
		}
	}
}

// TestPlanStrict: PlanStrict adopts only what Plan adopts and only when the
// reported host, in its ASCII form, is the stored host or its "www." sibling;
// the address it returns is Plan's, in the stored spelling.
func TestPlanStrict(t *testing.T) {
	cases := []struct {
		stored, reported string
		plan, strict     Decision
		to               string
	}{
		{"https://example.com", "https://www.example.com", Adopt, Adopt, "https://www.example.com"},
		{"http://example.com", "https://example.com/", Adopt, Adopt, "https://example.com"},
		{"https://bücher.de", "https://www.bücher.de", Adopt, Adopt, "https://www.bücher.de"},
		{"https://example.com", "https://EXAMPLE.com", Same, Same, ""},
		{"https://straße.de", "https://straẞe.de", Same, Mismatch, ""},
		{"http://straße.de", "https://www.straẞe.de", Adopt, Mismatch, ""},
		{"http://my_site.test", "https://my_site.test", Adopt, Mismatch, ""},
		{"https://example.com", "https://staging.example.com", Mismatch, Mismatch, ""},
	}
	for _, c := range cases {
		if p := Plan(c.stored, c.reported); p.Decision != c.plan {
			t.Errorf("Plan(%q, %q) = %v, want %v", c.stored, c.reported, p.Decision, c.plan)
		}
		p := PlanStrict(c.stored, c.reported)
		if p.Decision != c.strict || p.To != c.to {
			t.Errorf("PlanStrict(%q, %q) = %+v, want %v %q", c.stored, c.reported, p, c.strict, c.to)
		}
	}
}
