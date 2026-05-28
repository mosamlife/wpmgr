package autologin

import (
	"context"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Limiter is the swappable rate-limit interface the mint service consults. The
// V0 implementation is in-memory + per-process (see MemoryLimiter); a future
// implementation will swap in a Redis token-bucket without touching the
// service. The single-instance assumption is documented at MemoryLimiter.
//
// Allow returns true when the key is within budget; false means the request
// must be rejected with 429. retryAfter is the smallest duration the caller
// should wait before retrying (best-effort; may be zero when the limiter
// cannot estimate). The interface intentionally takes a context so a future
// Redis implementation can honour cancellation.
type Limiter interface {
	Allow(ctx context.Context, key string, limitPerMinute int) (allowed bool, retryAfter time.Duration)
}

// MemoryLimiter is an in-process Limiter built on golang.org/x/time/rate. Each
// distinct key gets its own *rate.Limiter; a periodic janitor drops limiters
// that have been idle for the JanitorIdle window to bound memory growth.
//
// IMPORTANT: this is per-API-instance. For multi-instance deployments a Redis
// token-bucket implementation MUST be wired in via Limiter — the per-instance
// limit would otherwise be multiplied by the instance count. The interface
// makes that swap a one-liner in main.go (no Service change).
type MemoryLimiter struct {
	mu       sync.Mutex
	buckets  map[string]*memoryBucket
	idle     time.Duration
	stop     chan struct{}
	stopOnce sync.Once
}

// memoryBucket holds a per-key limiter and its last-access timestamp.
type memoryBucket struct {
	lim        *rate.Limiter
	lastAccess time.Time
	limit      int
}

// JanitorIdle is the idle window beyond which an unused bucket is pruned.
const JanitorIdle = 10 * time.Minute

// JanitorInterval is how often the janitor sweeps for idle buckets.
const JanitorInterval = 5 * time.Minute

// NewMemoryLimiter builds a MemoryLimiter and starts its background janitor.
// The caller MUST defer Stop() (or rely on process exit in main) to release
// the janitor goroutine.
func NewMemoryLimiter() *MemoryLimiter {
	m := &MemoryLimiter{
		buckets: map[string]*memoryBucket{},
		idle:    JanitorIdle,
		stop:    make(chan struct{}),
	}
	go m.janitor()
	return m
}

// Stop terminates the janitor goroutine; safe to call multiple times.
func (m *MemoryLimiter) Stop() {
	m.stopOnce.Do(func() { close(m.stop) })
}

// Allow consumes one token from the per-key bucket. When the bucket is empty
// the call returns false and the smallest wait for a token to become available.
func (m *MemoryLimiter) Allow(_ context.Context, key string, limitPerMinute int) (bool, time.Duration) {
	if limitPerMinute <= 0 {
		return true, 0
	}
	now := time.Now()
	m.mu.Lock()
	b, ok := m.buckets[key]
	if !ok || b.limit != limitPerMinute {
		// Per-minute budget = limit tokens / 60 seconds; allow short bursts up to
		// the full per-minute budget so the limit is the cap on a 1-minute window.
		b = &memoryBucket{
			lim:   rate.NewLimiter(rate.Limit(float64(limitPerMinute)/60.0), limitPerMinute),
			limit: limitPerMinute,
		}
		m.buckets[key] = b
	}
	b.lastAccess = now
	r := b.lim.ReserveN(now, 1)
	m.mu.Unlock()

	if !r.OK() {
		// Bucket can never satisfy a single token — should be impossible with the
		// burst=limit configuration above, but guard against it anyway.
		return false, time.Minute
	}
	wait := r.DelayFrom(now)
	if wait <= 0 {
		return true, 0
	}
	// Token would be available, but only in the future: reject this request and
	// roll back the reservation so a polite client can retry.
	r.CancelAt(now)
	return false, wait
}

// janitor periodically prunes buckets that have been idle for longer than
// m.idle. This is necessary because every distinct (initiator, site) key
// creates a bucket; without pruning, long-lived processes leak memory.
func (m *MemoryLimiter) janitor() {
	t := time.NewTicker(JanitorInterval)
	defer t.Stop()
	for {
		select {
		case <-m.stop:
			return
		case now := <-t.C:
			m.mu.Lock()
			for k, b := range m.buckets {
				if now.Sub(b.lastAccess) > m.idle {
					delete(m.buckets, k)
				}
			}
			m.mu.Unlock()
		}
	}
}

// Rate-limit budgets, exposed as constants so tests and callers agree.
const (
	// LimitInitiatorSitePerMin caps mints per (initiator user, site) pair to
	// catch a runaway operator without affecting other operators on the site.
	LimitInitiatorSitePerMin = 10
	// LimitSitePerMin caps mints per site across ALL operators, providing a
	// per-target backstop a single user cannot tighten or loosen.
	LimitSitePerMin = 30
)
