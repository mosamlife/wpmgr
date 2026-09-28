package site

import (
	"net"
	"net/url"
	"strings"
)

// siteAddress is a site URL reduced to the parts that decide whether two
// spellings name the same WordPress install: the scheme, the lowercased host,
// the port only when it is not the scheme's default, and the path with its
// trailing slashes removed.
type siteAddress struct {
	Scheme string
	Host   string
	Port   string
	Path   string
}

// parseSiteAddress normalises raw into a siteAddress. It refuses anything that
// is not a plain http(s) site address: a parse failure, another scheme, an
// empty host, userinfo, a query or a fragment. The parsed URL is returned too,
// so a caller can rebuild an address from the original spelling.
func parseSiteAddress(raw string) (siteAddress, *url.URL, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u == nil || u.Opaque != "" {
		return siteAddress{}, nil, false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return siteAddress{}, nil, false
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" {
		return siteAddress{}, nil, false
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return siteAddress{}, nil, false
	}
	port := u.Port()
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		port = ""
	}
	return siteAddress{
		Scheme: scheme,
		Host:   host,
		Port:   port,
		Path:   strings.TrimRight(u.Path, "/"),
	}, u, true
}

// wwwSibling returns host with a leading "www." added or removed, and false
// when the host has no such sibling. Only the apex and its exact "www." label
// pair up: an IP literal, a single-label host, and a host whose apex would
// itself start with "www." have none.
func wwwSibling(host string) (string, bool) {
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

// enrollURLDecision is what enrollment does with the address the agent
// reports for a site that already has a stored address.
type enrollURLDecision int

const (
	// enrollURLSame: the two addresses are equal once normalised. Nothing
	// changes.
	enrollURLSame enrollURLDecision = iota
	// enrollURLAdopt: the reported address differs from the stored one only by
	// a leading "www." and/or an http to https upgrade, on the same port and
	// path. The stored address is replaced by enrollURLPlan.To.
	enrollURLAdopt
	// enrollURLMismatch: any other difference. The stored address is kept and
	// the difference is flagged.
	enrollURLMismatch
)

// enrollURLPlan is the outcome of planEnrollURL.
type enrollURLPlan struct {
	Decision enrollURLDecision
	// To is the address to store when Decision is enrollURLAdopt. It is built
	// from the stored address, never copied from the reported one: only the
	// scheme and the "www." label can differ from what was stored.
	To string
}

// planEnrollURL decides whether enrollment may replace a site's stored address
// with the address its agent reports. Adoption is limited to a leading "www."
// toggle and/or an http to https upgrade, on the same port and path. A
// downgrade from https to http, another host or subdomain, another port and
// another path are never adopted.
func planEnrollURL(stored, reported string) enrollURLPlan {
	s, su, okS := parseSiteAddress(stored)
	r, _, okR := parseSiteAddress(reported)
	if !okS || !okR {
		if strings.TrimSpace(stored) == strings.TrimSpace(reported) {
			return enrollURLPlan{Decision: enrollURLSame}
		}
		return enrollURLPlan{Decision: enrollURLMismatch}
	}
	if s == r {
		return enrollURLPlan{Decision: enrollURLSame}
	}
	if s.Port != r.Port || s.Path != r.Path {
		return enrollURLPlan{Decision: enrollURLMismatch}
	}
	if s.Scheme != r.Scheme && !(s.Scheme == "http" && r.Scheme == "https") {
		return enrollURLPlan{Decision: enrollURLMismatch}
	}
	host := s.Host
	if r.Host != s.Host {
		sibling, ok := wwwSibling(s.Host)
		if !ok || sibling != r.Host {
			return enrollURLPlan{Decision: enrollURLMismatch}
		}
		host = sibling
	}

	hostPart := host
	if strings.Contains(host, ":") { // IPv6 literal
		hostPart = "[" + host + "]"
	}
	if s.Port != "" {
		hostPart += ":" + s.Port
	}
	to := url.URL{
		Scheme:  r.Scheme,
		Host:    hostPart,
		Path:    su.Path,
		RawPath: su.RawPath,
	}
	return enrollURLPlan{Decision: enrollURLAdopt, To: to.String()}
}

// siteURLVariants lists the spellings the mint-time duplicate check treats as
// the same site as raw: raw itself first, then every combination of http or
// https, with or without the leading "www.", with or without a trailing
// slash, on the same port and path. The order is the lookup's priority, so an
// exact match is reported ahead of a variant.
func siteURLVariants(raw string) []string {
	out := []string{raw}
	a, u, ok := parseSiteAddress(raw)
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
	if sibling, ok := wwwSibling(a.Host); ok {
		hosts = append(hosts, sibling)
	}
	schemes := []string{a.Scheme, "https"}
	if a.Scheme == "https" {
		schemes[1] = "http"
	}
	path := strings.TrimRight(u.EscapedPath(), "/")
	for _, scheme := range schemes {
		for _, host := range hosts {
			hostPart := host
			if strings.Contains(host, ":") {
				hostPart = "[" + host + "]"
			}
			if a.Port != "" {
				hostPart += ":" + a.Port
			}
			base := scheme + "://" + hostPart + path
			add(base)
			add(base + "/")
		}
	}
	return out
}

// sanitizeReportedURL is the form of an agent-reported address that may be
// written to an audit row: userinfo, query and fragment removed, and the
// length capped. An address that does not parse is recorded as empty.
func sanitizeReportedURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u == nil {
		return ""
	}
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	u.RawFragment = ""
	out := u.String()
	const maxLen = 2048
	if len(out) > maxLen {
		out = out[:maxLen]
	}
	return out
}
