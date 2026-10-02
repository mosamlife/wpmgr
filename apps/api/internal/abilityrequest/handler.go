package abilityrequest

import (
	"bytes"
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
	// The organisation-wide queue (GH #828). Row security narrows a site
	// collaborator to their own sites.
	r.GET("/ai/ability-requests", authz.RequirePermission(authz.PermSiteContentEdit), h.listForOrg)
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
	c.JSON(http.StatusOK, h.rowDTO(c, p, row))
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
	// UndoOffered is whether POST .../undo would start an undo now: a done
	// row's undo inside its window, or the recovery undo of a draft a failed
	// or given-up write left on the site (GH #826).
	UndoOffered bool `json:"undo_offered"`
	// ResolveGaveUp is true once WPMgr stopped checking the site for the
	// outcome of a write whose reply was lost (GH #825): the result is final
	// and the person should look at the site's drafts.
	ResolveGaveUp bool `json:"resolve_gave_up"`
	// RouteID, RouteSHA256 and CardFacts are a wpmgr/rest-write request's
	// reviewed route, its approved hash and its structured card (null for
	// every other ability). CardFacts is the object the MCP layer built at
	// creation: every site string in it is under a from_the_site member,
	// cleaned and capped. It is passed through as stored, never added to.
	RouteID     *string         `json:"route_id"`
	RouteSHA256 *string         `json:"route_sha256"`
	CardFacts   json.RawMessage `json:"card_facts"`
}

// cardFactsJSON is the stored card as a JSON object, or null when the row
// has none or holds anything but an object.
func cardFactsJSON(b []byte) json.RawMessage {
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
		return json.RawMessage("null")
	}
	return append(json.RawMessage(nil), trimmed...)
}

// ListResponse is a page of the queue.
type ListResponse struct {
	Requests []RequestDTO `json:"requests"`
	Limit    int32        `json:"limit"`
	Offset   int32        `json:"offset"`
}

// OrgListResponse is a page of the organisation-wide queue with the badge.
type OrgListResponse struct {
	Requests     []RequestDTO `json:"requests"`
	PendingCount int64        `json:"pending_count"`
	Limit        int32        `json:"limit"`
	Offset       int32        `json:"offset"`
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

func toDTO(r sqlc.AssistantAbilityRequest, withDigest bool, agentVersion string) RequestDTO {
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
		UndoOffered:        UndoOffered(r, agentVersion, time.Now()),
		ResolveGaveUp:      resolveGaveUp(r),
		RouteID:            r.RouteID,
		RouteSHA256:        r.RouteSha256,
		CardFacts:          cardFactsJSON(r.CardFacts),
	}
	if withDigest {
		d := r.PresentedDigest
		out.PresentedDigest = &d
	}
	return out
}

// rowDTO is one row's wire form after a mutation, with its site's agent
// version when the row could offer a recovery undo.
func (h *Handler) rowDTO(c *gin.Context, p domain.Principal, row sqlc.AssistantAbilityRequest) RequestDTO {
	rows := []sqlc.AssistantAbilityRequest{row}
	return toDTO(row, true, h.svc.AgentVersions(c.Request.Context(), p, rows)[row.SiteID])
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
	versions := h.svc.AgentVersions(c.Request.Context(), p, rows)
	for _, r := range rows {
		out.Requests = append(out.Requests, toDTO(r, withDigest, versions[r.SiteID]))
	}
	c.JSON(http.StatusOK, out)
}

// resolveGaveUp: the ledger window closed with no answer. The worker's
// give-up records outcome 'outcome_unknown' on a row still in state
// 'outcome_unknown'; while resolving, outcome is NULL.
func resolveGaveUp(r sqlc.AssistantAbilityRequest) bool {
	return r.State == "outcome_unknown" && r.Outcome != nil && *r.Outcome == OutcomeUnknown
}

func (h *Handler) listForOrg(c *gin.Context) {
	p, ok := principal(c)
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
	var state *string
	if v := c.Query("state"); v != "" {
		if _, known := requestStates[v]; !known {
			httpx.Error(c, domain.Validation("invalid_state", "state is not a known request state"))
			return
		}
		state = &v
	}
	q, err := h.svc.ListOrg(c.Request.Context(), p, state, limit, offset)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	out := OrgListResponse{Requests: make([]RequestDTO, 0, len(q.Requests)), PendingCount: q.PendingCount, Limit: limit, Offset: offset}
	withDigest := p.Type == domain.PrincipalUser
	versions := h.svc.AgentVersions(c.Request.Context(), p, q.Requests)
	for _, r := range q.Requests {
		out.Requests = append(out.Requests, toDTO(r, withDigest, versions[r.SiteID]))
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
	c.JSON(http.StatusOK, h.rowDTO(c, p, row))
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
	c.JSON(http.StatusOK, h.rowDTO(c, p, row))
}
