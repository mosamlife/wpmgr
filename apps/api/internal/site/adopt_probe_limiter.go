package site

import (
	"container/list"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	// adoptProbeWindow is how long AdoptReportedURL waits before it probes a
	// site again for the same reported address, once a probe got a
	// definitive answer: a 2xx, or a redirect whose target was read.
	adoptProbeWindow = 24 * time.Hour
	// adoptProbeBackoff is how long it waits after a probe that got no
	// definitive answer (a transport failure, a timeout, any other status),
	// so a passing outage does not hold an address back for a day.
	adoptProbeBackoff = time.Hour
	// adoptProbeCapacity bounds how many (site, address) pairs the limiter
	// remembers. When it is full the pair used least recently is forgotten, so
	// that pair may be probed again before its window ends.
	adoptProbeCapacity = 4096
)

// probeKey names one site and one reported address, in the normalised form
// it would be stored in (the plan's To), so two spellings of one address
// share a window.
type probeKey struct {
	site    uuid.UUID
	address string
}

type probeEntry struct {
	key probeKey
	// until is the first instant the key may be probed again.
	until time.Time
	// inFlight is set from begin to finish, so a concurrent caller does not
	// probe the same key.
	inFlight bool
}

// probeLimiter allows one probe at a time per key and, after each probe,
// refuses the key for as long as the probe's outcome says (finish). It
// remembers at most capacity keys, forgetting the least recently used first.
// It is safe for concurrent use.
type probeLimiter struct {
	mu       sync.Mutex
	capacity int
	order    *list.List // front is most recently used
	entries  map[probeKey]*list.Element
}

func newProbeLimiter(capacity int) *probeLimiter {
	if capacity < 1 {
		capacity = 1
	}
	return &probeLimiter{
		capacity: capacity,
		order:    list.New(),
		entries:  make(map[probeKey]*list.Element),
	}
}

// begin reports whether key may be probed at now: it is not being probed,
// and its last hold has ended. When it may, the key is marked in flight
// until finish, which the caller must call once the probe is over. begin
// records no hold: only finish does, from the probe's outcome.
func (l *probeLimiter) begin(key probeKey, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if el, ok := l.entries[key]; ok {
		e := el.Value.(*probeEntry)
		l.order.MoveToFront(el)
		if e.inFlight || now.Before(e.until) {
			return false
		}
		e.inFlight = true
		return true
	}
	l.entries[key] = l.order.PushFront(&probeEntry{key: key, inFlight: true})
	l.evict()
	return true
}

// finish ends the probe begin allowed for key, and refuses the key until
// now+hold.
func (l *probeLimiter) finish(key probeKey, now time.Time, hold time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	el, ok := l.entries[key]
	if !ok {
		el = l.order.PushFront(&probeEntry{key: key})
		l.entries[key] = el
	}
	e := el.Value.(*probeEntry)
	e.inFlight = false
	e.until = now.Add(hold)
	l.order.MoveToFront(el)
	l.evict()
}

// evict forgets the least recently used keys beyond capacity. The caller
// holds mu.
func (l *probeLimiter) evict() {
	for l.order.Len() > l.capacity {
		oldest := l.order.Back()
		l.order.Remove(oldest)
		delete(l.entries, oldest.Value.(*probeEntry).key)
	}
}

// len reports how many keys are remembered.
func (l *probeLimiter) len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.order.Len()
}

// probeLimiter returns the service's adoption probe limiter, creating it on
// first use.
func (s *Service) probeLimiter() *probeLimiter {
	s.adoptProbesOnce.Do(func() {
		if s.adoptProbes == nil {
			s.adoptProbes = newProbeLimiter(adoptProbeCapacity)
		}
	})
	return s.adoptProbes
}

// now is the service clock, or the wall clock when none is wired.
func (s *Service) now() time.Time {
	if s.clock != nil {
		return s.clock.Now()
	}
	return time.Now()
}
