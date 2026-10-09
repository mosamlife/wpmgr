-- ai_trust (m174): the site's AI mode, the connection's switch, the
-- connection's automatic usage, the AI activity feed, and the one-time
-- notice to organisations whose sites moved to the launch default.
--
-- WHICH TRANSACTION, PER STATEMENT.
--
--   * The site mode statements run in the caller's own tenant transaction
--     (db.RunTenantTx with the request's principal). sites is FORCE ROW LEVEL
--     SECURITY with sites_tenant_isolation and the RESTRICTIVE
--     sites_site_scope, so a site collaborator reads and writes only their own
--     sites. sites_ai_mode_guard decides who may write which mode: a mode
--     above ask is written only with app.user_id equal to the recorded setter,
--     and with no user the only write is down to ask, recorded as tightened.
--   * The connection statements run in an organisation-wide tenant
--     transaction. mcp_grants_site_scope_select and _update refuse every
--     grant row while app.site_scope is 'on', so a site-scoped principal reads
--     and writes none (a connection is an organisation-wide credential).
--     mcp_grants_ai_auto_guard requires app.user_id to be the recorded setter
--     whenever a connection is allowed to run by the site's setting.
--   * The usage count runs organisation-wide (db.InTenantTx), as the decision
--     engine's own count does: a site allowlist in the transaction would
--     undercount and fail open.
--   * The activity page runs under the caller's principal: row security
--     narrows a site collaborator to their own sites on both request tables.
--   * The launch notice runs once per tenant under db.InTenantTx. sites is
--     FORCE ROW LEVEL SECURITY, so a statement with no tenant in the
--     transaction sees no site and claims nothing.
--
-- TIME IS THE DATABASE'S. Windows arrive as whole seconds and are measured
-- against now() in the database.

-- ---------------------------------------------------------------------------
-- The site's AI mode
-- ---------------------------------------------------------------------------

-- name: GetSiteAIMode :one
-- The site's mode, how it was chosen, by whom and when, its version, and
-- whether AI editing is on. set_by_name is a person's name (render it as
-- text); set_by_account_deleted is true when a setter is recorded and that
-- account no longer exists. No row (pgx.ErrNoRows) when the site is not
-- visible to the caller.
SELECT s.id,
       s.ai_mode,
       s.ai_mode_source,
       s.ai_mode_set_by,
       s.ai_mode_set_at,
       s.ai_mode_version,
       s.ai_mode_step_up,
       s.ai_mode_launch_emailed_at,
       s.content_editing_enabled_at,
       u.name AS set_by_name,
       (s.ai_mode_set_by IS NOT NULL AND u.id IS NULL)::boolean AS set_by_account_deleted
FROM sites s
LEFT JOIN users u ON u.id = s.ai_mode_set_by
WHERE s.tenant_id = @tenant_id
  AND s.id = @site_id;

-- name: SetSiteAIMode :one
-- Sets the site's mode as a compare-and-set on ai_mode_version. Exactly one
-- row comes back for a visible site:
--
--   * applied = true: the write was made. When the mode, its source or its
--     setter differ from what is stored, the version moves by one and the set
--     time and step-up are recorded; when all three are what is stored, the
--     row is left as it is (saving the setting that is already set changes
--     nothing).
--   * applied = false: expected_version is not the stored version. Nothing
--     was written, and the row carries the stored mode and version, which is
--     what a 409 stale_version answer reports.
--
-- No row (pgx.ErrNoRows) means the site is not visible to the caller.
--
-- The values an applied = false row carries are the ones this statement's
-- snapshot saw. A caller that holds the tenant's policy lock, as every mode
-- writer does, has no concurrent mode write to miss.
--
-- set_by is the signed-in person recording the choice, or NULL for a
-- lowering made without one (source 'tightened'). step_up is NULL except for
-- Full auto. sites_ai_mode_guard refuses a combination a caller may not
-- write.
WITH changed AS (
    UPDATE sites AS s SET
        ai_mode         = sqlc.arg(mode)::text,
        ai_mode_source  = sqlc.arg(source)::text,
        ai_mode_set_by  = sqlc.narg(set_by)::uuid,
        ai_mode_step_up = CASE
            WHEN (s.ai_mode, s.ai_mode_source, s.ai_mode_set_by)
                 IS DISTINCT FROM (sqlc.arg(mode)::text, sqlc.arg(source)::text, sqlc.narg(set_by)::uuid)
            THEN sqlc.narg(step_up)::text
            ELSE s.ai_mode_step_up
        END,
        ai_mode_set_at = CASE
            WHEN (s.ai_mode, s.ai_mode_source, s.ai_mode_set_by)
                 IS DISTINCT FROM (sqlc.arg(mode)::text, sqlc.arg(source)::text, sqlc.narg(set_by)::uuid)
            THEN now()
            ELSE s.ai_mode_set_at
        END,
        ai_mode_version = s.ai_mode_version + CASE
            WHEN (s.ai_mode, s.ai_mode_source, s.ai_mode_set_by)
                 IS DISTINCT FROM (sqlc.arg(mode)::text, sqlc.arg(source)::text, sqlc.narg(set_by)::uuid)
            THEN 1
            ELSE 0
        END,
        updated_at = now()
    WHERE s.tenant_id = @tenant_id
      AND s.id = @site_id
      AND s.ai_mode_version = sqlc.arg(expected_version)::bigint
    RETURNING s.id, s.ai_mode, s.ai_mode_source, s.ai_mode_set_by, s.ai_mode_set_at,
              s.ai_mode_version, s.ai_mode_step_up
)
SELECT true AS applied,
       c.id, c.ai_mode, c.ai_mode_source, c.ai_mode_set_by, c.ai_mode_set_at,
       c.ai_mode_version, c.ai_mode_step_up
FROM changed c
UNION ALL
SELECT false AS applied,
       s.id, s.ai_mode, s.ai_mode_source, s.ai_mode_set_by, s.ai_mode_set_at,
       s.ai_mode_version, s.ai_mode_step_up
FROM sites s
WHERE s.tenant_id = @tenant_id
  AND s.id = @site_id
  AND NOT EXISTS (SELECT 1 FROM changed);

-- name: ApplySiteAIModeEnableDefault :one
-- The default a site gets when a person turns AI editing on: Auto for AI
-- drafts, source 'enable_default', that person as the setter. Run it in the
-- same transaction as MarkSiteContentEditingEnabled, with that person's
-- app.user_id. It moves only a site whose mode no one has set ('unset') and
-- whose AI editing is on, so turning AI editing on again never resets a
-- choice a person made, the launch default or a tightening. No row
-- (pgx.ErrNoRows) means the site already carries a mode and nothing changed,
-- or the site is not visible to the caller.
UPDATE sites SET
    ai_mode         = 'ai_drafts',
    ai_mode_source  = 'enable_default',
    ai_mode_set_by  = sqlc.arg(enabled_by)::uuid,
    ai_mode_set_at  = now(),
    ai_mode_version = ai_mode_version + 1,
    updated_at      = now()
WHERE tenant_id = @tenant_id
  AND id = @site_id
  AND ai_mode_source = 'unset'
  AND content_editing_enabled_at IS NOT NULL
RETURNING id, ai_mode, ai_mode_source, ai_mode_set_by, ai_mode_set_at, ai_mode_version;

-- name: ClaimSiteAILaunchNotices :many
-- One tenant's sites still on the launch default whose owners have not been
-- told, claimed by stamping ai_mode_launch_emailed_at. Every row this
-- statement claims carries the same stamp (now() is the transaction's start
-- time); pass it to ReleaseSiteAILaunchNotices when the send fails, so the
-- next run claims the sites again. A site whose mode a person has since
-- chosen is no longer on the launch default and is not claimed. The stamp is
-- not one of the columns sites_ai_mode_guard watches.
UPDATE sites SET
    ai_mode_launch_emailed_at = now()
WHERE tenant_id = @tenant_id
  AND ai_mode_source = 'launch_default'
  AND ai_mode_launch_emailed_at IS NULL
RETURNING id, name, url, ai_mode_launch_emailed_at;

-- name: ReleaseSiteAILaunchNotices :execrows
-- Undoes one claim after a failed send. It clears only the stamp that claim
-- wrote (claimed_at is the stamp ClaimSiteAILaunchNotices returned), so it
-- never clears a later claim or a notice that was sent.
UPDATE sites SET
    ai_mode_launch_emailed_at = NULL
WHERE tenant_id = @tenant_id
  AND id = ANY(@site_ids::uuid[])
  AND ai_mode_launch_emailed_at = sqlc.arg(claimed_at)::timestamptz;

-- ---------------------------------------------------------------------------
-- The connection's switch and usage
-- ---------------------------------------------------------------------------

-- name: GetAIConnectionAuto :one
-- The connection's switch: 'site_setting' (runs a change where the site's
-- mode allows it) or 'never', who allowed it and when, and whether that
-- account still exists. created_by_user_id is NULL for a connection minted
-- with an API key. No row (pgx.ErrNoRows) when there is no such connection in
-- the tenant, or the transaction is site-scoped.
SELECT g.id,
       g.status,
       g.expires_at,
       g.created_by_user_id,
       g.ai_auto,
       g.ai_auto_set_by,
       g.ai_auto_set_at,
       u.name AS auto_set_by_name,
       (g.ai_auto_set_by IS NOT NULL AND u.id IS NULL)::boolean AS auto_set_by_account_deleted
FROM mcp_grants g
LEFT JOIN users u ON u.id = g.ai_auto_set_by
WHERE g.tenant_id = @tenant_id
  AND g.id = @grant_id;

-- name: SetAIConnectionAuto :one
-- Sets the connection's switch. 'site_setting' records set_by, the signed-in
-- person allowing it, even when the switch already reads 'site_setting'
-- (that is how a connection is allowed again after the person who allowed it
-- lost the access it needs). 'never' records no person. Only an active,
-- unexpired connection is written. No row (pgx.ErrNoRows) means there is no
-- such connection, or it is revoked or expired; GetAIConnectionAuto tells
-- the two apart.
UPDATE mcp_grants SET
    ai_auto        = sqlc.arg(ai_auto)::text,
    ai_auto_set_by = CASE WHEN sqlc.arg(ai_auto)::text = 'site_setting'
                          THEN sqlc.narg(set_by)::uuid
                     END,
    ai_auto_set_at = now()
WHERE tenant_id = @tenant_id
  AND id = @grant_id
  AND status = 'active'
  AND expires_at > now()
RETURNING id, ai_auto, ai_auto_set_by, ai_auto_set_at;

-- name: CountAIConnectionPolicyApprovals :one
-- What one connection ran by a site's setting in the window: approvals with
-- approval_source 'policy' whose class is in classes, and on how many sites.
-- Site changes only; cache clears are counted from their own table.
SELECT count(*)::bigint                AS changes,
       count(DISTINCT site_id)::bigint AS sites
FROM assistant_ability_requests
WHERE tenant_id = @tenant_id
  AND proposed_by_grant_id = @grant_id
  AND approval_source = 'policy'
  AND change_class = ANY(@classes::text[])
  AND decided_at > now() - (sqlc.arg(window_seconds)::int * interval '1 second');

-- ---------------------------------------------------------------------------
-- AI activity
-- ---------------------------------------------------------------------------

-- name: ListAIActivityPage :many
-- One page of the activity feed: every approved request from both request
-- tables, newest first, keyset on (created_at, id). It returns which table
-- and which id; ListAbilityRequestsByIDs and
-- ListAssistantCachePurgeRequestsByIDs read the rows, in the same
-- transaction.
--
-- Approved means the request entered the approved states: for site changes
-- approved, dispatched, done, failed, not_sent or outcome_unknown; for cache
-- clears approved_undispatched or dispatched. Waiting, declined, withdrawn
-- and expired requests are not listed.
--
-- filter is one of all, ran_automatically (approved by a setting or a
-- session), approved_by_person, failed_or_unknown or undone; anything else
-- lists nothing. site_id and grant_id narrow the page when set. The first
-- page passes NULL cursor_created_at and cursor_id; a later page passes the
-- created_at and id of the previous page's last row.
SELECT a.kind::text             AS kind,
       a.id::uuid               AS id,
       a.created_at::timestamptz AS created_at
FROM (
    SELECT 'ability_request'::text AS kind, r.id, r.created_at
    FROM assistant_ability_requests r
    WHERE r.tenant_id = @tenant_id
      AND r.state IN ('approved', 'dispatched', 'done', 'failed', 'not_sent', 'outcome_unknown')
      AND (sqlc.narg(site_id)::uuid IS NULL OR r.site_id = sqlc.narg(site_id)::uuid)
      AND (sqlc.narg(grant_id)::uuid IS NULL OR r.proposed_by_grant_id = sqlc.narg(grant_id)::uuid)
      AND CASE sqlc.arg(filter)::text
              WHEN 'all'                THEN true
              WHEN 'ran_automatically'  THEN r.approval_source <> 'person'
              WHEN 'approved_by_person' THEN r.approval_source = 'person'
              WHEN 'failed_or_unknown'  THEN r.state IN ('failed', 'outcome_unknown')
              WHEN 'undone'             THEN r.undo_state IS NOT DISTINCT FROM 'undone'
              ELSE false
          END
      AND (sqlc.narg(cursor_created_at)::timestamptz IS NULL
           OR (r.created_at, r.id) < (sqlc.narg(cursor_created_at)::timestamptz, sqlc.narg(cursor_id)::uuid))
    UNION ALL
    SELECT 'cache_purge_request'::text AS kind, p.id, p.created_at
    FROM assistant_cache_purge_requests p
    WHERE p.tenant_id = @tenant_id
      AND p.state IN ('approved_undispatched', 'dispatched')
      AND (sqlc.narg(site_id)::uuid IS NULL OR p.site_id = sqlc.narg(site_id)::uuid)
      AND (sqlc.narg(grant_id)::uuid IS NULL OR p.proposed_by_grant_id = sqlc.narg(grant_id)::uuid)
      AND CASE sqlc.arg(filter)::text
              WHEN 'all'                THEN true
              WHEN 'ran_automatically'  THEN p.approval_source <> 'person'
              WHEN 'approved_by_person' THEN p.approval_source = 'person'
              WHEN 'failed_or_unknown'  THEN p.outcome IS NOT NULL
                                             AND p.outcome IN ('site_reported_failure', 'agent_failed', 'outcome_unknown')
              ELSE false
          END
      AND (sqlc.narg(cursor_created_at)::timestamptz IS NULL
           OR (p.created_at, p.id) < (sqlc.narg(cursor_created_at)::timestamptz, sqlc.narg(cursor_id)::uuid))
) a
ORDER BY a.created_at DESC, a.id DESC
LIMIT sqlc.arg(row_limit)::int;

-- name: ListAbilityRequestsByIDs :many
-- The site-change rows of one activity page, newest first.
SELECT *
FROM assistant_ability_requests
WHERE tenant_id = @tenant_id
  AND id = ANY(@ids::uuid[])
ORDER BY created_at DESC, id DESC;

-- name: ListAssistantCachePurgeRequestsByIDs :many
-- The cache-clear rows of one activity page, newest first.
SELECT *
FROM assistant_cache_purge_requests
WHERE tenant_id = @tenant_id
  AND id = ANY(@ids::uuid[])
ORDER BY created_at DESC, id DESC;

-- name: ListUserNamesByIDs :many
-- Display names for the setters and approvers recorded on rows the caller
-- has already read in its own tenant. An id with no row is an account that
-- no longer exists. Pass only ids read from the caller's tenant.
SELECT id, name
FROM users
WHERE id = ANY(@ids::uuid[]);
