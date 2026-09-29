package mcp

import (
	"context"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// MinAgentVersionForOriginOnlyPurge is the first agent release that clears
// only this site's own page cache when asked for an origin-only clear, and
// reports which hosting caches it cleared and which it skipped. An older agent
// ignores the option, so the creation path, approval and dispatch all refuse a
// site below it (-32011 at creation). An empty or unparseable version counts
// as below it.
//
// It is a historical fact about the agent, pinned by
// TestMinAgentVersionForOriginOnlyPurge_NotAheadOfShippingAgent: it must name
// the release that first ships the origin-only clear, and it must never be
// ahead of the version apps/agent actually ships.
const MinAgentVersionForOriginOnlyPurge = "0.61.153"

// governedCachePurgeAliases is the CLOSED set of names an operator's AI rule
// may use to forbid the cache-clear request tool. A forbidden-tools entry is
// normalised (lower-case, trimmed, every run of whitespace, ".", "_" and "-"
// collapsed to "_") and compared against this set. Anything else does not
// match: a rule is a list of names, not a pattern language.
var governedCachePurgeAliases = map[string]struct{}{
	"site_cache_purge":         {},
	"site_cache_purge_request": {},
	"cache_purge":              {},
	"mcp_cache_purge":          {},
	"site_cache_purge_all":     {},
}

var governedNameSeparators = regexp.MustCompile(`[\s._-]+`)

// normaliseGovernedToolName is the matching rule for forbidden-tools entries.
func normaliseGovernedToolName(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	s = governedNameSeparators.ReplaceAllString(s, "_")
	return strings.Trim(s, "_")
}

// GovernedToolAliases returns the normalised names that, listed as forbidden
// in an operator's AI rules, forbid the given tool. It is empty for a tool no
// rule can name.
func GovernedToolAliases(tool string) []string {
	if tool != ToolSiteCachePurgeRequest {
		return nil
	}
	out := make([]string, 0, len(governedCachePurgeAliases))
	for a := range governedCachePurgeAliases {
		out = append(out, a)
	}
	return out
}

// forbiddenEntry returns the first entry of forbidden that names tool, as the
// operator wrote it.
func forbiddenEntry(forbidden []string, tool string) (string, bool) {
	aliases := GovernedToolAliases(tool)
	if len(aliases) == 0 {
		return "", false
	}
	set := make(map[string]struct{}, len(aliases))
	for _, a := range aliases {
		set[a] = struct{}{}
	}
	for _, e := range forbidden {
		if _, ok := set[normaliseGovernedToolName(e)]; ok {
			return e, true
		}
	}
	return "", false
}

// ForbiddenByContext reports whether the operator's governed context for this
// site forbids tool, and which entry did. It resolves at SITE scope, so an
// organisation rule and a site rule both apply, and it checks the full,
// untruncated union of every layer's forbidden tools rather than any rendered
// text: a rule the rendering would have cut still forbids.
//
// The resolver picks its transaction from the principal in ctx: the
// approver's own on approve, a tenant transaction in a worker with none.
//
// An error means the context could not be resolved. Callers answer it as
// context_unavailable and never as "not forbidden".
func (s *Service) ForbiddenByContext(ctx context.Context, tenantID, siteID uuid.UUID, tool string) (matchedEntry string, forbidden bool, err error) {
	if s.context == nil {
		return "", false, domain.Internal(ErrCodeContextUnavailable,
			"this site's governed context cannot be resolved")
	}
	rc, err := s.context.Resolve(ctx, tenantID, siteID, nil)
	if err != nil {
		return "", false, err
	}
	entry, ok := forbiddenEntry(rc.Restrictions.ForbiddenTools, tool)
	return entry, ok, nil
}
