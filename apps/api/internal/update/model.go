// Package update implements the M3 bulk plugin/theme/core update feature: an
// operator creates an update run targeting a selection of sites and items; the
// control plane fans the work out into per-(site,item) tasks executed by a
// River worker that snapshots, applies the update via a signed CP->agent
// command, health-probes the site, and auto-rolls-back on failure. Live
// progress is streamed over SSE from an in-process pub/sub hub.
//
// Every query is tenant-scoped both explicitly (tenant_id in the WHERE clause)
// and by Postgres RLS (the app.tenant_id policy on update_runs/update_tasks).
package update

import (
	"time"

	"github.com/google/uuid"
)

// Run statuses.
const (
	RunPending   = "pending"
	RunRunning   = "running"
	RunCompleted = "completed"
)

// Task statuses.
const (
	TaskPending    = "pending"
	TaskRunning    = "running"
	TaskSucceeded  = "succeeded"
	TaskFailed     = "failed"
	TaskRolledBack = "rolled_back"
	TaskSkipped    = "skipped"
)

// Target types (mirror agentcmd.TargetPlugin/Theme/Core).
const (
	TargetPlugin = "plugin"
	TargetTheme  = "theme"
	TargetCore   = "core"
)

// Run is an update run: a tenant-scoped unit grouping per-(site,item) tasks.
type Run struct {
	ID          uuid.UUID
	TenantID    uuid.UUID
	CreatedBy   *uuid.UUID
	Status      string
	DryRun      bool
	ScheduledAt *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Task is one unit of work: apply one item on one site.
type Task struct {
	ID             uuid.UUID
	RunID          uuid.UUID
	TenantID       uuid.UUID
	SiteID         uuid.UUID
	TargetType     string
	TargetSlug     string
	DesiredVersion string
	FromVersion    string
	ToVersion      string
	Status         string
	Detail         string
	Error          string
	StartedAt      *time.Time
	FinishedAt     *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Item is one requested update target within a CreateRunInput.
type Item struct {
	Type    string `json:"type" validate:"required,oneof=plugin theme core"`
	Slug    string `json:"slug" validate:"max=200"`
	Version string `json:"version" validate:"max=64"`
}

// CreateRunInput is the validated input for creating an update run. Exactly one
// of SiteIDs or Tag selects the target sites.
type CreateRunInput struct {
	TenantID    uuid.UUID
	CreatedBy   uuid.UUID
	SiteIDs     []uuid.UUID
	Tag         string
	Items       []Item `validate:"required,min=1,max=200,dive"`
	DryRun      bool
	ScheduledAt *time.Time
}

// terminal reports whether a task status is a final state.
func terminal(status string) bool {
	switch status {
	case TaskSucceeded, TaskFailed, TaskRolledBack, TaskSkipped:
		return true
	default:
		return false
	}
}
