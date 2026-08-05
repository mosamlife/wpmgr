package agentrelease

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mosamlife/wpmgr/apps/api/internal/admingate"
	"github.com/mosamlife/wpmgr/apps/api/internal/agentmirror"
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

	// mirrorReader/mirrorEnabled back the agent_mirror field on the fleet
	// rollup (GH #322): the freshness of the upstream agent-release mirror
	// (internal/agentupstream), wired via SetMirror. Both are zero-valued
	// (nil / false) until SetMirror is called, which degrades exactly like
	// disabled mirroring would: status "disabled", every timestamp null.
	mirrorReader  MirrorStateReader
	mirrorEnabled bool

	// mirrorCheckGate answers agent_mirror.can_check_now (GH #322), wired via
	// SetMirrorCheckGate. It is the SAME store, read by the SAME function,
	// that gates POST /api/v1/admin/agent-mirror/check itself
	// (admingate.CanRunAgentMirrorCheck; internal/admin/gate.go mounts it as
	// middleware). Nothing about the permission is recomputed here, so the
	// button this field reveals can never be offered to a caller the route
	// would refuse, nor hidden from one it would admit. Nil until wired,
	// which reports false, the safe direction for a capability flag.
	mirrorCheckGate admingate.Store
}

// MirrorStateReader reads the persisted upstream-mirror freshness sentinel
// (GH #322). Satisfied by *agentmirror.Repo.
type MirrorStateReader interface {
	Load(ctx context.Context) (agentmirror.State, error)
}

var _ MirrorStateReader = (*agentmirror.Repo)(nil)

// NewHandler builds the Handler. svc may be nil (see the routes' nil guards
// below); Register still mounts the routes so a misconfiguration is a clean
// 503, not a 404 (mirrors internal/vuln.Handler's enq-nil pattern).
// selfUpdateEnabled must be the same config value the update worker gates
// dispatch on.
func NewHandler(svc *Service, selfUpdateEnabled bool) *Handler {
	return &Handler{svc: svc, selfUpdateEnabled: selfUpdateEnabled}
}

// SetMirror wires the upstream agent-release mirror's freshness state into
// the fleet rollup response (GH #322). Called once at boot, after
// internal/agentmirror.Repo is built. reader may be nil (mirror persistence
// not wired); enabled MUST be the SAME cfg.Update.AgentMirrorEnabled value
// agentupstream.NewMirrorWorker was given, so the two agree on whether a
// mirror job exists on this install at all, see agent_mirror's field doc on
// fleetAgentsResponseDTO.
func (h *Handler) SetMirror(reader MirrorStateReader, enabled bool) {
	h.mirrorReader = reader
	h.mirrorEnabled = enabled
}

// SetMirrorCheckGate wires the permission behind agent_mirror.can_check_now
// (GH #322). Called once at boot with the SAME admingate.Store the admin
// handler's route gate uses, so the dashboard's button and the endpoint's 403
// are two readings of one decision rather than two decisions.
//
// Deliberately a separate setter from SetMirror: freshness and permission are
// independent facts with independent wiring, and an install that has one
// without the other must degrade rather than fail.
func (h *Handler) SetMirrorCheckGate(store admingate.Store) {
	h.mirrorCheckGate = store
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
	AgentMirror       agentMirrorDTO       `json:"agent_mirror"`
}

// agentMirrorDTO reports the freshness of the UPSTREAM AGENT-RELEASE MIRROR
// (internal/agentupstream), GH #322, and one capability: whether the CALLER
// may trigger a check on it right now (CanCheckNow). Everything else here
// describes the MIRROR JOB, never latest_version/reference_source directly:
// when reference_source is "fleet" or "none", or when enabled is false, these
// timestamps say nothing about latest_version and no freshness age should be
// presented to an operator as the age of the reference, see each field's own
// doc and Status's doc.
//
// Always emitted, never omitted, for the same reason self_update_enabled is:
// a caller must never have to treat "absent" and "off" as the same undefined
// thing. Every optional field is a real JSON null when unset, not omitted,
// deliberately NOT the legacy "unknown" string sentinel latest_version uses,
// because that sentinel is backwards compatibility on an existing field, and
// this is a new one.
type agentMirrorDTO struct {
	// Enabled mirrors cfg.Update.AgentMirrorEnabled. False on the hosted
	// service and by default: there, the release pipeline writes the
	// manifest directly, so there is no mirror run to time and this whole
	// object reports StatusDisabled with every timestamp null.
	Enabled bool `json:"enabled"`
	// Status is the single server-computed roll-up, see
	// agentmirror.State.Status's doc for the full derivation and ordering.
	Status string `json:"status"`
	// StaleAfterSeconds is the threshold behind status="stale"
	// (agentmirror.StalenessThreshold), emitted so nothing downstream has to
	// duplicate the constant.
	StaleAfterSeconds int `json:"stale_after_seconds"`

	// CanCheckNow says whether THE CALLER OF THIS REQUEST may trigger a mirror
	// check right now (GH #322). It is a property of the viewer, not of the
	// install, so it is not cacheable across users and two people looking at
	// the same fleet can legitimately get different values.
	//
	// False whenever Enabled is false: there is no run to trigger then, so no
	// caller may trigger one regardless of who they are.
	//
	// Otherwise it is exactly admingate.CanRunAgentMirrorCheck, the same call
	// that gates POST /api/v1/admin/agent-mirror/check. On a hosted,
	// multi-tenant install that means false for every non-superadmin, and a
	// superadmin never sees this response's dashboard anyway (the web app
	// redirects them off tenant pages), so the admin console stays their route.
	CanCheckNow bool `json:"can_check_now"`

	// LastSuccessAt/LastSuccessOutcome/LastSuccessVersion: the LAST time this
	// install CONFIRMED what upstream publishes. LastSuccessAt is the ONLY
	// field an operator-facing freshness AGE may ever be computed from.
	LastSuccessAt      *string `json:"last_success_at"`
	LastSuccessOutcome *string `json:"last_success_outcome"`
	LastSuccessVersion *string `json:"last_success_version"`

	// LastAttemptAt/LastAttemptOutcome/LastAttemptDetail/LastAttemptTrigger:
	// the LAST run that actually executed, whatever its result. Never render
	// an age from LastAttemptAt: a run that failed ten minutes ago must
	// never be reported as "checked ten minutes ago" (see the module doc).
	LastAttemptAt      *string `json:"last_attempt_at"`
	LastAttemptOutcome *string `json:"last_attempt_outcome"`
	LastAttemptDetail  *string `json:"last_attempt_detail"`
	LastAttemptTrigger *string `json:"last_attempt_trigger"`

	// LastMirroredAt/LastMirroredVersion: the last time a NEW release was
	// actually published into this install's storage, as distinct from
	// merely confirming the existing one.
	LastMirroredAt      *string `json:"last_mirrored_at"`
	LastMirroredVersion *string `json:"last_mirrored_version"`
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
		AgentMirror:       h.buildAgentMirrorDTO(c.Request.Context()),
	})
}

// buildAgentMirrorDTO reads the persisted mirror state and derives the wire
// DTO. This dashboard read must never fail the whole /fleet/agents response:
// a mirror-state read failure (or mirroring never having been wired at all)
// degrades to the zero agentmirror.State, which Status reports as "pending"
// once enabled, or "disabled" when not, never an error, matching this
// package's existing read-only, never-blocks contract (see reader.go).
func (h *Handler) buildAgentMirrorDTO(ctx context.Context) agentMirrorDTO {
	dto := agentMirrorDTO{
		Enabled:           h.mirrorEnabled,
		StaleAfterSeconds: int(agentmirror.StalenessThreshold / time.Second),
	}
	if !h.mirrorEnabled {
		// Returned with CanCheckNow still false, and WITHOUT consulting the
		// gate store: a check cannot be run at all when the mirror is off, so
		// there is nothing for a permission to permit, and the hosted service
		// (where mirroring is off by default) pays no extra query for this
		// field on any request.
		dto.Status = string(agentmirror.StatusDisabled)
		return dto
	}
	dto.CanCheckNow = admingate.CanRunAgentMirrorCheck(ctx, h.mirrorCheckGate)

	var st agentmirror.State
	if h.mirrorReader != nil {
		if loaded, err := h.mirrorReader.Load(ctx); err == nil {
			st = loaded
		}
		// A read failure leaves st at its zero value (never attempted),
		// which is the safe degraded answer, see the doc above.
	}

	now := time.Now()
	dto.Status = string(st.Status(h.mirrorEnabled, now))
	dto.LastSuccessAt = formatMirrorTime(st.LastSuccessAt)
	dto.LastSuccessOutcome = nonEmptyMirrorString(string(st.LastSuccessOutcome))
	dto.LastSuccessVersion = nonEmptyMirrorString(st.LastSuccessVersion)
	dto.LastAttemptAt = formatMirrorTime(st.LastAttemptAt)
	dto.LastAttemptOutcome = nonEmptyMirrorString(string(st.LastAttemptOutcome))
	dto.LastAttemptDetail = nonEmptyMirrorString(st.LastAttemptDetail)
	dto.LastAttemptTrigger = nonEmptyMirrorString(string(st.LastAttemptTrigger))
	dto.LastMirroredAt = formatMirrorTime(st.LastMirroredAt)
	dto.LastMirroredVersion = nonEmptyMirrorString(st.LastMirroredVersion)
	return dto
}

// formatMirrorTime renders a nullable timestamp as RFC3339 UTC, or nil (a
// real JSON null) when unset.
func formatMirrorTime(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}

// nonEmptyMirrorString returns nil (a real JSON null) for an empty string,
// and a pointer to s otherwise. Used for every agent_mirror string field so
// "never recorded" is a genuine null, not an empty string a caller has to
// treat specially.
func nonEmptyMirrorString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
