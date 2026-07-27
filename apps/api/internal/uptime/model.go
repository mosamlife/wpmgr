package uptime

import (
	"time"

	"github.com/google/uuid"
)

// Health status values written to sites.health_status from probe results. These
// extend the M2 freshness states ("unknown"/"healthy"/"unreachable") with the
// active-probe outcome: a site that responds is "healthy"; one that is down
// (5xx/timeout/conn-error/SSRF-blocked) is "unreachable".
const (
	HealthHealthy     = "healthy"
	HealthUnreachable = "unreachable"
	HealthUnknown     = "unknown"
)

// Alert status values tracked per site for transition detection.
const (
	StatusUp      = "up"
	StatusDown    = "down"
	StatusUnknown = "unknown"
)

// EnrolledSite is the slim projection the probe job iterates over (URL included
// so it can be probed).
type EnrolledSite struct {
	ID           uuid.UUID
	TenantID     uuid.UUID
	URL          string
	HealthStatus string
}

// AlertConfig is a tenant's default alert channel.
type AlertConfig struct {
	TenantID        uuid.UUID
	EmailRecipients []string
	WebhookURL      string
	// WebhookSecret keys the webhook HMAC signature; NEVER serialized to the API.
	WebhookSecret string
	Enabled       bool
	// NotifySecurity routes high-severity ADR-037 activity-log events into this
	// same channel (email + webhook). Default false.
	NotifySecurity bool
	// m103 (GH #247) — vulnerability alerting is the THIRD signal on this same
	// channel. NotifyVulns is opt-in (default false), mirroring NotifySecurity.
	NotifyVulns bool
	// VulnMinSeverity is the alert threshold: "critical"|"high"|"medium"|"low".
	// A finding with SeverityUnknown ALWAYS alerts regardless of this value
	// (see internal/vuln/alertdispatch.go) — "unknown" is deliberately not a
	// selectable threshold value.
	VulnMinSeverity string
	// VulnIncludeInDigest gates a "new vulnerabilities" section on the existing
	// email digest. Default true.
	VulnIncludeInDigest bool
	UpdatedAt           time.Time
}

// AlertState is a site's durable alert transition memory.
type AlertState struct {
	SiteID          uuid.UUID
	TenantID        uuid.UUID
	LastStatus      string
	ConsecutiveDown int32
	InIncident      bool
	LastAlertAt     *time.Time
}

// ---------------------------------------------------------------------------
// Fleet uptime models (GET /api/v1/fleet/status, GET /api/v1/fleet/incidents)
// ---------------------------------------------------------------------------

// slowThresholdMs is the latency threshold above which a probe is classified
// as "degraded" even when the site responds with a 2xx. Matches the UX spec.
const slowThresholdMs = 2000.0

// FleetSiteStatus is the derived availability status for one site in the fleet
// status endpoint.
type FleetSiteStatus string

const (
	FleetStatusUp       FleetSiteStatus = "up"
	FleetStatusDegraded FleetSiteStatus = "degraded"
	FleetStatusDown     FleetSiteStatus = "down"
	FleetStatusUnknown  FleetSiteStatus = "unknown"
)

// Fleet status reason strings (GH #291 Task 2): a short, machine-readable
// explanation for WHY deriveFleetStatus picked FleetStatusDegraded, so the API
// and UI can say which degraded it is instead of rendering a bare chip. Empty
// string means the status needs no further explanation (up/down/unknown).
const (
	// FleetReasonAgentUnreachable: sites.connection_state = "disconnected" AND
	// sites.disconnected_reason names one of the sweeper's own transitions
	// (see sweeperDisconnectReasons below). The connection sweeper's signed,
	// uncacheable active-verify already failed against the agent, or the agent
	// stopped heartbeating outright (GH #291).
	FleetReasonAgentUnreachable = "agent_unreachable"
	// FleetReasonAgentDegraded: sites.connection_state = "degraded", meaning
	// the agent heartbeat is stale but not yet past the disconnect threshold.
	// This state has no last-will path (see sweeperDisconnectReasons), so it
	// needs no reason disambiguation.
	FleetReasonAgentDegraded = "agent_degraded"
	// FleetReasonSlowResponse: the probe succeeded but total_ms exceeded
	// slowThresholdMs.
	FleetReasonSlowResponse = "slow_response"
)

// sweeperDisconnectReasons are the exact sites.disconnected_reason values the
// connection sweeper itself writes when it drives connected/degraded ->
// disconnected (internal/site/sweeper.go Sweep): "agent_unreachable" from the
// active-verify path, "heartbeat_timeout" from the passive fallback path used
// when active verify is disabled. Both mean the CP independently observed the
// agent going silent or refusing a signed probe, i.e. a real outage.
//
// Every OTHER disconnected site reached that state via a SIGNED agent
// last-will (ADR-040, connService.RecordLastWillTenant): the operator
// deactivated or uninstalled the plugin, or some other agent-supplied reason.
// The agent controls that reason string (bounded to 64 bytes, defaults to
// "user_initiated", see internal/agent/handler.go disconnect()), so it is
// deliberately NOT trusted as a positive signal; only the two CP-authored
// strings below are ever treated as evidence of an unhealthy site. Anything
// else (a known last-will reason, an unrecognized string, or an empty/legacy
// row predating this column) is treated as healthy: deriveFleetStatus must
// never raise an alarming Degraded chip on a site the operator cleanly took
// offline, and a value it cannot positively attribute to the sweeper is not
// proof of an outage.
var sweeperDisconnectReasons = map[string]bool{
	"agent_unreachable": true,
	"heartbeat_timeout": true,
}

// FleetStatusCounts is the summary count header in the fleet status response.
type FleetStatusCounts struct {
	Up       int `json:"up"`
	Degraded int `json:"degraded"`
	Down     int `json:"down"`
	Unknown  int `json:"unknown"`
}

// FleetStatusItem is the per-site row in the fleet status response.
// JSON field names are pinned to the frontend FleetStatusItem contract in
// apps/web/src/features/fleet/fleet-types.ts — do not rename without
// updating both sides.
type FleetStatusItem struct {
	SiteID          uuid.UUID       `json:"site_id"`
	Name            string          `json:"name"`
	URL             string          `json:"url"`
	ConnectionState string          `json:"connection_state"`
	HealthStatus    string          `json:"health_status"`
	Status          FleetSiteStatus `json:"status"`
	// StatusReason explains WHY Status is what it is when that is not
	// self-evident (currently only populated for FleetStatusDegraded, one of
	// the FleetReason* constants). Empty for up/down/unknown.
	StatusReason     string     `json:"status_reason,omitempty"`
	Up               *bool      `json:"up"`
	LastProbeAt      *time.Time `json:"last_probe_at"`
	UptimePct7d      float64    `json:"uptime_pct_7d"`
	AvgLatencyMs     *float64   `json:"avg_latency_ms"`
	TLSExpiry        *time.Time `json:"tls_expiry"`
	LatencySparkline []float64  `json:"latency_sparkline"`
	// InIncident is kept for internal use (summary counting) but not needed
	// by the frontend contract — retained for the service-layer logic.
	InIncident bool `json:"in_incident"`
}

// FleetStatusResponse is the response body for GET /api/v1/fleet/status.
type FleetStatusResponse struct {
	Summary FleetStatusCounts `json:"summary"`
	Items   []FleetStatusItem `json:"items"`
}

// FleetSiteInfo is the Postgres-resident projection used by GetFleetSiteInfo.
// It contains only fields that live in the sites / site_alert_state tables —
// probe / uptime metrics are sourced from the metrics.Store by the service.
type FleetSiteInfo struct {
	SiteID          uuid.UUID
	Name            string
	URL             string
	ConnectionState string
	HealthStatus    string
	InIncident      bool
	// DisconnectedReason is sites.disconnected_reason (empty when NULL or when
	// ConnectionState is not "disconnected"). deriveFleetStatus reads this to
	// tell a sweeper-detected outage apart from an operator-initiated last-will
	// disconnect (GH #291 follow-up). See the FleetReasonAgentUnreachable doc
	// comment for the full set of values this can hold.
	DisconnectedReason string
}

// FleetIncidentItem is one open or recently-started incident for the fleet
// incidents endpoint, read from the persisted site_incidents table (M94, GH
// #148) — real incident rows, not an estimate derived from site_alert_state's
// single mutable transition-memory row.
//
// JSON field names are pinned to the frontend contract consumed by the fleet
// incidents panel (GH #148) — do not rename without updating both sides.
// EndedAt and DurationSeconds intentionally do NOT use `omitempty`: an open
// incident must marshal them as explicit `null`, not omit the key, so the web
// client's `=== null` check can distinguish "ongoing" from a missing field
// (an omitted key deserializes to `undefined`, not `null`, and produced
// "NaNh" duration on open incidents).
type FleetIncidentItem struct {
	// ID is the site_incidents row id — the web uses it to open the
	// incident-detail modal (GET /api/v1/fleet/incidents/:incidentId).
	ID     uuid.UUID `json:"id"`
	SiteID uuid.UUID `json:"site_id"`
	// Kind is always "down": the alert state machine (see Evaluate in
	// alerts.go) opens an incident only on a down-threshold crossing —
	// "degraded" is a live fleet-status derivation (deriveFleetStatus) and is
	// never persisted as an incident state.
	Kind            string     `json:"kind"`
	SiteName        string     `json:"name"`
	SiteURL         string     `json:"url"`
	StartedAt       *time.Time `json:"started_at,omitempty"`
	EndedAt         *time.Time `json:"ended_at"`
	DurationSeconds *int64     `json:"duration_seconds"`
	Ongoing         bool       `json:"ongoing"`
	LatestTotalMs   *float64   `json:"latest_total_ms,omitempty"`
}

// ---------------------------------------------------------------------------
// Incident detail (GET /api/v1/fleet/incidents/:incidentId, GH #148 part 1)
// ---------------------------------------------------------------------------

// IncidentSummary is the tenant-scoped site_incidents row (joined with its
// site's name/url) returned by Repo.GetIncidentByID, WITHOUT the probe
// timeline. The handler authorizes a site-scoped principal against
// SiteID BEFORE spending a metrics-store round-trip building the full detail.
type IncidentSummary struct {
	ID             uuid.UUID
	SiteID         uuid.UUID
	SiteName       string
	SiteURL        string
	StartedAt      time.Time
	EndedAt        *time.Time
	PeakStatus     string
	LastHTTPStatus int
	Reason         string
}

// IncidentProbe is one raw probe result in the incident-detail timeline
// (sourced from metrics.Store.QueryProbeWindow). JSON field names are PINNED
// to the frontend incident-detail contract (GH #148) — do not rename without
// updating both sides. Error omits when empty (a healthy probe carries none).
type IncidentProbe struct {
	ProbedAt   time.Time `json:"probed_at"`
	Up         bool      `json:"up"`
	HTTPStatus int       `json:"http_status"`
	TotalMs    float64   `json:"total_ms"`
	Error      string    `json:"error,omitempty"`
}

// IncidentDetail is the response body for
// GET /api/v1/fleet/incidents/:incidentId. JSON field names are PINNED to the
// frontend incident-detail contract (GH #148) — do not rename without
// updating both sides. EndedAt/DurationSeconds do NOT use `omitempty`,
// mirroring FleetIncidentItem: an ongoing incident must marshal explicit
// `null`, not an omitted key (see FleetIncidentItem's doc comment for the
// client-side bug class this avoids). Probes is never nil — a metrics-store
// window with no data (retention-aged, disabled backend, or a site with no
// probes yet) still yields an empty slice, not an omitted/null key, and the
// incident summary is always present regardless (graceful degradation).
type IncidentDetail struct {
	ID               uuid.UUID       `json:"id"`
	SiteID           uuid.UUID       `json:"site_id"`
	Name             string          `json:"name"`
	URL              string          `json:"url"`
	StartedAt        time.Time       `json:"started_at"`
	EndedAt          *time.Time      `json:"ended_at"`
	DurationSeconds  *int64          `json:"duration_seconds"`
	Ongoing          bool            `json:"ongoing"`
	PeakStatus       string          `json:"peak_status"`
	LastHTTPStatus   int             `json:"last_http_status"`
	Reason           string          `json:"reason"`
	IncidentCount30d int             `json:"incident_count_30d"`
	Probes           []IncidentProbe `json:"probes"`
	ProbesTruncated  bool            `json:"probes_truncated"`
}

// Vulnerability alert-threshold values (m103, GH #247) for AlertConfig.
// VulnMinSeverity. These deliberately mirror internal/vuln's severity
// vocabulary as plain string literals (not an import of that package) so
// internal/uptime does not take on a cross-domain dependency for four
// constants — see CLAUDE.md's "don't cross-import sibling domains" rule.
// "unknown" is intentionally absent: it is never a selectable threshold (an
// unknown-severity finding always alerts regardless of threshold).
const (
	VulnSeverityCritical = "critical"
	VulnSeverityHigh     = "high"
	VulnSeverityMedium   = "medium"
	VulnSeverityLow      = "low"
)

// ValidVulnMinSeverity reports whether s is one of the four selectable
// alert-threshold values.
func ValidVulnMinSeverity(s string) bool {
	switch s {
	case VulnSeverityCritical, VulnSeverityHigh, VulnSeverityMedium, VulnSeverityLow:
		return true
	}
	return false
}

// AlertKind distinguishes a downtime alert from a recovery alert.
type AlertKind string

const (
	AlertDown     AlertKind = "down"
	AlertRecovery AlertKind = "recovery"
	// AlertSecurity is a high-severity ADR-037 activity-log event routed into
	// this alert channel (when the tenant has notify_security enabled).
	AlertSecurity AlertKind = "security"
)

// SecurityEvent is a high-severity activity-log event handed to the Dispatcher
// for delivery to the tenant's configured alert channels. It carries only what
// the email subject + webhook body need; the full event lives in the activity
// log (the tamper-evident store), not the alert payload.
type SecurityEvent struct {
	TenantID  uuid.UUID
	SiteID    uuid.UUID
	SiteURL   string
	SiteName  string
	Summary   string
	EventType string
	Severity  string
	FiredAt   time.Time
}

// Alert is a fired downtime/recovery notification delivered to a channel.
type Alert struct {
	Kind     AlertKind
	TenantID uuid.UUID
	SiteID   uuid.UUID
	SiteURL  string
	SiteName string
	// HTTPStatus / Error describe the probe that triggered the alert (down only).
	HTTPStatus int
	Error      string
	FiredAt    time.Time
}
