package backup

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
)

// Audit action names for the backup/restore lifecycle.
const (
	ActionBackupStarted    = "backup.started"
	ActionBackupCompleted  = "backup.completed"
	ActionBackupFailed     = "backup.failed"
	ActionRestoreStarted   = "restore.started"
	ActionRestoreCompleted = "restore.completed"
	ActionRestoreFailed    = "restore.failed"
	ActionScheduleChanged  = "backup.schedule.changed"
)

// Commander sends signed CP->agent backup/restore commands. siteID is bound into
// the command JWT's aud claim so a captured token cannot be replayed against a
// different tenant's site.
type Commander interface {
	Backup(ctx context.Context, siteID uuid.UUID, siteURL string, req agentcmd.BackupRequest) (agentcmd.BackupResponse, error)
	Restore(ctx context.Context, siteID uuid.UUID, siteURL string, req agentcmd.RestoreRequest) (agentcmd.RestoreResponse, error)
}

// ----------------------------------------------------------------------------
// backup job
// ----------------------------------------------------------------------------

// BackupArgs is the River job payload for one backup. It carries only IDs; the
// worker re-reads authoritative state (tenant-scoped) from the DB.
type BackupArgs struct {
	TenantID   uuid.UUID `json:"tenant_id"`
	SnapshotID uuid.UUID `json:"snapshot_id"`
}

// Kind implements river.JobArgs.
func (BackupArgs) Kind() string { return "backup_snapshot" }

// BackupWorker dispatches the signed `backup` command to the site's agent. The
// agent then chunks, encrypts (client-side, age), and uploads ciphertext via
// presigned PUT URLs it requests from the CP callback, and submits the manifest
// to the CP callback. The CP records snapshot+manifest+chunks at manifest time;
// this worker only kicks off and marks the snapshot running/failed.
type BackupWorker struct {
	river.WorkerDefaults[BackupArgs]
	svc    *Service
	cmd    Commander
	audit  *audit.Recorder
	logger *slog.Logger
	// cpBaseURL is the control-plane base URL the agent uses for the presign and
	// manifest callbacks (e.g. https://cp.example.com). Empty disables the
	// callbacks (the agent must be told where to call back).
	cpBaseURL string
}

// NewBackupWorker builds the backup worker.
func NewBackupWorker(svc *Service, cmd Commander, rec *audit.Recorder, logger *slog.Logger, cpBaseURL string) *BackupWorker {
	if logger == nil {
		logger = slog.Default()
	}
	return &BackupWorker{svc: svc, cmd: cmd, audit: rec, logger: logger, cpBaseURL: strings.TrimRight(cpBaseURL, "/")}
}

// Work dispatches one backup. A transient transport error returns the error so
// River retries; an agent refusal marks the snapshot failed (terminal).
func (w *BackupWorker) Work(ctx context.Context, job *river.Job[BackupArgs]) error {
	a := job.Args
	snap, err := w.svc.repo.GetSnapshot(ctx, a.TenantID, a.SnapshotID)
	if err != nil {
		return err
	}
	if snap.Status == StatusCompleted || snap.Status == StatusFailed {
		return nil // already terminal (retry/dup).
	}

	si, err := w.svc.SiteForSnapshot(ctx, a.TenantID, snap)
	if err != nil {
		return w.fail(ctx, snap, "site unresolved: "+err.Error())
	}
	if !si.Enrolled {
		return w.fail(ctx, snap, "site is not enrolled")
	}
	if snap.AgeRecipient == "" {
		return w.fail(ctx, snap, "no age recipient on snapshot")
	}

	running, err := w.svc.MarkRunning(ctx, a.TenantID, a.SnapshotID)
	if err != nil {
		return err
	}
	w.recordAudit(ctx, running, ActionBackupStarted, nil)

	req := agentcmd.BackupRequest{
		SnapshotID:       snap.ID.String(),
		Kind:             snap.Kind,
		AgeRecipient:     snap.AgeRecipient, // PUBLIC recipient only — NEVER a key.
		ChunkBytes:       agentcmd.ChunkBytes,
		PresignEndpoint:  w.presignEndpoint(snap.ID),
		ManifestEndpoint: w.manifestEndpoint(snap.ID),
	}
	resp, err := w.cmd.Backup(ctx, snap.SiteID, si.URL, req)
	if err != nil {
		// Transport/SSRF/agent-reject: retryable infra error.
		return fmt.Errorf("backup command to agent failed: %w", err)
	}
	if !resp.OK {
		return w.fail(ctx, snap, "agent refused the backup: "+resp.Detail)
	}
	// The agent accepted the job; completion happens when it submits the manifest
	// (SubmitManifest completes the snapshot). Audit completion is recorded there.
	return nil
}

func (w *BackupWorker) fail(ctx context.Context, snap Snapshot, msg string) error {
	failed, err := w.svc.FailSnapshot(ctx, snap.TenantID, snap.ID, msg)
	if err != nil {
		return err
	}
	w.recordAudit(ctx, failed, ActionBackupFailed, map[string]any{"error": msg})
	return nil // terminal failure recorded; the River job succeeds.
}

func (w *BackupWorker) presignEndpoint(snapshotID uuid.UUID) string {
	if w.cpBaseURL == "" {
		return ""
	}
	return fmt.Sprintf("%s/agent/v1/backups/%s/presign", w.cpBaseURL, snapshotID)
}

func (w *BackupWorker) manifestEndpoint(snapshotID uuid.UUID) string {
	if w.cpBaseURL == "" {
		return ""
	}
	return fmt.Sprintf("%s/agent/v1/backups/%s/manifest", w.cpBaseURL, snapshotID)
}

func (w *BackupWorker) recordAudit(ctx context.Context, snap Snapshot, action string, extra map[string]any) {
	if w.audit == nil {
		return
	}
	meta := map[string]any{
		"site_id": snap.SiteID.String(),
		"kind":    snap.Kind,
		"status":  snap.Status,
	}
	for k, v := range extra {
		meta[k] = v
	}
	_, _ = w.audit.Record(ctx, audit.Event{
		TenantID:   snap.TenantID,
		ActorType:  audit.ActorSystem,
		Action:     action,
		TargetType: "backup_snapshot",
		TargetID:   snap.ID.String(),
		Metadata:   meta,
	})
}

// ----------------------------------------------------------------------------
// restore job
// ----------------------------------------------------------------------------

// RestoreArgs is the River job payload for one restore.
type RestoreArgs struct {
	TenantID   uuid.UUID `json:"tenant_id"`
	SnapshotID uuid.UUID `json:"snapshot_id"`
	Full       bool      `json:"full"`
	Paths      []string  `json:"paths,omitempty"`
	DBTables   []string  `json:"db_tables,omitempty"`
}

// Kind implements river.JobArgs.
func (RestoreArgs) Kind() string { return "backup_restore" }

// RestoreWorker assembles the presigned-GET restore plan + ordered manifest and
// dispatches the signed `restore` command. The agent downloads, verifies
// BLAKE3, decrypts with its own age identity, and reassembles. NO decryption
// key is ever sent.
type RestoreWorker struct {
	river.WorkerDefaults[RestoreArgs]
	svc    *Service
	cmd    Commander
	audit  *audit.Recorder
	logger *slog.Logger
}

// NewRestoreWorker builds the restore worker.
func NewRestoreWorker(svc *Service, cmd Commander, rec *audit.Recorder, logger *slog.Logger) *RestoreWorker {
	if logger == nil {
		logger = slog.Default()
	}
	return &RestoreWorker{svc: svc, cmd: cmd, audit: rec, logger: logger}
}

// Work assembles and dispatches one restore. Transport errors retry; an agent
// reporting failure (or a failed integrity check) records a restore.failed
// audit event and returns nil (terminal — retrying would just re-download).
func (w *RestoreWorker) Work(ctx context.Context, job *river.Job[RestoreArgs]) error {
	a := job.Args
	sel := RestoreSelection{Full: a.Full, Paths: a.Paths, DBTables: a.DBTables}

	plan, snap, si, err := w.svc.PlanRestore(ctx, a.TenantID, a.SnapshotID, sel)
	if err != nil {
		return err
	}
	if !si.Enrolled {
		w.recordAudit(ctx, snap, ActionRestoreFailed, map[string]any{"error": "site not enrolled"})
		return nil
	}
	w.recordAudit(ctx, snap, ActionRestoreStarted, map[string]any{"entries": len(plan.Entries)})

	resp, err := w.cmd.Restore(ctx, snap.SiteID, si.URL, plan)
	if err != nil {
		return fmt.Errorf("restore command to agent failed: %w", err)
	}
	if !resp.OK || !resp.Verified {
		w.recordAudit(ctx, snap, ActionRestoreFailed, map[string]any{
			"ok": resp.OK, "verified": resp.Verified, "log": resp.Log,
		})
		return nil
	}
	w.recordAudit(ctx, snap, ActionRestoreCompleted, map[string]any{
		"restored_entries": resp.RestoredEntries, "verified": resp.Verified,
	})
	return nil
}

func (w *RestoreWorker) recordAudit(ctx context.Context, snap Snapshot, action string, extra map[string]any) {
	if w.audit == nil {
		return
	}
	meta := map[string]any{"site_id": snap.SiteID.String(), "kind": snap.Kind}
	for k, v := range extra {
		meta[k] = v
	}
	_, _ = w.audit.Record(ctx, audit.Event{
		TenantID:   snap.TenantID,
		ActorType:  audit.ActorSystem,
		Action:     action,
		TargetType: "backup_snapshot",
		TargetID:   snap.ID.String(),
		Metadata:   meta,
	})
}

// ----------------------------------------------------------------------------
// retention GC job (periodic)
// ----------------------------------------------------------------------------

// GCArgs is the River job payload for the periodic retention GC. It has no
// fields; the worker enumerates tenants itself.
type GCArgs struct{}

// Kind implements river.JobArgs.
func (GCArgs) Kind() string { return "backup_retention_gc" }

// GCWorker runs the retention GC across all tenants.
type GCWorker struct {
	river.WorkerDefaults[GCArgs]
	svc    *Service
	logger *slog.Logger
}

// NewGCWorker builds the GC worker.
func NewGCWorker(svc *Service, logger *slog.Logger) *GCWorker {
	if logger == nil {
		logger = slog.Default()
	}
	return &GCWorker{svc: svc, logger: logger}
}

// Work runs one GC pass.
func (w *GCWorker) Work(ctx context.Context, _ *river.Job[GCArgs]) error {
	snaps, chunks, err := w.svc.RunRetentionGCAllTenants(ctx)
	if err != nil {
		w.logger.Warn("backup retention GC error", slog.Any("error", err))
		return err
	}
	if snaps > 0 || chunks > 0 {
		w.logger.Info("backup retention GC", slog.Int("snapshots_deleted", snaps), slog.Int("chunks_deleted", chunks))
	}
	return nil
}

// ----------------------------------------------------------------------------
// scheduler job (periodic)
// ----------------------------------------------------------------------------

// ScheduleArgs is the River job payload for the periodic backup scheduler. It
// has no fields; the worker enumerates due schedules itself.
type ScheduleArgs struct{}

// Kind implements river.JobArgs.
func (ScheduleArgs) Kind() string { return "backup_scheduler" }

// ScheduleWorker enqueues due backups from backup_schedules.
type ScheduleWorker struct {
	river.WorkerDefaults[ScheduleArgs]
	svc    *Service
	logger *slog.Logger
}

// NewScheduleWorker builds the scheduler worker.
func NewScheduleWorker(svc *Service, logger *slog.Logger) *ScheduleWorker {
	if logger == nil {
		logger = slog.Default()
	}
	return &ScheduleWorker{svc: svc, logger: logger}
}

// Work enqueues a backup for each due schedule and advances its next_run_at.
func (w *ScheduleWorker) Work(ctx context.Context, _ *river.Job[ScheduleArgs]) error {
	due, err := w.svc.DueSchedules(ctx, 200)
	if err != nil {
		return err
	}
	for _, sched := range due {
		if eerr := w.svc.EnqueueScheduledBackup(ctx, sched); eerr != nil {
			// Per-schedule error (e.g. site not enrolled): logged, schedule already
			// advanced by EnqueueScheduledBackup; continue with the next.
			w.logger.Info("backup schedule skipped",
				slog.String("schedule_id", sched.ID.String()),
				slog.Any("reason", eerr))
		}
	}
	return nil
}
