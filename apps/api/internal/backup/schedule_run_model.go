package backup

import (
	"time"

	"github.com/google/uuid"
)

// ScheduleRun status values. Mirrors restore_run status values but includes
// 'scheduled' (pre-inserted next-run row) and 'skipped' (site not enrollable).
const (
	ScheduleRunStatusScheduled = "scheduled"
	ScheduleRunStatusQueued    = "queued"
	ScheduleRunStatusRunning   = "running"
	ScheduleRunStatusCompleted = "completed"
	ScheduleRunStatusFailed    = "failed"
	ScheduleRunStatusSkipped   = "skipped"
	ScheduleRunStatusCanceled  = "canceled"
)

// terminalScheduleRunStatuses is the closed set of statuses that finalize a
// schedule run; a run in any of these states will not be updated further.
var terminalScheduleRunStatuses = map[string]struct{}{
	ScheduleRunStatusCompleted: {},
	ScheduleRunStatusFailed:    {},
	ScheduleRunStatusSkipped:   {},
	ScheduleRunStatusCanceled:  {},
}

// IsTerminalScheduleRunStatus reports whether status is a terminal schedule
// run status (completed / failed / skipped / canceled).
func IsTerminalScheduleRunStatus(status string) bool {
	_, ok := terminalScheduleRunStatuses[status]
	return ok
}

// ScheduleRun is one row from backup_schedule_runs: the durable record of a
// single scheduled-backup fire (pre-inserted as 'scheduled', then advanced
// through queued / running / completed | failed | skipped | canceled).
type ScheduleRun struct {
	ID           uuid.UUID
	TenantID     uuid.UUID
	SiteID       uuid.UUID
	ScheduleID   uuid.UUID
	SnapshotID   *uuid.UUID // nil until the snapshot is created
	ScheduledFor time.Time
	Status       string
	Kind         string
	Error        *string
	TriggeredBy  *string
	CreatedAt    time.Time
	StartedAt    *time.Time
	FinishedAt   *time.Time
	UpdatedAt    time.Time
}
