package events

import (
	"context"
	"encoding/json"
	"net/http"
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
)

// Handler serves the tenant-scoped connection-events SSE stream (ADR-038). It is
// mounted on the /api/v1 group (session-auth + RequireTenant already applied)
// and gated with site:read so any viewer can watch their tenant's lifecycle.
type Handler struct {
	pool *db.Pool
	hub  *Hub
}

// NewHandler builds the SSE Handler.
func NewHandler(pool *db.Pool, hub *Hub) *Handler {
	return &Handler{pool: pool, hub: hub}
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
				writeEvent(c.Writer, ev)
				lastSent = ev.ID
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

	for {
		select {
		case <-ctx.Done():
			return
		case <-lifetime.C:
			return
		case <-ticker.C:
			_, _ = c.Writer.Write([]byte(":\n\n"))
			flusher.Flush()
		case ev, open := <-ch:
			if !open {
				return
			}
			// Skip anything the replay already delivered (the live event may
			// race the replay query around the cursor boundary).
			if lastSent != "" && ev.ID <= lastSent {
				continue
			}
			writeEvent(c.Writer, ev)
			lastSent = ev.ID
			flusher.Flush()
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
func writeEvent(w gin.ResponseWriter, ev site.ConnectionEvent) {
	payload, err := json.Marshal(ev)
	if err != nil {
		return
	}
	_, _ = w.Write([]byte("id: "))
	_, _ = w.Write([]byte(ev.ID))
	_, _ = w.Write([]byte("\nevent: "))
	_, _ = w.Write([]byte(ev.Type))
	_, _ = w.Write([]byte("\ndata: "))
	_, _ = w.Write(payload)
	_, _ = w.Write([]byte("\n\n"))
}
