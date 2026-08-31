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
// deliberately joined at exactly one place: AuthorizeTool, which every
// tools/call goes through.
//
// tools/list DOES NOT consult this file any more. Under the D1 ruling an
// unticked capability refuses rather than hides, so the listing is unfiltered
// and the capability is enforced on the invocation path alone. That is a
// disclosure change and not an authority change -- see the D1 note in
// registry.go.
//
// SiteSet in model.go is the shape this copies. Its zero value allows nothing
// and it has no method answering "does this mean everything", because the
// failure being designed against is an empty set read as an unrestricted one.
// CapabilitySet has the same two properties, for the same reason, on the other
// axis.
// ---------------------------------------------------------------------------

// ErrCodeToolNotAvailable is the refusal for a name that IS NOT IN THE REGISTRY
// AT ALL -- the model guessed. It no longer doubles as the capability refusal:
// that is ErrCodeCapabilityNotGranted below, and the D1 note in registry.go
// records why the two were merged and why they are now separate.
const ErrCodeToolNotAvailable = "mcp_tool_not_available"

// ErrCodeCapabilityNotGranted is the TYPED, TERMINAL refusal for a registered
// tool whose capability this connection's grant does not hold. It is the D1
// ruling's wire vocabulary: an unticked capability refuses under this code, it
// does not vanish from tools/list.
//
// IT IS A THIRD CODE AND NOT ONE OF THE TWO ABOVE, and the distinction is the
// whole reason it exists. ErrCodeCapabilityUnmapped means THE SERVER is
// misconfigured -- a scope it recognises confers nothing -- and telling a
// caller that when the truth is "your grant is narrower than you thought"
// sends an operator hunting a server fault they do not have.
// ErrCodeCapabilityWiderThanDefault is the OPERATOR-facing refusal on the
// narrowing path, raised while a connection is being configured, and it is
// never seen by a model calling a tool. This code is the model-facing one, and
// a client branching on it must read it as permanent: see AuthorizeTool.
const ErrCodeCapabilityNotGranted = "mcp_capability_not_granted"

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

// The v1 READ vocabulary, one constant per outcome group. m131 seated these
// eight in mcp_grants_capabilities_vocabulary_check and its DECISION 1 is the
// argument for each name and each boundary; it is not restated here, because a
// second copy of that argument is a second thing that can drift.
//
// Three properties of the set that this file DOES have to hold:
//
//   - The `mcp.` prefix is FROZEN by CapSitesRead, not chosen. The wireframes
//     render the operator-facing form as `site.*`; those are the SCREEN's
//     labels and the picker may render whatever label it likes above these
//     strings.
//   - EVERY MEMBER ENDS IN `.read`, which is what makes "no write capability is
//     reachable from this build" checkable by pattern rather than by claim.
//     TestEveryCapabilityIsARead pins it.
//   - The set is CLOSED, for the same reason recognisedScopes is closed: an
//     unrecognised capability must be a refusal, never a token quietly dropped
//     from a set that is then honoured.
const (
	// CapSitesRead is the fleet inventory, and the ONLY member any grant minted
	// before this commit holds. It is also the only member the tool registry
	// currently requires (registry.go).
	CapSitesRead Capability = "mcp.sites.read"

	CapUptimeRead      Capability = "mcp.uptime.read"
	CapBackupsRead     Capability = "mcp.backups.read"
	CapSecurityRead    Capability = "mcp.security.read"
	CapActivityRead    Capability = "mcp.activity.read"
	CapPerformanceRead Capability = "mcp.performance.read"
	CapDiagnosticsRead Capability = "mcp.diagnostics.read"

	// CapContentRead is SEATED AND DELIBERATELY UNREACHABLE. It is in the
	// vocabulary because the database's CHECK holds it and the two sets must
	// agree exactly (m131 DECISION 5), and it is in NO scope's capability list
	// so no grant can be minted holding it. See scopeCapabilities.
	CapContentRead Capability = "mcp.content.read"
)

// capabilityVocabulary is the CLOSED set of capabilities this surface knows.
// A Capability outside it is not a weaker grant, it is an unknown one, and an
// unknown grant is refused.
//
// IT IS THE GO HALF OF A SET THAT IS CLOSED IN TWO PLACES. The other half is
// mcp_grants_capabilities_vocabulary_check, and the two must hold the same
// eight names: a name the database accepts and this map does not is refused at
// a different layer with a different error, and a name this map accepts and the
// database does not is a 23514 at INSERT on a path an operator reached through
// a wizard. TestCapabilityVocabularyMatchesTheDatabaseCheckAsAppRole (in
// apps/api/tests) reads the live constraint and asserts both directions.
//
// KNOWING a capability is not CONFERRING it and is not DEFAULTING to it. Those
// are three separate sets and this is the widest of them:
//
//	capabilityVocabulary   -- what may be spelled at all           (8)
//	scopeCapabilities      -- what a scope confers, the CEILING    (7)
//	DefaultGrantCapabilities -- what an unasked grant receives     (1)
//
// Widening this map alone widens NOTHING a grant receives, and that separation
// is the whole point of this commit. See DefaultGrantCapabilities.
var capabilityVocabulary = map[Capability]struct{}{
	CapActivityRead:    {},
	CapBackupsRead:     {},
	CapContentRead:     {},
	CapDiagnosticsRead: {},
	CapPerformanceRead: {},
	CapSecurityRead:    {},
	CapSitesRead:       {},
	CapUptimeRead:      {},
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
//
// IT IS THE CEILING, AND IT IS WRITTEN OUT RATHER THAN DERIVED. `ScopeRead:
// AllCapabilities()` would read as the obvious spelling and would rebuild, one
// function over, exactly the coupling this commit exists to break: the first
// `.propose` or `.write` capability seated in the vocabulary would become
// conferred by the READ scope, silently, as a consequence of a map edit. The
// rule this literal encodes is instead stated and checkable -- the read scope
// confers the READ capabilities, and a member that does not end in `.read` is
// not conferred by it until someone writes that line and defends it.
//
// CapContentRead IS ABSENT, AND THE ABSENCE IS THE DECISION. m131 DECISION 3
// seats it in the database while nothing can grant it: no post or page table
// exists, no agent command returns post content, and ADR-062 holds the content
// work behind ship blockers including a plugin privacy disclosure. Go's
// equivalent of the database's "nothing writes it" is "no scope confers it",
// and this is that. Listing it here instead would let an operator mint a
// connection holding a capability that reaches nothing today and then reaches
// real post content the day those tools land -- a widening with no second
// consent, arriving as a side effect of a registry edit. Adding it to this list
// is a one-line change at the moment the content tools ship, in that diff,
// under that review. TestContentReadIsKnownButConferredByNoScope pins it.
var scopeCapabilities = map[Scope][]Capability{
	ScopeRead: {
		CapActivityRead,
		CapBackupsRead,
		CapDiagnosticsRead,
		CapPerformanceRead,
		CapSecurityRead,
		CapSitesRead,
		CapUptimeRead,
	},
}

// grantScopes returns the OAuth scopes a live grant holds.
//
// IT IS A CONSTANT, AND THAT IS THE WHOLE REASON THIS FUNCTION EXISTS RATHER
// THAN A LITERAL AT THE CALL SITE. mcp_grants has no scopes column (m124
// DECISION 1 declines to mint one), so there is nothing per-grant to read; every
// grant that can authenticate holds ScopeRead, because ParseRequestedScopes
// refuses anything recognisedScopes does not carry and recognisedScopes carries
// exactly one entry.
//
// That reasoning is load-bearing and it EXPIRES the moment a second scope is
// recognised: a connection granted only the new scope would still be handed
// ScopeRead's capabilities, which is a widening rather than a narrowing and is
// therefore the direction that matters. Naming it here gives
// TestGrantScopesIsExactOnlyWhileOneScopeExists something to point at, and gives
// whoever adds that scope one obvious place to replace with a real per-grant
// read.
func grantScopes() []Scope {
	return []Scope{ScopeRead}
}

// DefaultGrantCapabilities is the capability set stamped onto mcp_grants.
// capabilities when a NEW grant is created and NOBODY ASKED FOR A PARTICULAR
// ONE -- the OAuth consent screen, which offers no narrowing control, and the
// mint endpoint called with an empty Capabilities list.
//
// IT USED TO RETURN AllCapabilities() AND THAT IS THE TRAP m131 LEFT A WARNING
// ABOUT. The identity was true and safe while the vocabulary held exactly one
// member. The moment the map above widened to eight, that one line would have
// stamped all eight onto every newly minted grant -- including groups whose
// tools do not exist and a content group that is behind ADR-062's ship blockers
// -- as a silent consequence of a map edit that nothing would have failed on.
// A credential nobody chose the terms of is the precise failure m127 DECISION 1
// and m124 DECISION 1 built the NOT NULL column and the closed CHECK to
// prevent, and it would have arrived through the one door neither of them
// watches.
//
// SO THE DEFAULT IS NOW AN EXPLICIT PRESET AND IT IS DELIBERATELY THE
// NARROWEST ONE: the fleet inventory read, and nothing else.
//
// The argument for that value, rather than for the seven-member ceiling:
//
//  1. IT IS THE ONLY CAPABILITY THAT REACHES A TOOL. registry.go registers one
//     tool and it requires CapSitesRead. Every other member of the ceiling
//     names a group whose tools are not built, so defaulting to them would
//     stamp authority that resolves to nothing -- and would then silently
//     become real authority as each group's tools land, on grants whose
//     operators consented before those tools existed.
//  2. IT IS WHAT THE CONSENT SCREEN SAYS. Phase 1's screen describes reading
//     the fleet inventory; a default wider than the screen's own words is a
//     grant the operator did not read the terms of, whatever the terms were.
//  3. IT MOVES NO EXISTING GRANT AND NO FUTURE ONE. Every grant this surface
//     has ever minted holds exactly {mcp.sites.read}, and every grant minted
//     after this commit without an explicit request holds exactly the same
//     thing. This commit raises two ceilings and moves no floor, which is the
//     same stance m131 DECISION 4 takes on the database side by refusing a
//     backfill.
//
// THE CEILING IS STILL THE SEVEN. An operator who asks for a wider set gets it
// (Service.MintConnection -> resolveMintCapabilities -> NarrowTo), because
// asking is choosing. The default is what happens when nobody asked, and the
// answer to "nobody asked" is never "everything".
//
// It is a FUNCTION and not a package-level slice because a package variable
// would be shared, mutable, and one append away from widening every grant this
// surface has ever minted. The literal is rebuilt on each call for the same
// reason.
//
// This is where a chosen value replaces a defaulted one when Step 5's
// capability control ships: the operator's selection arrives on
// ApprovalRequest and is passed instead. It is deliberately NOT a database
// DEFAULT -- see the column's NOT NULL and m127's reasoning about a credential
// nobody chose the terms of.
func DefaultGrantCapabilities() []Capability {
	return []Capability{CapSitesRead}
}

// capabilityNames renders capabilities for the text[] column.
func capabilityNames(caps []Capability) []string {
	out := make([]string, 0, len(caps))
	for _, c := range caps {
		out = append(out, string(c))
	}
	return out
}

// capabilitiesFromColumn reads mcp_grants.capabilities back into the domain
// type.
//
// IT DOES NOT FILTER, and that omission is deliberate. Dropping an unknown name
// here would hand NarrowTo a set that had already been quietly reduced, so a
// row carrying a capability outside this build's vocabulary would authenticate
// as though it carried only the known ones. The refusal belongs at NarrowTo,
// where an unheld capability rejects the whole set instead of being trimmed out
// of it -- the same reasoning ParseRequestedScopes gives for refusing an
// unrecognised scope rather than ignoring it.
//
// A nil or empty column yields an empty slice, which the caller must treat as
// "no capability" and never as "unrestricted".
func capabilitiesFromColumn(names []string) []Capability {
	out := make([]Capability, 0, len(names))
	for _, n := range names {
		out = append(out, Capability(n))
	}
	return out
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
// IT IS THE CEILING, NOT THE ANSWER. m127 added mcp_grants.capabilities -- NOT
// NULL, no default, closed CHECK, exactly as m124 DECISION 1 required of any
// such column -- so a live connection's capability set is now READ FROM THAT
// ROW and then narrowed against this ceiling (Service.Authenticate). This
// function is no longer what a request path may use on its own: doing so
// answers from the scope registry rather than from the grant, which agrees with
// the row only while the vocabulary holds one name.
//
// It stays derived rather than stored because a ceiling that could be raised by
// writing a row is not a ceiling. scopeCapabilities is the only thing that can
// widen it, and widening that map is a code change under review.
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
