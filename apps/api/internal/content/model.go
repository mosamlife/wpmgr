// Package content owns the page-ownership inventory (Track B slice S1): which
// editor owns each page on a site, and whether the content column is the thing
// a visitor sees. It refreshes the inventory by sending the agent's
// content_probe command in list mode, validates everything the site returns,
// and serves the per-site inventory and the owner fleet report.
package content

import (
	"time"

	"github.com/google/uuid"
)

// MinAgentVersionForContentProbe is the first agent release that carries the
// content_probe command (probe v2). A site below it is shown "agent update
// needed", never an error, and is never sent the command.
//
// Pinned by TestMinAgentVersionForContentProbe_PinnedAboveLiveAgent: it must
// stay a well-formed dotted version and must not be moved without moving the
// release that ships the command.
const MinAgentVersionForContentProbe = "0.61.154"

// Refresh states reported by the inventory endpoint.
const (
	// StateOK: the site's agent is at or above the floor.
	StateOK = "ok"
	// StateAgentUpdateNeeded: the site's agent is below the floor.
	StateAgentUpdateNeeded = "agent_update_needed"
	// StateNotConnected: the site cannot be asked right now.
	StateNotConnected = "not_connected"
)

// Integration is one enabled row of the global allowlist, as the refresh job
// needs it.
type Integration struct {
	ID          string
	DisplayName string
	Status      string
	Enabled     bool
	ThemeSlug   string
	Descriptor  []byte
}

// Run is the record of a site's last refresh.
type Run struct {
	CheckedAt   time.Time
	PagesStored int32
	Truncated   bool
}

// SiteTarget is what the refresh needs to know about a site.
type SiteTarget struct {
	URL             string
	AgentVersion    string
	ConnectionState string
	Enrolled        bool
	Paused          bool
	Components      []byte
}

// Row is one validated page ready to store. Every string has been shape
// checked; Title has been through humantext.Clean and capped.
type Row struct {
	PostID      int64
	PostType    string
	PostStatus  string
	Verdict     string
	RouteNumber int16
	RouteReason string
	OwnerID     string
	OwnerName   string
	OwnerVer    string
	Title       string
}

// InventoryRow is a stored page as the API returns it.
type InventoryRow struct {
	PostID             int64
	PostType           string
	PostStatus         string
	Verdict            string
	RouteNumber        int16
	RouteReason        string
	OwnerIntegrationID *string
	OwnerDisplayName   *string
	OwnerVersion       *string
	Title              *string
	CheckedAt          time.Time
}

// SweepSite is one site the periodic sweep enqueues a refresh for.
type SweepSite struct {
	TenantID uuid.UUID
	SiteID   uuid.UUID
}

// FleetVerdictShare is one (verdict, route) line of the fleet report.
type FleetVerdictShare struct {
	Verdict     string
	RouteNumber int16
	Pages       int64
	Sites       int64
}

// FleetBuilderShare is one (builder, version) line of the fleet report.
type FleetBuilderShare struct {
	IntegrationID string
	Version       *string
	Pages         int64
	Sites         int64
}
