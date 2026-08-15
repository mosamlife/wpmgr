package site

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// GH #414 phase 1 — the two monitoring-pause routes:
//
//	POST /api/v1/sites/monitoring/pause    {site_ids, reason?, resume_at?}
//	POST /api/v1/sites/monitoring/resume   {site_ids}
//
// BULK-SHAPED FROM THE START, deliberately. The UI entry point is a bulk menu
// action over a multi-select; a single-site route would have to be written
// twice and the two would drift. A one-site pause is a one-element site_ids.
//
// PERMISSION: PermSiteWrite, the permission every mutating site route in this
// package already declares (delete, tags, recheck, the M21 lifecycle
// mutations). No new permission is minted for this: pause is an ordinary
// operator-tier site mutation, and inventing a "site:manage" alongside
// "site:write" would give two names to one tier and leave every existing role
// mapping to update.
//
// NO RequireOrgScope, and the site gate is PER-SITE INSIDE THE HANDLER rather
// than authz.RequireSiteAccess middleware — these routes carry no :siteId for
// the middleware to read. This mirrors sitetag's /tags/bulk-apply and perf's
// bulk-purge/bulk-config: a site-scoped collaborator gets ok:false on the
// sites outside their grant and still gets their own sites paused, instead of
// a global 403 that reveals nothing about which id was the problem.

// pauseMonitoringBody is the POST /sites/monitoring/pause request body.
type pauseMonitoringBody struct {
	SiteIDs []string `json:"site_ids"`
	Reason  string   `json:"reason"`
	// ResumeAt is RFC3339. Optional; must be in the future when present.
	ResumeAt *time.Time `json:"resume_at"`
}

// resumeMonitoringBody is the POST /sites/monitoring/resume request body.
type resumeMonitoringBody struct {
	SiteIDs []string `json:"site_ids"`
}

// monitoringResultDTO is one site's outcome. `ok` says the site was accepted
// and is now in the requested state; `changed` says THIS request moved it, so
// a caller can tell a real pause from an accepted retry.
type monitoringResultDTO struct {
	SiteID  string `json:"site_id"`
	OK      bool   `json:"ok"`
	Changed bool   `json:"changed"`
	// Detail is a stable machine-readable reason, not prose: "paused",
	// "already_paused", "resumed", "already_active", "forbidden",
	// "invalid_site_id", "site_not_found".
	Detail string `json:"detail"`
	// MonitoringPausedAt/Reason/ResumeAt echo the state AFTER the request, so
	// a retry that changed nothing still tells the caller what is stored.
	MonitoringPausedAt     *time.Time `json:"monitoring_paused_at,omitempty"`
	MonitoringPausedReason string     `json:"monitoring_paused_reason,omitempty"`
	MonitoringResumeAt     *time.Time `json:"monitoring_resume_at,omitempty"`
}

type monitoringBulkResponse struct {
	Results      []monitoringResultDTO `json:"results"`
	ChangedCount int                   `json:"changed_count"`
}

// RegisterMonitoring mounts the pause/resume routes. Called from Register.
//
// Both paths sit under /sites/monitoring/... — a literal segment, so Gin's
// router never confuses them with /sites/:siteId. They are declared here
// alongside the other tenant-wide collection routes for that reason.
func (h *Handler) RegisterMonitoring(r *gin.RouterGroup) {
	r.POST("/sites/monitoring/pause", authz.RequirePermission(authz.PermSiteWrite), h.pauseMonitoring)
	r.POST("/sites/monitoring/resume", authz.RequirePermission(authz.PermSiteWrite), h.resumeMonitoring)
}

// PARTIAL FAILURE: PER-SITE, WITH A REPORT — NOT ALL-OR-NOTHING.
//
// When 5 of 10 ids are valid, the 5 are paused and the response names all 10
// with a per-site ok/changed/detail. The alternative, failing the whole
// request because one id was stale, was rejected for three reasons:
//
//  1. The caller is a multi-select in a fleet dashboard. A site deleted or
//     un-shared in another tab between the page load and the click is normal,
//     not exceptional, and it would make the button unusable on large
//     selections — one stale row and nothing pauses.
//  2. Pause is idempotent, so per-site is safe to retry. The caller re-sends
//     only the ids that came back ok:false; the ones that succeeded are
//     untouched by the retry. All-or-nothing buys atomicity the caller cannot
//     use, because there is no state where "half paused" is corrupt — each
//     row is independent.
//  3. It is the shape every other bulk route in this tree already returns
//     (sitetag bulk-apply, perf bulk-purge), so clients have one contract.
//
// The write itself IS one transaction per request: every named site moves in
// a single UPDATE inside one InTenantTxAsUser. Per-site refers to the
// authorization filter and the reporting, not to a row-by-row commit — a
// database error still rolls the whole statement back and returns 5xx.
//
// The response always lets the caller tell exactly which sites changed:
// results[].changed is true only for the rows this request moved, and
// changed_count is their number.
func (h *Handler) pauseMonitoring(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}
	var body pauseMonitoringBody
	if err := c.ShouldBindJSON(&body); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
		return
	}
	if len(body.SiteIDs) == 0 {
		httpx.Error(c, domain.Validation("site_ids_required", "site_ids must not be empty"))
		return
	}
	rejected, authorized := partitionSiteIDs(p, body.SiteIDs)

	// Validate the pause itself BEFORE the authorization filter has a chance to
	// empty the list: a resume_at in the past must be a 422 whether or not the
	// caller also happened to name sites they cannot touch, otherwise the same
	// bad request 422s for one caller and 200s for another.
	states, err := h.svc.PauseMonitoring(c.Request.Context(), PauseMonitoringInput{
		TenantID: p.TenantID,
		// From the PRINCIPAL, never from the body. The FK is to users(id)
		// alone; Postgres cannot check that the referenced user belongs to
		// this site's tenant, so accepting an id from input would let a caller
		// attribute their pause to a stranger.
		ActorUserID: p.UserID,
		SiteIDs:     authorized,
		Reason:      body.Reason,
		ResumeAt:    body.ResumeAt,
	})
	if err != nil {
		httpx.Error(c, err)
		return
	}
	h.respondMonitoring(c, p.TenantID, rejected, authorized, states, true)
}

// resumeMonitoring is POST /sites/monitoring/resume. Same partial-failure
// contract as pause (see the doc comment above). Resuming a site that is
// already active is a success with changed=false, not a 409: the caller asked
// for a state, and the state holds.
func (h *Handler) resumeMonitoring(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}
	var body resumeMonitoringBody
	if err := c.ShouldBindJSON(&body); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
		return
	}
	if len(body.SiteIDs) == 0 {
		httpx.Error(c, domain.Validation("site_ids_required", "site_ids must not be empty"))
		return
	}
	rejected, authorized := partitionSiteIDs(p, body.SiteIDs)

	states, err := h.svc.ResumeMonitoring(c.Request.Context(), ResumeMonitoringInput{
		TenantID:    p.TenantID,
		ActorUserID: p.UserID,
		SiteIDs:     authorized,
	})
	if err != nil {
		httpx.Error(c, err)
		return
	}
	h.respondMonitoring(c, p.TenantID, rejected, authorized, states, false)
}

// partitionSiteIDs splits the raw body ids into the per-site rejections
// (unparseable, or outside a site-scoped collaborator's grant) and the ids the
// service may act on. Duplicates collapse to one result so a repeated id is
// never counted or audited twice.
//
// A site id belonging to ANOTHER TENANT survives this filter — CanAccessSite
// only knows the caller's share list, not tenancy. It is stopped at the
// database instead: the UPDATE carries an explicit tenant_id and runs under
// the app.tenant_id RLS policy, so a foreign row matches nothing, comes back
// in neither RETURNING nor the results as ok, and is reported site_not_found.
// Two independent gates, and the authoritative one is the one the request
// cannot talk its way around.
func partitionSiteIDs(p domain.Principal, raw []string) (rejected []monitoringResultDTO, authorized []uuid.UUID) {
	rejected = make([]monitoringResultDTO, 0, len(raw))
	authorized = make([]uuid.UUID, 0, len(raw))
	seen := make(map[uuid.UUID]bool, len(raw))
	seenInvalid := make(map[string]bool, len(raw))
	for _, s := range raw {
		id, err := uuid.Parse(s)
		if err != nil {
			if seenInvalid[s] {
				continue
			}
			seenInvalid[s] = true
			rejected = append(rejected, monitoringResultDTO{SiteID: s, Detail: "invalid_site_id"})
			continue
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		if !p.CanAccessSite(id) {
			rejected = append(rejected, monitoringResultDTO{SiteID: id.String(), Detail: "forbidden"})
			continue
		}
		authorized = append(authorized, id)
	}
	return rejected, authorized
}

// respondMonitoring builds the per-site report and writes ONE AUDIT EVENT PER
// SITE.
//
// Per-site, not per-request: someone auditing a single site filters the audit
// log by target_id and must find the pause. A single request-level event with
// a site_ids array in its metadata is invisible to that query, which is how a
// bulk action becomes untraceable for the one site anybody actually asks
// about. The cost is N rows for an N-site bulk, which is the same order as the
// N rows the request just wrote.
//
// Only CHANGED sites are audited. An accepted retry that moved nothing is not
// an event: auditing it would fill the log with duplicates that misreport a
// timeout retry as a second operator action.
func (h *Handler) respondMonitoring(c *gin.Context, tenantID uuid.UUID, rejected []monitoringResultDTO, authorized []uuid.UUID, states []MonitoringState, pausing bool) {
	byID := make(map[uuid.UUID]MonitoringState, len(states))
	for _, st := range states {
		byID[st.SiteID] = st
	}
	results := make([]monitoringResultDTO, 0, len(rejected)+len(authorized))
	results = append(results, rejected...)
	changed := 0

	for _, id := range authorized {
		st, found := byID[id]
		if !found {
			// No row came back: the site does not exist, or it belongs to
			// another tenant and RLS + the explicit tenant_id excluded it.
			// The same answer for both on purpose — telling the caller which
			// is which turns this route into a cross-tenant existence oracle.
			results = append(results, monitoringResultDTO{SiteID: id.String(), Detail: "site_not_found"})
			continue
		}
		res := monitoringResultDTO{
			SiteID:                 id.String(),
			OK:                     true,
			Changed:                st.Changed,
			MonitoringPausedAt:     st.PausedAt,
			MonitoringPausedReason: st.PausedReason,
			MonitoringResumeAt:     st.ResumeAt,
		}
		switch {
		case pausing && st.Changed:
			res.Detail = "paused"
		case pausing:
			res.Detail = "already_paused"
		case st.Changed:
			res.Detail = "resumed"
		default:
			res.Detail = "already_active"
		}
		if st.Changed {
			changed++
			h.recordMonitoringEvent(c, tenantID, id, st, pausing)
		}
		results = append(results, res)
	}
	c.JSON(http.StatusOK, monitoringBulkResponse{Results: results, ChangedCount: changed})
}

func (h *Handler) recordMonitoringEvent(c *gin.Context, tenantID, siteID uuid.UUID, st MonitoringState, pausing bool) {
	action := audit.ActionSiteMonitoringResumed
	meta := map[string]any{"bulk": true}
	if pausing {
		action = audit.ActionSiteMonitoringPaused
		if st.PausedReason != "" {
			meta["reason"] = st.PausedReason
		}
		if st.ResumeAt != nil {
			meta["resume_at"] = st.ResumeAt.UTC().Format(time.RFC3339)
		}
	}
	// target_type "site" + target_id the site id: the shape h.record already
	// uses for every other site event, so the audit-log site filter finds it.
	h.record(c, tenantID, action, siteID.String(), meta)
}
