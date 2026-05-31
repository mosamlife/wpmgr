package activity

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// Handler serves the operator-facing activity routes under
// /api/v1/sites/{siteId}/...
//
//	GET /activity         — filtered, paginated event list (newest first)
//	GET /activity/verify  — server-side hash-chain re-verification result
type Handler struct {
	svc *Service
}

// NewHandler builds the operator handler.
func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// Register mounts the routes on the authenticated /api/v1 group.
func (h *Handler) Register(r *gin.RouterGroup) {
	// RequireSiteAccess("siteId") is applied on the group so every sub-route
	// inherits it. This enforces the site allowlist for site-scoped principals
	// (belt-and-braces in front of the RLS policy on agent_activity_log).
	g := r.Group("/sites/:siteId", authz.RequireSiteAccess("siteId"))
	g.GET("/activity", authz.RequirePermission(authz.PermSiteRead), h.list)
	g.GET("/activity/verify", authz.RequirePermission(authz.PermSiteRead), h.verify)
}

type activityEventDTO struct {
	ID          string         `json:"id"`
	Seq         int64          `json:"seq"`
	EventType   string         `json:"event_type"`
	ObjectType  string         `json:"object_type"`
	ObjectID    string         `json:"object_id"`
	ObjectLabel string         `json:"object_label"`
	ActorUserID int64          `json:"actor_user_id"`
	ActorLogin  string         `json:"actor_login"`
	ActorIP     string         `json:"actor_ip"`
	Summary     string         `json:"summary"`
	Meta        map[string]any `json:"meta"`
	Severity    string         `json:"severity"`
	PrevHash    string         `json:"prev_hash"`
	ThisHash    string         `json:"this_hash"`
	ChainValid  bool           `json:"chain_valid"`
	OccurredAt  string         `json:"occurred_at"`
	ReceivedAt  string         `json:"received_at"`
}

type activityListDTO struct {
	Items []activityEventDTO `json:"items"`
}

type activityVerifyDTO struct {
	Valid      bool   `json:"valid"`
	BreakAtSeq *int64 `json:"break_at_seq"`
	Total      int    `json:"total"`
}

func toEventDTO(e Event) activityEventDTO {
	meta := e.Meta
	if meta == nil {
		meta = map[string]any{}
	}
	return activityEventDTO{
		ID:          strconv.FormatInt(e.ID, 10),
		Seq:         e.Seq,
		EventType:   e.EventType,
		ObjectType:  e.ObjectType,
		ObjectID:    e.ObjectID,
		ObjectLabel: e.ObjectLabel,
		ActorUserID: e.ActorUserID,
		ActorLogin:  e.ActorLogin,
		ActorIP:     e.ActorIP,
		Summary:     e.Summary,
		Meta:        meta,
		Severity:    e.Severity,
		PrevHash:    e.PrevHash,
		ThisHash:    e.ThisHash,
		ChainValid:  e.ChainValid,
		OccurredAt:  e.OccurredAt.UTC().Format(time.RFC3339),
		ReceivedAt:  e.ReceivedAt.UTC().Format(time.RFC3339),
	}
}

func (h *Handler) list(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, err := uuid.Parse(c.Param("siteId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_site_id", "siteId is not a valid UUID"))
		return
	}
	f := ListFilter{
		EventType:  c.Query("event_type"),
		ObjectType: c.Query("object_type"),
		ActorLogin: c.Query("actor_login"),
		Severity:   c.Query("severity"),
		Limit:      100,
	}
	if s := c.Query("limit"); s != "" {
		if n, perr := strconv.Atoi(s); perr == nil {
			f.Limit = n
		}
	}
	if s := c.Query("offset"); s != "" {
		if n, perr := strconv.Atoi(s); perr == nil {
			f.Offset = n
		}
	}
	if s := c.Query("since"); s != "" {
		if t, perr := time.Parse(time.RFC3339, s); perr == nil {
			f.Since = t
		}
	}
	if s := c.Query("until"); s != "" {
		if t, perr := time.Parse(time.RFC3339, s); perr == nil {
			f.Until = t
		}
	}
	rows, err := h.svc.ListActivity(c.Request.Context(), p.TenantID, siteID, f)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	items := make([]activityEventDTO, 0, len(rows))
	for _, r := range rows {
		items = append(items, toEventDTO(r))
	}
	c.JSON(http.StatusOK, activityListDTO{Items: items})
}

func (h *Handler) verify(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, err := uuid.Parse(c.Param("siteId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_site_id", "siteId is not a valid UUID"))
		return
	}
	res, err := h.svc.Verify(c.Request.Context(), p.TenantID, siteID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, activityVerifyDTO{
		Valid:      res.Valid,
		BreakAtSeq: res.BreakAtSeq,
		Total:      res.Total,
	})
}
