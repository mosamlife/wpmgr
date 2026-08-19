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
	// LastSeenAt is sites.last_seen_at (nil when the site has never
	// heartbeated). GH #291 Phase 2's app prober reads this for B0 (agent
	// ground truth): a fresh heartbeat already proves PHP booted and
	// WordPress loaded, so the prober makes zero network requests. Ignored
	// by the reachability prober and the cron-kicker.
	LastSeenAt *time.Time
	// AppProbePath is sites.app_probe_path (empty when unset). GH #291 Phase
	// 2's app prober reads this for B3 (per-site override): when set, it
	// replaces the default /wp-json/ (with ?rest_route=/ fallback) target
	// entirely. Ignored by the reachability prober and the cron-kicker.
	AppProbePath string
	// AppAlertsDisabled is sites.app_alerts_disabled (m108, GH #291 Phase 3):
	// the per-site opt-out for app-health ALERTING only. The app probe still
	// runs and the dashboard stays accurate; ProbeWorker skips the app-alert
	// transition step entirely for a site with this set (see processSite).
	// Ignored by the reachability prober, the app prober itself, and the
	// cron-kicker.
	AppAlertsDisabled bool
	// ConnectionState is sites.connection_state (GH #291 Phase 3 Fix 2).
	// Carried so processSite's app-alert eligibility gate (appAlertEligible)
	// can exclude a revoked/archived site using the IDENTICAL predicate
	// GetTenantAppAlertRatio's WHERE clause enforces server-side - the fire
	// path and the fleet circuit breaker's ratio must never disagree about
	// which sites count. Ignored by the reachability prober, the app prober
	// itself, and the cron-kicker.
	ConnectionState string
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
	// AppAlertsEnabled (m108, GH #291 Phase 3) is the FOURTH signal on this
	// same channel: whether the NEW app-health alert kind (down/recovery, and
	// the fleet circuit-breaker's aggregate) is allowed to dispatch for this
	// tenant. Deliberately independent of Enabled (the existing reachability
	// channel) so a tenant that already has downtime alerts on does not
	// silently start receiving app-health alerts too. Its default is decided
	// ONCE, deterministically, by migration m108 - see
	// Service.GetAlertConfig's zero-value path and app_alert_rollout.
	AppAlertsEnabled bool
	UpdatedAt        time.Time
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
	// FleetReasonAppDown (GH #291 Phase 2): the reachability probe reports
	// up=true (visitors are being served, possibly a cached response) but
	// the application-health probe conclusively found app_up=false - the
	// literal incident this phase exists to catch: a page cache masking a
	// completely dead PHP backend. The granular reason (rest_5xx,
	// wp_fatal_error, unreachable, ...) is carried separately on
	// FleetStatusItem.AppProbeReason; this constant is only the coarse
	// status_reason, matching the shape of every other FleetReason*.
	FleetReasonAppDown = "app_down"
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
	Up          *bool      `json:"up"`
	LastProbeAt *time.Time `json:"last_probe_at"`
	// UptimePct7d is the 7-day uptime percentage, and is NIL when the site
	// has no measurement in the window — never probed, monitoring never
	// enabled, or its whole history has aged past probeRetention. It is a
	// POINTER precisely so that case serialises as `null` rather than `0`:
	// as a bare float64 it carried the Go zero value onto the wire, and the
	// dashboard read 0 as "0% uptime" and painted a never-probed site as a
	// solid 90-day outage strip (GH #460). The frontend contract has always
	// declared this `number | null` (apps/web/src/features/fleet/fleet-types.ts);
	// this is the API finally sending the shape it promised. Do not
	// dereference-with-default it back into a float anywhere: "no data" and
	// "0% available" are different facts about a site and an operator may
	// be putting whichever one we send in front of their own client.
	UptimePct7d      *float64   `json:"uptime_pct_7d"`
	AvgLatencyMs     *float64   `json:"avg_latency_ms"`
	TLSExpiry        *time.Time `json:"tls_expiry"`
	LatencySparkline []float64  `json:"latency_sparkline"`
	// InIncident is kept for internal use (summary counting) but not needed
	// by the frontend contract — retained for the service-layer logic.
	InIncident bool `json:"in_incident"`
	// AppUp is the GH #291 Phase 2 application-health verdict from the most
	// recent app probe: true, false, or nil (never probed, or the most
	// recent probe was inconclusive - see AppProbeReason). Independent of
	// Up: a cached 200 (Up=true) can coexist with AppUp=false when a page
	// cache is masking a dead PHP backend. An additive, backend-only key - 	// see TestFleetStatusItemJSONContract.
	AppUp *bool `json:"app_up"`
	// AppProbeReason is the machine-readable reason for AppUp's most recent
	// verdict (one of the AppProbeReason* constants - e.g. "agent_fresh",
	// "rest_ok", "cache_hit"). Empty when no app probe has run yet.
	AppProbeReason string `json:"app_probe_reason,omitempty"`
}

// FleetStatusResponse is the response body for GET /api/v1/fleet/status.
type FleetStatusResponse struct {
	Summary FleetStatusCounts `json:"summary"`
	Items   []FleetStatusItem `json:"items"`
}

// ---------------------------------------------------------------------------
// Fleet uptime history (GET /api/v1/fleet/uptime-history, GH #460)
// ---------------------------------------------------------------------------

// FleetUptimeDay is one UTC day of measured availability for one site.
//
// UptimePct is NIL when that day has no stored measurement — the site did not
// exist yet, monitoring was off, the probe worker did not run, or the day has
// aged past probeRetention. It is NOT zero. A zero here means the site was
// measured and was down for every probe of that day, which is a completely
// different statement to put in front of an operator, or the client the
// operator forwards it to. The whole of GH #460 is that these two were the
// same value on the wire.
type FleetUptimeDay struct {
	// Date is the UTC calendar day, YYYY-MM-DD. Always UTC: the rollup is
	// keyed on UTC days, and re-bucketing per viewer timezone would split one
	// stored day across two rendered cells.
	Date string `json:"date"`
	// UptimePct is up_checks/total_checks*100 for the day, or nil for no data.
	UptimePct *float64 `json:"uptime_pct"`
	// Checks is the number of probes recorded that day (0 when UptimePct is
	// nil). Carried so a client can distinguish a confidently-measured day
	// from one with a single probe, and so "no data" is falsifiable rather
	// than something the client has to infer from a null alone.
	Checks int64 `json:"checks"`
	// AvgLatencyMs is the mean response time over SUCCESSFUL probes with a
	// non-zero reading, nil when the day had none.
	AvgLatencyMs *float64 `json:"avg_latency_ms"`
}

// FleetUptimeHistoryItem is one site's densified day strip.
type FleetUptimeHistoryItem struct {
	SiteID uuid.UUID `json:"site_id"`
	Name   string    `json:"name"`
	URL    string    `json:"url"`
	// Days is ALWAYS exactly the requested number of days, oldest first, with
	// no gaps: the service densifies the store's sparse output across every
	// UTC day in the window. A client can therefore index it positionally
	// without re-deriving dates, and every unmeasured day is explicitly
	// present with a nil UptimePct rather than silently missing.
	Days []FleetUptimeDay `json:"days"`
	// MeasuredDays is how many entries in Days carry a measurement. A young
	// site reads e.g. 28 of 90, which is the honest thing to show instead of
	// implying 90 days of history.
	MeasuredDays int `json:"measured_days"`
}

// FleetUptimeHistoryResponse is the response body for
// GET /api/v1/fleet/uptime-history.
type FleetUptimeHistoryResponse struct {
	// Window echoes the requested window ("7d", "30d", "90d").
	Window string `json:"window"`
	// Days is the number of entries in every item's Days array.
	Days int `json:"days"`
	// StartDate and EndDate are the inclusive UTC bounds of that array.
	StartDate string                   `json:"start_date"`
	EndDate   string                   `json:"end_date"`
	Items     []FleetUptimeHistoryItem `json:"items"`
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
	// AlertAppDown / AlertAppRecovery (m108, GH #291 Phase 3) are the NEW,
	// independent app-health alert kind - see EvaluateApp. Delivered through
	// the SAME Dispatcher.Fire path as AlertDown/AlertRecovery (same
	// channel, same audit trail), but gated separately by
	// AlertConfig.AppAlertsEnabled instead of Enabled (see
	// ProbeWorker.fireApp) so a tenant with reachability alerts already on
	// does not silently start receiving app-health alerts too.
	AlertAppDown     AlertKind = "app_down"
	AlertAppRecovery AlertKind = "app_recovery"
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
	// HTTPStatus / Error describe the probe that triggered the alert (down
	// only). For AlertAppDown, Error carries the AppProbeReason* value
	// instead of a reachability error string (there is no HTTPStatus to
	// report for an app-health verdict derived from B0/B3, so it stays 0).
	HTTPStatus int
	Error      string
	FiredAt    time.Time
}

// AppAggregateAlert is the fleet circuit breaker's own aggregate
// notification (m108, GH #291 Phase 3 section 2): fired ONCE when more than
// a configurable ratio of a tenant's alert-eligible sites are simultaneously
// app-down, collapsing what would otherwise be one alert per site into a
// single notification, and ONCE when the ratio recovers below threshold.
// Never carries a single SiteID/SiteURL - see Dispatcher.FireAppAggregate.
type AppAggregateAlert struct {
	// Recovered distinguishes the open vs recovery notification (the
	// tenant-wide sibling of AlertKind's down/recovery pair).
	Recovered bool
	// Updated (GH #291 Phase 3 Fix 3) marks a THIRD kind of notification:
	// the breaker is still tripped, but the down count has materially
	// worsened since the last notification. Mutually exclusive with
	// Recovered - both false means the original trip notification.
	Updated  bool
	TenantID uuid.UUID
	// DownCount / EligibleCount are the counts AT THE MOMENT the breaker
	// transitioned (GetTenantAppAlertRatio) - the authoritative numbers for
	// "how bad is this", not merely the sites that changed on this tick.
	DownCount     int
	EligibleCount int
	// SuppressedSites names sites whose individual app-down notification
	// this breaker is collapsing, so the notification body says what was
	// suppressed instead of leaving the operator to guess ("Include the
	// count and a clear statement of what was suppressed so nothing is
	// silently swallowed"). On the initial trip this is exactly this tick's
	// fires (nothing was suppressed before). On an Updated notification
	// (Fix 3) this is instead the LIVE, currently-down set read fresh via
	// ListTenantAppDownSites - never merely the tick that happened to
	// trigger the update - because an update can fire several ticks after
	// the sites it should describe actually went down (the min-interval
	// throttle delays it). DownCount/EligibleCount are always the true,
	// complete counts regardless of how many names this list holds
	// (bounded, for a large tenant).
	SuppressedSites []string
	FiredAt         time.Time
}
