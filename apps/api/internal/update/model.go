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
	"regexp"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
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

// versionPattern bounds the update version to "latest" or a conservative
// version-pin charset (leading alnum then a small safe set). It deliberately
// forbids whitespace, ';', '&', and '--' so a value cannot smuggle extra
// arguments into the agent's WP-CLI invocation (e.g. "latest --activate" or
// "1.0; rm -rf"). The agent re-validates as defense-in-depth (ADR contract).
var versionPattern = regexp.MustCompile(`^(latest|[0-9][0-9A-Za-z.\-]{0,63})$`)

// slugPattern bounds plugin/theme slugs to a safe filesystem-ish charset,
// optionally one path segment (e.g. "akismet" or "akismet/akismet"). No spaces,
// shell metacharacters, or path traversal sequences are allowed.
var slugPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)?$`)

// validateItems enforces the CP-side safe charset on each update item's version
// and slug AFTER normalization, returning a KindValidation (HTTP 422) domain
// error on the first offending value. This is the control-plane guard against
// argument injection into the agent's WP-CLI; the agent validates again.
func validateItems(items []Item) error {
	for _, it := range items {
		if len(it.Slug) > 200 || !slugPattern.MatchString(it.Slug) {
			return domain.Validation("invalid_slug", "update item slug contains an unsafe value: "+it.Slug)
		}
		if it.Version != "" && !versionPattern.MatchString(it.Version) {
			return domain.Validation("invalid_version", "update item version contains an unsafe value: "+it.Version)
		}
	}
	return nil
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
