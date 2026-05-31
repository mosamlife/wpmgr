package service

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

// rateLimiter is a small in-process token-bucket-ish cap on how many encode
// enqueues a single site (and tenant) may trigger per window. It guards the
// media_encode queue + the agent against a runaway bulk-optimize loop. It is
// intentionally process-local (the encoder queue's bounded MaxWorkers is the
// hard backstop); for a multi-instance deployment the queue bound dominates.
type rateLimiter struct {
	mu        sync.Mutex
	perSite   int
	perTenant int
	window    time.Duration
	sites     map[uuid.UUID]*window
	tenants   map[uuid.UUID]*window
	now       func() time.Time
}

type window struct {
	count   int
	resetAt time.Time
}

// newRateLimiter builds a limiter. perSite/perTenant are the max encode enqueues
// per window; win is the rolling reset interval. Zero caps disable the limit.
func newRateLimiter(perSite, perTenant int, win time.Duration) *rateLimiter {
	if win <= 0 {
		win = time.Minute
	}
	return &rateLimiter{
		perSite:   perSite,
		perTenant: perTenant,
		window:    win,
		sites:     map[uuid.UUID]*window{},
		tenants:   map[uuid.UUID]*window{},
		now:       time.Now,
	}
}

// allow reports whether `n` encode enqueues for (tenant, site) fit within the
// caps. It records the spend when it returns true. A disabled cap (<=0) never
// blocks that dimension.
func (r *rateLimiter) allow(tenantID, siteID uuid.UUID, n int) bool {
	if n <= 0 {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()

	if r.perSite > 0 {
		w := r.bucket(r.sites, siteID, now)
		if w.count+n > r.perSite {
			return false
		}
	}
	if r.perTenant > 0 {
		w := r.bucket(r.tenants, tenantID, now)
		if w.count+n > r.perTenant {
			return false
		}
	}
	// Commit the spend on both dimensions (only after both pass).
	if r.perSite > 0 {
		r.bucket(r.sites, siteID, now).count += n
	}
	if r.perTenant > 0 {
		r.bucket(r.tenants, tenantID, now).count += n
	}
	return true
}

func (r *rateLimiter) bucket(m map[uuid.UUID]*window, id uuid.UUID, now time.Time) *window {
	w, ok := m[id]
	if !ok || now.After(w.resetAt) {
		w = &window{resetAt: now.Add(r.window)}
		m[id] = w
	}
	return w
}
