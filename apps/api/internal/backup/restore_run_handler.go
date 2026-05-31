package backup

import (
	"context"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// UserDirectory resolves a triggered_by actor string (a user UUID) to a
// human-readable identity. The id parameter is the raw triggered_by value from
// the restore_run row (typically a user UUID string). Returns ok=false for
// unparseable strings, API-key IDs, or unknown user IDs — in those cases the
// caller keeps the raw triggered_by and leaves the email/name fields null.
type UserDirectory interface {
	ResolveActor(ctx context.Context, id string) (email string, name string, ok bool)
}

// RestoreRunHandler serves the restore-run endpoints under /api/v1. All routes
// require an authenticated principal with an active tenant; per-route RBAC is
// enforced by authz.RequirePermission. These are hand-rolled Gin routes
// (NOT ogen/openapi-generated) mirroring the backup/scan handler patterns.
type RestoreRunHandler struct {
	svc     *Service
	userDir UserDirectory // optional; nil → triggered_by_email/name stay null
}

// NewRestoreRunHandler builds a RestoreRunHandler.
func NewRestoreRunHandler(svc *Service) *RestoreRunHandler {
	return &RestoreRunHandler{svc: svc}
}

// SetUserDirectory wires the optional UserDirectory that resolves triggered_by
// UUIDs to human-readable email+name. Call this in main after constructing the
// handler.
func (h *RestoreRunHandler) SetUserDirectory(dir UserDirectory) {
	h.userDir = dir
}

// Register mounts the restore-run routes on the /api/v1 router group.
//
//	GET /sites/:siteId/restores                — list runs for site (PermSiteRead)
//	GET /restores/:restoreId                   — get run by id      (PermSiteRead)
//	GET /restores/:restoreId/events?after=<id> — list phase log     (PermSiteRead)
func (h *RestoreRunHandler) Register(r *gin.RouterGroup) {
	// Per-siteId route: RequireSiteAccess enforces the site allowlist for
	// site-scoped principals (belt-and-braces in front of the RLS policy).
	r.GET("/sites/:siteId/restores", authz.RequirePermission(authz.PermSiteRead), authz.RequireSiteAccess("siteId"), h.listForSite)
	// By-restoreId routes: site isolation is enforced via scoped RLS (the
	// RESTRICTIVE policy on restore_runs denies rows whose site_id is outside
	// AllowedSiteIDs). The handler also enforces canReadSite (see below).
	r.GET("/restores/:restoreId", authz.RequirePermission(authz.PermSiteRead), h.getByID)
	r.GET("/restores/:restoreId/events", authz.RequirePermission(authz.PermSiteRead), h.listEvents)
}

// ---------------------------------------------------------------------------
// DTOs
// ---------------------------------------------------------------------------

// restoreRunDTO is the wire shape for a restore run.
type restoreRunDTO struct {
	ID               string   `json:"id"`
	SiteID           string   `json:"site_id"`
	SnapshotID       string   `json:"snapshot_id"`
	Mode             string   `json:"mode"`
	Components       []string `json:"components"`
	Status           string   `json:"status"`
	CurrentPhase     string   `json:"current_phase,omitempty"`
	Error            string   `json:"error,omitempty"`
	TriggeredBy      string   `json:"triggered_by,omitempty"`
	TriggeredByEmail *string  `json:"triggered_by_email"`
	TriggeredByName  *string  `json:"triggered_by_name"`
	CreatedAt        string   `json:"created_at"`
	StartedAt        string   `json:"started_at,omitempty"`
	FinishedAt       string   `json:"finished_at,omitempty"`
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
	return toRestoreRunDTOWithActor(r, "", "")
}

// toRestoreRunDTOWithActor builds the DTO and populates triggered_by_email /
// triggered_by_name when non-empty strings are provided (from a resolved
// UserDirectory lookup). Empty strings produce null JSON fields.
func toRestoreRunDTOWithActor(r RestoreRun, email, name string) restoreRunDTO {
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

// resolveActor calls the optional UserDirectory and returns (email, name).
// Returns empty strings when no directory is wired or lookup fails.
func (h *RestoreRunHandler) resolveActor(ctx context.Context, triggeredBy string) (email, name string) {
	if h.userDir == nil || triggeredBy == "" {
		return
	}
	e, n, ok := h.userDir.ResolveActor(ctx, triggeredBy)
	if !ok {
		return
	}
	return e, n
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
		email, name := h.resolveActor(c.Request.Context(), r.TriggeredBy)
		items = append(items, toRestoreRunDTOWithActor(r, email, name))
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
	email, name := h.resolveActor(c.Request.Context(), run.TriggeredBy)
	c.JSON(http.StatusOK, toRestoreRunDTOWithActor(run, email, name))
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
// siteID. It is used for by-id routes where the site is resolved from the
// resource row (not the URL parameter), so RequireSiteAccess cannot be applied
// as middleware.
//
// For org-scoped principals: RLS already tenant-scopes the query; any member
// can read any site in their tenant (PermSiteRead is a tenant-wide grant).
//
// For site-scoped principals (outside collaborators): this is the SOLE site
// gate for by-id resource routes. These resolvers (GetSnapshot, GetRestoreRun,
// GetScheduleRun, GetFinding) run under plain InTenantTx — they do NOT set
// app.site_scope — so the RESTRICTIVE *_site_scope RLS policy is inert on them
// (it only fires under InScopedTenantTx). The explicit AllowedSiteIDs check
// here is therefore the actual enforcement, not a redundant second layer; call
// it after resolving the resource's site_id and 403/404 when it returns false.
func canReadSite(c *gin.Context, siteID uuid.UUID) bool {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok {
		return false
	}
	if p.Scope != domain.ScopeSite {
		// Org-scoped (or API-key): RLS + tenant filter is sufficient.
		return true
	}
	// Site-scoped: check explicit allowlist.
	for _, allowed := range p.AllowedSiteIDs {
		if allowed == siteID {
			return true
		}
	}
	return false
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
