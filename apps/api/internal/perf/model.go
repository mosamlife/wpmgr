// Package perf is the Performance Suite control-plane domain (ADR-046, Phase 6).
// It wires together the m36 data model (site_perf_config / site_cache_stats /
// cache_purge_audit), the agent cache commands (cache.enable/disable/purge/
// preload, perf.config.update, db.clean), the RUCSS engine + River worker, and
// the SSE event bus.
//
// The package holds plain view-models (this file) decoupled from internal/db/sqlc
// row types so the service/handler layers never import sqlc directly — the repo
// maps sqlc rows <-> these models.
package perf

import (
	"time"

	"github.com/google/uuid"
)

// Config is the per-site performance configuration. It mirrors site_perf_config
// minus the encrypted CDN credentials, which are NEVER surfaced past the repo
// boundary (the service decrypts them server-side for CDN purges only).
type Config struct {
	SiteID   uuid.UUID
	TenantID uuid.UUID

	// Caching
	CacheEnabled         bool
	CacheLoggedIn        bool
	CacheMobile          bool
	CacheRefresh         bool
	CacheRefreshInterval string
	CacheLinkPrefetch    bool
	CacheBypassURLs      []string
	CacheBypassCookies   []string
	CacheIncludeQueries  []string
	CacheIncludeCookies  []string

	// Preload (cache-warm) throttle — operator-tunable queue drain knobs (M37).
	// The agent clamps each to the same bounds locally: concurrency 1..4,
	// delay 0..10000 ms, batch 1..500, max-load-per-core 0..64 (0 = disabled).
	PreloadConcurrency int
	PreloadDelayMs     int
	PreloadBatchSize   int
	PreloadMaxLoad     float64

	// CSS / JS
	CSSJSMinify               bool
	CSSRucss                  bool
	CSSRucssIncludeSelectors  []string
	CSSJSSelfHostThirdParty   bool
	JSDelay                   bool
	JSDelayMethod             string
	JSDelayExcludes           []string
	JSDelayThirdParty         bool
	JSDelayThirdPartyExcludes []string

	// Fonts
	FontsDisplaySwap    bool
	FontsOptimizeGoogle bool
	FontsPreload        bool
	// FontsTranscodeWOFF2 enables server-side WOFF2 transcoding for self-hosted
	// fonts. When true the agent requests transcode jobs from the CP; the CP
	// enqueues a font_transcode River job which produces <hash>.woff2 in object
	// storage. Default false. Pinned contract field name: fonts_transcode_woff2.
	FontsTranscodeWOFF2 bool
	// FontsSubset enables the subset-WOFF2 path (Phase 2). When true the CP
	// instructs the media-encoder to also produce a subset WOFF2 restricted to
	// FontsSubsetRange. Default false (opt-in, experimental).
	FontsSubset bool
	// FontsSubsetMode is the subsetting strategy: "range" (fixed unicode-range
	// block) is the only mode supported in v1. The field is stored for forward
	// compat but the CP ignores any value other than "range".
	FontsSubsetMode string
	// FontsSubsetRange is the named unicode range to subset to. "latin-ext" is
	// the safe default (U+0000-024F,U+1E00-1EFF).
	FontsSubsetRange string

	// Media / lazy-load
	LazyLoad           bool
	LazyLoadExclusions []string
	ProperlySizeImages bool
	YouTubePlaceholder bool
	SelfHostGravatars  bool

	// CDN — the rewrite config is operator-visible; credentials are not. CDNHasCredentials
	// is a derived boolean the handler may surface (true when ciphertext is stored).
	CDNEnabled        bool
	CDNURL            string
	CDNFileTypes      string
	CDNProvider       string
	CDNHasCredentials bool

	// Database cleanup
	DBAutoClean         bool
	DBAutoCleanInterval string
	DBPostRevisions     bool
	DBPostAutoDrafts    bool
	DBPostTrashed       bool
	DBCommentsSpam      bool
	DBCommentsTrashed   bool
	DBTransientsExpired bool
	DBOptimizeTables    bool
	// M38: CP-owned scheduling; nil means no pending auto-clean (first-run case).
	NextDBCleanAt *time.Time

	// Bloat removal
	BloatDisableBlockCSS     bool
	BloatDisableDashicons    bool
	BloatDisableEmojis       bool
	BloatDisableJQueryMig    bool
	BloatDisableXMLRPC       bool
	BloatDisableRSSFeed      bool
	BloatDisableOembeds      bool
	BloatHeartbeatControl    bool
	BloatPostRevisionControl bool

	// Server / install state (agent-reported)
	ServerSoftware     string
	DropinInstalled    bool
	WPCacheConstantSet bool
	HtaccessManaged    bool

	// M53 / #169 — WooCommerce cacheable-session (issue #169).
	// WooCacheableSession is operator-writable (default false). When true the CP
	// includes this flag in the perf-config push to the agent so the agent can
	// cache the WooCommerce catalog shell for anonymous shoppers with a cart.
	// The API ACCEPTS woo_cacheable_session=true even when
	// WooThemeFragmentsSupported=nil: the agent hard-gates on its own theme
	// probe (defense-in-depth), and the web UI disables the toggle when the agent
	// reports unsupported. We do not hard-reject at the CP to allow an operator
	// to pre-enable the flag before the agent probes (or when using a custom theme
	// that supports it without the standard fragments WC_AJAX hook).
	WooCacheableSession bool
	// WooThemeFragmentsSupported is agent-reported (read-only from the operator
	// API). Tri-state: nil = never probed, false = probed unsupported, true =
	// probed supported. The CP stores it but NEVER lets an operator PUT overwrite
	// it. Nil (null) is the correct initial state after M67 — not false.
	WooThemeFragmentsSupported *bool
	// WooFragmentsProbedAt is the timestamp of the last agent probe. Nil when
	// never probed. Read-only from the operator API.
	WooFragmentsProbedAt *time.Time

	// M56 / RUM — Real User Monitoring settings.
	// RumEnabled enables the RUM ingest endpoint for this site. When false the
	// ingest handler rejects beacons even when the beacon_key_hash resolves.
	RumEnabled bool
	// RumSampleRate is the fraction of eligible events to store (0.0–1.0).
	// 1.0 means accept all; the ingest handler applies unbiased random sampling
	// server-side so stored distributions remain statistically representative.
	RumSampleRate float64
	// MaxDistinctCountries caps how many distinct country values are stored per
	// site per rollup bucket. Excess countries collapse to "__other__".
	// Default 8; operator-tunable.
	MaxDistinctCountries int
	// MinSampleCount is the minimum sample count required before a p75 estimate
	// is surfaced to the dashboard. Results below this threshold are suppressed to
	// avoid misleading low-N medians. Default 100.
	MinSampleCount int
	// BeaconKeySet reports whether a beacon_key_hash is stored for this site.
	// The plaintext key is NEVER returned via the API; this flag lets the UI
	// show whether RUM is fully provisioned without exposing the key.
	BeaconKeySet bool
	// BeaconKeyAckedPresent reports whether the agent's most recent config-ack
	// (POST /agent/v1/perf/config-ack, field rum_beacon_present) confirmed it
	// currently holds a non-empty rum_beacon_key (GH #174). Combined with
	// BeaconKeySet=true, false here identifies the "hash committed but the
	// agent never got/kept the plaintext" stuck state that the ack-based
	// reconcile job exists to self-heal. Agent-reported, read-only from the
	// operator API.
	BeaconKeyAckedPresent bool
	// BeaconKeyAckedAt is the timestamp of the most recent ack signal. Nil
	// before any agent has ever reported (pre-#174 agents, or a site that has
	// never completed a config-ack cycle).
	BeaconKeyAckedAt *time.Time

	ConfigVersion int
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// FontTranscodeState is the outcome recorded in font_transcode_results for a
// single content-addressed source font hash.
type FontTranscodeState string

const (
	// FontTranscodePending means a River job has been inserted but has not
	// completed yet. The agent should poll again on the next build.
	FontTranscodePending FontTranscodeState = "pending"
	// FontTranscodeReady means the WOFF2 is available at Woff2Key.
	FontTranscodeReady FontTranscodeState = "ready"
	// FontTranscodeNegative means transcoding permanently failed for this
	// hash; the agent must serve the original font indefinitely.
	FontTranscodeNegative FontTranscodeState = "negative"
)

// FontTranscodeResult is the domain view of one font_transcode_results row.
type FontTranscodeResult struct {
	SourceHash string
	TenantID   uuid.UUID
	SiteID     uuid.UUID
	// State is derived: pending when Woff2Key==nil && !Negative; ready when
	// Woff2Key!=nil; negative when Negative==true.
	State       FontTranscodeState
	Woff2Key    string // empty unless State==FontTranscodeReady
	ErrorDetail string // non-empty when State==FontTranscodeNegative
}

// FontResultState enumerates the lifecycle states for a font_results row.
type FontResultState string

const (
	// FontResultPending means the transcode/subset job is enqueued but not complete.
	FontResultPending FontResultState = "pending"
	// FontResultReady means the full WOFF2 is available (woff2_size set, subset_size nil).
	FontResultReady FontResultState = "ready"
	// FontResultSubset means both the full WOFF2 and the subset WOFF2 are available.
	FontResultSubset FontResultState = "subset"
	// FontResultNegative means a permanent failure; the agent must serve the original font.
	FontResultNegative FontResultState = "negative"
)

// FontResult is the domain view of one font_results row — the per-site dashboard
// catalog. It is distinct from FontTranscodeResult (job-control layer). The
// savings_pct is CP-derived at upsert: 1 - min(woff2_size, subset_size) / original_size.
type FontResult struct {
	ID           uuid.UUID
	TenantID     uuid.UUID
	SiteID       uuid.UUID
	SourceHash   string
	Family       string // empty until the agent reports it
	SourceFile   string // basename of the original font URL
	OriginalExt  string // ttf | otf | woff
	OriginalSize int    // bytes
	Woff2Size    int    // bytes; 0 until ready
	SubsetSize   int    // bytes; 0 unless subset
	UnicodeRange string // CSS unicode-range for the subset; "" unless subset
	State        FontResultState
	ErrorDetail  string  // non-empty when State == FontResultNegative
	SavingsPct   float64 // 0..100; 0 when sizes unknown
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// CacheStats is the latest cache gauge set the agent reported for one site.
type CacheStats struct {
	SiteID           uuid.UUID
	TenantID         uuid.UUID
	CachedPagesCount int
	CacheSizeBytes   int64
	LastPurgedAt     *time.Time
	LastPurgeKind    string
	LastPreloadAt    *time.Time
	PreloadPending   int
	PreloadTotal     int
	ReportedAt       time.Time

	// M52 / #162 — window DELTA counts emitted by the agent since its last
	// report. When BOTH are zero/absent the history row is skipped. These
	// fields are ephemeral: they are not persisted to site_cache_stats and
	// are only used by ReportCacheStats to decide whether to append a
	// site_cache_hit_ratio_history row.
	CacheHitCount  int64
	CacheMissCount int64
}

// PurgeKind enumerates the cache_purge_audit.kind values.
type PurgeKind string

const (
	PurgeKindAll     PurgeKind = "all"
	PurgeKindURL     PurgeKind = "url"
	PurgeKindPost    PurgeKind = "post"
	PurgeKindPreload PurgeKind = "preload"
	PurgeKindAuto    PurgeKind = "auto"
)

// PurgeAuditEntry is one row of cache_purge_audit.
type PurgeAuditEntry struct {
	ID              uuid.UUID
	TenantID        uuid.UUID
	SiteID          uuid.UUID
	Kind            string
	InitiatorUserID uuid.UUID // uuid.Nil when system-initiated
	TargetURLs      []string
	URLsCount       int
	CreatedAt       time.Time
}

// DbSizeTrendPoint is one row of the site_db_size_history table as returned
// by the GetDBSizeHistory query. Only the trend fields are serialized to the
// API response; the row identity and tenant_id are internal and omitted.
type DbSizeTrendPoint struct {
	ID          uuid.UUID `json:"-"`
	SiteID      uuid.UUID `json:"-"`
	TenantID    uuid.UUID `json:"-"`
	DBSizeBytes int64     `json:"db_size_bytes"`
	TableCount  int       `json:"table_count"`
	ScannedAt   time.Time `json:"scanned_at"`
	CreatedAt   time.Time `json:"-"`
}

// DBHealthResponse is the payload for GET /perf/db/health.
// Points is ordered ascending by scanned_at (oldest first). GrowthBytes and
// GrowthPct are derived from Points[0] vs Points[len-1]; both are zero when
// fewer than two points exist. The frontend must treat < 2 points as an
// empty-state condition and show the chart empty state rather than a line.
type DBHealthResponse struct {
	Points      []DbSizeTrendPoint `json:"points"`
	GrowthBytes int64              `json:"growth_bytes"`
	GrowthPct   float64            `json:"growth_pct"`
}

// CacheHitRatioPoint is one row of site_cache_hit_ratio_history as returned
// by GetCacheHitRatioHistory. Only the trend fields are serialized to the API
// response; row identity and tenant_id are internal and omitted.
type CacheHitRatioPoint struct {
	ID        uuid.UUID `json:"-"`
	SiteID    uuid.UUID `json:"-"`
	TenantID  uuid.UUID `json:"-"`
	HitCount  int64     `json:"hit_count"`
	MissCount int64     `json:"miss_count"`
	RatioPct  float64   `json:"ratio_pct"`
	SampledAt time.Time `json:"sampled_at"`
	CreatedAt time.Time `json:"-"`
}

// CacheHealthResponse is the payload for GET /sites/:siteId/perf/cache/health.
// Points is ordered ascending by sampled_at (oldest first). AvgRatioPct is
// the arithmetic mean of each point's ratio_pct; zero when Points is empty.
// The frontend must treat an empty Points slice as an empty-state condition
// and show the chart empty state rather than a line.
type CacheHealthResponse struct {
	Points      []CacheHitRatioPoint `json:"points"`
	AvgRatioPct float64              `json:"avg_ratio_pct"`
}

// ---------------------------------------------------------------------------
// P3.5 — Orphans Classification Report
// ---------------------------------------------------------------------------

// OrphanItem is one classified candidate-orphan artefact (wp_options row,
// WP-Cron event, or custom table). The Confidence field mirrors the
// dbclean.ConfidenceLevel wire values ("exact" | "prefix" | "heuristic" |
// "unknown"). Internal ids and tenant_id are never included.
type OrphanItem struct {
	// Name is the raw artefact identifier (option_name, hook name, or table name).
	Name string `json:"name"`

	// OwnerSlug is the wordpress.org plugin slug the corpus attributed the item
	// to. Empty when Confidence is "unknown".
	OwnerSlug string `json:"owner_slug,omitempty"`

	// Confidence is the attribution strength: "exact" | "prefix" |
	// "heuristic" | "unknown". Matches dbclean.ConfidenceLevel wire values.
	Confidence string `json:"confidence"`

	// KnownPlugins is the full set of corpus slugs whose patterns matched this
	// item. When len > 1 the item is potentially shared between multiple plugins
	// and DeletableEligible will be false.
	KnownPlugins []string `json:"known_plugins,omitempty"`

	// Installed is true when the classified OwnerSlug or any of KnownPlugins
	// is present in the site's installed-plugin snapshot at scan time.
	// An installed plugin still owns its artefacts; Installed=true means this
	// item is NOT a real orphan.
	Installed bool `json:"installed"`

	// DeletableEligible is the conservative gate for deletion candidates.
	// True only when ALL of the following hold:
	//   - Confidence is "exact" or "prefix" (never heuristic or unknown).
	//   - len(KnownPlugins) == 1 (no ownership ambiguity).
	//   - Installed is false (the owning plugin is not in the scan snapshot).
	// P3.8 will additionally require a live re-verify and type-to-confirm.
	DeletableEligible bool `json:"deletable_eligible"`

	// --- type-specific optional fields ---

	// SizeBytes is LENGTH(option_value) for options, or DATA+INDEX bytes for
	// tables. Zero for cron items.
	SizeBytes int64 `json:"size_bytes,omitempty"`

	// Autoload is set for options only (true when autoload='yes').
	Autoload *bool `json:"autoload,omitempty"`

	// NextRunAt is the Unix timestamp of the next scheduled run (cron only).
	NextRunAt *int64 `json:"next_run_at,omitempty"`

	// Recurrence is the cron schedule slug (cron only; empty = one-off).
	Recurrence string `json:"recurrence,omitempty"`

	// Rows is the estimated row count for tables (tables only).
	Rows *int64 `json:"rows,omitempty"`
}

// OrphansCountSummary is the count summary embedded in OrphansReport.
type OrphansCountSummary struct {
	Options   int `json:"options"`
	Cron      int `json:"cron"`
	Tables    int `json:"tables"`
	Deletable int `json:"deletable"`
}

// OrphansReport is the response payload for GET /perf/db/orphans.
// Classification is performed on-demand against the live corpus at request time.
type OrphansReport struct {
	// Options is the classified list of orphaned wp_options candidate items.
	// Items whose owning plugin is currently installed are excluded; see
	// HiddenInstalled for the suppressed count.
	Options []OrphanItem `json:"options"`

	// Cron is the classified list of orphaned WP-Cron event candidate items.
	// Items whose owning plugin is currently installed are excluded; see
	// HiddenInstalled for the suppressed count.
	Cron []OrphanItem `json:"cron"`

	// Tables is the classified list of orphaned custom table candidate items
	// (those whose owner_type was "orphan" in the scan's tables_json).
	// Items whose owning plugin is currently installed are excluded; see
	// HiddenInstalled for the suppressed count.
	Tables []OrphanItem `json:"tables"`

	// CorpusVersion is the corpus_version of the first Signature loaded during
	// this request. Zero when the corpus is empty.
	CorpusVersion int `json:"corpus_version"`

	// SnapshotAvailable is true when the scan carried a non-empty installed-
	// plugins snapshot. When false (e.g. a scan from an agent < 0.16.0), the
	// installed cross-check is indeterminate and no item is DeletableEligible;
	// the UI/P3.8 must require a fresh scan from a current agent before offering
	// any deletion.
	SnapshotAvailable bool `json:"snapshot_available"`

	// HiddenInstalled is the total count of candidate items that were dropped
	// from Options, Cron, and Tables because their attributed owner plugin is
	// present in the installed-plugins snapshot. These items are not real orphans
	// (the plugin still owns them). Counts and the per-kind slices reflect only
	// the remaining genuine-orphan items.
	HiddenInstalled int `json:"hidden_installed"`

	// Counts is the derived count summary reflecting only the non-installed items
	// that appear in Options, Cron, and Tables.
	Counts OrphansCountSummary `json:"counts"`
}

// ---------------------------------------------------------------------------
// P3.7 — Fleet / Portfolio DB Health aggregate
// ---------------------------------------------------------------------------

// FleetSiteDbSummary is the per-site entry in the top-N list returned by
// GET /api/v1/perf/db/fleet-health. It carries the site's latest DB size,
// table count, orphan counts, and growth relative to the lookback window.
type FleetSiteDbSummary struct {
	SiteID      uuid.UUID `json:"site_id"`
	SiteName    string    `json:"site_name"`
	DBSizeBytes int64     `json:"db_size_bytes"`
	TableCount  int       `json:"table_count"`
	// OrphanedOptionsCount is the length of orphaned_options_json from the
	// latest scan. This is the raw count of wp_options orphan candidates, not
	// the classified/deletable subset; corpus classification is per-site only.
	OrphanedOptionsCount int `json:"orphaned_options_count"`
	// OrphanedCronCount is the length of orphaned_cron_json from the latest
	// scan (raw cron-event orphan candidates, unclassified).
	OrphanedCronCount int       `json:"orphaned_cron_count"`
	ScannedAt         time.Time `json:"scanned_at"`
	// GrowthBytes is the DB size change from the earliest to the latest
	// size-history point within the lookback window. Zero when fewer than two
	// history points exist for this site in the window.
	GrowthBytes int64 `json:"growth_bytes"`
}

// FleetSiteDbReview is the slim per-site entry in FleetDbHealth.SitesNeedingReview.
// Unlike TopSites (capped at fleetTopN, ordered by DB size), this list carries
// EVERY site flagged for review — i.e. every site with at least one orphan
// candidate — regardless of DB size, so the FE can enumerate the full flagged
// set (GH #197).
type FleetSiteDbReview struct {
	SiteID               uuid.UUID `json:"site_id"`
	SiteName             string    `json:"site_name"`
	OrphanedOptionsCount int       `json:"orphaned_options_count"`
	OrphanedCronCount    int       `json:"orphaned_cron_count"`
}

// FleetDbHealth is the response payload for GET /api/v1/perf/db/fleet-health.
// It aggregates database health across every site in the tenant that has at
// least one completed scan.
type FleetDbHealth struct {
	// TotalSitesScanned is the number of sites with at least one scan result.
	TotalSitesScanned int `json:"total_sites_scanned"`
	// TotalDBSizeBytes is the sum of the latest db_size_bytes across all scanned sites.
	TotalDBSizeBytes int64 `json:"total_db_size_bytes"`
	// TotalTableCount is the sum of table_count across all scanned sites.
	TotalTableCount int `json:"total_table_count"`
	// TotalOrphanedOptions is the sum of orphaned_options_count across all scanned sites.
	TotalOrphanedOptions int `json:"total_orphaned_options"`
	// TotalOrphanedCron is the sum of orphaned_cron_count across all scanned sites.
	TotalOrphanedCron int `json:"total_orphaned_cron"`
	// SitesWithOrphans is the count of sites that have at least one orphan
	// candidate (options or cron). These are worth investigating with the
	// per-site orphans endpoint.
	SitesWithOrphans int `json:"sites_with_orphans"`
	// TopSites is the top-N sites by DB size (descending). N is capped at 10.
	// When TotalSitesScanned is zero, TopSites is an empty slice.
	TopSites []FleetSiteDbSummary `json:"top_sites"`
	// SitesNeedingReview is EVERY scanned site that has at least one orphan
	// candidate (options or cron) — NOT capped, unlike TopSites. Sorted by
	// (orphaned_options_count + orphaned_cron_count) descending, then by
	// site_name. This is what lets the FE enumerate the full "sites with
	// items to review" set (GH #197); TopSites alone can omit a small site
	// that's flagged but not in the top-10-by-DB-size.
	SitesNeedingReview []FleetSiteDbReview `json:"sites_needing_review"`
}

// ---------------------------------------------------------------------------
// CDN credentials (existing; kept below the new types)
// ---------------------------------------------------------------------------

// CDNCredentials is the decrypted CDN provider secret. It exists ONLY inside the
// service for the duration of a CDN purge — it is never serialized to a client
// nor sent to the agent. The provider field mirrors Config.CDNProvider.
type CDNCredentials struct {
	Provider string `json:"provider"`
	// APIToken is the Cloudflare API token, Bunny AccessKey, or KeyCDN API key.
	APIToken string `json:"api_token"`
	// ZoneID is the Cloudflare zone id (Cloudflare only).
	ZoneID string `json:"zone_id,omitempty"`
	// Zone is the Bunny pull-zone / KeyCDN zone identifier (path-purge providers).
	Zone string `json:"zone,omitempty"`
}
