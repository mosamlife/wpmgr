package abilityrequest

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// Handler serves the ability request queue and its decisions.
type Handler struct{ svc *Service }

// NewHandler builds the handler.
func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// Register mounts, on the authenticated /api/v1 group:
//
//	GET  /sites/:siteId/ai/ability-requests
//	POST /sites/:siteId/ai/ability-requests/:requestId/approve
//	POST /sites/:siteId/ai/ability-requests/:requestId/decline
//
// Route middleware requires site.content.edit and site access; the service
// re-checks the row's own operator_permission (W1). Decisions take JSON only
// (the CSRF guard for a cookie-authenticated change).
func (h *Handler) Register(r *gin.RouterGroup) {
	g := r.Group("/sites/:siteId", authz.RequireSiteAccess("siteId"))
	g.GET("/ai/ability-requests", authz.RequirePermission(authz.PermSiteContentEdit), h.listForSite)
	g.POST("/ai/ability-requests/:requestId/approve",
		authz.RequirePermission(authz.PermSiteContentEdit), httpx.RequireJSONBody(), h.approve)
	g.POST("/ai/ability-requests/:requestId/decline",
		authz.RequirePermission(authz.PermSiteContentEdit), httpx.RequireJSONBody(), h.decline)
	g.GET("/ai/content-editing", authz.RequirePermission(authz.PermSiteContentRead), h.getContentEditing)
	g.POST("/ai/content-editing/enable",
		authz.RequirePermission(authz.PermSiteContentEdit), httpx.RequireJSONBody(), h.enableContentEditing)
	g.POST("/ai/ability-requests/:requestId/undo",
		authz.RequirePermission(authz.PermSiteContentEdit), httpx.RequireJSONBody(), h.undo)
}

func (h *Handler) undo(c *gin.Context) {
	p, ok := principal(c)
	if !ok {
		return
	}
	if err := requireSession(p); err != nil {
		httpx.Error(c, err)
		return
	}
	siteID, ok := parseID(c, "siteId")
	if !ok {
		return
	}
	requestID, ok := parseID(c, "requestId")
	if !ok {
		return
	}
	row, err := h.svc.Undo(c.Request.Context(), p, siteID, requestID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, toDTO(row, true))
}

// RequestDTO is one ability request on the wire. site_label, site_host,
// grant_label and title_excerpt are text to render as text nodes. input_json
// is the exact input the AI chose, which the card shows in full.
type RequestDTO struct {
	ID                 uuid.UUID  `json:"id"`
	SiteID             uuid.UUID  `json:"site_id"`
	AbilityName        string     `json:"ability_name"`
	InputJSON          string     `json:"input_json"`
	TitleExcerpt       *string    `json:"title_excerpt"`
	Editor             *string    `json:"editor"`
	PostType           *string    `json:"post_type"`
	EffectCopy         string     `json:"effect_copy"`
	Snapshot           string     `json:"snapshot"`
	SiteLabel          string     `json:"site_label"`
	SiteHost           string     `json:"site_host"`
	GrantLabel         string     `json:"grant_label"`
	GrantVia           string     `json:"grant_via"`
	SetupClient        *string    `json:"setup_client"`
	CardCopyVersion    int32      `json:"card_copy_version"`
	PresentedDigest    *string    `json:"presented_digest,omitempty"`
	State              string     `json:"state"`
	CreatedAt          time.Time  `json:"created_at"`
	ExpiresAt          time.Time  `json:"expires_at"`
	DecidedAt          *time.Time `json:"decided_at"`
	Outcome            *string    `json:"outcome"`
	OutcomeCode        *string    `json:"outcome_code"`
	NotSentReason      *string    `json:"not_sent_reason"`
	CreatedPostID      *int64     `json:"created_post_id"`
	Trashed            *bool      `json:"trashed"`
	UndoState          *string    `json:"undo_state"`
	UndoAvailableUntil *time.Time `json:"undo_available_until"`
}

// ListResponse is a page of the queue.
type ListResponse struct {
	Requests []RequestDTO `json:"requests"`
	Limit    int32        `json:"limit"`
	Offset   int32        `json:"offset"`
}

// ApproveBody is the approve request body.
type ApproveBody struct {
	PresentedDigest string `json:"presented_digest"`
}

func ts(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}

func toDTO(r sqlc.AssistantAbilityRequest, withDigest bool) RequestDTO {
	out := RequestDTO{
		ID: r.ID, SiteID: r.SiteID, AbilityName: r.AbilityName, InputJSON: r.InputJson,
		TitleExcerpt: r.TitleExcerpt, Editor: r.Editor, PostType: r.PostType,
		EffectCopy: r.EffectCopy, Snapshot: r.Snapshot,
		SiteLabel: r.SiteLabel, SiteHost: r.SiteHost, GrantLabel: r.GrantLabel, GrantVia: r.GrantVia,
		SetupClient: r.SetupClient, CardCopyVersion: r.CardCopyVersion, State: r.State,
		CreatedAt: r.CreatedAt, ExpiresAt: r.ExpiresAt, DecidedAt: ts(r.DecidedAt),
		Outcome: r.Outcome, OutcomeCode: r.OutcomeCode, NotSentReason: r.NotSentReason,
		CreatedPostID: r.CreatedPostID, Trashed: r.Trashed, UndoState: r.UndoState,
		UndoAvailableUntil: ts(r.UndoAvailableUntil),
	}
	if withDigest {
		d := r.PresentedDigest
		out.PresentedDigest = &d
	}
	return out
}

func principal(c *gin.Context) (domain.Principal, bool) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
	}
	return p, ok
}

func parseID(c *gin.Context, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(c.Param(name))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_"+name, name+" is not a valid UUID"))
		return uuid.Nil, false
	}
	return id, true
}

func (h *Handler) listForSite(c *gin.Context) {
	p, ok := principal(c)
	if !ok {
		return
	}
	siteID, ok := parseID(c, "siteId")
	if !ok {
		return
	}
	limit, offset := int32(50), int32(0)
	if n, err := strconv.Atoi(c.Query("limit")); err == nil && n > 0 && n <= 100 {
		limit = int32(n)
	}
	if n, err := strconv.Atoi(c.Query("offset")); err == nil && n >= 0 && n <= 1_000_000 {
		offset = int32(n)
	}
	rows, err := h.svc.List(c.Request.Context(), p, &siteID, limit, offset)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	out := ListResponse{Requests: make([]RequestDTO, 0, len(rows)), Limit: limit, Offset: offset}
	withDigest := p.Type == domain.PrincipalUser
	for _, r := range rows {
		out.Requests = append(out.Requests, toDTO(r, withDigest))
	}
	c.JSON(http.StatusOK, out)
}

func (h *Handler) approve(c *gin.Context) {
	p, ok := principal(c)
	if !ok {
		return
	}
	if err := requireSession(p); err != nil {
		httpx.Error(c, err)
		return
	}
	siteID, ok := parseID(c, "siteId")
	if !ok {
		return
	}
	requestID, ok := parseID(c, "requestId")
	if !ok {
		return
	}
	var body ApproveBody
	if err := json.NewDecoder(io.LimitReader(c.Request.Body, 4<<10)).Decode(&body); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
		return
	}
	row, err := h.svc.Approve(c.Request.Context(), p, siteID, requestID, body.PresentedDigest)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, toDTO(row, true))
}

func (h *Handler) decline(c *gin.Context) {
	p, ok := principal(c)
	if !ok {
		return
	}
	if err := requireSession(p); err != nil {
		httpx.Error(c, err)
		return
	}
	siteID, ok := parseID(c, "siteId")
	if !ok {
		return
	}
	requestID, ok := parseID(c, "requestId")
	if !ok {
		return
	}
	row, err := h.svc.Decline(c.Request.Context(), p, siteID, requestID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, toDTO(row, true))
}
