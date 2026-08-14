package agentcmd

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
	CacheBypassURLs      []string `json:"cache_bypass_urls"`
	CacheBypassCookies   []string `json:"cache_bypass_cookies"`
	CacheIncludeQueries  []string `json:"cache_include_queries"`
	CacheIncludeCookies  []string `json:"cache_include_cookies"`

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

	// Media / lazy-load
	LazyLoad           bool     `json:"lazy_load"`
	LazyLoadExclusions []string `json:"lazy_load_exclusions"`
	ProperlySizeImages bool     `json:"properly_size_images"`
	YouTubePlaceholder bool     `json:"youtube_placeholder"`
	SelfHostGravatars  bool     `json:"self_host_gravatars"`

	// CDN (non-secret rewrite config only — credentials never leave the CP)
	CDNEnabled  bool   `json:"cdn_enabled"`
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
	// SitemapURL optionally overrides the agent's sitemap discovery.
	SitemapURL string `json:"sitemap_url,omitempty"`
}

// RucssComputeRequest is the POST body for `rucss_compute` — an operator-initiated
// "compute Remove-Unused-CSS now" trigger. The agent self-fetches each URL with an
// out-of-band header so the optimizer runs the RUCSS stage, which posts the page
// to the CP /agent/v1/rucss endpoint and enqueues a River compute job. URLs empty
// ⇒ the agent computes the home page. All URLs MUST be same-host (agent enforces).
type RucssComputeRequest struct {
	URLs []string `json:"urls,omitempty"`
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

// DBCleanRequest is the POST body for `db_clean`. The flags mirror the per-site
// db_* config so an ad-hoc clean can scope exactly what the operator requested.
type DBCleanRequest struct {
	PostRevisions     bool `json:"post_revisions"`
	PostAutoDrafts    bool `json:"post_auto_drafts"`
	PostTrashed       bool `json:"post_trashed"`
	CommentsSpam      bool `json:"comments_spam"`
	CommentsTrashed   bool `json:"comments_trashed"`
	TransientsExpired bool `json:"transients_expired"`
	OptimizeTables    bool `json:"optimize_tables"`
}

// DBCleanResult is the agent's response to `db_clean`. RowsCleaned is a
// best-effort total of rows removed across the enabled categories.
type DBCleanResult struct {
	OK          bool   `json:"ok"`
	Detail      string `json:"detail"`
	RowsCleaned int    `json:"rows_cleaned"`
}
