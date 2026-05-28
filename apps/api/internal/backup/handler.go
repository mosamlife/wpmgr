package backup

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/api/gen"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// Handler serves the operator/viewer-facing backup endpoints under /api/v1.
// Mutations (create backup, restore, put schedule) require operator+; reads
// (list, get, get schedule) require viewer+.
type Handler struct {
	svc   *Service
	audit *audit.Recorder
}

// NewHandler builds a backup Handler.
func NewHandler(svc *Service, rec *audit.Recorder) *Handler {
	return &Handler{svc: svc, audit: rec}
}

// Register mounts the backup routes on the /api/v1 router group.
func (h *Handler) Register(r *gin.RouterGroup) {
	r.POST("/sites/:siteId/backups", authz.RequirePermission(authz.PermSiteWrite), h.createBackup)
	r.GET("/sites/:siteId/backups", authz.RequirePermission(authz.PermSiteRead), h.listBackups)
	r.GET("/backups/:snapshotId", authz.RequirePermission(authz.PermSiteRead), h.getBackup)
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
