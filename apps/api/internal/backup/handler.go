package backup

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-faster/jx"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/api/gen"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// sseHeartbeat is the interval between SSE keep-alive comment lines.
const sseHeartbeat = 15 * time.Second

// sseMaxLifetime is the upper bound on how long a single SSE connection stays
// open. The handler does NOT close on terminal snapshot.status (see comment on
// events() below) — the browser closes its own EventSource when it observes a
// terminal `phase` (completed/failed) in `use-backup-stream.ts`. This timeout
// is a defence-in-depth safety net: if the client disconnects uncleanly and
// the TCP half-close doesn't surface as ctx.Done() promptly (proxy in the
// path, idle connection killers, etc.), we still bound the goroutine. 30 min
// comfortably exceeds the worst real restore (~10 min on a fat WP site).
const sseMaxLifetime = 30 * time.Minute

// Handler serves the operator/viewer-facing backup endpoints under /api/v1.
// Mutations (create backup, restore, put schedule) require operator+; reads
// (list, get, get schedule, events) require viewer+.
type Handler struct {
	svc   *Service
	hub   *Hub
	audit *audit.Recorder
}

// NewHandler builds a backup Handler. The hub may be nil in environments that
// don't need live SSE (the events route will then 500 on connect, mirroring the
// update package's "sse_unsupported" path).
func NewHandler(svc *Service, hub *Hub, rec *audit.Recorder) *Handler {
	return &Handler{svc: svc, hub: hub, audit: rec}
}

// Register mounts the backup routes on the /api/v1 router group.
func (h *Handler) Register(r *gin.RouterGroup) {
	r.POST("/sites/:siteId/backups", authz.RequirePermission(authz.PermSiteWrite), h.createBackup)
	r.GET("/sites/:siteId/backups", authz.RequirePermission(authz.PermSiteRead), h.listBackups)
	r.GET("/backups/:snapshotId", authz.RequirePermission(authz.PermSiteRead), h.getBackup)
	r.GET("/backups/:snapshotId/events", authz.RequirePermission(authz.PermSiteRead), h.events)
	r.POST("/backups/:snapshotId/restore", authz.RequirePermission(authz.PermSiteWrite), h.createRestore)
	r.GET("/sites/:siteId/backup-schedule", authz.RequirePermission(authz.PermSiteRead), h.getSchedule)
	r.PUT("/sites/:siteId/backup-schedule", authz.RequirePermission(authz.PermSiteWrite), h.putSchedule)
}

func (h *Handler) createBackup(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	siteID, ok := uuidParam(c, "siteId", "invalid_site_id")
	if !ok {
		return
	}
	var req gen.BackupCreate
	if err := c.ShouldBindJSON(&req); err != nil && c.Request.ContentLength > 0 {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
		return
	}
	kind := string(req.Kind.Or(gen.BackupCreateKindFull))

	var createdBy uuid.UUID
	if p, ok := domain.PrincipalFromContext(c.Request.Context()); ok {
		createdBy = p.UserID
	}

	snap, err := h.svc.CreateBackup(c.Request.Context(), tenantID, siteID, createdBy, kind)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	out := toAPISnapshot(snap)
	c.JSON(http.StatusCreated, &out)
}

func (h *Handler) listBackups(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	siteID, ok := uuidParam(c, "siteId", "invalid_site_id")
	if !ok {
		return
	}
	snaps, err := h.svc.ListSnapshots(c.Request.Context(), tenantID, siteID, parseInt32(c.Query("limit"), 50), parseInt32(c.Query("offset"), 0))
	if err != nil {
		httpx.Error(c, err)
		return
	}
	items := make([]gen.BackupSnapshot, 0, len(snaps))
	for _, s := range snaps {
		items = append(items, toAPISnapshot(s))
	}
	c.JSON(http.StatusOK, gen.BackupSnapshotList{Items: items})
}

func (h *Handler) getBackup(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	snapshotID, ok := uuidParam(c, "snapshotId", "invalid_snapshot_id")
	if !ok {
		return
	}
	snap, entries, err := h.svc.GetSnapshot(c.Request.Context(), tenantID, snapshotID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	out := gen.BackupSnapshotDetail{
		Snapshot: toAPISnapshot(snap),
		Entries:  make([]gen.BackupManifestEntry, 0, len(entries)),
	}
	for _, e := range entries {
		out.Entries = append(out.Entries, toAPIManifestEntry(e))
	}
	c.JSON(http.StatusOK, &out)
}

// events streams a backup snapshot's progress as Server-Sent Events. It
// authorizes + verifies the snapshot exists (tenant-scoped), subscribes to the
// hub, flushes an initial event reflecting current state, then streams live
// events plus heartbeats until the client disconnects (or a 30-min safety
// timeout fires).
//
// NOTE — divergence from the M3 update SSE handler (internal/update): for an
// update RUN, a terminal run.Status truly means "this entity is done forever";
// closing the stream then is correct. For a backup SNAPSHOT, the entity is
// long-lived: a completed snapshot can be the source of a subsequent restore,
// and the restore runner emits phase events on the SAME SSE channel while
// snapshot.status STAYS "completed" (the restore is overlaid via
// snapshot.progress only — see ADR-034 + restore-runner). If we close the
// stream on the first "completed" frame we see, we drop every restore event.
// So: the handler stays open until the client disconnects or the safety
// timeout fires. The browser closes its own EventSource when it observes a
// terminal `phase` (see use-backup-stream.ts onProgress); that's the correct
// layer for terminal detection because only the client knows whether the
// frame it just received is for the current viewing intent.
func (h *Handler) events(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	snapshotID, ok := uuidParam(c, "snapshotId", "invalid_snapshot_id")
	if !ok {
		return
	}
	if h.hub == nil {
		httpx.Error(c, domain.Internal("sse_unsupported", "streaming is not enabled"))
		return
	}

	// Verify the snapshot exists in this tenant before opening the stream (404
	// maps cleanly; once headers flush we can no longer send a JSON error).
	snap, _, err := h.svc.GetSnapshot(c.Request.Context(), tenantID, snapshotID)
	if err != nil {
		httpx.Error(c, err)
		return
	}

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		httpx.Error(c, domain.Internal("sse_unsupported", "streaming is not supported"))
		return
	}

	// Subscribe BEFORE writing the snapshot so no transition is missed in the gap.
	ch, unsub := h.hub.Subscribe(snapshotID)
	defer unsub()

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no") // disable proxy buffering
	c.Status(http.StatusOK)

	// Initial snapshot event: emit the current state so a late subscriber gets a
	// complete picture without waiting for the next runner POST.
	//
	// SUPPRESSION RULE (Bug 3 fix): if the snapshot's persisted progress is in a
	// TERMINAL phase (completed/failed) AND that progress was last written more
	// than `initialStaleThreshold` ago, do NOT send it as the initial frame. A
	// stale terminal echo poisons the client-side cache: the operator just
	// clicked Restore, the new restore lifecycle is about to start, and the
	// browser's `useBackup` cache would otherwise be overwritten with the OLD
	// terminal phase before the first real restore phase event lands. We let
	// the next live event from the hub be the first frame the client sees.
	//
	// Fresh terminal frames (within the threshold) are still emitted — a backup
	// that completed seconds ago is the legitimate current state and a late
	// SSE subscriber should see it immediately.
	if initial, ok := initialFrameToSend(snap); ok {
		writeBackupEvent(c.Writer, initial)
		flusher.Flush()
	}
	// NOTE: we intentionally do NOT early-return on isTerminalStatus(snap.Status)
	// here. See the long comment on this function — a terminal snapshot.status
	// is the steady state for an entity that can still receive restore events.

	ctx := c.Request.Context()
	ticker := time.NewTicker(sseHeartbeat)
	defer ticker.Stop()
	lifetime := time.NewTimer(sseMaxLifetime)
	defer lifetime.Stop()

	for {
		select {
		case <-ctx.Done():
			return // client disconnected
		case <-lifetime.C:
			return // safety: bound stream lifetime regardless of client state
		case <-ticker.C:
			// Heartbeat comment keeps intermediaries from closing an idle stream.
			_, _ = c.Writer.Write([]byte(":\n\n"))
			flusher.Flush()
		case ev, open := <-ch:
			if !open {
				return
			}
			writeBackupEvent(c.Writer, ev)
			flusher.Flush()
			// No terminal-status close: see function comment.
		}
	}
}

// writeBackupEvent serializes a BackupEvent as a single SSE "data:" frame
// tagged "event: progress" (the wire contract M5.6 frontend codes to).
func writeBackupEvent(w gin.ResponseWriter, ev BackupEvent) {
	payload, err := json.Marshal(ev)
	if err != nil {
		return
	}
	_, _ = w.Write([]byte("event: progress\ndata: "))
	_, _ = w.Write(payload)
	_, _ = w.Write([]byte("\n\n"))
}

// snapshotToEvent builds the initial "current state" event from a Snapshot's
// persisted progress JSONB. Empty progress (no runner POST yet) renders as
// phase="queued" with an empty detail, so the frontend always has a phase to
// display.
func snapshotToEvent(s Snapshot) BackupEvent {
	ev := BackupEvent{
		SnapshotID:  s.ID,
		Phase:       "queued",
		PhaseDetail: map[string]any{},
		Status:      s.Status,
	}
	if s.ProgressUpdatedAt != nil {
		ev.Timestamp = (*s.ProgressUpdatedAt).UTC()
	} else {
		ev.Timestamp = s.UpdatedAt.UTC()
	}
	if len(s.Progress) > 0 && string(s.Progress) != "{}" {
		var raw struct {
			Phase       string         `json:"phase"`
			PhaseDetail map[string]any `json:"phase_detail"`
		}
		if err := json.Unmarshal(s.Progress, &raw); err == nil {
			if raw.Phase != "" {
				ev.Phase = raw.Phase
			}
			if raw.PhaseDetail != nil {
				ev.PhaseDetail = raw.PhaseDetail
			}
		}
	}
	// For a terminal snapshot with no runner phase, surface the status as the
	// phase so the UI shows a final state.
	if (ev.Phase == "queued") && isTerminalStatus(s.Status) {
		ev.Phase = s.Status
		if s.Status == StatusFailed && s.Error != "" {
			ev.PhaseDetail = map[string]any{"error": s.Error}
		}
	}
	return ev
}

// isTerminalStatus reports whether a snapshot status is final. The SSE handler
// no longer closes on a terminal status — see the comment on Handler.events.
func isTerminalStatus(status string) bool {
	return status == StatusCompleted || status == StatusFailed
}

// isTerminalPhase reports whether a runner-side phase is final. Used by
// initialFrameToSend to decide whether to suppress a stale terminal echo.
func isTerminalPhase(phase string) bool {
	return phase == StatusCompleted || phase == StatusFailed
}

// initialStaleThreshold — frames older than this whose phase is terminal are
// suppressed (Bug 3 fix). 60 s comfortably covers the worst real "client just
// connected after backup finished" timing while being short enough that a
// just-clicked Restore does not see a stale completed/failed phase poison its
// cache. Tied to the watchdog cadence elsewhere in the system (the CP and
// agent both write progress at sub-minute intervals during real work).
const initialStaleThreshold = 60 * time.Second

// initialFrameToSend computes the initial SSE event for a freshly connected
// subscriber. Returns (event, true) when the event should be sent; (zero,
// false) when it should be suppressed because it's a stale terminal echo
// that would poison a client cache about to receive new restore events.
func initialFrameToSend(s Snapshot) (BackupEvent, bool) {
	ev := snapshotToEvent(s)
	if !isTerminalPhase(ev.Phase) {
		return ev, true // non-terminal phases always sent
	}
	// Terminal phase. If the progress timestamp is recent, this is the
	// legitimate current state and we should deliver it. If it's stale
	// (older than the threshold), suppress so the next real event drives
	// the client.
	var ts time.Time
	if s.ProgressUpdatedAt != nil {
		ts = *s.ProgressUpdatedAt
	} else {
		ts = s.UpdatedAt
	}
	if time.Since(ts) < initialStaleThreshold {
		return ev, true
	}
	return BackupEvent{}, false
}

func (h *Handler) createRestore(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	snapshotID, ok := uuidParam(c, "snapshotId", "invalid_snapshot_id")
	if !ok {
		return
	}
	var req gen.RestoreCreate
	if err := c.ShouldBindJSON(&req); err != nil && c.Request.ContentLength > 0 {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
		return
	}
	sel := RestoreSelection{
		Full:     req.Full.Or(false),
		Paths:    req.Paths,
		DBTables: req.DbTables,
	}
	snap, err := h.svc.CreateRestore(c.Request.Context(), tenantID, snapshotID, sel)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	h.recordRestore(c, snap, sel)
	out := toAPISnapshot(snap)
	c.JSON(http.StatusAccepted, &out)
}

func (h *Handler) getSchedule(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	siteID, ok := uuidParam(c, "siteId", "invalid_site_id")
	if !ok {
		return
	}
	sched, err := h.svc.GetSchedule(c.Request.Context(), tenantID, siteID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	out := toAPISchedule(sched)
	c.JSON(http.StatusOK, &out)
}

func (h *Handler) putSchedule(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	siteID, ok := uuidParam(c, "siteId", "invalid_site_id")
	if !ok {
		return
	}
	var req gen.BackupScheduleUpdate
	if err := c.ShouldBindJSON(&req); err != nil && c.Request.ContentLength > 0 {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
		return
	}
	in := PutScheduleInput{
		TenantID:           tenantID,
		SiteID:             siteID,
		Cadence:            string(req.Cadence.Or(gen.BackupScheduleUpdateCadenceDaily)),
		Kind:               string(req.Kind.Or(gen.BackupScheduleUpdateKindFull)),
		Enabled:            req.Enabled.Or(true),
		RetentionDays:      req.RetentionDays.Or(0),
		MonthlyArchiveKeep: req.MonthlyArchiveKeep.Or(-1),
	}
	sched, err := h.svc.PutSchedule(c.Request.Context(), in)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	h.recordScheduleChange(c, sched)
	out := toAPISchedule(sched)
	c.JSON(http.StatusOK, &out)
}

// --- audit ---

func (h *Handler) recordRestore(c *gin.Context, snap Snapshot, sel RestoreSelection) {
	if h.audit == nil {
		return
	}
	actorType, actorID := actorOf(c)
	_, _ = h.audit.Record(c.Request.Context(), audit.Event{
		TenantID:   snap.TenantID,
		ActorType:  actorType,
		ActorID:    actorID,
		Action:     ActionRestoreStarted,
		TargetType: "backup_snapshot",
		TargetID:   snap.ID.String(),
		Metadata: map[string]any{
			"site_id":   snap.SiteID.String(),
			"full":      sel.Full || (len(sel.Paths) == 0 && len(sel.DBTables) == 0),
			"paths":     len(sel.Paths),
			"db_tables": len(sel.DBTables),
		},
	})
}

func (h *Handler) recordScheduleChange(c *gin.Context, sched Schedule) {
	if h.audit == nil {
		return
	}
	actorType, actorID := actorOf(c)
	_, _ = h.audit.Record(c.Request.Context(), audit.Event{
		TenantID:   sched.TenantID,
		ActorType:  actorType,
		ActorID:    actorID,
		Action:     ActionScheduleChanged,
		TargetType: "backup_schedule",
		TargetID:   sched.SiteID.String(),
		Metadata: map[string]any{
			"cadence": sched.Cadence,
			"kind":    sched.Kind,
			"enabled": sched.Enabled,
		},
	})
}

// --- mapping helpers ---

func toAPISnapshot(s Snapshot) gen.BackupSnapshot {
	out := gen.BackupSnapshot{
		ID:        s.ID,
		TenantID:  s.TenantID,
		SiteID:    s.SiteID,
		Kind:      gen.BackupSnapshotKind(s.Kind),
		Status:    gen.BackupSnapshotStatus(s.Status),
		CreatedAt: s.CreatedAt,
		UpdatedAt: s.UpdatedAt,
	}
	if s.CreatedBy != nil {
		out.CreatedBy = gen.NewOptUUID(*s.CreatedBy)
	}
	if s.AgeRecipient != "" {
		out.AgeRecipient = gen.NewOptString(s.AgeRecipient)
	}
	out.TotalSize = gen.NewOptInt64(s.TotalSize)
	out.ChunkCount = gen.NewOptInt64(s.ChunkCount)
	out.Archived = gen.NewOptBool(s.Archived)
	if s.Error != "" {
		out.Error = gen.NewOptString(s.Error)
	}
	if s.StartedAt != nil {
		out.StartedAt = gen.NewOptDateTime(*s.StartedAt)
	}
	if s.FinishedAt != nil {
		out.FinishedAt = gen.NewOptDateTime(*s.FinishedAt)
	}
	// M5.6 / ADR-032 progress (JSONB → ogen map[string]jx.Raw). Empty {} is the
	// default and we just leave OptBackupSnapshotProgress unset (the frontend
	// treats it as "no runner phase yet").
	if len(s.Progress) > 0 && string(s.Progress) != "{}" {
		var raw map[string]jx.Raw
		if err := json.Unmarshal(s.Progress, &raw); err == nil && len(raw) > 0 {
			out.Progress = gen.NewOptBackupSnapshotProgress(gen.BackupSnapshotProgress(raw))
		}
	}
	if s.ProgressUpdatedAt != nil {
		out.ProgressUpdatedAt = gen.NewOptDateTime(*s.ProgressUpdatedAt)
	}
	return out
}

func toAPIManifestEntry(e ManifestEntry) gen.BackupManifestEntry {
	out := gen.BackupManifestEntry{
		Path:       e.Path,
		EntryKind:  gen.BackupManifestEntryEntryKind(e.EntryKind),
		Size:       e.Size,
		ChunkCount: int32(len(e.ChunkHashes)),
	}
	if e.TableName != "" {
		out.TableName = gen.NewOptString(e.TableName)
	}
	out.Mode = gen.NewOptInt32(e.Mode)
	return out
}

func toAPISchedule(s Schedule) gen.BackupSchedule {
	out := gen.BackupSchedule{
		ID:                 s.ID,
		TenantID:           s.TenantID,
		SiteID:             s.SiteID,
		Cadence:            gen.BackupScheduleCadence(s.Cadence),
		Kind:               gen.BackupScheduleKind(s.Kind),
		Enabled:            s.Enabled,
		RetentionDays:      s.RetentionDays,
		MonthlyArchiveKeep: s.MonthlyArchiveKeep,
		NextRunAt:          s.NextRunAt,
		CreatedAt:          s.CreatedAt,
		UpdatedAt:          s.UpdatedAt,
	}
	if s.LastRunAt != nil {
		out.LastRunAt = gen.NewOptDateTime(*s.LastRunAt)
	}
	return out
}

// --- gin helpers ---

func tenantOf(c *gin.Context) (uuid.UUID, bool) {
	tenantID, ok := domain.TenantIDFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Forbidden("tenant_required", "a tenant context is required"))
		return uuid.Nil, false
	}
	return tenantID, true
}

func uuidParam(c *gin.Context, name, code string) (uuid.UUID, bool) {
	id, err := uuid.Parse(c.Param(name))
	if err != nil {
		httpx.Error(c, domain.Validation(code, name+" is not a valid UUID"))
		return uuid.Nil, false
	}
	return id, true
}

func actorOf(c *gin.Context) (string, string) {
	if p, ok := domain.PrincipalFromContext(c.Request.Context()); ok {
		if p.Type == domain.PrincipalAPIKey {
			return audit.ActorAPIKey, p.ActorID()
		}
		return audit.ActorUser, p.ActorID()
	}
	return audit.ActorSystem, ""
}

func parseInt32(s string, def int32) int32 {
	if s == "" {
		return def
	}
	n, err := strconv.ParseInt(s, 10, 32)
	if err != nil {
		return def
	}
	return int32(n)
}
