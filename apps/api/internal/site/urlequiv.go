package site

import (
	"net/url"
	"strings"

	"github.com/mosamlife/wpmgr/apps/api/internal/siteaddr"
)

// The address rule lives in internal/siteaddr, shared with agentcmd, which
// suggests an address when a command is refused with a redirect. These names
// keep the site package's call sites and tests unchanged.

// siteAddress is siteaddr.Address.
type siteAddress = siteaddr.Address

// enrollURLDecision is siteaddr.Decision.
type enrollURLDecision = siteaddr.Decision

// enrollURLPlan is siteaddr.PlanResult.
type enrollURLPlan = siteaddr.PlanResult

const (
	enrollURLSame     = siteaddr.Same
	enrollURLAdopt    = siteaddr.Adopt
	enrollURLMismatch = siteaddr.Mismatch
)

// parseSiteAddress is siteaddr.Parse.
func parseSiteAddress(raw string) (siteAddress, *url.URL, bool) { return siteaddr.Parse(raw) }

// wwwSibling is siteaddr.WWWSibling.
func wwwSibling(host string) (string, bool) { return siteaddr.WWWSibling(host) }

// planEnrollURL is siteaddr.Plan: the rule enrollment applies. Its hosts are
// compared with only their ASCII letters lowercased, and an address it
// adopts dials the stored host's key or that key's "www." sibling; anything
// else is a mismatch, which keeps the stored address.
func planEnrollURL(stored, reported string) enrollURLPlan { return siteaddr.Plan(stored, reported) }

// sameSiteAddress is siteaddr.SameAddress: one address once normalised, a
// trailing slash included.
func sameSiteAddress(a, b string) bool { return siteaddr.SameAddress(a, b) }

// planReportedURL is siteaddr.PlanStrict: the rule push-time adoption
// applies, with hosts compared in the ASCII form they are dialled by.
func planReportedURL(stored, reported string) enrollURLPlan {
	return siteaddr.PlanStrict(stored, reported)
}

// siteURLVariants is siteaddr.Variants.
func siteURLVariants(raw string) []string { return siteaddr.Variants(raw) }

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
