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
    config_version, updated_at
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
    @config_version, now()
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
    last_purged_at     = EXCLUDED.last_purged_at,
    last_purge_kind    = EXCLUDED.last_purge_kind,
    last_preload_at    = EXCLUDED.last_preload_at,
    preload_pending    = EXCLUDED.preload_pending,
    preload_total      = EXCLUDED.preload_total,
    reported_at        = now()
RETURNING *;

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
