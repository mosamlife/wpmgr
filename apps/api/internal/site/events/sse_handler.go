package events

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

const (
	// sseKeepalive is the interval between SSE keep-alive comment lines.
	sseKeepalive = 15 * time.Second
	// sseMaxLifetime bounds a single stream so a half-closed connection that
	// never surfaces ctx.Done() (proxy in path) cannot leak a goroutine forever.
	sseMaxLifetime = 30 * time.Minute
	// replayLimit caps how many missed events a single ?since replay returns.
	replayLimit = 500
	// maxStreamsPerPrincipal bounds concurrent SSE streams a single principal may
	// hold open, so an authenticated caller can't exhaust goroutines/connections
	// (Phase 6 security review, finding D). Raised from 8 to absorb reload churn:
	// behind a proxy a reloaded tab's stale stream can briefly co-exist with the
	// new one until the next keepalive write fails (see sseWriteTimeout) and frees
	// its slot.
	maxStreamsPerPrincipal = 24
	// sseWriteTimeout bounds each keepalive/event write so a dead-but-lingering
	// client connection (Cloud Run / proxy that doesn't promptly cancel ctx)
	// surfaces as a write error within this window, ending the stream and freeing
	// its slot — instead of pinning it for the full sseMaxLifetime.
	sseWriteTimeout = 10 * time.Second
)

// Handler serves the tenant-scoped connection-events SSE stream (ADR-038). It is
// mounted on the /api/v1 group (session-auth + RequireTenant already applied)
// and gated with site:read so any viewer can watch their tenant's lifecycle.
type Handler struct {
	pool *db.Pool
	hub  *Hub

	mu      sync.Mutex
	streams map[uuid.UUID]int // concurrent stream count per principal (finding D)
}

// NewHandler builds the SSE Handler.
func NewHandler(pool *db.Pool, hub *Hub) *Handler {
	return &Handler{pool: pool, hub: hub, streams: make(map[uuid.UUID]int)}
}

// acquire reserves a stream slot for a principal, returning false if it already
// holds maxStreamsPerPrincipal concurrent streams.
func (h *Handler) acquire(key uuid.UUID) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.streams[key] >= maxStreamsPerPrincipal {
		return false
	}
	h.streams[key]++
	return true
}

// release frees a stream slot.
func (h *Handler) release(key uuid.UUID) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.streams[key] > 0 {
		h.streams[key]--
		if h.streams[key] == 0 {
			delete(h.streams, key)
		}
	}
}

// Register mounts GET /sites/events on the v1 group.
func (h *Handler) Register(r *gin.RouterGroup) {
	r.GET("/sites/events", authz.RequirePermission(authz.PermSiteRead), h.stream)
}

// stream opens the tenant SSE channel: it replays any events after the client's
// cursor (?since or Last-Event-ID) from the durable journal, then subscribes to
// the local Hub and streams live events with 15s keepalives until the client
// disconnects (or the safety timeout fires).
func (h *Handler) stream(c *gin.Context) {
	tenantID, ok := domain.TenantIDFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Forbidden("tenant_required", "a tenant context is required"))
		return
	}
	// Per-site authorization (Phase 6 security review, finding A): the stream is
	// tenant-keyed, so a site-scoped collaborator (shared exactly one site) would
	// otherwise receive live events for EVERY site in the tenant. Filter both the
	// replay and the live fan-out to the principal's allowed sites. Org-scoped
	// principals pass everything; a site-scoped principal sees only events for
	// sites in their allowlist (tenant-level events with a nil site_id are dropped
	// for them too). CanAccessSite encodes exactly this.
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	allowed := func(ev site.ConnectionEvent) bool { return p.CanAccessSite(ev.SiteID) }

	// Per-principal concurrent-stream cap (finding D). Key on the user id, or the
	// tenant id for keyless (API-key) callers.
	limitKey := p.UserID
	if limitKey == uuid.Nil {
		limitKey = tenantID
	}
	if !h.acquire(limitKey) {
		slog.WarnContext(c.Request.Context(), "sse stream cap reached",
			"principal", limitKey.String(), "tenant", tenantID.String(), "cap", maxStreamsPerPrincipal)
		httpx.Error(c, domain.RateLimited("too_many_streams", "too many concurrent event streams; close one and retry"))
		return
	}
	defer h.release(limitKey)

	if h.hub == nil {
		httpx.Error(c, domain.Internal("sse_unsupported", "streaming is not enabled"))
		return
	}
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		httpx.Error(c, domain.Internal("sse_unsupported", "streaming is not supported"))
		return
	}

	// Subscribe BEFORE the replay so no event is lost in the gap between the
	// replay query and the live subscription.
	ch, unsub := h.hub.Subscribe(tenantID)
	defer unsub()

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)

	ctx := c.Request.Context()

	// Replay window: ?since takes precedence over the Last-Event-ID header.
	since := c.Query("since")
	if since == "" {
		since = c.GetHeader("Last-Event-ID")
	}
	lastSent := since
	if since != "" {
		replayed, err := h.replay(ctx, tenantID, since)
		if err == nil {
			for _, ev := range replayed {
				if allowed(ev) {
					_, _ = c.Writer.Write(eventFrame(ev))
				}
				lastSent = ev.ID // advance the cursor even past filtered events
			}
			flusher.Flush()
		}
		// A replay error is non-fatal: the live stream + the client's
		// reconcile-on-connect (["sites","list"] invalidation) self-heal a gap.
	}

	ticker := time.NewTicker(sseKeepalive)
	defer ticker.Stop()
	lifetime := time.NewTimer(sseMaxLifetime)
	defer lifetime.Stop()

	// writeFrame writes p with a bounded deadline and returns false when the
	// client connection is gone — the caller then ends the stream so the deferred
	// release() frees the slot promptly (finding D / 429 fix). The write deadline
	// turns a wedged write (dead-but-lingering connection behind a proxy) into a
	// prompt error instead of an indefinite block.
	rc := http.NewResponseController(c.Writer)
	writeFrame := func(p []byte) bool {
		_ = rc.SetWriteDeadline(time.Now().Add(sseWriteTimeout)) // best-effort
		if _, err := c.Writer.Write(p); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-lifetime.C:
			return
		case <-ticker.C:
			if !writeFrame([]byte(":\n\n")) {
				return // client gone — free the slot now, don't wait 30 min
			}
		case ev, open := <-ch:
			if !open {
				return
			}
			// Skip anything the replay already delivered (the live event may
			// race the replay query around the cursor boundary).
			if lastSent != "" && ev.ID <= lastSent {
				continue
			}
			// Drop events the principal isn't authorized for (finding A).
			if !allowed(ev) {
				continue
			}
			if !writeFrame(eventFrame(ev)) {
				return // client gone
			}
			lastSent = ev.ID
		}
	}
}

// replay loads events strictly after the cursor from the durable journal.
func (h *Handler) replay(ctx context.Context, tenantID uuid.UUID, since string) ([]site.ConnectionEvent, error) {
	var out []site.ConnectionEvent
	err := h.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).ReplaySiteEvents(ctx, sqlc.ReplaySiteEventsParams{
			TenantID: tenantID,
			EventID:  since,
			Limit:    replayLimit,
		})
		if err != nil {
			return err
		}
		out = make([]site.ConnectionEvent, 0, len(rows))
		for _, row := range rows {
			out = append(out, RowToEvent(row))
		}
		return nil
	})
	return out, err
}

// writeEvent serializes a ConnectionEvent as a single SSE frame. The `id:` line
// is the ULID so the browser's EventSource sets Last-Event-ID for reconnect
// replay (ADR-038); `event:` is the event type so the client can addEventListener.
func eventFrame(ev site.ConnectionEvent) []byte {
	payload, err := json.Marshal(ev)
	if err != nil {
		return nil
	}
	var b bytes.Buffer
	b.WriteString("id: ")
	b.WriteString(ev.ID)
	b.WriteString("\nevent: ")
	b.WriteString(ev.Type)
	b.WriteString("\ndata: ")
	b.Write(payload)
	b.WriteString("\n\n")
	return b.Bytes()
}
