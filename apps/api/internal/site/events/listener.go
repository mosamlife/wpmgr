package events

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

// Listener runs ONE dedicated LISTEN wpmgr_site_events connection per process
// (ADR-038). On each notification it parses '<tenant>:<event_id>', loads the
// event row under that tenant's RLS scope, and fans it out to the local Hub's
// subscribers for that tenant. It reconnects with backoff on any connection
// error so a transient DB blip does not permanently silence the bus.
type Listener struct {
	pool   *db.Pool
	hub    *Hub
	logger *slog.Logger
}

// NewListener builds a Listener.
func NewListener(pool *db.Pool, hub *Hub, logger *slog.Logger) *Listener {
	return &Listener{pool: pool, hub: hub, logger: logger}
}

// Run blocks until ctx is cancelled, holding the dedicated LISTEN connection and
// fanning notifications out to the Hub. It is meant to be started in its own
// goroutine at boot. Reconnects on error (capped backoff) until ctx is done.
func (l *Listener) Run(ctx context.Context) {
	backoff := time.Second
	const maxBackoff = 30 * time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		if err := l.listen(ctx); err != nil && ctx.Err() == nil {
			l.logger.Warn("site events listener disconnected; reconnecting",
				slog.Any("error", err), slog.Duration("backoff", backoff))
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < maxBackoff {
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
			}
			continue
		}
		// Clean exit (ctx cancelled inside listen).
		return
	}
}

// listen acquires a dedicated connection, issues LISTEN, and loops on
// notifications until ctx is cancelled or the connection errors. It resets the
// caller's backoff implicitly: a successful long-lived session returns nil only
// on ctx cancel.
func (l *Listener) listen(ctx context.Context) error {
	conn, err := l.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "LISTEN "+notifyChannel); err != nil {
		return err
	}
	l.logger.Info("site events listener attached", slog.String("channel", notifyChannel))

	for {
		notification, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil // clean shutdown
			}
			return err
		}
		l.dispatch(ctx, notification.Payload)
	}
}

// dispatch loads the announced event under its tenant's scope and fans it out.
// A malformed payload or a missing row (already pruned) is logged and skipped —
// never fatal to the listen loop.
func (l *Listener) dispatch(ctx context.Context, payload string) {
	tenantID, eventID, err := parseNotifyPayload(payload)
	if err != nil {
		l.logger.Warn("site events: bad notify payload", slog.String("payload", payload), slog.Any("error", err))
		return
	}
	ev, err := l.loadEvent(ctx, tenantID, eventID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return // pruned out from under us; harmless
		}
		l.logger.Warn("site events: failed to load notified event",
			slog.String("tenant_id", tenantID.String()), slog.String("event_id", eventID), slog.Any("error", err))
		return
	}
	l.hub.Fanout(ev)
}

// loadEvent reads one site_events row under the tenant's RLS scope and decodes
// it into the SSE envelope.
func (l *Listener) loadEvent(ctx context.Context, tenantID uuid.UUID, eventID string) (site.ConnectionEvent, error) {
	var ev site.ConnectionEvent
	err := l.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).GetSiteEvent(ctx, sqlc.GetSiteEventParams{EventID: eventID, TenantID: tenantID})
		if err != nil {
			return err
		}
		ev = RowToEvent(row)
		return nil
	})
	return ev, err
}

// RowToEvent maps a persisted site_events row to the SSE envelope. Exported so
// the SSE handler can reuse it for the replay path.
func RowToEvent(row sqlc.SiteEvent) site.ConnectionEvent {
	ev := site.ConnectionEvent{
		ID:       row.EventID,
		Type:     row.Type,
		TenantID: row.TenantID,
		TS:       row.CreatedAt.UTC(),
	}
	if row.SiteID.Valid {
		ev.SiteID = uuid.UUID(row.SiteID.Bytes)
	}
	if len(row.Data) > 0 {
		_ = json.Unmarshal(row.Data, &ev.Data)
	}
	if ev.Data == nil {
		ev.Data = map[string]any{}
	}
	return ev
}
