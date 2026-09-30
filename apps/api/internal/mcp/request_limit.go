package mcp

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

// The per-process limits on the request tool (design 4.13): per connection
// 10 a minute with a burst of 3, per site 2 a minute with a burst of 1. They
// sit in front of the durable caps, which do not depend on the replica count.
const (
	requestRateConnectionPerMin = 10
	requestRateConnectionBurst  = 3
	requestRateSitePerMin       = 2
	requestRateSiteBurst        = 1

	limitScopeRequestConnection = "request_rate_connection"
	limitScopeRequestSite       = "request_rate_site"
)

// requestRateLimiter is the two-bucket limiter for site_cache_purge_request.
// It is toolCallLimiter's shape: a refused request costs nothing in either
// bucket, the clock is read inside the lock, and a nil receiver refuses.
type requestRateLimiter struct {
	mu    sync.Mutex
	conns map[uuid.UUID]*keyBucket
	sites map[uuid.UUID]*keyBucket
}

func newRequestRateLimiter() *requestRateLimiter {
	return &requestRateLimiter{
		conns: make(map[uuid.UUID]*keyBucket),
		sites: make(map[uuid.UUID]*keyBucket),
	}
}

type requestRateDecision struct {
	allowed    bool
	retryAfter time.Duration
	scope      string
}

func (l *requestRateLimiter) allow(grantID, siteID uuid.UUID) requestRateDecision {
	if l == nil {
		return requestRateDecision{retryAfter: time.Minute, scope: limitScopeRequestConnection}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()

	cb := requestBucketLocked(l.conns, grantID, requestRateConnectionPerMin, requestRateConnectionBurst, now)
	if wait := shortfall(cb.lim, now); wait > 0 {
		return requestRateDecision{retryAfter: atLeastASecond(wait), scope: limitScopeRequestConnection}
	}
	sb := requestBucketLocked(l.sites, siteID, requestRateSitePerMin, requestRateSiteBurst, now)
	if wait := shortfall(sb.lim, now); wait > 0 {
		return requestRateDecision{retryAfter: atLeastASecond(wait), scope: limitScopeRequestSite}
	}
	cb.lim.AllowN(now, 1)
	sb.lim.AllowN(now, 1)
	return requestRateDecision{allowed: true}
}

func requestBucketLocked(m map[uuid.UUID]*keyBucket, key uuid.UUID, perMin, burst int, now time.Time) *keyBucket {
	b, ok := m[key]
	if !ok {
		sweepFullOnly(m, toolCallKeyCap, now)
		b = &keyBucket{lim: toolCallBucket(perMin, burst)}
		m[key] = b
	}
	b.seen = now
	return b
}
