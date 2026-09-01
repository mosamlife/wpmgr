// Package mcp is the read-only MCP connection surface (ADR-064 S6b).
//
// Storage is m124 (20260826000000_m124_mcp_connection_surface.sql), which is
// SCHEMA ONLY: four tables, their RLS and their grants, and no behaviour. That
// file's header carries nine numbered DECISIONs explaining the shape of every
// column, and a closing block titled "WHAT S6b MUST DO" listing seven
// obligations this package owes. Read it before changing anything here; the
// constraints below are deliberately redundant with CHECKs in that file and
// the redundancy is the point.
//
// This file holds the domain vocabulary. The OAuth endpoints, the PKCE
// exchange and the site-scope resolution chokepoint live alongside it.
package mcp

import (
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Scope is an OAuth scope this surface recognises. The registry in scope.go is
// CLOSED: an unrecognised scope is refused, never ignored.
type Scope string

// ScopeRead is the only scope this surface grants, and the surface is
// read-only by construction rather than by configuration.
//
// m127 DID MINT mcp_grants.capabilities, so the older statement here -- that
// there is deliberately no capability column -- is no longer true and has been
// removed rather than left to be believed. What it was protecting survives, and
// survives in a stronger form: the column is NOT NULL with no default and
// carries a CHECK closed over the same one-name vocabulary this package holds
// (capabilityVocabulary), so a write capability STILL cannot appear in a row
// without a migration and without a review. The column narrows what a
// connection may do; it cannot widen it past what scopeCapabilities maps.
//
// A write scope does not belong here; it belongs in its own migration with its
// own review.
const ScopeRead Scope = "mcp:read"

// SiteScopeMode says which sites a grant may read. It mirrors
// mcp_grants_site_scope_mode_check: NOT NULL, closed set, and deliberately NO
// DEFAULT, because the whole failure this slice differentiates against is a
// scope column whose unset value means everything.
type SiteScopeMode string

const (
	// SiteScopeModeAll covers every site in the tenant, and carries no payload.
	SiteScopeModeAll SiteScopeMode = "all"
	// SiteScopeModeTags resolves through site_tags and must name >=1 tag.
	SiteScopeModeTags SiteScopeMode = "tags"
	// SiteScopeModeList is a literal site allowlist and must name >=1 site.
	SiteScopeModeList SiteScopeMode = "list"
)

// GrantStatus mirrors mcp_grants_status_check. Liveness is a stored column and
// not an inference from an expiry (m124 DECISION 2), so that revoking is a
// write somebody made rather than a clock somebody trusted.
type GrantStatus string

const (
	GrantStatusActive  GrantStatus = "active"
	GrantStatusRevoked GrantStatus = "revoked"
)

// SiteScopeRequest is what a caller asked for, before validation. It is
// deliberately NOT the stored shape: Mode has no zero-value meaning, and
// ValidateSiteScopeRequest refuses an empty Mode rather than reading it as
// SiteScopeModeAll.
type SiteScopeRequest struct {
	Mode    SiteScopeMode
	TagIDs  []uuid.UUID
	SiteIDs []uuid.UUID
}

// SiteSet is a RESOLVED set of site ids: the output of the single audited
// chokepoint that turns a grant's stored scope into the sites it may actually
// read, inside InTenantTx so that `sites` RLS drops every foreign UUID
// (m124 obligation 2 -- scope_site_ids is a uuid[] and PostgreSQL has no
// foreign key over array elements, so the column accepts any UUID including
// another organisation's site).
//
// It is a struct and not a []uuid.UUID on purpose. A bare slice has a zero
// value that reads as "no filter applied" at every call site that forgets to
// check it; this type's zero value allows NOTHING, and there is deliberately
// no method that answers "does this mean everything". Empty means no sites.
type SiteSet struct {
	ids map[uuid.UUID]struct{}
}

// NewSiteSet builds a SiteSet from ids that have ALREADY been resolved through
// `sites` under tenant RLS. Passing an unresolved or cached list defeats the
// only check that catches a foreign UUID.
func NewSiteSet(ids []uuid.UUID) SiteSet {
	set := SiteSet{ids: make(map[uuid.UUID]struct{}, len(ids))}
	for _, id := range ids {
		set.ids[id] = struct{}{}
	}
	return set
}

// Allows reports whether this grant may read the given site. On an empty or
// zero-value set it returns false for every id, which is the entire point.
func (s SiteSet) Allows(id uuid.UUID) bool {
	_, ok := s.ids[id]
	return ok
}

// Len is the number of sites resolved.
func (s SiteSet) Len() int { return len(s.ids) }

// IsEmpty reports that this grant resolved to no sites at all -- a tag that
// matches nothing, or a list whose every id was dropped by RLS. The caller
// must handle it as "reads nothing", and must never widen it.
func (s SiteSet) IsEmpty() bool { return len(s.ids) == 0 }

// Sorted returns the resolved ids in a stable order.
//
// IT RETURNS ONLY THE CALLER'S OWN SCOPE, which is what makes it safe to
// enumerate: every id here is a site this grant already resolved through
// `sites` under tenant RLS, so handing the list back to that same grant
// discloses nothing it was not already given. It exists so a fleet envelope
// can account for an in-scope site that went unread, rather than dropping it
// and returning a short list that reads as a complete fleet.
//
// The order is by id and not by name: this is used for accounting, and a
// stable order that does not depend on tenant-controlled strings keeps the
// output deterministic across calls.
func (s SiteSet) Sorted() []uuid.UUID {
	out := make([]uuid.UUID, 0, len(s.ids))
	for id := range s.ids {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

// ---------------------------------------------------------------------------
// The operator-facing view of a grant: what the connections list renders and
// what the revoke button acts on (S16, design Step 10).
// ---------------------------------------------------------------------------

// ClientProtocolState is which of FOUR distinguishable facts a grant's stored
// client identity represents. Four, not three, and not one nullable string.
//
// m124 Decision 10 stores two independent columns, and the pair carries more
// information than either alone:
//
//	client_identity_recorded_at IS NULL      -> the client has NEVER CONNECTED.
//	                                            It completed the OAuth dance and
//	                                            has not yet opened a session, so
//	                                            it has told us nothing at all.
//	recorded_at set, protocol_version NULL   -> it connected AND SENT NO HEADER.
//	                                            Most clients do exactly this;
//	                                            NegotiateProtocol treats absence
//	                                            as the floor and does not refuse.
//	recorded_at set, protocol_version set    -> it sent a revision string, which
//	                                            is either one we speak or one we
//	                                            do not.
//
// The first two are the pair this type exists to keep apart. Collapsing them
// gives an operator "no protocol version" for a connection that has never been
// used, which reads as a compatibility problem when the truth is that nothing
// has happened yet. The last two split because "2025-06-18" and "banana" are
// not the same answer either.
type ClientProtocolState string

const (
	// ClientProtocolNeverConnected: client_identity_recorded_at IS NULL.
	ClientProtocolNeverConnected ClientProtocolState = "never_connected"
	// ClientProtocolAbsent: connected, sent no MCP-Protocol-Version header.
	ClientProtocolAbsent ClientProtocolState = "absent"
	// ClientProtocolRecognised: sent a revision in SupportedRevisions().
	ClientProtocolRecognised ClientProtocolState = "recognised"
	// ClientProtocolUnrecognised: sent something this server does not speak.
	ClientProtocolUnrecognised ClientProtocolState = "unrecognised"
)

// ClientProtocol is a stored protocol_version classified into one of the four
// states above.
//
// Version is set ONLY for Recognised and Unrecognised, and it is the string the
// CLIENT SENT rather than anything this server assumed. It is deliberately
// empty for NeverConnected and for Absent: NULL means "this client sent no
// protocol header", NOT "unknown version", and rendering the negotiated floor
// here would be this package asserting a claim the client never made.
type ClientProtocol struct {
	State   ClientProtocolState
	Version string
}

// ClassifyStoredProtocol turns the two stored columns into the four-state
// answer. It is the ONE place that mapping is made, so a caller cannot invent a
// fifth reading or flatten two states into one.
//
// THE ORDER OF THE BRANCHES IS THE CONTRACT. recordedAt is tested FIRST and
// alone, because it is the column that separates "never connected" from
// everything else; testing the version first would report a never-connected
// grant as "sent no header", which is the exact conflation Decision 10 stores
// two columns to prevent.
//
// The empty-string case is deliberately NOT routed through NegotiateProtocol's
// absent branch. A non-NULL column holding "" is a data anomaly rather than an
// absent header, and NegotiateProtocol answers "" with NegotiationAssumedFloor
// carrying Version = ProtocolFloor -- so passing it through unguarded would
// print "2025-03-26" against a connection that sent no such thing. It is
// classified Unrecognised with the verbatim value instead, which is honest
// about both the anomaly and about our not speaking it.
func ClassifyStoredProtocol(recordedAt *time.Time, stored *string) ClientProtocol {
	if recordedAt == nil {
		return ClientProtocol{State: ClientProtocolNeverConnected}
	}
	if stored == nil {
		return ClientProtocol{State: ClientProtocolAbsent}
	}
	raw := *stored
	if strings.TrimSpace(raw) == "" {
		return ClientProtocol{State: ClientProtocolUnrecognised, Version: raw}
	}
	if n := NegotiateProtocol(raw); n.Outcome == NegotiationAccepted {
		return ClientProtocol{State: ClientProtocolRecognised, Version: n.Version}
	}
	// Below the floor, above the floor but outside the window, or unparseable.
	// All three are "not a revision we speak", and the caller renders the raw
	// value so an operator can see what was actually sent.
	return ClientProtocol{State: ClientProtocolUnrecognised, Version: raw}
}

// Connection is one AI connection as an OPERATOR sees it: the row from
// mcp_grants, with every nullable column kept nullable.
//
// EVERY POINTER HERE IS LOAD-BEARING AND NONE OF THEM MAY BECOME A VALUE.
// LastUsedAt nil is "never used" and must stay distinguishable from a date; a
// time.Time would carry the Go zero value 0001-01-01, which serialises as a
// real timestamp and reads as "used, in the year 1". The same argument holds
// for the two reported client strings, where "" would read as a client that
// reported an empty name rather than one that reported none.
//
// WHAT IS DELIBERATELY ABSENT: ScopeSiteIds and ScopeTagIds. They are the
// columns mcp_grants_site_scope_select exists to protect (PR #569 finding F1 --
// scope_site_ids enumerates every site the organisation has granted MCP access
// to), and no consumer of this slice renders them. Adding them later is a
// deliberate act with its own review, not a field that arrives by copying the
// row.
type Connection struct {
	ID   uuid.UUID
	Name string
	// Status is the stored liveness column, never inferred from RevokedAt.
	Status GrantStatus
	// SiteScopeMode says WHICH KIND of site scope this grant carries. The
	// resolved site ids are not exposed; see the type comment.
	SiteScopeMode SiteScopeMode
	// Scopes is the OAuth scope set. See Service.ListConnections for why this
	// is currently derived rather than read from the row.
	Scopes []Scope

	// Capabilities is READ STRAIGHT OFF mcp_grants.capabilities, the same
	// column and the same authority Authenticate reads chk.GrantCapabilities
	// from to decide the `authorized` verdict. It is NOT recomputed from the
	// scope registry -- that is exactly the mistake m127 DECISION 1 exists to
	// forbid, because a recomputed set agrees with the stored one only until
	// they diverge, and a connections list is precisely where an operator goes
	// to find out whether they have. GH #652: until the capability vocabulary
	// widened to eight strings, every grant held exactly {mcp.sites.read}, so
	// an omitted field here was indistinguishable from a correct one. It no
	// longer is.
	Capabilities []Capability
	CreatedAt    time.Time

	// ReportedClientName and ReportedClientVersion are the client's OWN claims,
	// recorded at connect. They are UNVERIFIED -- registration is
	// unauthenticated (m124 obligation 7) -- and are never defaulted to Name,
	// which is the operator's claim. The two can disagree and that disagreement
	// is worth seeing.
	ReportedClientName    *string
	ReportedClientVersion *string

	// SetupClient is THE OPERATOR'S CHOICE at the wizard's step 2, written once
	// at creation and never again. It is a fourth client fact, not a tidier
	// spelling of the two above, and the distinction is the whole reason the
	// column exists (m128 DECISION 1).
	//
	// nil MEANS "NO OPERATOR CHOICE WAS RECORDED" AND NOTHING ELSE. It does not
	// mean "generic": "generic" is the operator actively choosing "Other MCP
	// client" from the nine cards, which is a stated choice, and step 9 renders
	// the two differently. Never substitute one for the other in either
	// direction.
	//
	// IT MAY DISAGREE WITH ReportedClientName PERMANENTLY AND LEGITIMATELY --
	// set up for Claude Desktop, URL pasted into Cursor -- and neither is the
	// other's stale copy, so neither may overwrite the other. In particular
	// RecordConnect writes the reported pair and MUST NOT write this. It is
	// also the only one of the three that survives the never-connected case,
	// where both reported columns are nil by definition and step 9 still has to
	// say what the connection was set up for.
	SetupClient *string

	// Protocol is the four-state classification of the stored header.
	Protocol ClientProtocol

	// LastUsedAt is nil when the connection has never been used.
	LastUsedAt *time.Time
	// RevokedAt is nil for a live connection.
	RevokedAt *time.Time
}
