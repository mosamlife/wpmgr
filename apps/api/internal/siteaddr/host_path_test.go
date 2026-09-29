package siteaddr

import (
	"net/url"
	"testing"
)

// planners are the two rules an adoption goes through: enrollment's Plan and
// push-time PlanStrict. Every host and path guard below must hold for both.
var planners = []struct {
	name string
	plan func(stored, reported string) PlanResult
}{
	{"Plan", Plan},
	{"PlanStrict", PlanStrict},
}

// toKey is HostKey of the host in an address a plan returned.
func toKey(t *testing.T, to string) string {
	t.Helper()
	u, err := url.Parse(to)
	if err != nil {
		t.Fatalf("adopted address %q does not parse: %v", to, err)
	}
	k, ok := HostKey(u.Hostname())
	if !ok {
		t.Fatalf("adopted address %q has a host with no key", to)
	}
	return k
}

// TestPlan_StoresTheHostThatWasCompared: a stored host whose Unicode
// lowercase is another domain (a capital dotted I, a capital sharp s) is
// adopted in its own spelling with only its ASCII letters lowercased, so the
// address stored and pinged dials the key that was compared, for a
// scheme-only change and for a "www." change alike.
func TestPlan_StoresTheHostThatWasCompared(t *testing.T) {
	cases := []struct {
		stored, reported, to, key string
	}{
		{"http://İstanbul.test", "https://İstanbul.test", "https://İstanbul.test", "xn--istanbul-o0e.test"},
		{"https://İstanbul.test", "https://www.İstanbul.test", "https://www.İstanbul.test", "www.xn--istanbul-o0e.test"},
		{"http://STRAẞE.test", "https://STRAẞE.test", "https://straẞe.test", "strasse.test"},
		{"https://STRAẞE.test", "https://www.STRAẞE.test", "https://www.straẞe.test", "www.strasse.test"},
		{"https://www.STRAẞE.test/", "https://STRAẞE.test", "https://straẞe.test/", "strasse.test"},
	}
	for _, p := range planners {
		for _, c := range cases {
			got := p.plan(c.stored, c.reported)
			if got.Decision != Adopt || got.To != c.to {
				t.Errorf("%s(%q, %q) = %+v, want Adopt %q", p.name, c.stored, c.reported, got, c.to)
				continue
			}
			if k := toKey(t, got.To); k != c.key {
				t.Errorf("%s(%q, %q): To %q dials %q, compared %q", p.name, c.stored, c.reported, got.To, k, c.key)
			}
		}
	}
}

// TestPlan_RefusesAHostThatOnlyUnicodeLowercasingMatches: a reported host
// equal to the stored one only under Unicode lowercasing is another domain,
// and is never adopted.
func TestPlan_RefusesAHostThatOnlyUnicodeLowercasingMatches(t *testing.T) {
	cases := [][2]string{
		{"http://İstanbul.test", "https://istanbul.test"},
		{"https://İstanbul.test", "https://www.istanbul.test"},
		{"http://STRAẞE.test", "https://straße.test"},
		{"https://STRAẞE.test", "https://www.straße.test"},
	}
	for _, p := range planners {
		for _, c := range cases {
			if got := p.plan(c[0], c[1]); got.Decision != Mismatch {
				t.Errorf("%s(%q, %q) = %+v, want Mismatch", p.name, c[0], c[1], got)
			}
		}
	}
}

// TestPlan_RefusesAnAddressThatDoesNotDialTheComparedKey: when the address a
// plan would return dials neither the stored host's key nor that key's "www."
// sibling, or the stored host has no key, nothing is adopted. A fullwidth
// "ＷＷＷ" label is not the ASCII "www." label, so adding "www." to it names
// www.www.example.test, not example.test.
func TestPlan_RefusesAnAddressThatDoesNotDialTheComparedKey(t *testing.T) {
	cases := [][2]string{
		{"https://ＷＷＷ.example.test", "https://www.ＷＷＷ.example.test"},
		{"http://bü_cher.test", "https://bü_cher.test"}, // non-ASCII, and IDNA refuses the label
	}
	for _, p := range planners {
		for _, c := range cases {
			if got := p.plan(c[0], c[1]); got.Decision != Mismatch {
				t.Errorf("%s(%q, %q) = %+v, want Mismatch", p.name, c[0], c[1], got)
			}
		}
	}
}

// TestPlan_MixedCaseASCIIAdopts: ASCII-only lowercasing still treats every
// ASCII case spelling of a host as that host.
func TestPlan_MixedCaseASCIIAdopts(t *testing.T) {
	cases := []struct{ stored, reported, to string }{
		{"http://Example.COM", "https://EXAMPLE.com", "https://example.com"},
		{"https://Example.COM/", "https://WWW.Example.com", "https://www.example.com/"},
		{"HTTP://WWW.EXAMPLE.COM", "https://example.com", "https://example.com"},
		{"http://BÜCHER.de", "https://www.BÜCHER.de", "https://www.bÜcher.de"},
	}
	for _, p := range planners {
		for _, c := range cases {
			if got := p.plan(c.stored, c.reported); got.Decision != Adopt || got.To != c.to {
				t.Errorf("%s(%q, %q) = %+v, want Adopt %q", p.name, c.stored, c.reported, got, c.to)
			}
		}
	}
}

// TestPlan_ToDialsTheComparedKey: for every address either rule adopts, the
// host of To dials the stored host's key (a scheme-only change) or that key's
// "www." sibling (a host change).
func TestPlan_ToDialsTheComparedKey(t *testing.T) {
	pairs := [][2]string{
		{"http://example.com", "https://example.com"},
		{"https://Example.com/blog/", "https://www.example.com/blog"},
		{"http://İstanbul.test", "https://www.İstanbul.test"},
		{"https://www.İstanbul.test", "https://İstanbul.test"},
		{"http://STRAẞE.test:8443/", "https://www.STRAẞE.test:8443"},
		{"https://bücher.de", "https://www.bücher.de"},
		{"http://my_site.test", "https://www.my_site.test"},
		{"http://[2001:DB8::1]", "https://[2001:db8::1]"},
	}
	for _, p := range planners {
		for _, c := range pairs {
			got := p.plan(c[0], c[1])
			if got.Decision != Adopt {
				t.Errorf("%s(%q, %q) = %+v, want Adopt", p.name, c[0], c[1], got)
				continue
			}
			su, _ := url.Parse(c[0])
			want, ok := HostKey(su.Hostname())
			if !ok {
				t.Fatalf("stored %q has no key", c[0])
			}
			s, _, _ := Parse(c[0])
			a, _, _ := Parse(got.To)
			if a.Host != s.Host {
				if want, ok = WWWSibling(want); !ok {
					t.Fatalf("the key of %q has no www sibling", c[0])
				}
			}
			if k := toKey(t, got.To); k != want {
				t.Errorf("%s(%q, %q): To %q dials %q, compared %q", p.name, c[0], c[1], got.To, k, want)
			}
		}
	}
}

// TestHostKey_ASCIIIsDialledAsWritten: an all-ASCII host is its own key,
// ASCII-lowercased, with no IDNA validation, as net/http dials it; only a
// non-ASCII host goes through IDNA, and one IDNA refuses has no key.
func TestHostKey_ASCIIIsDialledAsWritten(t *testing.T) {
	cases := []struct {
		host, key string
		ok        bool
	}{
		{"my_site.test", "my_site.test", true},
		{"MY_SITE.Test", "my_site.test", true},
		{"-lead.test", "-lead.test", true},
		{"XN--BCHER-KVA.de", "xn--bcher-kva.de", true},
		{"2001:DB8::1", "2001:db8::1", true},
		{"bücher.de", "xn--bcher-kva.de", true},
		{"İstanbul.test", "xn--istanbul-o0e.test", true},
		{"STRAẞE.test", "strasse.test", true},
		{"bü_cher.test", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		k, ok := HostKey(c.host)
		if k != c.key || ok != c.ok {
			t.Errorf("HostKey(%q) = %q %v, want %q %v", c.host, k, ok, c.key, c.ok)
		}
	}
}

// TestPlanStrict_UnderscoreHostKeepsTheUpgrade: a host IDNA's lookup profile
// would refuse, but net/http dials, still gets the https upgrade and the
// "www." toggle.
func TestPlanStrict_UnderscoreHostKeepsTheUpgrade(t *testing.T) {
	cases := []struct{ stored, reported, to string }{
		{"http://my_site.test", "https://my_site.test", "https://my_site.test"},
		{"https://my_site.test", "https://www.my_site.test", "https://www.my_site.test"},
		{"http://WWW.My_Site.test/", "https://my_site.test", "https://my_site.test/"},
	}
	for _, c := range cases {
		if got := PlanStrict(c.stored, c.reported); got.Decision != Adopt || got.To != c.to {
			t.Errorf("PlanStrict(%q, %q) = %+v, want Adopt %q", c.stored, c.reported, got, c.to)
		}
	}
}

// TestSameAddress: an empty path and "/" are one address, and so are "/blog"
// and "/blog/"; any other difference is not.
func TestSameAddress(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"https://www.x.test", "https://www.x.test/", true},
		{"https://www.x.test/", "https://www.x.test", true},
		{"https://www.x.test/blog", "https://www.x.test/blog/", true},
		{"HTTPS://WWW.X.test:443/", "https://www.x.test", true},
		{"https://www.x.test/", "https://www.x.test/blog", false},
		{"https://www.x.test", "https://x.test", false},
		{"https://www.x.test", "http://www.x.test", false},
		{"https://www.x.test", "https://www.x.test:8443", false},
		{"https://İstanbul.test", "https://istanbul.test", false},
		{"", "", false},
		{"not a url", "not a url", false},
	}
	for _, c := range cases {
		if got := SameAddress(c.a, c.b); got != c.want {
			t.Errorf("SameAddress(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// TestPlan_TrailingSlash: a saved address with a trailing slash adopts its
// "www." sibling and keeps the slash; a reported address with one adopts over
// a saved address without; a path that differs by more than a slash is still
// refused.
func TestPlan_TrailingSlash(t *testing.T) {
	cases := []struct {
		stored, reported string
		want             Decision
		to               string
	}{
		{"https://x.test/", "https://www.x.test", Adopt, "https://www.x.test/"},
		{"https://x.test/", "https://www.x.test/", Adopt, "https://www.x.test/"},
		{"https://x.test", "https://www.x.test/", Adopt, "https://www.x.test"},
		{"http://x.test/", "https://x.test", Adopt, "https://x.test/"},
		{"https://x.test/", "https://x.test", Same, ""},
		{"https://x.test/", "https://www.x.test/blog", Mismatch, ""},
		{"https://x.test/blog/", "https://www.x.test/", Mismatch, ""},
	}
	for _, p := range planners {
		for _, c := range cases {
			if got := p.plan(c.stored, c.reported); got.Decision != c.want || got.To != c.to {
				t.Errorf("%s(%q, %q) = %+v, want %v %q", p.name, c.stored, c.reported, got, c.want, c.to)
			}
		}
	}
}
