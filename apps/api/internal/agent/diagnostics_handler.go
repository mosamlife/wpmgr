package agent

import (
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/mosamlife/wpmgr/apps/api/internal/diagnostics"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// maxDiagnosticsBytes bounds the agent-pushed 14-category payload. The 14
// blobs together are usually <50 KiB on a typical site; we cap at 4 MiB to
// give plenty of headroom for unusually large plugin licensing arrays.
const maxDiagnosticsBytes = 4 << 20

// DiagnosticsHandler serves POST /agent/v1/diagnostics — the agent's daily
// push of the 14-category blob. Authenticated via the standard agent
// Ed25519 signed-request middleware (tenant + site come from the verified
// identity on the context, NEVER a client header).
type DiagnosticsHandler struct {
	svc *diagnostics.Service
}

// NewDiagnosticsHandler wires the handler against the diagnostics service.
func NewDiagnosticsHandler(svc *diagnostics.Service) *DiagnosticsHandler {
	return &DiagnosticsHandler{svc: svc}
}

// Register mounts the route on the agent-authenticated group.
func (h *DiagnosticsHandler) Register(r *gin.RouterGroup) {
	r.POST("/diagnostics", h.push)
}

func (h *DiagnosticsHandler) push(c *gin.Context) {
	id, ok := IdentityFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("agent_unauthenticated", "agent identity required"))
		return
	}
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxDiagnosticsBytes))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "could not read request body"))
		return
	}
	if h.svc == nil {
		httpx.Error(c, domain.Unavailable("diagnostics_unwired", "diagnostics service not wired"))
		return
	}
	count, ierr := h.svc.IngestDiagnostics(c.Request.Context(), id.TenantID, id.SiteID, body)
	if ierr != nil {
		httpx.Error(c, ierr)
		return
	}
	c.JSON(http.StatusOK, gin.H{"categories_ingested": count})
}
