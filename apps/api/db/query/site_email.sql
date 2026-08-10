-- M59 — Per-site Email / SMTP Management queries (Phase 1: config CRUD).
--
-- All operator reads/writes run under pool.InTenantTx (app.tenant_id GUC).
-- The repo NEVER returns provider_secret_encrypted to callers — only a
-- secret_set boolean is surfaced (mirrors perf repo CDN credentials pattern).
-- updated_at is set via now() in the query (no trigger).

-- ---------------------------------------------------------------------------
-- site_email_config
-- ---------------------------------------------------------------------------

-- name: GetSiteEmailConfig :one
-- Returns the per-site config row. Caller resolves org-wide inheritance in the
-- service layer (call GetOrgEmailConfig when this returns pgx.ErrNoRows).
SELECT *
FROM site_email_config
WHERE tenant_id = @tenant_id
  AND site_id   = @site_id;

-- name: GetOrgEmailConfig :one
-- Returns the org-wide default config row (site_id IS NULL) for a tenant.
SELECT *
FROM site_email_config
WHERE tenant_id = @tenant_id
  AND site_id IS NULL;

-- name: UpsertSiteEmailConfig :one
-- Insert-or-update a per-site config row. provider_secret_encrypted uses a
-- nil-sentinel: when @set_secret is false the existing ciphertext is preserved,
-- so editing non-secret fields without re-entering the password keeps the stored
-- secret intact (mirrors smtp_settings.sql UpsertSMTPSettings pattern exactly).
INSERT INTO site_email_config (
    tenant_id, site_id,
    provider,
    from_address, from_name, force_from_email, force_from_name, return_path,
    config,
    provider_secret_encrypted,
    mappings, default_connection, fallback_connection,
    log_emails, store_body, retention_days,
    updated_at
) VALUES (
    @tenant_id, @site_id,
    @provider,
    @from_address, @from_name, @force_from_email, @force_from_name, @return_path,
    @config,
    -- ::bytea so Postgres can infer the param type (both CASE branches are
    -- otherwise untyped: a bare param + NULL -> "could not determine data type").
    CASE WHEN @set_secret::boolean THEN @provider_secret_encrypted::bytea ELSE NULL END,
    @mappings, @default_connection, @fallback_connection,
    @log_emails, @store_body, @retention_days,
    now()
)
ON CONFLICT ON CONSTRAINT site_email_config_pkey DO NOTHING
RETURNING *;

-- name: UpsertSiteEmailConfigByTenantSite :one
-- Upsert variant that matches on (tenant_id, site_id) instead of PK — used for
-- the standard per-site PUT where no id is known by the caller.
-- For the org-wide default (site_id IS NULL) use UpsertOrgEmailConfig instead.
INSERT INTO site_email_config (
    tenant_id, site_id,
    provider,
    from_address, from_name, force_from_email, force_from_name, return_path,
    config,
    provider_secret_encrypted,
    mappings, default_connection, fallback_connection,
    log_emails, store_body, retention_days,
    updated_at
) VALUES (
    @tenant_id, @site_id,
    @provider,
    @from_address, @from_name, @force_from_email, @force_from_name, @return_path,
    @config,
    CASE WHEN @set_secret::boolean THEN @provider_secret_encrypted::bytea ELSE NULL END,
    @mappings, @default_connection, @fallback_connection,
    @log_emails, @store_body, @retention_days,
    now()
)
ON CONFLICT (tenant_id, site_id) WHERE site_id IS NOT NULL
DO UPDATE SET
    provider                  = EXCLUDED.provider,
    from_address              = EXCLUDED.from_address,
    from_name                 = EXCLUDED.from_name,
    force_from_email          = EXCLUDED.force_from_email,
    force_from_name           = EXCLUDED.force_from_name,
    return_path               = EXCLUDED.return_path,
    config                    = EXCLUDED.config,
    provider_secret_encrypted = CASE WHEN @set_secret::boolean
                                    THEN EXCLUDED.provider_secret_encrypted
                                    ELSE site_email_config.provider_secret_encrypted END,
    mappings                  = EXCLUDED.mappings,
    default_connection        = EXCLUDED.default_connection,
    fallback_connection       = EXCLUDED.fallback_connection,
    log_emails                = EXCLUDED.log_emails,
    store_body                = EXCLUDED.store_body,
    retention_days            = EXCLUDED.retention_days,
    updated_at                = now()
RETURNING *;

-- name: UpsertOrgEmailConfig :one
-- Upsert the org-wide default row (site_id IS NULL).
INSERT INTO site_email_config (
    tenant_id, site_id,
    provider,
    from_address, from_name, force_from_email, force_from_name, return_path,
    config,
    provider_secret_encrypted,
    mappings, default_connection, fallback_connection,
    log_emails, store_body, retention_days,
    updated_at
) VALUES (
    @tenant_id, NULL,
    @provider,
    @from_address, @from_name, @force_from_email, @force_from_name, @return_path,
    @config,
    CASE WHEN @set_secret::boolean THEN @provider_secret_encrypted::bytea ELSE NULL END,
    @mappings, @default_connection, @fallback_connection,
    @log_emails, @store_body, @retention_days,
    now()
)
ON CONFLICT (tenant_id) WHERE site_id IS NULL
DO UPDATE SET
    provider                  = EXCLUDED.provider,
    from_address              = EXCLUDED.from_address,
    from_name                 = EXCLUDED.from_name,
    force_from_email          = EXCLUDED.force_from_email,
    force_from_name           = EXCLUDED.force_from_name,
    return_path               = EXCLUDED.return_path,
    config                    = EXCLUDED.config,
    provider_secret_encrypted = CASE WHEN @set_secret::boolean
                                    THEN EXCLUDED.provider_secret_encrypted
                                    ELSE site_email_config.provider_secret_encrypted END,
    mappings                  = EXCLUDED.mappings,
    default_connection        = EXCLUDED.default_connection,
    fallback_connection       = EXCLUDED.fallback_connection,
    log_emails                = EXCLUDED.log_emails,
    store_body                = EXCLUDED.store_body,
    retention_days            = EXCLUDED.retention_days,
    updated_at                = now()
RETURNING *;

-- name: ListSiteEmailConfigs :many
-- Lists all per-site config rows for a tenant (dashboard overview).
-- Excludes the org-wide default row (site_id IS NULL).
SELECT *
FROM site_email_config
WHERE tenant_id = @tenant_id
  AND site_id IS NOT NULL
ORDER BY created_at DESC, id DESC
LIMIT @row_limit OFFSET @row_offset;

-- name: GetEmailConfigByRouteTokenHash :one
-- m61: resolve a config row by its webhook_route_token_hash for the public
-- webhook dispatcher.  Runs under InAgentTx (webhook path, no tenant GUC).
-- Returns the full row so the handler can load and decrypt the per-row signing
-- key and verify the provider signature with the right key.
SELECT *
FROM site_email_config
WHERE webhook_route_token_hash = @token_hash::bytea;

-- name: SetEmailConfigWebhookFields :one
-- m61: write/rotate webhook security columns on a config row.
-- Use set_signing_key flag (nil-sentinel) to preserve the existing encrypted key
-- when rotating only the token or ARNs.
-- Runs under InTenantTx (operator PUT path).
UPDATE site_email_config
SET webhook_route_token_hash = @token_hash::bytea,
    ses_topic_arns            = @ses_topic_arns::text[],
    webhook_signing_key_enc   = CASE WHEN @set_signing_key::boolean
                                    THEN @signing_key_enc::bytea
                                    ELSE webhook_signing_key_enc END,
    updated_at                = now()
WHERE tenant_id = @tenant_id
  AND id        = @id
RETURNING *;

-- ---------------------------------------------------------------------------
-- site_email_log  (Phase 3 — ingest + viewer)
-- ---------------------------------------------------------------------------

-- name: IngestEmailLogEntry :one
-- Idempotent upsert of one agent-pushed log entry keyed on
-- (tenant_id, site_id, agent_seq). Status/response/error/resent_count may
-- change on re-push (e.g. an asynchronous provider callback updates the
-- status after initial delivery). Body is only stored when body_stored=true.
-- m62: connection_key + attachments added (additive; old agents send '' / '[]').
INSERT INTO site_email_log (
    tenant_id, site_id, agent_seq,
    message_id, to_addresses, from_address, subject, provider,
    status, response, error, retries, resent_count,
    body_stored, body,
    connection_key, attachments,
    created_at, updated_at
) VALUES (
    @tenant_id, @site_id, @agent_seq,
    @message_id, @to_addresses, @from_address, @subject, @provider,
    @status, @response, @error, @retries, @resent_count,
    @body_stored,
    -- Store body only when body_stored flag is set; otherwise NULL.
    CASE WHEN @body_stored::boolean THEN @body::text ELSE NULL END,
    @connection_key, @attachments,
    @created_at, now()
)
ON CONFLICT (tenant_id, site_id, agent_seq)
    WHERE agent_seq IS NOT NULL
DO UPDATE SET
    status         = EXCLUDED.status,
    response       = EXCLUDED.response,
    error          = EXCLUDED.error,
    resent_count   = EXCLUDED.resent_count,
    body_stored    = EXCLUDED.body_stored,
    body           = CASE WHEN EXCLUDED.body_stored THEN EXCLUDED.body ELSE site_email_log.body END,
    connection_key = EXCLUDED.connection_key,
    attachments    = EXCLUDED.attachments,
    updated_at     = now()
RETURNING *;

-- name: ListSiteEmailLog :many
-- Keyset-paginated list for a single site. Ordered created_at DESC, id DESC.
-- Composite (created_at, id) cursor predicate avoids skipping co-timestamped
-- rows (batch inserts share created_at — see wpmgr-keyset-cursor-composite).
-- Body is intentionally excluded from the list — callers use GetEmailLog for detail.
-- @cursor_ts and @cursor_id are the last row's created_at/id; pass far-future
-- values (e.g. now()+100yr, max-uuid) to get the first page.
-- Repo passes sentinel epoch-start/@range_from and far-future/@range_to when no
-- date filter is requested; @filter_status '' skips the status filter;
-- @search_q '' skips the text search.
SELECT
    id, tenant_id, site_id, agent_seq,
    message_id, to_addresses, from_address, subject, provider,
    status, response, error, retries, resent_count,
    body_stored,
    connection_key,
    jsonb_array_length(attachments) AS attachment_count,
    created_at, updated_at
FROM site_email_log
WHERE tenant_id   = @tenant_id
  AND site_id     = @site_id
  AND (created_at, id) < (@cursor_ts::timestamptz, @cursor_id::uuid)
  AND (@filter_status::text = '' OR status = @filter_status::text)
  AND created_at >= @range_from
  AND created_at <= @range_to
  AND (@search_q::text = '' OR (
        subject ILIKE '%' || @search_q::text || '%'
        OR from_address ILIKE '%' || @search_q::text || '%'
        OR to_addresses::text ILIKE '%' || @search_q::text || '%'
  ))
ORDER BY created_at DESC, id DESC
LIMIT @row_limit;

-- name: GetEmailLog :one
-- Fetch a single email log entry by id (operator detail view, includes body).
SELECT *
FROM site_email_log
WHERE tenant_id = @tenant_id
  AND site_id   = @site_id
  AND id        = @id;

-- name: GetEmailLogPrev :one
-- Returns the id of the next-older row for the Prev button in detail navigation.
SELECT id
FROM site_email_log
WHERE tenant_id = @tenant_id
  AND site_id   = @site_id
  AND (created_at, id) < (@this_ts::timestamptz, @this_id::uuid)
ORDER BY created_at DESC, id DESC
LIMIT 1;

-- name: GetEmailLogNext :one
-- Returns the id of the next-newer row for the Next button in detail navigation.
SELECT id
FROM site_email_log
WHERE tenant_id = @tenant_id
  AND site_id   = @site_id
  AND (created_at, id) > (@this_ts::timestamptz, @this_id::uuid)
ORDER BY created_at ASC, id ASC
LIMIT 1;

-- name: ListFleetEmailLog :many
-- Cross-site keyset-paginated list for a tenant (fleet/agency dashboard).
-- Same composite cursor as ListSiteEmailLog. Body excluded from list.
-- Repo passes sentinel epoch-start/@range_from and far-future/@range_to when no
-- date filter is requested.
SELECT
    id, tenant_id, site_id, agent_seq,
    message_id, to_addresses, from_address, subject, provider,
    status, response, error, retries, resent_count,
    body_stored,
    connection_key,
    jsonb_array_length(attachments) AS attachment_count,
    created_at, updated_at
FROM site_email_log
WHERE tenant_id   = @tenant_id
  AND (created_at, id) < (@cursor_ts::timestamptz, @cursor_id::uuid)
  AND (@filter_status::text = '' OR status = @filter_status::text)
  AND created_at >= @range_from
  AND created_at <= @range_to
  AND (@search_q::text = '' OR (
        subject ILIKE '%' || @search_q::text || '%'
        OR from_address ILIKE '%' || @search_q::text || '%'
  ))
ORDER BY created_at DESC, id DESC
LIMIT @row_limit;

-- name: GetEmailStats :one
-- Per-site summary: total sent/failed/bounced/complained counts over [range_from, range_to].
-- Repo always provides explicit bounds; use epoch-start + far-future as the
-- open-ended defaults so no NULL handling is needed in SQL.
SELECT
    COUNT(*)                                          AS total,
    COUNT(*) FILTER (WHERE status = 'sent')           AS sent_count,
    COUNT(*) FILTER (WHERE status = 'failed')         AS failed_count,
    COUNT(*) FILTER (WHERE status = 'bounced')        AS bounced_count,
    COUNT(*) FILTER (WHERE status = 'complained')     AS complained_count,
    COUNT(DISTINCT provider)                          AS provider_count
FROM site_email_log
WHERE tenant_id   = @tenant_id
  AND site_id     = @site_id
  AND created_at >= @range_from
  AND created_at <= @range_to;

-- name: GetEmailStatsByDay :many
-- Per-site daily time-series for the stats dashboard.
SELECT
    date_trunc('day', created_at AT TIME ZONE 'UTC')::timestamptz AS day,
    COUNT(*)                                          AS total,
    COUNT(*) FILTER (WHERE status = 'sent')           AS sent_count,
    COUNT(*) FILTER (WHERE status = 'failed')         AS failed_count,
    COUNT(*) FILTER (WHERE status = 'bounced')        AS bounced_count,
    COUNT(*) FILTER (WHERE status = 'complained')     AS complained_count
FROM site_email_log
WHERE tenant_id   = @tenant_id
  AND site_id     = @site_id
  AND created_at >= @range_from
  AND created_at <= @range_to
GROUP BY 1
ORDER BY 1 ASC;

-- name: GetEmailStatsByProvider :many
-- Per-site provider breakdown for the stats dashboard.
SELECT
    provider,
    COUNT(*)                                          AS total,
    COUNT(*) FILTER (WHERE status = 'sent')           AS sent_count,
    COUNT(*) FILTER (WHERE status = 'failed')         AS failed_count
FROM site_email_log
WHERE tenant_id   = @tenant_id
  AND site_id     = @site_id
  AND created_at >= @range_from
  AND created_at <= @range_to
GROUP BY provider
ORDER BY total DESC;

-- name: GetFleetEmailStats :one
-- Tenant-wide summary (fleet dashboard).
SELECT
    COUNT(*)                                          AS total,
    COUNT(*) FILTER (WHERE status = 'sent')           AS sent_count,
    COUNT(*) FILTER (WHERE status = 'failed')         AS failed_count,
    COUNT(*) FILTER (WHERE status = 'bounced')        AS bounced_count,
    COUNT(*) FILTER (WHERE status = 'complained')     AS complained_count,
    COUNT(DISTINCT provider)                          AS provider_count,
    COUNT(DISTINCT site_id)                           AS site_count
FROM site_email_log
WHERE tenant_id   = @tenant_id
  AND created_at >= @range_from
  AND created_at <= @range_to;

-- name: GetFleetEmailStatsByDay :many
-- Fleet daily time-series (tenant-wide).
SELECT
    date_trunc('day', created_at AT TIME ZONE 'UTC')::timestamptz AS day,
    COUNT(*)                                          AS total,
    COUNT(*) FILTER (WHERE status = 'sent')           AS sent_count,
    COUNT(*) FILTER (WHERE status = 'failed')         AS failed_count,
    COUNT(*) FILTER (WHERE status = 'bounced')        AS bounced_count,
    COUNT(*) FILTER (WHERE status = 'complained')     AS complained_count
FROM site_email_log
WHERE tenant_id   = @tenant_id
  AND created_at >= @range_from
  AND created_at <= @range_to
GROUP BY 1
ORDER BY 1 ASC;

-- name: DeleteEmailLogsOlderThan :execrows
-- Retention pruner: deletes rows older than the supplied cutoff timestamp.
-- Batched via LIMIT so a single invocation does not lock the table for too long.
-- The worker calls this in a loop until 0 rows are deleted.
-- Cross-tenant (InAgentTx): the agent policy allows full-table access.
DELETE FROM site_email_log
WHERE id IN (
    SELECT l.id
    FROM site_email_log l
    JOIN site_email_config c
      ON c.tenant_id = l.tenant_id
     AND c.site_id   = l.site_id
    WHERE l.created_at < @cutoff_ts::timestamptz
       OR (
            -- Per-site retention: delete when older than the config's retention_days.
            -- Falls back to 14 days when no per-site config exists (subquery returns NULL).
            l.created_at < now() - (COALESCE(
                (SELECT retention_days FROM site_email_config
                 WHERE tenant_id = l.tenant_id AND site_id = l.site_id
                 LIMIT 1), 14
            ) * INTERVAL '1 day')
          )
    LIMIT @batch_size::bigint
);

-- ---------------------------------------------------------------------------
-- email_suppression  (Phase 4a — webhooks + suppression list)
-- ---------------------------------------------------------------------------

-- name: UpsertEmailSuppression :one
-- Insert-or-ignore for one email suppression entry keyed on
-- (tenant_id, COALESCE(site_id,'00000000-0000-0000-0000-000000000000'), email_hash).
-- The two partial unique indexes enforce the uniqueness separately for per-site
-- and fleet-wide (site_id IS NULL) rows. ON CONFLICT DO UPDATE refreshes the
-- mutable fields so a duplicate bounce from a different provider still records
-- the most recent provider + event_at.
-- Runs under InAgentTx (webhook path) or InTenantTx (operator manual-add).
INSERT INTO email_suppression (
    tenant_id, site_id,
    email_hash, email,
    reason, provider, event_at, source_message_id
) VALUES (
    @tenant_id, @site_id,
    @email_hash, @email,
    @reason, @provider, @event_at, @source_message_id
)
ON CONFLICT (tenant_id, site_id, email_hash)
    WHERE site_id IS NOT NULL
DO UPDATE SET
    reason            = EXCLUDED.reason,
    provider          = EXCLUDED.provider,
    event_at          = EXCLUDED.event_at,
    source_message_id = EXCLUDED.source_message_id
RETURNING *;

-- name: UpsertEmailSuppressionFleet :one
-- Fleet-wide (site_id IS NULL) variant — separate query because the conflict
-- target differs (the fleet-wide partial index has no site_id column).
INSERT INTO email_suppression (
    tenant_id, site_id,
    email_hash, email,
    reason, provider, event_at, source_message_id
) VALUES (
    @tenant_id, NULL,
    @email_hash, @email,
    @reason, @provider, @event_at, @source_message_id
)
ON CONFLICT (tenant_id, email_hash)
    WHERE site_id IS NULL
DO UPDATE SET
    reason            = EXCLUDED.reason,
    provider          = EXCLUDED.provider,
    event_at          = EXCLUDED.event_at,
    source_message_id = EXCLUDED.source_message_id
RETURNING *;

-- name: GetEmailSuppression :one
-- Fetch a single suppression entry by id (operator detail).
SELECT *
FROM email_suppression
WHERE id        = @id
  AND tenant_id = @tenant_id;

-- name: IsSuppressed :one
-- Returns true when the given email_hash is suppressed for this tenant at either
-- the fleet level (site_id IS NULL) or the specific site.
-- Runs under InAgentTx (pre-send check from the delta-fetch query) or InTenantTx.
SELECT EXISTS (
    SELECT 1 FROM email_suppression
    WHERE tenant_id  = @tenant_id
      AND email_hash = @email_hash
      AND (site_id IS NULL OR site_id = @site_id)
) AS suppressed;

-- name: ListEmailSuppression :many
-- Keyset-paginated list of suppression entries for a site.
-- Pass site_id = uuid.Nil to list fleet-wide entries only (site_id IS NULL).
-- Pass a real site_id to list that site's entries (plus fleet-wide entries).
-- @include_fleet when true also returns fleet-wide (site_id IS NULL) rows.
-- Ordered created_at DESC, id DESC (composite keyset; see wpmgr-keyset-cursor-composite).
SELECT *
FROM email_suppression
WHERE tenant_id = @tenant_id
  AND (
        (@include_fleet::boolean AND site_id IS NULL)
        OR site_id = @site_id
  )
  AND (created_at, id) < (@cursor_ts::timestamptz, @cursor_id::uuid)
  AND (@filter_reason::text = '' OR reason = @filter_reason::text)
ORDER BY created_at DESC, id DESC
LIMIT @row_limit;

-- name: ListFleetEmailSuppression :many
-- Fleet-scope list (no site filter). Returns all suppression entries for the
-- tenant including both fleet-wide and per-site entries.
-- Ordered created_at DESC, id DESC.
SELECT *
FROM email_suppression
WHERE tenant_id = @tenant_id
  AND (created_at, id) < (@cursor_ts::timestamptz, @cursor_id::uuid)
  AND (@filter_reason::text = '' OR reason = @filter_reason::text)
ORDER BY created_at DESC, id DESC
LIMIT @row_limit;

-- name: DeleteEmailSuppression :execrows
-- Operator delete (un-suppress). Runs under scopedTenantTx.
--
-- :execrows, not :exec (GH #380). Under the m112 site-scope policies a
-- site-scoped collaborator can SEE a fleet-wide entry (site_id IS NULL) but not
-- delete one, and Postgres expresses that refusal as zero rows affected rather
-- than as an error. With :exec the caller could not tell the two apart and the
-- API answered 204 for a delete that removed nothing. The repo needs the count
-- to tell the caller the truth.
DELETE FROM email_suppression
WHERE id        = @id
  AND tenant_id = @tenant_id;

-- name: ListEmailSuppressionDeltas :many
-- Agent suppression-fetch: returns entries created after @since_id for
-- the given tenant + site (including fleet-wide site_id IS NULL rows).
-- Keyset cursor on (created_at, id) ASC (agent polls for new entries since last fetch).
-- @since_ts / @since_id: last seen row; pass epoch-start + uuid-zero for the first fetch.
SELECT *
FROM email_suppression
WHERE tenant_id = @tenant_id
  AND (site_id IS NULL OR site_id = @site_id)
  AND (created_at, id) > (@since_ts::timestamptz, @since_id::uuid)
ORDER BY created_at ASC, id ASC
LIMIT @row_limit;

-- ---------------------------------------------------------------------------
-- email_webhook_events  (Phase 4a — dedup / audit)
-- ---------------------------------------------------------------------------

-- name: InsertWebhookEventDedup :one
-- Inserts a dedup sentinel for a provider event. Returns (row, inserted).
-- ON CONFLICT DO NOTHING: if 0 rows are affected the event is a duplicate and
-- must be dropped. Runs under InAgentTx (webhook path).
-- m61: stores email_hash (SHA-256) instead of plaintext email (SHOULD-FIX #2).
INSERT INTO email_webhook_events (
    provider_event_id, provider,
    tenant_id, site_id,
    email_hash, event_type, suppression_id
) VALUES (
    @provider_event_id, @provider,
    @tenant_id, @site_id,
    @email_hash, @event_type, @suppression_id
)
ON CONFLICT (provider, provider_event_id) DO NOTHING
RETURNING *;

-- name: PruneWebhookEventDedup :execrows
-- Prune dedup rows older than the given cutoff (run by the GC worker).
-- Cross-tenant / InAgentTx.
DELETE FROM email_webhook_events
WHERE created_at < @cutoff_ts::timestamptz;

-- ---------------------------------------------------------------------------
-- site_email_log  Phase 4a additions
-- ---------------------------------------------------------------------------

-- name: MarkEmailLogBounced :exec
-- Update a log entry's status to 'bounced' or 'complained' when a webhook
-- event arrives. Matched by message_id + tenant_id + site_id.
-- m61 SHOULD-FIX #3: site_id added so a colliding message_id from a different
-- site in the same tenant cannot flip another site's row.
-- Runs under InAgentTx; tenant_id provided for defense-in-depth in addition to
-- RLS; site_id added to narrow the update scope.
UPDATE site_email_log
SET status     = @status,
    updated_at = now()
WHERE message_id = @message_id
  AND tenant_id  = @tenant_id
  AND site_id    = @site_id;

-- name: IncrEmailLogResentCount :exec
-- Increment resent_count on a specific log entry. Runs under InTenantTx.
UPDATE site_email_log
SET resent_count = resent_count + 1,
    updated_at   = now()
WHERE id        = @id
  AND tenant_id = @tenant_id
  AND site_id   = @site_id;

-- name: GetEmailLogBodyStored :one
-- Fetch only the body_stored flag + id for a resend gate check.
SELECT id, body_stored
FROM site_email_log
WHERE id        = @id
  AND tenant_id = @tenant_id
  AND site_id   = @site_id;

-- name: DeleteEmailLogsBulk :execrows
-- Bulk delete of log entries by id list. Must be tenant+site scoped (InTenantTx).
-- The ids array is the caller-supplied list; RLS provides the second line of defence.
DELETE FROM site_email_log
WHERE tenant_id = @tenant_id
  AND site_id   = @site_id
  AND id = ANY(@ids::uuid[]);

-- ---------------------------------------------------------------------------
-- site_email_connection  (m62 — multi-connection + failover)
-- ---------------------------------------------------------------------------

-- name: ListEmailConnections :many
-- List all named connections for a config row. Ordered created_at ASC, id ASC
-- (stable insertion order for the UI). Runs under InTenantTx.
SELECT *
FROM site_email_connection
WHERE config_id = @config_id
  AND tenant_id = @tenant_id
ORDER BY created_at ASC, id ASC;

-- name: GetEmailConnection :one
-- Fetch one connection by (config_id, connection_key). Runs under InTenantTx.
SELECT *
FROM site_email_connection
WHERE config_id      = @config_id
  AND connection_key = @connection_key
  AND tenant_id      = @tenant_id;

-- name: UpsertEmailConnection :one
-- Insert-or-update a named connection. secret uses the nil-sentinel pattern:
-- when @set_secret is false the existing ciphertext is preserved.
-- updated_at is set via now() (no trigger). Runs under InTenantTx.
INSERT INTO site_email_connection (
    tenant_id, config_id, connection_key,
    provider, from_address, from_name,
    config,
    provider_secret_encrypted,
    updated_at
) VALUES (
    @tenant_id, @config_id, @connection_key,
    @provider, @from_address, @from_name,
    @config,
    CASE WHEN @set_secret::boolean THEN @provider_secret_encrypted::bytea ELSE NULL END,
    now()
)
ON CONFLICT (config_id, connection_key)
DO UPDATE SET
    provider                  = EXCLUDED.provider,
    from_address              = EXCLUDED.from_address,
    from_name                 = EXCLUDED.from_name,
    config                    = EXCLUDED.config,
    provider_secret_encrypted = CASE WHEN @set_secret::boolean
                                    THEN EXCLUDED.provider_secret_encrypted
                                    ELSE site_email_connection.provider_secret_encrypted END,
    updated_at                = now()
RETURNING *;

-- name: DeleteEmailConnection :exec
-- Delete a named connection by (config_id, connection_key). The caller checks
-- for routing references (default/fallback/mappings) and returns 409 before
-- calling this. Runs under InTenantTx.
DELETE FROM site_email_connection
WHERE config_id      = @config_id
  AND connection_key = @connection_key
  AND tenant_id      = @tenant_id;

-- name: GetConnectionSecretCiphertexts :many
-- Fetch (connection_key, provider_secret_encrypted) for all connections under a
-- config row. Used by buildAgentConfigReq to decrypt and build the connections
-- registry. Runs under InTenantTx.
SELECT connection_key, provider_secret_encrypted
FROM site_email_connection
WHERE config_id = @config_id
  AND tenant_id = @tenant_id;

-- ---------------------------------------------------------------------------
-- Org-propagation (m62 — Area 1)
-- ---------------------------------------------------------------------------

-- name: ListEmailInheritingSites :many
-- Returns sites that should receive the org config propagation:
--   - enrolled (status IN ('connected','degraded'))
--   - no per-site email config row (NOT EXISTS site_email_config WHERE site_id=sites.id)
-- Runs under InAgentTx (cross-tenant fan-out by the propagation River worker).
SELECT s.id, s.url
FROM sites s
WHERE s.tenant_id = @tenant_id
  AND s.status IN ('connected', 'degraded')
  AND NOT EXISTS (
      SELECT 1 FROM site_email_config sec
      WHERE sec.tenant_id = s.tenant_id
        AND sec.site_id   = s.id
  );

-- name: GetSiteRef :one
-- Fetch a site's URL by id. Used by the propagation worker and alert digest to
-- build per-site dashboard links. Runs under InAgentTx.
SELECT id, url, name
FROM sites
WHERE id        = @id
  AND tenant_id = @tenant_id;

-- ---------------------------------------------------------------------------
-- email_notify_settings  (m62 — alerts + digest)
-- ---------------------------------------------------------------------------

-- name: GetNotifySettings :one
-- Fetch the notify settings row for a tenant. Returns pgx.ErrNoRows when not
-- configured (service returns defaults; GET NEVER 404s — 0.35.1 lesson).
-- Runs under InTenantTx.
SELECT *
FROM email_notify_settings
WHERE tenant_id = @tenant_id;

-- name: UpsertNotifySettings :one
-- Full-replace upsert of notify settings. next_digest_at is computed by the
-- service (time.LoadLocation + nextDigestAt helper) and passed here.
-- Runs under InTenantTx.
INSERT INTO email_notify_settings (
    tenant_id,
    enabled, recipients,
    alert_on_failure, alert_throttle_minutes,
    digest_enabled, digest_cadence, digest_day, digest_hour, timezone,
    next_digest_at,
    updated_at
) VALUES (
    @tenant_id,
    @enabled, @recipients,
    @alert_on_failure, @alert_throttle_minutes,
    @digest_enabled, @digest_cadence, @digest_day, @digest_hour, @timezone,
    @next_digest_at,
    now()
)
ON CONFLICT (tenant_id)
DO UPDATE SET
    enabled                = EXCLUDED.enabled,
    recipients             = EXCLUDED.recipients,
    alert_on_failure       = EXCLUDED.alert_on_failure,
    alert_throttle_minutes = EXCLUDED.alert_throttle_minutes,
    digest_enabled         = EXCLUDED.digest_enabled,
    digest_cadence         = EXCLUDED.digest_cadence,
    digest_day             = EXCLUDED.digest_day,
    digest_hour            = EXCLUDED.digest_hour,
    timezone               = EXCLUDED.timezone,
    next_digest_at         = EXCLUDED.next_digest_at,
    updated_at             = now()
RETURNING *;

-- ---------------------------------------------------------------------------
-- email_alert_state  (m62 — per-site durable alert throttle)
-- ---------------------------------------------------------------------------

-- name: AccumulateAlertFailures :exec
-- Upsert: increment failures_since_alert by @delta. Creates the row if absent.
-- Runs under InAgentTx (called after IngestLogBatch for failed entries).
INSERT INTO email_alert_state (tenant_id, site_id, failures_since_alert, updated_at)
VALUES (@tenant_id, @site_id, @delta, now())
ON CONFLICT (tenant_id, site_id)
DO UPDATE SET
    failures_since_alert = email_alert_state.failures_since_alert + @delta,
    updated_at           = now();

-- name: ClaimAlertSlot :one
-- Single-statement conditional claim: sets last_alert_at = now() and resets
-- failures_since_alert = 0 ONLY when:
--   - failures_since_alert >= @min_failures (at least one new failure)
--   - last_alert_at IS NULL OR last_alert_at < now() - @throttle_interval
-- Returns the updated row (claim won) or no rows (pgx.ErrNoRows = throttled/skipped).
-- Multi-instance safe: no external SELECT, no RETURNING on nothing.
-- Runs under InAgentTx.
UPDATE email_alert_state
SET last_alert_at        = now(),
    failures_since_alert = 0,
    updated_at           = now()
WHERE tenant_id = @tenant_id
  AND site_id   = @site_id
  AND failures_since_alert >= @min_failures
  AND (last_alert_at IS NULL OR last_alert_at < now() - (CAST(@throttle_minutes AS int) * INTERVAL '1 minute'))
RETURNING *;

-- ---------------------------------------------------------------------------
-- Digest queries (m62)
-- ---------------------------------------------------------------------------

-- name: ListDueDigests :many
-- List notify-settings rows whose next_digest_at is in the past.
-- RunOnStart: false for the periodic job; this is called by DigestWorker on each run.
-- Runs under InAgentTx (cross-tenant worker).
SELECT *
FROM email_notify_settings
WHERE digest_enabled  = true
  AND enabled         = true
  AND next_digest_at IS NOT NULL
  AND next_digest_at <= now()
ORDER BY next_digest_at ASC
LIMIT @row_limit;

-- name: ClaimAdvanceDigest :one
-- Advance next_digest_at to the next period as a conditional claim.
-- Returns the updated row if the claim succeeds (next_digest_at still <= now(),
-- guard against double-claim by concurrent workers). Runs under InAgentTx.
UPDATE email_notify_settings
SET next_digest_at = @new_next_digest_at,
    updated_at     = now()
WHERE tenant_id     = @tenant_id
  AND next_digest_at <= now()
RETURNING *;

-- name: GetFleetStatsBySite :many
-- Per-site stats for the digest [from, to] window. Returns top sites by failure
-- count. Runs under InAgentTx.
SELECT
    site_id,
    COUNT(*)                                        AS total,
    COUNT(*) FILTER (WHERE status = 'sent')         AS sent_count,
    COUNT(*) FILTER (WHERE status = 'failed')       AS failed_count,
    COUNT(*) FILTER (WHERE status = 'bounced'
                        OR status = 'complained')   AS bounced_count
FROM site_email_log
WHERE tenant_id   = @tenant_id
  AND created_at >= @range_from
  AND created_at <= @range_to
GROUP BY site_id
ORDER BY failed_count DESC, total DESC
LIMIT @row_limit;

-- name: GetFleetDeliveryPerSite :many
-- Deliverability endpoint: per-site aggregate for GET /email/deliverability.
-- Joins site_email_log with sites (name, url) and site_email_config (provider
-- from the effective per-site row, or NULL when no config exists).
-- sorted by bounce_rate DESC then total DESC (riskiest first). Runs under InTenantTx.
SELECT
    l.site_id,
    s.name                                            AS site_name,
    s.url                                             AS site_url,
    COALESCE(sec.provider, '')                        AS provider,
    COUNT(*)                                          AS total,
    COUNT(*) FILTER (WHERE l.status = 'sent')         AS sent_count,
    COUNT(*) FILTER (WHERE l.status = 'failed')       AS failed_count,
    COUNT(*) FILTER (WHERE l.status = 'bounced')      AS bounced_count,
    COUNT(*) FILTER (WHERE l.status = 'complained')   AS complained_count,
    MAX(l.created_at) FILTER (WHERE l.status = 'sent') AS last_sent_at
FROM site_email_log l
JOIN sites s
  ON s.id        = l.site_id
 AND s.tenant_id = l.tenant_id
LEFT JOIN site_email_config sec
  ON sec.tenant_id = l.tenant_id
 AND sec.site_id   = l.site_id
WHERE l.tenant_id   = @tenant_id
  AND l.created_at >= @range_from
  AND l.created_at <= @range_to
GROUP BY l.site_id, s.name, s.url, sec.provider
ORDER BY
    (COUNT(*) FILTER (WHERE l.status = 'bounced'))::float
        / NULLIF(COUNT(*), 0) DESC NULLS LAST,
    COUNT(*) DESC;

-- name: GetFleetDeliveryDailyBySite :many
-- Sparkline data for GET /email/deliverability: daily sent counts per site
-- across the window, ordered oldest-first. The caller buckets these into
-- per-site slices ordered by day ASC to build the sparkline array.
-- Runs under InTenantTx.
SELECT
    site_id,
    date_trunc('day', created_at AT TIME ZONE 'UTC')::timestamptz AS day,
    COUNT(*) FILTER (WHERE status = 'sent')                        AS sent_count
FROM site_email_log
WHERE tenant_id   = @tenant_id
  AND created_at >= @range_from
  AND created_at <= @range_to
GROUP BY site_id, 2
ORDER BY site_id ASC, 2 ASC;

-- name: TopFailureSamples :many
-- Top failure samples for the digest (subject + truncated error, no bodies).
-- Runs under InAgentTx.
SELECT
    site_id,
    subject,
    error
FROM site_email_log
WHERE tenant_id   = @tenant_id
  AND status      = 'failed'
  AND created_at >= @range_from
  AND created_at <= @range_to
ORDER BY created_at DESC
LIMIT @row_limit;

-- name: TopFailureSamplesBySite :many
-- Top failure samples for a per-site failure alert email (subject + error, no bodies).
-- Runs under InAgentTx.
SELECT
    site_id,
    subject,
    error
FROM site_email_log
WHERE tenant_id   = @tenant_id
  AND site_id     = @site_id
  AND status      = 'failed'
  AND created_at >= @range_from
  AND created_at <= @range_to
ORDER BY created_at DESC
LIMIT @row_limit;
