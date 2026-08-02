package admin

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// Handler serves the superadmin area under /api/v1/admin.
type Handler struct {
	svc          *Service
	pool         *db.Pool
	auditRec     *audit.Recorder
	vulnFeedH    *vulnFeedAdminHandler    // wired via SetVulnFeed; nil until wired
	agentMirrorH *agentMirrorAdminHandler // wired via SetAgentMirror; nil until wired
}

// NewHandler builds an admin Handler.
func NewHandler(svc *Service, pool *db.Pool) *Handler {
	return &Handler{svc: svc, pool: pool}
}

// SetAuditRecorder wires the audit recorder into the handler. Called once at boot.
func (h *Handler) SetAuditRecorder(rec *audit.Recorder) { h.auditRec = rec }

// Register mounts the admin routes on the auth-gated (not tenant-gated)
// v1Auth group. The requireSuperadmin middleware gates the entire sub-group.
func (h *Handler) Register(r *gin.RouterGroup) {
	g := r.Group("/admin", requireSuperadmin(h.pool))
	g.GET("/stats", h.stats)
	g.GET("/users", h.listUsers)
	g.DELETE("/users/:userId", h.deleteUser)
	g.PATCH("/users/:userId", h.setStatus)
	g.POST("/users/:userId/resend-verification", h.resendVerification)
	g.GET("/users/:userId/sites", h.userSites)
	g.GET("/sites/:siteId/tenancy", h.siteTenancy)
	g.POST("/sites/:siteId/grant-self-membership", h.grantSelfMembership)
	g.GET("/accounts-tenancy", h.accountsTenancy)
	// M16 Phase C1 — superadmin billing-admin panel (accounts / account
	// detail / revenue / manual controls). See billing_handler.go +
	// billing_dto.go for the full contract.
	g.GET("/accounts", h.accountsList)
	g.GET("/accounts/:id", h.accountDetail)
	g.POST("/accounts/:id/comp", h.compAccount)
	g.DELETE("/accounts/:id/comp", h.revokeComp)
	g.PUT("/accounts/:id/overrides", h.setOverrides)
	g.POST("/accounts/:id/grace", h.extendGrace)
	g.POST("/accounts/:id/suspend", h.suspendAccount)
	g.POST("/accounts/:id/restore", h.restoreAccount)
	g.POST("/accounts/:id/state", h.forceState)
	g.GET("/revenue", h.revenue)
	// vuln-feed key management (optional; wired via RegisterVulnFeed after boot).
	if h.vulnFeedH != nil {
		vfg := g.Group("/vuln-feed")
		vfg.GET("/status", h.vulnFeedStatus)
		vfg.PUT("/key", h.vulnFeedSetKey)
		vfg.DELETE("/key", h.vulnFeedClearKey)
		vfg.POST("/sync", h.vulnFeedSync)
	}
	// GH #322: manual "check now" for the upstream agent-release mirror
	// (optional; wired via SetAgentMirror after boot).
	if h.agentMirrorH != nil {
		g.POST("/agent-mirror/check", h.agentMirrorCheck)
	}
}

// grantSelfMembership re-attaches the calling superadmin as an OWNER of the org
// that owns the given site (idempotent). Use to recover from a recovery-induced
// org split where the superadmin's account landed in a different org than the
// site. Superadmin-gated; only ever adds the CALLER (never an arbitrary user) to
// the SITE's own org (never an arbitrary tenant).
func (h *Handler) grantSelfMembership(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, err := uuid.Parse(c.Param("siteId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_site_id", "siteId is not a valid UUID"))
		return
	}
	tenantID, tenantName, added, err := h.svc.GrantSelfOwnerMembership(c.Request.Context(), p.UserID, siteID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"ok":          true,
		"tenant_id":   tenantID,
		"tenant_name": tenantName,
		"added":       added,
		"detail": func() string {
			if added {
				return "Added you as owner of " + tenantName + ". Switch to that organization to see the site's data."
			}
			return "You are already a member of " + tenantName + "."
		}(),
	})
}

// siteTenancy is a read-only diagnostic: it returns where a site + its perf data
// (rucss results / cache stats / config) live vs the calling superadmin's org
// memberships, to surface a tenant/ownership split. No mutation.
func (h *Handler) siteTenancy(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, err := uuid.Parse(c.Param("siteId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_site_id", "siteId is not a valid UUID"))
		return
	}
	rep, err := h.svc.SiteTenancy(c.Request.Context(), p.UserID, siteID)
	if err != nil {
		httpx.Error(c, err)
		return
	}

	// Derive a human verdict: do any of your orgs match the tenant that owns the
	// site's perf data?
	dataTenant := uuid.Nil
	for _, d := range rep.DataTenants {
		dataTenant = d.TenantID // last writer wins; they should all agree
	}
	youMatchData := false
	for _, m := range rep.Memberships {
		if dataTenant != uuid.Nil && m.TenantID == dataTenant {
			youMatchData = true
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"site_id":          rep.SiteID,
		"site_found":       rep.SiteFound,
		"site_tenant_id":   rep.SiteTenantID,
		"site_tenant_name": rep.SiteTenantName,
		"site_url":         rep.SiteURL,
		"data_tenants":     rep.DataTenants,
		"your_memberships": rep.Memberships,
		"site_shares":      rep.SiteShares,
		"verdict": gin.H{
			"site_matches_data":     rep.SiteFound && dataTenant != uuid.Nil && rep.SiteTenantID == dataTenant,
			"you_can_see_perf_data": youMatchData,
		},
	})
}

// accountsTenancy is a read-only diagnostic: it returns every user whose email
// matches the ?email=<substr> query parameter (ILIKE %substr%), with their org
// memberships, plus a full org census (every tenant with site + member counts).
// Intended for diagnosing account/org splits (e.g. a superadmin stranded in the
// wrong org while site data lives in a different org). No mutation.
//
// Query param:
//
//	email  — substring to ILIKE-match against users.email (required; empty string matches all users)
func (h *Handler) accountsTenancy(c *gin.Context) {
	emailSubstr := c.Query("email")
	rep, err := h.svc.AccountsTenancy(c.Request.Context(), emailSubstr)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"users": rep.Users,
		"orgs":  rep.Orgs,
	})
}

// userSites returns every site reachable by the given user via their org
// memberships. The route is lazy — it fires only when a row is expanded in the
// UI. The :userId path parameter must be a valid UUID; an invalid value returns
// 400 before any DB call.
func (h *Handler) userSites(c *gin.Context) {
	userID, err := uuid.Parse(c.Param("userId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_user_id", "userId is not a valid UUID"))
		return
	}
	sites, err := h.svc.ListSitesByUser(c.Request.Context(), userID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	items := make([]gin.H, 0, len(sites))
	for _, s := range sites {
		item := gin.H{
			"site_id":          s.SiteID,
			"url":              s.URL,
			"name":             s.Name,
			"connection_state": s.ConnectionState,
			"enrolled_at":      s.EnrolledAt,
			"site_created_at":  s.SiteCreatedAt,
			"tenant_id":        s.TenantID,
			"tenant_name":      s.TenantName,
			"member_role":      s.MemberRole,
		}
		items = append(items, item)
	}
	c.JSON(http.StatusOK, gin.H{
		"user_id": userID,
		"sites":   items,
	})
}

// requireSuperadmin is a Gin middleware that returns 403 unless the
// authenticated principal has is_superadmin=true. It does a targeted
// single-column DB read (no joins) against the users table, which has no RLS,
// so it runs on the bare pool without any tenant context.
func requireSuperadmin(pool *db.Pool) gin.HandlerFunc {
	return func(c *gin.Context) {
		p, ok := domain.PrincipalFromContext(c.Request.Context())
		if !ok || p.Type != domain.PrincipalUser {
			httpx.Error(c, domain.Forbidden("superadmin_required", "superadmin access required"))
			c.Abort()
			return
		}
		var isSA bool
		err := pool.QueryRow(c.Request.Context(),
			`SELECT is_superadmin FROM users WHERE id = $1`, p.UserID,
		).Scan(&isSA)
		if err != nil || !isSA {
			httpx.Error(c, domain.Forbidden("superadmin_required", "superadmin access required"))
			c.Abort()
			return
		}
		c.Next()
	}
}

func (h *Handler) stats(c *gin.Context) {
	s, err := h.svc.Stats(c.Request.Context())
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"users":         s.Users,
		"organizations": s.Orgs,
		"sites":         s.Sites,
	})
}

func (h *Handler) listUsers(c *gin.Context) {
	search := c.Query("search")
	limit := parseInt32(c.Query("limit"), 50)
	offset := parseInt32(c.Query("offset"), 0)
	users, err := h.svc.ListUsers(c.Request.Context(), search, limit, offset)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	items := make([]gin.H, 0, len(users))
	for _, u := range users {
		items = append(items, userToJSON(u))
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

func (h *Handler) deleteUser(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	targetID, err := uuid.Parse(c.Param("userId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_user_id", "userId is not a valid UUID"))
		return
	}
	res, err := h.svc.DeleteUser(c.Request.Context(), p.UserID, targetID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	kept := make([]gin.H, 0, len(res.KeptOrgs))
	for _, o := range res.KeptOrgs {
		kept = append(kept, gin.H{"id": o.ID, "name": o.Name, "site_count": o.SiteCount})
	}
	c.JSON(http.StatusOK, gin.H{
		"deleted_orgs":         res.DeletedOrgs,
		"kept_orgs_with_sites": kept,
	})
}

type setStatusBody struct {
	Status string `json:"status"`
}

func (h *Handler) setStatus(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	targetID, err := uuid.Parse(c.Param("userId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_user_id", "userId is not a valid UUID"))
		return
	}
	var body setStatusBody
	if err := c.ShouldBindJSON(&body); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
		return
	}
	updated, err := h.svc.SetStatus(c.Request.Context(), p.UserID, targetID, body.Status)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, userToJSON(updated))
}

func (h *Handler) resendVerification(c *gin.Context) {
	targetID, err := uuid.Parse(c.Param("userId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_user_id", "userId is not a valid UUID"))
		return
	}
	if err := h.svc.ResendVerification(c.Request.Context(), targetID); err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func userToJSON(u AdminUser) gin.H {
	m := gin.H{
		"id":             u.ID,
		"email":          u.Email,
		"name":           u.Name,
		"status":         u.Status,
		"email_verified": u.EmailVerified,
		"created_at":     u.CreatedAt,
		"is_superadmin":  u.IsSuperadmin,
		"org_count":      u.OrgCount,
	}
	if u.LastLoginAt != nil {
		m["last_login_at"] = *u.LastLoginAt
	}
	return m
}

func parseInt32(s string, def int32) int32 {
	if s == "" {
		return def
	}
	n, err := strconv.ParseInt(s, 10, 32)
	if err != nil || n < 0 {
		return def
	}
	if n > 200 {
		return 200
	}
	return int32(n)
}
