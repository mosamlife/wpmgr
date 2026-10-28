package report

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// Handler serves the report management routes:
//
//	GET    /api/v1/clients/:clientId/report-schedule
//	PUT    /api/v1/clients/:clientId/report-schedule
//	POST   /api/v1/clients/:clientId/reports
//	GET    /api/v1/clients/:clientId/reports
//	GET    /api/v1/clients/:clientId/reports/:reportId
//	DELETE /api/v1/clients/:clientId/reports/:reportId
type Handler struct {
	svc   *Service
	audit *audit.Recorder
}

// NewHandler constructs the report handler.
func NewHandler(svc *Service, rec *audit.Recorder) *Handler {
	return &Handler{svc: svc, audit: rec}
}

// Register mounts all report routes on the authenticated /api/v1 group.
// Uses the same mounting pattern as client/handler.go:39-48.
func (h *Handler) Register(r *gin.RouterGroup) {
	g := r.Group("/clients", authz.RequireOrgScope())
	g.GET("/:clientId/report-schedule",
		authz.RequirePermission(authz.PermClientRead), h.getSchedule)
	g.PUT("/:clientId/report-schedule",
		authz.RequirePermission(authz.PermClientManage), h.putSchedule)
	g.POST("/:clientId/reports",
		authz.RequirePermission(authz.PermClientManage), h.generateReport)
	g.GET("/:clientId/reports",
		authz.RequirePermission(authz.PermClientRead), h.listReports)
	g.GET("/:clientId/reports/:reportId",
		authz.RequirePermission(authz.PermClientRead), h.getReport)
	g.DELETE("/:clientId/reports/:reportId",
		authz.RequirePermission(authz.PermClientManage), h.deleteReport)
}

// ---------------------------------------------------------------------------
// Wire DTOs (local; not from ogen — reports endpoints are not in the existing
// generated spec; hand-written Gin per the perf package pattern)
// ---------------------------------------------------------------------------

type scheduleResponseDTO struct {
	ClientID                 uuid.UUID    `json:"client_id"`
	Enabled                  bool         `json:"enabled"`
	Cadence                  string       `json:"cadence"`
	SendDay                  int          `json:"send_day"`
	SendHour                 int          `json:"send_hour"`
	Timezone                 string       `json:"timezone,omitempty"`
	Recipients               []string     `json:"recipients"`
	Sections                 SectionFlags `json:"sections"`
	IntroText                string       `json:"intro_text"`
	ClosingText              string       `json:"closing_text"`
	PoweredByRemoved         bool         `json:"powered_by_removed"`
	NextRunAt                *time.Time   `json:"next_run_at"`
	LastRunAt                *time.Time   `json:"last_run_at"`
	InstanceMailerConfigured bool         `json:"instance_mailer_configured"`
}

type scheduleUpdateBody struct {
	Enabled    bool     `json:"enabled"`
	Cadence    string   `json:"cadence"`
	SendDay    int      `json:"send_day"`
	SendHour   int      `json:"send_hour"`
	Recipients []string `json:"recipients"`
	// Sections is partial: a section left out keeps its stored value.
	Sections         *SectionFlagsPatch `json:"sections"`
	IntroText        string             `json:"intro_text"`
	ClosingText      string             `json:"closing_text"`
	PoweredByRemoved bool               `json:"powered_by_removed"`
}

type generateReportBody struct {
	PeriodStart *time.Time `json:"period_start"`
	PeriodEnd   *time.Time `json:"period_end"`
	// Sections optionally overrides the client's stored sections for this
	// one report; a section left out keeps the stored value.
	Sections *SectionFlagsPatch `json:"sections"`
	Notify   bool               `json:"notify"`
}

type reportDTO struct {
	ID          uuid.UUID  `json:"id"`
	ClientID    uuid.UUID  `json:"client_id"`
	ScheduleID  *uuid.UUID `json:"schedule_id,omitempty"`
	PeriodStart time.Time  `json:"period_start"`
	PeriodEnd   time.Time  `json:"period_end"`
	Status      string     `json:"status"`
	Error       string     `json:"error,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	// Only present when status == completed.
	HTMLURL string `json:"html_url,omitempty"`
	PDFURL  string `json:"pdf_url,omitempty"`
}

type reportListDTO struct {
	Items      []reportDTO `json:"items"`
	NextCursor string      `json:"next_cursor,omitempty"`
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (h *Handler) getSchedule(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}
	clientID, err := parseClientID(c)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	sched, _, err := h.svc.GetSchedule(c.Request.Context(), p.TenantID, clientID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, toScheduleDTO(sched, h.svc.MailerConfigured(c.Request.Context())))
}

func (h *Handler) putSchedule(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}
	clientID, err := parseClientID(c)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	var body scheduleUpdateBody
	if err := c.ShouldBindJSON(&body); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
		return
	}
	var sections SectionFlagsPatch
	if body.Sections != nil {
		sections = *body.Sections
	}
	sched, err := h.svc.UpsertSchedule(c.Request.Context(), UpsertScheduleInput{
		TenantID:         p.TenantID,
		ClientID:         clientID,
		Enabled:          body.Enabled,
		Cadence:          body.Cadence,
		SendDay:          body.SendDay,
		SendHour:         body.SendHour,
		Recipients:       body.Recipients,
		SectionsPatch:    sections,
		IntroText:        body.IntroText,
		ClosingText:      body.ClosingText,
		PoweredByRemoved: body.PoweredByRemoved,
	})
	if err != nil {
		httpx.Error(c, err)
		return
	}
	h.record(c, p.TenantID, audit.ActionClientReportScheduleUpdated, clientID.String(),
		map[string]any{"enabled": sched.Enabled, "cadence": sched.Cadence})
	c.JSON(http.StatusOK, toScheduleDTO(sched, h.svc.MailerConfigured(c.Request.Context())))
}

func (h *Handler) generateReport(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}
	clientID, err := parseClientID(c)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	var body generateReportBody
	if err := c.ShouldBindJSON(&body); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
		return
	}
	in := GenerateNowInput{
		Notify: body.Notify,
	}
	if body.PeriodStart != nil {
		in.PeriodStart = *body.PeriodStart
	}
	if body.PeriodEnd != nil {
		in.PeriodEnd = *body.PeriodEnd
	}
	in.Sections = body.Sections
	rpt, err := h.svc.GenerateNow(c.Request.Context(), p.TenantID, clientID, in)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	h.record(c, p.TenantID, audit.ActionClientReportGenerated, rpt.ID.String(),
		map[string]any{"client_id": clientID})
	c.JSON(http.StatusAccepted, toReportDTO(rpt, "", ""))
}

func (h *Handler) listReports(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}
	clientID, err := parseClientID(c)
	if err != nil {
		httpx.Error(c, err)
		return
	}

	in := ListReportsInput{
		TenantID: p.TenantID,
		ClientID: clientID,
		Limit:    20,
	}
	if cursor := c.Query("cursor"); cursor != "" {
		t, id, cerr := DecodeCursor(cursor)
		if cerr == nil {
			in.CursorCreatedAt = &t
			in.CursorID = &id
		}
	}
	if limitStr := c.Query("limit"); limitStr != "" {
		var n int32
		if _, err := parseIntParam32(limitStr, &n, 1, 100); err == nil {
			in.Limit = n
		}
	}
	result, err := h.svc.ListReports(c.Request.Context(), in)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	items := make([]reportDTO, 0, len(result.Items))
	for _, r := range result.Items {
		// The web list renders download links directly from this response, so
		// completed rows must carry presigned URLs (presigning is local SigV4
		// signing — no storage round trip per item).
		htmlURL, pdfURL := h.svc.PresignReportURLs(c.Request.Context(), r)
		items = append(items, toReportDTO(r, htmlURL, pdfURL))
	}
	c.JSON(http.StatusOK, reportListDTO{Items: items, NextCursor: result.NextCursor})
}

func (h *Handler) getReport(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}
	clientID, err := parseClientID(c)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	reportID, err := parseReportID(c)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	rpt, htmlURL, pdfURL, err := h.svc.GetReportWithURLs(c.Request.Context(), p.TenantID, clientID, reportID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, toReportDTO(rpt, htmlURL, pdfURL))
}

func (h *Handler) deleteReport(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}
	clientID, err := parseClientID(c)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	reportID, err := parseReportID(c)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	if err := h.svc.DeleteReport(c.Request.Context(), p.TenantID, clientID, reportID); err != nil {
		httpx.Error(c, err)
		return
	}
	h.record(c, p.TenantID, audit.ActionClientReportDeleted, reportID.String(),
		map[string]any{"client_id": clientID})
	c.Status(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// DTO mapping
// ---------------------------------------------------------------------------

func toScheduleDTO(s Schedule, mailerConfigured bool) scheduleResponseDTO {
	return scheduleResponseDTO{
		ClientID:                 s.ClientID,
		Enabled:                  s.Enabled,
		Cadence:                  s.Cadence,
		SendDay:                  s.SendDay,
		SendHour:                 s.SendHour,
		Recipients:               s.Recipients,
		Sections:                 s.Sections,
		IntroText:                s.IntroText,
		ClosingText:              s.ClosingText,
		PoweredByRemoved:         s.PoweredByRemoved,
		NextRunAt:                s.NextRunAt,
		LastRunAt:                s.LastRunAt,
		InstanceMailerConfigured: mailerConfigured,
	}
}

func toReportDTO(r GeneratedReport, htmlURL, pdfURL string) reportDTO {
	d := reportDTO{
		ID:          r.ID,
		ClientID:    r.ClientID,
		ScheduleID:  r.ScheduleID,
		PeriodStart: r.PeriodStart,
		PeriodEnd:   r.PeriodEnd,
		Status:      r.Status,
		Error:       r.Error,
		CreatedAt:   r.CreatedAt,
		CompletedAt: r.CompletedAt,
		HTMLURL:     htmlURL,
		PDFURL:      pdfURL,
	}
	return d
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func parseClientID(c *gin.Context) (uuid.UUID, error) {
	id, err := uuid.Parse(c.Param("clientId"))
	if err != nil {
		return uuid.Nil, domain.Validation("invalid_client_id", "clientId is not a valid UUID")
	}
	return id, nil
}

func parseReportID(c *gin.Context) (uuid.UUID, error) {
	id, err := uuid.Parse(c.Param("reportId"))
	if err != nil {
		return uuid.Nil, domain.Validation("invalid_report_id", "reportId is not a valid UUID")
	}
	return id, nil
}

func parseIntParam32(s string, n *int32, min, max int32) (int32, error) {
	var v int32
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, domain.Validation("invalid_int", "not a valid integer")
		}
		v = v*10 + int32(c-'0')
	}
	if v < min {
		v = min
	}
	if v > max {
		v = max
	}
	*n = v
	return v, nil
}

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
		TargetType: "report",
		TargetID:   targetID,
		Metadata:   meta,
	})
}
