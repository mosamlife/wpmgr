package site

import (
	"container/list"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	// adoptProbeWindow is how long AdoptReportedURL waits before it probes a
	// site again for the same reported address.
	adoptProbeWindow = 24 * time.Hour
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
	at  time.Time
}

// probeLimiter allows at most one probe per window per key, remembering at
// most capacity keys (least recently used forgotten first). It is safe for
// concurrent use.
type probeLimiter struct {
	mu       sync.Mutex
	window   time.Duration
	capacity int
	order    *list.List // front is most recently used
	entries  map[probeKey]*list.Element
}

func newProbeLimiter(window time.Duration, capacity int) *probeLimiter {
	if capacity < 1 {
		capacity = 1
	}
	return &probeLimiter{
		window:   window,
		capacity: capacity,
		order:    list.New(),
		entries:  make(map[probeKey]*list.Element),
	}
}

// allow reports whether key may be probed at now, and when it may, records
// now as its last probe. A key probed less than window before now is
// refused.
func (l *probeLimiter) allow(key probeKey, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if el, ok := l.entries[key]; ok {
		e := el.Value.(*probeEntry)
		l.order.MoveToFront(el)
		if now.Sub(e.at) < l.window {
			return false
		}
		e.at = now
		return true
	}
	l.entries[key] = l.order.PushFront(&probeEntry{key: key, at: now})
	for l.order.Len() > l.capacity {
		oldest := l.order.Back()
		l.order.Remove(oldest)
		delete(l.entries, oldest.Value.(*probeEntry).key)
	}
	return true
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
			s.adoptProbes = newProbeLimiter(adoptProbeWindow, adoptProbeCapacity)
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
