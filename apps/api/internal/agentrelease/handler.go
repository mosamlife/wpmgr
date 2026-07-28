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
	// selfUpdateEnabled mirrors cfg.Update.AgentSelfUpdateEnabled
	// (WPMGR_UPDATE_AGENT_SELF_UPDATE_ENABLED), the SAME fleet-wide kill
	// switch the update service and worker check before dispatching an agent
	// self-update. It is reported on the fleet rollup so the UI reveals the
	// "Update WPMgr agent" action exactly when the control plane would honour
	// it, instead of offering an operator a run that is refused pre-dispatch.
	// A boot-time snapshot is the whole truth here: the flag is process
	// configuration (env/koanf), never a per-request or database value.
	selfUpdateEnabled bool
}

// NewHandler builds the Handler. svc may be nil (see the routes' nil guards
// below); Register still mounts the routes so a misconfiguration is a clean
// 503, not a 404 (mirrors internal/vuln.Handler's enq-nil pattern).
// selfUpdateEnabled must be the same config value the update worker gates
// dispatch on.
func NewHandler(svc *Service, selfUpdateEnabled bool) *Handler {
	return &Handler{svc: svc, selfUpdateEnabled: selfUpdateEnabled}
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
//	  "reference_source": "published",
//	  "counts": {"current": 10, "outdated": 3, "unknown": 1, "ineligible": 0},
//	  "sites": [
//	    {"site_id": "...", "site_name": "...", "agent_version": "0.61.90", "status": "outdated"},
//	    ...
//	  ],
//	  "self_update_enabled": false
//	}
//
// latest_version is the version the counts were computed against, and
// reference_source says where it came from: "published" (the release channel
// manifest), "fleet" (the newest agent version in this tenant's own fleet,
// used when the manifest cannot be read), or "none" (neither was available,
// so every site is "unknown"). A caller must read the two together: under
// "fleet", latest_version is the newest agent SEEN HERE, not the newest that
// exists, and presenting it as the latter would overclaim.
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

// self_update_enabled is always emitted, never omitted: a caller must be able
// to read it as a settled false rather than have to treat "absent" and "off"
// as the same undefined thing.
type fleetAgentsResponseDTO struct {
	LatestVersion     string               `json:"latest_version"`
	ReferenceSource   string               `json:"reference_source"`
	Counts            fleetAgentsCountsDTO `json:"counts"`
	Sites             []fleetAgentSiteDTO  `json:"sites"`
	SelfUpdateEnabled bool                 `json:"self_update_enabled"`
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

	latest := summary.ReferenceVersion
	if latest == "" {
		latest = unknownVersion
	}
	// A zero ReferenceSource can only mean "nothing was available"; never emit
	// an empty discriminator the caller would have to interpret.
	source := summary.ReferenceSource
	if source == "" {
		source = ReferenceSourceNone
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
		LatestVersion:   latest,
		ReferenceSource: string(source),
		Counts: fleetAgentsCountsDTO{
			Current:    summary.Counts.Current,
			Outdated:   summary.Counts.Outdated,
			Unknown:    summary.Counts.Unknown,
			Ineligible: summary.Counts.Ineligible,
		},
		Sites:             sites,
		SelfUpdateEnabled: h.selfUpdateEnabled,
	})
}
