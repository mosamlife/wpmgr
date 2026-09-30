package content

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// Handler serves the content inventory routes.
type Handler struct {
	svc      *Service
	enqueuer Enqueuer
	rec      *audit.Recorder
}

// NewHandler builds the handler. The enqueuer and recorder are wired after
// River starts and audit is constructed.
func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// SetEnqueuer wires the River enqueuer.
func (h *Handler) SetEnqueuer(e Enqueuer) { h.enqueuer = e }

// SetAuditRecorder wires the audit recorder.
func (h *Handler) SetAuditRecorder(r *audit.Recorder) { h.rec = r }

// Register mounts the per-site routes on the authenticated /api/v1 group.
// RequireSiteAccess gates the by-id group.
func (h *Handler) Register(r *gin.RouterGroup) {
	g := r.Group("/sites/:siteId", authz.RequireSiteAccess("siteId"))
	g.GET("/content/inventory", authz.RequirePermission(authz.PermSiteRead), h.getInventory)
	g.POST("/content/inventory/refresh", authz.RequirePermission(authz.PermSiteContentRefresh), h.refresh)
}

// RegisterAdmin mounts the superadmin routes on a group that is ALREADY behind
// the admin package's requireSuperadmin. Nothing here re-checks the gate; the
// caller must pass the gated group.
func (h *Handler) RegisterAdmin(g *gin.RouterGroup) {
	g.GET("/content/fleet-report", h.fleetReport)
	g.GET("/content/integrations", h.listIntegrations)
	g.PUT("/content/integrations/:integrationId", h.upsertIntegration)
}

func (h *Handler) getInventory(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}
	siteID, err := uuid.Parse(c.Param("siteId"))
	if err != nil {
		httpx.Error(c, domain.NotFound("site_not_found", "site not found"))
		return
	}
	after, limit := int64(0), 50
	if v := c.Query("after_post_id"); v != "" {
		n, perr := strconv.ParseInt(v, 10, 64)
		if perr != nil || n < 0 {
			httpx.Error(c, domain.Validation("invalid_after_post_id", "after_post_id must be a non-negative integer"))
			return
		}
		after = n
	}
	if v := c.Query("limit"); v != "" {
		n, perr := strconv.Atoi(v)
		if perr != nil || n < 1 || n > MaxInventoryPage {
			httpx.Error(c, domain.Validation("invalid_limit", "limit must be from 1 to 200"))
			return
		}
		limit = n
	}
	// Titles are the site's own text: a separate permission from the route facts.
	withTitles := authz.PrincipalAllows(p, authz.PermSiteContentRead)
	page, err := h.svc.Inventory(c.Request.Context(), p, siteID, after, c.Query("editor"), limit, withTitles)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, toInventoryPageDTO(page))
}

func (h *Handler) refresh(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}
	siteID, err := uuid.Parse(c.Param("siteId"))
	if err != nil {
		httpx.Error(c, domain.NotFound("site_not_found", "site not found"))
		return
	}
	// Prove the site is in the caller's tenant before queueing anything for it.
	if _, err := h.svc.repo.GetSiteTarget(c.Request.Context(), p.TenantID, siteID); err != nil {
		httpx.Error(c, err)
		return
	}
	if err := h.svc.RequestRefresh(c.Request.Context(), h.enqueuer, p.TenantID, siteID); err != nil {
		if de, ok := domain.AsDomain(err); ok && de.Kind == domain.KindRateLimited {
			c.Header("Retry-After", strconv.Itoa(int(RefreshRateWindow.Seconds())))
		}
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"status": "queued"})
}

func (h *Handler) fleetReport(c *gin.Context) {
	rep, err := h.svc.Fleet(c.Request.Context())
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, toFleetReportDTO(rep))
}

func (h *Handler) listIntegrations(c *gin.Context) {
	rows, err := h.svc.ListIntegrations(c.Request.Context())
	if err != nil {
		httpx.Error(c, err)
		return
	}
	out := make([]integrationDTO, 0, len(rows))
	for _, r := range rows {
		out = append(out, toIntegrationDTO(r))
	}
	c.JSON(http.StatusOK, gin.H{"integrations": out})
}

func (h *Handler) upsertIntegration(c *gin.Context) {
	// The actor is the authenticated session, and only that.
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok || p.Type != domain.PrincipalUser || p.UserID == uuid.Nil {
		httpx.Error(c, domain.Forbidden("superadmin_required", "superadmin access required"))
		return
	}
	body, err := c.GetRawData()
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body could not be read"))
		return
	}
	var in integrationInput
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid for this route"))
		return
	}
	abil := in.Abilities
	if string(bytes.TrimSpace(abil)) == "null" {
		abil = nil
	}
	rec, err := h.svc.UpsertIntegration(c.Request.Context(), AdminUpsertInput{
		ActorUserID:   p.UserID,
		IntegrationID: c.Param("integrationId"),
		DisplayName:   in.DisplayName, Enabled: in.Enabled, Status: in.Status,
		Descriptor: in.Descriptor, Abilities: abil,
		MinVersion: in.MinVersion, MaxTestedVersion: in.MaxTestedVersion, MinWPVersion: in.MinWPVersion,
	})
	if err != nil {
		httpx.Error(c, err)
		return
	}
	if h.rec != nil {
		sha := ""
		if rec.IntegrationEntrySHA256 != nil {
			sha = *rec.IntegrationEntrySHA256
		}
		_, _ = h.rec.Record(c.Request.Context(), audit.Event{
			ActorType:  audit.ActorUser,
			ActorID:    p.UserID.String(),
			Action:     "admin.content_integration.upsert",
			TargetType: "content_integration",
			TargetID:   rec.IntegrationID,
			Metadata:   map[string]any{"enabled": rec.Enabled, "integration_entry_sha256": sha},
		})
	}
	c.JSON(http.StatusOK, toIntegrationDTO(rec))
}
