// Package events implements the ADR-038 cross-instance SSE bus for site
// connection-lifecycle events: a durable site_events journal + Postgres
// LISTEN/NOTIFY fan-out, a tenant-keyed in-process subscriber Hub, and the
// dashboard SSE endpoint plumbing.
//
// Flow: ConnectionService.Publish → INSERT site_events (ULID) → pg_notify
// 'wpmgr_site_events','<tenant>:<event_id>'. Every API instance runs one
// dedicated LISTEN connection (Listener); on a notification it loads the row
// under that tenant's scope and fans it out to the local Hub subscribers for
// that tenant. The SSE handler replays from site_events first (?since), then
// streams live from the Hub.
package events

import (
	"sync"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

// Hub is an in-process pub/sub fan-out for connection events, keyed by
// tenant_id (ADR-038 — clients open one tenant-scoped stream and filter by
// site_id in the browser). Safe for concurrent use. Delivery is best-effort: a
// slow subscriber whose buffer is full drops the event rather than blocking the
// publisher (the SSE handler also reconciles ["sites","list"] on connect, so a
// dropped live event is not a correctness problem — see ADR-038 §4).
type Hub struct {
	mu   sync.Mutex
	subs map[uuid.UUID]map[*subscription]struct{}
}

type subscription struct {
	ch chan site.ConnectionEvent
}

// NewHub builds an empty Hub.
func NewHub() *Hub {
	return &Hub{subs: make(map[uuid.UUID]map[*subscription]struct{})}
}

// Subscribe registers a subscriber for the given tenant and returns its event
// channel plus an unsubscribe func. The channel is buffered; the caller must
// drain it and call unsubscribe (e.g. via defer) on disconnect.
func (h *Hub) Subscribe(tenantID uuid.UUID) (<-chan site.ConnectionEvent, func()) {
	sub := &subscription{ch: make(chan site.ConnectionEvent, 64)}
	h.mu.Lock()
	if h.subs[tenantID] == nil {
		h.subs[tenantID] = make(map[*subscription]struct{})
	}
	h.subs[tenantID][sub] = struct{}{}
	h.mu.Unlock()

	unsub := func() {
		h.mu.Lock()
		if set, ok := h.subs[tenantID]; ok {
			if _, ok := set[sub]; ok {
				delete(set, sub)
				close(sub.ch)
			}
			if len(set) == 0 {
				delete(h.subs, tenantID)
			}
		}
		h.mu.Unlock()
	}
	return sub.ch, unsub
}

// Fanout delivers ev to every current subscriber of ev.TenantID. Non-blocking:
// a subscriber whose buffer is full drops this event. Called by the Listener on
// each NOTIFY (local-instance fan-out).
func (h *Hub) Fanout(ev site.ConnectionEvent) {
	h.mu.Lock()
	set := h.subs[ev.TenantID]
	subs := make([]*subscription, 0, len(set))
	for s := range set {
		subs = append(subs, s)
	}
	h.mu.Unlock()

	for _, s := range subs {
		select {
		case s.ch <- ev:
		default:
			// Drop for a slow consumer; the SSE handler reconciles on connect.
		}
	}
}

// SubscriberCount returns the number of active subscribers for a tenant (test
// aid).
func (h *Hub) SubscriberCount(tenantID uuid.UUID) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs[tenantID])
}
