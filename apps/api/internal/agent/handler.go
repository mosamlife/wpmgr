package agent

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/api/gen"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// Metadata mirrors the site domain's metadata input without importing it (the
// site package imports this package for the signature helpers, so this package
// must not import site — that would be a cycle).
type Metadata struct {
	WPVersion   string
	PHPVersion  string
	ServerInfo  string
	Multisite   bool
	ActiveTheme string
	Plugins     []Component
	Themes      []Component
}

// Component is one installed plugin/theme.
type Component struct {
	Slug    string
	Name    string
	Version string
	Active  bool
}

// MetadataSink applies agent-pushed metadata and heartbeats. Implemented by the
// site service (wired in main) so this package needs no site import. The
// metadata call returns the updated site in its OpenAPI form.
type MetadataSink interface {
	ApplyAgentMetadata(ctx context.Context, tenantID, siteID uuid.UUID, m Metadata) (gen.Site, error)
	Heartbeat(ctx context.Context, tenantID, siteID uuid.UUID) error
}

// Handler serves the agent-authenticated endpoints under /agent/v1. Every route
// runs behind the agent Authenticator; the site/tenant come from the verified
// identity on the context.
type Handler struct {
	sink MetadataSink
}

// NewHandler builds an agent Handler.
func NewHandler(sink MetadataSink) *Handler {
	return &Handler{sink: sink}
}

// Register mounts the agent routes on the given group (already wrapped with the
// agent Authenticator middleware).
func (h *Handler) Register(r *gin.RouterGroup) {
	r.POST("/metadata", h.metadata)
	r.POST("/heartbeat", h.heartbeat)
}

func (h *Handler) metadata(c *gin.Context) {
	id, ok := IdentityFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("agent_unauthenticated", "agent identity required"))
		return
	}
	var req gen.AgentMetadata
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body did not match the metadata schema: "+err.Error()))
		return
	}
	m := Metadata{
		WPVersion:   req.WpVersion.Or(""),
		PHPVersion:  req.PhpVersion.Or(""),
		ServerInfo:  req.ServerInfo.Or(""),
		Multisite:   req.Multisite.Or(false),
		ActiveTheme: req.ActiveTheme.Or(""),
		Plugins:     fromAPIComponents(req.Plugins),
		Themes:      fromAPIComponents(req.Themes),
	}
	out, err := h.sink.ApplyAgentMetadata(c.Request.Context(), id.TenantID, id.SiteID, m)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, &out)
}

func (h *Handler) heartbeat(c *gin.Context) {
	id, ok := IdentityFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("agent_unauthenticated", "agent identity required"))
		return
	}
	if err := h.sink.Heartbeat(c.Request.Context(), id.TenantID, id.SiteID); err != nil {
		httpx.Error(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func fromAPIComponents(cs []gen.SiteComponent) []Component {
	out := make([]Component, 0, len(cs))
	for _, c := range cs {
		out = append(out, Component{
			Slug:    c.Slug,
			Name:    c.Name.Or(""),
			Version: c.Version.Or(""),
			Active:  c.Active.Or(false),
		})
	}
	return out
}
