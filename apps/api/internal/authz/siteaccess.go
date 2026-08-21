package authz

import (
	"context"
	"sync"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// STATUS: this file is scaffolding, not a boundary that is currently load
// bearing. AuthorizeSites has no production call site yet, and no route mints a
// site-scoped capability key, so no principal that it would filter exists
// outside a test. Read every claim below as "what this shape is for", not as a
// description of what the running server does. It ships ahead of its callers so
// that the routes converted next have one place to go; the conversion of those
// routes, and the wiring of SetSiteAccessAuditor in cmd/wpmgr/main.go, is the
// work that makes this a chokepoint. Until then it enforces nothing.
//
// The intent: one place that answers "which sites may this principal touch?"
// for a route that handles MANY sites at once.
//
// The by-id routes are already covered, today, by RequireSiteAccess, which keys
// on the same predicate. What that middleware cannot cover is a bulk or fleet
// route that takes one request and fans out over a set of sites resolved inside
// the handler — no :siteId is in the path, so no middleware ever sees the ids.
// Those handlers each do their own filtering, or none, and "or none" is not
// visible in review because the missing code is missing. RequireOrgScope is
// what stands in front of the fleet rollups meanwhile; it refuses a constrained
// principal outright rather than filtering for it.
//
// The intended fix is not another helper that a handler may remember to call.
// It is a value the fan-out cannot proceed without: AuthorizedSites carries the
// filtered set, its fields are unexported so only this package can populate
// one, and its zero value authorises nothing. A handler that takes the token
// has no way to iterate a set it did not filter, so the omission becomes a
// compile error or an empty loop rather than a silent tenant-wide fan-out —
// once handlers take it.

// AuthorizedSites is the result of the site-authorization chokepoint: the
// subset of a requested site set that the principal is actually permitted to
// touch.
//
// It can only be produced by AuthorizeSites. Both fields are unexported, so no
// other package can construct a populated one — `authz.AuthorizedSites{}` is
// declarable but inert, because the zero value has ok=false and no ids, and
// every accessor on it reports "nothing authorized". That is the point: the
// unforgeable-token shape turns "the handler forgot to filter" from a review
// comment into an empty result set at runtime and, where a function signature
// demands the token, into a compile error.
type AuthorizedSites struct {
	ids []uuid.UUID
	ok  bool
}

// IDs returns the authorized site ids, in the order they were requested, with
// duplicates removed. The returned slice is a copy; mutating it cannot widen
// the token. The zero value returns nil.
func (a AuthorizedSites) IDs() []uuid.UUID {
	if !a.ok || len(a.ids) == 0 {
		return nil
	}
	out := make([]uuid.UUID, len(a.ids))
	copy(out, a.ids)
	return out
}

// Len is the number of authorized sites. The zero value is 0.
func (a AuthorizedSites) Len() int {
	if !a.ok {
		return 0
	}
	return len(a.ids)
}

// Contains reports whether a specific site survived authorization. The zero
// value returns false for every id, including uuid.Nil.
func (a AuthorizedSites) Contains(id uuid.UUID) bool {
	if !a.ok {
		return false
	}
	for _, s := range a.ids {
		if s == id {
			return true
		}
	}
	return false
}

// Authorized reports whether this token came from AuthorizeSites at all. A
// false result means the value is a zero literal someone declared rather than a
// filtered set, which is never a state a real fan-out should act on.
func (a AuthorizedSites) Authorized() bool { return a.ok }

// SiteAccessAuditor receives one record per denied site. It is a local
// interface, not a dependency on internal/audit, so wiring the real auditor in
// cannot create an import cycle.
type SiteAccessAuditor interface {
	RecordSiteAccessDenied(ctx context.Context, p domain.Principal, siteIDs []uuid.UUID)
}

var (
	auditorMu sync.RWMutex
	auditor   SiteAccessAuditor
)

// SetSiteAccessAuditor installs the process-wide auditor for chokepoint
// denials. It is meant to be called once during server wiring, and as of #513
// cmd/wpmgr/main.go does not call it: the auditor is nil in the running binary
// and no denial is recorded. That is not yet a gap in the audit trail, because
// AuthorizeSites has no production caller to deny anything; wiring it belongs
// with the first route that does.
//
// A nil auditor is allowed and means denials are filtered but not recorded —
// the filtering is the security boundary, the audit trail is the evidence.
func SetSiteAccessAuditor(a SiteAccessAuditor) {
	auditorMu.Lock()
	defer auditorMu.Unlock()
	auditor = a
}

func currentAuditor() SiteAccessAuditor {
	auditorMu.RLock()
	defer auditorMu.RUnlock()
	return auditor
}

// AuthorizeSites is the intended chokepoint for bulk routes — intended, because
// it has no production caller yet; see the STATUS note at the top of this file.
// Given the sites a bulk request wants to operate on, it returns the subset the
// principal may actually touch, plus the ids that were refused so the caller can
// shape its response.
//
// Semantics, and each one is deliberate:
//
//   - An unconstrained principal (org member, tenant-wide key) is authorized
//     for everything it requested. This is not a widening: the tenant filter in
//     every query and the tenant RLS policies still bound it to its own tenant.
//     AuthorizeSites answers the site question only, and a site id from another
//     tenant survives this filter — it dies at the tenant boundary instead.
//
//   - A constrained principal keeps only the requested ids that are on its
//     allowlist. An empty allowlist authorizes nothing, which is the
//     fail-closed state m120's site_scope column exists to keep expressible.
//
//   - An empty request authorizes nothing and denies nothing. It does NOT mean
//     "all sites". A caller that wants the principal's full reach must pass the
//     candidate set it resolved; there is no expansion path here, because
//     silence must not mean everything.
//
// Duplicates in requested are collapsed. Order is preserved so a response can
// be zipped against the request.
//
// Denials are passed to the installed SiteAccessAuditor before the token is
// returned, so that once an auditor is wired every refusal at a fan-out route
// leaves a trail without each handler having to remember to write one. No
// auditor is installed today, so the call is a no-op.
func AuthorizeSites(ctx context.Context, p domain.Principal, requested []uuid.UUID) (AuthorizedSites, []uuid.UUID) {
	allowed := make([]uuid.UUID, 0, len(requested))
	var denied []uuid.UUID
	seen := make(map[uuid.UUID]struct{}, len(requested))

	constrained := p.IsSiteConstrained()
	for _, id := range requested {
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		if !constrained || p.CanAccessSite(id) {
			allowed = append(allowed, id)
			continue
		}
		denied = append(denied, id)
	}

	if len(denied) > 0 {
		if a := currentAuditor(); a != nil {
			a.RecordSiteAccessDenied(ctx, p, denied)
		}
	}

	// ok is true whenever this function produced the value, even when the
	// filtered set is empty. "Authorized for zero sites" and "never went
	// through the chokepoint" are different states and must stay
	// distinguishable — collapsing them is how a zero literal would start
	// passing for a real, if empty, authorization.
	return AuthorizedSites{ids: allowed, ok: true}, denied
}

// AuthorizeSite is the single-site form, for a fan-out that has already
// narrowed to one id and wants the same audited path. It is exactly
// AuthorizeSites with a one-element slice.
func AuthorizeSite(ctx context.Context, p domain.Principal, siteID uuid.UUID) bool {
	tok, _ := AuthorizeSites(ctx, p, []uuid.UUID{siteID})
	return tok.Contains(siteID)
}
