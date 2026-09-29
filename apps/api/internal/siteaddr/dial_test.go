package siteaddr

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"unicode/utf8"
)

// errNoDial stops every request at its dial: nothing reaches the network.
var errNoDial = errors.New("dial refused by the test")

// dialledHost sends a request for host through a real http.Transport whose
// dialer records the host it was asked to dial and refuses the dial. It
// returns the URL's Hostname and the dialled host, and dialled=false when
// the transport never dialled (the URL did not parse, or the request was
// refused first).
func dialledHost(t *testing.T, host string) (hostname, got string, dialled bool) {
	t.Helper()
	hostPart := host
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		hostPart = "[" + host + "]"
	}
	u, err := url.Parse("http://" + hostPart + "/")
	if err != nil {
		return "", "", false
	}
	tr := &http.Transport{
		Proxy: nil,
		DialContext: func(_ context.Context, _, addr string) (net.Conn, error) {
			h, _, splitErr := net.SplitHostPort(addr)
			if splitErr != nil {
				h = addr
			}
			got, dialled = h, true
			return nil, errNoDial
		},
	}
	defer tr.CloseIdleConnections()
	req := &http.Request{Method: http.MethodGet, URL: u, Host: u.Host, Header: http.Header{}}
	if _, err := tr.RoundTrip(req); err == nil {
		t.Fatalf("%q: RoundTrip succeeded through a dialer that refuses every dial", host)
	}
	return u.Hostname(), got, dialled
}

// trickyHosts are spellings whose dialled form a change in IDNA mapping
// would move: letters whose Unicode case or folding is another domain, a
// final sigma, fullwidth letters and dots, a trailing dot, mixed-case
// Punycode, a label IDNA refuses but net/http dials, and IP literals.
var trickyHosts = []string{
	"İstanbul.test", "istanbul.test", "i̇stanbul.test",
	"STRAẞE.test", "straẞe.test", "straße.test", "strasse.test",
	"ΣΑΣ.test", "σας.test", "σασ.test", "ὈΔΥΣΣΕΎΣ.test",
	"ｅｘａｍｐｌｅ．ｃｏｍ", "ＥＸＡＭＰＬＥ.com", "example。com", "bücher｡de",
	"example.com.", "bücher.de.", "BÜCHER.de",
	"XN--BCHER-KVA.de", "xn--bcher-kva.DE", "Xn--Bcher-Kva.De",
	"my_site.test", "MY_SITE.test", "bü_cher.test",
	"192.0.2.10", "2001:DB8::1", "::ffff:192.0.2.10",
	"ǅemal.test", "ǆemal.test", "ﬀ.test", "ﬁle.test", "Ⅻ.test", "㎒.test",
}

// sampledHosts is a fixed, deterministic sample across the Basic
// Multilingual Plane: every code point at a fixed stride from U+00A0,
// skipping surrogates, each placed in a label in upper and lower ASCII
// context.
func sampledHosts() []string {
	const stride = 211
	var out []string
	for r := rune(0xA0); r <= 0xFFFD; r += stride {
		if r >= 0xD800 && r <= 0xDFFF || !utf8.ValidRune(r) {
			continue
		}
		out = append(out, string(r)+"ab.test", "X"+string(r)+"Y.Test")
	}
	return out
}

// TestHostKey_IsTheHostNetHTTPDials ties HostKey to what net/http actually
// does with a host: for every host HostKey accepts, the host a real
// http.Transport asks its dialer for is HostKey's result once its ASCII
// letters are lowercased. A Go or golang.org/x/net update that changes the
// IDNA mapping one of them applies turns this red, instead of silently
// letting two spellings count as one host while they dial two.
func TestHostKey_IsTheHostNetHTTPDials(t *testing.T) {
	hosts := append(append([]string(nil), trickyHosts...), sampledHosts()...)
	compared := 0
	for _, host := range hosts {
		hostname, got, dialled := dialledHost(t, host)
		if hostname == "" {
			continue
		}
		key, ok := HostKey(hostname)
		if !ok {
			continue
		}
		if !dialled {
			t.Errorf("%q: HostKey accepts it as %q, but net/http never dialled it", host, key)
			continue
		}
		compared++
		if LowerASCII(got) != key {
			t.Errorf("%q: net/http dialled %q, HostKey is %q", host, got, key)
		}
	}
	// A sweep that compares nothing proves nothing.
	if floor := len(trickyHosts) + 100; compared < floor {
		t.Fatalf("compared %d hosts, want at least %d: the sample stopped reaching the dialer", compared, floor)
	}
	t.Logf("compared the dialled host with HostKey for %d of %d hosts", compared, len(hosts))
}

// TestHostKey_TrickyHostsAllDial: each of these tricky hosts must be accepted
// by HostKey and reach the dialer, so the property test above cannot pass by
// skipping the cases it exists for.
func TestHostKey_TrickyHostsAllDial(t *testing.T) {
	for _, host := range []string{
		"İstanbul.test", "STRAẞE.test", "ΣΑΣ.test", "σας.test", "ｅｘａｍｐｌｅ．ｃｏｍ",
		"example.com.", "XN--BCHER-KVA.de", "my_site.test", "192.0.2.10", "2001:DB8::1",
	} {
		hostname, got, dialled := dialledHost(t, host)
		key, ok := HostKey(hostname)
		if !ok || !dialled {
			t.Errorf("%q: HostKey ok=%v, dialled=%v; want both", host, ok, dialled)
			continue
		}
		if LowerASCII(got) != key {
			t.Errorf("%q: net/http dialled %q, HostKey is %q", host, got, key)
		}
	}
}
