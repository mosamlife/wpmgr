package site

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/api/gen"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// RefreshEnqueuer schedules an immediate CP->agent inventory-refresh job for a
// site. Implemented by the update package's RiverEnqueuer (wired in main); a
// local interface here keeps the site package free of an update import.
type RefreshEnqueuer interface {
	EnqueueRefresh(ctx context.Context, tenantID, siteID uuid.UUID, siteURL, source string) error
}

// Handler serves the site HTTP endpoints under /api/v1/sites plus the public
// /enroll endpoint. The active tenant for authed routes is taken from the
// authenticated principal; /enroll derives the tenant from the pairing code.
type Handler struct {
	svc   *Service
	audit *audit.Recorder
	// conn is the M21 connection-lifecycle service (ADR-041). nil ⇒ the
	// lifecycle routes + the site-first create return 501 / fall back to legacy.
	conn ConnectionService
	// cpPublicKey is the control-plane's base64 Ed25519 PUBLIC signing key,
	// returned to agents at enrollment so they can verify CP->agent commands.
	cpPublicKey string
	// refresher enqueues inventory-refresh jobs. nil when CP->agent commands are
	// disabled (the /updates/refresh route then returns 501).
	refresher RefreshEnqueuer
	// staleAfter is the freshness window used to decide whether the agent is
	// reachable for an immediate refresh. Zero ⇒ no freshness gate.
	staleAfter time.Duration
	now        func() time.Time
}

// NewHandler builds a site Handler. cpPublicKey is the control plane's base64
// public signing key.
func NewHandler(svc *Service, rec *audit.Recorder, cpPublicKey string) *Handler {
	return &Handler{svc: svc, audit: rec, cpPublicKey: cpPublicKey, now: time.Now}
}

// SetRefreshEnqueuer wires the inventory-refresh enqueuer and the agent
// freshness threshold the refresh route uses to decide 409 vs 202. Call once
// at boot.
func (h *Handler) SetRefreshEnqueuer(r RefreshEnqueuer, staleAfter time.Duration) {
	h.refresher = r
	h.staleAfter = staleAfter
}

// SetClock overrides the time source used for staleness checks (tests).
func (h *Handler) SetClock(now func() time.Time) { h.now = now }

func (h *Handler) record(c *gin.Context, tenantID uuid.UUID, action, siteID string, meta map[string]any) {
	if h.audit == nil {
		return // tests may not wire an audit recorder
	}
	actorType := audit.ActorSystem
	actorID := ""
	if p, ok := domain.PrincipalFromContext(c.Request.Context()); ok {
		actorType = audit.ActorUser
		if p.Type == domain.PrincipalAPIKey {
			actorType = audit.ActorAPIKey
		}
		actorID = p.ActorID()
	}
	_, _ = h.audit.Record(c.Request.Context(), audit.Event{
		TenantID:   tenantID,
		ActorType:  actorType,
		ActorID:    actorID,
		Action:     action,
		TargetType: "site",
		TargetID:   siteID,
		Metadata:   meta,
	})
}

// Register mounts the authed site routes on the /api/v1 router group.
func (h *Handler) Register(r *gin.RouterGroup) {
	// Tenant-wide collection routes: no :siteId, site-scoped filtering is done
	// by RLS (InScopedTenantTx activated by RunTenantTx in the repo.List path).
	r.POST("/sites", authz.RequirePermission(authz.PermSiteWrite), h.create)
	r.GET("/sites", authz.RequirePermission(authz.PermSiteRead), h.list)
	r.POST("/sites/pairing-codes", authz.RequirePermission(authz.PermSiteWrite), h.createPairingCode)
	// M21 connection-lifecycle mutations (revoke/archive/restore/re-enroll).
	h.RegisterConnection(r)
	// Per-siteId routes: RequireSiteAccess enforces the site allowlist for
	// site-scoped principals (belt-and-braces in front of the RLS policy).
	r.GET("/sites/:siteId", authz.RequirePermission(authz.PermSiteRead), authz.RequireSiteAccess("siteId"), h.get)
	// Deleting a site is a severing action like revoke/archive — require org
	// scope so a site-scoped collaborator can't delete a site merely shared with
	// them (Phase 6 security review, finding #5 — same class as the lifecycle routes).
	r.DELETE("/sites/:siteId", authz.RequirePermission(authz.PermSiteWrite), authz.RequireOrgScope(), authz.RequireSiteAccess("siteId"), h.delete)
	r.PUT("/sites/:siteId/tags", authz.RequirePermission(authz.PermSiteWrite), authz.RequireSiteAccess("siteId"), h.setTags)
	// Updates feature (Track B): an operator triggers an immediate inventory
	// refresh, or reads the cached per-item available-updates list. Both are
	// view-permission routes: refresh is a side-effecting POST but it does not
	// mutate persisted state (it asks the agent to push fresh metadata back).
	r.POST("/sites/:siteId/updates/refresh", authz.RequirePermission(authz.PermSiteRead), authz.RequireSiteAccess("siteId"), h.refreshUpdates)
	r.GET("/sites/:siteId/updates/available", authz.RequirePermission(authz.PermSiteRead), authz.RequireSiteAccess("siteId"), h.getAvailableUpdates)
}

// RegisterPublic mounts the public, unauthenticated /enroll endpoint on the
// root engine. The agent has no session/tenant; the pairing code is the
// authorization.
func (h *Handler) RegisterPublic(r gin.IRouter) {
	r.POST("/enroll", h.enroll)
}

type createSiteRequest struct {
	URL        string `json:"url"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	WPVersion  string `json:"wp_version"`
	PHPVersion string `json:"php_version"`
}

func (h *Handler) create(c *gin.Context) {
	// M21 site-first flow: when the connection-lifecycle service is wired, POST
	// /sites provisions a pending_enrollment site + a site-bound enrollment code
	// and returns {site_id, enrollment_code, expires_at} (BREAKING shape change
	// vs the legacy bare-Site response). Falls back to the legacy create when the
	// lifecycle service is disabled (no SSE bus / dev builds).
	if h.conn != nil {
		h.createWithEnrollment(c)
		return
	}
	tenantID, ok := domain.TenantIDFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Forbidden("tenant_required", "a tenant context is required"))
		return
	}
	var req createSiteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
		return
	}
	s, err := h.svc.Create(c.Request.Context(), CreateInput{
		TenantID:   tenantID,
		URL:        req.URL,
		Name:       req.Name,
		Status:     req.Status,
		WPVersion:  req.WPVersion,
		PHPVersion: req.PHPVersion,
	})
	if err != nil {
		httpx.Error(c, err)
		return
	}
	h.record(c, tenantID, audit.ActionSiteCreate, s.ID.String(), nil)
	out := toAPI(s)
	c.JSON(http.StatusCreated, &out)
}

func (h *Handler) get(c *gin.Context) {
	tenantID, ok := domain.TenantIDFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Forbidden("tenant_required", "a tenant context is required"))
		return
	}
	id, err := uuid.Parse(c.Param("siteId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_site_id", "siteId is not a valid UUID"))
		return
	}
	s, err := h.svc.Get(c.Request.Context(), tenantID, id)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	out := toAPI(s)
	c.JSON(http.StatusOK, &out)
}

func (h *Handler) list(c *gin.Context) {
	tenantID, ok := domain.TenantIDFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Forbidden("tenant_required", "a tenant context is required"))
		return
	}
	// FIX 3: thread the principal so repo.List can activate scoped RLS for
	// site-scoped principals (they should see ONLY their granted sites).
	in := ListInput{
		TenantID: tenantID,
		Tag:      c.Query("tag"),
		Limit:    parseInt32(c.Query("limit"), 50),
		Offset:   parseInt32(c.Query("offset"), 0),
	}
	// M21: default hides archived sites; ?state=<connection_state> filters to a
	// single state (e.g. archived chip). ?include_archived=true is a convenience
	// alias that surfaces only the archived list.
	if st := c.Query("state"); st != "" {
		in.State = st
	} else if c.Query("include_archived") == "true" {
		in.State = string(StateArchived)
	}
	if p, ok := domain.PrincipalFromContext(c.Request.Context()); ok {
		in.Principal = p
	}
	ss, err := h.svc.List(c.Request.Context(), in)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	items := make([]gen.Site, 0, len(ss))
	for _, s := range ss {
		items = append(items, toAPI(s))
	}
	c.JSON(http.StatusOK, gen.SiteList{Items: items})
}

func (h *Handler) delete(c *gin.Context) {
	tenantID, ok := domain.TenantIDFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Forbidden("tenant_required", "a tenant context is required"))
		return
	}
	id, err := uuid.Parse(c.Param("siteId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_site_id", "siteId is not a valid UUID"))
		return
	}
	if err := h.svc.Delete(c.Request.Context(), tenantID, id); err != nil {
		httpx.Error(c, err)
		return
	}
	h.record(c, tenantID, audit.ActionSiteDelete, id.String(), nil)
	c.Status(http.StatusNoContent)
}

type pairingCodeRequest struct {
	SiteName string   `json:"site_name"`
	Tags     []string `json:"tags"`
}

func (h *Handler) createPairingCode(c *gin.Context) {
	tenantID, ok := domain.TenantIDFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Forbidden("tenant_required", "a tenant context is required"))
		return
	}
	var req pairingCodeRequest
	// Body is optional; tolerate an empty/absent body.
	if c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
			return
		}
	}
	var createdBy uuid.UUID
	if p, ok := domain.PrincipalFromContext(c.Request.Context()); ok {
		createdBy = p.UserID
	}
	created, err := h.svc.CreatePairingCode(c.Request.Context(), CreatePairingCodeInput{
		TenantID:  tenantID,
		CreatedBy: createdBy,
		SiteName:  req.SiteName,
		Tags:      req.Tags,
	})
	if err != nil {
		httpx.Error(c, err)
		return
	}
	h.record(c, tenantID, audit.ActionPairingCodeCreated, "", map[string]any{
		"pairing_code_id": created.Code.ID.String(),
		"expires_at":      created.Code.ExpiresAt,
	})
	out := gen.PairingCode{
		ID:        created.Code.ID,
		TenantID:  created.Code.TenantID,
		Code:      created.Plaintext,
		Tags:      created.Code.Tags,
		ExpiresAt: created.Code.ExpiresAt,
		CreatedAt: created.Code.CreatedAt,
	}
	if created.Code.SiteName != "" {
		out.SiteName = gen.NewOptString(created.Code.SiteName)
	}
	c.JSON(http.StatusCreated, &out)
}

type setTagsRequest struct {
	Tags []string `json:"tags"`
}

func (h *Handler) setTags(c *gin.Context) {
	tenantID, ok := domain.TenantIDFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Forbidden("tenant_required", "a tenant context is required"))
		return
	}
	id, err := uuid.Parse(c.Param("siteId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_site_id", "siteId is not a valid UUID"))
		return
	}
	var req setTagsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
		return
	}
	s, err := h.svc.SetTags(c.Request.Context(), SetTagsInput{TenantID: tenantID, SiteID: id, Tags: req.Tags})
	if err != nil {
		httpx.Error(c, err)
		return
	}
	h.record(c, tenantID, audit.ActionSiteTagsSet, s.ID.String(), map[string]any{"tags": s.Tags})
	out := toAPI(s)
	c.JSON(http.StatusOK, &out)
}

type enrollRequest struct {
	PairingCode    string   `json:"pairing_code"`
	SiteURL        string   `json:"site_url"`
	AgentPublicKey string   `json:"agent_public_key"`
	Name           string   `json:"name"`
	WPVersion      string   `json:"wp_version"`
	PHPVersion     string   `json:"php_version"`
	Tags           []string `json:"tags"`
}

func (h *Handler) enroll(c *gin.Context) {
	var req enrollRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
		return
	}
	s, err := h.svc.Enroll(c.Request.Context(), EnrollRequest{
		PairingCode:    req.PairingCode,
		SiteURL:        req.SiteURL,
		AgentPublicKey: req.AgentPublicKey,
		Name:           req.Name,
		WPVersion:      req.WPVersion,
		PHPVersion:     req.PHPVersion,
		Tags:           req.Tags,
		ConsumedFromIP: c.ClientIP(),
	})
	if err != nil {
		httpx.Error(c, err)
		return
	}
	h.record(c, s.TenantID, audit.ActionSiteEnrolled, s.ID.String(), map[string]any{"url": s.URL})
	c.JSON(http.StatusOK, &gen.EnrollResponse{
		SiteID:                s.ID,
		TenantID:              s.TenantID,
		ControlPlanePublicKey: h.cpPublicKey,
	})
}

// refreshUpdates enqueues an immediate CP->agent inventory-refresh for the
// resolved site. Returns 202 on success; 404 if the site isn't in the tenant;
// 409 with `site_unreachable` when the site isn't enrolled or its last
// heartbeat is older than the configured stale threshold; 501 when the
// refresher isn't wired (CP signing disabled).
func (h *Handler) refreshUpdates(c *gin.Context) {
	tenantID, ok := domain.TenantIDFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Forbidden("tenant_required", "a tenant context is required"))
		return
	}
	id, err := uuid.Parse(c.Param("siteId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_site_id", "siteId is not a valid UUID"))
		return
	}
	s, err := h.svc.Get(c.Request.Context(), tenantID, id)
	if err != nil {
		httpx.Error(c, err) // 404 via RLS-scoped Get
		return
	}
	if h.refresher == nil {
		// CP->agent commands disabled (no signing key) — surface clearly rather
		// than silently dropping the request.
		httpx.Error(c, domain.Internal("refresh_disabled", "inventory refresh is disabled: no CP signing key configured"))
		return
	}
	if s.EnrolledAt == nil {
		httpx.Error(c, domain.Conflict("site_unreachable", "site is not enrolled with an agent"))
		return
	}
	if h.staleAfter > 0 {
		cutoff := h.now().Add(-h.staleAfter)
		if s.LastSeenAt == nil || s.LastSeenAt.Before(cutoff) {
			httpx.Error(c, domain.Conflict("site_unreachable", "site agent heartbeat is stale; cannot refresh now"))
			return
		}
	}
	if err := h.refresher.EnqueueRefresh(c.Request.Context(), tenantID, s.ID, s.URL, "api"); err != nil {
		httpx.Error(c, domain.Internal("refresh_enqueue_failed", "failed to enqueue inventory refresh").WithCause(err))
		return
	}
	h.record(c, tenantID, audit.ActionUpdateRefreshRequested, s.ID.String(), map[string]any{
		"site_id": s.ID.String(),
		"source":  "api",
	})
	c.Status(http.StatusAccepted)
}

// getAvailableUpdates returns the cached list of items with updates available
// for the resolved site, sorted core -> plugins -> themes (active before
// inactive). as_of is the site's last updated_at.
func (h *Handler) getAvailableUpdates(c *gin.Context) {
	tenantID, ok := domain.TenantIDFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Forbidden("tenant_required", "a tenant context is required"))
		return
	}
	id, err := uuid.Parse(c.Param("siteId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_site_id", "siteId is not a valid UUID"))
		return
	}
	s, err := h.svc.Get(c.Request.Context(), tenantID, id)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	out := buildAvailableUpdates(s)
	c.JSON(http.StatusOK, &out)
}

// buildAvailableUpdates projects a Site's JSONB inventory into the OpenAPI
// SiteAvailableUpdates response: filters to components with an AvailableUpdate,
// attaches the optional CoreUpdate, sorts core->plugins->themes (active before
// inactive within each kind), and stamps as_of with the site's updated_at.
func buildAvailableUpdates(s Site) gen.SiteAvailableUpdates {
	plugins, themes := s.ParsedComponents()
	core := s.ParsedCoreUpdate()
	items := make([]gen.SiteAvailableUpdatesItemsItem, 0, len(plugins)+len(themes))
	for _, p := range plugins {
		if p.AvailableUpdate == nil || p.AvailableUpdate.NewVersion == "" {
			continue
		}
		items = append(items, toAvailableItem(gen.SiteAvailableUpdatesItemsItemTypePlugin, p))
	}
	for _, t := range themes {
		if t.AvailableUpdate == nil || t.AvailableUpdate.NewVersion == "" {
			continue
		}
		items = append(items, toAvailableItem(gen.SiteAvailableUpdatesItemsItemTypeTheme, t))
	}
	// Stable sort by (type rank, !active, slug) so the response order is
	// deterministic across calls and easy for the UI to render.
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Type != items[j].Type {
			return typeRank(items[i].Type) < typeRank(items[j].Type)
		}
		if items[i].Active != items[j].Active {
			return items[i].Active // active before inactive
		}
		return items[i].Slug < items[j].Slug
	})
	out := gen.SiteAvailableUpdates{SiteID: s.ID, Items: items}
	if core != nil {
		out.CoreUpdate = gen.NewOptNilSiteAvailableUpdatesCoreUpdate(gen.SiteAvailableUpdatesCoreUpdate{
			NewVersion:     core.NewVersion,
			CurrentVersion: core.CurrentVersion,
		})
	}
	if !s.UpdatedAt.IsZero() {
		out.AsOf = gen.NewOptNilDateTime(s.UpdatedAt)
	}
	return out
}

// typeRank orders core (synthetic, here absent because core lives in CoreUpdate)
// then plugin then theme. Plugins outrank themes in the items list per spec.
func typeRank(t gen.SiteAvailableUpdatesItemsItemType) int {
	switch t {
	case gen.SiteAvailableUpdatesItemsItemTypePlugin:
		return 1
	case gen.SiteAvailableUpdatesItemsItemTypeTheme:
		return 2
	default:
		return 3
	}
}

func toAvailableItem(t gen.SiteAvailableUpdatesItemsItemType, c Component) gen.SiteAvailableUpdatesItemsItem {
	item := gen.SiteAvailableUpdatesItemsItem{
		Type:       t,
		Slug:       c.Slug,
		Name:       c.Name,
		Version:    c.Version,
		NewVersion: c.AvailableUpdate.NewVersion,
		Active:     c.Active,
	}
	if c.AvailableUpdate.Package != "" {
		item.Package = gen.NewOptNilString(c.AvailableUpdate.Package)
	}
	if c.AvailableUpdate.Tested != "" {
		item.Tested = gen.NewOptNilString(c.AvailableUpdate.Tested)
	}
	if c.AvailableUpdate.RequiresPHP != "" {
		item.RequiresPhp = gen.NewOptNilString(c.AvailableUpdate.RequiresPHP)
	}
	return item
}

// toAPI maps a Site to its OpenAPI representation, including the M2 enrollment,
// health, and metadata fields.
func toAPI(s Site) gen.Site {
	u, _ := url.Parse(s.URL)
	if u == nil {
		u = &url.URL{}
	}
	out := gen.Site{
		ID:           s.ID,
		TenantID:     s.TenantID,
		URL:          *u,
		Name:         s.Name,
		Status:       gen.SiteStatus(s.Status),
		WpVersion:    s.WPVersion,
		PhpVersion:   s.PHPVersion,
		HealthStatus: gen.SiteHealthStatus(s.HealthStatus),
		Multisite:    s.Multisite,
		Tags:         s.Tags,
		Enrolled:     gen.NewOptBool(s.EnrolledAt != nil),
		CreatedAt:    s.CreatedAt,
		UpdatedAt:    s.UpdatedAt,
	}
	if s.Tags == nil {
		out.Tags = []string{}
	}
	if s.ServerInfo != "" {
		out.ServerInfo = gen.NewOptString(s.ServerInfo)
	}
	if s.ActiveTheme != "" {
		out.ActiveTheme = gen.NewOptString(s.ActiveTheme)
	}
	if s.EnrolledAt != nil {
		out.EnrolledAt = gen.NewOptDateTime(*s.EnrolledAt)
	}
	if s.LastSeenAt != nil {
		out.LastSeenAt = gen.NewOptDateTime(*s.LastSeenAt)
	}
	// M21 connection lifecycle (ADR-041): surface connection_state to the REST
	// API so the dashboard's ConnectionStateBadge + sites list render the live
	// lifecycle on first load (the SSE stream patches it thereafter).
	if s.ConnectionState != "" {
		out.ConnectionState = gen.NewOptSiteConnectionState(gen.SiteConnectionState(s.ConnectionState))
	}
	out.ConnectionGeneration = gen.NewOptInt32(s.ConnectionGeneration)
	if s.DisconnectedReason != "" {
		out.DisconnectedReason = gen.NewOptString(s.DisconnectedReason)
	}
	if len(s.Components) > 0 {
		var comp struct {
			Plugins    []Component `json:"plugins"`
			Themes     []Component `json:"themes"`
			CoreUpdate *CoreUpdate `json:"core_update,omitempty"`
		}
		if json.Unmarshal(s.Components, &comp) == nil &&
			(len(comp.Plugins) > 0 || len(comp.Themes) > 0 || comp.CoreUpdate != nil) {
			sc := gen.SiteComponents{
				Plugins: toAPIComponents(comp.Plugins),
				Themes:  toAPIComponents(comp.Themes),
			}
			if comp.CoreUpdate != nil {
				sc.CoreUpdate = gen.NewOptNilSiteComponentsCoreUpdate(gen.SiteComponentsCoreUpdate{
					NewVersion:     comp.CoreUpdate.NewVersion,
					CurrentVersion: comp.CoreUpdate.CurrentVersion,
				})
			}
			out.Components = gen.NewOptSiteComponents(sc)
		}
	}
	return out
}

func toAPIComponents(cs []Component) []gen.SiteComponent {
	out := make([]gen.SiteComponent, 0, len(cs))
	for _, c := range cs {
		gc := gen.SiteComponent{Slug: c.Slug, Active: gen.NewOptBool(c.Active)}
		if c.Name != "" {
			gc.Name = gen.NewOptString(c.Name)
		}
		if c.Version != "" {
			gc.Version = gen.NewOptString(c.Version)
		}
		if c.AvailableUpdate != nil && c.AvailableUpdate.NewVersion != "" {
			au := gen.SiteComponentAvailableUpdate{NewVersion: c.AvailableUpdate.NewVersion}
			if c.AvailableUpdate.Package != "" {
				au.Package = gen.NewOptNilString(c.AvailableUpdate.Package)
			}
			if c.AvailableUpdate.Tested != "" {
				au.Tested = gen.NewOptNilString(c.AvailableUpdate.Tested)
			}
			if c.AvailableUpdate.RequiresPHP != "" {
				au.RequiresPhp = gen.NewOptNilString(c.AvailableUpdate.RequiresPHP)
			}
			gc.AvailableUpdate = gen.NewOptNilSiteComponentAvailableUpdate(au)
		}
		out = append(out, gc)
	}
	return out
}

func parseInt32(s string, def int32) int32 {
	if s == "" {
		return def
	}
	n, err := strconv.ParseInt(s, 10, 32)
	if err != nil {
		return def
	}
	return int32(n)
}
