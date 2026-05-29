package backup

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

// BackupEvent is a progress transition published to SSE subscribers of a
// snapshot. It is intentionally small and carries no secrets. PhaseDetail is
// a pass-through of the runner's POST /progress payload (e.g. chunk counters).
// Status mirrors the parent snapshot's status at publish time so a subscriber
// can detect terminal state and close the stream.
type BackupEvent struct {
	SnapshotID  uuid.UUID      `json:"snapshot_id"`
	Phase       string         `json:"phase"`        // closed set; see backup.allowedProgressPhases
	PhaseDetail map[string]any `json:"phase_detail"` // pass-through from the agent's /progress POST payload
	Status      string         `json:"status"`       // snapshot.status — when "completed"/"failed", client closes EventSource
	Timestamp   time.Time      `json:"ts"`
}

// Hub is an in-process pub/sub fan-out for backup progress, keyed by snapshot
// ID. Backup workers / progress POSTs Publish transitions; SSE handlers
// Subscribe to a snapshot. It is safe for concurrent use. Delivery is
// best-effort: a slow subscriber whose buffer is full drops the event rather
// than blocking the publisher (the SSE handler also re-reads authoritative
// state from the DB on connect, so a dropped in-flight event is not a
// correctness problem — it only affects live smoothness).
type Hub struct {
	mu   sync.Mutex
	subs map[uuid.UUID]map[*subscription]struct{}
}

type subscription struct {
	ch chan BackupEvent
}

// NewHub builds an empty Hub.
func NewHub() *Hub {
	return &Hub{subs: make(map[uuid.UUID]map[*subscription]struct{})}
}

// Subscribe registers a subscriber for the given snapshot and returns its
// event channel plus an unsubscribe func. The channel is buffered; the caller
// must drain it and call unsubscribe (e.g. via defer) on disconnect.
func (h *Hub) Subscribe(snapshotID uuid.UUID) (<-chan BackupEvent, func()) {
	sub := &subscription{ch: make(chan BackupEvent, 64)}
	h.mu.Lock()
	if h.subs[snapshotID] == nil {
		h.subs[snapshotID] = make(map[*subscription]struct{})
	}
	h.subs[snapshotID][sub] = struct{}{}
	h.mu.Unlock()

	unsub := func() {
		h.mu.Lock()
		if set, ok := h.subs[snapshotID]; ok {
			if _, ok := set[sub]; ok {
				delete(set, sub)
				close(sub.ch)
			}
			if len(set) == 0 {
				delete(h.subs, snapshotID)
			}
		}
		h.mu.Unlock()
	}
	return sub.ch, unsub
}

// Publish delivers ev to every current subscriber of ev.SnapshotID.
// Non-blocking: a subscriber whose buffer is full drops this event.
func (h *Hub) Publish(ev BackupEvent) {
	h.mu.Lock()
	set := h.subs[ev.SnapshotID]
	subs := make([]*subscription, 0, len(set))
	for s := range set {
		subs = append(subs, s)
	}
	h.mu.Unlock()

	for _, s := range subs {
		select {
		case s.ch <- ev:
		default:
			// Drop for a slow consumer; the SSE handler reconciles from the DB.
		}
	}
}

// SubscriberCount returns the number of active subscribers for a snapshot
// (test aid).
func (h *Hub) SubscriberCount(snapshotID uuid.UUID) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs[snapshotID])
}
