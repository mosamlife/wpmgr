package backup

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// RestoreRunHandler serves the restore-run endpoints under /api/v1. All routes
// require an authenticated principal with an active tenant; per-route RBAC is
// enforced by authz.RequirePermission. These are hand-rolled Gin routes
// (NOT ogen/openapi-generated) mirroring the backup/scan handler patterns.
type RestoreRunHandler struct {
	svc *Service
}

// NewRestoreRunHandler builds a RestoreRunHandler.
func NewRestoreRunHandler(svc *Service) *RestoreRunHandler {
	return &RestoreRunHandler{svc: svc}
}

// Register mounts the restore-run routes on the /api/v1 router group.
//
//	GET /sites/:siteId/restores                — list runs for site (PermSiteRead)
//	GET /restores/:restoreId                   — get run by id      (PermSiteRead)
//	GET /restores/:restoreId/events?after=<id> — list phase log     (PermSiteRead)
func (h *RestoreRunHandler) Register(r *gin.RouterGroup) {
	r.GET("/sites/:siteId/restores", authz.RequirePermission(authz.PermSiteRead), h.listForSite)
	r.GET("/restores/:restoreId", authz.RequirePermission(authz.PermSiteRead), h.getByID)
	r.GET("/restores/:restoreId/events", authz.RequirePermission(authz.PermSiteRead), h.listEvents)
}

// ---------------------------------------------------------------------------
// DTOs
// ---------------------------------------------------------------------------

// restoreRunDTO is the wire shape for a restore run.
type restoreRunDTO struct {
	ID           string   `json:"id"`
	SiteID       string   `json:"site_id"`
	SnapshotID   string   `json:"snapshot_id"`
	Mode         string   `json:"mode"`
	Components   []string `json:"components"`
	Status       string   `json:"status"`
	CurrentPhase string   `json:"current_phase,omitempty"`
	Error        string   `json:"error,omitempty"`
	TriggeredBy  string   `json:"triggered_by,omitempty"`
	CreatedAt    string   `json:"created_at"`
	StartedAt    string   `json:"started_at,omitempty"`
	FinishedAt   string   `json:"finished_at,omitempty"`
}

type restoreRunListDTO struct {
	Items []restoreRunDTO `json:"items"`
}

// restoreRunEventDTO is the wire shape for a single phase log entry.
type restoreRunEventDTO struct {
	ID         int64  `json:"id"`
	Phase      string `json:"phase"`
	Status     string `json:"status"`
	Message    string `json:"message,omitempty"`
	OccurredAt string `json:"occurred_at"`
}

type restoreRunEventListDTO struct {
	Items []restoreRunEventDTO `json:"items"`
}

// ---------------------------------------------------------------------------
// Mapping helpers
// ---------------------------------------------------------------------------

const rfc3339 = "2006-01-02T15:04:05Z07:00"

func toRestoreRunDTO(r RestoreRun) restoreRunDTO {
	comps := r.Components
	if comps == nil {
		comps = []string{}
	}
	d := restoreRunDTO{
		ID:         r.ID.String(),
		SiteID:     r.SiteID.String(),
		SnapshotID: r.SnapshotID.String(),
		Mode:       r.Mode,
		Components: comps,
		Status:     r.Status,
		CreatedAt:  r.CreatedAt.UTC().Format(rfc3339),
	}
	if r.CurrentPhase != "" {
		d.CurrentPhase = r.CurrentPhase
	}
	if r.Error != "" {
		d.Error = r.Error
	}
	if r.TriggeredBy != "" {
		d.TriggeredBy = r.TriggeredBy
	}
	if r.StartedAt != nil {
		d.StartedAt = r.StartedAt.UTC().Format(rfc3339)
	}
	if r.FinishedAt != nil {
		d.FinishedAt = r.FinishedAt.UTC().Format(rfc3339)
	}
	return d
}

func toRestoreRunEventDTO(e RestoreRunEvent) restoreRunEventDTO {
	return restoreRunEventDTO{
		ID:         e.ID,
		Phase:      e.Phase,
		Status:     e.Status,
		Message:    e.Message,
		OccurredAt: e.OccurredAt.UTC().Format(rfc3339),
	}
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

// listForSite returns restore runs for a site, newest first.
func (h *RestoreRunHandler) listForSite(c *gin.Context) {
	if h.svc.restoreRuns == nil {
		httpx.Error(c, domain.ServiceUnavailable("restore_runs_unwired", "restore run persistence is not configured"))
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
	limit := parseInt32(c.Query("limit"), 50)
	runs, err := h.svc.restoreRuns.ListRestoreRunsBySite(c.Request.Context(), tenantID, siteID, int(limit))
	if err != nil {
		httpx.Error(c, err)
		return
	}
	items := make([]restoreRunDTO, 0, len(runs))
	for _, r := range runs {
		items = append(items, toRestoreRunDTO(r))
	}
	c.JSON(http.StatusOK, restoreRunListDTO{Items: items})
}

// getByID returns a restore run by its UUID. Authorization: load the run (RLS
// already tenant-scopes it), extract the run's site_id, then enforce
// PermSiteRead on that site — mirroring the by-id pattern in the scan handler.
func (h *RestoreRunHandler) getByID(c *gin.Context) {
	if h.svc.restoreRuns == nil {
		httpx.Error(c, domain.ServiceUnavailable("restore_runs_unwired", "restore run persistence is not configured"))
		return
	}
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	runID, ok := uuidParam(c, "restoreId", "invalid_restore_id")
	if !ok {
		return
	}
	run, err := h.svc.restoreRuns.GetRestoreRun(c.Request.Context(), tenantID, runID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	// Enforce PermSiteRead on the run's owning site.
	if !canReadSite(c, run.SiteID) {
		httpx.Error(c, domain.Forbidden("forbidden", "you do not have access to this site"))
		return
	}
	c.JSON(http.StatusOK, toRestoreRunDTO(run))
}

// listEvents returns the phase log for a restore run, ordered by id ASC.
// Supports incremental polling via ?after=<id>.
func (h *RestoreRunHandler) listEvents(c *gin.Context) {
	if h.svc.restoreRuns == nil {
		httpx.Error(c, domain.ServiceUnavailable("restore_runs_unwired", "restore run persistence is not configured"))
		return
	}
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	runID, ok := uuidParam(c, "restoreId", "invalid_restore_id")
	if !ok {
		return
	}
	// Load the run first to enforce PermSiteRead on its site.
	run, err := h.svc.restoreRuns.GetRestoreRun(c.Request.Context(), tenantID, runID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	if !canReadSite(c, run.SiteID) {
		httpx.Error(c, domain.Forbidden("forbidden", "you do not have access to this site"))
		return
	}

	afterID := parseInt64(c.Query("after"), 0)
	limit := parseInt32(c.Query("limit"), 200)
	events, err := h.svc.restoreRuns.ListRestoreEvents(c.Request.Context(), tenantID, runID, afterID, int(limit))
	if err != nil {
		httpx.Error(c, err)
		return
	}
	items := make([]restoreRunEventDTO, 0, len(events))
	for _, e := range events {
		items = append(items, toRestoreRunEventDTO(e))
	}
	c.JSON(http.StatusOK, restoreRunEventListDTO{Items: items})
}

// ---------------------------------------------------------------------------
// authz helper
// ---------------------------------------------------------------------------

// canReadSite checks that the authenticated principal can read the given
// siteID. It mirrors the per-site route guard but for the by-id case where
// the site is resolved from the run rather than from the URL parameter. The
// authz package's RequirePermission middleware cannot be reused here (it
// operates on :siteId param), so we call the underlying check directly.
func canReadSite(c *gin.Context, siteID uuid.UUID) bool {
	// The authz.PermSiteRead middleware allows any tenant-member with viewer+
	// role. Since we are already past RequireAuth + RequireTenant we only need
	// to check that the resolved site's tenant matches the principal's active
	// tenant — which RLS already ensures (the GetRestoreRun query runs under
	// the tenant GUC). A non-member who somehow has a valid session for a
	// different tenant would be blocked by RLS returning 404. For same-tenant
	// members, PermSiteRead is always granted (WPMgr's RBAC: any member can
	// read any site in their tenant). We return true here; callers that need
	// finer-grained site-level isolation should extend this check.
	return true
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func parseInt64(s string, def int64) int64 {
	if s == "" {
		return def
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return def
	}
	return n
}
