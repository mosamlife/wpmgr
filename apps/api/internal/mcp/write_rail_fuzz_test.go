package mcp

import (
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/mosamlife/wpmgr/apps/api/internal/siteaddr"
)

// The DB backstops, restated here independently of the rail's own mirrors, so
// a mirror that drifts from the CHECK cannot make this test agree with it.
var (
	fuzzStoredURL = regexp.MustCompile(`^https?://[!-~]+$`)
	fuzzSiteHost  = regexp.MustCompile(`^[!-~]{1,255}$`)
)

// FuzzRailStoredURLSatisfiesBackstop drives the creation rail, both scopes,
// over arbitrary stored site addresses and model-written pages. Whatever it
// accepts satisfies the url_backstop and site_host_ascii CHECKs and stays on
// the site's host; whatever it refuses is refused as -32003 or -32014, and it
// never answers -32603.
func FuzzRailStoredURLSatisfiesBackstop(f *testing.F) {
	sites := []string{
		"https://bücher.de/shop",
		"https://SHOP.Example.com",
		"https://shop.example.com/",
		"http://[::1]:8080/blog",
		"https://[2001:db8::1]",
		"https://x.test:8443",
		"http://x.test:443/a/b",
		"https://bü_cher.test",
		"https://x.test/?p=1",
		"https://ÉCOLE.fr/Été",
		"https://xn--bcher-kva.de",
		"https://" + strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 62) + ".e",
		"ftp://x.test",
		"",
	}
	paths := []string{"", "sale/", "a/b", "../x", "a/%2e%2e/b", "a%2fb", "a?b", "a#b", "a b", "%E2%80%AE", "~user/", strings.Repeat("p", 2100)}
	for _, s := range sites {
		for _, p := range paths {
			for _, all := range []bool{true, false} {
				f.Add(s, p, all, uint8(0))
			}
		}
		f.Add(s, "x", false, uint8(1))
		f.Add(s, "x", false, uint8(2))
	}

	f.Fuzz(func(t *testing.T, siteURL, path string, scopeAll bool, portMode uint8) {
		// Model paths are printable ASCII: the grammar refuses anything else
		// at step 1, which the argument tests already pin.
		path = strings.Map(func(r rune) rune {
			if r < 0x20 || r > 0x7e {
				return -1
			}
			return r
		}, path)

		fk := newRailFake()
		s := fk.addSite(siteURL)
		env := newRailEnv(t, fk, nil)
		_, siteUsable := siteAddressOf(siteURL)

		var args map[string]any
		var page string
		if scopeAll {
			args = allReq(s.ID)
		} else {
			page = fuzzPageFor(siteURL, path, portMode)
			args = urlReq(s.ID, page)
		}
		c := env.call(ToolSiteCachePurgeRequest, args)

		switch c.code {
		case 0:
		case codeInternalError:
			t.Fatalf("the rail answered -32603 for site %q, args %v: %s", siteURL, args, c.raw)
		case codeInvalidToolArguments:
			if scopeAll {
				t.Fatalf("scope all was refused as -32003 for site %q: %s", siteURL, c.raw)
			}
			return
		case codeSiteAddressUnusable:
			if siteUsable {
				t.Fatalf("a usable site %q answered -32014: %s", siteURL, c.raw)
			}
			return
		default:
			t.Fatalf("unexpected code %d for site %q, args %v: %s", c.code, siteURL, args, c.raw)
		}
		if !siteUsable {
			t.Fatalf("the rail accepted a request on an unusable site address %q", siteURL)
		}

		rows := fk.rowsFor(env.auth.GrantID)
		if len(rows) != 1 {
			t.Fatalf("accepted, but %d rows were written", len(rows))
		}
		row := rows[0]
		if !fuzzSiteHost.MatchString(row.SiteHost) {
			t.Fatalf("site_host %q fails the site_host_ascii CHECK (site %q)", row.SiteHost, siteURL)
		}
		if scopeAll {
			if row.Url != nil {
				t.Fatalf("a whole-site request stored url %q", *row.Url)
			}
			return
		}
		if row.Url == nil {
			t.Fatal("a page request stored no url")
		}
		u := *row.Url
		if len(u) > 2048 || !fuzzStoredURL.MatchString(u) || strings.ContainsAny(u, "?#") {
			t.Fatalf("stored url %q fails the url_backstop CHECK (site %q, page %q)", u, siteURL, page)
		}
		pu, err := url.Parse(u)
		su, serr := url.Parse(siteURL)
		if err != nil || serr != nil || !siteaddr.SameHost(pu.Hostname(), su.Hostname()) {
			t.Fatalf("stored url %q is not on the site's host %q", u, siteURL)
		}
		if pu.Hostname() != row.SiteHost {
			t.Fatalf("stored url %q does not carry the stored site_host %q", u, row.SiteHost)
		}
	})
}

// fuzzPageFor builds the model's page on the site's own host: its ASCII key
// on even portMode, else the host as the site stored it (so IDN and mixed case
// reach the grammar), with the port
// written per portMode (0 none, 1 the default for the scheme, 2 a foreign
// port), and path appended.
func fuzzPageFor(siteURL, path string, portMode uint8) string {
	su, err := url.Parse(siteURL)
	if err != nil || su.Host == "" {
		return "https://x.test/" + path
	}
	host := su.Hostname()
	if hk, ok := siteaddr.HostKey(host); ok && portMode%2 == 0 {
		host = hk
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	switch portMode % 3 {
	case 1:
		if su.Scheme == "http" {
			host += ":80"
		} else {
			host += ":443"
		}
	case 2:
		host += ":9"
	default:
		if p := su.Port(); p != "" {
			host += ":" + p
		}
	}
	base := strings.TrimRight(su.EscapedPath(), "/")
	return "https://" + host + base + "/" + path
}
