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
	// envFetcher resolves the agent-shipped environment.json manifest entry's
	// ordered chunks to a JSON blob. Re-uses the same fetcher adapter the
	// SQL-inspection endpoint uses (chunk-store-by-presigned-GET); satisfies
	// the ManifestInspectionFetcher interface. Optional — when nil the
	// /backups/:snapshotId/environment route returns 503.
	envFetcher ManifestInspectionFetcher
}

// NewHandler builds a backup Handler. The hub may be nil in environments that
// don't need live SSE (the events route will then 500 on connect, mirroring the
// update package's "sse_unsupported" path).
func NewHandler(svc *Service, hub *Hub, rec *audit.Recorder) *Handler {
	return &Handler{svc: svc, hub: hub, audit: rec}
}

// SetEnvironmentFetcher wires the manifest-fetcher used by the
// environment-fingerprint endpoint. Same adapter as the SQL-inspection
// endpoint uses (the fetcher is artifact-agnostic — it just concatenates
// chunks and probes JSON), so callers pass the same instance.
func (h *Handler) SetEnvironmentFetcher(f ManifestInspectionFetcher) {
	h.envFetcher = f
}

// Register mounts the backup routes on the /api/v1 router group.
func (h *Handler) Register(r *gin.RouterGroup) {
	// Per-siteId routes: RequireSiteAccess enforces the site allowlist for
	// site-scoped principals (belt-and-braces in front of the RLS policy).
	r.POST("/sites/:siteId/backups", authz.RequirePermission(authz.PermSiteWrite), authz.RequireSiteAccess("siteId"), h.createBackup)
	r.GET("/sites/:siteId/backups", authz.RequirePermission(authz.PermSiteRead), authz.RequireSiteAccess("siteId"), h.listBackups)
	r.GET("/sites/:siteId/backup-schedule", authz.RequirePermission(authz.PermSiteRead), authz.RequireSiteAccess("siteId"), h.getSchedule)
	r.PUT("/sites/:siteId/backup-schedule", authz.RequirePermission(authz.PermSiteWrite), authz.RequireSiteAccess("siteId"), h.putSchedule)
	// Routes by snapshotId (no :siteId param): site isolation is enforced by
	// running the repo queries through pool.RunTenantTx (which activates scoped
	// RLS for site-scoped principals). The RESTRICTIVE RLS policy on
	// backup_snapshots denies rows whose site_id is not in AllowedSiteIDs, so
	// a non-granted site's snapshot returns 404 naturally.
	r.GET("/backups/:snapshotId", authz.RequirePermission(authz.PermSiteRead), h.getBackup)
	r.GET("/backups/:snapshotId/events", authz.RequirePermission(authz.PermSiteRead), h.events)
	r.POST("/backups/:snapshotId/restore", authz.RequirePermission(authz.PermSiteWrite), h.createRestore)
	// ADR-037 Sprint 1, 1D — environment fingerprint. Returns the JSON the
	// agent shipped as the synthetic `environment.json` manifest entry, or 404
	// when the snapshot pre-dates the env-fingerprint feature. Reads use the
	// same manifest-fetcher path the sql-inspection endpoint uses.
	r.GET("/backups/:snapshotId/environment", authz.RequirePermission(authz.PermSiteRead), h.getEnvironment)
}

// getEnvironment serves the agent-shipped environment.json manifest entry for
// a snapshot. Mirrors the resolution shape of the sql-inspection endpoint but
// without the legacy-parser fallback (env fingerprint is agent-only — there's
// no CP-side reconstruction of "what environment was this dump taken from").
//
// Resolution:
//  1. Snapshot lookup is tenant-scoped (404s on tenant mismatch).
//  2. Manifest scanned for an entry kind=="environment" or path=="environment.json".
//     Old snapshots without the entry get a 404 with code env_not_recorded.
//  3. Fetch ordered chunks via the ManifestInspectionFetcher path (same
//     adapter handles arbitrary text/JSON artifacts; the SQL-inspection
//     framing is incidental, not contractual).
func (h *Handler) getEnvironment(c *gin.Context) {
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
	if !canReadSite(c, snap.SiteID) {
		httpx.Error(c, domain.Forbidden("forbidden", "you do not have access to this site"))
		return
	}
	if h.envFetcher == nil {
		httpx.Error(c, domain.ServiceUnavailable("env_fetch_unwired",
			"environment fingerprint reader is not configured on this control plane"))
		return
	}
	entry, ok := findEnvironmentEntry(entries)
	if !ok {
		httpx.Error(c, domain.NotFound("env_not_recorded",
			"this snapshot pre-dates the environment-fingerprint feature; rebuild with a v0.9.10+ agent"))
		return
	}
	raw, ferr := h.envFetcher.Fetch(c.Request.Context(), tenantID, snap, entry)
	if ferr != nil {
		httpx.Error(c, domain.Internal("env_fetch_failed",
			"could not fetch the environment fingerprint").WithCause(ferr))
		return
	}
	// Pass-through: the agent ships JSON; we re-emit verbatim. Validates as
	// JSON in the fetcher already (the adapter probes the bytes for a JSON
	// parse before returning), so we can write Content-Type: application/json
	// without re-decoding.
	c.Data(http.StatusOK, "application/json", raw)
}

// findEnvironmentEntry locates the agent-supplied environment fingerprint in a
// manifest. Mirror of findInspectionEntry — typed entry_kind first, literal
// logical path fallback.
func findEnvironmentEntry(entries []ManifestEntry) (ManifestEntry, bool) {
	const (
		kindEnvironment        = "environment"
		environmentLogicalPath = "environment.json"
	)
	for _, e := range entries {
		if e.EntryKind == kindEnvironment {
			return e, true
		}
	}
	for _, e := range entries {
		if e.Path == environmentLogicalPath {
			return e, true
		}
	}
	return ManifestEntry{}, false
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
	if !canReadSite(c, snap.SiteID) {
		httpx.Error(c, domain.Forbidden("forbidden", "you do not have access to this site"))
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
	if !canReadSite(c, snap.SiteID) {
		httpx.Error(c, domain.Forbidden("forbidden", "you do not have access to this site"))
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
	// Map the ogen-generated enum-typed components slice to the service's
	// plain []string. The wire enum is closed ({files, db}); validation
	// already rejected anything else at decode time.
	components := make([]string, 0, len(req.Components))
	for _, c := range req.Components {
		components = append(components, string(c))
	}
	sel := RestoreSelection{
		Full:         req.Full.Or(false),
		Paths:        req.Paths,
		DBTables:     req.DbTables,
		Components:   components,
		KeepOldFiles: req.KeepOldFiles.Or(false),
	}

	var triggeredBy string
	if p, ok := domain.PrincipalFromContext(c.Request.Context()); ok {
		triggeredBy = p.ActorID()
	}

	// Restore is a destructive cross-site action resolved by snapshot id (no
	// :siteId). Bind the snapshot's site to the caller's access BEFORE starting
	// so a site-scoped collaborator cannot restore another site's snapshot.
	if snap, _, gerr := h.svc.GetSnapshot(c.Request.Context(), tenantID, snapshotID); gerr != nil {
		httpx.Error(c, gerr)
		return
	} else if !canReadSite(c, snap.SiteID) {
		httpx.Error(c, domain.Forbidden("forbidden", "you do not have access to this site"))
		return
	}

	result, err := h.svc.CreateRestore(c.Request.Context(), tenantID, snapshotID, sel, triggeredBy)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	h.recordRestore(c, result.Snapshot, sel)
	out := toAPISnapshot(result.Snapshot)
	// Include the restore_run_id in the response so callers can navigate to it.
	type createRestoreResponse struct {
		gen.BackupSnapshot
		RestoreRunID string `json:"restore_run_id,omitempty"`
	}
	resp := createRestoreResponse{BackupSnapshot: out}
	if result.RestoreRunID != uuid.Nil {
		resp.RestoreRunID = result.RestoreRunID.String()
	}
	c.JSON(http.StatusAccepted, &resp)
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
		RunHour:            req.RunHour.Or(2),
		RunMinute:          req.RunMinute.Or(0),
		KeepLast:           req.KeepLast.Or(7),
	}
	// Map nullable int32 timing fields (OptNilInt32 → *int32).
	if v, ok := req.DayOfWeek.Get(); ok {
		in.DayOfWeek = &v
	}
	if v, ok := req.DayOfMonth.Get(); ok {
		in.DayOfMonth = &v
	}
	if v, ok := req.FrequencyHours.Get(); ok {
		in.FrequencyHours = &v
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
			"site_id":        snap.SiteID.String(),
			"full":           sel.Full || (len(sel.Paths) == 0 && len(sel.DBTables) == 0),
			"paths":          len(sel.Paths),
			"db_tables":      len(sel.DBTables),
			"components":     sel.Components,
			"keep_old_files": sel.KeepOldFiles,
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
		RunHour:            s.RunHour,
		RunMinute:          s.RunMinute,
		KeepLast:           s.KeepLast,
		Timezone:           s.Timezone,
		GmtOffset:          s.GmtOffset,
		NextRunAt:          s.NextRunAt,
		CreatedAt:          s.CreatedAt,
		UpdatedAt:          s.UpdatedAt,
	}
	// Map nullable timing fields (*int32 → OptNilInt32).
	if s.DayOfWeek != nil {
		out.DayOfWeek = gen.NewOptNilInt32(*s.DayOfWeek)
	} else {
		out.DayOfWeek.SetToNull()
	}
	if s.DayOfMonth != nil {
		out.DayOfMonth = gen.NewOptNilInt32(*s.DayOfMonth)
	} else {
		out.DayOfMonth.SetToNull()
	}
	if s.FrequencyHours != nil {
		out.FrequencyHours = gen.NewOptNilInt32(*s.FrequencyHours)
	} else {
		out.FrequencyHours.SetToNull()
	}
	if s.LastRunAt != nil {
		out.LastRunAt = gen.NewOptDateTime(*s.LastRunAt)
	}
	// Compute the next ~3 occurrences for the upcoming-preview strip.
	out.NextRuns = scheduleNextRuns(s, 3)
	return out
}

// scheduleNextRuns computes the next n occurrence times after s.NextRunAt for
// the preview strip. It uses the resolved timezone from s.Timezone (already
// stored on the Schedule by PutSchedule/GetSchedule). Falls back to UTC when
// the timezone cannot be loaded (should never happen — PutSchedule validates it).
func scheduleNextRuns(s Schedule, n int) []time.Time {
	if n <= 0 || s.NextRunAt.IsZero() {
		return nil
	}
	loc := resolveLocation(s.Timezone, s.GmtOffset)
	dow := optInt32ToInt(s.DayOfWeek)
	dom := optInt32ToInt(s.DayOfMonth)
	fh := optInt32ToInt(s.FrequencyHours)
	jitter := SiteJitter(s.SiteID)

	runs := make([]time.Time, 0, n)
	// The first occurrence is the already-computed next_run_at.
	cur := s.NextRunAt
	runs = append(runs, cur.UTC())
	// Compute subsequent occurrences by chaining nextOccurrence from each prior result.
	for len(runs) < n {
		cur = nextOccurrence(cur, s.Cadence,
			int(s.RunHour), int(s.RunMinute),
			dow, dom, fh,
			jitter, loc)
		runs = append(runs, cur.UTC())
	}
	return runs
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
