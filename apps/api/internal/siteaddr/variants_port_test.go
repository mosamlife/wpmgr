package siteaddr

import (
	"net/url"
	"slices"
	"testing"
)

// TestVariants_DefaultPortBothWays: an address stored with its scheme's
// default port written out is found by the spelling without it, and an
// address stored without it is found by the spelling with it, for both
// schemes, with and without "www." and a trailing slash.
func TestVariants_DefaultPortBothWays(t *testing.T) {
	pairs := []struct{ stored, added string }{
		{"http://example.com:80", "http://example.com"},
		{"https://example.com:443", "https://example.com"},
		{"https://example.com:443/", "http://www.example.com"},
		{"http://example.com:80/blog/", "https://example.com/blog"},
		{"https://www.example.com:443/blog", "https://example.com:443/blog/"},
	}
	for _, p := range pairs {
		if !slices.Contains(Variants(p.added), p.stored) {
			t.Errorf("adding %q does not find the stored %q: %q", p.added, p.stored, Variants(p.added))
		}
		if !slices.Contains(Variants(p.stored), p.added) {
			t.Errorf("adding %q does not find the stored %q: %q", p.stored, p.added, Variants(p.stored))
		}
	}
}

// TestVariants_DefaultPortOnlyWhereItIsTheDefault: a written port is only
// ever the scheme's own default (":80" on http, ":443" on https), a
// non-default port is kept on every variant, and the spellings without the
// port come first.
func TestVariants_DefaultPortOnlyWhereItIsTheDefault(t *testing.T) {
	got := Variants("https://example.com")
	firstPorted := -1
	for i, v := range got {
		u, err := url.Parse(v)
		if err != nil {
			t.Fatalf("variant %q does not parse: %v", v, err)
		}
		switch port := u.Port(); {
		case port == "":
			if firstPorted >= 0 {
				t.Errorf("variant %q without a port comes after a ported one (%q)", v, got[firstPorted])
			}
		case (u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443"):
			if firstPorted < 0 {
				firstPorted = i
			}
		default:
			t.Errorf("variant %q writes a port that is not its scheme's default", v)
		}
	}
	if firstPorted < 0 {
		t.Fatalf("no variant writes the default port: %q", got)
	}
	for _, v := range Variants("https://example.com:8443") {
		u, err := url.Parse(v)
		if err != nil || u.Port() != "8443" {
			t.Errorf("variant %q of a non-default port dropped or changed it", v)
		}
	}
}
