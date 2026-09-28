package settings

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/admingate"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// Handler serves the instance SMTP settings under /api/v1/settings/smtp.
//
// The smtp_settings row is one row for the whole install, so every route here
// (GET, PUT and POST /test) requires instance-level authority as decided by
// admingate.CanManageInstanceEmail: a superadmin, or the owner of the only live
// organisation on the install. A role inside an organisation does not qualify
// on its own. RequireOrgScope() additionally blocks site-scoped principals.
//
// Instance authority is not tied to an organisation, so internal/server mounts
// these routes on the authenticated group that does not require an active
// one: an operator with no membership anywhere must still reach them.
type Handler struct {
	svc   *Service
	audit *audit.Recorder
	gate  admingate.Store
}

// NewHandler builds the settings Handler. gate answers the instance-authority
// reads; a nil gate refuses every request.
func NewHandler(svc *Service, rec *audit.Recorder, gate admingate.Store) *Handler {
	return &Handler{svc: svc, audit: rec, gate: gate}
}

// Register mounts the SMTP settings routes on r, which must already require
// authentication. It need not require an active organisation.
func (h *Handler) Register(r *gin.RouterGroup) {
	g := r.Group("/settings/smtp", authz.RequireOrgScope(), requireInstanceAuthority(h.gate))
	g.GET("", h.get)
	g.PUT("", h.put)
	g.POST("/test", h.test)
}

// InstanceAuthorityRequiredCode is the error code of the single refusal every
// refusing path of this gate emits. The message is identical on every path too,
// so a refusal does not reveal which fact was missing.
const InstanceAuthorityRequiredCode = "instance_authority_required"

// requireInstanceAuthority refuses the request unless admingate grants
// instance-level authority. The decision, including its fail-closed handling
// of store errors, is admingate's; this only maps a refusal to one 403. The Me
// response's can_manage_instance_email calls the same function, so the
// dashboard is offered this page exactly when these routes would admit.
func requireInstanceAuthority(store admingate.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !admingate.CanManageInstanceEmail(c.Request.Context(), store) {
			httpx.Error(c, domain.Forbidden(InstanceAuthorityRequiredCode,
				"instance-level access is required to manage the SMTP relay"))
			c.Abort()
			return
		}
		c.Next()
	}
}

func (h *Handler) get(c *gin.Context) {
	out, err := h.svc.Get(c.Request.Context())
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

func (h *Handler) put(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	var body SMTPUpdate
	if err := c.ShouldBindJSON(&body); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
		return
	}
	out, err := h.svc.Update(c.Request.Context(), body, p.UserID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	h.recordUpdate(c.Request.Context(), p, out)
	c.JSON(http.StatusOK, out)
}

// AuditActionUpdate is the audit action every successful PUT records.
const AuditActionUpdate = "smtp.settings.update"

// recordUpdate records a successful PUT. Where the record goes depends on
// whether the caller has an active organisation:
//
//   - With one, it is appended to that organisation's hash-chained audit_log.
//   - Without one, it goes to system_audit_log with the nil tenant id.
//     audit_log cannot hold it, because its tenant_id references tenants and
//     a row against no organisation is refused by the database.
//     system_audit_log carries no such reference and is where every other
//     event that belongs to no organisation is recorded; the admin console's
//     system audit view reads it.
//
// Best-effort in both cases, as before: the relay row is already written, and
// a failed append is logged rather than reported as a failure of a change
// that did happen.
func (h *Handler) recordUpdate(ctx context.Context, p domain.Principal, out SMTPSettings) {
	meta := map[string]any{
		"enabled":  out.Enabled,
		"host":     out.Host,
		"tls_mode": out.TLSMode,
	}
	if p.TenantID == uuid.Nil {
		if h.svc != nil {
			h.svc.RecordInstanceEvent(ctx, p.UserID, AuditActionUpdate, meta)
		}
		return
	}
	if h.audit != nil {
		_, _ = h.audit.Record(ctx, audit.Event{
			TenantID:   p.TenantID,
			ActorType:  audit.ActorUser,
			ActorID:    p.ActorID(),
			Action:     AuditActionUpdate,
			TargetType: "smtp_settings",
			TargetID:   "instance",
			Metadata:   meta,
		})
	}
}

type testBody struct {
	ToAddress string `json:"to_address"`
}

type testResult struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

// test sends a test email through the stored config. Failures are returned as a
// 200 {ok:false, message} so the UI can show the scrubbed reason inline (the
// message never contains internal IPs/hostnames).
func (h *Handler) test(c *gin.Context) {
	var body testBody
	if err := c.ShouldBindJSON(&body); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
		return
	}
	if err := h.svc.SendTest(c.Request.Context(), body.ToAddress); err != nil {
		c.JSON(http.StatusOK, testResult{OK: false, Message: err.Error()})
		return
	}
	c.JSON(http.StatusOK, testResult{OK: true, Message: "Test email sent to " + body.ToAddress})
}
