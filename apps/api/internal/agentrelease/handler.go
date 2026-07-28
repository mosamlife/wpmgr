package agentrelease

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// unknownVersion is the wire sentinel for "no reliable published version is
// known right now" (manifest never published, object storage unconfigured,
// or a transient read failure). Never surfaced as an error response.
const unknownVersion = "unknown"

// Handler serves the read-only agent-release visibility routes:
//
//	GET /agent/latest: the currently published agent version
//	GET /fleet/agents: tenant-scoped per-site agent-version rollup
type Handler struct {
	svc *Service
}

// NewHandler builds the Handler. svc may be nil (see the routes' nil guards
// below); Register still mounts the routes so a misconfiguration is a clean
// 503, not a 404 (mirrors internal/vuln.Handler's enq-nil pattern).
func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// Register mounts the routes on the authenticated /api/v1 group.
func (h *Handler) Register(r *gin.RouterGroup) {
	r.GET("/agent/latest", authz.RequirePermission(authz.PermSiteRead), h.latest)
	// Fleet rollup: cross-site, tenant-scoped. RequireOrgScope mirrors the
	// vulnerability-scanner fleet rollup (internal/vuln.Handler.Register):
	// a site-scoped collaborator has no cross-site rollup, only its own
	// sites' data via the ordinary per-tenant sites list.
	r.GET("/fleet/agents",
		authz.RequireOrgScope(),
		authz.RequirePermission(authz.PermSiteRead),
		h.fleetAgents,
	)
}

// ---------------------------------------------------------------------------
// DTOs
// ---------------------------------------------------------------------------

// The GET /api/v1/agent/latest response shape:
//
//	{"version": "0.61.95"}
//
// version is "unknown" when the published release manifest cannot currently
// be read.
type agentLatestResponseDTO struct {
	Version string `json:"version"`
}

// The GET /api/v1/fleet/agents response shape:
//
//	{
//	  "latest_version": "0.61.95",
//	  "counts": {"current": 10, "outdated": 3, "unknown": 1, "ineligible": 0},
//	  "sites": [
//	    {"site_id": "...", "site_name": "...", "agent_version": "0.61.90", "status": "outdated"},
//	    ...
//	  ]
//	}
//
// agent_version is the site's raw last-reported value verbatim (empty when
// the agent has never reported one), distinct from status="unknown", which
// also covers a malformed version string on either side of the comparison.
type fleetAgentsCountsDTO struct {
	Current    int `json:"current"`
	Outdated   int `json:"outdated"`
	Unknown    int `json:"unknown"`
	Ineligible int `json:"ineligible"`
}

type fleetAgentSiteDTO struct {
	SiteID       string `json:"site_id"`
	SiteName     string `json:"site_name"`
	AgentVersion string `json:"agent_version"`
	Status       string `json:"status"`
}

type fleetAgentsResponseDTO struct {
	LatestVersion string               `json:"latest_version"`
	Counts        fleetAgentsCountsDTO `json:"counts"`
	Sites         []fleetAgentSiteDTO  `json:"sites"`
}

// ---------------------------------------------------------------------------
// Route handlers
// ---------------------------------------------------------------------------

func (h *Handler) latest(c *gin.Context) {
	version := unknownVersion
	if h.svc != nil {
		if v := h.svc.LatestVersion(c.Request.Context()); v != "" {
			version = v
		}
	}
	c.JSON(http.StatusOK, agentLatestResponseDTO{Version: version})
}

func (h *Handler) fleetAgents(c *gin.Context) {
	if h.svc == nil {
		httpx.Error(c, domain.ServiceUnavailable("agent_fleet_not_wired", "agent fleet rollup is not available"))
		return
	}
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	summary, err := h.svc.FleetRollup(c.Request.Context(), p.TenantID)
	if err != nil {
		httpx.Error(c, domain.Internal("fleet_agents_failed", "failed to build agent fleet rollup").WithCause(err))
		return
	}

	latest := summary.LatestVersion
	if latest == "" {
		latest = unknownVersion
	}

	sites := make([]fleetAgentSiteDTO, 0, len(summary.Sites))
	for _, row := range summary.Sites {
		sites = append(sites, fleetAgentSiteDTO{
			SiteID:       row.SiteID.String(),
			SiteName:     row.SiteName,
			AgentVersion: row.AgentVersion,
			Status:       string(row.Status),
		})
	}

	c.JSON(http.StatusOK, fleetAgentsResponseDTO{
		LatestVersion: latest,
		Counts: fleetAgentsCountsDTO{
			Current:    summary.Counts.Current,
			Outdated:   summary.Counts.Outdated,
			Unknown:    summary.Counts.Unknown,
			Ineligible: summary.Counts.Ineligible,
		},
		Sites: sites,
	})
}
