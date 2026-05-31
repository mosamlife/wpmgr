// Package authz defines the WPMgr role hierarchy, the permission matrix, and
// the helpers handlers/middleware use to enforce role minimums and discrete
// permissions. Roles are totally ordered: owner > admin > operator > viewer.
package authz

// Role is a principal's role within a tenant.
type Role string

const (
	// RoleViewer can read tenant-scoped resources but not mutate them.
	RoleViewer Role = "viewer"
	// RoleOperator can manage sites (create/update/delete) in addition to reads.
	RoleOperator Role = "operator"
	// RoleAdmin can manage members and API keys and read the audit log.
	RoleAdmin Role = "admin"
	// RoleOwner has full control of the tenant, including ownership transfer.
	RoleOwner Role = "owner"
)

// rank gives each role a comparable level; higher is more privileged.
var rank = map[Role]int{
	RoleViewer:   1,
	RoleOperator: 2,
	RoleAdmin:    3,
	RoleOwner:    4,
}

// Valid reports whether r is a known role.
func (r Role) Valid() bool {
	_, ok := rank[r]
	return ok
}

// AtLeast reports whether r is at least as privileged as min.
func (r Role) AtLeast(min Role) bool {
	return rank[r] >= rank[min]
}

// Permission is a discrete capability checked by RequirePermission.
type Permission string

const (
	// PermSiteRead lists/reads sites.
	PermSiteRead Permission = "site:read"
	// PermSiteWrite creates/updates/deletes sites.
	PermSiteWrite Permission = "site:write"
	// PermMemberRead lists tenant members.
	PermMemberRead Permission = "member:read"
	// PermMemberManage invites/updates/removes tenant members.
	PermMemberManage Permission = "member:manage"
	// PermAPIKeyRead lists API keys.
	PermAPIKeyRead Permission = "apikey:read"
	// PermAPIKeyManage creates/revokes API keys.
	PermAPIKeyManage Permission = "apikey:manage"
	// PermAuditRead reads the audit log.
	PermAuditRead Permission = "audit:read"
	// PermTenantManage manages tenant settings.
	PermTenantManage Permission = "tenant:manage"
	// PermSiteAutologin mints a one-time autologin URL into a managed WordPress
	// site. The minted JWT lets the receiving agent establish an authenticated
	// wp-admin session in the operator's browser. Reserved for owner+admin in V0
	// (operator/viewer are explicitly excluded; finer per-grant flows are out of
	// scope for V0).
	PermSiteAutologin Permission = "site:autologin"
	// PermMediaDeleteOriginals authorises the IRREVERSIBLE "delete originals"
	// media action (ADR-043 §6): once a site's archived originals are deleted,
	// an optimized attachment can never be restored. Gated at admin+ (above the
	// operator-level PermSiteWrite that guards sync/optimize/restore) and paired
	// with a type-the-hostname UI confirmation.
	PermMediaDeleteOriginals Permission = "media:delete_originals"
)

// minRoleFor maps each permission to the minimum role that holds it. The matrix
// is intentionally simple (role-rank based) for V0; finer-grained grants can be
// layered later without changing call sites.
var minRoleFor = map[Permission]Role{
	PermSiteRead:      RoleViewer,
	PermSiteWrite:     RoleOperator,
	PermMemberRead:    RoleViewer,
	PermMemberManage:  RoleAdmin,
	PermAPIKeyRead:    RoleAdmin,
	PermAPIKeyManage:  RoleAdmin,
	PermAuditRead:     RoleAdmin,
	PermTenantManage:  RoleOwner,
	PermSiteAutologin: RoleAdmin,
	// Irreversible media original-deletion: admin+ (ADR-043 §6).
	PermMediaDeleteOriginals: RoleAdmin,
}

// Allows reports whether role r is permitted to perform p.
func Allows(r Role, p Permission) bool {
	min, ok := minRoleFor[p]
	if !ok {
		return false
	}
	return r.AtLeast(min)
}
