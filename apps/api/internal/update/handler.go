package update

import (
	"context"
	"encoding/json"
	"log/slog"
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
	// Retrying a run creates a new run, so it carries exactly the authorization
	// creating one does. It sits under /updates/runs/... rather than
	// /updates/:runId/retry because :runId is a GET-tree parameter here and a
	// sibling static segment on the same tree would be ambiguous.
	r.POST("/updates/runs/:id/retry", authz.RequireOrgScope(), authz.RequirePermission(authz.PermSiteWrite), h.retry)
	// GH #463 — call back a scheduled run before it fires. Carries exactly the
	// authorization creating one does (org-scoped + PermSiteWrite): the same
	// principal who could schedule a fleet-wide update is the one who must be
	// able to stop it. There is no separate halt ENDPOINT to match — halting is
	// internal to the agent wave machine — so this follows the sibling mutation
	// on this tree rather than inventing a permission. It shares /updates/runs/
	// with retry for the same routing reason given above.
	r.POST("/updates/runs/:id/cancel", authz.RequireOrgScope(), authz.RequirePermission(authz.PermSiteWrite), h.cancel)
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
		// A NON-ZERO run means the run and its tasks were already COMMITTED and
		// only the background enqueue failed. CreateRun documents that case as
		// best effort and says the caller still gets the run, but this handler
		// used to discard both and return an error, so the operator was told
		// nothing happened while a real run sat in the database with pending
		// tasks that nothing would move until the reaper failed them (45m for
		// ordinary tasks, 6h for agent ones). They would then create a second
		// run, which the m88 in-flight unique index can reject, making a
		// recoverable hiccup look like a broken product.
		//
		// Report the run that exists. The tasks are pending and visible on the
		// run page, the enqueue failure is logged and audited below, and a
		// caller that sees fewer tasks than it asked for can act on it.
		if run.ID == uuid.Nil {
			httpx.Error(c, err)
			return
		}
		slog.Error("update run created but a task enqueue failed; tasks remain pending",
			slog.String("run_id", run.ID.String()),
			slog.String("tenant_id", run.TenantID.String()),
			slog.Int("task_count", len(tasks)),
			slog.Any("error", err))
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
	summaries, err := h.svc.ListRunSummaries(c.Request.Context(), tenantID, parseInt32(c.Query("limit"), 50), parseInt32(c.Query("offset"), 0))
	if err != nil {
		httpx.Error(c, err)
		return
	}
	items := make([]gen.UpdateRun, 0, len(summaries))
	for _, s := range summaries {
		items = append(items, toAPIRunSummary(s))
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

// cancel calls back a scheduled run before it fires (GH #463).
func (h *Handler) cancel(c *gin.Context) {
	tenantID, ok := domain.TenantIDFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Forbidden("tenant_required", "a tenant context is required"))
		return
	}
	runID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_run_id", "the run id is not a valid UUID"))
		return
	}

	res, err := h.svc.CancelScheduledRun(c.Request.Context(), tenantID, runID)
	if err != nil {
		// Every failure here is a clean refusal, and unlike create/retry there
		// is no partial-success case to disentangle: the repo's CAS either
		// cancelled the run and its tasks in one transaction or changed
		// nothing at all. httpx maps the typed conflict to 409, which is what
		// lets the client tell "too late" from a server fault.
		httpx.Error(c, err)
		return
	}

	h.recordRunCancelled(c, tenantID, res)
	out := gen.UpdateRunCancelResult{
		// nil tasks: the cancel response is about the RUN. A client that
		// wants the cancelled task rows re-reads the run detail.
		Run:            toAPIRun(res.Run, nil),
		CancelledTasks: int64(res.CancelledTasks),
	}
	c.JSON(http.StatusOK, &out)
}

// recordRunCancelled writes the audit entry for an operator cancellation.
//
// Best-effort against the RESPONSE, deliberately: the run and its tasks are
// already committed terminal, so failing the request here would tell the
// operator their cancel did not happen when it did — and they would press it
// again, get a 409, and have no idea which of the two answers was true. The
// write is still not allowed to be silent, so a failure logs at Error, matching
// the dispatch and expiry records.
func (h *Handler) recordRunCancelled(c *gin.Context, tenantID uuid.UUID, res CancelRunResult) {
	if h.audit == nil {
		return
	}
	// The actor type is COMPUTED, never assumed, and ActorID comes from
	// Principal.ActorID() rather than UserID. This endpoint is reachable by an
	// API-key principal, for which UserID is uuid.Nil and APIKeyID carries the
	// identity — hardcoding ActorUser and reading UserID directly recorded
	// "a user did this" over a zero UUID, naming nobody, on the one action
	// whose entire purpose is recording that somebody chose to stop a
	// fleet-wide operation.
	//
	// A wrong audit record is worse than a missing one: a missing record
	// prompts a question, a wrong one answers it incorrectly. It also breaks
	// the actor-name join in audit.go, which resolves the user's name for
	// ActorUser and the key's label for ActorAPIKey.
	//
	// Same shape as recordRunCreated and recordRunRetried in this file. The
	// ActorSystem default is deliberate: no principal in context means no human
	// and no key, and claiming either would be the same lie in a quieter form.
	actorType, actorID := auditActor(c.Request.Context())
	ev := audit.Event{
		TenantID:   tenantID,
		ActorType:  actorType,
		ActorID:    actorID,
		Action:     ActionRunCancelled,
		TargetType: "update_run",
		TargetID:   res.Run.ID.String(),
		Metadata: map[string]any{
			"cancelled_tasks": res.CancelledTasks,
			"run_status":      res.Run.Status,
			"sites_contacted": 0,
		},
	}
	if res.Run.ScheduledAt != nil {
		ev.Metadata["scheduled_at"] = res.Run.ScheduledAt.UTC().Format(time.RFC3339)
	}
	if _, err := h.audit.Record(c.Request.Context(), ev); err != nil {
		slog.Error("update: failed to record the scheduled-run cancellation audit entry",
			slog.String("run_id", res.Run.ID.String()),
			slog.String("tenant_id", tenantID.String()),
			slog.Any("error", err))
	}
}

// retry creates a NEW run repeating the selected tasks of an existing one (GH
// #336). It answers with a complete account of every requested task rather than
// a bare success: see RetryResult.
func (h *Handler) retry(c *gin.Context) {
	tenantID, ok := domain.TenantIDFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Forbidden("tenant_required", "a tenant context is required"))
		return
	}
	runID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_run_id", "the run id is not a valid UUID"))
		return
	}
	var req gen.UpdateRunRetryRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
		return
	}

	in := RetryRunInput{TenantID: tenantID, RunID: runID, TaskIDs: req.TaskIds}
	if p, ok := domain.PrincipalFromContext(c.Request.Context()); ok {
		in.CreatedBy = p.UserID
	}

	res, err := h.svc.RetryRun(c.Request.Context(), in)
	if err != nil {
		// Same discriminator the create path uses: a non-zero run id means the
		// new run and its tasks are ALREADY COMMITTED and only a best-effort
		// step after the commit failed. Reporting an error there would tell the
		// operator nothing happened while a real run sat in the database with
		// pending tasks, which is the defect fixed in 0.61.117 for create; a
		// retry button on top of it would make that worse, not better.
		if res.RunID == uuid.Nil {
			httpx.Error(c, err)
			return
		}
		slog.Error("update retry run created but a task enqueue failed; tasks remain pending",
			slog.String("run_id", res.RunID.String()),
			slog.String("source_run_id", runID.String()),
			slog.String("tenant_id", tenantID.String()),
			slog.Int("created", res.Created),
			slog.Any("error", err))
	}

	h.recordRunRetried(c, tenantID, runID, res)
	out := toAPIRetryResult(res)
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

// auditActor resolves the audit actor for a request, and it is one function
// rather than three copies because the copies were where this went wrong.
//
// Two things must move together and neither may be assumed:
//
//   - the TYPE, which drives audit.go's actor-name join: the user's name for
//     ActorUser, the API key's label for ActorAPIKey, nil for ActorSystem;
//   - the ID, which is APIKeyID for an API-key principal and UserID otherwise.
//     Principal.ActorID() is the only correct source; reading p.UserID directly
//     yields uuid.Nil for every API-key caller.
//
// Hardcoding ActorUser and reading UserID recorded "a user did this" over a
// zero UUID — an entry asserting a human acted while naming nobody. That is
// worse than no entry at all: a missing record prompts a question, a wrong one
// answers it incorrectly, and it lands on the action someone goes looking for
// precisely when something has gone wrong.
//
// ActorSystem with an empty id is the deliberate default. No principal in
// context means no human and no key, and naming either would be the same lie in
// a quieter form.
func auditActor(ctx context.Context) (string, string) {
	p, ok := domain.PrincipalFromContext(ctx)
	if !ok {
		return audit.ActorSystem, ""
	}
	if p.Type == domain.PrincipalAPIKey {
		return audit.ActorAPIKey, p.ActorID()
	}
	return audit.ActorUser, p.ActorID()
}

func (h *Handler) recordRunCreated(c *gin.Context, run Run, taskCount int) {
	if h.audit == nil {
		return
	}
	actorType, actorID := auditActor(c.Request.Context())
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

// recordRunRetried audits a retry against the SOURCE run, with the full
// accounting in the metadata: an audit row that recorded only "retried" would
// lose the one fact worth keeping, which is how much of the selection actually
// became work.
func (h *Handler) recordRunRetried(c *gin.Context, tenantID, sourceRunID uuid.UUID, res RetryResult) {
	if h.audit == nil {
		return
	}
	actorType, actorID := auditActor(c.Request.Context())
	meta := map[string]any{
		"source_run_id": sourceRunID.String(),
		"requested":     res.Requested,
		"created":       res.Created,
		"excluded":      len(res.Excluded),
	}
	if res.RunID != uuid.Nil {
		meta["run_id"] = res.RunID.String()
	}
	if res.Warning != "" {
		meta["warning"] = res.Warning
	}
	_, _ = h.audit.Record(c.Request.Context(), audit.Event{
		TenantID:   tenantID,
		ActorType:  actorType,
		ActorID:    actorID,
		Action:     ActionRunRetried,
		TargetType: "update_run",
		TargetID:   sourceRunID.String(),
		Metadata:   meta,
	})
}

func toAPIRetryResult(res RetryResult) gen.UpdateRunRetryResult {
	out := gen.UpdateRunRetryResult{
		Requested: res.Requested,
		Created:   res.Created,
		// Never nil: an empty JSON array is "nothing was excluded", whereas a
		// null would make a client choose what that meant.
		Excluded: make([]gen.UpdateRunRetryExclusion, 0, len(res.Excluded)),
	}
	if res.RunID != uuid.Nil {
		out.RunID = gen.NewOptUUID(res.RunID)
	}
	if res.Warning != "" {
		out.Warning = gen.NewOptString(res.Warning)
	}
	for _, ex := range res.Excluded {
		out.Excluded = append(out.Excluded, gen.UpdateRunRetryExclusion{
			TaskID:  ex.TaskID,
			Reason:  gen.UpdateRunRetryExclusionReason(ex.Reason),
			Message: ex.Message,
		})
	}
	return out
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

// toAPIRunSummary maps a RunSummary (from the list endpoint) to the OpenAPI
// representation, populating the aggregate count fields and omitting the tasks
// array (callers use the GET /update-runs/:runId detail endpoint for that).
func toAPIRunSummary(s RunSummary) gen.UpdateRun {
	out := toAPIRun(s.Run, nil)
	out.TaskCount = gen.NewOptInt64(s.TaskCount)
	out.SucceededCount = gen.NewOptInt64(s.SucceededCount)
	out.FailedCount = gen.NewOptInt64(s.FailedCount)
	out.SiteCount = gen.NewOptInt64(s.SiteCount)
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
	// The retry decision is computed HERE, by the server, from the task's own
	// recorded status. It is never left for a client to infer from detail/error
	// prose: those fields carry text the AGENT wrote, which no contract covers,
	// so a rule built on them is a safety decision resting on a regex.
	retryable, class := retryClassify(t.Status)
	out := gen.UpdateTask{
		ID:         t.ID,
		RunID:      t.RunID,
		TenantID:   t.TenantID,
		SiteID:     t.SiteID,
		TargetType: gen.UpdateTaskTargetType(t.TargetType),
		TargetSlug: t.TargetSlug,
		Status:     gen.UpdateTaskStatus(t.Status),
		Retryable:  retryable,
		RetryClass: gen.UpdateTaskRetryClass(class),
		CreatedAt:  t.CreatedAt,
		UpdatedAt:  t.UpdatedAt,
	}
	// Populated on the run DETAIL read (see ListUpdateTasksForRunWithSiteName);
	// the create response's freshly-inserted rows carry no join, so the field is
	// omitted there rather than sent as an empty string pretending to be a name.
	if t.SiteName != "" {
		out.SiteName = gen.NewOptString(t.SiteName)
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
