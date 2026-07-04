// Package authz defines the WPMgr role hierarchy, the permission matrix, and
// the helpers handlers/middleware use to enforce role minimums and discrete
// permissions. Roles are totally ordered: owner > admin > operator > viewer > client.
package authz

// Role is a principal's role within a tenant.
type Role string

const (
	// RoleClient is a read-only client portal principal. Ranked below viewer;
	// holds zero permissions. Portal access is granted via client_members, not
	// org membership or site shares. A future permission grant must be explicit.
	RoleClient Role = "client"
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
// RoleClient sits at 1 (not 0) so that a missing-key zero-value does NOT
// accidentally satisfy AtLeast(RoleClient). Every registered role is
// reachable by rank; an unregistered string gets 0 and fails all AtLeast checks.
var rank = map[Role]int{
	RoleClient:   1,
	RoleViewer:   2,
	RoleOperator: 3,
	RoleAdmin:    4,
	RoleOwner:    5,
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
	// PermAuditManage moves a tenant's audit-integrity re-baseline anchor
	// (audit.integrity.rebaselined) — the operation that lets an operator
	// acknowledge a historical, unrepairable chain break so Verify stops
	// reporting it while still catching any NEW tampering going forward.
	// Owner-only: it is a tenant-wide trust decision about the integrity
	// mechanism itself, the same bar as PermTenantManage/PermSMTPManage, and
	// strictly higher than the admin-level PermAuditRead it sits alongside.
	PermAuditManage Permission = "audit:manage"
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
	// PermSMTPManage edits the instance-level SMTP relay (ADR-045): host/port/
	// credentials/From + the send-test. It writes a stored secret and is the
	// instance's mail transport, so it sits with PermTenantManage at owner-only.
	PermSMTPManage Permission = "smtp:manage"
	// PermSiteCacheManage enables/disables and reconfigures the agent-side page
	// cache for a site (Performance Suite, ADR-046). Operator+ — the same
	// site-management tier as PermSiteWrite; site-scoped (NOT in orgLevelPerms),
	// so a collaborator with access to a site can manage that site's cache.
	PermSiteCacheManage Permission = "site.cache.manage"
	// PermSiteCachePurge triggers a cache purge/preload for a site (ADR-046).
	// Operator+; site-scoped.
	PermSiteCachePurge Permission = "site.cache.purge"
	// PermSitePerfConfig saves the per-site performance configuration — minify,
	// RUCSS, lazy-load, CDN, DB-clean, bloat removal (ADR-046). Operator+;
	// site-scoped.
	PermSitePerfConfig Permission = "site.perf.config"
	// PermSiteCacheDeleteAll authorises the destructive "delete everything"
	// cache action — drop the on-disk cache directory, the advanced-cache
	// drop-in and the managed .htaccess block in one shot (ADR-046). Gated at
	// admin+ (above the operator-level cache perms), mirroring the
	// PermMediaDeleteOriginals destructive-action precedent.
	PermSiteCacheDeleteAll Permission = "site.cache.delete-everything"

	// PermMediaCleanScan authorises the read-only attachment reference scan
	// (#190). Viewer+ — no side effects.
	PermMediaCleanScan Permission = "site.media.clean.scan"
	// PermMediaCleanWrite authorises the reversible isolate and restore actions
	// (#190). Operator+ — data-mutation operations comparable to PermSiteWrite.
	PermMediaCleanWrite Permission = "site.media.clean.write"
	// PermMediaCleanDelete authorises the PERMANENT deletion of quarantined
	// attachments (#190). Admin+ — irreversible, mirrors PermMediaDeleteOriginals.
	PermMediaCleanDelete Permission = "site.media.clean.delete"

	// PermEmailManage configures and reads per-site outgoing email (SMTP /
	// provider config, secrets, test-send). Operator+ (same tier as
	// PermSiteCacheManage and PermSitePerfConfig) — site-write-class, NOT in
	// orgLevelPerms. A site collaborator with access to a site can manage
	// that site's email config.
	PermEmailManage Permission = "site.email.manage"

	// PermClientRead lists and reads agency clients (m63). Viewer+ — org-scoped
	// only; site-scoped collaborators never see the client roster.
	PermClientRead Permission = "client:read"
	// PermClientManage creates, updates, deletes, and assigns agency clients (m63).
	// Operator+ — same tier as PermSiteWrite; includes the assignment flow.
	PermClientManage Permission = "client:manage"

	// PermSecurityManage reads and writes the per-site security hardening
	// config and ban list (ADR-057, Security Suite Phase 1). Operator+ —
	// same tier as PermSiteCacheManage and PermEmailManage; site-scoped so
	// a collaborator with access to a site can manage that site's security
	// hardening. Viewers get read access via PermSiteRead on the GET routes.
	PermSecurityManage Permission = "site.security.manage"

	// PermSiteFilesRead authorises browse/list/read/download operations on the
	// per-site File Manager (P1, read-only). Admin+ — the file manager exposes
	// raw filesystem content including configuration files, and is
	// off-by-default per site. Viewer and operator tiers are excluded: this is
	// a high-privilege, site-filesystem-access capability.
	PermSiteFilesRead Permission = "site.files.read"

	// PermSiteFilesReadSensitive authorises reading or downloading a sensitive
	// file (wp-config.php, .env*, *.pem, *.key, id_rsa*, .git/, .htpasswd,
	// auth.json). Owner only — these files contain secrets and their exposure
	// is the highest-risk read operation in the manager. The caller must also
	// pass confirm_sensitive=true (belt-and-braces, T6).
	PermSiteFilesReadSensitive Permission = "site.files.read_sensitive"

	// PermSiteFilesManage enables or disables the file manager for a site via
	// the settings endpoint (PUT /sites/{siteId}/files/settings). Admin+ —
	// turning on the file manager is an access-control decision that grants
	// filesystem visibility to all admin+ members of the site; it should not be
	// delegated to operator-level principals.
	PermSiteFilesManage Permission = "site.files.manage"

	// PermSiteFilesWrite authorises write operations on the per-site File Manager
	// (P2): file_write, file_mkdir, file_rename, file_chmod, file_upload_apply.
	// Admin+ — same tier as PermSiteFilesRead; write is equally privileged.
	// The per-site files_write_enabled flag must ALSO be true before the CP
	// will sign any write command; this permission governs role access while the
	// flag governs per-site opt-in.
	PermSiteFilesWrite Permission = "site.files.write"

	// PermSiteFilesDelete authorises the destructive file_delete operation (P2).
	// Owner only — deletion is permanent and cannot be undone without a backup.
	// The caller must also supply confirm="DELETE" in the request body.
	PermSiteFilesDelete Permission = "site.files.delete"

	// PermSiteFilesWriteCode authorises writes whose target path matches the
	// executable-extension deny-list or the sensitive-file deny-list when the
	// caller sets confirm_executable_write=true or confirm_sensitive=true (P2).
	// Owner only — executable writes can introduce code-execution paths; this is
	// the highest-risk write operation in the manager.
	PermSiteFilesWriteCode Permission = "site.files.write_code"
)

// minRoleFor maps each permission to the minimum role that holds it. The matrix
// is intentionally simple (role-rank based) for V0; finer-grained grants can be
// layered later without changing call sites.
//
// RoleClient intentionally appears in ZERO entries. Allows(RoleClient, p) is
// false for every Permission, including PermSiteRead. Portal routes use
// RequireClientPortal() instead of RequirePermission(). A future permission
// grant for client principals must be added here deliberately.
var minRoleFor = map[Permission]Role{
	PermSiteRead:      RoleViewer,
	PermSiteWrite:     RoleOperator,
	PermMemberRead:    RoleViewer,
	PermMemberManage:  RoleAdmin,
	PermAPIKeyRead:    RoleAdmin,
	PermAPIKeyManage:  RoleAdmin,
	PermAuditRead:     RoleAdmin,
	PermAuditManage:   RoleOwner,
	PermTenantManage:  RoleOwner,
	PermSiteAutologin: RoleAdmin,
	// Irreversible media original-deletion: admin+ (ADR-043 §6).
	PermMediaDeleteOriginals: RoleAdmin,
	// Instance SMTP transport + stored secret: owner-only (ADR-045).
	PermSMTPManage: RoleOwner,
	// Performance Suite (ADR-046). Cache enable/purge + perf config are
	// site-management actions at operator+; the destructive delete-everything is
	// admin+ (mirrors PermMediaDeleteOriginals).
	PermSiteCacheManage:    RoleOperator,
	PermSiteCachePurge:     RoleOperator,
	PermSitePerfConfig:     RoleOperator,
	PermSiteCacheDeleteAll: RoleAdmin,
	// Media Cleaner (#190). Scan is read-only (viewer+); isolate/restore are
	// reversible mutations (operator+); permanent delete is admin+ (irreversible).
	PermMediaCleanScan:   RoleViewer,
	PermMediaCleanWrite:  RoleOperator,
	PermMediaCleanDelete: RoleAdmin,
	// Per-site Email Management (m59). Operator+ — site-write-class.
	PermEmailManage: RoleOperator,
	// Agency Clients (m63). Read: viewer+; manage: operator+; org-scoped only.
	PermClientRead:   RoleViewer,
	PermClientManage: RoleOperator,
	// Security Suite (ADR-057). Hardening config + ban list: operator+;
	// site-write-class, mirrors PermSiteCacheManage and PermEmailManage.
	PermSecurityManage: RoleOperator,
	// File Manager (P1). Browse/read/download: admin+; sensitive-file reads: owner only.
	// Settings (enable/disable toggle): admin+ (access-control decision).
	PermSiteFilesRead:          RoleAdmin,
	PermSiteFilesReadSensitive: RoleOwner,
	PermSiteFilesManage:        RoleAdmin,
	// File Manager (P2). Write/mkdir/rename/chmod/upload: admin+; delete: owner;
	// executable/sensitive writes (confirm_executable_write/confirm_sensitive): owner.
	PermSiteFilesWrite:     RoleAdmin,
	PermSiteFilesDelete:    RoleOwner,
	PermSiteFilesWriteCode: RoleOwner,
}

// Allows reports whether role r is permitted to perform p.
func Allows(r Role, p Permission) bool {
	min, ok := minRoleFor[p]
	if !ok {
		return false
	}
	return r.AtLeast(min)
}
