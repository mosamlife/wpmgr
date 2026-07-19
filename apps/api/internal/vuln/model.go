// Package vuln implements the vulnerability scanner domain: ingesting the
// Wordfence Intelligence V3 feed, matching each site's installed inventory
// against that feed, and persisting per-site findings in site_vulnerabilities.
//
// Detection is a pure CP-side join. No agent change is required: the CP already
// holds each site's installed plugins/themes/core + version in Site.Components
// (site/model.go). The agent's refresh_inventory command (already existing)
// keeps that data fresh; a completed update triggers an immediate rescan.
//
// Attribution obligations (Wordfence Intelligence ToS, 2026-01-26):
//   - Defiant copyright + license text are stored once in wordfence_vuln_feed_meta
//     and rendered in the UI footer on any vulnerability view.
//   - MITRE copyright notice is stored in wordfence_vuln_feed_meta and rendered
//     on any finding row that carries a CVE identifier.
//   - CVE and Wordfence references[] link-out is included in the finding DTO.
//   - The API key (WPMGR_WORDFENCE_API_KEY) is private and per-caller; it is
//     read from env and never persisted to the database or emitted to clients.
package vuln

import (
	"time"

	"github.com/google/uuid"
)

// Severity levels mirror the Wordfence cvss_rating buckets and the scan
// domain's severity vocabulary. SeverityUnknown is a distinct bucket (GH
// #245): a finding whose feed record carries neither a cvss_rating nor a
// cvss_score is NOT the same thing as a confirmed-low-severity finding, and
// must never be silently indistinguishable from one — the honest signal is
// "unknown", not "low".
const (
	SeverityCritical = "critical"
	SeverityHigh     = "high"
	SeverityMedium   = "medium"
	SeverityLow      = "low"
	SeverityUnknown  = "unknown"
)

// Status values for a site_vulnerabilities row.
const (
	StatusOpen      = "open"
	StatusDismissed = "dismissed"
	StatusResolved  = "resolved"
)

// Kind values for the software type dimension.
const (
	KindPlugin = "plugin"
	KindTheme  = "theme"
	KindCore   = "core"
)

// FeedMeta holds the current state of the feed ingestion sentinel row.
//
// OK/FetchedAt/RecordCount track Scanner-driven DETECTION freshness (Scanner
// is the authoritative superset ID catalog; RescanSite gates on OK).
// EnrichmentOK/LastEnrichmentAt separately track Production-driven CVSS/CVE
// ENRICHMENT health — kept independent so a transient Production hiccup
// (rate-limited, zero records) never blocks detection rescans.
type FeedMeta struct {
	FetchedAt        *time.Time
	OK               bool
	RecordCount      int
	DefiantNotice    string
	DefiantLicense   string
	MitreNotice      string
	LastError        string
	EnrichmentOK     bool
	LastEnrichmentAt *time.Time
}

// VulnSoftware is one row from the wordfence_vuln_software index — a
// (kind, slug) entry linked to its parent vulnerability record.
type VulnSoftware struct {
	VulnID           string
	Kind             string
	Slug             string
	AffectedVersions []byte // raw JSONB — parsed by the matcher
	Patched          bool
	PatchedVersions  []byte // raw JSONB array of version strings
}

// VulnRecord is the minimal projection of wordfence_vuln_feed needed by the
// matcher to populate a finding row.
type VulnRecord struct {
	VulnID    string
	Title     string
	CVE       string
	CVELink   string
	CVSSScore *float64
	CVSSRating string
	References []byte // raw JSONB
}

// Finding is one row from site_vulnerabilities.
type Finding struct {
	ID               uuid.UUID
	TenantID         uuid.UUID
	SiteID           uuid.UUID
	VulnID           string
	Kind             string
	Slug             string
	Name             string
	InstalledVersion string
	FixedVersion     string
	Severity         string
	CVSSScore        *float64
	CVE              string
	Title            string
	Status           string
	FirstSeen        time.Time
	LastSeen         time.Time
	ResolvedAt       *time.Time
	DismissedAt      *time.Time
	DismissedBy      *uuid.UUID
	// Enrichment from the feed (not stored on the finding row; joined at read time).
	CVELink    string
	References []byte // raw JSONB
}

// FleetSummary is the tenant-level rollup returned by the fleet endpoint.
type FleetSummary struct {
	TotalOpen int
	Critical  int
	High      int
	Medium    int
	Low       int
	Unknown   int
	Findings  []FleetFinding
}

// FleetFinding is one row in the cross-site prioritized findings list.
type FleetFinding struct {
	SiteID   uuid.UUID
	SiteName string
	SiteURL  string
	Finding  Finding
}

// SiteSnapshot is the minimal site projection the vuln domain needs.
type SiteSnapshot struct {
	ID        uuid.UUID
	TenantID  uuid.UUID
	Name      string
	URL       string
	WPVersion string
	Plugins   []ComponentSnapshot
	Themes    []ComponentSnapshot
}

// ComponentSnapshot is one installed plugin or theme entry.
type ComponentSnapshot struct {
	Slug    string
	Name    string
	Version string
}

// SeverityFromRating maps a Wordfence cvss_rating string to our severity bucket.
// Falls back to numeric score when rating is absent. When NEITHER a
// recognised rating NOR a numeric score is available, returns SeverityUnknown
// — NOT SeverityLow (GH #245: bucketing "no data" as "low" silently hid a
// CVSS 9.8 core RCE behind a confirmed-low-severity label). "None" is a
// legitimate Wordfence rating (CVSS 0.0, informational) and stays mapped to
// low; only the true no-data case changes.
func SeverityFromRating(rating string, score *float64) string {
	switch rating {
	case "Critical":
		return SeverityCritical
	case "High":
		return SeverityHigh
	case "Medium":
		return SeverityMedium
	case "Low", "None":
		return SeverityLow
	}
	// Fall back to numeric CVSS score buckets.
	if score != nil {
		switch {
		case *score >= 9.0:
			return SeverityCritical
		case *score >= 7.0:
			return SeverityHigh
		case *score >= 4.0:
			return SeverityMedium
		default:
			return SeverityLow
		}
	}
	return SeverityUnknown
}
