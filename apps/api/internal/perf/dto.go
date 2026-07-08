package perf

import (
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/rum"
)

// ---------------------------------------------------------------------------
// config DTOs
// ---------------------------------------------------------------------------

// cdnCredentialsDTO is the WRITE-ONLY credentials sub-object on PUT /perf/config.
// It is NEVER returned by GET (the service decrypts server-side only).
type cdnCredentialsDTO struct {
	APIToken string `json:"api_token"`
	ZoneID   string `json:"zone_id,omitempty"`
	Zone     string `json:"zone,omitempty"`
}

// perfConfigDTO is the JSON shape for GET/PUT /perf/config. CDNCredentials is
// accepted on PUT and never echoed on GET; CDNHasCredentials is read-only.
type perfConfigDTO struct {
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

	// Preload (cache-warm) throttle (M37). Clamped server-side to bounds:
	// concurrency 1..4, delay 0..10000 ms, batch 1..500, max-load 0..64.
	PreloadConcurrency int     `json:"preload_concurrency"`
	PreloadDelayMs     int     `json:"preload_delay_ms"`
	PreloadBatchSize   int     `json:"preload_batch_size"`
	PreloadMaxLoad     float64 `json:"preload_max_load"`

	// CSS / JS
	CSSJSMinify               bool     `json:"css_js_minify"`
	CSSRucss                  bool     `json:"css_rucss"`
	CSSRucssIncludeSelectors  []string `json:"css_rucss_include_selectors"`
	CSSJSSelfHostThirdParty   bool     `json:"css_js_self_host_third_party"`
	JSDelay                   bool     `json:"js_delay"`
	JSDelayMethod             string   `json:"js_delay_method"`
	JSDelayExcludes           []string `json:"js_delay_excludes"`
	JSDelayThirdParty         bool     `json:"js_delay_third_party"`
	JSDelayThirdPartyExcludes []string `json:"js_delay_third_party_excludes"`

	// Fonts
	FontsDisplaySwap    bool `json:"fonts_display_swap"`
	FontsOptimizeGoogle bool `json:"fonts_optimize_google"`
	FontsPreload        bool `json:"fonts_preload"`
	// FontsTranscodeWOFF2 enables server-side TTF/OTF/WOFF → WOFF2 transcoding.
	// When true the agent requests transcode jobs from the CP. Default false.
	FontsTranscodeWOFF2 bool `json:"fonts_transcode_woff2"`
	// FontsSubset enables the subset-WOFF2 path (Phase 2, opt-in, experimental).
	FontsSubset      bool   `json:"fonts_subset"`
	FontsSubsetMode  string `json:"fonts_subset_mode"`
	FontsSubsetRange string `json:"fonts_subset_range"`

	// Media / lazy-load
	LazyLoad           bool     `json:"lazy_load"`
	LazyLoadExclusions []string `json:"lazy_load_exclusions"`
	ProperlySizeImages bool     `json:"properly_size_images"`
	YouTubePlaceholder bool     `json:"youtube_placeholder"`
	SelfHostGravatars  bool     `json:"self_host_gravatars"`

	// CDN
	CDNEnabled        bool               `json:"cdn_enabled"`
	CDNURL            string             `json:"cdn_url"`
	CDNFileTypes      string             `json:"cdn_file_types"`
	CDNProvider       string             `json:"cdn_provider"`
	CDNHasCredentials bool               `json:"cdn_has_credentials"` // read-only
	CDNCredentials    *cdnCredentialsDTO `json:"cdn_credentials,omitempty"`

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

	// Server / install state (read-only, agent-reported)
	ServerSoftware     string `json:"server_software,omitempty"`
	DropinInstalled    bool   `json:"dropin_installed"`
	WPCacheConstantSet bool   `json:"wp_cache_constant_set"`
	HtaccessManaged    bool   `json:"htaccess_managed"`

	// M53 / #169 — WooCommerce cacheable-session.
	// WooCacheableSession is operator read+write (default false).
	WooCacheableSession bool `json:"woo_cacheable_session"`
	// WooThemeFragmentsSupported is agent-reported, read-only from the web.
	// Tri-state: null = never probed, false = probed unsupported, true = probed
	// supported. The CP ignores this field on PUT; the agent is the sole writer.
	WooThemeFragmentsSupported *bool `json:"woo_theme_fragments_supported"`
	// WooFragmentsProbedAt is the RFC3339 timestamp of the last probe. Omitted
	// (null / absent) when never probed. Read-only.
	WooFragmentsProbedAt *string `json:"woo_fragments_probed_at,omitempty"`

	// M56 / RUM — Real User Monitoring settings.
	// RumEnabled and RumSampleRate are operator read+write. BeaconKeySet is
	// read-only (the plaintext key is never returned; the UI uses this boolean
	// to show whether RUM is fully provisioned). MaxDistinctCountries and
	// MinSampleCount are operator-tunable thresholds.
	RumEnabled           bool    `json:"rum_enabled"`
	RumSampleRate        float64 `json:"rum_sample_rate"`
	MaxDistinctCountries int     `json:"max_distinct_countries"`
	MinSampleCount       int     `json:"min_sample_count"`
	BeaconKeySet         bool    `json:"beacon_key_set"` // read-only
	// BeaconKeyAckedPresent is read-only (GH #174): reports whether the
	// agent's most recent config-ack confirmed it holds the beacon key. When
	// BeaconKeySet=true but this is false, RUM is provisioned CP-side but the
	// agent has not (yet) confirmed receiving it — the CP is auto-retrying;
	// an operator can also force it via POST .../perf/rum/rotate-key.
	BeaconKeyAckedPresent bool `json:"beacon_key_acked_present"` // read-only

	ConfigVersion int    `json:"config_version"`
	UpdatedAt     string `json:"updated_at,omitempty"`
}

func toConfigDTO(c Config) perfConfigDTO {
	dto := perfConfigDTO{
		CacheEnabled:               c.CacheEnabled,
		CacheLoggedIn:              c.CacheLoggedIn,
		CacheMobile:                c.CacheMobile,
		CacheRefresh:               c.CacheRefresh,
		CacheRefreshInterval:       c.CacheRefreshInterval,
		CacheLinkPrefetch:          c.CacheLinkPrefetch,
		CacheBypassURLs:            nonNil(c.CacheBypassURLs),
		CacheBypassCookies:         nonNil(c.CacheBypassCookies),
		CacheIncludeQueries:        nonNil(c.CacheIncludeQueries),
		CacheIncludeCookies:        nonNil(c.CacheIncludeCookies),
		PreloadConcurrency:         c.PreloadConcurrency,
		PreloadDelayMs:             c.PreloadDelayMs,
		PreloadBatchSize:           c.PreloadBatchSize,
		PreloadMaxLoad:             c.PreloadMaxLoad,
		CSSJSMinify:                c.CSSJSMinify,
		CSSRucss:                   c.CSSRucss,
		CSSRucssIncludeSelectors:   nonNil(c.CSSRucssIncludeSelectors),
		CSSJSSelfHostThirdParty:    c.CSSJSSelfHostThirdParty,
		JSDelay:                    c.JSDelay,
		JSDelayMethod:              c.JSDelayMethod,
		JSDelayExcludes:            nonNil(c.JSDelayExcludes),
		JSDelayThirdParty:          c.JSDelayThirdParty,
		JSDelayThirdPartyExcludes:  nonNil(c.JSDelayThirdPartyExcludes),
		FontsDisplaySwap:           c.FontsDisplaySwap,
		FontsOptimizeGoogle:        c.FontsOptimizeGoogle,
		FontsPreload:               c.FontsPreload,
		FontsTranscodeWOFF2:        c.FontsTranscodeWOFF2,
		FontsSubset:                c.FontsSubset,
		FontsSubsetMode:            c.FontsSubsetMode,
		FontsSubsetRange:           c.FontsSubsetRange,
		LazyLoad:                   c.LazyLoad,
		LazyLoadExclusions:         nonNil(c.LazyLoadExclusions),
		ProperlySizeImages:         c.ProperlySizeImages,
		YouTubePlaceholder:         c.YouTubePlaceholder,
		SelfHostGravatars:          c.SelfHostGravatars,
		CDNEnabled:                 c.CDNEnabled,
		CDNURL:                     c.CDNURL,
		CDNFileTypes:               c.CDNFileTypes,
		CDNProvider:                c.CDNProvider,
		CDNHasCredentials:          c.CDNHasCredentials,
		DBAutoClean:                c.DBAutoClean,
		DBAutoCleanInterval:        c.DBAutoCleanInterval,
		DBPostRevisions:            c.DBPostRevisions,
		DBPostAutoDrafts:           c.DBPostAutoDrafts,
		DBPostTrashed:              c.DBPostTrashed,
		DBCommentsSpam:             c.DBCommentsSpam,
		DBCommentsTrashed:          c.DBCommentsTrashed,
		DBTransientsExpired:        c.DBTransientsExpired,
		DBOptimizeTables:           c.DBOptimizeTables,
		BloatDisableBlockCSS:       c.BloatDisableBlockCSS,
		BloatDisableDashicons:      c.BloatDisableDashicons,
		BloatDisableEmojis:         c.BloatDisableEmojis,
		BloatDisableJQueryMig:      c.BloatDisableJQueryMig,
		BloatDisableXMLRPC:         c.BloatDisableXMLRPC,
		BloatDisableRSSFeed:        c.BloatDisableRSSFeed,
		BloatDisableOembeds:        c.BloatDisableOembeds,
		BloatHeartbeatControl:      c.BloatHeartbeatControl,
		BloatPostRevisionControl:   c.BloatPostRevisionControl,
		ServerSoftware:             c.ServerSoftware,
		DropinInstalled:            c.DropinInstalled,
		WPCacheConstantSet:         c.WPCacheConstantSet,
		HtaccessManaged:            c.HtaccessManaged,
		WooCacheableSession:        c.WooCacheableSession,
		WooThemeFragmentsSupported: c.WooThemeFragmentsSupported,
		RumEnabled:                 c.RumEnabled,
		RumSampleRate:              c.RumSampleRate,
		MaxDistinctCountries:       c.MaxDistinctCountries,
		MinSampleCount:             c.MinSampleCount,
		BeaconKeySet:               c.BeaconKeySet,
		BeaconKeyAckedPresent:      c.BeaconKeyAckedPresent,
		ConfigVersion:              c.ConfigVersion,
	}
	if !c.UpdatedAt.IsZero() {
		dto.UpdatedAt = c.UpdatedAt.UTC().Format(time.RFC3339)
	}
	if c.WooFragmentsProbedAt != nil {
		s := c.WooFragmentsProbedAt.UTC().Format(time.RFC3339)
		dto.WooFragmentsProbedAt = &s
	}
	return dto
}

func fromConfigDTO(dto perfConfigDTO, tenantID, siteID uuid.UUID) Config {
	return Config{
		TenantID:                  tenantID,
		SiteID:                    siteID,
		CacheEnabled:              dto.CacheEnabled,
		CacheLoggedIn:             dto.CacheLoggedIn,
		CacheMobile:               dto.CacheMobile,
		CacheRefresh:              dto.CacheRefresh,
		CacheRefreshInterval:      dto.CacheRefreshInterval,
		CacheLinkPrefetch:         dto.CacheLinkPrefetch,
		CacheBypassURLs:           dto.CacheBypassURLs,
		CacheBypassCookies:        dto.CacheBypassCookies,
		CacheIncludeQueries:       dto.CacheIncludeQueries,
		CacheIncludeCookies:       dto.CacheIncludeCookies,
		PreloadConcurrency:        dto.PreloadConcurrency,
		PreloadDelayMs:            dto.PreloadDelayMs,
		PreloadBatchSize:          dto.PreloadBatchSize,
		PreloadMaxLoad:            dto.PreloadMaxLoad,
		CSSJSMinify:               dto.CSSJSMinify,
		CSSRucss:                  dto.CSSRucss,
		CSSRucssIncludeSelectors:  dto.CSSRucssIncludeSelectors,
		CSSJSSelfHostThirdParty:   dto.CSSJSSelfHostThirdParty,
		JSDelay:                   dto.JSDelay,
		JSDelayMethod:             dto.JSDelayMethod,
		JSDelayExcludes:           dto.JSDelayExcludes,
		JSDelayThirdParty:         dto.JSDelayThirdParty,
		JSDelayThirdPartyExcludes: dto.JSDelayThirdPartyExcludes,
		FontsDisplaySwap:          dto.FontsDisplaySwap,
		FontsOptimizeGoogle:       dto.FontsOptimizeGoogle,
		FontsPreload:              dto.FontsPreload,
		FontsTranscodeWOFF2:       dto.FontsTranscodeWOFF2,
		FontsSubset:               dto.FontsSubset,
		FontsSubsetMode:           dto.FontsSubsetMode,
		FontsSubsetRange:          dto.FontsSubsetRange,
		LazyLoad:                  dto.LazyLoad,
		LazyLoadExclusions:        dto.LazyLoadExclusions,
		ProperlySizeImages:        dto.ProperlySizeImages,
		YouTubePlaceholder:        dto.YouTubePlaceholder,
		SelfHostGravatars:         dto.SelfHostGravatars,
		CDNEnabled:                dto.CDNEnabled,
		CDNURL:                    dto.CDNURL,
		CDNFileTypes:              dto.CDNFileTypes,
		CDNProvider:               dto.CDNProvider,
		DBAutoClean:               dto.DBAutoClean,
		DBAutoCleanInterval:       dto.DBAutoCleanInterval,
		DBPostRevisions:           dto.DBPostRevisions,
		DBPostAutoDrafts:          dto.DBPostAutoDrafts,
		DBPostTrashed:             dto.DBPostTrashed,
		DBCommentsSpam:            dto.DBCommentsSpam,
		DBCommentsTrashed:         dto.DBCommentsTrashed,
		DBTransientsExpired:       dto.DBTransientsExpired,
		DBOptimizeTables:          dto.DBOptimizeTables,
		BloatDisableBlockCSS:      dto.BloatDisableBlockCSS,
		BloatDisableDashicons:     dto.BloatDisableDashicons,
		BloatDisableEmojis:        dto.BloatDisableEmojis,
		BloatDisableJQueryMig:     dto.BloatDisableJQueryMig,
		BloatDisableXMLRPC:        dto.BloatDisableXMLRPC,
		BloatDisableRSSFeed:       dto.BloatDisableRSSFeed,
		BloatDisableOembeds:       dto.BloatDisableOembeds,
		BloatHeartbeatControl:     dto.BloatHeartbeatControl,
		BloatPostRevisionControl:  dto.BloatPostRevisionControl,
		// M53 / #169: WooCacheableSession is operator-writable; accept it from PUT.
		// WooThemeFragmentsSupported is agent-reported and deliberately NOT read from
		// dto here — the PUT handler must not let an operator write it.
		WooCacheableSession: dto.WooCacheableSession,
		// M56 / RUM: RumEnabled, RumSampleRate, MaxDistinctCountries,
		// MinSampleCount are operator-writable. BeaconKeySet is read-only and
		// deliberately NOT read from dto here — the service manages beacon keys.
		RumEnabled:           dto.RumEnabled,
		RumSampleRate:        dto.RumSampleRate,
		MaxDistinctCountries: dto.MaxDistinctCountries,
		MinSampleCount:       dto.MinSampleCount,
	}
}

// ---------------------------------------------------------------------------
// cache stats DTO
// ---------------------------------------------------------------------------

type cacheStatsDTO struct {
	CachedPagesCount int    `json:"cached_pages_count"`
	CacheSizeBytes   int64  `json:"cache_size_bytes"`
	LastPurgedAt     string `json:"last_purged_at,omitempty"`
	LastPurgeKind    string `json:"last_purge_kind,omitempty"`
	LastPreloadAt    string `json:"last_preload_at,omitempty"`
	PreloadPending   int    `json:"preload_pending"`
	PreloadTotal     int    `json:"preload_total"`
	ReportedAt       string `json:"reported_at,omitempty"`
}

func toCacheStatsDTO(s CacheStats) cacheStatsDTO {
	dto := cacheStatsDTO{
		CachedPagesCount: s.CachedPagesCount,
		CacheSizeBytes:   s.CacheSizeBytes,
		LastPurgeKind:    s.LastPurgeKind,
		PreloadPending:   s.PreloadPending,
		PreloadTotal:     s.PreloadTotal,
	}
	if s.LastPurgedAt != nil {
		dto.LastPurgedAt = s.LastPurgedAt.UTC().Format(time.RFC3339)
	}
	if s.LastPreloadAt != nil {
		dto.LastPreloadAt = s.LastPreloadAt.UTC().Format(time.RFC3339)
	}
	if !s.ReportedAt.IsZero() {
		dto.ReportedAt = s.ReportedAt.UTC().Format(time.RFC3339)
	}
	return dto
}

// ---------------------------------------------------------------------------
// request bodies
// ---------------------------------------------------------------------------

// purgeBody is the POST /cache/purge request.
type purgeBody struct {
	Scope            string   `json:"scope"` // "all" | "url"
	URL              string   `json:"url,omitempty"`
	URLs             []string `json:"urls,omitempty"`
	DeleteEverything bool     `json:"delete_everything,omitempty"`
}

type bulkPurgeBody struct {
	SiteIDs []string `json:"site_ids"`
}

type bulkConfigBody struct {
	SiteIDs []string `json:"site_ids"`
	Preset  string   `json:"preset"`
}

type bulkResultDTO struct {
	SiteID        string `json:"site_id"`
	OK            bool   `json:"ok"`
	Detail        string `json:"detail"`
	ConfigVersion int    `json:"config_version,omitempty"`
}

// FontResultDTO is one font_results row in the operator dashboard list.
type FontResultDTO struct {
	ID           string  `json:"id"`
	SourceHash   string  `json:"source_hash"`
	Family       string  `json:"family,omitempty"`
	SourceFile   string  `json:"source_file,omitempty"`
	OriginalExt  string  `json:"original_ext,omitempty"`
	OriginalSize int     `json:"original_size"`
	Woff2Size    int     `json:"woff2_size,omitempty"`
	SubsetSize   int     `json:"subset_size,omitempty"`
	UnicodeRange string  `json:"unicode_range,omitempty"`
	State        string  `json:"state"`
	ErrorDetail  string  `json:"error_detail,omitempty"`
	SavingsPct   float64 `json:"savings_pct"`
	UpdatedAt    string  `json:"updated_at,omitempty"`
}

// ToFontResultDTO converts a domain FontResult to its wire shape.
func ToFontResultDTO(r FontResult) FontResultDTO {
	dto := FontResultDTO{
		ID:           r.ID.String(),
		SourceHash:   r.SourceHash,
		Family:       r.Family,
		SourceFile:   r.SourceFile,
		OriginalExt:  r.OriginalExt,
		OriginalSize: r.OriginalSize,
		Woff2Size:    r.Woff2Size,
		SubsetSize:   r.SubsetSize,
		UnicodeRange: r.UnicodeRange,
		State:        string(r.State),
		ErrorDetail:  r.ErrorDetail,
		SavingsPct:   r.SavingsPct,
	}
	if !r.UpdatedAt.IsZero() {
		dto.UpdatedAt = r.UpdatedAt.UTC().Format(time.RFC3339)
	}
	return dto
}

// RucssResultDTO is one cached RUCSS result row in the operator results list.
type RucssResultDTO struct {
	ID            string  `json:"id"`
	StructureHash string  `json:"structure_hash"`
	URL           string  `json:"url,omitempty"`
	OriginalBytes int     `json:"original_css_bytes"`
	UsedBytes     int     `json:"used_css_bytes"`
	ReductionPct  float64 `json:"reduction_pct"`
	S3Key         string  `json:"used_css_s3_key"`
	LastUsedAt    string  `json:"last_used_at,omitempty"`
}

// ---------------------------------------------------------------------------
// presets (bulk-config)
// ---------------------------------------------------------------------------

// preset is a small set of toggles a bulk-config apply spreads onto each site's
// existing config without clobbering per-site lists.
type preset struct {
	CacheEnabled bool
	CSSJSMinify  bool
	CSSRucss     bool
	JSDelay      bool
	LazyLoad     bool
}

func presetConfig(name string) (preset, bool) {
	switch name {
	case "safe":
		return preset{CacheEnabled: true, CSSJSMinify: true, LazyLoad: true}, true
	case "balanced":
		return preset{CacheEnabled: true, CSSJSMinify: true, CSSRucss: true, LazyLoad: true}, true
	case "aggressive":
		return preset{CacheEnabled: true, CSSJSMinify: true, CSSRucss: true, JSDelay: true, LazyLoad: true}, true
	default:
		return preset{}, false
	}
}

func applyPreset(c *Config, p preset) {
	c.CacheEnabled = p.CacheEnabled
	c.CSSJSMinify = p.CSSJSMinify
	c.CSSRucss = p.CSSRucss
	c.JSDelay = p.JSDelay
	c.LazyLoad = p.LazyLoad
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// ---------------------------------------------------------------------------
// RUM read DTOs (M56 Phase 3a)
// ---------------------------------------------------------------------------

// RumDistribution carries the good / needs-improvement / poor sample-count
// breakdown for one metric slice. Counts are raw (pre-scale) pageviews; Pct
// fields are rounded integers guaranteed to sum to exactly 100 (Hamilton /
// largest-remainder rounding). Distribution is nil (omitted) when the slice
// is suppressed (SampleCount < min_sample_count).
//
// CLS distribution caveat: the CrUX histogram boundaries start at 200 ms-units
// while the CLS "good" threshold is 100 milli-units and "NI" is 250 milli-units.
// The entire first bucket [0, 200) straddles both boundaries, so it is assigned
// wholesale to "good" per the straddle-to-lower-band rule. This means CLS
// distribution is coarse for V1: the "good" band is slightly over-counted and
// "needs_improvement" slightly under-counted relative to the true split within
// [0, 200). The effect is minor in practice (most CLS values are near 0) and
// will be refined in a future iteration using sub-200 histogram resolution.
type RumDistribution struct {
	Good                int64 `json:"good"`
	NeedsImprovement    int64 `json:"needs_improvement"`
	Poor                int64 `json:"poor"`
	GoodPct             int   `json:"good_pct"`
	NeedsImprovementPct int   `json:"needs_improvement_pct"`
	PoorPct             int   `json:"poor_pct"`
}

// foldBucketsIntoDistribution folds a NumBuckets-length bucket_counts array
// into three bands (good / needs-improvement / poor) using the official CWV
// thresholds for the given metric.
//
// Fold rule: a bucket is assigned to the band whose upper bound first reaches
// or exceeds the band boundary. Concretely, a bucket whose ENTIRE value range
// is ≤ good-threshold goes to "good"; one entirely within (good, NI] goes to
// "needs_improvement"; otherwise "poor". A bucket that straddles a threshold
// (i.e. its lower bound is below the threshold but its upper bound is above) is
// assigned to the LOWER band (conservative/PSI-compatible behaviour). For
// LCP/FCP/TTFB/INP the thresholds coincide exactly with CrUX bucket boundaries,
// so no bucket straddles and the fold is exact. For CLS (thresholds 100/250) the
// first bucket [0,200) straddles both 100 and 250; it is assigned to "good".
// See the RumDistribution comment for the full CLS coarseness caveat.
//
// Percentages are computed with Hamilton (largest-remainder) rounding so that
// good_pct + needs_improvement_pct + poor_pct == 100 exactly, even in the
// presence of rounding ties.
func foldBucketsIntoDistribution(metric string, counts []int64) *RumDistribution {
	goodUpper, niUpper := cwvThresholds(metric)

	var good, ni, poor int64
	for i, c := range counts {
		if c == 0 {
			continue
		}
		// Lower bound (inclusive) of this bucket.
		var bucketLower int32
		if i > 0 {
			bucketLower = rum.CrUXBuckets[i-1]
		}
		// Assign by the bucket's lower bound. The straddle-to-lower-band rule is
		// implicit: if the lower bound is below the threshold but the upper bound
		// crosses it, the bucket's lower bound still places it in the lower band.
		// For LCP/FCP/TTFB/INP the thresholds align with bucket boundaries exactly,
		// so no bucket straddles in practice. For CLS (thresholds 100/250) the first
		// bucket [0,200) has lower=0 which is < 100, so it correctly lands in "good".
		switch {
		case bucketLower < int32(goodUpper):
			good += c
		case bucketLower < int32(niUpper):
			ni += c
		default:
			poor += c
		}
	}

	total := good + ni + poor
	d := &RumDistribution{
		Good:             good,
		NeedsImprovement: ni,
		Poor:             poor,
	}
	if total > 0 {
		d.GoodPct, d.NeedsImprovementPct, d.PoorPct = hamiltonRound3(good, ni, poor, total)
	}
	return d
}

// cwvThresholds returns the (goodUpperInclusive, niUpperInclusive) for a metric
// in the same integer unit as the histogram (ms for all; milli-units for CLS).
// These are the same constants as cwvRating — kept here to avoid any divergence.
func cwvThresholds(metric string) (goodUpper int, niUpper int) {
	switch metric {
	case "lcp":
		return 2500, 4000
	case "inp":
		return 200, 500
	case "cls":
		// CLS histogram is in milli-units (value * 1000); thresholds in milli-units.
		return 100, 250
	case "fcp":
		return 1800, 3000
	case "ttfb":
		return 800, 1800
	}
	// Unknown metric: treat entire range as "good" (no threshold to fold on).
	return 1 << 30, 1 << 30
}

// hamiltonRound3 rounds three proportions (a/total, b/total, c/total) to
// integers that sum to 100, using the Hamilton (largest-remainder) method.
// This guarantees good_pct + ni_pct + poor_pct == 100 regardless of rounding.
func hamiltonRound3(a, b, c, total int64) (aPct, bPct, cPct int) {
	if total == 0 {
		return 0, 0, 0
	}
	// Compute exact floats and take the floor.
	fa := float64(a) * 100.0 / float64(total)
	fb := float64(b) * 100.0 / float64(total)
	fc := float64(c) * 100.0 / float64(total)

	ia, ib, ic := int(fa), int(fb), int(fc)
	rem := 100 - ia - ib - ic // how many units to distribute (0, 1, or 2)

	// Distribute remainders to the slots with the largest fractional parts.
	ra := fa - float64(ia)
	rb := fb - float64(ib)
	rc := fc - float64(ic)

	type slot struct {
		idx  int
		frac float64
	}
	slots := [3]slot{{0, ra}, {1, rb}, {2, rc}}
	// Sort by descending fractional part (simple insertion sort for 3 elements).
	for i := 1; i < 3; i++ {
		for j := i; j > 0 && slots[j].frac > slots[j-1].frac; j-- {
			slots[j], slots[j-1] = slots[j-1], slots[j]
		}
	}
	vals := [3]int{ia, ib, ic}
	for k := 0; k < rem; k++ {
		vals[slots[k].idx]++
	}
	return vals[0], vals[1], vals[2]
}

// RumMetricSummary is the p75 summary for one metric in one device/country slice.
// Suppressed is true when the sample count is below the min_sample_count floor;
// in that case P75Ms is 0 and the UI renders "insufficient samples (SampleCount
// of MinNeeded)" rather than a number. This mirrors the CrUX suppression model.
type RumMetricSummary struct {
	Metric      string  `json:"metric"`       // lcp | inp | cls | ttfb | fcp
	Device      string  `json:"device"`       // desktop | mobile | tablet | all
	Country     string  `json:"country"`      // ISO-3166-1 alpha-2 or "__other__"
	P75Ms       float64 `json:"p75_ms"`       // 0 when Suppressed
	SampleCount int64   `json:"sample_count"` // raw (pre-scale) count
	// Rating is the CWV standard rating band: "good" | "needs_improvement" | "poor".
	// Empty when Suppressed is true or when the metric has no official threshold.
	Rating string `json:"rating,omitempty"`
	// Suppressed is true when SampleCount < min_sample_count. The dashboard must
	// render "insufficient samples" rather than a p75 in this state.
	Suppressed bool `json:"suppressed"`
	// Distribution is the good/needs-improvement/poor breakdown for this metric
	// slice. Nil (omitted) when the slice is suppressed (same floor as P75Ms).
	Distribution *RumDistribution `json:"distribution,omitempty"`
}

// RumSummaryDTO is the response shape for GET /perf/rum/summary. It carries
// site-level Core Web Vitals p75s over the requested window, with per-device
// and per-country breakdowns. Any slice below the min_sample_count floor is
// returned with Suppressed=true and P75Ms=0.
type RumSummaryDTO struct {
	// WindowDays is the number of days covered by this summary.
	WindowDays int `json:"window_days"`
	// MinSampleCount is the site's configured floor (from perf config).
	MinSampleCount int `json:"min_sample_count"`
	// Metrics is the flat list of p75 results by (metric, device, country).
	Metrics []RumMetricSummary `json:"metrics"`
}

// RumResultDTO is one per-URL/metric/device p75 breakdown row for the dashboard
// table. Suppressed carries the same semantics as in RumMetricSummary.
type RumResultDTO struct {
	URLPattern  string  `json:"url_pattern"`
	Metric      string  `json:"metric"`
	Device      string  `json:"device"`
	Country     string  `json:"country"`
	P75Ms       float64 `json:"p75_ms"`
	SampleCount int64   `json:"sample_count"`
	Rating      string  `json:"rating,omitempty"`
	Suppressed  bool    `json:"suppressed"`
}

// ---------------------------------------------------------------------------
// RUM trend DTOs (dashboard redesign)
// ---------------------------------------------------------------------------

// RumTrendDayPoint is one day's p75 in a metric trend series.
// CLS p75_ms is in milli-units (raw value * 1000); the client divides by 1000
// for display, matching the existing FleetRumPanel formatP75 convention.
// When Suppressed is true, P75Ms is 0 and the client should render a GAP on
// the trend line for that day (not a zero). The day entry is always included so
// the client has a consistent X-axis across all metrics.
type RumTrendDayPoint struct {
	Day         string  `json:"day"`    // "YYYY-MM-DD"
	P75Ms       float64 `json:"p75_ms"` // 0 when Suppressed
	SampleCount int64   `json:"sample_count"`
	Rating      string  `json:"rating"` // "good" | "needs-improvement" | "poor" | ""
	Suppressed  bool    `json:"suppressed"`
}

// RumTrendResponse is the shape for GET /perf/rum/trend. Each metric key maps
// to a slice of daily p75 points ordered ascending by day. Days with no rollup
// row at all are omitted (the client is only given days we actually have data
// for). Days with insufficient samples appear as suppressed=true, p75_ms=0
// entries so the client can render a GAP on the trend line.
type RumTrendResponse struct {
	WindowDays     int                           `json:"window_days"`
	MinSampleCount int                           `json:"min_sample_count"`
	Metrics        map[string][]RumTrendDayPoint `json:"metrics"`
}

// cwvRating returns the CWV standard band for a p75 millisecond value.
// Thresholds are the official web-vitals constants (same as the tracker JS).
// Returns "" for metrics with no official threshold (none currently).
func cwvRating(metric string, p75Ms float64) string {
	switch metric {
	case "lcp":
		if p75Ms <= 2500 {
			return "good"
		} else if p75Ms <= 4000 {
			return "needs_improvement"
		}
		return "poor"
	case "inp":
		if p75Ms <= 200 {
			return "good"
		} else if p75Ms <= 500 {
			return "needs_improvement"
		}
		return "poor"
	case "cls":
		// CLS is stored as milli-units (value x 1000) to share integer machinery.
		// p75Ms is therefore cls_value * 1000; thresholds are 0.1 and 0.25 raw,
		// i.e. 100 and 250 in the milli-unit representation.
		if p75Ms <= 100 {
			return "good"
		} else if p75Ms <= 250 {
			return "needs_improvement"
		}
		return "poor"
	case "fcp":
		if p75Ms <= 1800 {
			return "good"
		} else if p75Ms <= 3000 {
			return "needs_improvement"
		}
		return "poor"
	case "ttfb":
		if p75Ms <= 800 {
			return "good"
		} else if p75Ms <= 1800 {
			return "needs_improvement"
		}
		return "poor"
	}
	return ""
}
