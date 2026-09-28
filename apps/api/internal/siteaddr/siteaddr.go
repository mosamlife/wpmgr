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

// HostKey is host in the ASCII form a resolver is asked for: an IP literal
// lowercased as written, and any other host through IDNA's lookup profile
// (UTS #46 mapping, then Punycode), which is the conversion net/http applies
// before it dials a non-ASCII host. ok is false when the host is empty or
// does not convert (a label IDNA refuses, such as one holding "_"), and a
// caller treats that as "not the same host".
func HostKey(host string) (string, bool) {
	if host == "" {
		return "", false
	}
	if ip := net.ParseIP(host); ip != nil {
		return asciiLower(host)
	}
	ascii, err := idna.Lookup.ToASCII(host)
	if err != nil || ascii == "" {
		return "", false
	}
	return asciiLower(ascii)
}

// asciiLower lowercases A to Z only, and refuses a string holding any byte
// outside ASCII.
func asciiLower(s string) (string, bool) {
	b := []byte(s)
	for i, c := range b {
		switch {
		case c >= 0x80:
			return "", false
		case c >= 'A' && c <= 'Z':
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b), true
}

// SameHost reports whether a and b name the same host once each is reduced
// to its HostKey. Unicode case folding is never used: two spellings it
// treats as equal can convert to different registrable domains (a capital
// sharp s maps to "ss", a small one to its own Punycode label), and a
// request is dialled by the converted form. A host that does not convert is
// the same as nothing.
func SameHost(a, b string) bool {
	ka, okA := HostKey(a)
	kb, okB := HostKey(b)
	return okA && okB && ka == kb
}

// PlanStrict is Plan with each host compared by HostKey: it answers Same or
// Adopt only when Plan does and the reported host, as it would be dialled, is
// the stored host (Same, or an https upgrade) or the stored host's "www."
// sibling (a host change), both taken through HostKey. Anything else, a host
// that does not convert included, is Mismatch. The address it returns to
// store is Plan's, built from the stored spelling.
//
// Push-time adoption and a refused redirect's suggestion use it. Enrollment
// keeps Plan.
func PlanStrict(stored, reported string) PlanResult {
	p := Plan(stored, reported)
	if p.Decision == Mismatch {
		return p
	}
	s, su, okS := Parse(stored)
	r, ru, okR := Parse(reported)
	if !okS || !okR {
		// Plan's own fallback: the two strings are identical.
		return p
	}
	ks, okKS := HostKey(su.Hostname())
	kr, okKR := HostKey(ru.Hostname())
	if !okKS || !okKR {
		return PlanResult{Decision: Mismatch}
	}
	want := ks
	if s.Host != r.Host {
		sibling, ok := WWWSibling(ks)
		if !ok {
			return PlanResult{Decision: Mismatch}
		}
		want = sibling
	}
	if kr != want {
		return PlanResult{Decision: Mismatch}
	}
	return p
}

// Address is a site URL reduced to the parts that decide whether two
// spellings name the same WordPress install: the scheme, the lowercased host,
// the port only when it is not the scheme's default, and the path with its
// trailing slashes removed.
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
	host := strings.ToLower(u.Hostname())
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
		Path:   strings.TrimRight(u.Path, "/"),
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
	// Same: the two addresses are equal once normalised. Nothing
	// changes.
	Same Decision = iota
	// Adopt: the reported address differs from the stored one only by
	// a leading "www." and/or an http to https upgrade, on the same port and
	// path. The stored address is replaced by PlanResult.To.
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
// another path are never adopted.
func Plan(stored, reported string) PlanResult {
	s, su, okS := Parse(stored)
	r, _, okR := Parse(reported)
	if !okS || !okR {
		if strings.TrimSpace(stored) == strings.TrimSpace(reported) {
			return PlanResult{Decision: Same}
		}
		return PlanResult{Decision: Mismatch}
	}
	if s == r {
		return PlanResult{Decision: Same}
	}
	if s.Port != r.Port || s.Path != r.Path {
		return PlanResult{Decision: Mismatch}
	}
	if s.Scheme != r.Scheme && !(s.Scheme == "http" && r.Scheme == "https") {
		return PlanResult{Decision: Mismatch}
	}
	host := s.Host
	if r.Host != s.Host {
		sibling, ok := WWWSibling(s.Host)
		if !ok || sibling != r.Host {
			return PlanResult{Decision: Mismatch}
		}
		host = sibling
	}

	return PlanResult{Decision: Adopt, To: Join(r.Scheme, host, s.Port, su.EscapedPath())}
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
// slash, on the same port and path. The order is the lookup's priority, so an
// exact match is reported ahead of a variant.
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
	path := strings.TrimRight(u.EscapedPath(), "/")
	for _, scheme := range schemes {
		for _, host := range hosts {
			base := Join(scheme, host, a.Port, path)
			add(base)
			add(base + "/")
		}
	}
	return out
}
