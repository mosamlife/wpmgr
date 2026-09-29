package settings

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/admingate"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// smtpService is the part of *Service the handler calls.
type smtpService interface {
	Get(ctx context.Context) (SMTPSettings, error)
	Update(ctx context.Context, in SMTPUpdate, updatedBy uuid.UUID) (SMTPSettings, error)
	SendTest(ctx context.Context, to string) error
	RecordInstanceEvent(ctx context.Context, actorID uuid.UUID, action string, meta map[string]any)
}

// tenantAuditRecorder is the part of *audit.Recorder the handler calls.
type tenantAuditRecorder interface {
	Record(ctx context.Context, e audit.Event) (audit.Entry, error)
}

// Handler serves the instance SMTP settings under /api/v1/settings/smtp.
//
// The smtp_settings row is one row for the whole install, so every route here
// (GET, PUT and POST /test) requires instance-level authority as decided by
// admingate.CanManageInstanceEmail: a superadmin, the owner of the only live
// organisation on the install, or, on a self-hosted install, the account that
// set the install up. A role inside an organisation does not qualify on its
// own. RequireOrgScope() additionally blocks site-scoped principals.
//
// Instance authority is not tied to an organisation, so internal/server mounts
// these routes on the authenticated group that does not require an active
// one: an operator with no membership anywhere must still reach them.
type Handler struct {
	svc   smtpService
	audit tenantAuditRecorder
	gate  admingate.InstanceEmailStore
	log   *slog.Logger
}

// NewHandler builds the settings Handler. gate answers the instance-authority
// reads; a nil gate refuses every request.
func NewHandler(svc *Service, rec *audit.Recorder, gate admingate.InstanceEmailStore) *Handler {
	h := &Handler{gate: gate, log: slog.Default()}
	// Assigned only when non-nil, so a nil pointer never becomes a non-nil
	// interface value that the nil checks below would call through.
	if svc != nil {
		h.svc = svc
		h.log = svc.log
	}
	if rec != nil {
		h.audit = rec
	}
	return h
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

// instanceAuthorityKey is the gin context key under which the gate leaves the
// admitting decision for the handlers behind it.
const instanceAuthorityKey = "settings.instance_authority"

func refuseInstanceAuthority(c *gin.Context) {
	httpx.Error(c, domain.Forbidden(InstanceAuthorityRequiredCode,
		"instance-level access is required to manage the SMTP relay"))
	c.Abort()
}

// requireInstanceAuthority refuses the request unless admingate grants
// instance-level authority. The decision, including its fail-closed handling
// of store errors, is admingate's; this only maps a refusal to one 403 and
// hands the admitting decision to the handler, which routes the audit record
// by it. The Me response's can_manage_instance_email reads the same decision,
// so the dashboard is offered this page exactly when these routes would admit.
func requireInstanceAuthority(store admingate.InstanceEmailStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		a := admingate.InstanceEmailAuthority(c.Request.Context(), store)
		if !a.Admitted() {
			refuseInstanceAuthority(c)
			return
		}
		c.Set(instanceAuthorityKey, a)
		c.Next()
	}
}

// admittingAuthority returns the decision requireInstanceAuthority left on c.
// ok is false when there is none, which means the handler was reached without
// the gate in front of it.
func admittingAuthority(c *gin.Context) (admingate.Authority, bool) {
	v, ok := c.Get(instanceAuthorityKey)
	if !ok {
		return admingate.Authority{}, false
	}
	a, ok := v.(admingate.Authority)
	return a, ok && a.Admitted()
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
	// The audit destination is decided by how the caller was admitted, so a
	// write with no admitting decision is refused rather than left unrecorded.
	authority, ok := admittingAuthority(c)
	if !ok {
		refuseInstanceAuthority(c)
		return
	}
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
	h.recordUpdate(c.Request.Context(), p, authority, out)
	c.JSON(http.StatusOK, out)
}

// AuditActionUpdate is the audit action every successful PUT records.
const AuditActionUpdate = "smtp.settings.update"

// auditAppendTimeout bounds the audit appends of one successful PUT, together.
//
// The appends run after the relay row has committed, on a context detached
// from the request's cancellation (see recordUpdate), so a client that
// disconnects after the save still leaves the change recorded. Detached alone
// would be unbounded: a wedged database would hold this handler for as long as
// the socket took to fail. The bound has to sit between two things:
//
//   - The healthy path is two short transactions, about a dozen round trips
//     in all counting the pool's acquire-time ping, each under 10 ms on an
//     established pooled connection (the measurement db.SessionCleanupTimeout
//     is sized from). On top of that sits any wait for a pool connection and,
//     for the organisation append, for that organisation's audit chain lock
//     behind a concurrent append. 5 s is over forty times the healthy path's
//     ceiling.
//   - The default graceful-shutdown budget is 15 s. 5 s stays well inside it,
//     so these appends cannot be what makes a drain overrun.
const auditAppendTimeout = 5 * time.Second

// recordUpdate records a successful PUT. The relay belongs to the install, so
// its trail does too:
//
//   - Every admitted PUT is recorded in system_audit_log, the instance trail,
//     whichever arm admitted the caller and whatever organisation the request
//     had active.
//   - When the caller was admitted as the owner of the only live organisation,
//     or as the install owner, the change is also appended to the audit_log of
//     the organisation the admitting decision named, so its owner reads it
//     where they read everything else. That organisation is never the
//     request's active one. For the install owner it is the organisation
//     created with them at bootstrap, and only while it is live and they
//     still own it; otherwise the decision names none and there is no copy.
//     The install-owner arm is exactly the case with no superadmin to read the
//     instance trail, which only the admin console shows. A superadmin's
//     change is recorded in the instance trail only.
//   - Both records carry admitted_as, the arm that admitted the caller, so the
//     instance trail tells a superadmin's change from an owner's.
//
// Best-effort in both cases: the relay row is already written, so a failed
// append is logged rather than reported as a failure of a change that did
// happen.
//
// Both appends run on a context that keeps the request's values but not its
// cancellation, bounded by auditAppendTimeout: the change is saved by the time
// this runs, so a client that disconnects now must not cost it its record.
func (h *Handler) recordUpdate(reqCtx context.Context, p domain.Principal, authority admingate.Authority, out SMTPSettings) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(reqCtx), auditAppendTimeout)
	defer cancel()
	meta := map[string]any{
		"enabled":     out.Enabled,
		"host":        out.Host,
		"tls_mode":    out.TLSMode,
		"admitted_as": authority.Arm.String(),
	}
	if h.svc != nil {
		h.svc.RecordInstanceEvent(ctx, p.UserID, AuditActionUpdate, meta)
	}
	if (authority.Arm != admingate.ArmSoleLiveTenantOwner && authority.Arm != admingate.ArmInstallOwner) ||
		authority.TenantID == uuid.Nil || h.audit == nil {
		return
	}
	if _, err := h.audit.Record(ctx, audit.Event{
		TenantID:   authority.TenantID,
		ActorType:  audit.ActorUser,
		ActorID:    p.ActorID(),
		Action:     AuditActionUpdate,
		TargetType: "smtp_settings",
		TargetID:   "instance",
		Metadata:   meta,
	}); err != nil {
		h.log.ErrorContext(ctx, "organisation audit record failed",
			slog.String("action", AuditActionUpdate),
			slog.String("tenant_id", authority.TenantID.String()),
			slog.String("actor_id", p.UserID.String()),
			slog.Any("error", err))
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
