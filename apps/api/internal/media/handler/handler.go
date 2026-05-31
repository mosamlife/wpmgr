// Package handler serves the Media Optimizer HTTP surface: the operator-facing
// dashboard routes under /api/v1/sites/:siteId/media/... (session + RBAC) and
// the agent-callback routes under /agent/v1/media/... (Ed25519 signed-request).
// DTOs are hand-rolled + c.JSON (the scan-feature convention — ADR-043 §7), not
// ogen-generated.
package handler

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/media/model"
	"github.com/mosamlife/wpmgr/apps/api/internal/media/repo"
	"github.com/mosamlife/wpmgr/apps/api/internal/media/service"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// Handler serves the operator-facing media routes under /api/v1.
type Handler struct {
	svc *service.Service
}

// NewHandler builds the operator handler.
func NewHandler(svc *service.Service) *Handler {
	return &Handler{svc: svc}
}

// Register mounts the dashboard routes nested under /sites/:siteId so
// RequireSiteAccess(":siteId") gates per-site (site-scoped collaborators
// included). sync/optimize/restore/cancel gate on PermSiteWrite; delete-originals
// on PermMediaDeleteOriginals (admin+).
func (h *Handler) Register(r *gin.RouterGroup) {
	g := r.Group("/sites/:siteId/media", authz.RequireSiteAccess("siteId"))
	g.GET("/assets", authz.RequirePermission(authz.PermSiteRead), h.listAssets)
	g.POST("/sync", authz.RequirePermission(authz.PermSiteWrite), h.sync)
	g.POST("/optimize", authz.RequirePermission(authz.PermSiteWrite), h.optimize)
	g.POST("/restore", authz.RequirePermission(authz.PermSiteWrite), h.restore)
	g.POST("/delete-originals", authz.RequirePermission(authz.PermMediaDeleteOriginals), h.deleteOriginals)
	g.POST("/cancel", authz.RequirePermission(authz.PermSiteWrite), h.cancel)
	g.GET("/jobs", authz.RequirePermission(authz.PermSiteRead), h.listJobs)
	g.GET("/jobs/:jobId", authz.RequirePermission(authz.PermSiteRead), h.getJob)
}

// ---------------------------------------------------------------------------
// DTOs
// ---------------------------------------------------------------------------

type assetDTO struct {
	ID                string            `json:"id"`
	SiteID            string            `json:"site_id"`
	WPAttachmentID    int64             `json:"wp_attachment_id"`
	Title             string            `json:"title"`
	OriginalURL       string            `json:"original_url"`
	OriginalMime      string            `json:"original_mime"`
	OriginalSizeBytes int64             `json:"original_size_bytes"`
	CurrentFormat     string            `json:"current_format"`
	CurrentSizeBytes  int64             `json:"current_size_bytes"`
	Status            string            `json:"status"`
	Generation        int               `json:"generation"`
	SizesOptimized    []string          `json:"sizes_optimized,omitempty"`
	SizesUnoptimized  map[string]string `json:"sizes_unoptimized,omitempty"`
	LastOptimizedAt   string            `json:"last_optimized_at,omitempty"`
}

type summaryDTO struct {
	Total      int64 `json:"total"`
	Optimized  int64 `json:"optimized"`
	Pending    int64 `json:"pending"`
	Failed     int64 `json:"failed"`
	BytesSaved int64 `json:"bytes_saved"`
}

type listAssetsDTO struct {
	Items      []assetDTO `json:"items"`
	NextCursor string     `json:"next_cursor,omitempty"`
	TotalCount int64      `json:"total_count"`
	Summary    summaryDTO `json:"summary"`
}

type jobDTO struct {
	ID                string `json:"id"`
	SiteID            string `json:"site_id"`
	AssetID           string `json:"asset_id,omitempty"`
	WPAttachmentID    int64  `json:"wp_attachment_id"`
	Kind              string `json:"kind"`
	TargetFormat      string `json:"target_format,omitempty"`
	TargetQuality     string `json:"target_quality,omitempty"`
	State             string `json:"state"`
	BytesBefore       *int64 `json:"bytes_before,omitempty"`
	BytesAfter        *int64 `json:"bytes_after,omitempty"`
	VariantsTotal     int    `json:"variants_total"`
	VariantsSucceeded int    `json:"variants_succeeded"`
	VariantsFailed    int    `json:"variants_failed"`
	ErrorReason       string `json:"error_reason,omitempty"`
	CreatedAt         string `json:"created_at"`
	StartedAt         string `json:"started_at,omitempty"`
	CompletedAt       string `json:"completed_at,omitempty"`
}

type variantDTO struct {
	VariantName        string `json:"variant_name"`
	SourceSizeBytes    int64  `json:"source_size_bytes"`
	OptimizedSizeBytes *int64 `json:"optimized_size_bytes,omitempty"`
	SourceMime         string `json:"source_mime"`
	OptimizedMime      string `json:"optimized_mime,omitempty"`
	EncodeMS           *int   `json:"encode_ms,omitempty"`
	State              string `json:"state"`
	Reason             string `json:"reason,omitempty"`
}

type jobListDTO struct {
	Items      []jobDTO `json:"items"`
	NextCursor string   `json:"next_cursor,omitempty"`
}

type jobDetailDTO struct {
	jobDTO
	Variants []variantDTO `json:"variants"`
}

type optimizeBody struct {
	AssetIDs      []string `json:"asset_ids"`
	AllPending    bool     `json:"all_pending"`
	TargetFormat  string   `json:"target_format"`
	TargetQuality string   `json:"target_quality"`
}

type assetSelectionBody struct {
	AssetIDs []string `json:"asset_ids"`
}

// ---------------------------------------------------------------------------
// converters
// ---------------------------------------------------------------------------

func toAssetDTO(a model.Asset) assetDTO {
	d := assetDTO{
		ID:                a.ID.String(),
		SiteID:            a.SiteID.String(),
		WPAttachmentID:    a.WPAttachmentID,
		Title:             a.Title,
		OriginalURL:       a.OriginalURL,
		OriginalMime:      a.OriginalMime,
		OriginalSizeBytes: a.OriginalSizeBytes,
		CurrentFormat:     a.CurrentFormat,
		CurrentSizeBytes:  a.CurrentSizeBytes,
		Status:            string(a.Status),
		Generation:        a.Generation,
		SizesOptimized:    a.SizesOptimized,
		SizesUnoptimized:  a.SizesUnoptimized,
	}
	if a.LastOptimizedAt != nil {
		d.LastOptimizedAt = a.LastOptimizedAt.UTC().Format(time.RFC3339)
	}
	return d
}

func toJobDTO(j model.Job) jobDTO {
	d := jobDTO{
		ID:                j.ID,
		SiteID:            j.SiteID.String(),
		WPAttachmentID:    j.WPAttachmentID,
		Kind:              string(j.Kind),
		TargetFormat:      j.TargetFormat,
		TargetQuality:     j.TargetQuality,
		State:             string(j.State),
		BytesBefore:       j.BytesBefore,
		BytesAfter:        j.BytesAfter,
		VariantsTotal:     j.VariantsTotal,
		VariantsSucceeded: j.VariantsSucceeded,
		VariantsFailed:    j.VariantsFailed,
		ErrorReason:       j.ErrorReason,
		CreatedAt:         j.CreatedAt.UTC().Format(time.RFC3339),
	}
	if j.AssetID != nil {
		d.AssetID = j.AssetID.String()
	}
	if j.StartedAt != nil {
		d.StartedAt = j.StartedAt.UTC().Format(time.RFC3339)
	}
	if j.CompletedAt != nil {
		d.CompletedAt = j.CompletedAt.UTC().Format(time.RFC3339)
	}
	return d
}

func toVariantDTO(v model.VariantResult) variantDTO {
	return variantDTO{
		VariantName:        v.VariantName,
		SourceSizeBytes:    v.SourceSizeBytes,
		OptimizedSizeBytes: v.OptimizedSizeBytes,
		SourceMime:         v.SourceMime,
		OptimizedMime:      v.OptimizedMime,
		EncodeMS:           v.EncodeMS,
		State:              string(v.State),
		Reason:             v.Reason,
	}
}

// ---------------------------------------------------------------------------
// route handlers
// ---------------------------------------------------------------------------

func (h *Handler) listAssets(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := siteParam(c)
	if !ok {
		return
	}
	in := repo.ListAssetsInput{
		Limit:  queryInt(c, "limit", 50),
		Cursor: c.Query("cursor"),
		Status: c.Query("status"),
		Format: c.Query("format"),
		Search: c.Query("search"),
	}
	res, err := h.svc.ListAssets(c.Request.Context(), p.TenantID, siteID, in)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	items := make([]assetDTO, 0, len(res.Items))
	for _, a := range res.Items {
		items = append(items, toAssetDTO(a))
	}
	c.JSON(http.StatusOK, listAssetsDTO{
		Items:      items,
		NextCursor: res.NextCursor,
		TotalCount: res.Summary.Total,
		Summary: summaryDTO{
			Total:      res.Summary.Total,
			Optimized:  res.Summary.Optimized,
			Pending:    res.Summary.Pending,
			Failed:     res.Summary.Failed,
			BytesSaved: res.Summary.BytesSaved,
		},
	})
}

func (h *Handler) sync(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := siteParam(c)
	if !ok {
		return
	}
	res, err := h.svc.Sync(c.Request.Context(), p.TenantID, siteID, p)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusAccepted, gin.H{
		"job_id":     res.JobID,
		"started_at": res.StartedAt.UTC().Format(time.RFC3339),
	})
}

func (h *Handler) optimize(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := siteParam(c)
	if !ok {
		return
	}
	var body optimizeBody
	_ = bindJSON(c, &body)
	assetIDs, perr := parseUUIDs(body.AssetIDs)
	if perr != nil {
		httpx.Error(c, perr)
		return
	}
	res, err := h.svc.StartOptimize(c.Request.Context(), p.TenantID, siteID, assetIDs, body.AllPending, body.TargetFormat, body.TargetQuality, p)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusAccepted, gin.H{
		"batch_job_id": res.BatchJobID,
		"queued_count": res.QueuedCount,
	})
}

func (h *Handler) restore(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := siteParam(c)
	if !ok {
		return
	}
	var body assetSelectionBody
	_ = bindJSON(c, &body)
	assetIDs, perr := parseUUIDs(body.AssetIDs)
	if perr != nil {
		httpx.Error(c, perr)
		return
	}
	res, err := h.svc.StartRestore(c.Request.Context(), p.TenantID, siteID, assetIDs, p)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusAccepted, gin.H{
		"batch_job_id": res.BatchJobID,
		"queued_count": res.QueuedCount,
	})
}

func (h *Handler) deleteOriginals(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := siteParam(c)
	if !ok {
		return
	}
	var body assetSelectionBody
	_ = bindJSON(c, &body)
	assetIDs, perr := parseUUIDs(body.AssetIDs)
	if perr != nil {
		httpx.Error(c, perr)
		return
	}
	res, err := h.svc.StartDeleteOriginals(c.Request.Context(), p.TenantID, siteID, assetIDs, p)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusAccepted, gin.H{
		"batch_job_id": res.BatchJobID,
		"queued_count": res.QueuedCount,
		"irreversible": true,
	})
}

func (h *Handler) cancel(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := siteParam(c)
	if !ok {
		return
	}
	res, err := h.svc.Cancel(c.Request.Context(), p.TenantID, siteID, p)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"ok":              res.OK,
		"cancelled_count": res.CancelledCount,
	})
}

func (h *Handler) listJobs(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := siteParam(c)
	if !ok {
		return
	}
	jobs, next, err := h.svc.ListJobs(c.Request.Context(), p.TenantID, siteID, repo.ListJobsInput{
		Limit:  queryInt(c, "limit", 50),
		Cursor: c.Query("cursor"),
		State:  c.Query("state"),
	})
	if err != nil {
		httpx.Error(c, err)
		return
	}
	items := make([]jobDTO, 0, len(jobs))
	for _, j := range jobs {
		items = append(items, toJobDTO(j))
	}
	c.JSON(http.StatusOK, jobListDTO{Items: items, NextCursor: next})
}

func (h *Handler) getJob(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	jobID := c.Param("jobId")
	if jobID == "" {
		httpx.Error(c, domain.Validation("invalid_job_id", "jobId is required"))
		return
	}
	detail, err := h.svc.GetJob(c.Request.Context(), p.TenantID, jobID, p)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	variants := make([]variantDTO, 0, len(detail.Variants))
	for _, v := range detail.Variants {
		variants = append(variants, toVariantDTO(v))
	}
	c.JSON(http.StatusOK, jobDetailDTO{jobDTO: toJobDTO(detail.Job), Variants: variants})
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func siteParam(c *gin.Context) (uuid.UUID, bool) {
	siteID, err := uuid.Parse(c.Param("siteId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_site_id", "siteId is not a valid UUID"))
		return uuid.Nil, false
	}
	return siteID, true
}

func queryInt(c *gin.Context, key string, def int) int {
	if s := c.Query(key); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			return n
		}
	}
	return def
}

func parseUUIDs(in []string) ([]uuid.UUID, error) {
	out := make([]uuid.UUID, 0, len(in))
	for _, s := range in {
		id, err := uuid.Parse(s)
		if err != nil {
			return nil, domain.Validation("invalid_asset_id", "asset_ids contains an invalid UUID")
		}
		out = append(out, id)
	}
	return out, nil
}

func bindJSON(c *gin.Context, dst any) error {
	dec := json.NewDecoder(c.Request.Body)
	if err := dec.Decode(dst); err != nil {
		return domain.Validation("invalid_body", "request body is not valid JSON: "+err.Error())
	}
	return nil
}
