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

	ConfigVersion int
	CreatedAt     time.Time
	UpdatedAt     time.Time
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
