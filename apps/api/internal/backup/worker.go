package backup

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

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
	// jobTimeout overrides River's default 60s per-job context deadline. The
	// agent processes a real-site backup inline (dump+chunk+encrypt+upload) and
	// easily exceeds a minute — set this to ≥ the backup HTTPTimeout so the HTTP
	// client gets the chance to fire its (clearer) per-attempt timeout first.
	// Zero falls back to river.Config.JobTimeout.
	jobTimeout time.Duration
}

// NewBackupWorker builds the backup worker. jobTimeout overrides River's
// default 60s per-job deadline; pass cfg.Backup.HTTPTimeout + a small buffer.
func NewBackupWorker(svc *Service, cmd Commander, rec *audit.Recorder, logger *slog.Logger, cpBaseURL string, jobTimeout time.Duration) *BackupWorker {
	if logger == nil {
		logger = slog.Default()
	}
	return &BackupWorker{svc: svc, cmd: cmd, audit: rec, logger: logger, cpBaseURL: strings.TrimRight(cpBaseURL, "/"), jobTimeout: jobTimeout}
}

// Timeout overrides River's default per-job context deadline (60s) for the
// backup worker. Returning a positive duration makes River use it instead of
// river.Config.JobTimeout; returning 0 keeps the default. River documents that
// returning -1 disables the deadline entirely — we intentionally do NOT do that
// (a wedged backup must eventually error out so River can retry).
func (w *BackupWorker) Timeout(*river.Job[BackupArgs]) time.Duration { return w.jobTimeout }

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
		ProgressEndpoint: w.progressEndpoint(snap.ID),
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

func (w *BackupWorker) progressEndpoint(snapshotID uuid.UUID) string {
	if w.cpBaseURL == "" {
		return ""
	}
	return fmt.Sprintf("%s/agent/v1/backups/%s/progress", w.cpBaseURL, snapshotID)
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

// RestoreWorker assembles the presigned-GET restore plan + ordered manifest
// and dispatches the signed `restore` command (ADR-034 v0.8.1 wire shape: per-
// artifact-part `logical_path` with presigned GET URLs for each PLAIN chunk).
//
// The worker is SHORT (~1 s — it mints the plan and hands it off). The agent
// does the heavy lifting (download, verify, reassemble, swap) over MINUTES and
// posts phase events back to the existing /agent/v1/backups/:id/progress
// endpoint, which fans them out via the backup SSE hub (same UI channel as
// backup progress).
//
// V0 NOTE: we do NOT add a new column for the restore state. The snapshot's
// JSONB progress field (`progress.phase` + `progress.phase_detail.restore_id`)
// is the canonical place the UI watches; the audit log records the actual
// restore_id for support diagnostics.
type RestoreWorker struct {
	river.WorkerDefaults[RestoreArgs]
	svc    *Service
	cmd    Commander
	audit  *audit.Recorder
	logger *slog.Logger
	// cpBaseURL is the control-plane base URL the agent uses for the progress
	// callback (the same /agent/v1/backups/{id}/progress endpoint backups use).
	cpBaseURL string
	// jobTimeout — same rationale as BackupWorker.jobTimeout: the agent ACKs
	// the dispatch fast but the HTTP round-trip may still exceed 60s on a
	// slow site; the actual long-running restore proceeds async on the agent.
	jobTimeout time.Duration
}

// NewRestoreWorker builds the restore worker. jobTimeout overrides River's
// default 60s per-job deadline; pass cfg.Backup.HTTPTimeout + a small buffer.
// cpBaseURL is the CP origin the agent posts progress events back to (empty
// disables the callback — the agent will not be able to publish progress).
func NewRestoreWorker(svc *Service, cmd Commander, rec *audit.Recorder, logger *slog.Logger, cpBaseURL string, jobTimeout time.Duration) *RestoreWorker {
	if logger == nil {
		logger = slog.Default()
	}
	return &RestoreWorker{svc: svc, cmd: cmd, audit: rec, logger: logger, cpBaseURL: strings.TrimRight(cpBaseURL, "/"), jobTimeout: jobTimeout}
}

// Timeout overrides River's default per-job context deadline for the restore
// worker. See BackupWorker.Timeout for the rationale.
func (w *RestoreWorker) Timeout(*river.Job[RestoreArgs]) time.Duration { return w.jobTimeout }

// Work assembles and dispatches one restore. The flow:
//
//  1. Mint a fresh `restore_id` (CP-side dedup key for this attempt).
//  2. Resolve the snapshot's manifest entries and presign GET URLs for each
//     chunk via PlanRestore (which builds the new ADR-034 wire shape).
//  3. Emit ONE `preflight` progress event so the SSE hub fans out a
//     "Worker dispatching restore" tick to the UI BEFORE the (slow) agent
//     POST returns.
//  4. POST the signed `restore` command to the agent and wait for the ACK.
//  5. On ACK ok=true: return nil — the WORKER is done; the agent now drives
//     completion via /progress events through the existing endpoint.
//  6. On agent refusal or transport error: emit a `failed` progress event
//     (which the SSE hub fans out + the existing service code records to
//     audit) so the UI surfaces the failure without waiting for a watchdog.
//
// Transport errors are returned (River retries with a fresh JWT). Agent-side
// refusals are recorded as terminal and return nil.
func (w *RestoreWorker) Work(ctx context.Context, job *river.Job[RestoreArgs]) error {
	a := job.Args
	sel := RestoreSelection{Full: a.Full, Paths: a.Paths, DBTables: a.DBTables}

	// CP-generated dedup key. Recorded in audit and surfaced to the UI via the
	// preflight progress event's phase_detail.restore_id.
	restoreID := uuid.NewString()
	progressEndpoint := w.progressEndpoint(a.SnapshotID)

	plan, snap, si, err := w.svc.PlanRestore(ctx, a.TenantID, a.SnapshotID, sel, restoreID, progressEndpoint)
	if err != nil {
		return err
	}
	if !si.Enrolled {
		w.recordAudit(ctx, snap, ActionRestoreFailed, map[string]any{"restore_id": restoreID, "error": "site not enrolled"})
		// Best-effort fan-out so the UI shows the failure immediately.
		_, _ = w.svc.RecordProgress(ctx, snap.TenantID, snap.ID, "failed", map[string]any{
			"restore_id": restoreID,
			"error":      "site not enrolled",
		})
		return nil
	}

	w.recordAudit(ctx, snap, ActionRestoreStarted, map[string]any{
		"restore_id":  restoreID,
		"kind":        snap.Kind,
		"entry_count": len(plan.Manifest.Entries),
	})

	// Emit ONE "preflight" progress tick so the UI sees the dispatch BEFORE the
	// agent's first phase event lands. Carrying restore_id in phase_detail lets
	// the frontend key the restore UI element off it.
	if _, perr := w.svc.RecordProgress(ctx, snap.TenantID, snap.ID, "preflight", map[string]any{
		"restore_id":  restoreID,
		"step":        "cp_dispatch",
		"entry_count": len(plan.Manifest.Entries),
	}); perr != nil {
		// Best-effort: a progress publish failure must not block the dispatch.
		w.logger.Warn("restore preflight progress publish failed",
			slog.String("snapshot_id", snap.ID.String()),
			slog.String("restore_id", restoreID),
			slog.Any("error", perr))
	}

	resp, err := w.cmd.Restore(ctx, snap.SiteID, si.URL, plan)
	if err != nil {
		// Transport / SSRF / agent-reject: retryable infra error. Surface the
		// in-flight failure on the SSE channel so the UI does not hang waiting
		// for the watchdog. River will retry with a fresh JWT.
		_, _ = w.svc.RecordProgress(ctx, snap.TenantID, snap.ID, "failed", map[string]any{
			"restore_id": restoreID,
			"error":      err.Error(),
		})
		return fmt.Errorf("restore command to agent failed: %w", err)
	}
	if !resp.OK {
		// Agent refused the dispatch (e.g. another restore in flight). Terminal.
		w.recordAudit(ctx, snap, ActionRestoreFailed, map[string]any{
			"restore_id": restoreID,
			"error":      "agent refused the restore: " + resp.Log,
		})
		_, _ = w.svc.RecordProgress(ctx, snap.TenantID, snap.ID, "failed", map[string]any{
			"restore_id": restoreID,
			"error":      "agent refused: " + resp.Log,
		})
		return nil
	}
	// Agent ACKed; it will now drive completion via /progress events on the
	// existing endpoint. The worker is done.
	return nil
}

// progressEndpoint mirrors BackupWorker.progressEndpoint: the agent POSTs
// restore phase events to the SAME /agent/v1/backups/{snapshotId}/progress
// endpoint backups already use. The CP /progress handler validates the phase
// against allowedProgressPhases (which now includes the restore set), persists
// to backup_snapshots.progress, and fans out to the existing backup SSE hub.
func (w *RestoreWorker) progressEndpoint(snapshotID uuid.UUID) string {
	if w.cpBaseURL == "" {
		return ""
	}
	return fmt.Sprintf("%s/agent/v1/backups/%s/progress", w.cpBaseURL, snapshotID)
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
// progress watchdog (periodic) — M5.6 / ADR-032
// ----------------------------------------------------------------------------

// ProgressWatchdogArgs is the periodic-job arg type for the M5.6 watchdog.
type ProgressWatchdogArgs struct{}

// Kind implements river.JobArgs.
func (ProgressWatchdogArgs) Kind() string { return "backup_progress_watchdog" }

// ProgressWatchdogWorker enumerates running snapshots whose phpbu runner has
// gone silent for longer than the configured threshold and fails them with a
// `stalled` error so the UI surfaces the dead run and the operator (or the
// schedule's next tick) can retry. This defends against runner crashes,
// host-side OOM kills, and `proc_open` losses that leave the snapshot row
// stuck in `running` forever.
type ProgressWatchdogWorker struct {
	river.WorkerDefaults[ProgressWatchdogArgs]
	svc       *Service
	threshold time.Duration
	logger    *slog.Logger
}

// NewProgressWatchdogWorker builds the watchdog. threshold should be generous
// enough to cover the agent's worst-case time-between-phase-events (the longest
// silent gap is between `compressing_files` and the first `uploading` chunk —
// that's the age-encrypt pass, which on a multi-GB site can be a couple
// minutes). 120s is the recommended default.
func NewProgressWatchdogWorker(svc *Service, threshold time.Duration, logger *slog.Logger) *ProgressWatchdogWorker {
	if logger == nil {
		logger = slog.Default()
	}
	if threshold <= 0 {
		threshold = 120 * time.Second
	}
	return &ProgressWatchdogWorker{svc: svc, threshold: threshold, logger: logger}
}

// Work runs one watchdog pass. The list query is cross-tenant under app.agent;
// each fail is tenant-scoped.
func (w *ProgressWatchdogWorker) Work(ctx context.Context, _ *river.Job[ProgressWatchdogArgs]) error {
	stalled, err := w.svc.ListStalledRunningSnapshots(ctx, w.threshold)
	if err != nil {
		w.logger.Warn("backup progress watchdog list error", slog.Any("error", err))
		return err
	}
	if len(stalled) == 0 {
		return nil
	}
	failed := 0
	for _, s := range stalled {
		// Stamp `stalled` with the silent-gap detail so the UI can render it.
		// FailSnapshot moves status → failed and sets finished_at; subsequent
		// passes won't pick the row up (the WHERE filter is status='running').
		msg := fmt.Sprintf("stalled — no progress for >%s", w.threshold)
		if _, err := w.svc.FailSnapshot(ctx, s.TenantID, s.ID, msg); err != nil {
			w.logger.Warn("backup progress watchdog fail error",
				slog.String("snapshot_id", s.ID.String()),
				slog.String("tenant_id", s.TenantID.String()),
				slog.Any("error", err))
			continue
		}
		failed++
		w.logger.Info("backup snapshot marked stalled",
			slog.String("snapshot_id", s.ID.String()),
			slog.String("tenant_id", s.TenantID.String()),
			slog.String("site_id", s.SiteID.String()),
			slog.Duration("threshold", w.threshold))
	}
	if failed > 0 {
		w.logger.Info("backup progress watchdog pass", slog.Int("stalled_failed", failed), slog.Int("found", len(stalled)))
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
