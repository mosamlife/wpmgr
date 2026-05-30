package diagnostics

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// Handler serves the operator-facing diagnostics + errors routes under
// /api/v1/sites/{siteId}/...
//
//	GET    /diagnostics                  — latest payload per category + freshness
//	POST   /diagnostics/refresh          — enqueue on-demand agent push
//	GET    /errors                       — fingerprint-grouped php-error list
//	POST   /errors/{md5}/silence         — toggle silence
type Handler struct {
	svc   *Service
	audit *audit.Recorder
}

// NewHandler builds the operator handler.
func NewHandler(svc *Service, rec *audit.Recorder) *Handler {
	return &Handler{svc: svc, audit: rec}
}

// Register mounts the routes on the authenticated /api/v1 group.
func (h *Handler) Register(r *gin.RouterGroup) {
	g := r.Group("/sites/:siteId")
	g.GET("/diagnostics", authz.RequirePermission(authz.PermSiteRead), h.list)
	g.POST("/diagnostics/refresh", authz.RequirePermission(authz.PermSiteWrite), h.refresh)
	g.GET("/errors", authz.RequirePermission(authz.PermSiteRead), h.listErrors)
	g.POST("/errors/:md5/silence", authz.RequirePermission(authz.PermSiteWrite), h.silenceError)
}

// diagnosticsCardDTO is the per-card shape rendered by the UI. `payload` is
// the agent's raw JSON for the category; the UI parses it lazily so the
// shape can evolve without a schema change.
type diagnosticsCardDTO struct {
	Category    string          `json:"category"`
	Payload     json.RawMessage `json:"payload"`
	CollectedAt string          `json:"collected_at"`
	ReceivedAt  string          `json:"received_at"`
	Fresh       bool            `json:"fresh"`
}

type diagnosticsListDTO struct {
	Items []diagnosticsCardDTO `json:"items"`
}

func (h *Handler) list(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, err := uuid.Parse(c.Param("siteId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_site_id", "siteId is not a valid UUID"))
		return
	}
	rows, err := h.svc.LatestBySite(c.Request.Context(), p.TenantID, siteID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	cards := make([]diagnosticsCardDTO, 0, len(AllCategories()))
	freshThreshold := time.Now().Add(-26 * time.Hour) // a daily push, +2h tolerance
	for _, cat := range AllCategories() {
		row, ok := rows[cat]
		if !ok {
			cards = append(cards, diagnosticsCardDTO{
				Category: string(cat),
				Payload:  json.RawMessage(`null`),
				Fresh:    false,
			})
			continue
		}
		cards = append(cards, diagnosticsCardDTO{
			Category:    string(cat),
			Payload:     row.Payload,
			CollectedAt: row.CollectedAt.UTC().Format(time.RFC3339),
			ReceivedAt:  row.ReceivedAt.UTC().Format(time.RFC3339),
			Fresh:       row.CollectedAt.After(freshThreshold),
		})
	}
	c.JSON(http.StatusOK, diagnosticsListDTO{Items: cards})
}

func (h *Handler) refresh(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, err := uuid.Parse(c.Param("siteId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_site_id", "siteId is not a valid UUID"))
		return
	}
	if err := h.svc.RefreshAgent(c.Request.Context(), p.TenantID, siteID); err != nil {
		// Treat the unwired sentinel as a 503 the UI can render as
		// "on-demand refresh not yet wired".
		if errors.Is(err, errUnwired) || err.Error() == "diagnostics_refresh_unwired" {
			httpx.Error(c, domain.Unavailable("diagnostics_refresh_unwired", "on-demand diagnostics refresh is not yet wired"))
			return
		}
		httpx.Error(c, err)
		return
	}
	_, _ = h.audit.Record(c.Request.Context(), audit.Event{
		TenantID:   p.TenantID,
		ActorType:  actorType(p),
		ActorID:    p.ActorID(),
		Action:     "site_diagnostics.refresh",
		TargetType: "site",
		TargetID:   siteID.String(),
	})
	c.Status(http.StatusAccepted)
}

// errorFrameDTO is the per-frame shape in the backtrace array.
type errorFrameDTO struct {
	File     string `json:"file"`
	Line     int    `json:"line"`
	Function string `json:"function"`
}

type phpErrorDTO struct {
	ID              string          `json:"id"`
	MD5             string          `json:"md5"`
	Code            int             `json:"code"`
	Severity        string          `json:"severity"`
	Message         string          `json:"message"`
	File            string          `json:"file"`
	Line            int             `json:"line"`
	RequestPath     string          `json:"request_path"`
	FirstSeenAt     string          `json:"first_seen_at"`
	LastSeenAt      string          `json:"last_seen_at"`
	OccurrenceCount int64           `json:"occurrence_count"`
	Silenced        bool            `json:"silenced"`
	Backtrace       []errorFrameDTO `json:"backtrace"`
}

type phpErrorListDTO struct {
	Items []phpErrorDTO `json:"items"`
}

func toErrorDTO(e PHPError) phpErrorDTO {
	frames := make([]errorFrameDTO, 0, len(e.Backtrace))
	for _, f := range e.Backtrace {
		frames = append(frames, errorFrameDTO{
			File:     f.File,
			Line:     f.Line,
			Function: f.Function,
		})
	}
	return phpErrorDTO{
		ID:              e.ID.String(),
		MD5:             e.MD5,
		Code:            e.Code,
		Severity:        e.Severity,
		Message:         e.Message,
		File:            e.File,
		Line:            e.Line,
		RequestPath:     e.RequestPath,
		FirstSeenAt:     e.FirstSeenAt.UTC().Format(time.RFC3339),
		LastSeenAt:      e.LastSeenAt.UTC().Format(time.RFC3339),
		OccurrenceCount: e.OccurrenceCount,
		Silenced:        e.Silenced,
		Backtrace:       frames,
	}
}

func (h *Handler) listErrors(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, err := uuid.Parse(c.Param("siteId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_site_id", "siteId is not a valid UUID"))
		return
	}
	f := ListPHPErrorsFilter{Limit: 100}
	if s := c.Query("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			f.Limit = n
		}
	}
	if s := c.Query("since"); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			f.Since = t
		}
	}
	if s := c.Query("silenced"); s != "" {
		switch s {
		case "true", "1":
			b := true
			f.Silenced = &b
		case "false", "0":
			b := false
			f.Silenced = &b
		}
	}
	rows, err := h.svc.ListErrors(c.Request.Context(), p.TenantID, siteID, f)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	items := make([]phpErrorDTO, 0, len(rows))
	for _, r := range rows {
		items = append(items, toErrorDTO(r))
	}
	c.JSON(http.StatusOK, phpErrorListDTO{Items: items})
}

type silenceBody struct {
	Silenced bool `json:"silenced"`
}

func (h *Handler) silenceError(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, err := uuid.Parse(c.Param("siteId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_site_id", "siteId is not a valid UUID"))
		return
	}
	md5 := c.Param("md5")
	if md5 == "" {
		httpx.Error(c, domain.Validation("invalid_md5", "md5 fingerprint is required"))
		return
	}
	var body silenceBody
	if err := bindJSON(c, &body); err != nil {
		// Tolerate an empty body — default to "silence=true" so the UI's
		// one-click silence button can POST with no body.
		body.Silenced = true
	}
	if err := h.svc.SetSilenced(c.Request.Context(), p.TenantID, siteID, md5, body.Silenced); err != nil {
		httpx.Error(c, err)
		return
	}
	_, _ = h.audit.Record(c.Request.Context(), audit.Event{
		TenantID:   p.TenantID,
		ActorType:  actorType(p),
		ActorID:    p.ActorID(),
		Action:     "php_error.silence",
		TargetType: "php_error",
		TargetID:   md5,
		Metadata:   map[string]any{"site_id": siteID.String(), "silenced": body.Silenced},
	})
	c.Status(http.StatusNoContent)
}

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
