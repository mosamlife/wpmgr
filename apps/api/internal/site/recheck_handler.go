package site

import (
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	agentpkg "github.com/mosamlife/wpmgr/apps/api/internal/agent"
	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// LimitRecheckPerSitePerMin is the maximum number of Re-check connection
// dispatches allowed per (tenant, site) pair per minute.  4 per minute means
// one recheck roughly every 15 seconds — fast enough for a legitimate operator
// to retry a flapping connection, slow enough that a tight loop cannot spam the
// agent's wp-cron queue.
const LimitRecheckPerSitePerMin = 4

// RecheckResponse is the M58 POST /api/v1/sites/:siteId/recheck response body.
// The frontend reconciles the badge in place without a full page reload.
type RecheckResponse struct {
	// ConnectionState is the site's connection_state AFTER the recheck
	// (recovered sites return "connected"; unchanged sites return their current
	// state).
	ConnectionState string `json:"connection_state"`
	// LastSeenAt is the refreshed liveness timestamp in RFC 3339 format, or ""
	// when the site has never been seen (should not occur on a successful
	// recheck).
	LastSeenAt string `json:"last_seen_at,omitempty"`
	// Recovered is true when the recheck promoted the site from
	// degraded/disconnected to connected. The SSE site.state_changed event fires
	// concurrently; this field lets the web skip waiting for it.
	Recovered bool `json:"recovered"`
}

// recheck handles POST /api/v1/sites/:siteId/recheck (M58).
//
// Flow:
//  1. Load the site to get its URL and confirm access.
//  2. Dispatch a signed `metadata` command to the agent (synchronous — returns
//     the full inventory in the 200 body).
//  3. Feed the response through ApplyAgentMetadata (updates inventory +
//     last_seen_at + age_recipient).
//  4. Call conn.RecordHeartbeat to trigger the recovery transition
//     (degraded/disconnected→connected) and reset the miss counter (M58).
//  5. Return {connection_state, last_seen_at, recovered}.
//
// On agent unreachable: return 502 {code:"agent_unreachable"} without forcing
// the site to disconnected. The sweeper owns that transition.
func (h *Handler) recheck(c *gin.Context) {
	ctx := c.Request.Context()

	p, hasPrincipal := domain.PrincipalFromContext(ctx)
	if !hasPrincipal {
		httpx.Error(c, domain.Forbidden("tenant_required", "a tenant context is required"))
		return
	}

	siteID, err := uuid.Parse(c.Param("siteId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_site_id", "siteId is not a valid UUID"))
		return
	}

	if h.rechecker == nil {
		httpx.Error(c, domain.Unavailable("recheck_disabled", "re-check is not available: CP signing key is not configured"))
		return
	}
	if h.conn == nil {
		httpx.Error(c, domain.Unavailable("lifecycle_disabled", "connection lifecycle is not enabled on this control plane"))
		return
	}

	// Per-(tenant, site) rate limit: cap synchronous metadata dispatches to
	// LimitRecheckPerSitePerMin.  The key includes tenantID so one tenant's
	// spamming of a site they own cannot starve a different tenant's checks, and
	// siteID so one noisy site doesn't block rechecks on other sites in the same
	// tenant.
	if h.recheckLimiter != nil {
		key := p.TenantID.String() + "|" + siteID.String()
		if allowed, retry := h.recheckLimiter.Allow(ctx, key, LimitRecheckPerSitePerMin); !allowed {
			sec := recheckRetryAfterSeconds(retry)
			c.Header("Retry-After", fmt.Sprintf("%d", sec))
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"code":                "recheck_rate_limited",
				"message":             fmt.Sprintf("recheck rate limited; retry after %d seconds", sec),
				"retry_after_seconds": sec,
			})
			return
		}
	}

	// Load the site to get its URL.
	st, err := h.svc.Get(ctx, p.TenantID, siteID)
	if err != nil {
		httpx.Error(c, err)
		return
	}

	// Audit: operator requested a re-check.
	h.record(c, p.TenantID, "site.recheck", siteID.String(), nil)

	// Dispatch the synchronous metadata command. A transport or non-2xx error
	// is treated as agent_unreachable (502) — do NOT flip the site to
	// disconnected; let the sweeper own that transition.
	rawMeta, cmdErr := h.rechecker.MetadataRaw(ctx, siteID, st.URL)
	if re, ok := agentcmd.AsRedirect(cmdErr); ok {
		// The site answered with a redirect, so the saved address is wrong.
		// Same 502 as unreachable, but a distinct code that names the target
		// and the remedy. Every URL here is already sanitised by agentcmd.
		details := gin.H{"from": re.SavedSiteURL(), "to": re.To}
		if re.SuggestedSiteURL != "" {
			details["suggested_url"] = re.SuggestedSiteURL
		}
		c.JSON(http.StatusBadGateway, gin.H{
			"code":    "site_url_redirects",
			"message": "Couldn't reach the agent. " + re.Explanation(),
			"details": details,
		})
		return
	}
	if cmdErr != nil {
		// Log at warn; return a structured 502 so the web can render a
		// "Couldn't reach agent" state without flipping the status badge.
		c.JSON(http.StatusBadGateway, gin.H{
			"code":    "agent_unreachable",
			"message": "could not reach the site agent",
		})
		return
	}

	// Decode the raw metadata bytes through the agent package's tolerant decoder
	// (the same path the normal POST /agent/v1/metadata uses).
	agentMeta, decErr := agentpkg.DecodeMetadataBytes(rawMeta)
	if decErr != nil {
		// Malformed body from the agent — treat as an infrastructure error.
		httpx.Error(c, domain.Internal("metadata_decode_failed", "agent returned unreadable metadata").WithCause(decErr))
		return
	}

	// Apply the metadata (updates inventory, last_seen_at, age_recipient).
	// We ignore the gen.Site return value here; the recheck response shape is
	// intentionally minimal and reads state back from the heartbeat result.
	if _, applyErr := h.svc.ApplyAgentMetadata(ctx, p.TenantID, siteID, agentMeta); applyErr != nil {
		httpx.Error(c, applyErr)
		return
	}

	// Trigger the connection-state recovery (degraded/disconnected→connected)
	// and reset the M58 miss counter. RecordHeartbeat is the ONLY recovery
	// writer (ADR-039); calling it here after a confirmed successful metadata
	// response is semantically equivalent to the agent pushing a heartbeat.
	var recovered bool
	beforeState := st.ConnectionState
	if _, hbErr := h.conn.RecordHeartbeat(ctx, HeartbeatInput{
		TenantID: p.TenantID,
		SiteID:   siteID,
	}); hbErr != nil {
		// Recovery failure must not fail the request (metadata already landed).
		// Fall through with the pre-recheck state.
		_ = hbErr
	} else {
		// Reload the site to get the fresh state after the heartbeat transition.
		refreshed, refreshErr := h.svc.Get(ctx, p.TenantID, siteID)
		if refreshErr == nil {
			recovered = (beforeState == StateDegraded || beforeState == StateDisconnected) &&
				refreshed.ConnectionState == StateConnected
			// Overwrite st so we report the post-recovery state.
			st = refreshed
		}
	}

	var lastSeenAt string
	if st.LastSeenAt != nil {
		lastSeenAt = st.LastSeenAt.UTC().Format(time.RFC3339)
	}

	c.JSON(http.StatusOK, RecheckResponse{
		ConnectionState: string(st.ConnectionState),
		LastSeenAt:      lastSeenAt,
		Recovered:       recovered,
	})
}

// recheckRetryAfterSeconds rounds a duration up to whole seconds, never below 1.
// Mirrors the equivalent helper in the autologin package.
func recheckRetryAfterSeconds(d time.Duration) int {
	sec := int(d.Round(time.Second) / time.Second)
	if sec < 1 {
		sec = 1
	}
	return sec
}
