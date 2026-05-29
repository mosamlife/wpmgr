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
	UpdatedAt      time.Time
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
