package mcp

import (
	"fmt"
	"sort"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// ---------------------------------------------------------------------------
// S7: the capability half of the tool policy.
//
// This file answers "WHICH TOOLS may this connection reach". registry.go
// answers "WHICH TOOLS EXIST, and what does each require". The two are
// deliberately separate types so that neither can be read as the other, and
// deliberately joined at exactly one place (AuthorizeTool) that both tools/list
// and tools/call go through.
//
// SiteSet in model.go is the shape this copies. Its zero value allows nothing
// and it has no method answering "does this mean everything", because the
// failure being designed against is an empty set read as an unrestricted one.
// CapabilitySet has the same two properties, for the same reason, on the other
// axis.
// ---------------------------------------------------------------------------

// ErrCodeToolNotAvailable is the SINGLE domain code for every reason a tool is
// not reachable by this connection. See the disclosure note on AuthorizeTool
// for why there is one code and not three.
const ErrCodeToolNotAvailable = "mcp_tool_not_available"

// ErrCodeCapabilityUnmapped is the fail-closed refusal for a recognised OAuth
// scope that no capability mapping covers. It is an internal misconfiguration,
// not a caller error.
const ErrCodeCapabilityUnmapped = "mcp_capability_unmapped"

// ErrCodeCapabilityWiderThanDefault is the refusal for a per-connection
// narrowing request that names a capability the org's default does not hold.
const ErrCodeCapabilityWiderThanDefault = "mcp_capability_wider_than_default"

// Capability is one discrete thing an MCP connection may do. It is NOT an
// authz.Permission: an authz.Permission is checked against a user principal and
// an MCP request has no user principal, only a grant. Keeping the two types
// distinct stops a tool's declared operator permission from being mistaken for
// something this path enforces. See ToolPolicy.OperatorPermission.
type Capability string

// CapSitesRead is the only capability the Phase 1 surface has: read the fleet
// inventory. The vocabulary is closed (capabilityVocabulary below) for the same
// reason recognisedScopes is closed -- an unrecognised capability must be a
// refusal, never a token quietly dropped from a set that is then honoured.
const CapSitesRead Capability = "mcp.sites.read"

// capabilityVocabulary is the CLOSED set of capabilities this surface knows.
// A Capability outside it is not a weaker grant, it is an unknown one, and an
// unknown grant is refused.
var capabilityVocabulary = map[Capability]struct{}{
	CapSitesRead: {},
}

// KnownCapability reports whether c is in the vocabulary.
func KnownCapability(c Capability) bool {
	_, ok := capabilityVocabulary[c]
	return ok
}

// AllCapabilities lists the vocabulary, sorted, so tests and operator surfaces
// enumerate it deterministically.
func AllCapabilities() []Capability {
	out := make([]Capability, 0, len(capabilityVocabulary))
	for c := range capabilityVocabulary {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// scopeCapabilities maps each RECOGNISED OAuth scope to the capabilities it
// confers. It is the org-default source, and it is a total function over
// recognisedScopes by test (TestEveryRecognisedScopeHasACapabilityMapping).
//
// THE TOTALITY IS THE POINT, and it is why CapabilitiesForScopes returns an
// error rather than skipping. If a write scope is ever added to
// recognisedScopes and this map is not updated, the tempting behaviour is to
// yield a smaller set -- which is fail-closed and therefore feels safe. It is
// still wrong: the operator consented to a scope, the surface silently conferred
// nothing for it, and neither party is told. That is the same shape as a scope
// token silently dropped from an otherwise-honoured request, which scope.go
// already refuses. An unmapped scope is a misconfiguration and it says so.
var scopeCapabilities = map[Scope][]Capability{
	ScopeRead: {CapSitesRead},
}

// CapabilitySet is a RESOLVED set of capabilities.
//
// It is a struct and not a []Capability on purpose, and the reasoning is
// SiteSet's verbatim: a bare slice has a zero value that reads as "no filter
// applied" at every call site that forgets to check it. This type's zero value
// allows NOTHING, and there is deliberately NO method answering "does this mean
// everything". Empty means no capabilities, which means no tools.
type CapabilitySet struct {
	caps map[Capability]struct{}
}

// NewCapabilitySet builds a set from capabilities that are already known to be
// in the vocabulary. Unknown entries are DROPPED here rather than admitted,
// because admitting one would put a name outside the vocabulary into a set that
// AuthorizeTool then compares against -- and the only way a comparison against
// an unknown name can matter is if something later decided to trust it.
//
// Dropping is safe HERE and is not the silent-coercion failure, because
// dropping only ever removes authority. The place where an unknown name must
// REFUSE rather than drop is the caller-facing narrowing request, and
// NarrowTo does exactly that.
func NewCapabilitySet(caps []Capability) CapabilitySet {
	set := CapabilitySet{caps: make(map[Capability]struct{}, len(caps))}
	for _, c := range caps {
		if !KnownCapability(c) {
			continue
		}
		set.caps[c] = struct{}{}
	}
	return set
}

// Allows reports whether this connection holds c. On an empty or zero-value set
// it returns false for every capability, which is the entire point.
func (s CapabilitySet) Allows(c Capability) bool {
	_, ok := s.caps[c]
	return ok
}

// Len is the number of capabilities held.
func (s CapabilitySet) Len() int { return len(s.caps) }

// IsEmpty reports that this connection holds no capability at all. The caller
// must handle it as "reaches nothing", and must never widen it.
func (s CapabilitySet) IsEmpty() bool { return len(s.caps) == 0 }

// Sorted lists the held capabilities for logs and operator surfaces.
func (s CapabilitySet) Sorted() []Capability {
	out := make([]Capability, 0, len(s.caps))
	for c := range s.caps {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// OrgDefaultCapabilities is the WIDEST capability set any connection in this
// organisation may hold, derived from the scopes the surface granted it.
//
// There is no capabilities column on mcp_grants and that is m124 DECISION 1:
// minting one would create the place a write capability could appear without a
// migration and without a review. So the org default is DERIVED from the closed
// scope registry rather than stored, and it cannot exceed what
// scopeCapabilities maps. When a differentiated-read column does arrive it
// arrives NOT NULL with no default and a closed CHECK, and it narrows this --
// it never replaces it as the ceiling.
//
// An empty or nil scope list yields an EMPTY set and an error, never a
// permissive one. Absence is refusal here exactly as it is in
// ParseRequestedScopes.
func OrgDefaultCapabilities(scopes []Scope) (CapabilitySet, error) {
	if len(scopes) == 0 {
		return CapabilitySet{}, domain.Forbidden(ErrCodeCapabilityUnmapped,
			"this connection holds no scope, so it confers no capability")
	}

	out := make([]Capability, 0, len(scopes))
	for _, s := range scopes {
		caps, ok := scopeCapabilities[s]
		if !ok {
			// Refuse, do not skip. See the comment on scopeCapabilities.
			return CapabilitySet{}, domain.Forbidden(ErrCodeCapabilityUnmapped,
				fmt.Sprintf("scope %q confers no capability on this server", s))
		}
		out = append(out, caps...)
	}
	set := NewCapabilitySet(out)
	if set.IsEmpty() {
		// Cannot happen while every mapping is non-empty, and it is checked
		// rather than assumed because "cannot happen" returning a silently
		// empty grant is this project's signature defect. An empty set here
		// would be a working connection that reaches no tool and is told
		// nothing about why.
		return CapabilitySet{}, domain.Forbidden(ErrCodeCapabilityUnmapped,
			"the scopes on this connection resolved to no capability")
	}
	return set, nil
}

// NarrowTo applies PER-CONNECTION NARROWING to an org default.
//
// The rule is one-directional: a connection can only ever be NARROWER than its
// org's default, never wider. So this is an intersection, and a requested
// capability the default does not hold is a REFUSAL of the whole narrowing --
// not a token quietly dropped from an otherwise-honoured request.
//
// DROPPING WOULD BE THE BUG. It "still works" for a well-behaved caller and it
// is even fail-closed, because the result is a subset either way. It is still
// wrong: the operator asked for a connection covering {A, B}, the surface built
// one covering {A}, and neither party ever learns they disagreed. That is the
// same reasoning ParseRequestedScopes gives for refusing an unrecognised scope
// rather than ignoring it, one layer down.
//
// An EMPTY requested list is refused. "Narrow me to nothing" is a connection
// that can do nothing, which is almost certainly a caller that meant to say
// something and said nothing -- and reading it as "narrow me to nothing" would
// mint a live credential that silently reaches no tool. It is not read as "keep
// the default" either; that would be an absence widening a grant.
func (s CapabilitySet) NarrowTo(requested []Capability) (CapabilitySet, error) {
	if len(requested) == 0 {
		return CapabilitySet{}, domain.Validation(ErrCodeCapabilityWiderThanDefault,
			"a capability narrowing must name at least one capability; "+
				"an empty list is not a way to request the organisation default")
	}

	kept := make([]Capability, 0, len(requested))
	for _, c := range requested {
		if !s.Allows(c) {
			// Naming the offending capability back is safe: it is the caller's
			// own input, and this path is the OPERATOR configuring a connection
			// in the dashboard, not the model calling a tool. The
			// non-disclosure rule on AuthorizeTool governs the model-facing
			// path and does not apply here.
			return CapabilitySet{}, domain.Validation(ErrCodeCapabilityWiderThanDefault,
				fmt.Sprintf("capability %q is not held by this organisation's default, "+
					"so a connection cannot be granted it; a connection may only ever be "+
					"narrower than the organisation default", c))
		}
		kept = append(kept, c)
	}

	narrowed := NewCapabilitySet(kept)
	if narrowed.IsEmpty() {
		// Every requested capability passed s.Allows, so each is in the
		// vocabulary and NewCapabilitySet dropped none. Reaching here means the
		// loop accepted every entry and kept none, which cannot happen --
		// checked rather than assumed, for the reason above.
		return CapabilitySet{}, domain.Validation(ErrCodeCapabilityWiderThanDefault,
			"the requested narrowing resolved to no capability")
	}
	return narrowed, nil
}
