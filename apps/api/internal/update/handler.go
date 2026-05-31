package update

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/api/gen"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// sseHeartbeat is the interval between SSE keep-alive comment lines.
const sseHeartbeat = 15 * time.Second

// Handler serves the update endpoints under /api/v1/updates.
type Handler struct {
	svc   *Service
	hub   *Hub
	audit *audit.Recorder
}

// NewHandler builds an update Handler.
func NewHandler(svc *Service, hub *Hub, rec *audit.Recorder) *Handler {
	return &Handler{svc: svc, hub: hub, audit: rec}
}

// Register mounts the update routes on /api/v1. Mutations require operator+;
// reads (including the SSE stream) require viewer+.
func (h *Handler) Register(r *gin.RouterGroup) {
	// The bulk update-run orchestrator is org-level: a run can span multiple
	// sites, and the run/tasks are resolved by runId (not bound to a single
	// :siteId), so a per-site allowlist check is insufficient. Restrict the
	// whole group to org-scoped principals — site-scoped collaborators view
	// their site's available updates via /sites/:siteId/updates/* instead.
	r.POST("/updates", authz.RequireOrgScope(), authz.RequirePermission(authz.PermSiteWrite), h.create)
	r.GET("/updates", authz.RequireOrgScope(), authz.RequirePermission(authz.PermSiteRead), h.list)
	r.GET("/updates/:runId", authz.RequireOrgScope(), authz.RequirePermission(authz.PermSiteRead), h.get)
	r.GET("/updates/:runId/events", authz.RequireOrgScope(), authz.RequirePermission(authz.PermSiteRead), h.events)
}

func (h *Handler) create(c *gin.Context) {
	tenantID, ok := domain.TenantIDFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Forbidden("tenant_required", "a tenant context is required"))
		return
	}
	var req gen.UpdateRunCreate
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
		return
	}

	in := CreateRunInput{
		TenantID: tenantID,
		SiteIDs:  req.SiteIds,
		Tag:      req.Tag.Or(""),
		DryRun:   req.DryRun.Or(false),
		Items:    fromAPIItems(req.Items),
	}
	if p, ok := domain.PrincipalFromContext(c.Request.Context()); ok {
		in.CreatedBy = p.UserID
	}
	if req.ScheduleAt.Set {
		t := req.ScheduleAt.Value
		in.ScheduledAt = &t
	}

	run, tasks, err := h.svc.CreateRun(c.Request.Context(), in)
	if err != nil {
		httpx.Error(c, err)
		return
	}

	h.recordRunCreated(c, run, len(tasks))
	out := toAPIRun(run, tasks)
	c.JSON(http.StatusCreated, &out)
}

func (h *Handler) list(c *gin.Context) {
	tenantID, ok := domain.TenantIDFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Forbidden("tenant_required", "a tenant context is required"))
		return
	}
	runs, err := h.svc.ListRuns(c.Request.Context(), tenantID, parseInt32(c.Query("limit"), 50), parseInt32(c.Query("offset"), 0))
	if err != nil {
		httpx.Error(c, err)
		return
	}
	items := make([]gen.UpdateRun, 0, len(runs))
	for _, r := range runs {
		items = append(items, toAPIRun(r, nil))
	}
	c.JSON(http.StatusOK, gen.UpdateRunList{Items: items})
}

func (h *Handler) get(c *gin.Context) {
	tenantID, ok := domain.TenantIDFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Forbidden("tenant_required", "a tenant context is required"))
		return
	}
	runID, err := uuid.Parse(c.Param("runId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_run_id", "runId is not a valid UUID"))
		return
	}
	run, tasks, err := h.svc.GetRun(c.Request.Context(), tenantID, runID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	out := toAPIRun(run, tasks)
	c.JSON(http.StatusOK, &out)
}

// events streams task-status transitions for a run as Server-Sent Events. It
// authorizes + verifies the run exists (tenant-scoped), subscribes to the hub,
// flushes an initial snapshot, then streams live events plus heartbeats until
// the run completes or the client disconnects.
func (h *Handler) events(c *gin.Context) {
	tenantID, ok := domain.TenantIDFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Forbidden("tenant_required", "a tenant context is required"))
		return
	}
	runID, err := uuid.Parse(c.Param("runId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_run_id", "runId is not a valid UUID"))
		return
	}

	// Verify the run exists in this tenant before opening the stream (404 maps
	// cleanly; once headers flush we can no longer send a JSON error).
	run, tasks, err := h.svc.GetRun(c.Request.Context(), tenantID, runID)
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
	ch, unsub := h.hub.Subscribe(runID)
	defer unsub()

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no") // disable proxy buffering
	c.Status(http.StatusOK)

	// Initial snapshot: emit the current state of every task so a late subscriber
	// gets a complete picture, then stream live deltas.
	runStatus := run.Status
	for _, t := range tasks {
		writeEvent(c.Writer, taskToEvent(t, runStatus))
	}
	flusher.Flush()
	// If the run is already complete, send a final snapshot and stop.
	if run.Status == RunCompleted {
		return
	}

	ctx := c.Request.Context()
	ticker := time.NewTicker(sseHeartbeat)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return // client disconnected
		case <-ticker.C:
			// Heartbeat comment keeps intermediaries from closing an idle stream.
			_, _ = c.Writer.Write([]byte(":\n\n"))
			flusher.Flush()
		case ev, open := <-ch:
			if !open {
				return
			}
			writeEvent(c.Writer, ev)
			flusher.Flush()
			if ev.RunStatus == RunCompleted {
				return
			}
		}
	}
}

// writeEvent serializes an Event as a single SSE "data:" frame.
func writeEvent(w gin.ResponseWriter, ev Event) {
	payload, err := json.Marshal(ev)
	if err != nil {
		return
	}
	_, _ = w.Write([]byte("event: task\ndata: "))
	_, _ = w.Write(payload)
	_, _ = w.Write([]byte("\n\n"))
}

func taskToEvent(t Task, runStatus string) Event {
	return Event{
		RunID:       t.RunID,
		TaskID:      t.ID,
		SiteID:      t.SiteID,
		TargetType:  t.TargetType,
		TargetSlug:  t.TargetSlug,
		Status:      t.Status,
		FromVersion: t.FromVersion,
		ToVersion:   t.ToVersion,
		Detail:      t.Detail,
		RunStatus:   runStatus,
	}
}

func (h *Handler) recordRunCreated(c *gin.Context, run Run, taskCount int) {
	if h.audit == nil {
		return
	}
	actorType := audit.ActorSystem
	actorID := ""
	if p, ok := domain.PrincipalFromContext(c.Request.Context()); ok {
		actorType = audit.ActorUser
		if p.Type == domain.PrincipalAPIKey {
			actorType = audit.ActorAPIKey
		}
		actorID = p.ActorID()
	}
	_, _ = h.audit.Record(c.Request.Context(), audit.Event{
		TenantID:   run.TenantID,
		ActorType:  actorType,
		ActorID:    actorID,
		Action:     ActionRunCreated,
		TargetType: "update_run",
		TargetID:   run.ID.String(),
		Metadata: map[string]any{
			"dry_run":    run.DryRun,
			"task_count": taskCount,
		},
	})
}

func fromAPIItems(items []gen.UpdateItem) []Item {
	out := make([]Item, 0, len(items))
	for _, it := range items {
		out = append(out, Item{
			Type:    string(it.Type),
			Slug:    it.Slug.Or(""),
			Version: it.Version.Or(""),
		})
	}
	return out
}

// toAPIRun maps a Run (and optionally its tasks) to the OpenAPI representation.
func toAPIRun(r Run, tasks []Task) gen.UpdateRun {
	out := gen.UpdateRun{
		ID:        r.ID,
		TenantID:  r.TenantID,
		Status:    gen.UpdateRunStatus(r.Status),
		DryRun:    r.DryRun,
		CreatedAt: r.CreatedAt,
		UpdatedAt: r.UpdatedAt,
	}
	if r.CreatedBy != nil {
		out.CreatedBy = gen.NewOptUUID(*r.CreatedBy)
	}
	if r.ScheduledAt != nil {
		out.ScheduledAt = gen.NewOptDateTime(*r.ScheduledAt)
	}
	if tasks != nil {
		out.Tasks = make([]gen.UpdateTask, 0, len(tasks))
		for _, t := range tasks {
			out.Tasks = append(out.Tasks, toAPITask(t))
		}
	}
	return out
}

func toAPITask(t Task) gen.UpdateTask {
	out := gen.UpdateTask{
		ID:         t.ID,
		RunID:      t.RunID,
		TenantID:   t.TenantID,
		SiteID:     t.SiteID,
		TargetType: gen.UpdateTaskTargetType(t.TargetType),
		TargetSlug: t.TargetSlug,
		Status:     gen.UpdateTaskStatus(t.Status),
		CreatedAt:  t.CreatedAt,
		UpdatedAt:  t.UpdatedAt,
	}
	if t.DesiredVersion != "" {
		out.DesiredVersion = gen.NewOptString(t.DesiredVersion)
	}
	if t.FromVersion != "" {
		out.FromVersion = gen.NewOptString(t.FromVersion)
	}
	if t.ToVersion != "" {
		out.ToVersion = gen.NewOptString(t.ToVersion)
	}
	if t.Detail != "" {
		out.Detail = gen.NewOptString(t.Detail)
	}
	if t.Error != "" {
		out.Error = gen.NewOptString(t.Error)
	}
	if t.StartedAt != nil {
		out.StartedAt = gen.NewOptDateTime(*t.StartedAt)
	}
	if t.FinishedAt != nil {
		out.FinishedAt = gen.NewOptDateTime(*t.FinishedAt)
	}
	return out
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
