package agentcmd

import (
	"bytes"
	"encoding/json"
)

// This file is the AUTHORITATIVE CP->agent command contract for the Performance
// Suite (ADR-046, Phase 6). The wp-agent-engineer mirrors these shapes in the
// agent's command handlers. Field names are JSON wire names; do not rename
// without updating both sides.
//
// Commands (all POST {site_url}/wp-json/wpmgr/v1/command/{cmd},
// Authorization: Bearer <minted EdDSA JWT>, aud=<siteId>, cmd=<command slug>):
//
//	perf_config_update — push the full per-site performance config so the agent
//	                     can mirror it locally on the request fast-path. The CP
//	                     is always the source of truth; the agent never decrypts
//	                     CDN credentials (those stay CP-side) and never receives
//	                     them — only the non-secret config travels here.
//	cache_enable       — install the advanced-cache drop-in + .htaccess block and
//	                     turn page caching on.
//	cache_disable      — remove the drop-in/.htaccess block and turn caching off.
//	cache_purge        — purge cached pages: scope "all" wipes the whole cache;
//	                     scope "url" purges only the listed urls.
//	cache_preload      — start a background preload (warm) pass over the sitemap.
//	db_clean           — run the configured database cleanup (revisions, drafts,
//	                     trashed, spam, expired transients, optimize tables).
//
// Every command's response is the {ok,detail} envelope: an HTTP 200 with ok=false
// is a semantic failure carrying the agent's reason in detail.

// PerfConfigRequest is the POST body for `perf_config_update`. It carries the
// non-secret subset of site_perf_config the agent applies locally. CDN
// credentials are deliberately ABSENT — the control plane is the only holder of
// the decrypted secret and performs CDN purges itself.
type PerfConfigRequest struct {
	ConfigVersion int `json:"config_version"`

	// Caching
	CacheEnabled         bool     `json:"cache_enabled"`
	CacheLoggedIn        bool     `json:"cache_logged_in"`
	CacheMobile          bool     `json:"cache_mobile"`
	CacheRefresh         bool     `json:"cache_refresh"`
	CacheRefreshInterval string   `json:"cache_refresh_interval"`
	CacheLinkPrefetch    bool     `json:"cache_link_prefetch"`
	// Page-cache variant lists use the agent's persisted/drop-in config keys
	// (unprefixed) on the wire, not the CP public/API field names. The agent
	// stores and serves from bypass_urls, bypass_cookies, include_queries, and
	// include_cookies; keeping these names aligned avoids silent ignore bugs.
	CacheBypassURLs      []string `json:"bypass_urls"`
	CacheBypassCookies   []string `json:"bypass_cookies"`
	CacheIncludeQueries  []string `json:"include_queries"`
	CacheIncludeCookies  []string `json:"include_cookies"`

	// Preload (cache-warm) throttle (M37). Operator-tunable; the agent clamps
	// each to the same bounds locally (concurrency 1..4, delay 0..10000 ms,
	// batch 1..500, max-load-per-core 0..64 with 0 disabling the load gate).
	PreloadConcurrency int     `json:"preload_concurrency"`
	PreloadDelayMs     int     `json:"preload_delay_ms"`
	PreloadBatchSize   int     `json:"preload_batch_size"`
	PreloadMaxLoad     float64 `json:"preload_max_load"`

	// CSS / JS
	CSSJSMinify             bool     `json:"css_js_minify"`
	CSSRucss                bool     `json:"css_rucss"`
	CSSRucssIncludeSelect   []string `json:"css_rucss_include_selectors"`
	CSSJSSelfHostThirdParty bool     `json:"css_js_self_host_third_party"`
	JSDelay                 bool     `json:"js_delay"`
	JSDelayMethod           string   `json:"js_delay_method"`
	JSDelayExcludes         []string `json:"js_delay_excludes"`
	JSDelayThirdParty       bool     `json:"js_delay_third_party"`
	JSDelayThirdPartyExc    []string `json:"js_delay_third_party_excludes"`

	// Fonts
	FontsDisplaySwap    bool `json:"fonts_display_swap"`
	FontsOptimizeGoogle bool `json:"fonts_optimize_google"`
	FontsPreload        bool `json:"fonts_preload"`
	// FontsTranscodeWOFF2 enables server-side WOFF2 transcoding for self-hosted
	// fonts. When true the agent requests transcode jobs from the CP for each
	// discovered TTF/OTF/WOFF font file it encounters. The CP enqueues a
	// font_transcode River job; the agent polls the result on subsequent builds
	// and serves WOFF2 with format() fallbacks when the result is ready.
	FontsTranscodeWOFF2 bool `json:"fonts_transcode_woff2"`

	// Media / lazy-load
	LazyLoad           bool     `json:"lazy_load"`
	LazyLoadExclusions []string `json:"lazy_load_exclusions"`
	ProperlySizeImages bool     `json:"properly_size_images"`
	YouTubePlaceholder bool     `json:"youtube_placeholder"`
	SelfHostGravatars  bool     `json:"self_host_gravatars"`

	// CDN (non-secret rewrite config only — credentials never leave the CP).
	// The agent persists the enabled flag as "cdn", not "cdn_enabled".
	CDNEnabled  bool   `json:"cdn"`
	CDNURL      string `json:"cdn_url,omitempty"`
	CDNFileType string `json:"cdn_file_types"`

	// Database cleanup
	DBAutoClean         bool   `json:"db_auto_clean"`
	DBAutoCleanInterval string `json:"db_auto_clean_interval"`
	DBPostRevisions     bool   `json:"db_post_revisions"`
	DBPostAutoDrafts    bool   `json:"db_post_auto_drafts"`
	DBPostTrashed       bool   `json:"db_post_trashed"`
	DBCommentsSpam      bool   `json:"db_comments_spam"`
	DBCommentsTrashed   bool   `json:"db_comments_trashed"`
	DBTransientsExpired bool   `json:"db_transients_expired"`
	DBOptimizeTables    bool   `json:"db_optimize_tables"`

	// Bloat removal
	BloatDisableBlockCSS     bool `json:"bloat_disable_block_css"`
	BloatDisableDashicons    bool `json:"bloat_disable_dashicons"`
	BloatDisableEmojis       bool `json:"bloat_disable_emojis"`
	BloatDisableJQueryMig    bool `json:"bloat_disable_jquery_migrate"`
	BloatDisableXMLRPC       bool `json:"bloat_disable_xml_rpc"`
	BloatDisableRSSFeed      bool `json:"bloat_disable_rss_feed"`
	BloatDisableOembeds      bool `json:"bloat_disable_oembeds"`
	BloatHeartbeatControl    bool `json:"bloat_heartbeat_control"`
	BloatPostRevisionControl bool `json:"bloat_post_revisions_control"`

	// M53 / #169 — WooCommerce cacheable-session.
	// When true the agent MAY cache the WooCommerce catalog shell for anonymous
	// shoppers with a cart. The agent hard-gates on its own theme probe
	// (woo_theme_fragments_supported) before acting; this flag is a CP-side
	// permission level only.
	WooCacheableSession bool `json:"woo_cacheable_session"`

	// M56 / RUM — Real User Monitoring settings.
	// RumEnabled tells the agent whether to inject the beacon collector snippet.
	RumEnabled bool `json:"rum_enabled"`
	// RumSampleRate is the server-side sampling rate (0.0–1.0). The agent also
	// applies this client-side so over-quota traffic is dropped before transmission.
	RumSampleRate float64 `json:"rum_sample_rate"`
	// RumBeaconKey is the PLAINTEXT beacon key, included ONLY when freshly
	// generated or rotated (i.e. the CP just minted a new key for this push).
	// On subsequent pushes where the key is unchanged, this field is omitted
	// (empty string) because the CP stores only the hash and cannot resend the
	// plaintext. The agent MUST persist the key locally and re-bake it into
	// cached HTML; it MUST NOT discard a non-empty value.
	RumBeaconKey string `json:"rum_beacon_key,omitempty"`
	// RumIngestURL is the full ingest endpoint URL (e.g.
	// "https://manage.example.com/rum/ingest"). Included whenever RumEnabled is
	// true so the agent can inject the correct endpoint into cached HTML.
	// Empty when RumEnabled is false.
	RumIngestURL string `json:"rum_ingest_url,omitempty"`
}

// PerfConfigResult is the agent's response to `perf_config_update`. The agent
// also reports the server/install state it observed when applying the config so
// the CP can record it (server_software, dropin_installed, …) via
// UpdatePerfInstallState. ok=false is a semantic failure.
type PerfConfigResult struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
	// Install-state facts the agent observed (optional; old agents omit them).
	ServerSoftware     string `json:"server_software,omitempty"`
	DropinInstalled    bool   `json:"dropin_installed,omitempty"`
	WPCacheConstantSet bool   `json:"wp_cache_constant_set,omitempty"`
	HtaccessManaged    bool   `json:"htaccess_managed,omitempty"`
}

// CacheEnableRequest is the POST body for `cache_enable`. It carries the config
// version so the agent can confirm it enabled with the right config.
type CacheEnableRequest struct {
	ConfigVersion int `json:"config_version"`
}

// CacheEnableResult is the agent's response to `cache_enable`.
type CacheEnableResult struct {
	OK                 bool   `json:"ok"`
	Detail             string `json:"detail"`
	ServerSoftware     string `json:"server_software,omitempty"`
	DropinInstalled    bool   `json:"dropin_installed,omitempty"`
	WPCacheConstantSet bool   `json:"wp_cache_constant_set,omitempty"`
	HtaccessManaged    bool   `json:"htaccess_managed,omitempty"`
}

// CacheDisableRequest is the POST body for `cache_disable`.
type CacheDisableRequest struct{}

// CacheDisableResult is the agent's response to `cache_disable`.
type CacheDisableResult struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// CachePurgeRequest is the POST body for `cache_purge`. Scope "all" purges the
// entire cache; scope "url" purges a single URL's variants. The agent's
// cache_purge handler reads the SINGULAR `url` field for scope=url, so URL must
// be set for a targeted purge; URLs is retained for forward-compat but is not
// what the current agent reads.
type CachePurgeRequest struct {
	Scope string   `json:"scope"` // "all" | "url"
	URL   string   `json:"url,omitempty"`
	URLs  []string `json:"urls,omitempty"`
}

// CachePurgeResult is the agent's response to `cache_purge`. PurgedCount is the
// number of cached entries the agent removed (best-effort gauge).
type CachePurgeResult struct {
	OK          bool   `json:"ok"`
	Detail      string `json:"detail"`
	PurgedCount int    `json:"purged_count"`
}

// CachePreloadRequest is the POST body for `cache_preload`.
type CachePreloadRequest struct {
	// Mode selects the warm strategy: "full" ⇒ the agent enumerates EVERY cacheable
	// front-end URL (all public post types incl. WooCommerce products, all taxonomy
	// term archives, author archives) and warms desktop + mobile buckets. Empty/any
	// other value with no URLs falls back to the agent's default (also full-site).
	Mode string `json:"mode,omitempty"`
	// URLs, when set, warms exactly those (the agent caps at 1000). Used by
	// content-update auto-preloads; the operator "Preload" button sends Mode:"full".
	URLs []string `json:"urls,omitempty"`
	// SitemapURL optionally overrides the agent's sitemap discovery (legacy; unused
	// by the full enumerator).
	SitemapURL string `json:"sitemap_url,omitempty"`
}

// RucssComputeRequest is the POST body for `rucss_compute` — an operator-initiated
// "compute Remove-Unused-CSS now" trigger. The agent self-fetches each URL with an
// out-of-band header so the optimizer runs the RUCSS stage, which posts the page
// to the CP /agent/v1/rucss endpoint and enqueues a River compute job. URLs empty
// ⇒ the agent computes the home page. All URLs MUST be same-host (agent enforces).
type RucssComputeRequest struct {
	URLs []string `json:"urls,omitempty"`
	// Reheat marks this as a post-compute re-warm self-fetch (CP-internal): the
	// agent tags the resulting /agent/v1/rucss POST with reheat=true so that if it
	// MISSES again (structure_hash drift) the worker does NOT re-trigger another
	// reheat — the loop terminates after one extra cycle. Operator-initiated
	// computes leave this false.
	Reheat bool `json:"reheat,omitempty"`
}

// RucssComputeResult is the agent's response to `rucss_compute`. Queued is the
// number of URLs the agent self-fetched to trigger a compute.
type RucssComputeResult struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
	Queued int    `json:"queued"`
}

// CachePreloadResult is the agent's response to `cache_preload`. Total is the
// number of URLs the agent will warm (when known up front).
type CachePreloadResult struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
	Total  int    `json:"total"`
}

// DBCleanRequest is the POST body for `db_clean` (M38 wire format).
//
// tasks carries the category ids the agent should execute (from the 14-standard
// list). An empty slice means all categories whose corresponding perf-config
// flag is enabled. The CP translates its stored per-flag config into the category
// id strings before sending; the agent NEVER reads local PerfConfig flags to gate
// execution on the command path.
//
// job_id is REQUIRED — the agent rejects a body without it. progress_endpoint is
// OPTIONAL; omit or leave empty to skip per-category progress callbacks.
type DBCleanRequest struct {
	JobID            string   `json:"job_id"`
	Tasks            []string `json:"tasks"`
	ProgressEndpoint string   `json:"progress_endpoint,omitempty"`
}

// DBCleanResult is the agent's immediate ACK response to `db_clean`. The ACK
// carries only ok+job_id — row counts arrive exclusively via per-category
// progress pushes to ProgressEndpoint. ok=false means the agent REFUSED; the
// CP should surface this as a terminal error, not retry.
type DBCleanResult struct {
	OK     bool   `json:"ok"`
	JobID  string `json:"job_id"`
	Detail string `json:"detail,omitempty"`
}

// DBCleanCategoryResult is one per-category result reported by the agent via
// the progress_endpoint POST. Each push contains exactly one category result.
// The CP aggregates rows_deleted across all categories to compute the total.
type DBCleanCategoryResult struct {
	// Category is one of the 14 standard category ids (e.g. "revisions").
	Category string `json:"category"`
	// RowsDeleted is rows removed for this category (0 when nothing cleaned).
	RowsDeleted int `json:"rows_deleted"`
	// BytesFreed is bytes reclaimed (DATA_FREE delta for optimize_tables; 0 for
	// deletion-only categories that cannot cheaply compute reclaimed bytes).
	BytesFreed int `json:"bytes_freed"`
	// State is "done" | "skipped" | "error".
	State string `json:"state"`
	// Detail is a human reason for skipped/error states.
	Detail string `json:"detail,omitempty"`
}

// ---------------------------------------------------------------------------
// db_scan — synchronous read-only database scan (M39 Phase 2)
// ---------------------------------------------------------------------------

// DBScanRequest is the POST body for the `db_scan` command.
// categories is the subset to scan; empty means all 14.
// The scan is SYNCHRONOUS — the full result is returned in the ACK body.
type DBScanRequest struct {
	JobID      string   `json:"job_id"`
	Categories []string `json:"categories,omitempty"`
}

// DBScanCategoryResult is one category's read-only scan result.
type DBScanCategoryResult struct {
	Count  int                 `json:"count"`
	Bytes  int64               `json:"bytes"`
	Tables []DBScanTableDetail `json:"tables,omitempty"`
}

// DBScanTableDetail is per-table detail for the optimize_tables category.
type DBScanTableDetail struct {
	Name       string `json:"name"`
	Engine     string `json:"engine"`
	DataLength int64  `json:"data_length"`
	DataFree   int64  `json:"data_free"`
}

// DBScanResult is the agent's synchronous ACK+result for `db_scan`.
// The full per-category map is returned in the ACK body (no async progress).
// Phase 2.1: Tables carries the full per-table inventory (name, rows, size,
// engine, overhead, ownership classification) returned alongside categories.
// Phase 3.3: OrphanedOptions, OrphanedCron, and InstalledPlugins are new
// fields added for agent >= 0.16.0. All three are omitempty for backward
// compatibility — agents < 0.16.0 omit them and the zero value (nil slice)
// is safe; the CP defaults the corresponding columns to '[]'.
type DBScanResult struct {
	OK          bool                `json:"ok"`
	JobID       string              `json:"job_id"`
	Detail      string              `json:"detail,omitempty"`
	Categories  dbScanCategoriesMap `json:"categories,omitempty"`
	DBSizeBytes int64               `json:"db_size_bytes"`
	TableCount  int                 `json:"table_count"`
	ScannedAt   int64               `json:"scanned_at"`
	// Tables is the full per-table inventory added in Phase 2.1.
	// Each element is classified with owner_type: core|plugin|theme|orphan|unknown.
	Tables []DBScanTableInventoryRow `json:"tables,omitempty"`

	// Phase 3.3: orphaned wp_options rows attributable to no installed plugin.
	// Omitted by agents < 0.16.0 (omitempty ensures nil drops from wire).
	OrphanedOptions []OrphanedOptionItem `json:"orphaned_options,omitempty"`

	// Phase 3.3: WP-Cron events attributable to no installed plugin or WP core.
	// Omitted by agents < 0.16.0.
	OrphanedCron []OrphanedCronItem `json:"orphaned_cron,omitempty"`

	// Phase 3.3: snapshot of every installed plugin/mu-plugin/dropin/network
	// plugin at scan time. Foundation for the P3.8 safety gate.
	// Omitted by agents < 0.16.0.
	InstalledPlugins []InstalledPluginItem `json:"installed_plugins,omitempty"`
}

// dbScanCategoriesMap tolerates a JSON array [] (PHP json_encode of an empty
// associative array) or null, decoding either as an empty map — same
// defense-in-depth technique as agentcmd.perRoleMap (GH #170, same class as
// #148). The agent-built categories map rarely empties in practice, but this
// keeps the CP decode robust regardless of agent version.
type dbScanCategoriesMap map[string]DBScanCategoryResult

func (m *dbScanCategoriesMap) UnmarshalJSON(data []byte) error {
	t := bytes.TrimSpace(data)
	if len(t) == 0 || string(t) == "null" || string(t) == "[]" {
		*m = dbScanCategoriesMap{}
		return nil
	}
	var raw map[string]DBScanCategoryResult
	if err := json.Unmarshal(t, &raw); err != nil {
		return err
	}
	*m = raw
	return nil
}

// ---------------------------------------------------------------------------
// Phase 3.3 orphan-enumeration types (agent >= 0.16.0)
// ---------------------------------------------------------------------------

// OrphanedOptionItem is one wp_options row assessed as a candidate-orphan:
// it passed all four conservative exclusion passes (WP-core name set, wpmgr_
// prefix, installed-plugin attribution, source-scan cross-check) without a
// plausible owner being found.
type OrphanedOptionItem struct {
	// Name is the option_name value.
	Name string `json:"name"`
	// Autoload reports whether the option is autoloaded ('yes' → true).
	Autoload bool `json:"autoload"`
	// SizeBytes is LENGTH(option_value) from MySQL — bytes, not characters.
	SizeBytes int64 `json:"size_bytes"`
	// GuessedPrefix is the longest token before the first underscore in Name
	// that the agent extracted as a candidate plugin prefix. Empty when the
	// name has no underscore (rare; signals truly unattributable).
	GuessedPrefix string `json:"guessed_prefix,omitempty"`
}

// OrphanedCronItem is one WP-Cron scheduled event assessed as a candidate-
// orphan. The signal is slug-prefix + source-scan attribution, NOT has_action()
// (which only reports callbacks registered in the current PHP request and would
// mass-false-positive at scan time when most plugins are not loaded).
type OrphanedCronItem struct {
	// Hook is the cron hook name (e.g. "myplugin_hourly_cleanup").
	Hook string `json:"hook"`
	// NextRunAt is the Unix timestamp of the next scheduled execution.
	NextRunAt int64 `json:"next_run_at"`
	// Recurrence is the schedule slug (e.g. "hourly", "twicedaily", "daily"),
	// or empty for one-off (non-recurring) events.
	Recurrence string `json:"recurrence,omitempty"`
	// ArgsHash is the key WP uses internally to de-duplicate events per hook
	// (the MD5 of the serialised args array). Carried for correlation only;
	// the raw args are NEVER included (privacy).
	ArgsHash string `json:"args_hash"`
	// ArgsCount is the number of elements in the args array (0 for no-args events).
	ArgsCount int `json:"args_count"`
}

// InstalledPluginItem is one entry in the per-scan installed-plugin snapshot.
// The snapshot includes ALL installed plugins (active OR inactive), must-use
// plugins, dropins, and network-activated plugins on multisite. It is the
// authoritative installed-set oracle for the P3.8 safety gate: an installed
// (even inactive) plugin is an owner; only artefacts from UNINSTALLED plugins
// are candidates for deletion.
type InstalledPluginItem struct {
	// Slug is the plugin directory name (e.g. "woocommerce"), the dropin
	// filename without .php (e.g. "object-cache"), or the mu-plugin filename
	// without .php.
	Slug string `json:"slug"`
	// Name is the display name from the plugin header ("Plugin Name:" header),
	// or the slug when unavailable.
	Name string `json:"name"`
	// Active is true when the plugin is network-active, must-use, a dropin,
	// OR in get_option("active_plugins"). False means installed-but-inactive.
	// Informational only — inactive installed plugins ARE still owners.
	Active bool `json:"active"`
	// Source is the installation channel:
	//   "plugin"    — regular plugin from get_plugins() (active or inactive)
	//   "mu-plugin" — must-use plugin from get_mu_plugins()
	//   "dropin"    — WordPress dropin (object-cache.php, advanced-cache.php, db.php, …)
	//   "network"   — network-activated on a multisite via get_site_option("active_sitewide_plugins")
	Source string `json:"source"`
}

// DBScanTableInventoryRow is one row in the per-table inventory returned by the
// agent's db_scan ACK (Phase 2.1). JSON keys are STABLE wire names — do not
// rename without coordinating with the agent and web layers.
type DBScanTableInventoryRow struct {
	// Name is the full table name including prefix (e.g. "wp_posts").
	Name string `json:"name"`
	// Rows is TABLE_ROWS from information_schema — an estimate for InnoDB tables
	// (can be 40-50% off actual count); exact for MyISAM/ARIA. No COUNT(*) issued.
	Rows int64 `json:"rows"`
	// SizeBytes is DATA_LENGTH + INDEX_LENGTH in bytes.
	SizeBytes int64 `json:"size_bytes"`
	// Engine is the storage engine (e.g. "InnoDB", "MyISAM").
	Engine string `json:"engine"`
	// OverheadBytes is DATA_FREE in bytes (reclaimable fragmented space).
	// InnoDB reports 0 in most cases; MyISAM can have meaningful values.
	OverheadBytes int64 `json:"overhead_bytes"`
	// BelongsTo is the human-readable ownership label:
	// "WordPress core" | plugin display name | theme display name | "Orphan"
	BelongsTo string `json:"belongs_to"`
	// OwnerType is the machine-readable ownership category:
	// "core" | "plugin" | "theme" | "orphan" | "unknown"
	OwnerType string `json:"owner_type"`
}

// ---------------------------------------------------------------------------
// db_table_action — per-table or bulk synchronous DDL operations (Phase 2.2)
// ---------------------------------------------------------------------------

// DBTableActionRequest is the POST body for the `db_table_action` command.
// The command is SYNCHRONOUS: the full per-table result is returned in the ACK.
//
// action must be one of: optimize | repair | drop | empty | analyze | convert_innodb.
// tables is the list of full table names (including prefix, e.g. "wp_wc_orders").
// job_id is a UUID v4 minted by the CP for idempotency correlation; it is
// echoed back in the response. The agent does NOT deduplicate execution on
// job_id — DROP is not retryable — but it is logged for correlation.
type DBTableActionRequest struct {
	JobID  string   `json:"job_id"`
	Action string   `json:"action"`
	Tables []string `json:"tables"`
}

// DBTableActionTableResult is one table's outcome in a DBTableActionResult.
// status is one of: done | skipped | error | not_found | rejected.
//
//   - done      — action executed successfully.
//   - skipped   — table failed the destructive safety gate: WP-core (drop+empty)
//                 or unclassified/unknown (drop). plugin/theme/orphan are allowed.
//   - rejected  — table name failed exact-match validation against information_schema.
//   - not_found — table not found in information_schema at validation time.
//   - error     — SQL execution returned an error (detail carries wpdb->last_error).
type DBTableActionTableResult struct {
	Table  string `json:"table"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// DBTableActionResult is the agent's synchronous ACK+result for `db_table_action`.
// ok=false with an empty results slice signals early validation failure (bad
// action, missing job_id, empty tables list); detail carries the reason.
// ok=false with non-empty results is never emitted — per-table failures use
// status="error" inside results while ok=true at the top level.
type DBTableActionResult struct {
	OK      bool                       `json:"ok"`
	JobID   string                     `json:"job_id,omitempty"`
	Action  string                     `json:"action,omitempty"`
	Results []DBTableActionTableResult `json:"results,omitempty"`
	Detail  string                     `json:"detail,omitempty"`
}

// ---------------------------------------------------------------------------
// db_orphan_delete — ASYNC destructive orphan removal (Phase 3.8)
// ---------------------------------------------------------------------------

// OrphanDeleteItem is one CP-signed deletion candidate carried in
// DBOrphanDeleteRequest. The agent re-verifies every field against the live
// installed-plugin set and exclusion lists before acting; owner_slug is the
// primary re-verification anchor.
type OrphanDeleteItem struct {
	// Kind is the artefact category: "option" | "cron" | "table".
	Kind string `json:"kind"`
	// Name is the artefact identifier:
	//   option → option_name (e.g. "myplugin_settings")
	//   cron   → hook name (e.g. "myplugin_hourly_cleanup")
	//   table  → full table name incl. prefix (e.g. "wp_myplugin_log")
	Name string `json:"name"`
	// OwnerSlug is the corpus-attributed wordpress.org plugin slug.
	// The agent re-checks: if owner_slug is now in the LIVE installed set,
	// the item is SKIPPED (not deleted).
	OwnerSlug string `json:"owner_slug"`
}

// DBOrphanDeleteRequest is the POST body for the `db_orphan_delete` command.
//
// Items carries ONLY the CP-signed eligible allowlist — exact items the
// operator confirmed. The agent never adds to it; it may only SKIP items
// that fail live re-verification.
//
// job_id is a UUID v4 required for idempotency correlation and async
// shutdown capture. progress_endpoint is the CP URL for async progress
// pushes (same signing + push pattern as db_clean).
type DBOrphanDeleteRequest struct {
	JobID            string             `json:"job_id"`
	Items            []OrphanDeleteItem `json:"items"`
	ProgressEndpoint string             `json:"progress_endpoint,omitempty"`
}

// DBOrphanDeleteResult is the agent's immediate ACK for `db_orphan_delete`.
// ok=false with detail means the agent REFUSED the whole batch (missing
// job_id, empty items list, or a JWT verification failure). Per-item
// outcomes arrive via progress POSTs; the final push has done=true.
type DBOrphanDeleteResult struct {
	OK     bool   `json:"ok"`
	JobID  string `json:"job_id,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// OrphanDeleteItemResult is one per-item outcome in an orphan-delete progress push.
// Status values: "done" | "skipped" | "error" | "not_found".
type OrphanDeleteItemResult struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// DBOrphanDeleteProgressBody is the body the agent POSTs for each batch of
// processed items (at POST /agent/v1/db-orphan-delete/progress).
type DBOrphanDeleteProgressBody struct {
	JobID          string                   `json:"job_id"`
	Results        []OrphanDeleteItemResult `json:"results"`
	DeletedOptions int                      `json:"deleted_options"`
	DeletedCron    int                      `json:"deleted_cron"`
	DeletedTables  int                      `json:"deleted_tables"`
	Skipped        int                      `json:"skipped"`
	// Done is true only on the FINAL push for this job.
	Done bool `json:"done"`
}
