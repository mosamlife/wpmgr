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
	"time"

	"github.com/google/uuid"
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
type ListInput struct {
	TenantID uuid.UUID
	Tag      string
	// State, when non-empty, filters to exactly that connection_state (e.g.
	// "archived" for the archived chip). When empty the list hides archived
	// sites (the ADR-041 default).
	State     string
	Limit     int32
	Offset    int32
	Principal ScopedPrincipal // optional; nil → plain InTenantTx (org-scoped)
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

// Metadata is the site inventory an authenticated agent pushes.
type Metadata struct {
	WPVersion   string      `json:"wp_version" validate:"max=32"`
	PHPVersion  string      `json:"php_version" validate:"max=32"`
	ServerInfo  string      `json:"server_info" validate:"max=512"`
	Multisite   bool        `json:"multisite"`
	ActiveTheme string      `json:"active_theme" validate:"max=200"`
	Plugins     []Component `json:"plugins" validate:"max=2000,dive"`
	Themes      []Component `json:"themes" validate:"max=500,dive"`
	// CoreUpdate (when set) carries the WordPress core update advisory. nil
	// when there is no core update, or when the agent is old enough that it
	// does not report the field at all.
	CoreUpdate *CoreUpdate `json:"core_update,omitempty"`
	// ADR-037 Sprint 1, 1C — sparse-metadata expansion. Optional and best-
	// effort: round-tripped through the JSONB inventory column. nil when the
	// agent reported none of the expansion fields — the sink does not
	// overwrite previously-stored values in that case.
	Extras *MetadataExtras `json:"-"`
}

// MetadataExtras carries the ADR-037 Sprint 1 sparse-metadata expansion. The
// shape is round-tripped through the existing JSONB inventory column as
// host_flags / disk / user_count / admin_count peer keys to plugins/themes.
type MetadataExtras struct {
	HostFlags  *HostFlags `json:"host_flags,omitempty"`
	Disk       *Disk      `json:"disk,omitempty"`
	UserCount  int        `json:"user_count,omitempty"`
	AdminCount int        `json:"admin_count,omitempty"`
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
