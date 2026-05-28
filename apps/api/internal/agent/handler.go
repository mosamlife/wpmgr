package agent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/api/gen"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// maxMetadataBytes bounds the agent metadata body (untrusted input).
const maxMetadataBytes = 16 << 20 // 16 MiB

// metadataDTO is a TOLERANT decode target for agent metadata. Agent telemetry
// comes from arbitrary real WordPress sites, so we deliberately do NOT use the
// strict OpenAPI-generated decoder (which requires SiteComponent.slug and is
// type-strict) — a single quirky plugin/theme must not 422 the whole sync. All
// fields are optional; unknown fields are ignored; the service layer sanitizes
// (truncates/drops) before persisting.
type metadataDTO struct {
	WPVersion   string         `json:"wp_version"`
	PHPVersion  string         `json:"php_version"`
	ServerInfo  string         `json:"server_info"`
	Multisite   bool           `json:"multisite"`
	ActiveTheme string         `json:"active_theme"`
	Plugins     []componentDTO `json:"plugins"`
	Themes      []componentDTO `json:"themes"`
}

type componentDTO struct {
	Slug    string `json:"slug"`
	Name    string `json:"name"`
	Version string `json:"version"`
	Active  bool   `json:"active"`
}

func (d metadataDTO) toMetadata() Metadata {
	conv := func(cs []componentDTO) []Component {
		out := make([]Component, 0, len(cs))
		for _, c := range cs {
			out = append(out, Component{Slug: c.Slug, Name: c.Name, Version: c.Version, Active: c.Active})
		}
		return out
	}
	return Metadata{
		WPVersion:   d.WPVersion,
		PHPVersion:  d.PHPVersion,
		ServerInfo:  d.ServerInfo,
		Multisite:   d.Multisite,
		ActiveTheme: d.ActiveTheme,
		Plugins:     conv(d.Plugins),
		Themes:      conv(d.Themes),
	}
}

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
	body, rerr := io.ReadAll(io.LimitReader(c.Request.Body, maxMetadataBytes))
	if rerr != nil {
		httpx.Error(c, domain.Validation("invalid_body", "could not read request body"))
		return
	}
	// Tolerant decode (see metadataDTO). Only genuinely malformed JSON is rejected.
	var dto metadataDTO
	if err := json.Unmarshal(body, &dto); err != nil {
		slog.WarnContext(c.Request.Context(), "agent metadata: malformed JSON body",
			slog.String("site_id", id.SiteID.String()),
			slog.Int("bytes", len(body)),
			slog.String("error", err.Error()))
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON: "+err.Error()))
		return
	}
	m := dto.toMetadata()
	slog.InfoContext(c.Request.Context(), "agent metadata received",
		slog.String("site_id", id.SiteID.String()),
		slog.Int("plugins", len(m.Plugins)), slog.Int("themes", len(m.Themes)),
		slog.String("active_theme", m.ActiveTheme))
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
