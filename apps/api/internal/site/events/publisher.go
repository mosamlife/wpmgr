package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

// notifyChannel is the Postgres LISTEN/NOTIFY channel name (ADR-038).
const notifyChannel = "wpmgr_site_events"

// Publisher persists a connection event to site_events (minting a ULID) and
// emits a Postgres NOTIFY carrying only '<tenant_id>:<event_id>' (NOTIFY's
// payload cap means the body is never shipped on the wire — every instance
// reads the row under tenant scope, see the Listener). It satisfies
// site.EventPublisher; the ConnectionService calls Publish AFTER its state
// transition commits, so the insert + notify run in their own short tx.
type Publisher struct {
	pool  *db.Pool
	clock domain.Clock
}

// NewPublisher builds a Publisher.
func NewPublisher(pool *db.Pool, clock domain.Clock) *Publisher {
	return &Publisher{pool: pool, clock: clock}
}

// Publish mints the event_id (if the caller left it empty), persists the event
// under the tenant's RLS scope, and fires NOTIFY in the same transaction so a
// committed row is always announced. The event's ID and TS are filled in on the
// supplied ev for the caller's convenience (e.g. so an SSE replay cursor lines
// up), but Publish is one-way — failures are returned, not retried.
func (p *Publisher) Publish(ctx context.Context, ev site.ConnectionEvent) error {
	if ev.TenantID == uuid.Nil {
		return domain.Validation("event_tenant_required", "connection event requires a tenant_id")
	}
	if ev.ID == "" {
		ev.ID = NewULID(p.clock.Now())
	}
	if ev.TS.IsZero() {
		ev.TS = p.clock.Now().UTC()
	}
	data, err := json.Marshal(orEmptyData(ev.Data))
	if err != nil {
		return domain.Internal("event_marshal_failed", "failed to encode connection event data").WithCause(err)
	}

	var siteID pgtype.UUID
	if ev.SiteID != uuid.Nil {
		siteID = pgtype.UUID{Bytes: ev.SiteID, Valid: true}
	}

	return p.pool.InTenantTx(ctx, ev.TenantID, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		if _, err := q.InsertSiteEvent(ctx, sqlc.InsertSiteEventParams{
			EventID:  ev.ID,
			TenantID: ev.TenantID,
			SiteID:   siteID,
			Type:     ev.Type,
			Data:     data,
		}); err != nil {
			return domain.Internal("event_insert_failed", "failed to persist connection event").WithCause(err)
		}
		// NOTIFY ids only; the body is read by each instance under tenant scope.
		payload := ev.TenantID.String() + ":" + ev.ID
		if _, err := tx.Exec(ctx, "SELECT pg_notify($1, $2)", notifyChannel, payload); err != nil {
			return domain.Internal("event_notify_failed", "failed to emit connection event notify").WithCause(err)
		}
		return nil
	})
}

// PruneOlderThan deletes site_events older than the cutoff (the ADR-038
// ring-buffer prune, ~5 min retention). Cross-tenant maintenance under the
// app.agent GUC. Returns the number of rows deleted.
func (p *Publisher) PruneOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	var deleted int64
	err := p.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		n, err := sqlc.New(tx).PruneSiteEvents(ctx, cutoff)
		if err != nil {
			return domain.Internal("event_prune_failed", "failed to prune site events").WithCause(err)
		}
		deleted = n
		return nil
	})
	return deleted, err
}

func orEmptyData(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// parseNotifyPayload splits a '<tenant_id>:<event_id>' NOTIFY payload.
func parseNotifyPayload(s string) (uuid.UUID, string, error) {
	for i := 0; i < len(s); i++ {
		if s[i] == ':' {
			tid, err := uuid.Parse(s[:i])
			if err != nil {
				return uuid.Nil, "", err
			}
			return tid, s[i+1:], nil
		}
	}
	return uuid.Nil, "", fmt.Errorf("malformed notify payload %q", s)
}
