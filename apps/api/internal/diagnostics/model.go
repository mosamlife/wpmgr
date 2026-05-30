// Package diagnostics holds the ADR-037 Sprint 2 "site health" domain on the
// control-plane side. The agent ships:
//
//   - A 14-category diagnostics blob via POST /agent/v1/diagnostics. We persist
//     one row per (site, category) keyed by `category`; the agent overwrites
//     the row each time it ships.
//   - A batch of fingerprint-deduped PHP errors via POST /agent/v1/errors. We
//     upsert by (site, md5) and increment occurrence_count on conflict.
//
// Operators read:
//
//   - GET /api/v1/sites/{siteId}/diagnostics — latest row per category +
//     freshness timestamps (the Health tab renders 9 cards).
//   - GET /api/v1/sites/{siteId}/errors — fingerprint-grouped table.
//   - POST /api/v1/sites/{siteId}/errors/{md5}/silence — silence toggle.
//   - POST /api/v1/sites/{siteId}/diagnostics/refresh — enqueue an on-demand
//     diagnostics command to the agent.
package diagnostics

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Category is one of the 14 diagnostics buckets the agent ships. Strings
// match the agent's JSON keys verbatim.
type Category string

const (
	CategoryIdentity    Category = "identity"
	CategoryPHP         Category = "php"
	CategoryMySQL       Category = "mysql"
	CategoryFilesystem  Category = "filesystem"
	CategoryHTTP        Category = "http"
	CategoryCron        Category = "cron"
	CategoryThemes      Category = "themes"
	CategoryPlugins     Category = "plugins"
	CategoryUsers       Category = "users"
	CategorySecurity    Category = "security"
	CategoryHTTPS       Category = "https"
	CategoryMail        Category = "mail"
	CategoryPerformance Category = "performance"
	CategoryHosting     Category = "hosting"
	// CategoryWPNative is the verbatim WP_Debug_Data::debug_data() dump (the
	// full Site Health > Info screen), shipped by the Site-Health-Full agent
	// build (v0.9.14+). The payload is a multi-section structure shaped like
	//   { "wp-core": { "label": "...", "fields": {...} }, ... }
	// with one section per WP_Debug_Data category (wp-core, wp-paths-sizes,
	// wp-dropins, wp-active-theme, wp-parent-theme, wp-themes-inactive,
	// wp-mu-plugins, wp-plugins-active, wp-plugins-inactive, wp-media,
	// wp-server, wp-database, wp-constants, wp-filesystem) plus any
	// `debug_information` filter contributions from third-party plugins
	// (Yoast SEO, WooCommerce, ACF, etc.). Stored as JSONB on the existing
	// agent_diagnostics row keyed by (tenant, site, "wp_native").
	CategoryWPNative Category = "wp_native"
)

// AllCategories lists every category we expect the agent to populate. Used by
// the operator GET handler to emit a "card" entry per category even when the
// agent has never shipped a payload for it (so the UI can render an "awaiting
// first sync" placeholder rather than silently omitting the card).
func AllCategories() []Category {
	return []Category{
		CategoryIdentity, CategoryPHP, CategoryMySQL, CategoryFilesystem,
		CategoryHTTP, CategoryCron, CategoryThemes, CategoryPlugins,
		CategoryUsers, CategorySecurity, CategoryHTTPS, CategoryMail,
		CategoryPerformance, CategoryHosting, CategoryWPNative,
	}
}

// Diagnostic is one stored row — the latest payload the agent shipped for the
// given (site, category).
type Diagnostic struct {
	ID          uuid.UUID
	TenantID    uuid.UUID
	SiteID      uuid.UUID
	Category    Category
	Payload     json.RawMessage // the category's JSON sub-blob, as the agent shipped it
	CollectedAt time.Time       // agent-side collection time (Unix seconds reported)
	ReceivedAt  time.Time       // CP-side ingestion time
}

// ErrorFrame is one frame in a PHP backtrace. Frame order is most-recent-call-
// first (the agent ships them that way; we store and expose them in that order).
type ErrorFrame struct {
	File     string `json:"file"`
	Line     int    `json:"line"`
	Function string `json:"function"`
}

// PHPError is one stored fingerprint-deduped error.
type PHPError struct {
	ID              uuid.UUID
	TenantID        uuid.UUID
	SiteID          uuid.UUID
	MD5             string
	Code            int
	Severity        string
	Message         string
	File            string
	Line            int
	RequestPath     string
	FirstSeenAt     time.Time
	LastSeenAt      time.Time
	OccurrenceCount int64
	Silenced        bool
	Backtrace       []ErrorFrame
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// ErrorConfig is the per-site PHP error reporting configuration stored in
// site_error_config and pushed to the agent via the sync_error_config command.
//
// ErrorLevel is the PHP E_* bitmask (e.g. 6143 = E_ALL & ~E_STRICT, the WP
// default). IgnoreMD5s is the ordered list of md5 fingerprints the agent must
// suppress without counting or reporting. An empty list clears all suppression.
type ErrorConfig struct {
	TenantID   uuid.UUID
	SiteID     uuid.UUID
	ErrorLevel int
	IgnoreMD5s []string
	UpdatedAt  time.Time
}

// ValidCategory reports whether c is one of the known buckets (14 legacy
// WPMgr-extra categories + the v0.9.14 `wp_native` full Site-Health dump).
func ValidCategory(c Category) bool {
	switch c {
	case CategoryIdentity, CategoryPHP, CategoryMySQL, CategoryFilesystem,
		CategoryHTTP, CategoryCron, CategoryThemes, CategoryPlugins,
		CategoryUsers, CategorySecurity, CategoryHTTPS, CategoryMail,
		CategoryPerformance, CategoryHosting, CategoryWPNative:
		return true
	}
	return false
}
