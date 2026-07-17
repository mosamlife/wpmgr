package sitetag

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/api/gen"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// Handler serves the GH #230 "rich tags" tenant-level tag-registry routes:
//
//	GET    /api/v1/tags              — list the tag registry (PermSiteRead)
//	POST   /api/v1/tags              — create a tag (PermSiteWrite)
//	POST   /api/v1/tags/bulk-apply   — add/remove tags across many sites (PermSiteWrite)
//	PATCH  /api/v1/tags/:tagId       — rename/recolor/merge a tag (PermSiteWrite)
//	DELETE /api/v1/tags/:tagId       — delete a tag fleet-wide (PermSiteWrite)
//
// GET /tags and POST /tags/bulk-apply carry NO RequireOrgScope, mirroring the
// existing PUT /sites/{siteId}/tags route: a site-scoped collaborator with
// PermSiteRead/PermSiteWrite may read the tenant-wide registry (usage_count
// is deliberately visible to a scoped collaborator — it is only a COUNT,
// never the list of sites carrying a tag, so it discloses no site identity
// outside their grant) and bulk-apply is gated PER-SITE via
// Principal.CanAccessSite (the same by-id gate every by-id route uses) —
// a foreign/unauthorized site yields ok:false in the results, never a
// global 403 (mirrors internal/perf's bulk-purge/bulk-config).
//
// POST /tags, PATCH /tags/:tagId, and DELETE /tags/:tagId DO carry
// RequireOrgScope (m100 security-review follow-up, GH #230): these mutate
// the registry FLEET-WIDE (rename/recolor/delete rewrite sites.tags on every
// site in the tenant, including sites a site-scoped collaborator was never
// granted). Without this gate, a collaborator holding only PermSiteWrite on
// one granted site could rename/delete a tag and silently rewrite tags on
// sites outside their allowlist — a fleet-wide write side-channel. Create is
// gated the same way as a defense-in-depth match (a newly created tag has
// zero usage, so the risk is lower, but scoped collaborators have no
// legitimate reason to grow the tenant's tag vocabulary either).
type Handler struct {
	svc   *Service
	audit *audit.Recorder
}

// NewHandler constructs the tag-registry handler.
func NewHandler(svc *Service, rec *audit.Recorder) *Handler {
	return &Handler{svc: svc, audit: rec}
}

// Register mounts the tag-registry routes on the authenticated /api/v1 group.
func (h *Handler) Register(r *gin.RouterGroup) {
	g := r.Group("/tags")
	g.GET("", authz.RequirePermission(authz.PermSiteRead), h.list)
	g.POST("", authz.RequirePermission(authz.PermSiteWrite), authz.RequireOrgScope(), h.create)
	// /bulk-apply must be declared before /:tagId to avoid routing conflicts
	// (mirrors internal/client's /assignments-before-/:clientId convention).
	// No RequireOrgScope: bulk-apply is per-site-authorized (see comment above).
	g.POST("/bulk-apply", authz.RequirePermission(authz.PermSiteWrite), h.bulkApply)
	g.PATCH("/:tagId", authz.RequirePermission(authz.PermSiteWrite), authz.RequireOrgScope(), h.update)
	g.DELETE("/:tagId", authz.RequirePermission(authz.PermSiteWrite), authz.RequireOrgScope(), h.delete)
}

// ---------------------------------------------------------------------------
// Request DTOs
// ---------------------------------------------------------------------------

type createTagBody struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

type updateTagBody struct {
	Name  *string `json:"name"`
	Color *string `json:"color"`
	Merge *bool   `json:"merge"`
}

type bulkApplyBody struct {
	SiteIDs []string `json:"site_ids"`
	Add     []string `json:"add"`
	Remove  []string `json:"remove"`
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (h *Handler) list(c *gin.Context) {
	tenantID, ok := domain.TenantIDFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Forbidden("tenant_required", "a tenant context is required"))
		return
	}
	tags, err := h.svc.List(c.Request.Context(), tenantID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	items := make([]gen.SiteTag, 0, len(tags))
	for _, t := range tags {
		items = append(items, toAPI(t))
	}
	c.JSON(http.StatusOK, gen.SiteTagList{Items: items})
}

func (h *Handler) create(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}
	var body createTagBody
	if err := c.ShouldBindJSON(&body); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
		return
	}
	tag, err := h.svc.Create(c.Request.Context(), CreateInput{
		TenantID: p.TenantID,
		Name:     body.Name,
		Color:    body.Color,
	})
	if err != nil {
		httpx.Error(c, err)
		return
	}
	h.record(c, p.TenantID, audit.ActionTagCreate, tag.ID.String(), map[string]any{"name": tag.Name, "color": tag.Color})
	c.JSON(http.StatusCreated, toAPI(tag))
}

func (h *Handler) update(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}
	tagID, err := uuid.Parse(c.Param("tagId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_tag_id", "tagId is not a valid UUID"))
		return
	}
	var body updateTagBody
	if err := c.ShouldBindJSON(&body); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
		return
	}
	merge := false
	if body.Merge != nil {
		merge = *body.Merge
	}
	res, err := h.svc.Update(c.Request.Context(), UpdateInput{
		TenantID: p.TenantID,
		ID:       tagID,
		Name:     body.Name,
		Color:    body.Color,
		Merge:    merge,
	})
	if err != nil {
		httpx.Error(c, err)
		return
	}
	action := audit.ActionTagUpdate
	meta := map[string]any{"name": res.Tag.Name, "color": res.Tag.Color}
	if res.Merged {
		action = audit.ActionTagMerge
		meta["old_name"] = res.OldName
		meta["merged_into"] = res.MergedInto
	} else if res.OldName != "" && res.OldName != res.Tag.Name {
		meta["old_name"] = res.OldName
	}
	h.record(c, p.TenantID, action, res.Tag.ID.String(), meta)
	c.JSON(http.StatusOK, toAPI(res.Tag))
}

func (h *Handler) delete(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}
	tagID, err := uuid.Parse(c.Param("tagId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_tag_id", "tagId is not a valid UUID"))
		return
	}
	if err := h.svc.Delete(c.Request.Context(), p.TenantID, tagID); err != nil {
		httpx.Error(c, err)
		return
	}
	h.record(c, p.TenantID, audit.ActionTagDelete, tagID.String(), nil)
	c.Status(http.StatusNoContent)
}

func (h *Handler) bulkApply(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}
	var body bulkApplyBody
	if err := c.ShouldBindJSON(&body); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
		return
	}
	if len(body.SiteIDs) == 0 {
		httpx.Error(c, domain.Validation("site_ids_required", "site_ids must not be empty"))
		return
	}
	if len(body.SiteIDs) > maxBulkApplySites {
		httpx.Error(c, domain.Validation("too_many_sites", "site_ids must contain at most 200 entries per request"))
		return
	}
	// Validate the delta BEFORE per-site authorization filtering, so a
	// request with no effective delta is rejected outright rather than
	// silently succeeding with zero effect when every site_id also happens
	// to be unauthorized.
	add, remove, err := h.svc.ValidateDelta(body.Add, body.Remove)
	if err != nil {
		httpx.Error(c, err)
		return
	}

	results := make([]bulkTagResultDTO, 0, len(body.SiteIDs))
	authorized := make([]uuid.UUID, 0, len(body.SiteIDs))
	// Dedupe by parsed UUID (and by raw string for malformed entries) so a
	// repeated site_id produces exactly one result and is never applied/
	// counted twice.
	seen := make(map[uuid.UUID]bool, len(body.SiteIDs))
	seenInvalid := make(map[string]bool, len(body.SiteIDs))
	for _, raw := range body.SiteIDs {
		siteID, perr := uuid.Parse(raw)
		if perr != nil {
			if seenInvalid[raw] {
				continue
			}
			seenInvalid[raw] = true
			results = append(results, bulkTagResultDTO{SiteID: raw, OK: false, Detail: "invalid site id"})
			continue
		}
		if seen[siteID] {
			continue
		}
		seen[siteID] = true
		if !p.CanAccessSite(siteID) {
			results = append(results, bulkTagResultDTO{SiteID: raw, OK: false, Detail: "forbidden"})
			continue
		}
		authorized = append(authorized, siteID)
	}

	updated, err := h.svc.BulkApply(c.Request.Context(), p.TenantID, authorized, add, remove)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	appliedCount := 0
	for _, siteID := range authorized {
		if updated[siteID] {
			appliedCount++
			results = append(results, bulkTagResultDTO{SiteID: siteID.String(), OK: true, Detail: "applied"})
		} else {
			results = append(results, bulkTagResultDTO{SiteID: siteID.String(), OK: false, Detail: "site_not_found"})
		}
	}
	if len(authorized) > 0 {
		h.record(c, p.TenantID, audit.ActionTagBulkApply, "", map[string]any{
			"site_count":    len(authorized),
			"applied_count": appliedCount,
			"add":           add,
			"remove":        remove,
		})
	}
	c.JSON(http.StatusOK, gin.H{"results": results})
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func (h *Handler) record(c *gin.Context, tenantID uuid.UUID, action, targetID string, meta map[string]any) {
	if h.audit == nil {
		return
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
		TargetType: "tag",
		TargetID:   targetID,
		Metadata:   meta,
	})
}
