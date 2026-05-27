package update

import (
	"sync"

	"github.com/google/uuid"
)

// Event is a task-status transition published to SSE subscribers of a run. It
// is intentionally small and carries no secrets.
type Event struct {
	RunID       uuid.UUID `json:"run_id"`
	TaskID      uuid.UUID `json:"task_id"`
	SiteID      uuid.UUID `json:"site_id"`
	TargetType  string    `json:"target_type"`
	TargetSlug  string    `json:"target_slug"`
	Status      string    `json:"status"`
	FromVersion string    `json:"from_version,omitempty"`
	ToVersion   string    `json:"to_version,omitempty"`
	Detail      string    `json:"detail,omitempty"`
	// RunStatus is the parent run's status at publish time, so a subscriber can
	// detect completion and close the stream.
	RunStatus string `json:"run_status"`
}

// Hub is an in-process pub/sub fan-out for update progress, keyed by run ID.
// River workers Publish transitions; SSE handlers Subscribe to a run. It is
// safe for concurrent use. Delivery is best-effort: a slow subscriber whose
// buffer is full drops the event rather than blocking the worker (the SSE
// handler also re-reads authoritative state from the DB, so a dropped in-flight
// event is not a correctness problem — it only affects live smoothness).
type Hub struct {
	mu   sync.Mutex
	subs map[uuid.UUID]map[*subscription]struct{}
}

type subscription struct {
	ch chan Event
}

// NewHub builds an empty Hub.
func NewHub() *Hub {
	return &Hub{subs: make(map[uuid.UUID]map[*subscription]struct{})}
}

// Subscribe registers a subscriber for the given run and returns its event
// channel plus an unsubscribe func. The channel is buffered; the caller must
// drain it and call unsubscribe (e.g. via defer) on disconnect.
func (h *Hub) Subscribe(runID uuid.UUID) (<-chan Event, func()) {
	sub := &subscription{ch: make(chan Event, 64)}
	h.mu.Lock()
	if h.subs[runID] == nil {
		h.subs[runID] = make(map[*subscription]struct{})
	}
	h.subs[runID][sub] = struct{}{}
	h.mu.Unlock()

	unsub := func() {
		h.mu.Lock()
		if set, ok := h.subs[runID]; ok {
			if _, ok := set[sub]; ok {
				delete(set, sub)
				close(sub.ch)
			}
			if len(set) == 0 {
				delete(h.subs, runID)
			}
		}
		h.mu.Unlock()
	}
	return sub.ch, unsub
}

// Publish delivers ev to every current subscriber of ev.RunID. Non-blocking: a
// subscriber whose buffer is full drops this event.
func (h *Hub) Publish(ev Event) {
	h.mu.Lock()
	set := h.subs[ev.RunID]
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

// SubscriberCount returns the number of active subscribers for a run (test aid).
func (h *Hub) SubscriberCount(runID uuid.UUID) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs[runID])
}
