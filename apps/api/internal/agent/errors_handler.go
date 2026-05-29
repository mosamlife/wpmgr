package agent

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/mosamlife/wpmgr/apps/api/internal/diagnostics"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// maxErrorBatchBytes bounds the agent-pushed error batch. With SHIP_BATCH=50
// at <2 KiB per row this is comfortably under 1 MiB; we cap at 2 MiB.
const maxErrorBatchBytes = 2 << 20

// ErrorsHandler serves POST /agent/v1/errors — the agent's heartbeat-driven
// batch of fingerprint-deduped PHP errors. The response carries the highest
// agent-row id we processed so the agent can advance its local ship cursor.
type ErrorsHandler struct {
	svc *diagnostics.Service
}

// NewErrorsHandler wires the handler.
func NewErrorsHandler(svc *diagnostics.Service) *ErrorsHandler {
	return &ErrorsHandler{svc: svc}
}

// Register mounts the route on the agent-authenticated group.
func (h *ErrorsHandler) Register(r *gin.RouterGroup) {
	r.POST("/errors", h.push)
}

func (h *ErrorsHandler) push(c *gin.Context) {
	id, ok := IdentityFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("agent_unauthenticated", "agent identity required"))
		return
	}
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxErrorBatchBytes))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "could not read request body"))
		return
	}
	var batch diagnostics.ErrorBatch
	if err := json.Unmarshal(body, &batch); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON: "+err.Error()))
		return
	}
	if h.svc == nil {
		httpx.Error(c, domain.Unavailable("diagnostics_unwired", "diagnostics service not wired"))
		return
	}
	highest, ierr := h.svc.IngestErrorBatch(c.Request.Context(), id.TenantID, id.SiteID, batch)
	if ierr != nil {
		httpx.Error(c, ierr)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"consumed_count": len(batch.Errors),
		"highest_id":     highest,
	})
}
