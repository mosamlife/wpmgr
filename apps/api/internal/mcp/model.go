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

import "github.com/google/uuid"

// Scope is an OAuth scope this surface recognises. The registry in scope.go is
// CLOSED: an unrecognised scope is refused, never ignored.
type Scope string

// ScopeRead is the only scope this surface grants, and the surface is
// read-only by construction rather than by configuration (m124 DECISION 1:
// there is deliberately no capability column, because it would be the place a
// write capability could later appear without a migration and without a
// review). A write scope does not belong here; it belongs in its own migration
// with its own review.
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
