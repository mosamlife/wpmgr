// Package security implements the S2 Login Protection + IP store domain on the
// control-plane side.
//
// It stores per-site login-protection config, pushes it to the agent via the
// signed `sync_security_config` command, ingests the agent's login-event batch
// (POST /agent/v1/security/login-events), and exposes an unblock-IP action.
package security

import (
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
)

// SecurityConfig is the per-site login-protection configuration stored in
// site_security_config and pushed to the agent via the sync_security_config
// command.
type SecurityConfig struct {
	TenantID   uuid.UUID
	SiteID     uuid.UUID
	Mode       string                      // "disabled" | "audit" | "protect"
	Thresholds agentcmd.SecurityThresholds // inline struct so the wire contract is shared
	IPHeader   string
	AllowCIDRs []string
	DenyCIDRs  []string
	UpdatedAt  time.Time
}

// LoginEventStatus is the agent's numeric status column.
type LoginEventStatus int16

const (
	StatusFailure LoginEventStatus = 1
	StatusSuccess LoginEventStatus = 2
	StatusBlocked LoginEventStatus = 3
)

// LoginEvent is one stored login-attempt row from the agent.
type LoginEvent struct {
	ID           int64
	TenantID     uuid.UUID
	SiteID       uuid.UUID
	AgentEventID int64
	IP           string
	Status       LoginEventStatus
	Category     string
	Username     string
	RequestID    string
	OccurredAt   time.Time
	IngestedAt   time.Time
}
