package sitedestination

import (
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// Handler serves the per-site destination CRUD under
// /api/v1/sites/{site_id}/destinations. Operator+ can list/manage destinations;
// owners only? No — backups in general are an operator task in V0, so we mirror
// site:write for management and site:read for the list endpoint.
type Handler struct {
	svc   *Service
	audit *audit.Recorder
}

// NewHandler builds a handler from the wired service.
func NewHandler(svc *Service, rec *audit.Recorder) *Handler {
	return &Handler{svc: svc, audit: rec}
}

// Register mounts the routes on the authenticated /api/v1 group. The siteId is
// captured at the per-route level so the resource hierarchy reads naturally
// (sites > destinations).
func (h *Handler) Register(r *gin.RouterGroup) {
	// RequireSiteAccess("siteId") is applied on the group so every sub-route
	// inherits it. This enforces the site allowlist for site-scoped principals
	// (belt-and-braces in front of the RLS policy on site_destinations).
	g := r.Group("/sites/:siteId/destinations", authz.RequireSiteAccess("siteId"))
	g.GET("", authz.RequirePermission(authz.PermSiteRead), h.list)
	g.POST("", authz.RequirePermission(authz.PermSiteWrite), h.create)
	g.POST("/test", authz.RequirePermission(authz.PermSiteWrite), h.test)
	g.GET("/:destinationId", authz.RequirePermission(authz.PermSiteRead), h.get)
	g.PATCH("/:destinationId", authz.RequirePermission(authz.PermSiteWrite), h.update)
	g.DELETE("/:destinationId", authz.RequirePermission(authz.PermSiteWrite), h.delete)
}

// destinationDTO is the public, secret-stripped shape we render. The encrypted
// secret bytes NEVER cross the API boundary; only a "has_secret" boolean does,
// so the UI can render its "Re-enter to save changes" hint correctly.
type destinationDTO struct {
	ID             uuid.UUID `json:"id"`
	TenantID       uuid.UUID `json:"tenant_id"`
	SiteID         uuid.UUID `json:"site_id"`
	Kind           string    `json:"kind"`
	Label          string    `json:"label"`
	Endpoint       string    `json:"endpoint"`
	Region         string    `json:"region"`
	Bucket         string    `json:"bucket"`
	PathPrefix     string    `json:"path_prefix"`
	AccessKeyID    string    `json:"access_key_id"`
	HasSecret      bool      `json:"has_secret"`
	ForcePathStyle bool      `json:"force_path_style"`
	IsDefault      bool      `json:"is_default"`
	CreatedAt      string    `json:"created_at"`
	UpdatedAt      string    `json:"updated_at"`
}

type destinationListDTO struct {
	Items []destinationDTO `json:"items"`
}

func toDTO(d SiteDestination) destinationDTO {
	return destinationDTO{
		ID:             d.ID,
		TenantID:       d.TenantID,
		SiteID:         d.SiteID,
		Kind:           string(d.Kind),
		Label:          d.Label,
		Endpoint:       d.Endpoint,
		Region:         d.Region,
		Bucket:         d.Bucket,
		PathPrefix:     d.PathPrefix,
		AccessKeyID:    d.AccessKeyID,
		HasSecret:      len(d.SecretKeyEnc) > 0,
		ForcePathStyle: d.ForcePathStyle,
		IsDefault:      d.IsDefault,
		CreatedAt:      d.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		UpdatedAt:      d.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
	}
}

// createBody is the JSON shape POSTed by the UI.
type createBody struct {
	Kind           string `json:"kind"`
	Label          string `json:"label"`
	Endpoint       string `json:"endpoint"`
	Region         string `json:"region"`
	Bucket         string `json:"bucket"`
	PathPrefix     string `json:"path_prefix"`
	AccessKeyID    string `json:"access_key_id"`
	SecretKey      string `json:"secret_key"`
	ForcePathStyle bool   `json:"force_path_style"`
	IsDefault      bool   `json:"is_default"`
}

// updateBody mirrors createBody but each field is optional. We pointer-wrap
// the strings so the handler can tell "omitted" from "set to empty".
type updateBody struct {
	Label          *string `json:"label"`
	Endpoint       *string `json:"endpoint"`
	Region         *string `json:"region"`
	Bucket         *string `json:"bucket"`
	PathPrefix     *string `json:"path_prefix"`
	AccessKeyID    *string `json:"access_key_id"`
	SecretKey      *string `json:"secret_key"`
	ForcePathStyle *bool   `json:"force_path_style"`
	IsDefault      *bool   `json:"is_default"`
}

// testBody mirrors createBody minus the side-effect fields (label / is_default
// don't influence whether the credentials work).
type testBody struct {
	Kind           string `json:"kind"`
	Endpoint       string `json:"endpoint"`
	Region         string `json:"region"`
	Bucket         string `json:"bucket"`
	PathPrefix     string `json:"path_prefix"`
	AccessKeyID    string `json:"access_key_id"`
	SecretKey      string `json:"secret_key"`
	ForcePathStyle bool   `json:"force_path_style"`
}

func (h *Handler) list(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, err := uuid.Parse(c.Param("siteId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_site_id", "siteId is not a valid UUID"))
		return
	}
	rows, err := h.svc.ListBySite(c.Request.Context(), p.TenantID, siteID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	items := make([]destinationDTO, 0, len(rows))
	for _, r := range rows {
		items = append(items, toDTO(r))
	}
	c.JSON(http.StatusOK, destinationListDTO{Items: items})
}

func (h *Handler) get(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	id, ok := h.parseDestID(c)
	if !ok {
		return
	}
	d, err := h.svc.GetByID(c.Request.Context(), p.TenantID, id)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	if !destBelongsToPathSite(c, d.SiteID) {
		return
	}
	c.JSON(http.StatusOK, toDTO(d))
}

// destBelongsToPathSite verifies the resolved destination belongs to the
// :siteId in the path (which RequireSiteAccess already authorized for the
// caller). Destinations are fetched by their own id, so without this a caller
// — especially a site-scoped collaborator — could read/mutate a destination on
// another site by passing its destinationId under their own allowed :siteId.
// Writes a 404 and returns false on mismatch (mirrors RLS hiding rows).
func destBelongsToPathSite(c *gin.Context, destSiteID uuid.UUID) bool {
	siteID, err := uuid.Parse(c.Param("siteId"))
	if err != nil || destSiteID != siteID {
		httpx.Error(c, domain.NotFound("destination_not_found", "destination not found"))
		return false
	}
	return true
}

func (h *Handler) create(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, err := uuid.Parse(c.Param("siteId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_site_id", "siteId is not a valid UUID"))
		return
	}
	var body createBody
	if err := bindJSON(c, &body); err != nil {
		httpx.Error(c, err)
		return
	}
	d, err := h.svc.Create(c.Request.Context(), CreateServiceInput{
		TenantID:       p.TenantID,
		SiteID:         siteID,
		Kind:           Kind(body.Kind),
		Label:          body.Label,
		Endpoint:       body.Endpoint,
		Region:         body.Region,
		Bucket:         body.Bucket,
		PathPrefix:     body.PathPrefix,
		AccessKeyID:    body.AccessKeyID,
		SecretKey:      body.SecretKey,
		ForcePathStyle: body.ForcePathStyle,
		IsDefault:      body.IsDefault,
	})
	if err != nil {
		httpx.Error(c, err)
		return
	}
	_, _ = h.audit.Record(c.Request.Context(), audit.Event{
		TenantID:   p.TenantID,
		ActorType:  actorType(p),
		ActorID:    p.ActorID(),
		Action:     "site_destination.create",
		TargetType: "site_destination",
		TargetID:   d.ID.String(),
		Metadata:   map[string]any{"kind": string(d.Kind), "site_id": d.SiteID.String(), "label": d.Label},
	})
	c.JSON(http.StatusCreated, toDTO(d))
}

func (h *Handler) update(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	id, ok := h.parseDestID(c)
	if !ok {
		return
	}
	var body updateBody
	if err := bindJSON(c, &body); err != nil {
		httpx.Error(c, err)
		return
	}
	// Verify the destination belongs to the path :siteId BEFORE mutating.
	if existing, gerr := h.svc.GetByID(c.Request.Context(), p.TenantID, id); gerr != nil {
		httpx.Error(c, gerr)
		return
	} else if !destBelongsToPathSite(c, existing.SiteID) {
		return
	}
	d, err := h.svc.Update(c.Request.Context(), p.TenantID, id, UpdateServiceInput{
		Label:          body.Label,
		Endpoint:       body.Endpoint,
		Region:         body.Region,
		Bucket:         body.Bucket,
		PathPrefix:     body.PathPrefix,
		AccessKeyID:    body.AccessKeyID,
		SecretKey:      body.SecretKey,
		ForcePathStyle: body.ForcePathStyle,
		IsDefault:      body.IsDefault,
	})
	if err != nil {
		httpx.Error(c, err)
		return
	}
	_, _ = h.audit.Record(c.Request.Context(), audit.Event{
		TenantID:   p.TenantID,
		ActorType:  actorType(p),
		ActorID:    p.ActorID(),
		Action:     "site_destination.update",
		TargetType: "site_destination",
		TargetID:   d.ID.String(),
	})
	c.JSON(http.StatusOK, toDTO(d))
}

func (h *Handler) delete(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	id, ok := h.parseDestID(c)
	if !ok {
		return
	}
	// Verify the destination belongs to the path :siteId BEFORE deleting.
	if existing, gerr := h.svc.GetByID(c.Request.Context(), p.TenantID, id); gerr != nil {
		httpx.Error(c, gerr)
		return
	} else if !destBelongsToPathSite(c, existing.SiteID) {
		return
	}
	if err := h.svc.Delete(c.Request.Context(), p.TenantID, id); err != nil {
		httpx.Error(c, err)
		return
	}
	_, _ = h.audit.Record(c.Request.Context(), audit.Event{
		TenantID:   p.TenantID,
		ActorType:  actorType(p),
		ActorID:    p.ActorID(),
		Action:     "site_destination.delete",
		TargetType: "site_destination",
		TargetID:   id.String(),
	})
	c.Status(http.StatusNoContent)
}

func (h *Handler) test(c *gin.Context) {
	// siteId is captured for audit symmetry even though the test itself is a
	// pure validation: the credentials might never reach the destination row.
	if _, err := uuid.Parse(c.Param("siteId")); err != nil {
		httpx.Error(c, domain.Validation("invalid_site_id", "siteId is not a valid UUID"))
		return
	}
	var body testBody
	if err := bindJSON(c, &body); err != nil {
		httpx.Error(c, err)
		return
	}
	res := h.svc.TestConnection(c.Request.Context(), TestConnectionInput{
		Kind:           Kind(body.Kind),
		Endpoint:       body.Endpoint,
		Region:         body.Region,
		Bucket:         body.Bucket,
		PathPrefix:     body.PathPrefix,
		AccessKeyID:    body.AccessKeyID,
		SecretKey:      body.SecretKey,
		ForcePathStyle: body.ForcePathStyle,
	})
	// Always 200; the body carries the success/failure flag.
	c.JSON(http.StatusOK, res)
}

func (h *Handler) parseDestID(c *gin.Context) (uuid.UUID, bool) {
	id, err := uuid.Parse(c.Param("destinationId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_destination_id", "destinationId is not a valid UUID"))
		return uuid.Nil, false
	}
	return id, true
}

// bindJSON wraps Gin's binding so an unparsable body always renders a stable
// 400 with a code the client UI can switch on. We don't use ShouldBindJSON
// directly because its default 400 leaks the parser's error verbatim.
func bindJSON(c *gin.Context, dst any) error {
	dec := json.NewDecoder(c.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return domain.Validation("invalid_body", "request body is not valid JSON")
	}
	return nil
}

func actorType(p domain.Principal) string {
	if p.Type == domain.PrincipalAPIKey {
		return audit.ActorAPIKey
	}
	return audit.ActorUser
}
