package backup

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agent"
	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// AgentHandler serves the agent-authenticated backup callbacks under /agent/v1.
// Every route runs behind the agent Authenticator; the site/tenant come from
// the verified Ed25519 identity on the context (NEVER a client header). The
// snapshot path param is always re-validated against that tenant scope, so an
// agent can only act on its own tenant's in-flight snapshots.
type AgentHandler struct {
	svc   *Service
	audit *audit.Recorder
}

// NewAgentHandler builds the agent-facing backup callback handler.
func NewAgentHandler(svc *Service, rec *audit.Recorder) *AgentHandler {
	return &AgentHandler{svc: svc, audit: rec}
}

// Register mounts the agent callbacks on the given group (already wrapped with
// the agent Authenticator middleware).
func (h *AgentHandler) Register(r *gin.RouterGroup) {
	r.POST("/backups/:snapshotId/presign", h.presign)
	r.POST("/backups/:snapshotId/manifest", h.manifest)
	// M5.6 / ADR-032: the phpbu agent runner POSTs phase progress here on every
	// stage transition + per-chunk during the custom PresignedS3 Sync.
	r.POST("/backups/:snapshotId/progress", h.progress)
}

// presign returns presigned PUT URLs for the candidate ciphertext chunk hashes
// that are NOT already stored for the tenant (incremental dedup). The s3 keys
// are content-addressed and tenant-namespaced, so a presign can never target
// another tenant's chunk prefix.
func (h *AgentHandler) presign(c *gin.Context) {
	id, ok := agent.IdentityFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("agent_unauthenticated", "agent identity required"))
		return
	}
	snapshotID, ok := uuidParam(c, "snapshotId", "invalid_snapshot_id")
	if !ok {
		return
	}
	var req agentcmd.PresignChunksRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
		return
	}
	if err := h.assertSnapshotSite(c, id.TenantID, snapshotID, id.SiteID); err != nil {
		httpx.Error(c, err)
		return
	}
	uploads, err := h.svc.PresignChunks(c.Request.Context(), id.TenantID, snapshotID, req.Hashes)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, agentcmd.PresignChunksResponse{
		Uploads:    uploads,
		TTLSeconds: h.svc.presignTTLSeconds(),
	})
}

// manifest records the agent-submitted manifest: it upserts not-yet-stored
// chunks, increments refcounts for every reference, inserts manifest entries,
// and completes the snapshot.
func (h *AgentHandler) manifest(c *gin.Context) {
	id, ok := agent.IdentityFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("agent_unauthenticated", "agent identity required"))
		return
	}
	snapshotID, ok := uuidParam(c, "snapshotId", "invalid_snapshot_id")
	if !ok {
		return
	}
	var req agentcmd.SubmitManifestRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
		return
	}
	if err := h.assertSnapshotSite(c, id.TenantID, snapshotID, id.SiteID); err != nil {
		httpx.Error(c, err)
		return
	}
	chunkRefs, stored, err := h.svc.SubmitManifest(c.Request.Context(), id.TenantID, snapshotID, req)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	h.recordCompleted(c, id.TenantID, snapshotID, id.SiteID, chunkRefs, stored)
	c.JSON(http.StatusOK, agentcmd.SubmitManifestResponse{
		OK:          true,
		ChunkCount:  chunkRefs,
		StoredCount: stored,
	})
}

// progressDTO is the agent's progress POST shape. snapshot_id comes from the
// URL path, NEVER from the body — a compromised agent must not be able to
// target another snapshot by spoofing it in the JSON body.
type progressDTO struct {
	Phase       string         `json:"phase"`
	PhaseDetail map[string]any `json:"phase_detail"`
}

// progress records the agent runner's latest phase. The Ed25519 identity on the
// context proves the request comes from the snapshot's own site (re-asserted
// below via assertSnapshotSite). Failures to record progress MUST be visible
// to the agent (the runner uses the response status to decide whether to retry),
// but a 4xx for an unknown phase or oversized body is terminal — the runner
// drops the event and moves on rather than spinning.
func (h *AgentHandler) progress(c *gin.Context) {
	id, ok := agent.IdentityFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("agent_unauthenticated", "agent identity required"))
		return
	}
	snapshotID, ok := uuidParam(c, "snapshotId", "invalid_snapshot_id")
	if !ok {
		return
	}
	// Read the body with a hard cap BEFORE JSON decoding. ShouldBindJSON would
	// happily allocate however much memory the agent sends; the size cap belongs
	// at the transport boundary.
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, MaxProgressPayloadBytes+1024)
	var dto progressDTO
	if err := c.ShouldBindJSON(&dto); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON or exceeds size cap"))
		return
	}
	if err := h.assertSnapshotSite(c, id.TenantID, snapshotID, id.SiteID); err != nil {
		httpx.Error(c, err)
		return
	}
	if _, err := h.svc.RecordProgress(c.Request.Context(), id.TenantID, snapshotID, dto.Phase, dto.PhaseDetail); err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// assertSnapshotSite verifies the snapshot exists in the agent's tenant AND
// belongs to the agent's own site, so a compromised agent cannot manipulate
// another site's snapshot even within its tenant.
func (h *AgentHandler) assertSnapshotSite(c *gin.Context, tenantID, snapshotID, siteID uuid.UUID) error {
	snap, err := h.svc.repo.GetSnapshot(c.Request.Context(), tenantID, snapshotID)
	if err != nil {
		return err
	}
	if snap.SiteID != siteID {
		return domain.Forbidden("snapshot_site_mismatch", "the snapshot does not belong to this site")
	}
	return nil
}

func (h *AgentHandler) recordCompleted(c *gin.Context, tenantID, snapshotID, siteID uuid.UUID, chunkRefs, stored int64) {
	if h.audit == nil {
		return
	}
	_, _ = h.audit.Record(c.Request.Context(), audit.Event{
		TenantID:   tenantID,
		ActorType:  audit.ActorSystem,
		Action:     ActionBackupCompleted,
		TargetType: "backup_snapshot",
		TargetID:   snapshotID.String(),
		Metadata: map[string]any{
			"site_id":      siteID.String(),
			"chunk_refs":   chunkRefs,
			"stored_count": stored,
		},
	})
}
