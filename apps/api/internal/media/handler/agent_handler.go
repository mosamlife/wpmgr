package handler

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/mosamlife/wpmgr/apps/api/internal/agent"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/media/repo"
	"github.com/mosamlife/wpmgr/apps/api/internal/media/service"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// maxMediaAgentBody bounds each agent media-callback body. The sync-batch page
// (≤200 attachments) and the presign/status payloads are all small JSON, well
// under the agent middleware's 4 MiB buffer cap.
const maxMediaAgentBody = 4 << 20

// AgentHandler serves the agent-authenticated media callbacks under /agent/v1.
// Every route runs behind the agent Authenticator; the site + tenant come from
// the verified Ed25519 identity on the context (NEVER a client header). Each job
// is re-asserted to that tenant+site in the service before any mutation.
type AgentHandler struct {
	svc *service.Service
}

// NewAgentHandler builds the agent-facing media callback handler.
func NewAgentHandler(svc *service.Service) *AgentHandler {
	return &AgentHandler{svc: svc}
}

// Register mounts the callbacks on the agent-authenticated group.
func (h *AgentHandler) Register(r *gin.RouterGroup) {
	r.POST("/media/sync-batch", h.syncBatch)
	r.POST("/media/presign", h.presign)
	r.POST("/media/encode-ready", h.encodeReady)
	r.POST("/media/job-status", h.jobStatus)
	r.POST("/media/restore-status", h.restoreStatus)
}

// ---------------------------------------------------------------------------
// agent DTOs
// ---------------------------------------------------------------------------

type syncBatchAttachmentDTO struct {
	WPAttachmentID    int64  `json:"wp_attachment_id"`
	Title             string `json:"title"`
	OriginalPath      string `json:"original_path"`
	OriginalURL       string `json:"original_url"`
	OriginalMime      string `json:"original_mime"`
	OriginalWidth     *int   `json:"original_width"`
	OriginalHeight    *int   `json:"original_height"`
	OriginalSizeBytes int64  `json:"original_size_bytes"`
}

type syncBatchBody struct {
	Attachments []syncBatchAttachmentDTO `json:"attachments"`
}

type presignVariantDTO struct {
	Name       string `json:"name"`
	SourceSize int64  `json:"source_size"`
	SourceMime string `json:"source_mime"`
}

type presignBody struct {
	JobID    string              `json:"job_id"`
	Variants []presignVariantDTO `json:"variants"`
}

type encodeReadyBody struct {
	JobID    string              `json:"job_id"`
	Variants []presignVariantDTO `json:"variants"`
}

type jobStatusBody struct {
	JobID            string            `json:"job_id"`
	AppliedVariants  []string          `json:"applied_variants"`
	SizesUnoptimized map[string]string `json:"sizes_unoptimized"`
	CurrentFormat    string            `json:"current_format"`
	CurrentSizeBytes int64             `json:"current_size_bytes"`
	BytesBefore      *int64            `json:"bytes_before"`
	BytesAfter       *int64            `json:"bytes_after"`
	CompressionLevel string            `json:"compression_level"`
	TargetFormat     string            `json:"target_format"`
	RewriteStats     map[string]any    `json:"rewrite_stats"`
	Error            string            `json:"error"`
}

type restoreStatusBody struct {
	JobID    string `json:"job_id"`
	Restored bool   `json:"restored"`
	Error    string `json:"error"`
}

// ---------------------------------------------------------------------------
// route handlers
// ---------------------------------------------------------------------------

func (h *AgentHandler) syncBatch(c *gin.Context) {
	id, ok := identity(c)
	if !ok {
		return
	}
	var body syncBatchBody
	if !decode(c, &body) {
		return
	}
	rows := make([]repo.UpsertAssetInput, 0, len(body.Attachments))
	for _, a := range body.Attachments {
		rows = append(rows, repo.UpsertAssetInput{
			WPAttachmentID:    a.WPAttachmentID,
			Title:             a.Title,
			OriginalPath:      a.OriginalPath,
			OriginalURL:       a.OriginalURL,
			OriginalMime:      a.OriginalMime,
			OriginalWidth:     a.OriginalWidth,
			OriginalHeight:    a.OriginalHeight,
			OriginalSizeBytes: a.OriginalSizeBytes,
		})
	}
	n, err := h.svc.HandleSyncBatch(c.Request.Context(), id.TenantID, id.SiteID, service.SyncBatchInput{Attachments: rows})
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"upserted_count": n})
}

func (h *AgentHandler) presign(c *gin.Context) {
	id, ok := identity(c)
	if !ok {
		return
	}
	var body presignBody
	if !decode(c, &body) {
		return
	}
	variants := make([]service.PresignVariant, 0, len(body.Variants))
	for _, v := range body.Variants {
		variants = append(variants, service.PresignVariant{Name: v.Name, SourceSize: v.SourceSize, SourceMime: v.SourceMime})
	}
	urls, err := h.svc.HandlePresign(c.Request.Context(), id.TenantID, id.SiteID, body.JobID, variants)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"uploads": urls})
}

func (h *AgentHandler) encodeReady(c *gin.Context) {
	id, ok := identity(c)
	if !ok {
		return
	}
	var body encodeReadyBody
	if !decode(c, &body) {
		return
	}
	variants := make([]service.EncodeReadyVariant, 0, len(body.Variants))
	for _, v := range body.Variants {
		variants = append(variants, service.EncodeReadyVariant{Name: v.Name, SourceSize: v.SourceSize, SourceMime: v.SourceMime})
	}
	if err := h.svc.HandleEncodeReady(c.Request.Context(), id.TenantID, id.SiteID, body.JobID, variants); err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *AgentHandler) jobStatus(c *gin.Context) {
	id, ok := identity(c)
	if !ok {
		return
	}
	var body jobStatusBody
	if !decode(c, &body) {
		return
	}
	if err := h.svc.HandleApplyStatus(c.Request.Context(), id.TenantID, id.SiteID, body.JobID, service.ApplyStatusInput{
		AppliedVariants:  body.AppliedVariants,
		SizesUnoptimized: body.SizesUnoptimized,
		CurrentFormat:    body.CurrentFormat,
		CurrentSizeBytes: body.CurrentSizeBytes,
		BytesBefore:      body.BytesBefore,
		BytesAfter:       body.BytesAfter,
		CompressionLevel: body.CompressionLevel,
		TargetFormat:     body.TargetFormat,
		Error:            body.Error,
	}); err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *AgentHandler) restoreStatus(c *gin.Context) {
	id, ok := identity(c)
	if !ok {
		return
	}
	var body restoreStatusBody
	if !decode(c, &body) {
		return
	}
	if err := h.svc.HandleRestoreStatus(c.Request.Context(), id.TenantID, id.SiteID, body.JobID, service.RestoreStatusInput{
		Restored: body.Restored,
		Error:    body.Error,
	}); err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func identity(c *gin.Context) (agent.Identity, bool) {
	id, ok := agent.IdentityFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("agent_unauthenticated", "agent identity required"))
		return agent.Identity{}, false
	}
	return id, true
}

// decode reads the body with a hard cap BEFORE JSON decoding (the size cap
// belongs at the transport boundary), then unmarshals into dst.
func decode(c *gin.Context, dst any) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxMediaAgentBody+1024)
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "could not read request body or exceeds size cap"))
		return false
	}
	if err := json.Unmarshal(body, dst); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
		return false
	}
	return true
}
