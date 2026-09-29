// Package siteaddr holds the one rule for when two spellings of a site
// address name the same WordPress install, and when a reported address may
// replace a stored one. It is a leaf (the standard library and
// golang.org/x/net/idna only) so that the site package, which decides
// enrollment and push-time adoption, and the agentcmd package, which suggests
// an address on a refused redirect, apply the same function rather than two
// copies of it.
package siteaddr

import (
	"net"
	"net/url"
	"strings"

	"golang.org/x/net/idna"
)

// HostKey is host in the ASCII form it is dialled by, which is what net/http
// does with it. An all-ASCII host (a name or an IP literal) is used as
// written, so its key is itself with A to Z lowercased and no IDNA
// validation: a label IDNA would refuse, such as one holding "_", still names
// the host net/http dials. A host holding any non-ASCII byte goes through
// IDNA's lookup profile (UTS #46 mapping, then Punycode), the conversion
// net/http applies before it dials such a host. ok is false when the host is
// empty or a non-ASCII host does not convert, and a caller treats that as
// "not the same host".
func HostKey(host string) (string, bool) {
	if host == "" {
		return "", false
	}
	if isASCII(host) {
		return LowerASCII(host), true
	}
	ascii, err := idna.Lookup.ToASCII(host)
	if err != nil || ascii == "" || !isASCII(ascii) {
		return "", false
	}
	return LowerASCII(ascii), true
}

// LowerASCII lowercases the letters A to Z and leaves every other byte as it
// is. It is the only lowercasing the address rule applies to a host: Unicode
// lowercasing can turn one domain's spelling into another's (a capital
// dotted I becomes a plain "i", a capital sharp s a small one), and each of
// those converts to a different name.
func LowerASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// isASCII reports whether s holds only bytes below 0x80.
func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// addressHostKey is the HostKey of the host in address, an address Join
// built.
func addressHostKey(address string) (string, bool) {
	u, err := url.Parse(address)
	if err != nil || u == nil {
		return "", false
	}
	return HostKey(u.Hostname())
}

// NormalizePath is a site address's path in the form the address rule
// compares it: its trailing slashes removed, so an empty path and "/" name
// the same address, as do "/blog" and "/blog/". Parse applies it, so Plan,
// PlanStrict and SameAddress compare paths through it, and the suggestion a
// refused redirect makes is built with it.
func NormalizePath(p string) string { return strings.TrimRight(p, "/") }

// SameAddress reports whether a and b are one site address: the same scheme,
// port and path once each is normalised by Parse (scheme lowercased, a
// default port dropped, the path through NormalizePath), and the same host,
// which is the same spelling once its ASCII letters are lowercased, or two
// spellings with one HostKey, since those dial the same host.
// "https://x.test/" and "https://x.test" are the same address, and so are
// "https://BÜCHER.de" and "https://bücher.de"; "https://x.test/blog" is not,
// and neither are "https://İstanbul.test" and "https://istanbul.test", whose
// keys differ. An address Parse refuses is the same as nothing. It is the
// comparison Plan answers Same by.
func SameAddress(a, b string) bool {
	x, xu, okA := Parse(a)
	y, yu, okB := Parse(b)
	return okA && okB && sameAddress(x, xu, y, yu)
}

// sameAddress is SameAddress for two addresses Parse accepted, each with the
// URL Parse returned for it.
func sameAddress(x Address, xu *url.URL, y Address, yu *url.URL) bool {
	if x.Scheme != y.Scheme || x.Port != y.Port || x.Path != y.Path {
		return false
	}
	return x.Host == y.Host || SameHost(xu.Hostname(), yu.Hostname())
}

// SameHost reports whether a and b name the same host once each is reduced
// to its HostKey. Unicode case folding is never used: two spellings it
// treats as equal can convert to different registrable domains (a capital
// sharp s maps to "ss", a small one to its own Punycode label), and a
// request is dialled by the converted form. A non-ASCII host that does not
// convert is the same as nothing.
func SameHost(a, b string) bool {
	ka, okA := HostKey(a)
	kb, okB := HostKey(b)
	return okA && okB && ka == kb
}

// PlanStrict is Plan, except that it answers Same only for a host that has a
// HostKey: two equal spellings of a non-ASCII host that does not convert name
// no host a request can be dialled to, and are Mismatch. Every other answer
// is Plan's. Plan is the one place the dialled host is enforced: its Adopt
// returns an address that dials the stored host's key (a scheme-only change)
// or that key's "www." sibling (a host change), and the reported host has
// that same key.
//
// Push-time adoption and a refused redirect's suggestion and self-redirect
// check use it. Enrollment uses Plan.
func PlanStrict(stored, reported string) PlanResult {
	p := Plan(stored, reported)
	if p.Decision != Same {
		return p
	}
	_, su, okS := Parse(stored)
	_, ru, okR := Parse(reported)
	if okS && okR && !SameHost(su.Hostname(), ru.Hostname()) {
		return PlanResult{Decision: Mismatch}
	}
	return p
}

// Address is a site URL reduced to the parts that decide whether two
// spellings name the same WordPress install: the scheme, the host with its
// ASCII letters lowercased (LowerASCII; any other character is kept as
// written), the port only when it is not the scheme's default, and the path
// through NormalizePath.
type Address struct {
	Scheme string
	Host   string
	Port   string
	Path   string
}

// Parse normalises raw into an Address. It refuses anything that
// is not a plain http(s) site address: a parse failure, another scheme, an
// empty host, userinfo, a query or a fragment. The parsed URL is returned too,
// so a caller can rebuild an address from the original spelling.
func Parse(raw string) (Address, *url.URL, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u == nil || u.Opaque != "" {
		return Address{}, nil, false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return Address{}, nil, false
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" {
		return Address{}, nil, false
	}
	host := LowerASCII(u.Hostname())
	if host == "" {
		return Address{}, nil, false
	}
	port := u.Port()
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		port = ""
	}
	return Address{
		Scheme: scheme,
		Host:   host,
		Port:   port,
		Path:   NormalizePath(u.Path),
	}, u, true
}

// WWWSibling returns host with a leading "www." added or removed, and false
// when the host has no such sibling. Only the apex and its exact "www." label
// pair up: an IP literal, a single-label host, and a host whose apex would
// itself start with "www." have none.
func WWWSibling(host string) (string, bool) {
	apex, sibling := host, "www."+host
	if strings.HasPrefix(host, "www.") {
		apex = strings.TrimPrefix(host, "www.")
		sibling = apex
	}
	if apex == "" || strings.HasPrefix(apex, "www.") || !strings.Contains(apex, ".") ||
		strings.HasSuffix(apex, ".") || net.ParseIP(apex) != nil {
		return "", false
	}
	return sibling, true
}

// Decision is what to do with an address the agent reports for a site that
// already has a stored address: at enrollment, on an agent push, and when a
// refused redirect suggests an address.
type Decision int

const (
	// Same: the two addresses are one address (SameAddress): equal once
	// normalised, or differing only in the spelling of a host that dials
	// the same HostKey. Nothing changes.
	Same Decision = iota
	// Adopt: the reported address differs from the stored one only by
	// a leading "www." and/or an http to https upgrade, on the same port and
	// path, a host spelt another way with the same HostKey counting as the
	// same host. The stored address is replaced by PlanResult.To.
	Adopt
	// Mismatch: any other difference. The stored address is kept and
	// the difference is flagged.
	Mismatch
)

// PlanResult is the outcome of Plan.
type PlanResult struct {
	Decision Decision
	// To is the address to store when Decision is Adopt. It is built
	// from the stored address, never copied from the reported one: only the
	// scheme and the "www." label can differ from what was stored.
	To string
}

// Plan decides whether a site's stored address may be replaced by the address
// its agent reports. Adoption is limited to a leading "www."
// toggle and/or an http to https upgrade, on the same port and path. A
// downgrade from https to http, another host or subdomain, another port and
// another path are never adopted. Same is SameAddress: a trailing slash never
// makes two addresses differ, and neither does a spelling of the host that
// dials the same HostKey ("https://BÜCHER.de" and "https://bücher.de").
//
// For Adopt, a reported host with the stored host's HostKey is the stored
// host, so "http://BÜCHER.de" reporting "https://bücher.de" is a scheme-only
// upgrade to the stored spelling, "https://bÜcher.de". Any other reported
// host must be the stored host's "www." sibling, compared with only their
// ASCII letters lowercased.
// Before it answers Adopt, Plan checks that the address it returns dials, by
// HostKey, the stored host's key (a scheme-only change) or the "www." sibling
// of that key (a host change); any other key, or a host that does not
// convert, is Mismatch. So the host that is stored, and pinged to confirm the
// change, is always the host that was compared. To keeps the stored address's
// path as written, a trailing slash included.
func Plan(stored, reported string) PlanResult {
	s, su, okS := Parse(stored)
	r, ru, okR := Parse(reported)
	if !okS || !okR {
		if strings.TrimSpace(stored) == strings.TrimSpace(reported) {
			return PlanResult{Decision: Same}
		}
		return PlanResult{Decision: Mismatch}
	}
	if sameAddress(s, su, r, ru) {
		return PlanResult{Decision: Same}
	}
	if s.Port != r.Port || s.Path != r.Path {
		return PlanResult{Decision: Mismatch}
	}
	if s.Scheme != r.Scheme && !(s.Scheme == "http" && r.Scheme == "https") {
		return PlanResult{Decision: Mismatch}
	}
	want, ok := HostKey(su.Hostname())
	if !ok {
		return PlanResult{Decision: Mismatch}
	}
	// A reported host with the stored host's HostKey is the stored host,
	// however it is spelt: the change is scheme-only, and To keeps the stored
	// spelling. Any other host must be the stored host's "www." sibling.
	host := s.Host
	if !SameHost(su.Hostname(), ru.Hostname()) {
		sibling, ok := WWWSibling(s.Host)
		if !ok || sibling != r.Host {
			return PlanResult{Decision: Mismatch}
		}
		host = sibling
		if want, ok = WWWSibling(want); !ok {
			return PlanResult{Decision: Mismatch}
		}
	}

	to := Join(r.Scheme, host, s.Port, su.EscapedPath())
	if got, ok := addressHostKey(to); !ok || got != want {
		return PlanResult{Decision: Mismatch}
	}
	return PlanResult{Decision: Adopt, To: to}
}

// Join builds an address from its parts. The host is written as
// given (an internationalised host keeps its form rather than being
// percent-encoded), bracketed when it is an IPv6 literal.
func Join(scheme, host, port, escapedPath string) string {
	hostPart := host
	if strings.Contains(host, ":") { // IPv6 literal
		hostPart = "[" + host + "]"
	}
	if port != "" {
		hostPart += ":" + port
	}
	return scheme + "://" + hostPart + escapedPath
}

// Variants lists the spellings the mint-time duplicate check treats as
// the same site as raw: raw itself first, then every combination of http or
// https, with or without the leading "www.", with or without a trailing
// slash, on the same port and path. On a scheme's default port each
// combination is listed with the port omitted and then written out (":80"
// for http, ":443" for https), so "https://example.com" and
// "https://example.com:443" find each other. The order is the lookup's
// priority, so an exact match is reported ahead of a variant.
func Variants(raw string) []string {
	out := []string{raw}
	a, u, ok := Parse(raw)
	if !ok {
		return out
	}
	seen := map[string]bool{raw: true}
	add := func(s string) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	hosts := []string{a.Host}
	if sibling, ok := WWWSibling(a.Host); ok {
		hosts = append(hosts, sibling)
	}
	schemes := []string{a.Scheme, "https"}
	if a.Scheme == "https" {
		schemes[1] = "http"
	}
	path := NormalizePath(u.EscapedPath())
	for _, scheme := range schemes {
		for _, host := range hosts {
			base := Join(scheme, host, a.Port, path)
			add(base)
			add(base + "/")
		}
	}
	if a.Port == "" {
		for _, scheme := range schemes {
			for _, host := range hosts {
				base := Join(scheme, host, defaultPort(scheme), path)
				add(base)
				add(base + "/")
			}
		}
	}
	return out
}

// defaultPort is the port a scheme Parse accepts uses when none is written.
func defaultPort(scheme string) string {
	if scheme == "https" {
		return "443"
	}
	return "80"
}
