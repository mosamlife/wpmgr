package admin

// vuln_feed_handler.go — superadmin HTTP handlers for the Wordfence Intelligence
// API key configuration. Routes live under /api/v1/admin/vuln-feed/, which is
// already gated by requireSuperadmin (see Register in handler.go).
//
// Endpoints:
//   GET    /admin/vuln-feed/status   — masked status; NEVER returns the key
//   PUT    /admin/vuln-feed/key      — set key (plaintext in body over TLS)
//   DELETE /admin/vuln-feed/key      — clear UI-stored key (falls back to env)
//   POST   /admin/vuln-feed/sync     — enqueue immediate feed refresh

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

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
}

// SetVulnFeed wires the vuln-feed sub-handler into the admin Handler. It must
// be called before the first Register call (i.e. at boot, before the HTTP
// server starts) so that Register can conditionally mount the routes.
func (h *Handler) SetVulnFeed(meta VulnFeedMetaReader, keySvc *VulnFeedKeyService) {
	h.vulnFeedH = &vulnFeedAdminHandler{meta: meta, keySvc: keySvc}
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
	p, _ := domain.PrincipalFromContext(c.Request.Context())
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
	// Audit: actor + action only; key value is NOT included in metadata.
	if h.auditRec != nil {
		_, _ = h.auditRec.Record(c.Request.Context(), audit.Event{
			ActorType:  audit.ActorUser,
			ActorID:    p.UserID.String(),
			Action:     "admin.vuln_feed.key.set",
			TargetType: "instance_setting",
			TargetID:   instanceSettingKey,
		})
	}
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
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	vf := h.vulnFeedH
	if vf == nil {
		httpx.Error(c, domain.ServiceUnavailable("vuln_feed_not_wired", "vulnerability feed management is not configured"))
		return
	}
	if err := vf.keySvc.ClearKey(c.Request.Context()); err != nil {
		httpx.Error(c, err)
		return
	}
	if h.auditRec != nil {
		_, _ = h.auditRec.Record(c.Request.Context(), audit.Event{
			ActorType:  audit.ActorUser,
			ActorID:    p.UserID.String(),
			Action:     "admin.vuln_feed.key.clear",
			TargetType: "instance_setting",
			TargetID:   instanceSettingKey,
		})
	}
	// Determine fallback source after clearing.
	_, fallbackSrc := vf.keySvc.ResolveAPIKey(c.Request.Context())
	c.JSON(http.StatusOK, gin.H{
		"ok":              true,
		"fallback_source": fallbackSrc,
	})
}

func (h *Handler) vulnFeedSync(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	vf := h.vulnFeedH
	if vf == nil {
		httpx.Error(c, domain.ServiceUnavailable("vuln_feed_not_wired", "vulnerability feed management is not configured"))
		return
	}
	if err := vf.keySvc.TriggerSync(c.Request.Context()); err != nil {
		httpx.Error(c, err)
		return
	}
	if h.auditRec != nil {
		_, _ = h.auditRec.Record(c.Request.Context(), audit.Event{
			ActorType:  audit.ActorUser,
			ActorID:    p.UserID.String(),
			Action:     "admin.vuln_feed.sync",
			TargetType: "instance_setting",
			TargetID:   instanceSettingKey,
		})
	}
	c.JSON(http.StatusAccepted, gin.H{"ok": true, "syncing": true})
}
