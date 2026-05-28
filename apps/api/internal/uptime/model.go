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
	UpdatedAt     time.Time
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
)

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
