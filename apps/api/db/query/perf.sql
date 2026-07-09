-- M36 Performance Suite queries (ADR-046). Every statement is tenant-scoped both
-- explicitly (tenant_id in the WHERE/VALUES) and by RLS (the app.tenant_id
-- policy / app.agent policy). The repo wraps each call in InTenantTx/InAgentTx —
-- these queries never set GUCs themselves. updated_at is set here via now().

-- ---------------------------------------------------------------------------
-- site_perf_config
-- ---------------------------------------------------------------------------

-- name: GetPerfConfig :one
SELECT * FROM site_perf_config
WHERE site_id = @site_id;

-- name: UpsertPerfConfig :one
-- Insert-or-update the per-site performance config. The agent install-state
-- columns (server_software, dropin_installed, wp_cache_constant_set,
-- htaccess_managed) are NOT touched here — those are reported by the agent via
-- UpdatePerfInstallState — so an operator config save never clobbers them.
INSERT INTO site_perf_config (
    site_id, tenant_id,
    cache_enabled, cache_logged_in, cache_mobile, cache_refresh,
    cache_refresh_interval, cache_link_prefetch,
    cache_bypass_urls, cache_bypass_cookies, cache_include_queries, cache_include_cookies,
    css_js_minify, css_rucss, css_rucss_include_selectors, css_js_self_host_third_party,
    js_delay, js_delay_method, js_delay_excludes,
    js_delay_third_party, js_delay_third_party_excludes,
    fonts_display_swap, fonts_optimize_google, fonts_preload,
    lazy_load, lazy_load_exclusions, properly_size_images,
    youtube_placeholder, self_host_gravatars,
    cdn_enabled, cdn_url, cdn_file_types, cdn_provider, cdn_credentials_encrypted,
    db_auto_clean, db_auto_clean_interval,
    db_post_revisions, db_post_auto_drafts, db_post_trashed,
    db_comments_spam, db_comments_trashed, db_transients_expired, db_optimize_tables,
    bloat_disable_block_css, bloat_disable_dashicons, bloat_disable_emojis,
    bloat_disable_jquery_migrate, bloat_disable_xml_rpc, bloat_disable_rss_feed,
    bloat_disable_oembeds, bloat_heartbeat_control, bloat_post_revisions_control,
    preload_concurrency, preload_delay_ms, preload_batch_size, preload_max_load,
    config_version, woo_cacheable_session, fonts_transcode_woff2,
    fonts_subset, fonts_subset_mode, fonts_subset_range,
    rum_enabled, rum_sample_rate, max_distinct_countries, min_sample_count,
    updated_at
) VALUES (
    @site_id, @tenant_id,
    @cache_enabled, @cache_logged_in, @cache_mobile, @cache_refresh,
    @cache_refresh_interval, @cache_link_prefetch,
    @cache_bypass_urls, @cache_bypass_cookies, @cache_include_queries, @cache_include_cookies,
    @css_js_minify, @css_rucss, @css_rucss_include_selectors, @css_js_self_host_third_party,
    @js_delay, @js_delay_method, @js_delay_excludes,
    @js_delay_third_party, @js_delay_third_party_excludes,
    @fonts_display_swap, @fonts_optimize_google, @fonts_preload,
    @lazy_load, @lazy_load_exclusions, @properly_size_images,
    @youtube_placeholder, @self_host_gravatars,
    @cdn_enabled, @cdn_url, @cdn_file_types, @cdn_provider, @cdn_credentials_encrypted,
    @db_auto_clean, @db_auto_clean_interval,
    @db_post_revisions, @db_post_auto_drafts, @db_post_trashed,
    @db_comments_spam, @db_comments_trashed, @db_transients_expired, @db_optimize_tables,
    @bloat_disable_block_css, @bloat_disable_dashicons, @bloat_disable_emojis,
    @bloat_disable_jquery_migrate, @bloat_disable_xml_rpc, @bloat_disable_rss_feed,
    @bloat_disable_oembeds, @bloat_heartbeat_control, @bloat_post_revisions_control,
    @preload_concurrency, @preload_delay_ms, @preload_batch_size, @preload_max_load,
    @config_version, @woo_cacheable_session, @fonts_transcode_woff2,
    @fonts_subset, @fonts_subset_mode, @fonts_subset_range,
    @rum_enabled, @rum_sample_rate, @max_distinct_countries, @min_sample_count,
    now()
)
ON CONFLICT (site_id) DO UPDATE SET
    cache_enabled                 = EXCLUDED.cache_enabled,
    cache_logged_in               = EXCLUDED.cache_logged_in,
    cache_mobile                  = EXCLUDED.cache_mobile,
    cache_refresh                 = EXCLUDED.cache_refresh,
    cache_refresh_interval        = EXCLUDED.cache_refresh_interval,
    cache_link_prefetch           = EXCLUDED.cache_link_prefetch,
    cache_bypass_urls             = EXCLUDED.cache_bypass_urls,
    cache_bypass_cookies          = EXCLUDED.cache_bypass_cookies,
    cache_include_queries         = EXCLUDED.cache_include_queries,
    cache_include_cookies         = EXCLUDED.cache_include_cookies,
    css_js_minify                 = EXCLUDED.css_js_minify,
    css_rucss                     = EXCLUDED.css_rucss,
    css_rucss_include_selectors   = EXCLUDED.css_rucss_include_selectors,
    css_js_self_host_third_party  = EXCLUDED.css_js_self_host_third_party,
    js_delay                      = EXCLUDED.js_delay,
    js_delay_method               = EXCLUDED.js_delay_method,
    js_delay_excludes             = EXCLUDED.js_delay_excludes,
    js_delay_third_party          = EXCLUDED.js_delay_third_party,
    js_delay_third_party_excludes = EXCLUDED.js_delay_third_party_excludes,
    fonts_display_swap            = EXCLUDED.fonts_display_swap,
    fonts_optimize_google         = EXCLUDED.fonts_optimize_google,
    fonts_preload                 = EXCLUDED.fonts_preload,
    lazy_load                     = EXCLUDED.lazy_load,
    lazy_load_exclusions          = EXCLUDED.lazy_load_exclusions,
    properly_size_images          = EXCLUDED.properly_size_images,
    youtube_placeholder           = EXCLUDED.youtube_placeholder,
    self_host_gravatars           = EXCLUDED.self_host_gravatars,
    cdn_enabled                   = EXCLUDED.cdn_enabled,
    cdn_url                       = EXCLUDED.cdn_url,
    cdn_file_types                = EXCLUDED.cdn_file_types,
    cdn_provider                  = EXCLUDED.cdn_provider,
    cdn_credentials_encrypted     = EXCLUDED.cdn_credentials_encrypted,
    db_auto_clean                 = EXCLUDED.db_auto_clean,
    db_auto_clean_interval        = EXCLUDED.db_auto_clean_interval,
    db_post_revisions             = EXCLUDED.db_post_revisions,
    db_post_auto_drafts           = EXCLUDED.db_post_auto_drafts,
    db_post_trashed               = EXCLUDED.db_post_trashed,
    db_comments_spam              = EXCLUDED.db_comments_spam,
    db_comments_trashed           = EXCLUDED.db_comments_trashed,
    db_transients_expired         = EXCLUDED.db_transients_expired,
    db_optimize_tables            = EXCLUDED.db_optimize_tables,
    bloat_disable_block_css       = EXCLUDED.bloat_disable_block_css,
    bloat_disable_dashicons       = EXCLUDED.bloat_disable_dashicons,
    bloat_disable_emojis          = EXCLUDED.bloat_disable_emojis,
    bloat_disable_jquery_migrate  = EXCLUDED.bloat_disable_jquery_migrate,
    bloat_disable_xml_rpc         = EXCLUDED.bloat_disable_xml_rpc,
    bloat_disable_rss_feed        = EXCLUDED.bloat_disable_rss_feed,
    bloat_disable_oembeds         = EXCLUDED.bloat_disable_oembeds,
    bloat_heartbeat_control       = EXCLUDED.bloat_heartbeat_control,
    bloat_post_revisions_control  = EXCLUDED.bloat_post_revisions_control,
    preload_concurrency           = EXCLUDED.preload_concurrency,
    preload_delay_ms              = EXCLUDED.preload_delay_ms,
    preload_batch_size            = EXCLUDED.preload_batch_size,
    preload_max_load              = EXCLUDED.preload_max_load,
    config_version                = EXCLUDED.config_version,
    woo_cacheable_session         = EXCLUDED.woo_cacheable_session,
    fonts_transcode_woff2         = EXCLUDED.fonts_transcode_woff2,
    fonts_subset                  = EXCLUDED.fonts_subset,
    fonts_subset_mode             = EXCLUDED.fonts_subset_mode,
    fonts_subset_range            = EXCLUDED.fonts_subset_range,
    rum_enabled                   = EXCLUDED.rum_enabled,
    rum_sample_rate               = EXCLUDED.rum_sample_rate,
    max_distinct_countries        = EXCLUDED.max_distinct_countries,
    min_sample_count              = EXCLUDED.min_sample_count,
    updated_at                    = now()
RETURNING *;

-- name: UpdatePerfInstallState :one
-- The agent reports the server/install state it observed when applying config.
-- Kept separate from UpsertPerfConfig so an operator's config save never
-- overwrites agent-reported facts (and vice-versa). Runs under app.agent.
UPDATE site_perf_config
SET server_software       = @server_software,
    dropin_installed      = @dropin_installed,
    wp_cache_constant_set = @wp_cache_constant_set,
    htaccess_managed      = @htaccess_managed,
    updated_at            = now()
WHERE site_id = @site_id
RETURNING *;

-- name: UpdateBeaconKeyAcked :one
-- GH #174 — records whether the agent's most recent config-ack reported that
-- it currently holds a non-empty rum_beacon_key (configAckBody.rum_beacon_present).
-- Kept separate from UpsertPerfConfig for the same reason as
-- UpdatePerfInstallState: an operator config save must never overwrite this
-- agent-reported fact, and vice-versa. Runs under app.agent. RETURNING * lets
-- the caller decide, in one round-trip, whether a re-mint reconcile job is
-- needed: rum_enabled AND beacon_key_hash IS NOT NULL AND NOT @present.
UPDATE site_perf_config
SET beacon_key_acked_present = @present,
    beacon_key_acked_at      = now(),
    updated_at                = now()
WHERE site_id = @site_id
RETURNING *;

-- ---------------------------------------------------------------------------
-- site_cache_stats
-- ---------------------------------------------------------------------------

-- name: GetCacheStats :one
SELECT * FROM site_cache_stats
WHERE site_id = @site_id;

-- name: UpsertCacheStats :one
-- The agent pushes the latest cache gauges; overwritten in place (no history).
INSERT INTO site_cache_stats (
    site_id, tenant_id, cached_pages_count, cache_size_bytes,
    last_purged_at, last_purge_kind, last_preload_at,
    preload_pending, preload_total, reported_at
) VALUES (
    @site_id, @tenant_id, @cached_pages_count, @cache_size_bytes,
    @last_purged_at, @last_purge_kind, @last_preload_at,
    @preload_pending, @preload_total, now()
)
ON CONFLICT (site_id) DO UPDATE SET
    cached_pages_count = EXCLUDED.cached_pages_count,
    cache_size_bytes   = EXCLUDED.cache_size_bytes,
    -- last_purged_at has TWO writers: the control plane (MarkCachePurged on every
    -- operator dashboard purge) and the agent (records its own auto-purges and
    -- reports them here). GREATEST keeps the MOST RECENT of the two and ignores
    -- NULLs, so neither writer can regress the gauge: an agent push with a NULL or
    -- older purge time never clobbers a newer CP stamp, and a newer agent
    -- auto-purge advances it. The kind tracks whichever timestamp wins.
    last_purged_at     = GREATEST(EXCLUDED.last_purged_at, site_cache_stats.last_purged_at),
    last_purge_kind    = CASE
        WHEN EXCLUDED.last_purged_at IS NOT NULL
         AND (site_cache_stats.last_purged_at IS NULL
              OR EXCLUDED.last_purged_at >= site_cache_stats.last_purged_at)
        THEN EXCLUDED.last_purge_kind
        ELSE site_cache_stats.last_purge_kind
    END,
    last_preload_at    = EXCLUDED.last_preload_at,
    preload_pending    = EXCLUDED.preload_pending,
    preload_total      = EXCLUDED.preload_total,
    reported_at        = now()
RETURNING *;

-- name: MarkCachePurged :exec
-- Stamp the last-purge gauge from the CONTROL PLANE when an operator purge runs
-- (dashboard "Purge everything" / "Purge URL"). The agent's periodic stats push
-- never reports a purge time, so without this the gauge sits at "Never" forever.
-- Sets ONLY the purge columns: on a first-ever insert the other gauges take their
-- schema defaults (0) and the agent fills them in on its next push; on conflict
-- they keep the agent's last-reported values. Paired with UpsertCacheStats's
-- GREATEST so a later agent push cannot wipe or regress this.
INSERT INTO site_cache_stats (
    site_id, tenant_id, last_purged_at, last_purge_kind, reported_at
) VALUES (
    @site_id, @tenant_id, now(), @last_purge_kind, now()
)
ON CONFLICT (site_id) DO UPDATE SET
    last_purged_at  = now(),
    last_purge_kind = EXCLUDED.last_purge_kind;

-- ---------------------------------------------------------------------------
-- cache_purge_audit
-- ---------------------------------------------------------------------------

-- name: InsertCachePurgeAudit :one
INSERT INTO cache_purge_audit (
    tenant_id, site_id, kind, initiator_user_id, target_urls, urls_count
) VALUES (
    @tenant_id, @site_id, @kind, @initiator_user_id, @target_urls, @urls_count
)
RETURNING *;

-- name: ListCachePurgeAuditForSite :many
SELECT * FROM cache_purge_audit
WHERE tenant_id = @tenant_id AND site_id = @site_id
ORDER BY created_at DESC, id DESC
LIMIT @row_limit OFFSET @row_offset;

-- ---------------------------------------------------------------------------
-- rucss_results
-- ---------------------------------------------------------------------------

-- name: GetRucssResultByHash :one
SELECT * FROM rucss_results
WHERE site_id = @site_id AND structure_hash = @structure_hash;

-- name: UpsertRucssResult :one
INSERT INTO rucss_results (
    tenant_id, site_id, structure_hash, url,
    original_css_bytes, used_css_bytes, reduction_pct, used_css_s3_key,
    selectors_total, selectors_kept, selectors_dropped, compute_ms,
    last_used_at
) VALUES (
    @tenant_id, @site_id, @structure_hash, @url,
    @original_css_bytes, @used_css_bytes, @reduction_pct, @used_css_s3_key,
    @selectors_total, @selectors_kept, @selectors_dropped, @compute_ms,
    now()
)
ON CONFLICT (site_id, structure_hash) DO UPDATE SET
    url                = EXCLUDED.url,
    original_css_bytes = EXCLUDED.original_css_bytes,
    used_css_bytes     = EXCLUDED.used_css_bytes,
    reduction_pct      = EXCLUDED.reduction_pct,
    used_css_s3_key    = EXCLUDED.used_css_s3_key,
    selectors_total    = EXCLUDED.selectors_total,
    selectors_kept     = EXCLUDED.selectors_kept,
    selectors_dropped  = EXCLUDED.selectors_dropped,
    compute_ms         = EXCLUDED.compute_ms,
    last_used_at       = now()
RETURNING *;

-- name: TouchRucssResultLastUsed :exec
UPDATE rucss_results
SET last_used_at = now()
WHERE site_id = @site_id AND structure_hash = @structure_hash;

-- name: ListRucssResultsForSite :many
SELECT * FROM rucss_results
WHERE tenant_id = @tenant_id AND site_id = @site_id
ORDER BY last_used_at DESC, id DESC
LIMIT @row_limit OFFSET @row_offset;

-- ---------------------------------------------------------------------------
-- rucss_jobs
-- ---------------------------------------------------------------------------

-- name: InsertRucssJob :one
INSERT INTO rucss_jobs (
    id, tenant_id, site_id, structure_hash, url, state
) VALUES (
    @id, @tenant_id, @site_id, @structure_hash, @url, 'queued'
)
RETURNING *;

-- name: UpdateRucssJobState :one
-- Advance the job lifecycle (queued->running->done|failed). completed_at is set
-- (to now()) only when @done is true; result_id/error_reason are passed through
-- as the terminal state dictates.
UPDATE rucss_jobs
SET state        = @state,
    error_reason = @error_reason,
    result_id    = @result_id,
    completed_at = CASE WHEN @done::boolean THEN now() ELSE completed_at END
WHERE id = @id AND tenant_id = @tenant_id
RETURNING *;

-- name: GetRucssJob :one
SELECT * FROM rucss_jobs
WHERE id = @id AND tenant_id = @tenant_id;

-- ---------------------------------------------------------------------------
-- db-clean scheduling (M38)
-- ---------------------------------------------------------------------------

-- name: GetDueDBCleanSites :many
-- Returns up to @limit site_perf_config rows where db_auto_clean=true and the
-- site is due for cleanup (next_db_clean_at IS NULL means first-run-ever and
-- is treated as immediately due). Runs under app.agent (cross-tenant sweep).
SELECT site_id, tenant_id, db_auto_clean_interval, next_db_clean_at
FROM site_perf_config
WHERE db_auto_clean = true
  AND (next_db_clean_at IS NULL OR next_db_clean_at <= now())
LIMIT @row_limit;

-- name: UpdateNextDBCleanAt :exec
-- Advance the next_db_clean_at timestamp after a clean job is dispatched.
-- Runs under app.agent (the scheduled-dispatch path is cross-tenant).
UPDATE site_perf_config
SET next_db_clean_at = @next_db_clean_at,
    updated_at       = now()
WHERE site_id = @site_id;

-- name: SetActiveDBCleanJob :exec
-- Stamp the in-flight db_clean job id + start time for the watchdog.
-- Runs under app.agent (cross-tenant scheduled path) or InTenantTx (operator).
UPDATE site_perf_config
SET active_db_clean_job_id  = @active_db_clean_job_id,
    active_db_clean_started = @active_db_clean_started,
    updated_at              = now()
WHERE site_id = @site_id;

-- name: ClearActiveDBCleanJob :exec
-- Clear the in-flight db_clean watchdog columns on completion or failure.
UPDATE site_perf_config
SET active_db_clean_job_id  = NULL,
    active_db_clean_started = NULL,
    updated_at              = now()
WHERE site_id = @site_id;

-- name: SetActiveDBScanJob :exec
-- Stamp the in-flight db_scan job id + start time for the watchdog.
UPDATE site_perf_config
SET active_db_scan_job_id  = @active_db_scan_job_id,
    active_db_scan_started = @active_db_scan_started,
    updated_at             = now()
WHERE site_id = @site_id;

-- name: ClearActiveDBScanJob :exec
-- Clear the in-flight db_scan watchdog columns on completion or failure.
UPDATE site_perf_config
SET active_db_scan_job_id  = NULL,
    active_db_scan_started = NULL,
    updated_at             = now()
WHERE site_id = @site_id;

-- name: GetStalledDBCleanJobs :many
-- Returns site_perf_config rows with a stalled db_clean job (started but no
-- terminal event within the threshold). Runs under app.agent (cross-tenant).
SELECT site_id, tenant_id, active_db_clean_job_id
FROM site_perf_config
WHERE active_db_clean_started IS NOT NULL
  AND active_db_clean_started < now() - @clean_threshold::interval;

-- name: GetStalledDBScanJobs :many
-- Returns site_perf_config rows with a stalled db_scan job (started but no
-- result within the threshold). Runs under app.agent (cross-tenant).
SELECT site_id, tenant_id, active_db_scan_job_id
FROM site_perf_config
WHERE active_db_scan_started IS NOT NULL
  AND active_db_scan_started < now() - @scan_threshold::interval;

-- ---------------------------------------------------------------------------
-- site_db_scan_results (M39)
-- ---------------------------------------------------------------------------

-- name: UpsertDBScanResult :one
-- Persists (or refreshes) the latest db_scan result for a site.
-- Uses upsert so there is always at most one row per site.
-- Phase 2.1: tables_json carries the per-table inventory alongside categories_json.
-- Phase 3.3 (M41): orphaned_options_json, orphaned_cron_json, installed_plugins_json
--   carry the orphan-enumeration output from agents >= 0.16.0. Agents < 0.16.0
--   omit these fields; the caller passes '[]' for backward compat.
INSERT INTO site_db_scan_results (
    site_id, tenant_id, job_id, categories_json, tables_json,
    db_size_bytes, table_count, scanned_at, created_at,
    orphaned_options_json, orphaned_cron_json, installed_plugins_json
) VALUES (
    @site_id, @tenant_id, @job_id, @categories_json, @tables_json,
    @db_size_bytes, @table_count, @scanned_at, now(),
    @orphaned_options_json, @orphaned_cron_json, @installed_plugins_json
)
ON CONFLICT (site_id) DO UPDATE SET
    tenant_id              = EXCLUDED.tenant_id,
    job_id                 = EXCLUDED.job_id,
    categories_json        = EXCLUDED.categories_json,
    tables_json            = EXCLUDED.tables_json,
    db_size_bytes          = EXCLUDED.db_size_bytes,
    table_count            = EXCLUDED.table_count,
    scanned_at             = EXCLUDED.scanned_at,
    created_at             = now(),
    orphaned_options_json  = EXCLUDED.orphaned_options_json,
    orphaned_cron_json     = EXCLUDED.orphaned_cron_json,
    installed_plugins_json = EXCLUDED.installed_plugins_json
RETURNING *;

-- name: GetDBScanResult :one
-- Returns the latest scan result for a site (tenant-scoped via RLS).
SELECT * FROM site_db_scan_results
WHERE site_id = @site_id AND tenant_id = @tenant_id;

-- ---------------------------------------------------------------------------
-- site_db_size_history (M42)
-- ---------------------------------------------------------------------------

-- name: InsertDBSizeHistory :one
-- Appends one size data point after a successful db_scan.
-- Called from the same InTenantTx as UpsertDBScanResult so both land
-- atomically. ON CONFLICT DO NOTHING on (site_id, scanned_at) prevents
-- duplicate rows if the operator retriggers a scan within the same second.
INSERT INTO site_db_size_history (
    site_id, tenant_id, db_size_bytes, table_count, scanned_at
) VALUES (
    @site_id, @tenant_id, @db_size_bytes, @table_count, @scanned_at
)
ON CONFLICT DO NOTHING
RETURNING *;

-- name: GetDBSizeHistory :many
-- Returns up to 366 data points for the trend chart, ordered oldest-first.
-- The caller passes the cutoff as a timestamptz (now() - interval).
-- Tenant-scoped via RLS (InTenantTx sets app.tenant_id).
SELECT * FROM site_db_size_history
WHERE site_id   = @site_id
  AND tenant_id = @tenant_id
  AND scanned_at >= @since
ORDER BY scanned_at ASC
LIMIT 366;

-- name: GetLatestDBSizeHistoryTableCount :one
-- Returns the table_count from the most recent site_db_size_history row for a
-- site (from either a manual "Scan database" run or a prior diagnostics-
-- sourced point; GH #196). The daily diagnostics push does not carry a table
-- inventory, so the diagnostics ingest tap carries this value forward rather
-- than writing a 0 over a real count. pgx.ErrNoRows when no prior point
-- exists yet — callers default to 0 in that case.
SELECT table_count FROM site_db_size_history
WHERE site_id = @site_id AND tenant_id = @tenant_id
ORDER BY scanned_at DESC
LIMIT 1;

-- name: PruneDBSizeHistory :execrows
-- Deletes rows older than the cutoff across ALL tenants (InAgentTx / app.agent).
-- LIMIT 2000 keeps each GC transaction short; the periodic job runs daily so
-- at typical scan frequency the cap is never hit in practice.
DELETE FROM site_db_size_history
WHERE id IN (
    SELECT h.id FROM site_db_size_history h
    WHERE h.created_at < @cutoff
    LIMIT 2000
);

-- ---------------------------------------------------------------------------
-- site_db_clean_results (M71)
-- ---------------------------------------------------------------------------

-- name: UpsertDBCleanResult :one
-- Persists (or refreshes) the latest db_clean result for a site.
-- Uses UPSERT so there is always at most one row per site.
-- Called under InTenantTx by HandleDBCleanProgress when done=true.
-- updated_at is set via now() on the created_at column (mirrors scan pattern).
INSERT INTO site_db_clean_results (
    site_id, tenant_id, job_id, result_json, rows_deleted, bytes_freed, cleaned_at, created_at
) VALUES (
    @site_id, @tenant_id, @job_id, @result_json, @rows_deleted, @bytes_freed, @cleaned_at, now()
)
ON CONFLICT (site_id) DO UPDATE SET
    tenant_id    = EXCLUDED.tenant_id,
    job_id       = EXCLUDED.job_id,
    result_json  = EXCLUDED.result_json,
    rows_deleted = EXCLUDED.rows_deleted,
    bytes_freed  = EXCLUDED.bytes_freed,
    cleaned_at   = EXCLUDED.cleaned_at,
    created_at   = now()
RETURNING *;

-- name: GetDBCleanResult :one
-- Returns the latest clean result for a site (tenant-scoped via RLS).
SELECT * FROM site_db_clean_results
WHERE site_id = @site_id AND tenant_id = @tenant_id;

-- ---------------------------------------------------------------------------
-- P3.7 — Fleet / Portfolio DB Health aggregate (tenant-level, no site_id param)
-- ---------------------------------------------------------------------------

-- name: GetFleetDbHealth :many
-- Returns one row per site that has a scan result, with the site name, latest
-- db_size_bytes, table_count, orphan counts from stored JSONB arrays, and a
-- growth_bytes derived from the earliest vs latest size-history points.
-- Tenant-scoped via RLS (InTenantTx sets app.tenant_id). Top-N ordering is
-- applied by the caller; this returns ALL scanned sites so the service can
-- compute tenant-level aggregates and then slice the top-N list.
WITH size_bounds AS (
    -- Earliest and latest size-history points per site within the lookback window.
    SELECT
        h.site_id,
        MIN(h.db_size_bytes) FILTER (WHERE h.scanned_at = (
            SELECT MIN(h2.scanned_at) FROM site_db_size_history h2
            WHERE h2.site_id = h.site_id AND h2.tenant_id = h.tenant_id
              AND h2.scanned_at >= @since
        )) AS first_size_bytes,
        MAX(h.db_size_bytes) FILTER (WHERE h.scanned_at = (
            SELECT MAX(h2.scanned_at) FROM site_db_size_history h2
            WHERE h2.site_id = h.site_id AND h2.tenant_id = h.tenant_id
              AND h2.scanned_at >= @since
        )) AS last_size_bytes
    FROM site_db_size_history h
    WHERE h.tenant_id = @tenant_id
      AND h.scanned_at >= @since
    GROUP BY h.site_id
)
SELECT
    s.id                                                   AS site_id,
    s.name                                                 AS site_name,
    r.db_size_bytes,
    r.table_count,
    jsonb_array_length(r.orphaned_options_json)            AS orphaned_options_count,
    jsonb_array_length(r.orphaned_cron_json)               AS orphaned_cron_count,
    r.scanned_at,
    COALESCE(sb.first_size_bytes, r.db_size_bytes)         AS first_size_bytes,
    COALESCE(sb.last_size_bytes,  r.db_size_bytes)         AS last_size_bytes
FROM site_db_scan_results r
JOIN sites s ON s.id = r.site_id
LEFT JOIN size_bounds sb ON sb.site_id = r.site_id
WHERE r.tenant_id = @tenant_id
ORDER BY r.db_size_bytes DESC;

-- ---------------------------------------------------------------------------
-- font_results (m55 — per-site dashboard catalog)
-- ---------------------------------------------------------------------------

-- name: UpsertFontResult :one
-- Agent -> CP results push. Inserts or updates the per-(site,source_hash)
-- catalog row. savings_pct is CP-derived: 1 - min(woff2_size, subset_size) / original_size.
-- Runs under app.agent (InAgentTx). tenant_id + site_id ALWAYS come from the
-- VERIFIED agent identity, never from the body.
INSERT INTO font_results (
    tenant_id, site_id, source_hash,
    family, source_file, original_ext, original_size,
    woff2_size, subset_size, unicode_range,
    state, error_detail, savings_pct,
    updated_at
) VALUES (
    @tenant_id, @site_id, @source_hash,
    @family, @source_file, @original_ext, @original_size,
    @woff2_size, @subset_size, @unicode_range,
    @state, @error_detail,
    -- savings_pct: CP-derived from best output size vs original.
    -- Uses LEAST(woff2_size, subset_size) ignoring NULLs. NULL when original unknown.
    CASE
        WHEN @original_size::integer > 0 AND (
             @woff2_size::integer IS NOT NULL OR @subset_size::integer IS NOT NULL
        ) THEN ROUND(
            (1.0 - LEAST(
                COALESCE(@woff2_size::integer, @original_size::integer),
                COALESCE(@subset_size::integer, @original_size::integer)
            )::numeric / GREATEST(@original_size::integer, 1)),
            4
        ) * 100
        ELSE NULL
    END,
    now()
)
ON CONFLICT (site_id, source_hash) DO UPDATE SET
    family        = COALESCE(EXCLUDED.family,       font_results.family),
    source_file   = COALESCE(EXCLUDED.source_file,  font_results.source_file),
    original_ext  = COALESCE(EXCLUDED.original_ext, font_results.original_ext),
    original_size = COALESCE(EXCLUDED.original_size, font_results.original_size),
    woff2_size    = COALESCE(EXCLUDED.woff2_size,   font_results.woff2_size),
    subset_size   = COALESCE(EXCLUDED.subset_size,  font_results.subset_size),
    unicode_range = COALESCE(EXCLUDED.unicode_range, font_results.unicode_range),
    state         = EXCLUDED.state,
    error_detail  = EXCLUDED.error_detail,
    savings_pct   = EXCLUDED.savings_pct,
    updated_at    = now()
RETURNING *;

-- name: UpdateWooThemeFragmentsSupported :execrows
-- Stamps the agent-reported woo_theme_fragments_supported flag and records when
-- the probe ran (woo_fragments_probed_at). Agent write path (InAgentTx) — the
-- agent is the sole writer; operators can never set this via the API.
-- Returns the number of rows affected so the caller can detect a missing config
-- row (0 rows = operator has never saved a config for this site).
UPDATE site_perf_config
SET woo_theme_fragments_supported = @woo_theme_fragments_supported,
    woo_fragments_probed_at       = now(),
    updated_at                    = now()
WHERE site_id = @site_id;

-- ---------------------------------------------------------------------------
-- site_cache_hit_ratio_history (M52 / #162)
-- ---------------------------------------------------------------------------

-- name: InsertCacheHitRatioHistory :one
-- Appends one hit-ratio data point. ON CONFLICT DO NOTHING on (site_id,
-- sampled_at) makes it idempotent within the same second. Operator write path
-- (InTenantTx).
INSERT INTO site_cache_hit_ratio_history (
    site_id, tenant_id, hit_count, miss_count, ratio_pct, sampled_at
) VALUES (
    @site_id, @tenant_id, @hit_count, @miss_count, @ratio_pct, @sampled_at
)
ON CONFLICT DO NOTHING
RETURNING *;

-- name: GetCacheHitRatioHistory :many
-- Returns up to 366 daily-aggregated hit-ratio data points for a site since
-- @since, ordered oldest-first. Each point is one calendar day (UTC) of data:
-- avg(ratio_pct), sum(hit_count), sum(miss_count). Daily downsampling ensures a
-- 365-day window fits within 366 points regardless of hourly sampling density.
-- Tenant-scoped via RLS (InTenantTx sets app.tenant_id).
SELECT
    date_trunc('day', sampled_at)::timestamptz                 AS sampled_at,
    avg(ratio_pct)::numeric                                     AS ratio_pct,
    sum(hit_count)::bigint                                      AS hit_count,
    sum(miss_count)::bigint                                     AS miss_count
FROM site_cache_hit_ratio_history
WHERE site_id   = @site_id
  AND tenant_id = @tenant_id
  AND sampled_at >= @since
GROUP BY date_trunc('day', sampled_at)
ORDER BY date_trunc('day', sampled_at) DESC
LIMIT 366;

-- name: PruneCacheHitRatioHistory :execrows
-- Deletes rows older than the cutoff across ALL tenants (InAgentTx / app.agent).
-- LIMIT 2000 keeps each GC transaction short.
DELETE FROM site_cache_hit_ratio_history
WHERE id IN (
    SELECT h.id FROM site_cache_hit_ratio_history h
    WHERE h.created_at < @cutoff
    LIMIT 2000
);

-- name: ListFontResultsForSite :many
-- Dashboard list: ordered by updated_at DESC, id DESC (standing `, id` tiebreaker
-- convention; batch inserts share updated_at so id breaks ties deterministically).
-- Runs under InTenantTx (operator path).
SELECT * FROM font_results
WHERE tenant_id = @tenant_id AND site_id = @site_id
ORDER BY updated_at DESC, id DESC
LIMIT @row_limit OFFSET @row_offset;
