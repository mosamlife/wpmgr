package site

import (
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/mosamlife/wpmgr/apps/api/internal/siteaddr"
)

func TestPlanEnrollURL(t *testing.T) {
	cases := []struct {
		name     string
		stored   string
		reported string
		want     enrollURLDecision
		to       string
	}{
		// Equal once normalised: nothing changes.
		{"identical", "https://example.com", "https://example.com", enrollURLSame, ""},
		{"host case", "https://Example.COM", "https://example.com", enrollURLSame, ""},
		{"trailing slash", "https://example.com", "https://example.com/", enrollURLSame, ""},
		{"explicit default https port", "https://example.com", "https://example.com:443/", enrollURLSame, ""},
		{"explicit default http port", "http://example.com:80", "http://example.com", enrollURLSame, ""},
		{"subdirectory trailing slash", "https://example.com/blog/", "https://example.com/blog", enrollURLSame, ""},
		// Hosts spelt differently that dial one HostKey are one host.
		{"non-ASCII letter case only", "https://BÜCHER.de", "https://bücher.de", enrollURLSame, ""},
		{"idn unicode vs punycode", "https://bücher.example", "https://xn--bcher-kva.example", enrollURLSame, ""},

		// Adopted: a leading www. and/or http to https, same port and path.
		{"apex to www", "https://example.com", "https://www.example.com", enrollURLAdopt, "https://www.example.com"},
		{"www to apex", "https://www.example.com", "https://example.com/", enrollURLAdopt, "https://example.com"},
		{"http to https", "http://example.com", "https://example.com", enrollURLAdopt, "https://example.com"},
		{"http apex to https www", "http://example.com", "https://www.example.com", enrollURLAdopt, "https://www.example.com"},
		{"stored trailing slash kept", "https://example.com/", "https://www.example.com", enrollURLAdopt, "https://www.example.com/"},
		{"stored host case normalised", "https://Example.com", "https://www.example.com", enrollURLAdopt, "https://www.example.com"},
		{"subdirectory www toggle", "https://example.com/blog", "https://www.example.com/blog/", enrollURLAdopt, "https://www.example.com/blog"},
		{"explicit port kept", "https://example.com:8443", "https://www.example.com:8443", enrollURLAdopt, "https://www.example.com:8443"},
		{"same explicit port upgrade", "http://example.com:8080", "https://example.com:8080", enrollURLAdopt, "https://example.com:8080"},
		{"http default port to https default port", "http://example.com:80", "https://example.com", enrollURLAdopt, "https://example.com"},
		{"ipv4 upgrade", "http://192.0.2.10", "https://192.0.2.10", enrollURLAdopt, "https://192.0.2.10"},
		{"ipv6 upgrade", "http://[2001:db8::1]", "https://[2001:db8::1]", enrollURLAdopt, "https://[2001:db8::1]"},
		{"idn www toggle, same form", "https://bücher.example", "https://www.bücher.example", enrollURLAdopt, "https://www.bücher.example"},
		// A host is adopted in its own spelling with only its ASCII letters
		// lowercased, so it dials the key that was compared.
		{"capital dotted I upgrade keeps its spelling", "http://İstanbul.test", "https://İstanbul.test", enrollURLAdopt, "https://İstanbul.test"},
		{"capital dotted I www toggle", "https://İstanbul.test", "https://www.İstanbul.test", enrollURLAdopt, "https://www.İstanbul.test"},
		{"capital sharp s upgrade keeps its spelling", "http://STRAẞE.test", "https://STRAẞE.test", enrollURLAdopt, "https://straẞe.test"},
		{"capital sharp s www toggle", "https://STRAẞE.test", "https://www.STRAẞE.test", enrollURLAdopt, "https://www.straẞe.test"},
		{"underscore label upgrade", "http://my_site.test", "https://my_site.test", enrollURLAdopt, "https://my_site.test"},

		// Flagged: anything else keeps the stored address.
		{"https to http downgrade", "https://example.com", "http://example.com", enrollURLMismatch, ""},
		{"downgrade with www toggle", "https://www.example.com", "http://example.com", enrollURLMismatch, ""},
		{"other subdomain", "https://example.com", "https://blog.example.com", enrollURLMismatch, ""},
		{"www to other subdomain", "https://www.example.com", "https://shop.example.com", enrollURLMismatch, ""},
		{"other host", "https://example.com", "https://attacker.test", enrollURLMismatch, ""},
		{"lookalike without dot", "https://example.com", "https://wwwexample.com", enrollURLMismatch, ""},
		{"double www", "https://www.example.com", "https://www.www.example.com", enrollURLMismatch, ""},
		{"suffix host", "https://example.com", "https://example.com.attacker.test", enrollURLMismatch, ""},
		{"path difference", "https://example.com", "https://example.com/blog", enrollURLMismatch, ""},
		{"path difference with www", "https://example.com/blog", "https://www.example.com/shop", enrollURLMismatch, ""},
		{"port difference", "https://example.com", "https://example.com:8443", enrollURLMismatch, ""},
		{"port difference with www", "https://example.com:8443", "https://www.example.com:9443", enrollURLMismatch, ""},
		{"http non-default port to https default", "http://example.com:8080", "https://example.com", enrollURLMismatch, ""},
		{"ip literal www", "https://192.0.2.10", "https://www.192.0.2.10", enrollURLMismatch, ""},
		{"single-label host www", "http://intranet", "http://www.intranet", enrollURLMismatch, ""},
		{"trailing-dot host", "https://example.com", "https://example.com.", enrollURLMismatch, ""},
		{"reported userinfo", "https://example.com", "https://user:pass@www.example.com", enrollURLMismatch, ""},
		{"reported query", "https://example.com", "https://www.example.com/?p=1", enrollURLMismatch, ""},
		{"reported fragment", "https://example.com", "https://www.example.com/#x", enrollURLMismatch, ""},
		{"reported other scheme", "https://example.com", "ftp://www.example.com", enrollURLMismatch, ""},
		{"reported empty", "https://example.com", "", enrollURLMismatch, ""},
		{"stored unparseable", "not a url", "https://www.example.com", enrollURLMismatch, ""},
		// Equal only under Unicode lowercasing: another name, or one kept
		// apart rather than guessed at.
		{"dotted I vs its Unicode lowercase", "http://İstanbul.test", "https://istanbul.test", enrollURLMismatch, ""},
		{"dotted I www vs its Unicode lowercase", "https://İstanbul.test", "https://www.istanbul.test", enrollURLMismatch, ""},
		{"sharp s vs its Unicode lowercase", "https://STRAẞE.test", "https://straße.test", enrollURLMismatch, ""},
		{"sharp s upgrade vs its Unicode lowercase", "http://STRAẞE.test", "https://straße.test", enrollURLMismatch, ""},
		{"fullwidth www label", "https://ＷＷＷ.example.test", "https://www.ＷＷＷ.example.test", enrollURLMismatch, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := planEnrollURL(c.stored, c.reported)
			if got.Decision != c.want || got.To != c.to {
				t.Fatalf("planEnrollURL(%q, %q) = {%d %q}, want {%d %q}", c.stored, c.reported, got.Decision, got.To, c.want, c.to)
			}
			if got.Decision != enrollURLAdopt {
				return
			}
			// Whatever is adopted is derived from the stored address: the host
			// is the stored host or its www sibling, the scheme is the stored
			// scheme or https, and the port and path are unchanged.
			s, _, _ := parseSiteAddress(c.stored)
			a, _, ok := parseSiteAddress(got.To)
			if !ok {
				t.Fatalf("adopted address %q does not parse", got.To)
			}
			sibling, _ := wwwSibling(s.Host)
			if a.Host != s.Host && a.Host != sibling {
				t.Fatalf("adopted host %q is neither %q nor its www sibling", a.Host, s.Host)
			}
			if a.Scheme != s.Scheme && a.Scheme != "https" {
				t.Fatalf("adopted scheme %q from stored %q", a.Scheme, s.Scheme)
			}
			if a.Port != s.Port || a.Path != s.Path {
				t.Fatalf("adopted port/path %q/%q differ from stored %q/%q", a.Port, a.Path, s.Port, s.Path)
			}
			if s.Scheme == "https" && a.Scheme != "https" {
				t.Fatalf("adopted a downgrade: %q", got.To)
			}
			// The adopted address dials the stored host's key (a scheme-only
			// change) or that key's www sibling (a host change).
			su, _ := url.Parse(c.stored)
			tu, _ := url.Parse(got.To)
			want, okW := siteaddr.HostKey(su.Hostname())
			if a.Host != s.Host {
				want, okW = siteaddr.WWWSibling(want)
			}
			key, okK := siteaddr.HostKey(tu.Hostname())
			if !okW || !okK || key != want {
				t.Fatalf("adopted address %q dials %q, compared %q", got.To, key, want)
			}
		})
	}
}

func TestSiteURLVariants(t *testing.T) {
	got := siteURLVariants("https://example.com")
	want := []string{
		"https://example.com",
		"https://example.com/",
		"https://www.example.com",
		"https://www.example.com/",
		"http://example.com",
		"http://example.com/",
		"http://www.example.com",
		"http://www.example.com/",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("variants:\n got %q\nwant %q", got, want)
	}

	// The exact spelling is always first, even when it is not canonical.
	if v := siteURLVariants("https://Example.com/blog/"); v[0] != "https://Example.com/blog/" {
		t.Fatalf("exact spelling not first: %q", v)
	}
	for _, v := range siteURLVariants("https://example.com/blog") {
		if !strings.HasSuffix(strings.TrimSuffix(v, "/"), "/blog") {
			t.Fatalf("variant %q dropped the path", v)
		}
	}
	for _, v := range siteURLVariants("https://example.com:8443") {
		u, err := url.Parse(v)
		if err != nil || u.Port() != "8443" {
			t.Fatalf("variant %q dropped the port", v)
		}
	}
	// No www variant for an IP literal, and an unparseable input is only itself.
	for _, v := range siteURLVariants("https://192.0.2.10") {
		if strings.Contains(v, "www.") {
			t.Fatalf("www variant for an IP literal: %q", v)
		}
	}
	if v := siteURLVariants("not a url"); len(v) != 1 {
		t.Fatalf("unparseable input produced variants: %q", v)
	}
}

func TestSanitizeReportedURL(t *testing.T) {
	cases := map[string]string{
		"https://user:secret@www.example.com/?token=abc#frag": "https://www.example.com/",
		"https://www.example.com":                             "https://www.example.com",
		"http://[::1":                                         "",
	}
	for in, want := range cases {
		if got := sanitizeReportedURL(in); got != want {
			t.Fatalf("sanitizeReportedURL(%q) = %q, want %q", in, got, want)
		}
	}
	if got := sanitizeReportedURL("https://example.com/" + strings.Repeat("a", 5000)); len(got) > 2048 {
		t.Fatalf("not capped: %d bytes", len(got))
	}
}
