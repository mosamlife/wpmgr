package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/security"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// maxLoginEventBatchBytes bounds the agent-pushed login-event batch.
// With up to 50 events at <1 KiB per row this is well under 2 MiB.
const maxLoginEventBatchBytes = 2 << 20 // 2 MiB

// LoginEventsIngester is the subset of security.Service the agent handler
// needs. Declared as an interface so tests can substitute a fake.
type LoginEventsIngester interface {
	IngestLoginEvents(ctx context.Context, tenantID, siteID uuid.UUID, batch security.LoginEventBatch) (int64, error)
}

// SecurityLoginEventsHandler serves POST /agent/v1/security/login-events —
// the agent's heartbeat-driven batch of login events. The response carries
// the highest agent-row id we processed so the agent can advance its cursor.
type SecurityLoginEventsHandler struct {
	svc LoginEventsIngester
}

// NewSecurityLoginEventsHandler wires the handler.
func NewSecurityLoginEventsHandler(svc LoginEventsIngester) *SecurityLoginEventsHandler {
	return &SecurityLoginEventsHandler{svc: svc}
}

// Register mounts the route on the agent-authenticated group.
func (h *SecurityLoginEventsHandler) Register(r *gin.RouterGroup) {
	r.POST("/security/login-events", h.push)
}

func (h *SecurityLoginEventsHandler) push(c *gin.Context) {
	id, ok := IdentityFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("agent_unauthenticated", "agent identity required"))
		return
	}
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxLoginEventBatchBytes))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "could not read request body"))
		return
	}
	var batch security.LoginEventBatch
	if err := json.Unmarshal(body, &batch); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON: "+err.Error()))
		return
	}
	if h.svc == nil {
		httpx.Error(c, domain.Unavailable("security_unwired", "security service not wired"))
		return
	}
	highest, ierr := h.svc.IngestLoginEvents(c.Request.Context(), id.TenantID, id.SiteID, batch)
	if ierr != nil {
		httpx.Error(c, ierr)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"consumed_count": len(batch.LoginEvents),
		"highest_id":     highest,
	})
}
