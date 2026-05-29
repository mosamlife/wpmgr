package agent

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/mosamlife/wpmgr/apps/api/internal/activity"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// maxActivityBatchBytes bounds the agent-pushed activity batch. The agent ships
// bounded batches of newest-above-cursor events; at <2 KiB per event a batch
// stays well under 1 MiB. We cap at 4 MiB for headroom on large meta blobs.
const maxActivityBatchBytes = 4 << 20

// ActivityHandler serves POST /agent/v1/activity — the agent's hash-chained
// WordPress activity-log push. Authenticated via the standard agent Ed25519
// signed-request middleware (tenant + site come from the verified identity on
// the context, NEVER a client header). The CP re-verifies the hash chain at
// ingest and flags any tamper as chain_valid=false on the affected row.
type ActivityHandler struct {
	svc *activity.Service
}

// NewActivityHandler wires the handler against the activity service.
func NewActivityHandler(svc *activity.Service) *ActivityHandler {
	return &ActivityHandler{svc: svc}
}

// Register mounts the route on the agent-authenticated group.
func (h *ActivityHandler) Register(r *gin.RouterGroup) {
	r.POST("/activity", h.push)
}

func (h *ActivityHandler) push(c *gin.Context) {
	id, ok := IdentityFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("agent_unauthenticated", "agent identity required"))
		return
	}
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxActivityBatchBytes))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "could not read request body"))
		return
	}
	var req activity.IngestRequest
	if uerr := json.Unmarshal(body, &req); uerr != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON: "+uerr.Error()))
		return
	}
	if h.svc == nil {
		httpx.Error(c, domain.Unavailable("activity_unwired", "activity service not wired"))
		return
	}
	ingested, breaks, ierr := h.svc.IngestActivity(c.Request.Context(), id.TenantID, id.SiteID, req)
	if ierr != nil {
		httpx.Error(c, ierr)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"ingested":     ingested,
		"chain_breaks": breaks,
	})
}
