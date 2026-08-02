package admin

// agent_mirror_handler.go: HTTP handler for the manual agent-mirror check
// (GH #322). See agent_mirror.go for the service and the full design
// rationale.

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// agentMirrorAdminHandler groups the agent-mirror admin sub-handler
// dependency. Nil until SetAgentMirror is called, mirroring vulnFeedH's
// optional-wiring pattern exactly.
type agentMirrorAdminHandler struct {
	svc *AgentMirrorCheckService
}

// SetAgentMirror wires the agent-mirror manual-check sub-handler into the
// admin Handler. Must be called before the first Register call (i.e. at
// boot), so Register can conditionally mount the route, mirroring
// SetVulnFeed exactly.
func (h *Handler) SetAgentMirror(svc *AgentMirrorCheckService) {
	h.agentMirrorH = &agentMirrorAdminHandler{svc: svc}
}

// agentMirrorCheckQueuedDTO is the 202 response body. status is always
// "queued", never "checked": the run has NOT executed yet, and may still end
// rate limited, refused, or unavailable; the real outcome appears as
// agent_mirror.last_attempt_outcome on GET /api/v1/fleet/agents.
type agentMirrorCheckQueuedDTO struct {
	Status   string `json:"status"`
	QueuedAt string `json:"queued_at"`
	Message  string `json:"message"`
}

func (h *Handler) agentMirrorCheck(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	am := h.agentMirrorH
	if am == nil || am.svc == nil {
		httpx.Error(c, domain.ServiceUnavailable("agent_mirror_not_wired", "upstream agent-release mirror management is not configured"))
		return
	}

	res, err := am.svc.TriggerCheck(c.Request.Context())
	if err != nil {
		// Rate-limit responses carry a Retry-After header AND the structured
		// retry_after_seconds field the domain error already attaches as
		// "details" (see httpx.Error), mirroring the autologin mint handler's
		// precedent for KindRateLimited.
		if de, ok := domain.AsDomain(err); ok && de.Kind == domain.KindRateLimited {
			if ra, ok := de.Details["retry_after_seconds"].(int); ok {
				c.Header("Retry-After", strconv.Itoa(ra))
			}
		}
		httpx.Error(c, err)
		return
	}

	if h.auditRec != nil {
		_, _ = h.auditRec.Record(c.Request.Context(), audit.Event{
			ActorType:  audit.ActorUser,
			ActorID:    p.UserID.String(),
			Action:     "admin.agent_mirror.check",
			TargetType: "instance",
			TargetID:   "agent_mirror",
		})
	}

	c.JSON(http.StatusAccepted, agentMirrorCheckQueuedDTO{
		Status:   "queued",
		QueuedAt: res.QueuedAt.UTC().Format(time.RFC3339),
		Message:  "A mirror run has been queued. It has not run yet; refresh the fleet agent view for the result.",
	})
}
