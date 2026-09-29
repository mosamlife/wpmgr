package siteaddr

import (
	"net/url"
	"testing"
)

// TestPlan_OneHostKeyIsOneAddress: two addresses with the same scheme, port
// and path whose hosts have one HostKey dial the same host, so they are one
// address under both rules and under SameAddress. A host that only Unicode
// lowercasing makes equal has another key and is still a mismatch, and ASCII
// letter case alone is still the same address.
func TestPlan_OneHostKeyIsOneAddress(t *testing.T) {
	cases := []struct {
		name, a, b string
		want       Decision
	}{
		{"non-ASCII letter case", "https://BÜCHER.de", "https://bücher.de", Same},
		{"non-ASCII letter case, reversed", "https://bücher.de", "https://BÜCHER.de", Same},
		{"non-ASCII letter case, path and slash", "https://BÜCHER.de/blog/", "https://bücher.de/blog", Same},
		{"Unicode and Punycode spellings", "https://bücher.de", "https://xn--bcher-kva.de", Same},
		{"ASCII letter case only", "https://Example.COM", "https://example.com", Same},
		{"ASCII letter case, default port and slash", "http://EXAMPLE.com:80/", "http://example.com", Same},
		{"dotted capital I vs its Unicode lowercase", "https://İstanbul.test", "https://istanbul.test", Mismatch},
		{"capital sharp s vs its Unicode lowercase", "https://STRAẞE.test", "https://straße.test", Mismatch},
		{"one key, another port", "https://BÜCHER.de", "https://bücher.de:8443", Mismatch},
		{"one key, another path", "https://BÜCHER.de", "https://bücher.de/blog", Mismatch},
		{"one key, a downgrade", "https://BÜCHER.de", "http://bücher.de", Mismatch},
	}
	for _, c := range cases {
		for _, p := range planners {
			if got := p.plan(c.a, c.b); got.Decision != c.want || got.To != "" {
				t.Errorf("%s: %s(%q, %q) = %+v, want %v", c.name, p.name, c.a, c.b, got, c.want)
			}
		}
		if got := SameAddress(c.a, c.b); got != (c.want == Same) {
			t.Errorf("%s: SameAddress(%q, %q) = %v, want %v", c.name, c.a, c.b, got, c.want == Same)
		}
	}
}

// TestPlanStrict_SameNeedsAHostKey: the one answer PlanStrict does not take
// from Plan. Two equal spellings of a non-ASCII host that does not convert are
// the same address to enrollment's Plan and a mismatch to PlanStrict, which
// names only hosts a request can be dialled to.
func TestPlanStrict_SameNeedsAHostKey(t *testing.T) {
	const a = "https://bü_cher.test"
	if got := Plan(a, a+"/"); got.Decision != Same {
		t.Errorf("Plan(%q, %q) = %+v, want Same", a, a+"/", got)
	}
	if got := PlanStrict(a, a+"/"); got.Decision != Mismatch {
		t.Errorf("PlanStrict(%q, %q) = %+v, want Mismatch", a, a+"/", got)
	}
}

// adoptCorpus spells hosts in the forms the rule has to keep apart: ASCII and
// non-ASCII letter case, Unicode and Punycode, the "www." label and
// look-alikes of it, spellings that only Unicode lowercasing or case folding
// makes equal, hosts IDNA refuses, IP literals and single-label hosts.
var adoptCorpus = []string{
	"example.com", "Example.COM", "www.example.com", "WWW.Example.com",
	"bücher.de", "BÜCHER.de", "www.bücher.de", "WWW.BÜCHER.de", "xn--bcher-kva.de", "www.xn--bcher-kva.de",
	"İstanbul.test", "istanbul.test", "www.İstanbul.test", "www.istanbul.test",
	"STRAẞE.test", "straße.test", "strasse.test", "www.STRAẞE.test", "www.straße.test",
	"ＷＷＷ.example.test", "www.ＷＷＷ.example.test", "example.test", "www.example.test",
	"σ.gr", "ς.gr", "www.σ.gr",
	"my_site.test", "www.my_site.test", "bü_cher.test", "www.bü_cher.test",
	"192.0.2.10", "www.192.0.2.10", "[2001:db8::1]", "[2001:DB8::1]",
	"intranet", "www.intranet", "example.com.", "www.example.com.",
}

// adoptCorpusAddresses is every corpus host under each scheme, with and
// without an explicit port, and with and without a path.
func adoptCorpusAddresses() []string {
	var out []string
	for _, h := range adoptCorpus {
		for _, scheme := range []string{"http", "https"} {
			for _, port := range []string{"", ":8443"} {
				for _, path := range []string{"", "/blog/"} {
					out = append(out, scheme+"://"+h+port+path)
				}
			}
		}
	}
	return out
}

// hostOf is the Hostname of an address that parses.
func hostOf(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("%q does not parse: %v", raw, err)
	}
	return u.Hostname()
}

// TestPlan_AdoptDialsTheReportedKey pins the one place the dialled host is
// enforced. Over every pair of corpus addresses, whenever Plan answers Adopt,
// the address it returns dials the stored host's key (a scheme-only change)
// or that key's "www." sibling (a host change), and the reported host has
// exactly that key; PlanStrict answers what Plan answers except a Same for a
// host with no key; and every Same names one host by key or by its spelling.
func TestPlan_AdoptDialsTheReportedKey(t *testing.T) {
	addrs := adoptCorpusAddresses()
	adopts := 0
	for _, stored := range addrs {
		for _, reported := range addrs {
			p := Plan(stored, reported)
			ps := PlanStrict(stored, reported)
			sh, rh := hostOf(t, stored), hostOf(t, reported)
			switch p.Decision {
			case Adopt:
				adopts++
				ks, okS := HostKey(sh)
				kr, okR := HostKey(rh)
				kt, okT := addressHostKey(p.To)
				sib, okSib := WWWSibling(ks)
				if !okS || !okR || !okT || kt != kr || (kt != ks && (!okSib || kt != sib)) {
					t.Errorf("Plan(%q, %q) adopts %q: stored key %q, reported key %q, adopted key %q", stored, reported, p.To, ks, kr, kt)
				}
				if ps != p {
					t.Errorf("PlanStrict(%q, %q) = %+v, Plan = %+v", stored, reported, ps, p)
				}
			case Same:
				s, _, _ := Parse(stored)
				r, _, _ := Parse(reported)
				if s.Host != r.Host && !SameHost(sh, rh) {
					t.Errorf("Plan(%q, %q) = Same for hosts %q and %q", stored, reported, sh, rh)
				}
				want := Same
				if !SameHost(sh, rh) {
					want = Mismatch
				}
				if ps.Decision != want {
					t.Errorf("PlanStrict(%q, %q) = %+v, want %v", stored, reported, ps, want)
				}
			default:
				if ps != p {
					t.Errorf("PlanStrict(%q, %q) = %+v, Plan = %+v", stored, reported, ps, p)
				}
			}
		}
	}
	// A guard that finds nothing must go red: the corpus has to exercise
	// Adopt, or the checks above assert nothing.
	if adopts == 0 {
		t.Fatalf("no pair of %d corpus addresses was adopted", len(addrs))
	}
	t.Logf("%d addresses, %d pairs, %d adopted", len(addrs), len(addrs)*len(addrs), adopts)
}
