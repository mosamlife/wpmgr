package scan

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// Handler serves the operator-facing scan routes under /api/v1.
//
//	POST /sites/{id}/scans                          — start scan (Write → 202)
//	GET  /sites/{id}/scans                          — list runs (Read)
//	GET  /sites/{id}/scans/{runId}                  — get run (Read)
//	GET  /sites/{id}/scans/{runId}/findings         — list findings for run (Read)
//	POST /findings/{id}/ignore                      — toggle ignore (Write)
//	POST /sites/{id}/scans/{runId}/findings/{fid}/file — fetch file (Write → base64)
type Handler struct {
	svc *Service
}

// NewHandler builds the operator handler.
func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// Register mounts the scan routes on the authenticated /api/v1 group.
func (h *Handler) Register(r *gin.RouterGroup) {
	// Per-site scan run routes. RequireSiteAccess("siteId") is applied on the
	// group so every sub-route inherits it. This enforces the site allowlist for
	// site-scoped principals (belt-and-braces in front of the RLS policy on
	// scan_runs / scan_findings / scan_run_hashes).
	sites := r.Group("/sites/:siteId", authz.RequireSiteAccess("siteId"))
	sites.POST("/scans", authz.RequirePermission(authz.PermSiteWrite), h.startRun)
	sites.GET("/scans", authz.RequirePermission(authz.PermSiteRead), h.listRuns)
	sites.GET("/scans/:runId", authz.RequirePermission(authz.PermSiteRead), h.getRun)
	sites.GET("/scans/:runId/findings", authz.RequirePermission(authz.PermSiteRead), h.listFindingsForRun)
	sites.POST("/scans/:runId/findings/:fid/file", authz.RequirePermission(authz.PermSiteWrite), h.fetchFile)

	// Global finding route (no site prefix — finding ID is globally unique per
	// tenant). RequireSiteAccess cannot guard it (no :siteId in the path), so
	// IgnoreFinding resolves the finding's site and calls Principal.CanAccessSite
	// before mutating — a site-scoped collaborator is denied findings outside
	// their allowlist.
	r.POST("/findings/:id/ignore", authz.RequirePermission(authz.PermSiteWrite), h.ignoreFinding)
}

// ---------------------------------------------------------------------------
// DTOs
// ---------------------------------------------------------------------------

type startScanBody struct {
	Kind string `json:"kind"`
}

type runDTO struct {
	ID            string         `json:"id"`
	SiteID        string         `json:"site_id"`
	Kind          string         `json:"kind"`
	Status        string         `json:"status"`
	FilesScanned  int64          `json:"files_scanned"`
	WPVersion     string         `json:"wp_version,omitempty"`
	Locale        string         `json:"locale,omitempty"`
	Error         string         `json:"error,omitempty"`
	FindingCounts map[string]int `json:"finding_counts,omitempty"`
	CreatedAt     string         `json:"created_at"`
	StartedAt     string         `json:"started_at,omitempty"`
	FinishedAt    string         `json:"finished_at,omitempty"`
}

type runListDTO struct {
	Items []runDTO `json:"items"`
}

type findingDTO struct {
	ID          string `json:"id"`
	SiteID      string `json:"site_id"`
	RunID       string `json:"run_id"`
	FindingType string `json:"finding_type"`
	Path        string `json:"path"`
	Severity    string `json:"severity"`
	ExpectedMD5 string `json:"expected_md5,omitempty"`
	ActualMD5   string `json:"actual_md5,omitempty"`
	Ignored     bool   `json:"ignored"`
	IgnoredBy   string `json:"ignored_by,omitempty"`
	CreatedAt   string `json:"created_at"`
	LastSeenRun string `json:"last_seen_run"`
}

type findingListDTO struct {
	Items []findingDTO `json:"items"`
}

type ignoreBody struct {
	Ignored bool `json:"ignored"`
}

type fetchFileResponseDTO struct {
	OK            bool   `json:"ok"`
	Path          string `json:"path"`
	Size          int64  `json:"size"`
	ContentBase64 string `json:"content_base64,omitempty"`
	Error         string `json:"error,omitempty"`
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func toRunDTO(r Run) runDTO {
	d := runDTO{
		ID:            r.ID.String(),
		SiteID:        r.SiteID.String(),
		Kind:          r.Kind,
		Status:        r.Status,
		FilesScanned:  r.FilesScanned,
		WPVersion:     r.WPVersion,
		Locale:        r.Locale,
		Error:         r.Error,
		FindingCounts: r.FindingCounts,
		CreatedAt:     r.CreatedAt.UTC().Format(time.RFC3339),
	}
	if r.StartedAt != nil {
		d.StartedAt = r.StartedAt.UTC().Format(time.RFC3339)
	}
	if r.FinishedAt != nil {
		d.FinishedAt = r.FinishedAt.UTC().Format(time.RFC3339)
	}
	return d
}

func toFindingDTO(f Finding) findingDTO {
	return findingDTO{
		ID:          f.ID.String(),
		SiteID:      f.SiteID.String(),
		RunID:       f.RunID.String(),
		FindingType: f.FindingType,
		Path:        f.Path,
		Severity:    f.Severity,
		ExpectedMD5: f.ExpectedMD5,
		ActualMD5:   f.ActualMD5,
		Ignored:     f.Ignored,
		IgnoredBy:   f.IgnoredBy,
		CreatedAt:   f.CreatedAt.UTC().Format(time.RFC3339),
		LastSeenRun: f.LastSeenRun.String(),
	}
}

func bindJSON(c *gin.Context, dst any) error {
	dec := json.NewDecoder(c.Request.Body)
	if err := dec.Decode(dst); err != nil {
		return domain.Validation("invalid_body", "request body is not valid JSON: "+err.Error())
	}
	return nil
}

// ---------------------------------------------------------------------------
// route handlers
// ---------------------------------------------------------------------------

func (h *Handler) startRun(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, err := uuid.Parse(c.Param("siteId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_site_id", "siteId is not a valid UUID"))
		return
	}
	var body startScanBody
	// Body is optional; empty body defaults to kind=core.
	_ = bindJSON(c, &body)

	run, err := h.svc.StartRun(c.Request.Context(), p.TenantID, siteID, body.Kind)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusAccepted, toRunDTO(run))
}

func (h *Handler) listRuns(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, err := uuid.Parse(c.Param("siteId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_site_id", "siteId is not a valid UUID"))
		return
	}
	limit := 50
	if s := c.Query("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			limit = n
		}
	}
	runs, err := h.svc.ListRuns(c.Request.Context(), p.TenantID, siteID, limit)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	items := make([]runDTO, 0, len(runs))
	for _, r := range runs {
		items = append(items, toRunDTO(r))
	}
	c.JSON(http.StatusOK, runListDTO{Items: items})
}

func (h *Handler) getRun(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	runID, err := uuid.Parse(c.Param("runId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_run_id", "runId is not a valid UUID"))
		return
	}
	run, err := h.svc.GetRun(c.Request.Context(), p.TenantID, runID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	// The run is resolved by id only (tenant-scoped); bind it to a site the
	// caller may access so a site-scoped collaborator cannot read another
	// site's scan run by passing its runId under their own :siteId.
	if !p.CanAccessSite(run.SiteID) {
		httpx.Error(c, domain.Forbidden("forbidden", "you do not have access to this site"))
		return
	}
	c.JSON(http.StatusOK, toRunDTO(run))
}

func (h *Handler) listFindingsForRun(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, err := uuid.Parse(c.Param("siteId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_site_id", "siteId is not a valid UUID"))
		return
	}
	runID, err := uuid.Parse(c.Param("runId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_run_id", "runId is not a valid UUID"))
		return
	}
	limit := 100
	if s := c.Query("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			limit = n
		}
	}
	findings, err := h.svc.ListFindingsForRun(c.Request.Context(), p.TenantID, siteID, runID, limit)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	items := make([]findingDTO, 0, len(findings))
	for _, f := range findings {
		items = append(items, toFindingDTO(f))
	}
	c.JSON(http.StatusOK, findingListDTO{Items: items})
}

func (h *Handler) ignoreFinding(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	findingID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_finding_id", "finding id is not a valid UUID"))
		return
	}
	var body ignoreBody
	body.Ignored = true // default to ignoring
	_ = bindJSON(c, &body)

	ignoredBy := p.ActorID()
	f, err := h.svc.IgnoreFinding(c.Request.Context(), p.TenantID, findingID, body.Ignored, ignoredBy, p)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, toFindingDTO(f))
}

func (h *Handler) fetchFile(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	findingID, err := uuid.Parse(c.Param("fid"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_finding_id", "finding id is not a valid UUID"))
		return
	}
	resp, err := h.svc.FetchFile(c.Request.Context(), p.TenantID, findingID, p)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, fetchFileResponseDTO{
		OK:            resp.OK,
		Path:          resp.Path,
		Size:          resp.Size,
		ContentBase64: resp.ContentBase64,
		Error:         resp.Error,
	})
}
