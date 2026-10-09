package admin

// vuln_feed_handler.go — HTTP handlers for the vulnerability-feed API key
// configuration. Routes live under /api/v1/admin/vuln-feed/ and are gated by
// admingate.InstanceEmailAuthority, the decision behind the instance email
// settings (see Register in handler.go): a superadmin, the owner of the only
// live organisation, or, on a self-hosted install, the install owner.
//
// Endpoints:
//   GET    /admin/vuln-feed/status   — masked status; NEVER returns the key
//   PUT    /admin/vuln-feed/key      — set key (plaintext in body over TLS)
//   DELETE /admin/vuln-feed/key      — clear UI-stored key (falls back to env)
//   POST   /admin/vuln-feed/sync     — enqueue immediate feed refresh

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/admingate"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// Audit actions and target of the vuln-feed key routes. The names predate the
// widened gate and are kept, so existing readers of the trail keep matching.
const (
	auditActionVulnFeedKeySet   = "admin.vuln_feed.key.set"
	auditActionVulnFeedKeyClear = "admin.vuln_feed.key.clear"
	auditActionVulnFeedSync     = "admin.vuln_feed.sync"
	vulnFeedAuditTargetType     = "instance_setting"
)

// vulnFeedAuditTimeout bounds the audit appends of one vuln-feed change,
// together. The appends run after the change, on a context detached from the
// request's cancellation, so a client that disconnects after the save still
// leaves the change recorded; the bound keeps a wedged database from holding
// the handler. It is the instance SMTP settings' bound, for the same reasons.
const vulnFeedAuditTimeout = 5 * time.Second

// VulnFeedMetaReader is a narrow interface for reading the feed freshness row.
// Implemented in main by an adapter that wraps *vuln.Repo.
type VulnFeedMetaReader interface {
	// GetFeedMetaStatus returns the condensed feed meta needed by the status
	// endpoint. Returns zero values (ok=false, recordCount=0, lastSynced=nil,
	// lastError="", enrichmentOK=false, lastEnrichmentAt=nil) with nil error
	// when the meta row has never been written.
	//
	// ok/recordCount/lastSynced track Scanner-driven DETECTION freshness;
	// enrichmentOK/lastEnrichmentAt separately track Production-driven CVSS
	// enrichment health (kept independent so a Production hiccup never masks
	// as a whole-feed outage — see vuln.FeedMeta doc).
	GetFeedMetaStatus(ctx context.Context) (ok bool, recordCount int, lastSynced *time.Time, lastError string, enrichmentOK bool, lastEnrichmentAt *time.Time, err error)
}

// vulnFeedAdminHandler groups the vuln-feed admin sub-handler dependencies.
type vulnFeedAdminHandler struct {
	meta   VulnFeedMetaReader // may be nil if vuln domain is not fully wired
	keySvc *VulnFeedKeyService
	// gate answers the instance-email decision for these routes. A nil gate
	// refuses every request.
	gate admingate.InstanceEmailStore
}

// SetVulnFeed wires the vuln-feed sub-handler into the admin Handler. It must
// be called before the first Register call (i.e. at boot, before the HTTP
// server starts) so that Register can conditionally mount the routes.
//
// gate is the store behind the routes' gate. It must be the same
// InstanceEmailStore the instance SMTP settings routes and the Me capability
// can_manage_instance_email are built with, so all three give one answer.
func (h *Handler) SetVulnFeed(meta VulnFeedMetaReader, keySvc *VulnFeedKeyService, gate admingate.InstanceEmailStore) {
	h.vulnFeedH = &vulnFeedAdminHandler{meta: meta, keySvc: keySvc, gate: gate}
}

// recordVulnFeedEvent records one admitted vuln-feed change. The key is
// install-wide, so its trail is routed the way the instance SMTP settings route
// theirs:
//
//   - Every change is recorded in system_audit_log, the instance trail,
//     whichever arm admitted the caller.
//   - A caller admitted as the owner of the only live organisation, or as the
//     install owner, is also recorded in the audit_log of the organisation the
//     admitting decision named, never the request's active one, so an owner
//     reads the change where they read everything else. A superadmin's change
//     is recorded in the instance trail only.
//   - Both records carry admitted_as, the arm that admitted the caller.
//     Neither carries the key.
//
// Best-effort: the change has already happened, so a failed append is logged
// rather than reported as a failure of a change that did happen. Both appends
// run on a context that keeps the request's values but not its cancellation,
// bounded by vulnFeedAuditTimeout.
func (h *Handler) recordVulnFeedEvent(reqCtx context.Context, p domain.Principal, authority admingate.Authority, action string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(reqCtx), vulnFeedAuditTimeout)
	defer cancel()
	if h.sysAudit != nil {
		payload, err := json.Marshal(map[string]any{
			"admitted_as": authority.Arm.String(),
			"target_type": vulnFeedAuditTargetType,
			"target_id":   instanceSettingKey,
		})
		if err == nil {
			err = h.sysAudit.RecordSystemAudit(ctx, p.UserID, action, payload)
		}
		if err != nil {
			slog.Default().ErrorContext(ctx, "system audit record failed",
				slog.String("action", action),
				slog.String("actor_id", p.UserID.String()),
				slog.Any("error", err))
		}
	}
	if (authority.Arm != admingate.ArmSoleLiveTenantOwner && authority.Arm != admingate.ArmInstallOwner) ||
		authority.TenantID == uuid.Nil || h.auditRec == nil {
		return
	}
	if _, err := h.auditRec.Record(ctx, audit.Event{
		TenantID:   authority.TenantID,
		ActorType:  audit.ActorUser,
		ActorID:    p.ActorID(),
		Action:     action,
		TargetType: vulnFeedAuditTargetType,
		TargetID:   instanceSettingKey,
		Metadata:   map[string]any{"admitted_as": authority.Arm.String()},
	}); err != nil {
		slog.Default().ErrorContext(ctx, "organisation audit record failed",
			slog.String("action", action),
			slog.String("tenant_id", authority.TenantID.String()),
			slog.String("actor_id", p.UserID.String()),
			slog.Any("error", err))
	}
}

// admittedVulnFeedWriter returns the principal and the admitting decision for a
// vuln-feed write. A write reached without the gate in front of it has no
// admitting decision to route its audit record by, so it is refused with the
// family's refusal rather than left unrecorded.
func admittedVulnFeedWriter(c *gin.Context) (domain.Principal, admingate.Authority, bool) {
	authority, ok := admingate.AdmittingAuthority(c)
	if !ok {
		denyAdminGate(c)
		return domain.Principal{}, admingate.Authority{}, false
	}
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	return p, authority, true
}

// ---------------------------------------------------------------------------
// Route handlers (called from Register when vulnFeedH != nil)
// ---------------------------------------------------------------------------

func (h *Handler) vulnFeedStatus(c *gin.Context) {
	ctx := c.Request.Context()
	vf := h.vulnFeedH
	if vf == nil {
		httpx.Error(c, domain.ServiceUnavailable("vuln_feed_not_wired", "vulnerability feed management is not configured"))
		return
	}

	// Determine source without decrypting the key — just checks DB row presence
	// and falls back to env. The key itself is never returned.
	_, source := vf.keySvc.ResolveAPIKey(ctx)

	status := VulnFeedStatus{
		Configured: source != "none",
		Source:     source,
	}

	// Read feed meta (non-fatal: if the table row is never set, ok=false is fine).
	if vf.meta != nil {
		ok, cnt, synced, lastErr, enrichmentOK, lastEnrichAt, err := vf.meta.GetFeedMetaStatus(ctx)
		if err == nil {
			status.FeedOK = ok
			status.RecordCount = cnt
			status.LastError = lastErr
			status.EnrichmentAvailable = enrichmentOK
			if synced != nil {
				s := synced.UTC().Format(time.RFC3339)
				status.LastSynced = &s
			}
			if lastEnrichAt != nil {
				s := lastEnrichAt.UTC().Format(time.RFC3339)
				status.LastEnrichmentAt = &s
			}
		}
		// If err != nil, leave zero values — the UI shows "not yet synced".
	}

	c.JSON(http.StatusOK, status)
}

type setKeyBody struct {
	Key string `json:"key"`
}

func (h *Handler) vulnFeedSetKey(c *gin.Context) {
	p, authority, ok := admittedVulnFeedWriter(c)
	if !ok {
		return
	}
	vf := h.vulnFeedH
	if vf == nil {
		httpx.Error(c, domain.ServiceUnavailable("vuln_feed_not_wired", "vulnerability feed management is not configured"))
		return
	}
	var body setKeyBody
	if err := c.ShouldBindJSON(&body); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
		return
	}
	if err := vf.keySvc.SetKey(c.Request.Context(), body.Key); err != nil {
		httpx.Error(c, err)
		return
	}
	// Audit: actor, action and admitting arm only; the key is never recorded.
	h.recordVulnFeedEvent(c.Request.Context(), p, authority, auditActionVulnFeedKeySet)
	// Trigger immediate sync so the operator sees it connect without waiting an hour.
	syncErr := vf.keySvc.TriggerSync(c.Request.Context())
	if syncErr != nil {
		// Non-fatal: key is stored; sync will happen on the next hourly tick.
		c.JSON(http.StatusOK, gin.H{
			"ok":      true,
			"syncing": false,
			"warning": "key saved but immediate sync could not be queued: " + syncErr.Error(),
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "syncing": true})
}

func (h *Handler) vulnFeedClearKey(c *gin.Context) {
	p, authority, ok := admittedVulnFeedWriter(c)
	if !ok {
		return
	}
	vf := h.vulnFeedH
	if vf == nil {
		httpx.Error(c, domain.ServiceUnavailable("vuln_feed_not_wired", "vulnerability feed management is not configured"))
		return
	}
	if err := vf.keySvc.ClearKey(c.Request.Context()); err != nil {
		httpx.Error(c, err)
		return
	}
	h.recordVulnFeedEvent(c.Request.Context(), p, authority, auditActionVulnFeedKeyClear)
	// Determine fallback source after clearing.
	_, fallbackSrc := vf.keySvc.ResolveAPIKey(c.Request.Context())
	c.JSON(http.StatusOK, gin.H{
		"ok":              true,
		"fallback_source": fallbackSrc,
	})
}

func (h *Handler) vulnFeedSync(c *gin.Context) {
	p, authority, ok := admittedVulnFeedWriter(c)
	if !ok {
		return
	}
	vf := h.vulnFeedH
	if vf == nil {
		httpx.Error(c, domain.ServiceUnavailable("vuln_feed_not_wired", "vulnerability feed management is not configured"))
		return
	}
	if err := vf.keySvc.TriggerSync(c.Request.Context()); err != nil {
		httpx.Error(c, err)
		return
	}
	h.recordVulnFeedEvent(c.Request.Context(), p, authority, auditActionVulnFeedSync)
	c.JSON(http.StatusAccepted, gin.H{"ok": true, "syncing": true})
}
