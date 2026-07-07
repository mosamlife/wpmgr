-- Client portal member queries (m66). Auth-time and agency roster operations.
-- GetClientAccessForUserTenant and FirstClientMemberTenant run under InUserTx
-- (app.user_id only); all other queries run under InTenantTx.

-- name: GetClientAccessForUserTenant :many
-- Runs under InUserTx (app.user_id; clients rows visible via
-- clients_member_read). The INNER JOIN to clients excludes ARCHIVED clients
-- at the auth chokepoint, so ClientIDs itself never carries an archived
-- client and reports/branding/overview access drops with the sites. LEFT
-- JOIN sites: a zero-site active client still yields a row (NULL site_id) so
-- the principal gets portal access + branding before sites are assigned.
SELECT cm.client_id, s.id AS site_id
FROM client_members cm
JOIN clients cl
  ON cl.id = cm.client_id AND cl.tenant_id = cm.tenant_id
 AND cl.archived_at IS NULL
LEFT JOIN sites s
  ON s.client_id = cm.client_id AND s.tenant_id = cm.tenant_id
WHERE cm.user_id = $1 AND cm.tenant_id = $2;

-- name: FirstClientMemberTenant :one
-- Runs under InUserTx. Returns the tenant of the user's earliest ACTIVE
-- client membership, used at login to resolve an active tenant for
-- portal-only users (mirrors FirstActiveShareTenant in auth/repo.go).
-- Archived clients are excluded so a stale earliest membership cannot
-- shadow a live one in another tenant.
--
-- GH #152 LOW fast-follow: also joins tenants and excludes a soft-deleted
-- tenant — mirrors ListSharesForUser's own fix (same login-time active-tenant
-- hint, same 403-loop failure mode for a portal-only user whose only access
-- was into a now-deleted org).
SELECT cm.tenant_id
FROM client_members cm
JOIN clients cl
  ON cl.id = cm.client_id AND cl.tenant_id = cm.tenant_id
 AND cl.archived_at IS NULL
JOIN tenants t ON t.id = cm.tenant_id
WHERE cm.user_id = $1 AND t.deleted_at IS NULL
ORDER BY cm.created_at ASC LIMIT 1;

-- name: ListMembersForClient :many
-- Agency roster: all members for a client, newest first.
SELECT cm.id, cm.user_id, cm.client_id, cm.tenant_id, cm.invited_by, cm.created_at
FROM client_members cm
WHERE cm.client_id = @client_id AND cm.tenant_id = @tenant_id
ORDER BY cm.created_at DESC, cm.id DESC;

-- name: CreateClientMember :one
-- Upsert: ON CONFLICT DO NOTHING so the caller detects "already a member" by
-- checking for zero RETURNING rows (pgx.ErrNoRows), matching the Conflict pattern.
INSERT INTO client_members (tenant_id, client_id, user_id, invited_by)
VALUES (@tenant_id, @client_id, @user_id, @invited_by)
ON CONFLICT (client_id, user_id) DO NOTHING
RETURNING *;

-- name: DeleteClientMember :execrows
-- Immediate revoke. Returns 0 rows when the member does not exist.
DELETE FROM client_members
WHERE client_id = @client_id AND user_id = @user_id AND tenant_id = @tenant_id;
