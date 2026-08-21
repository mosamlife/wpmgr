package domain

import (
	"context"

	"github.com/google/uuid"
)

// PrincipalType distinguishes a human (session) caller from a machine (API key).
type PrincipalType string

const (
	// PrincipalUser is a human authenticated via a session cookie.
	PrincipalUser PrincipalType = "user"
	// PrincipalAPIKey is a machine authenticated via a bearer API key.
	PrincipalAPIKey PrincipalType = "api_key"
)

// ScopeOrg is the scope value for a full org member (existing behaviour).
const ScopeOrg = "org"

// AuthModelRole is the legacy authorization model: permissions are derived from
// the principal's role through the totally ordered role hierarchy. It is the
// meaning of the zero value, so a Principal built by code that predates m120
// (#510) behaves exactly as it did before.
const AuthModelRole = "role"

// AuthModelCapability is the least-privilege authorization model: the explicit
// capability set is authoritative and the role is NEVER consulted, not even as
// a fallback when the set is empty. These strings match the api_keys.auth_model
// column values exactly (see migration m120).
const AuthModelCapability = "capability"

// ScopeSite is the scope value for an outside collaborator who has been granted
// access to one or more specific sites via site_shares, but has no membership
// row in the active tenant.
const ScopeSite = "site"

// Principal is the authenticated caller for a request: who they are, which
// tenant is active, and their role in that tenant. It is derived by the auth
// middleware from EITHER a session OR a bearer API key, never from a raw header.
type Principal struct {
	Type     PrincipalType
	UserID   uuid.UUID // set when Type == PrincipalUser
	APIKeyID uuid.UUID // set when Type == PrincipalAPIKey
	TenantID uuid.UUID // the active tenant the request operates in
	Role     string    // the principal's role within TenantID

	// Scope is "org" (default, full member) or "site" (scoped collaborator with
	// access only to sites listed in AllowedSiteIDs). All existing code that
	// does not set Scope gets the zero-value ""; callers treat "" as "org" for
	// backward compatibility.
	Scope string

	// AllowedSiteIDs is populated only when Scope == ScopeSite. It contains the
	// set of site UUIDs the principal may access in the active tenant, derived at
	// auth time from non-expired site_shares rows. It is empty for Scope=="org".
	AllowedSiteIDs []uuid.UUID

	// AuthModel selects how this principal's permissions are computed:
	// AuthModelRole (the default, and the zero value's meaning) consults Role
	// through the totally ordered hierarchy; AuthModelCapability consults
	// Capabilities ONLY and never falls back to Role.
	//
	// This field is deliberately redundant with `Capabilities != nil`, and it
	// is the reason the collapse below is not a fail-open. The database column
	// capabilities is nullable and sqlc generates it as a plain []string with
	// no validity flag, so SQL NULL and '{}' both arrive here as a zero-length
	// slice: len(Capabilities) == 0 cannot distinguish "no capability set, use
	// the role" from "a real capability set containing nothing, allow nothing".
	// Reading AuthModel instead of the slice length is what keeps a
	// zero-capability key at zero authority rather than handing it its role.
	//
	// The empty string means AuthModelRole, so every principal built before
	// m120 (#510) — and every session principal, which has no key row at all —
	// keeps exactly the authority it had.
	AuthModel string

	// Capabilities is the explicit permission set for an AuthModelCapability
	// principal. Each element is an authz.Permission string validated against
	// the vocabulary at mint time. Empty (or nil) means zero authority when
	// AuthModel == AuthModelCapability; it is meaningless and ignored when
	// AuthModel is AuthModelRole.
	Capabilities []string

	// ClientIDs holds the client UUIDs the principal belongs to as a portal
	// member. Populated only when Role == "client" (resolved via client_members).
	// Empty for every non-portal principal.
	ClientIDs []uuid.UUID
}

// IsCapabilityScoped reports whether this principal's authority comes from an
// explicit capability set rather than from its role rank. Call this instead of
// testing len(Capabilities) — see the AuthModel doc comment for why the slice
// length is not a safe discriminator.
func (p Principal) IsCapabilityScoped() bool {
	return p.AuthModel == AuthModelCapability
}

// ActorID returns the stable identifier of the principal for audit logging.
func (p Principal) ActorID() string {
	if p.Type == PrincipalAPIKey {
		return p.APIKeyID.String()
	}
	return p.UserID.String()
}

// GetScope returns the principal's scope ("org", "site", or "" for legacy org
// principals). It satisfies the db.Pool.RunTenantTx principal interface.
func (p Principal) GetScope() string { return p.Scope }

// GetUserID returns the principal's user UUID (uuid.Nil for API-key principals).
// It satisfies the db.Pool.RunTenantTx principal interface.
func (p Principal) GetUserID() uuid.UUID { return p.UserID }

// GetTenantID returns the principal's active tenant UUID.
// It satisfies the db.Pool.RunTenantTx principal interface.
func (p Principal) GetTenantID() uuid.UUID { return p.TenantID }

// GetAllowedSiteIDs returns the slice of site UUIDs accessible to a
// site-scoped principal. It is empty for Scope=="org".
// It satisfies the db.Pool.RunTenantTx principal interface.
func (p Principal) GetAllowedSiteIDs() []uuid.UUID { return p.AllowedSiteIDs }

// IsSiteConstrained reports whether this principal's site access is limited to
// an explicit allowlist rather than being tenant-wide. It is the SINGLE
// predicate behind every site gate — CanAccessSite, authz.RequireSiteAccess,
// authz.AuthorizeSites and db.RunTenantTx's dispatch all ask this one question,
// so a principal cannot be constrained by one gate and tenant-wide at the next.
//
// Two disjuncts, and the second is the fail-closed backstop:
//
//   - Scope == ScopeSite is authoritative and is the ONLY condition that can
//     fire for a principal loaded from the database. m120's
//     api_keys_site_scope_check pins site_scope to 'org' | 'site', and
//     api_keys_site_scope_allowlist_check forbids the half-state (site_scope
//     'org' carrying a populated allowed_site_ids). site_scope, not the
//     emptiness of the list, is what says "restricted", because "restricted to
//     zero sites" is a legitimate fail-CLOSED state that must stay expressible.
//
//   - A non-empty AllowedSiteIDs constrains regardless of the Scope label. For
//     any principal built by apikey.PrincipalFor or by the session path this
//     disjunct is UNREACHABLE — PrincipalFor copies the allowlist only when
//     Scope == ScopeSite, and the DB CHECK makes the stored half-state
//     impossible — so it changes no existing behaviour by one bit. It exists so
//     that a hand-built Principal, or a future constructor that populates the
//     allowlist and forgets the label, fails CLOSED instead of silently
//     widening to the whole tenant.
//
// Note what is deliberately NOT here: a bare "no scope set" does not constrain.
// The zero-value Scope means org, and org-scoped user principals are legitimately
// tenant-wide today. Refusing them would break real access for real members,
// which is why the predicate keys on an expressed restriction, not on silence.
func (p Principal) IsSiteConstrained() bool {
	return p.Scope == ScopeSite || len(p.AllowedSiteIDs) > 0
}

// CanAccessSite reports whether this principal may access the given site.
// Unconstrained principals (org members, tenant-wide API keys) are bounded by
// the tenant filter and RLS, so this returns true for them. Constrained
// principals are held to their explicit allowlist — including the empty
// allowlist, which admits nothing.
//
// This is the canonical app-layer gate for a by-id resource whose site is only
// known after the row is loaded, which the path-based RequireSiteAccess
// middleware cannot cover. Call it after resolving the resource's site_id and
// 404 when it returns false. For a route that fans out over MANY sites, use
// authz.AuthorizeSites instead: it returns a token that carries the filtered
// set, so the fan-out cannot proceed on an unfiltered list.
func (p Principal) CanAccessSite(siteID uuid.UUID) bool {
	if !p.IsSiteConstrained() {
		return true
	}
	for _, allowed := range p.AllowedSiteIDs {
		if allowed == siteID {
			return true
		}
	}
	return false
}

type principalCtxKey struct{}

// WithPrincipal returns a copy of ctx carrying the authenticated principal. The
// principal's tenant is also mirrored into the tenant-id context so existing
// tenant-scoped code (and logging) keeps working unchanged.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	ctx = context.WithValue(ctx, principalCtxKey{}, p)
	if p.TenantID != uuid.Nil {
		ctx = WithTenantID(ctx, p.TenantID)
	}
	return ctx
}

// PrincipalFromContext returns the authenticated principal and whether one was
// present.
func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalCtxKey{}).(Principal)
	return p, ok
}
