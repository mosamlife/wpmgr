package govcontext

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// Handler serves ADR-064 Decision 13's 13 routes: 6 organisation-scope routes
// under /api/v1/orgs/{orgId}/context..., and 7 site-scope routes under
// /api/v1/sites/{siteId}/context... (the 7th being the effective-context
// preview). None of these routes are reachable from an assistant-kind
// credential — Decision 6 — because context.org.write/context.site.write are
// never granted to one (see role.go) and because, as of this slice, no
// assistant-kind principal type or tool-registry dispatch exists anywhere in
// this codebase for such a grant to be absent FROM; that mechanism is
// ADR-061's, not yet built, and this handler adds nothing that reaches it.
type Handler struct {
	svc *Service
}

// NewHandler builds the Handler.
func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// Register mounts the organisation-scope routes on the caller's :orgId — which
// this handler verifies equals the caller's active tenant (org routes here
// deliberately do NOT support the cross-org membership lookup org.Handler's
// rename/delete/activate routes use; see handler.go's doc comment) — and the
// site-scope routes on the existing per-site group, gated by
// authz.RequireSiteAccess like every other per-site resource in this
// codebase.
func (h *Handler) Register(r *gin.RouterGroup) {
	r.GET("/orgs/:orgId/context", authz.RequirePermission(authz.PermOrgContextRead), h.getOrgContext)
	r.PATCH("/orgs/:orgId/context", authz.RequirePermission(authz.PermOrgContextWrite), h.patchOrgContext)
	r.GET("/orgs/:orgId/context/versions", authz.RequirePermission(authz.PermOrgContextRead), h.listOrgVersions)
	r.GET("/orgs/:orgId/context/versions/:versionId", authz.RequirePermission(authz.PermOrgContextRead), h.getOrgVersion)
	r.GET("/orgs/:orgId/context/versions/:versionId/diff", authz.RequirePermission(authz.PermOrgContextRead), h.diffOrgVersion)
	r.POST("/orgs/:orgId/context/versions/:versionId/restore", authz.RequirePermission(authz.PermOrgContextWrite), h.restoreOrgVersion)

	g := r.Group("/sites/:siteId", authz.RequireSiteAccess("siteId"))
	g.GET("/context", authz.RequirePermission(authz.PermSiteContextRead), h.getSiteContext)
	g.PATCH("/context", authz.RequirePermission(authz.PermSiteContextWrite), h.patchSiteContext)
	g.GET("/context/versions", authz.RequirePermission(authz.PermSiteContextRead), h.listSiteVersions)
	g.GET("/context/versions/:versionId", authz.RequirePermission(authz.PermSiteContextRead), h.getSiteVersion)
	g.GET("/context/versions/:versionId/diff", authz.RequirePermission(authz.PermSiteContextRead), h.diffSiteVersion)
	g.POST("/context/versions/:versionId/restore", authz.RequirePermission(authz.PermSiteContextWrite), h.restoreSiteVersion)
	g.GET("/context/effective", authz.RequirePermission(authz.PermSiteContextRead), h.getEffectiveContext)
}

// --- organisation routes ------------------------------------------------------

func (h *Handler) getOrgContext(c *gin.Context) {
	p, orgID, ok := h.orgPrincipal(c)
	if !ok {
		return
	}
	v, err := h.svc.GetOrgContext(c.Request.Context(), orgID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	_ = p
	c.JSON(http.StatusOK, toContextDTO(v))
}

func (h *Handler) patchOrgContext(c *gin.Context) {
	p, orgID, ok := h.orgPrincipal(c)
	if !ok {
		return
	}
	var body patchBody
	if err := bindJSON(c, &body); err != nil {
		httpx.Error(c, err)
		return
	}
	if body.BaseVersion == nil {
		httpx.Error(c, domain.Validation("base_version_required", "base_version is required"))
		return
	}
	v, err := h.svc.PatchOrgContext(c.Request.Context(), orgID, PatchOrgContextInput{
		BaseVersion:  *body.BaseVersion,
		Restrictions: body.Restrictions,
		Guidance:     body.Guidance,
	}, actorFor(p))
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, toContextDTO(v))
}

func (h *Handler) listOrgVersions(c *gin.Context) {
	_, orgID, ok := h.orgPrincipal(c)
	if !ok {
		return
	}
	cursor, limit := listParams(c)
	vs, err := h.svc.ListOrgContextVersions(c.Request.Context(), orgID, cursor, limit)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, toVersionListDTO(vs, limit))
}

func (h *Handler) getOrgVersion(c *gin.Context) {
	_, orgID, ok := h.orgPrincipal(c)
	if !ok {
		return
	}
	versionID, ok := parseVersionID(c)
	if !ok {
		return
	}
	v, err := h.svc.GetOrgContextVersion(c.Request.Context(), orgID, versionID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, toVersionItemDTO(v))
}

func (h *Handler) diffOrgVersion(c *gin.Context) {
	_, orgID, ok := h.orgPrincipal(c)
	if !ok {
		return
	}
	versionID, ok := parseVersionID(c)
	if !ok {
		return
	}
	target, prior, isBaseline, err := h.svc.DiffOrgContext(c.Request.Context(), orgID, versionID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, toDiffDTO(target, prior, isBaseline))
}

func (h *Handler) restoreOrgVersion(c *gin.Context) {
	p, orgID, ok := h.orgPrincipal(c)
	if !ok {
		return
	}
	versionID, ok := parseVersionID(c)
	if !ok {
		return
	}
	v, err := h.svc.RestoreOrgContext(c.Request.Context(), orgID, versionID, actorFor(p))
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, toContextDTO(v))
}

// --- site routes ---------------------------------------------------------------

func (h *Handler) getSiteContext(c *gin.Context) {
	p, siteID, ok := h.sitePrincipal(c)
	if !ok {
		return
	}
	v, err := h.svc.GetSiteContext(c.Request.Context(), p.TenantID, siteID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, toContextDTO(v))
}

func (h *Handler) patchSiteContext(c *gin.Context) {
	p, siteID, ok := h.sitePrincipal(c)
	if !ok {
		return
	}
	var body patchBody
	if err := bindJSON(c, &body); err != nil {
		httpx.Error(c, err)
		return
	}
	if body.BaseVersion == nil {
		httpx.Error(c, domain.Validation("base_version_required", "base_version is required"))
		return
	}
	v, err := h.svc.PatchSiteContext(c.Request.Context(), p.TenantID, siteID, PatchSiteContextInput{
		BaseVersion:  *body.BaseVersion,
		Restrictions: body.Restrictions,
		Guidance:     body.Guidance,
	}, actorFor(p))
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, toContextDTO(v))
}

func (h *Handler) listSiteVersions(c *gin.Context) {
	p, siteID, ok := h.sitePrincipal(c)
	if !ok {
		return
	}
	cursor, limit := listParams(c)
	vs, err := h.svc.ListSiteContextVersions(c.Request.Context(), p.TenantID, siteID, cursor, limit)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, toVersionListDTO(vs, limit))
}

func (h *Handler) getSiteVersion(c *gin.Context) {
	p, siteID, ok := h.sitePrincipal(c)
	if !ok {
		return
	}
	versionID, ok := parseVersionID(c)
	if !ok {
		return
	}
	v, err := h.svc.GetSiteContextVersion(c.Request.Context(), p.TenantID, siteID, versionID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, toVersionItemDTO(v))
}

func (h *Handler) diffSiteVersion(c *gin.Context) {
	p, siteID, ok := h.sitePrincipal(c)
	if !ok {
		return
	}
	versionID, ok := parseVersionID(c)
	if !ok {
		return
	}
	target, prior, isBaseline, err := h.svc.DiffSiteContext(c.Request.Context(), p.TenantID, siteID, versionID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, toDiffDTO(target, prior, isBaseline))
}

func (h *Handler) restoreSiteVersion(c *gin.Context) {
	p, siteID, ok := h.sitePrincipal(c)
	if !ok {
		return
	}
	versionID, ok := parseVersionID(c)
	if !ok {
		return
	}
	v, err := h.svc.RestoreSiteContext(c.Request.Context(), p.TenantID, siteID, versionID, actorFor(p))
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, toContextDTO(v))
}

func (h *Handler) getEffectiveContext(c *gin.Context) {
	p, siteID, ok := h.sitePrincipal(c)
	if !ok {
		return
	}
	rc, err := h.svc.GetEffectiveContext(c.Request.Context(), p.TenantID, siteID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, toEffectiveContextDTO(rc))
}

// --- helpers -------------------------------------------------------------------

// orgPrincipal resolves the caller and the :orgId path param, and enforces
// that orgId equals the caller's own active tenant. Unlike internal/org's
// rename/delete/activate routes, context routes do not support operating on
// an organisation the caller is not currently active in — every other
// resource in this codebase (perf, email, security, ...) reads directly off
// the active-tenant principal, and Decision 6 gates context on the same
// capability registry those use, not org.Handler's membership-lookup
// pattern. A mismatch 404s rather than 403s, matching this codebase's
// existing "do not confirm existence to a caller with no access" convention.
func (h *Handler) orgPrincipal(c *gin.Context) (domain.Principal, uuid.UUID, bool) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return domain.Principal{}, uuid.Nil, false
	}
	orgID, err := uuid.Parse(c.Param("orgId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_org_id", "orgId is not a valid UUID"))
		return domain.Principal{}, uuid.Nil, false
	}
	if orgID != p.TenantID {
		httpx.Error(c, domain.NotFound("org_not_found", "organisation not found"))
		return domain.Principal{}, uuid.Nil, false
	}
	return p, orgID, true
}

func (h *Handler) sitePrincipal(c *gin.Context) (domain.Principal, uuid.UUID, bool) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return domain.Principal{}, uuid.Nil, false
	}
	siteID, err := uuid.Parse(c.Param("siteId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_site_id", "siteId is not a valid UUID"))
		return domain.Principal{}, uuid.Nil, false
	}
	return p, siteID, true
}

// actorFor maps the authenticated principal to the Actor a context write is
// attributed to. AuthorSystem is never produced here — it is reserved for the
// (unbuilt) site-transfer mechanism (ADR-064 Decision 12), which has no HTTP
// caller.
func actorFor(p domain.Principal) Actor {
	if p.Type == domain.PrincipalAPIKey {
		return Actor{Type: AuthorAPIKey, ID: p.APIKeyID}
	}
	return Actor{Type: AuthorUser, ID: p.UserID}
}

func parseVersionID(c *gin.Context) (uuid.UUID, bool) {
	id, err := uuid.Parse(c.Param("versionId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_version_id", "versionId is not a valid UUID"))
		return uuid.Nil, false
	}
	return id, true
}

// bindJSON caps the request body at 1 MiB — the same limit internal/perf's
// bindJSON already applies to every PUT/POST body in this codebase. This is
// this package's answer to ADR-064 open question 4 (nothing bounds a stored
// context row's size): see model.go's package doc comment for the full
// reasoning on why that stays an ordinary transport-level limit rather than a
// new Decision-13 write refusal.
func bindJSON(c *gin.Context, dst any) error {
	dec := json.NewDecoder(io.LimitReader(c.Request.Body, 1<<20))
	if err := dec.Decode(dst); err != nil {
		return domain.Validation("invalid_body", "request body is not valid JSON: "+err.Error())
	}
	return nil
}

func listParams(c *gin.Context) (cursor int64, limit int32) {
	limit = 50
	if s := c.Query("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 && n <= 200 {
			limit = int32(n)
		}
	}
	if s := c.Query("cursor"); s != "" {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil && n >= 0 {
			cursor = n
		}
	}
	return cursor, limit
}
