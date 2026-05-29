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
	CreatedAt    time.Time
	UpdatedAt    time.Time
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

// ListInput is tenant-scoped pagination input, optionally filtered by tag.
type ListInput struct {
	TenantID uuid.UUID
	Tag      string
	Limit    int32
	Offset   int32
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
}
