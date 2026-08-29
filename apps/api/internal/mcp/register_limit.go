package mcp

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// ---------------------------------------------------------------------------
// THE ANONYMOUS-REGISTRATION DEFENCE.
//
// POST /oauth/mcp/register is unauthenticated BY SPECIFICATION (RFC 7591): a
// GUI client enrols itself before any human has signed in, so there is nobody
// to attribute the request to and no session to gate it on. Mounting it means
// anyone on the internet can create rows in mcp_oauth_clients.
//
// WHAT THAT DOES NOT BUY AN ATTACKER, so the defence is sized to the real harm
// rather than an imagined one: the table carries no tenant_id by design, a
// client_id is not a secret (RFC 6749 2.2), possession authorizes nothing --
// every request still resolves to a grant under that grant's tenant -- and the
// table is invisible to any transaction that sets neither of its two GUCs.
// Registration alone reaches nothing.
//
// WHAT IT DOES BUY IS UNBOUNDED ANONYMOUS ROW CREATION, which is a storage and
// availability problem rather than a confidentiality one. That is what this
// bounds.
//
// ---------------------------------------------------------------------------
// WHY THE KEY IS RemoteIP AND NEVER ClientIP.
//
// THIS IS THE WHOLE DESIGN, and getting it wrong produces a limiter that
// reports success while enforcing nothing -- the exact "absence coerced into a
// plausible value" shape this endpoint is most likely to fail in.
//
// gin's Engine defaults are ForwardedByClientIP: true, RemoteIPHeaders:
// ["X-Forwarded-For", "X-Real-IP"], and trustedCIDRs: 0.0.0.0/0 + ::/0 (see
// gin@v1.12.0 gin.go:39 and gin.go:214-226). SetTrustedProxies is never called
// anywhere in this repository -- grep it. Every proxy is therefore trusted, so
// Context.ClientIP walks X-Forwarded-For right-to-left, finds every hop
// "trusted", reaches index 0 and returns the LEFTMOST entry: a string the
// caller put there. On an unauthenticated public endpoint that means a limiter
// keyed on ClientIP hands a fresh, empty bucket to every request that varies
// one header, and it does so silently, at full speed, while looking correct in
// the code and in the logs. TestRegisterLimiter_ClientIPWouldHaveBeenSpoofable
// executes that bypass rather than asserting it from this comment.
//
// RemoteIP is the TCP peer from Request.RemoteAddr. It cannot be set by a
// header. It is the only unspoofable identity available at this layer.
//
// The cost of that choice is stated rather than hidden: behind the Google
// Cloud load balancer the TCP peer IS the balancer, so every request shares one
// peer bucket and the per-peer layer degrades into a second global cap. That is
// a loss of fairness, never a loss of the bound -- it fails toward refusing too
// much, not toward permitting too much. On a self-hosted install with the API
// directly exposed the same key is a true per-source limit. Repointing this at
// a header to recover per-client fairness behind the balancer requires
// SetTrustedProxies to be configured first, and that is a deployment decision
// for the whole engine, not a change to make here.
//
// ---------------------------------------------------------------------------
// WHY THERE ARE TWO LAYERS.
//
// The global bucket is the SECURITY BOUND. It is keyed on nothing, so no header
// and no source diversity can enlarge it, and it is never reset or evicted. A
// botnet on ten thousand addresses gets the same ceiling as one host.
//
// The per-peer bucket is FAIRNESS, and it is explicitly not the bound: its map
// is capacity-limited, and a limiter evicted to keep memory finite is a fresh
// empty bucket for that peer. Eviction is therefore fail-open for this layer by
// construction, which is tolerable only because the global layer sits behind it
// and cannot be evicted. If the global layer is ever removed, this comment is
// wrong and so is the code.
//
// Both are per-process. A multi-instance deployment multiplies the ceiling by
// the instance count; the honest bound on N instances is
// N * RegisterGlobalPerMin. A cross-instance cap needs shared state (Redis) or
// a database-side row cap with GC, and neither is in this slice -- the
// database-side cap is database-engineer's path.
// ---------------------------------------------------------------------------

const (
	// RegisterGlobalPerMin caps registrations this process accepts per minute
	// across ALL sources. Unspoofable and never evicted; this is the bound.
	// Sized far above any plausible legitimate rate -- a human enrolling a GUI
	// client registers once, and a group of new operators onboarding together
	// is still nowhere near one per second.
	RegisterGlobalPerMin = 60

	// RegisterPerPeerPerMin caps registrations per TCP peer per minute. Only
	// meaningful where the peer is the caller (direct exposure, self-host); it
	// collapses into a second global cap behind a load balancer.
	RegisterPerPeerPerMin = 10

	// registerPeerCap bounds the per-peer map so an attacker varying source
	// addresses cannot grow it without limit. Reaching it is a memory bound
	// being enforced, not an error.
	registerPeerCap = 4096

	// registerPeerIdle is how long an untouched peer bucket survives a sweep.
	registerPeerIdle = 10 * time.Minute
)

// registrationLimiter is the two-layer token bucket described above. The zero
// value is NOT usable: construct it with newRegistrationLimiter.
type registrationLimiter struct {
	mu      sync.Mutex
	global  *rate.Limiter
	peers   map[string]*peerBucket
	perPeer int
	cap     int
	idle    time.Duration
}

type peerBucket struct {
	lim  *rate.Limiter
	seen time.Time
}

// newRegistrationLimiter builds the limiter. There is no janitor goroutine and
// therefore nothing to Stop and nothing to leak: the peer map is swept inline
// when it grows past its cap, which is the only moment the memory bound is
// actually at risk.
func newRegistrationLimiter(globalPerMin, perPeerPerMin int) *registrationLimiter {
	return &registrationLimiter{
		global:  perMinuteLimiter(globalPerMin),
		peers:   make(map[string]*peerBucket),
		perPeer: perPeerPerMin,
		cap:     registerPeerCap,
		idle:    registerPeerIdle,
	}
}

// perMinuteLimiter turns a per-minute budget into a token bucket whose burst is
// the whole budget, so the limit is a cap on any rolling one-minute window
// rather than a hard inter-arrival gap. Matches internal/autologin's shape.
func perMinuteLimiter(perMin int) *rate.Limiter {
	if perMin <= 0 {
		// A non-positive budget means "allow nothing", never "allow everything".
		// rate.NewLimiter(0, 0) rejects every request, which is the direction a
		// misconfiguration must fail in.
		return rate.NewLimiter(0, 0)
	}
	return rate.NewLimiter(rate.Limit(float64(perMin)/60.0), perMin)
}

// allow consumes one token from the global bucket and one from the peer's.
//
// The global bucket is consulted FIRST and, when it refuses, the peer bucket is
// left untouched -- a rejected request must not also spend the caller's own
// fair-share budget.
//
// A nil receiver returns false. An unconstructed limiter is a wiring failure,
// and the one thing it must never do is read as "no limit configured,
// therefore permitted".
func (l *registrationLimiter) allow(peer string) (bool, time.Duration) {
	if l == nil {
		return false, time.Minute
	}

	now := time.Now()

	l.mu.Lock()
	defer l.mu.Unlock()

	if ok, wait := reserveOne(l.global, now); !ok {
		return false, wait
	}

	// An empty peer string means RemoteAddr could not be parsed. It is bucketed
	// under a single reserved key rather than skipped: "we could not identify
	// the source" must be more restrictive than identifying it, never less.
	if peer == "" {
		peer = "\x00unknown"
	}

	b, ok := l.peers[peer]
	if !ok {
		l.sweepLocked(now)
		b = &peerBucket{lim: perMinuteLimiter(l.perPeer)}
		l.peers[peer] = b
	}
	b.seen = now

	if ok, wait := reserveOne(b.lim, now); !ok {
		return false, wait
	}
	return true, 0
}

// reserveOne takes one token if one is available now, and otherwise cancels the
// reservation so a later polite retry is not penalised for this attempt.
func reserveOne(lim *rate.Limiter, now time.Time) (bool, time.Duration) {
	r := lim.ReserveN(now, 1)
	if !r.OK() {
		// Burst cannot satisfy a single token: the budget is zero by
		// configuration. Refuse.
		return false, time.Minute
	}
	if wait := r.DelayFrom(now); wait > 0 {
		r.CancelAt(now)
		return false, wait
	}
	return true, 0
}

// sweepLocked bounds the peer map. Idle buckets go first; if that is not enough
// the map is cleared outright.
//
// Clearing is deliberate and its consequence is accepted: it hands every peer a
// fresh bucket, so the per-peer layer is fail-open under memory pressure. It is
// safe ONLY because the global bucket above is not swept and still refuses.
// Caller holds l.mu.
func (l *registrationLimiter) sweepLocked(now time.Time) {
	if len(l.peers) < l.cap {
		return
	}
	for k, b := range l.peers {
		if now.Sub(b.seen) > l.idle {
			delete(l.peers, k)
		}
	}
	if len(l.peers) >= l.cap {
		l.peers = make(map[string]*peerBucket)
	}
}
