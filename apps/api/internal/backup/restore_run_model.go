package backup

import (
	"time"

	"github.com/google/uuid"
)

// Restore run status values (parallel to snapshot statuses but on restore_runs).
const (
	RestoreStatusQueued     = "queued"
	RestoreStatusRunning    = "running"
	RestoreStatusCompleted  = "completed"
	RestoreStatusFailed     = "failed"
	RestoreStatusRolledBack = "rolled_back"
)

// restorePhases is the closed set of phases that belong to the restore engine
// (as opposed to the backup engine). A progress POST that belongs to this set
// triggers restore_run_events persistence.
//
// Note: "completed" and "failed" appear in BOTH the backup AND restore engines.
// We detect which side they belong to by whether an active restore run exists
// for the snapshot at the time the phase is posted (see RecordProgress).
var restorePhases = map[string]struct{}{
	"preflight":          {},
	"download_artifacts": {},
	"verify_artifacts":   {},
	"maintenance_on":     {},
	"stage_files":        {},
	"swap_files":         {},
	"restore_db":         {},
	"migrate_db":         {},
	"url_rewrite":        {},
	"swap_db":            {},
	"post_hooks":         {},
	"maintenance_off":    {},
	"cleanup":            {},
	"rolled_back":        {},
	// These two are shared with backup but are treated as restore-terminal
	// phases when an active restore run is present.
	"completed": {},
	"failed":    {},
}

// terminalRestorePhases is the subset of restorePhases that finalize the run.
var terminalRestorePhases = map[string]struct{}{
	"completed":   {},
	"failed":      {},
	"rolled_back": {},
}

// isRestorePhase reports whether phase belongs to the restore engine.
func isRestorePhase(phase string) bool {
	_, ok := restorePhases[phase]
	return ok
}

// isTerminalRestorePhase reports whether phase is a terminal restore phase
// (completed / failed / rolled_back).
func isTerminalRestorePhase(phase string) bool {
	_, ok := terminalRestorePhases[phase]
	return ok
}

// RestoreRun is one row from restore_runs: the durable record of a restore
// attempt, created when the operator calls POST /backups/:id/restore.
type RestoreRun struct {
	ID           uuid.UUID
	TenantID     uuid.UUID
	SiteID       uuid.UUID
	SnapshotID   uuid.UUID
	Mode         string
	Components   []string
	Selection    []byte // raw JSONB snapshot of the RestoreSelection
	Status       string
	CurrentPhase string
	Error        string
	TriggeredBy  string
	CreatedAt    time.Time
	StartedAt    *time.Time
	FinishedAt   *time.Time
	UpdatedAt    time.Time
}

// RestoreRunEvent is one row from restore_run_events: a single phase event
// appended by RecordProgress when a restore phase POST arrives.
type RestoreRunEvent struct {
	ID           int64
	TenantID     uuid.UUID
	RestoreRunID uuid.UUID
	Phase        string
	Status       string
	Message      string
	Detail       []byte // raw JSONB (the phase_detail map)
	OccurredAt   time.Time
}
