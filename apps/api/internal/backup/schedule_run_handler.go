package backup

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// ScheduleRunHandler serves the schedule-run endpoints under /api/v1. All
// routes require an authenticated principal with an active tenant; per-route
// RBAC is enforced by authz.RequirePermission. These are hand-rolled Gin
// routes (NOT ogen/openapi-generated) mirroring the restore_run_handler patterns.
type ScheduleRunHandler struct {
	svc     *Service
	userDir UserDirectory // optional; nil → triggered_by_email/name stay null
}

// NewScheduleRunHandler builds a ScheduleRunHandler.
func NewScheduleRunHandler(svc *Service) *ScheduleRunHandler {
	return &ScheduleRunHandler{svc: svc}
}

// SetUserDirectory wires the optional UserDirectory that resolves triggered_by
// values to human-readable email+name. Call this in main after constructing
// the handler.
func (h *ScheduleRunHandler) SetUserDirectory(dir UserDirectory) {
	h.userDir = dir
}

// Register mounts the schedule-run routes on the /api/v1 router group.
//
//	GET /sites/:siteId/schedule-runs            — list runs for site (PermSiteRead)
//	GET /schedule-runs/:runId                   — get run by id      (PermSiteRead)
func (h *ScheduleRunHandler) Register(r *gin.RouterGroup) {
	// Per-siteId route: RequireSiteAccess enforces the site allowlist for
	// site-scoped principals (belt-and-braces in front of the RLS policy).
	r.GET("/sites/:siteId/schedule-runs", authz.RequirePermission(authz.PermSiteRead), authz.RequireSiteAccess("siteId"), h.listForSite)
	// By-runId route: site isolation is enforced via scoped RLS (the
	// RESTRICTIVE policy on backup_schedule_runs denies rows whose site_id is
	// outside AllowedSiteIDs). No :siteId param to check here.
	r.GET("/schedule-runs/:runId", authz.RequirePermission(authz.PermSiteRead), h.getByID)
}

// ---------------------------------------------------------------------------
// DTOs
// ---------------------------------------------------------------------------

// scheduleRunDTO is the wire shape for a schedule run.
type scheduleRunDTO struct {
	ID               string  `json:"id"`
	SiteID           string  `json:"site_id"`
	ScheduleID       string  `json:"schedule_id"`
	SnapshotID       string  `json:"snapshot_id,omitempty"`
	ScheduledFor     string  `json:"scheduled_for"`
	Status           string  `json:"status"`
	Kind             string  `json:"kind"`
	Error            string  `json:"error,omitempty"`
	TriggeredBy      string  `json:"triggered_by,omitempty"`
	TriggeredByEmail *string `json:"triggered_by_email"`
	TriggeredByName  *string `json:"triggered_by_name"`
	CreatedAt        string  `json:"created_at"`
	StartedAt        string  `json:"started_at,omitempty"`
	FinishedAt       string  `json:"finished_at,omitempty"`
}

type scheduleRunListDTO struct {
	Upcoming []scheduleRunDTO `json:"upcoming"`
	Past     []scheduleRunDTO `json:"past"`
}

// ---------------------------------------------------------------------------
// Mapping helpers
// ---------------------------------------------------------------------------

// toScheduleRunDTOWithActor builds the DTO and populates triggered_by_email /
// triggered_by_name when non-empty strings are provided (from a resolved
// UserDirectory lookup). Empty strings produce null JSON fields.
func toScheduleRunDTOWithActor(r ScheduleRun, email, name string) scheduleRunDTO {
	d := scheduleRunDTO{
		ID:           r.ID.String(),
		SiteID:       r.SiteID.String(),
		ScheduleID:   r.ScheduleID.String(),
		Status:       r.Status,
		Kind:         r.Kind,
		ScheduledFor: r.ScheduledFor.UTC().Format(rfc3339),
		CreatedAt:    r.CreatedAt.UTC().Format(rfc3339),
	}
	if r.SnapshotID != nil {
		d.SnapshotID = r.SnapshotID.String()
	}
	if r.Error != nil && *r.Error != "" {
		d.Error = *r.Error
	}
	if r.TriggeredBy != nil && *r.TriggeredBy != "" {
		d.TriggeredBy = *r.TriggeredBy
	}
	if email != "" {
		v := email
		d.TriggeredByEmail = &v
	}
	if name != "" {
		v := name
		d.TriggeredByName = &v
	}
	if r.StartedAt != nil {
		d.StartedAt = r.StartedAt.UTC().Format(rfc3339)
	}
	if r.FinishedAt != nil {
		d.FinishedAt = r.FinishedAt.UTC().Format(rfc3339)
	}
	return d
}

// resolveScheduleRunActor calls the optional UserDirectory and returns (email,
// name). Returns empty strings when no directory is wired, triggered_by is nil
// or empty, or lookup fails. Schedule-fired runs carry triggered_by="schedule"
// which is not a UUID, so ResolveActor will return ok=false — that's correct.
func (h *ScheduleRunHandler) resolveScheduleRunActor(ctx context.Context, run ScheduleRun) (email, name string) {
	if h.userDir == nil || run.TriggeredBy == nil || *run.TriggeredBy == "" {
		return
	}
	e, n, ok := h.userDir.ResolveActor(ctx, *run.TriggeredBy)
	if !ok {
		return
	}
	return e, n
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

// listForSite returns schedule runs for a site. The ?status= query parameter
// selects "upcoming" (non-terminal), "past" (terminal), or both (default,
// split into two sub-lists). Pagination: upcoming is bounded to 10; past uses
// limit/offset.
func (h *ScheduleRunHandler) listForSite(c *gin.Context) {
	if h.svc.scheduleRuns == nil {
		httpx.Error(c, domain.ServiceUnavailable("schedule_runs_unwired", "schedule run persistence is not configured"))
		return
	}
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	siteID, ok := uuidParam(c, "siteId", "invalid_site_id")
	if !ok {
		return
	}
	statusFilter := c.Query("status")
	limit := parseInt32(c.Query("limit"), 50)
	offset := parseInt32(c.Query("offset"), 0)
	ctx := c.Request.Context()

	var upcoming []scheduleRunDTO
	var past []scheduleRunDTO

	switch statusFilter {
	case "upcoming":
		runs, err := h.svc.scheduleRuns.ListUpcomingScheduleRuns(ctx, tenantID, siteID, 10)
		if err != nil {
			httpx.Error(c, err)
			return
		}
		upcoming = make([]scheduleRunDTO, 0, len(runs))
		for _, r := range runs {
			email, name := h.resolveScheduleRunActor(ctx, r)
			upcoming = append(upcoming, toScheduleRunDTOWithActor(r, email, name))
		}
		past = []scheduleRunDTO{}

	case "past":
		runs, err := h.svc.scheduleRuns.ListPastScheduleRuns(ctx, tenantID, siteID, limit, offset)
		if err != nil {
			httpx.Error(c, err)
			return
		}
		upcoming = []scheduleRunDTO{}
		past = make([]scheduleRunDTO, 0, len(runs))
		for _, r := range runs {
			email, name := h.resolveScheduleRunActor(ctx, r)
			past = append(past, toScheduleRunDTOWithActor(r, email, name))
		}

	default:
		// Both lists.
		upRuns, err := h.svc.scheduleRuns.ListUpcomingScheduleRuns(ctx, tenantID, siteID, 10)
		if err != nil {
			httpx.Error(c, err)
			return
		}
		pastRuns, err := h.svc.scheduleRuns.ListPastScheduleRuns(ctx, tenantID, siteID, limit, offset)
		if err != nil {
			httpx.Error(c, err)
			return
		}
		upcoming = make([]scheduleRunDTO, 0, len(upRuns))
		for _, r := range upRuns {
			email, name := h.resolveScheduleRunActor(ctx, r)
			upcoming = append(upcoming, toScheduleRunDTOWithActor(r, email, name))
		}
		past = make([]scheduleRunDTO, 0, len(pastRuns))
		for _, r := range pastRuns {
			email, name := h.resolveScheduleRunActor(ctx, r)
			past = append(past, toScheduleRunDTOWithActor(r, email, name))
		}
	}

	c.JSON(http.StatusOK, scheduleRunListDTO{Upcoming: upcoming, Past: past})
}

// getByID returns a schedule run by its UUID.
func (h *ScheduleRunHandler) getByID(c *gin.Context) {
	if h.svc.scheduleRuns == nil {
		httpx.Error(c, domain.ServiceUnavailable("schedule_runs_unwired", "schedule run persistence is not configured"))
		return
	}
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	runID, ok := uuidParam(c, "runId", "invalid_run_id")
	if !ok {
		return
	}
	run, err := h.svc.scheduleRuns.GetScheduleRun(c.Request.Context(), tenantID, runID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	// This route has no :siteId for RequireSiteAccess to guard, so enforce the
	// run's owning site here — mirror restore_run_handler. A site-scoped
	// collaborator must not read a schedule run for a non-granted site.
	if !canReadSite(c, run.SiteID) {
		httpx.Error(c, domain.Forbidden("forbidden", "you do not have access to this site"))
		return
	}
	email, name := h.resolveScheduleRunActor(c.Request.Context(), run)
	c.JSON(http.StatusOK, toScheduleRunDTOWithActor(run, email, name))
}
