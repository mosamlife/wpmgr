// Package site implements the site domain: WordPress sites managed under a
// tenant. Every query is tenant-scoped both explicitly (tenant_id in the WHERE
// clause) and by Postgres RLS (the app.tenant_id policy), giving
// defense-in-depth against cross-tenant access.
//
// M2 adds agent enrollment (pairing codes + /enroll), agent-pushed metadata,
// connection-health tracking, and site tags.
package site

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// Site is a managed WordPress site.
type Site struct {
	ID         uuid.UUID
	TenantID   uuid.UUID
	URL        string
	Name       string
	Status     string
	WPVersion  string
	PHPVersion string
	// M27 — current WPMgr agent plugin version (last-synced via metadata push).
	AgentVersion string
	// M2 enrollment + agent identity.
	AgentPublicKey string
	EnrolledAt     *time.Time
	LastSeenAt     *time.Time
	HealthStatus   string
	// M2 metadata.
	ServerInfo  string
	Multisite   bool
	ActiveTheme string
	Components  []byte // JSONB inventory of installed plugins/themes
	Tags        []string
	// AgeRecipient is the per-site age PUBLIC recipient backups are encrypted to
	// (client-side, on the agent). The control plane never holds the identity.
	AgeRecipient string
	// WpTimezone is the IANA timezone name from the site's WordPress settings
	// (captured by the agent's diagnostics identity category). Empty when
	// diagnostics have not yet been ingested.
	WpTimezone string
	// WpGmtOffset is the site's GMT offset in fractional hours (e.g. 5.5 for
	// +05:30). Used as a fallback when WpTimezone is empty.
	WpGmtOffset float64
	// HostProvider (M28) is the inferred hosting/infrastructure provider name
	// (e.g. "DigitalOcean", "Hetzner", "AWS"), derived CP-side from the agent's
	// observed public egress IP via an offline ASN lookup. Empty when no
	// diagnostics push has landed yet, or when the network could not be
	// confidently attributed. A best-effort hint: a positive agent HostFlag
	// (managed-host detection) always takes precedence over this value.
	HostProvider string
	// HostProviderOrg (M29) is the raw Autonomous System organization string for
	// the inferred network (e.g. "Hetzner Online GmbH"). Used as a display
	// fallback when HostProvider (the canonical mapped name) is empty but the
	// network is still known. HostProviderIP is the IP the inference used,
	// surfaced to the site's own operators for debugging.
	HostProviderOrg string
	HostProviderIP  string
	// M21 connection lifecycle (ADR-041). ConnectionState is the single source of
	// truth for the agent connection; the legacy Status/HealthStatus columns are
	// kept in sync but only ConnectionState drives the lifecycle UI.
	ConnectionState      ConnectionState
	ConnectionGeneration int32
	DisconnectedAt       *time.Time
	DisconnectedReason   string
	ArchivedAt           *time.Time
	CreatedAt            time.Time
	UpdatedAt            time.Time
	// Derived for the sites list (NOT stored on the site row): the most-recent
	// backup snapshot's normalized status (success|running|failed) + time. Empty/
	// nil when the site has no backups. Populated by repo.List's batched lookup.
	LastBackupStatus string
	LastBackupAt     *time.Time
	// m63 — agency client assignment. ClientID is nil when unassigned;
	// ClientName is populated in the list by a batched JOIN lookup (same pattern
	// as LastBackupStatus). Absent on detail (not needed by the detail view).
	ClientID   *uuid.UUID
	ClientName string
	// M72 — site screenshot. Populated by repo.List's batched lookup (same
	// pattern as LastBackupStatus). Nil when the site has no screenshot row yet.
	// Never nil after the first capture is enqueued (status=pending).
	ScreenshotURL      *string // presigned GCS GET URL for the 1x WebP; nil when not ready
	ScreenshotURL2x    *string // presigned GCS GET URL for the 2x WebP; nil when absent
	ScreenshotStatus   *string // "pending"|"ready"|"failed"; nil = never captured
	ScreenshotCapturedAt *time.Time
	ScreenshotFailedReason *string
	// Uptime summary — populated by repo.List's batched raw-SQL lookup (same
	// pattern as LastBackupStatus, but using raw SQL to handle nullable probe
	// columns that sqlc cannot generate correct types for).
	// Nil/zero when no probes exist yet for this site.
	UptimePct30d   *float64   // 0–100, rounded to 2 decimal places; nil = no probes
	UptimeUp       *bool      // current up/down from the most-recent probe; nil = never probed
	AvgLatencyMs   *float64   // average total_ms over successful probes in the 30d window
	TLSExpiresAt   *time.Time // cert expiry from the most-recent probe; nil = non-HTTPS or no probes
	// GH #243 — the real drop-in config state, populated by repo.Get/List via a
	// PK-keyed LEFT JOIN onto site_perf_config.cache_enabled and
	// site_object_cache_config.enabled (both one-row-per-site, site_id PK).
	// false both when a config row exists with the feature off AND when no
	// config row exists yet (the site has never touched that feature). This
	// replaces the old site-card capability dots, which inferred state from
	// plugin slugs that can never exist — both features ship as drop-ins.
	PageCacheEnabled   bool
	ObjectCacheEnabled bool
	// GH #414 — monitoring pause state, read straight off the sites row (m117).
	// MonitoringPausedAt nil means monitoring is ACTIVE; the flag and the
	// since-when are one column so they cannot disagree. MonitoringPausedBy can
	// be nil on a paused site: the FK is ON DELETE SET NULL, so a pause outlives
	// the user who set it.
	MonitoringPausedAt     *time.Time
	MonitoringPausedBy     *uuid.UUID
	MonitoringPausedReason string
	MonitoringResumeAt     *time.Time
	// HealthCheckedAt is site_uptime_status.last_probed_at: the last uptime
	// probe that actually RAN against this site, which is the "as of" for
	// HealthStatus. Nil when the site has never been probed.
	//
	// This exists because pause freezes HealthStatus. The uptime prober is what
	// refreshes health_status (uptime/worker.go processSite), and GH #414 Phase 2
	// filters paused sites out of the probe enumeration — so a paused site keeps
	// serving its last health_status forever while this stamp stops advancing.
	// Serving the verdict without its age is the "lie to me" failure the whole
	// feature is built to avoid, so the two travel together.
	//
	// NOT UpdatedAt. sites.updated_at is the row mtime — heartbeats, metadata
	// pushes and operator writes all move it — so it says nothing about when the
	// health verdict was last confirmed. Populated by repo.Get/List from
	// site_uptime_status (PK-keyed, inside the same RLS-scoped tx).
	HealthCheckedAt *time.Time
	// ComponentsUpdatedAt is sites.components_updated_at (m121, GH #553): the
	// control-plane write instant of the last UpdateSiteMetadata call, i.e. the
	// age of the Components inventory. Nil means the inventory has never been
	// collected — every pre-m121 row, and any row that has never synced.
	//
	// NOT UpdatedAt. sites.updated_at is bumped by the 60s heartbeat
	// (TouchSiteHeartbeat), which never touches Components, so it can only ever
	// overstate inventory freshness. This is the field getAvailableUpdates' as_of
	// must read; falling back to UpdatedAt when this is nil silently recreates
	// GH #553.
	ComponentsUpdatedAt *time.Time
}

// CreateInput is the validated input for creating a site under a tenant.
type CreateInput struct {
	TenantID   uuid.UUID `validate:"required"`
	URL        string    `validate:"required,url,max=2048"`
	Name       string    `validate:"required,max=200"`
	Status     string    `validate:"omitempty,oneof=pending active error disabled"`
	WPVersion  string    `validate:"max=32"`
	PHPVersion string    `validate:"max=32"`
}

// ScopedPrincipal is satisfied by domain.Principal (and test doubles). It is
// defined here to avoid a circular import: site cannot import domain directly
// for an interface that db also uses, but the interface is small enough to
// repeat here. The values are used by repo.List to choose between InTenantTx
// (org-scoped) and RunTenantTx (site-scoped, activates restrictive RLS).
type ScopedPrincipal interface {
	GetScope() string
	GetUserID() uuid.UUID
	GetTenantID() uuid.UUID
	GetAllowedSiteIDs() []uuid.UUID
}

// ListInput is tenant-scoped pagination input, optionally filtered by tag.
// Principal, when non-nil, is used by the repo to choose the correct
// transaction wrapper (InTenantTx for org-scoped, InScopedTenantTx for
// site-scoped) so the RESTRICTIVE RLS policy filters the result to only
// the sites the principal is allowed to see.
//
// M100 (GH #230 "rich tags"): AnyTags (tags && ...) and AllTags (tags @> ...)
// replace the single Tag filter. The handler maps exactly one of them per
// request — the legacy ?tag= query param becomes AnyTags=[tag].
type ListInput struct {
	TenantID uuid.UUID
	// AnyTags, when non-empty, filters to sites carrying AT LEAST ONE of these
	// tags (tags && any_tags). Mutually exclusive with AllTags in practice
	// (the handler sends only one), but both may be set — both predicates are
	// simply AND-combined.
	AnyTags []string
	// AllTags, when non-empty, filters to sites carrying EVERY one of these
	// tags (tags @> all_tags).
	AllTags []string
	// State, when non-empty, filters to exactly that connection_state (e.g.
	// "archived" for the archived chip). When empty the list hides archived
	// sites (the ADR-041 default).
	State     string
	Limit     int32
	Offset    int32
	Principal ScopedPrincipal // optional; nil → plain InTenantTx (org-scoped)
	// ClientID, when set, filters to sites assigned to that client (m63).
	ClientID *uuid.UUID
	// Query (GH #349) is the operator's free-text search. When non-blank the
	// list is filtered, IN THE DATABASE, to sites whose name, url or any tag
	// contains it (case-insensitive substring). Blank or whitespace-only means
	// "no search" and is indistinguishable from absent.
	//
	// This is deliberately server side. The web previously filtered a page it
	// had already fetched, so a tenant with more sites than one page searched
	// only the newest page's worth and was told "no results" for sites it owns.
	Query string
	// Sort (GH #349) is the requested ordering, as the raw wire value
	// ("name", "-name", "created_at", "-created_at", "last_seen",
	// "-last_seen"; leading "-" is descending). Empty means the historical
	// default, DefaultListSort. Service.List validates it via ParseListSort and
	// returns a 422 for anything else: an ignored sort would show the operator
	// a list ordered differently from the control they just set.
	Sort string
}

// ListSort is the closed set of orderings GET /sites accepts (GH #349).
//
// Keeping this a closed set in Go is what makes the ordering safe: the value
// is bound into the query as a PARAMETER and compared against fixed literals
// inside CASE expressions (see db/query/sites.sql), so no request text ever
// reaches the SQL string, and an unknown value is rejected rather than
// silently ignored.
type ListSort string

const (
	// SortNameAsc orders by site name, A to Z, case-insensitively.
	SortNameAsc ListSort = "name"
	// SortNameDesc orders by site name, Z to A, case-insensitively.
	SortNameDesc ListSort = "-name"
	// SortCreatedAsc orders by date added, oldest first.
	SortCreatedAsc ListSort = "created_at"
	// SortCreatedDesc orders by date added, newest first. This is the
	// historical (and default) ordering of GET /sites.
	SortCreatedDesc ListSort = "-created_at"
	// SortLastSeenAsc orders by last agent check-in, oldest first. Sites that
	// have never checked in sort LAST (see DefaultListSort's doc for why).
	SortLastSeenAsc ListSort = "last_seen"
	// SortLastSeenDesc orders by last agent check-in, most recent first. Sites
	// that have never checked in sort LAST here too.
	SortLastSeenDesc ListSort = "-last_seen"
)

// DefaultListSort is the ordering used when the client sends no sort. It is
// the ordering GET /sites has always had, so adding the parameter cannot
// change what an existing client sees.
//
// NULL last_seen_at (a site enrolled but never checked in) sorts LAST in BOTH
// last_seen directions. Ascending, that reads as "oldest contact first, and
// the ones we have never heard from at the very end"; descending, it keeps
// never-seen sites from occupying the top of a "most recently seen" list.
// Either way they stay in the result and stay findable.
const DefaultListSort = SortCreatedDesc

// listSorts is the accept-set. Order here is the order quoted back in the
// validation message.
var listSorts = []ListSort{
	SortNameAsc, SortNameDesc,
	SortCreatedAsc, SortCreatedDesc,
	SortLastSeenAsc, SortLastSeenDesc,
}

// ParseListSort maps a raw ?sort= value to a ListSort. An empty (or
// whitespace-only) value yields DefaultListSort. Anything else that is not in
// the accept-set is a validation error (HTTP 422), never a silent fallback.
func ParseListSort(raw string) (ListSort, error) {
	s := ListSort(strings.TrimSpace(raw))
	if s == "" {
		return DefaultListSort, nil
	}
	for _, allowed := range listSorts {
		if s == allowed {
			return s, nil
		}
	}
	names := make([]string, 0, len(listSorts))
	for _, allowed := range listSorts {
		names = append(names, string(allowed))
	}
	return "", domain.Validation("invalid_sort",
		"sort must be one of: "+strings.Join(names, ", "))
}

// normalizeListSort is the repo-side backstop for callers that reach the repo
// without going through Service.List (health jobs, tests). It never errors:
// the 422 for a bad value belongs to the service, and a repo that received a
// value it does not recognise should still produce a deterministic order
// rather than an arbitrary one.
func normalizeListSort(raw string) ListSort {
	s, err := ParseListSort(raw)
	if err != nil {
		return DefaultListSort
	}
	return s
}

// SetTagsInput sets the full tag set on a tenant-scoped site.
type SetTagsInput struct {
	TenantID uuid.UUID
	SiteID   uuid.UUID
	Tags     []string `validate:"max=50,dive,min=1,max=64"`
}

// Component is one installed plugin or theme reported by the agent.
// AvailableUpdate (when set) carries the per-item update advisory. The JSONB
// inventory column stores Component as-is, so the optional advisory is
// persisted/round-tripped without a schema migration.
type Component struct {
	Slug            string           `json:"slug" validate:"required,max=200"`
	Name            string           `json:"name" validate:"max=200"`
	Version         string           `json:"version" validate:"max=64"`
	Active          bool             `json:"active"`
	AvailableUpdate *AvailableUpdate `json:"available_update,omitempty"`
	// ADR-037 Sprint 1, 1C — sparse-metadata expansion. Plugin-header URIs +
	// Network flag. All optional; omitempty keeps the persisted JSON minimal.
	// Old agents send none of these; new agents may send any subset.
	PluginURI string `json:"plugin_uri,omitempty" validate:"max=2048"`
	UpdateURI string `json:"update_uri,omitempty" validate:"max=2048"`
	AuthorURI string `json:"author_uri,omitempty" validate:"max=2048"`
	Network   bool   `json:"network,omitempty"`
}

// AvailableUpdate is the optional per-item available-update advisory recorded
// alongside each Component in the JSONB inventory. omitempty everywhere keeps
// the encoded shape minimal when the field is unset.
type AvailableUpdate struct {
	NewVersion  string `json:"new_version" validate:"max=64"`
	Package     string `json:"package,omitempty" validate:"max=2048"`
	Tested      string `json:"tested,omitempty" validate:"max=32"`
	RequiresPHP string `json:"requires_php,omitempty" validate:"max=32"`
}

// CoreUpdate is the optional WordPress core update advisory recorded on the
// site inventory document.
type CoreUpdate struct {
	NewVersion     string `json:"new_version" validate:"max=32"`
	CurrentVersion string `json:"current_version" validate:"max=32"`
}

// ParsedComponents decodes the site's JSONB component inventory into plugins
// and themes. A malformed/empty inventory yields empty slices (never an error)
// — callers use it only to seed best-effort from-versions.
func (s Site) ParsedComponents() (plugins, themes []Component) {
	if len(s.Components) == 0 {
		return nil, nil
	}
	var comp struct {
		Plugins []Component `json:"plugins"`
		Themes  []Component `json:"themes"`
	}
	if json.Unmarshal(s.Components, &comp) != nil {
		return nil, nil
	}
	return comp.Plugins, comp.Themes
}

// ParsedCoreUpdate decodes the site's JSONB inventory and returns the optional
// core update advisory (nil when there is none, or the inventory is
// empty/malformed).
func (s Site) ParsedCoreUpdate() *CoreUpdate {
	if len(s.Components) == 0 {
		return nil
	}
	var comp struct {
		CoreUpdate *CoreUpdate `json:"core_update,omitempty"`
	}
	if json.Unmarshal(s.Components, &comp) != nil {
		return nil
	}
	return comp.CoreUpdate
}

// ParsedAgentSelfUpdate decodes the site's JSONB inventory and returns the
// optional record of the agent's last self-update apply beat (nil when the site
// never reported one, or the inventory is empty/malformed). A record with no
// status is treated as absent: it says nothing.
func (s Site) ParsedAgentSelfUpdate() *AgentSelfUpdateResult {
	if len(s.Components) == 0 {
		return nil
	}
	var comp struct {
		AgentSelfUpdate *AgentSelfUpdateResult `json:"agent_self_update,omitempty"`
	}
	if json.Unmarshal(s.Components, &comp) != nil {
		return nil
	}
	if comp.AgentSelfUpdate == nil || comp.AgentSelfUpdate.Status == "" {
		return nil
	}
	return comp.AgentSelfUpdate
}

// ParsedRoles decodes the site's JSONB inventory and returns the WordPress role
// registry the agent last reported (GH #350).
//
// nil means UNKNOWN, not "no roles": either the agent predates role reporting
// or its registry was unreadable. Callers must render that as an explicit
// "not reported yet" state rather than silently substituting a guess, which is
// exactly the failure this feature exists to fix. A malformed inventory also
// yields nil.
func (s Site) ParsedRoles() []SiteRole {
	if len(s.Components) == 0 {
		return nil
	}
	var comp struct {
		Roles []SiteRole `json:"roles"`
	}
	if json.Unmarshal(s.Components, &comp) != nil {
		return nil
	}
	return comp.Roles
}

// Metadata is the site inventory an authenticated agent pushes.
type Metadata struct {
	WPVersion   string `json:"wp_version" validate:"max=32"`
	PHPVersion  string `json:"php_version" validate:"max=32"`
	ServerInfo  string `json:"server_info" validate:"max=512"`
	Multisite   bool   `json:"multisite"`
	ActiveTheme string `json:"active_theme" validate:"max=200"`
	// AgentVersion is the WPMgr agent plugin version (M27). Optional; empty when
	// an older agent does not report it.
	AgentVersion string      `json:"-" validate:"max=64"`
	Plugins      []Component `json:"plugins" validate:"max=2000,dive"`
	Themes       []Component `json:"themes" validate:"max=500,dive"`
	// CoreUpdate (when set) carries the WordPress core update advisory. nil
	// when there is no core update, or when the agent is old enough that it
	// does not report the field at all.
	CoreUpdate *CoreUpdate `json:"core_update,omitempty"`
	// ADR-037 Sprint 1, 1C — sparse-metadata expansion. Optional and best-
	// effort: round-tripped through the JSONB inventory column. nil when the
	// agent reported none of the expansion fields — the sink does not
	// overwrite previously-stored values in that case.
	Extras *MetadataExtras `json:"-"`
	// AgentSelfUpdate is the agent's own account of its last self-update apply
	// beat. Optional and additive: nil for a site that never staged one and for
	// every agent old enough to predate the channel.
	AgentSelfUpdate *AgentSelfUpdateResult `json:"-"`
}

// AgentSelfUpdateResult is the agent's account of what happened on the apply
// beat of its own upgrade, round-tripped through the JSONB inventory column
// under the `agent_self_update` key (no migration: the column is JSONB).
//
// The apply runs inside a WordPress cron request that has no control-plane
// response to ride on, so this record is the ONLY channel by which a failed,
// expired or already-applied outcome ever reaches the control plane. Dropping it
// leaves a confirmation timeout unable to distinguish "the cron run never
// happened" (the site was never touched) from "the cron run happened and the
// upgrade failed" (the site may need looking at), which are the two things an
// operator most needs told apart.
type AgentSelfUpdateResult struct {
	// Status is one of applied|failed|expired|already_applied. Stored as a plain
	// string, not an enum: a status a newer agent introduces must reach an
	// operator verbatim rather than being dropped by an older control plane.
	Status string `json:"status"`
	// FromVersion is the on-disk version at stage time; ToVersion the staged
	// target. Both best-effort.
	FromVersion string `json:"from_version,omitempty"`
	ToVersion   string `json:"to_version,omitempty"`
	// Detail is the agent's own human-readable, non-secret sentence. The agent
	// scrubs anything URL-shaped out of it before storing (the manifest's
	// package URL is a short-lived bearer credential), and this side bounds its
	// length like every other agent-supplied string.
	Detail string `json:"detail,omitempty"`
	// At is the unix timestamp the agent stamped the record with; 0 when it did
	// not say.
	At int64 `json:"at,omitempty"`
	// ApplyID is the opaque per-apply identifier the agent stamped this record
	// with; empty for a record written by an agent that predates it. It is
	// what the beat-3 confirmation worker compares against the apply id it
	// carried on the arm it sent, so a version movement can only be credited
	// to THIS run's own apply when the two match. Never treated as a version
	// or a time, and never parsed: compared whole.
	ApplyID string `json:"apply_id,omitempty"`
	// Rung is the connection-release rung the apply ran on: fpm, litespeed, or
	// the portable fallback that held the caller's connection for the whole
	// swap. Diagnostic only, it gates nothing, and it is how a fleet-wide read
	// can tell which sites upgrade on an attached connection. Empty for every
	// agent before 0.61.110.
	Rung string `json:"rung,omitempty"`
}

// MetadataExtras carries the ADR-037 Sprint 1 sparse-metadata expansion. The
// shape is round-tripped through the existing JSONB inventory column as
// host_flags / disk / user_count / admin_count peer keys to plugins/themes.
type MetadataExtras struct {
	HostFlags  *HostFlags `json:"host_flags,omitempty"`
	Disk       *Disk      `json:"disk,omitempty"`
	UserCount  int        `json:"user_count,omitempty"`
	AdminCount int        `json:"admin_count,omitempty"`
	// Roles is the site's WordPress role registry (GH #350), stored under the
	// `roles` key of the same JSONB inventory document. nil when the agent did
	// not report it; readers must treat nil as "unknown", never as "none".
	Roles []SiteRole `json:"roles,omitempty"`
}

// SiteRole is one WordPress role that exists on a site: the slug the security
// policy is written in, plus the display name that site shows for it. The agent
// localizes the name through translate_user_role() before reporting, so an
// Italian site sends "Amministratore" for the `administrator` slug.
type SiteRole struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// HostFlags is the hosting-platform fingerprint surfaced from the agent's
// defined()-based probes. All false when the agent doesn't recognise the host.
type HostFlags struct {
	IsPressable bool `json:"is_pressable,omitempty"`
	IsGridpane  bool `json:"is_gridpane,omitempty"`
	IsWPEngine  bool `json:"is_wpengine,omitempty"`
	IsAtomic    bool `json:"is_atomic,omitempty"`
	IsKinsta    bool `json:"is_kinsta,omitempty"`
	IsFlywheel  bool `json:"is_flywheel,omitempty"`
	IsRunCloud  bool `json:"is_runcloud,omitempty"`
	IsCloudways bool `json:"is_cloudways,omitempty"`
}

// Disk is the sampled disk-usage snapshot the agent ships. Bytes.
type Disk struct {
	WPContentBytes int64 `json:"wp_content_bytes,omitempty"`
	UploadsBytes   int64 `json:"uploads_bytes,omitempty"`
	FreeBytes      int64 `json:"free_bytes,omitempty"`
}
