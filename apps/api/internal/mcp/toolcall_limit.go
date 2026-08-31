package mcp

import (
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/time/rate"
)

// ---------------------------------------------------------------------------
// THE TOOL-CALL BOUND (1A-11).
//
// POST /mcp authenticates a bearer connection token on every request and then,
// before this file existed, dispatched tools/list and tools/call without any
// budget at all. /oauth/mcp/register and POST /api/v1/mcp/connections were both
// limited; the one endpoint a model actually drives in a loop was not. An
// authenticated client -- or a leaked token, or a model stuck in a retry loop
// -- could call tools without bound, and every call performs an authenticated
// database write through Service.RecordActivity before it performs the read.
//
// ---------------------------------------------------------------------------
// WHY THE KEY IS THE TENANT AND THE GRANT, AND WHY NEITHER CAN BE SPOOFED.
//
// Both keys come off AuthorizedRequest, which Service.Authenticate built by
// hashing the bearer credential, looking the row up, and re-checking it under
// the resolved tenant inside a tenant transaction. NEITHER VALUE IS EVER READ
// FROM THE REQUEST. There is no ClientIP call, no GetHeader call and no
// RemoteAddr parse anywhere in this file or on the path that reaches it, so
// there is no header an attacker can vary to obtain a fresh bucket. That is the
// property TestToolCallLimiter_KeysAreImmuneToClientHeaders executes rather
// than asserts from this comment.
//
// This is a DELIBERATE DIVERGENCE from registrationLimiter next door, and the
// reason is that the two endpoints differ in whether the caller is known.
// /register is unauthenticated by RFC 7591, so the only unspoofable identity
// available there is the TCP peer, and the bound has to be a global bucket
// because a botnet supplies unlimited peers. Here the caller is authenticated
// before this code runs, so the tenant IS the accountable identity and the
// bound can be attributed to it.
//
// ---------------------------------------------------------------------------
// WHY THE TENANT IS THE BOUND AND THE GRANT IS ONLY FAIRNESS.
//
// The grant cannot be the bound. A tenant that wants more budget would simply
// mint more connections: minting is rate-limited but not forbidden, so a
// per-grant bound is escapable by anyone willing to hold N credentials, which
// makes it not a bound at all.
//
// The token cannot be the bound either, for the same reason one level down: a
// grant may carry several connection tokens over its life and rotating a token
// would hand the caller a fresh empty bucket, so the budget would reset on
// exactly the action an operator takes when something is already wrong.
//
// The tenant is the one key an authenticated caller cannot multiply. The grant
// layer sits in front of it as FAIRNESS ONLY, so one runaway model loop on one
// connection cannot consume the whole organisation's budget and starve the
// operator's other connections. It is explicitly not the bound, and if the
// tenant layer is ever removed this comment is wrong and so is the code.
//
// ---------------------------------------------------------------------------
// WHY THERE IS NO GLOBAL LAYER, WHICH IS THE OTHER DIVERGENCE.
//
// registrationLimiter has one because it must: an unauthenticated endpoint
// cannot attribute load, so the only bound available is process-wide. Adding
// the same layer here would REINTRODUCE THE EXACT STARVATION DEFECT that
// file's own comment describes fixing -- one tenant saturating a shared bucket
// denies service to every other tenant on the instance, and on Cloud Run that
// is a cross-tenant availability event caused by a single compromised
// credential. Since the caller here is always attributable, the correct ceiling
// is per-tenant and the correct answer to a saturating tenant is to refuse that
// tenant. The body cap in serve() already bounds the memory side.
//
// Both layers are per-process, so an N-instance deployment multiplies the
// ceiling by N; the honest bound on N instances is N * ToolCallTenantPerMin. A
// cross-instance cap needs shared state and is not in this slice.
// ---------------------------------------------------------------------------

const (
	// ToolCallTenantPerMin is THE BOUND: tool-invoking requests one tenant may
	// make per minute, per process, across every connection it holds.
	//
	// Sized against the real workload rather than a round number. Phase 1
	// exposes one read tool over a fleet; an interactive model answering a
	// question about that fleet makes a handful of calls per turn, and a
	// generous human conversation is a few turns per minute. Two per second
	// sustained is far above that and far below what a retry loop produces.
	ToolCallTenantPerMin = 120

	// ToolCallGrantPerMin is FAIRNESS, not the bound: the ceiling one
	// connection may consume of its organisation's budget. Deliberately half
	// the tenant budget, so a single looping connection cannot starve its
	// siblings but a lone connection on a one-connection tenant still gets a
	// workable share.
	ToolCallGrantPerMin = 60

	// toolCallKeyCap bounds each key map. Reaching it is a memory bound being
	// enforced, not an error.
	toolCallKeyCap = 4096

	// toolCallIdle is how long an untouched bucket survives a sweep.
	toolCallIdle = 10 * time.Minute
)

// toolCallLimiter is the two-layer bucket described above. The zero value is
// NOT usable: construct it with newToolCallLimiter.
type toolCallLimiter struct {
	mu           sync.Mutex
	tenants      map[uuid.UUID]*keyBucket
	grants       map[uuid.UUID]*keyBucket
	tenantPerMin int
	grantPerMin  int
	cap          int
	idle         time.Duration
}

type keyBucket struct {
	lim  *rate.Limiter
	seen time.Time
}

// newToolCallLimiter builds the limiter. Like its sibling it runs no janitor
// goroutine: each map is swept inline when it grows past the cap, which is the
// only moment the memory bound is actually at risk.
func newToolCallLimiter(tenantPerMin, grantPerMin int) *toolCallLimiter {
	return &toolCallLimiter{
		tenants:      make(map[uuid.UUID]*keyBucket),
		grants:       make(map[uuid.UUID]*keyBucket),
		tenantPerMin: tenantPerMin,
		grantPerMin:  grantPerMin,
		cap:          toolCallKeyCap,
		idle:         toolCallIdle,
	}
}

// limitScope names WHICH budget refused, so the refusal can tell the operator
// whether to look at one connection or at the whole organisation. It is part of
// the wire contract: it appears in the JSON-RPC error's data.
type limitScope string

const (
	// scopeOrganisation means the tenant-wide bound refused. Every connection
	// this organisation holds is affected.
	scopeOrganisation limitScope = "organisation"

	// scopeConnection means only this connection's fairness share refused. The
	// organisation's other connections are unaffected.
	scopeConnection limitScope = "connection"
)

// toolCallDecision is the limiter's verdict. RetryAfter is meaningful only when
// Allowed is false, and is always at least one second when refusing so a caller
// rounding it down to whole seconds never computes a zero wait and retries
// immediately -- which is the retry loop this whole mechanism exists to stop.
type toolCallDecision struct {
	Allowed    bool
	RetryAfter time.Duration
	Scope      limitScope
}

// allow admits a tool-invoking request only when BOTH buckets can pay for it,
// and charges BOTH only when it is admitted.
//
// A REFUSED REQUEST COSTS NOTHING, IN EITHER BUCKET, AND THAT IS STRUCTURAL --
// the same shape, and for the same reason, as registrationLimiter.allow. Every
// rejection path runs BEFORE any mutation and there is exactly ONE mutation
// site, reached only on the admit path, so a future branch added to a rejection
// path cannot leak a token: at the point those branches run there is nothing
// yet to leak. Without this, a connection already over its own share would
// still spend its organisation's budget on every request it was REFUSED for,
// and the fairness layer would become the denial-of-service.
//
// The tenant bucket is checked FIRST so a saturated tenant does not allocate a
// grant bucket per credential it holds. Because nothing is charged until both
// checks pass, the ordering is a memory optimisation and not the invariant.
//
// A nil receiver REFUSES. An unconstructed limiter is a wiring failure, and the
// one thing it must never do is read as "no limit configured, therefore
// permitted".
func (l *toolCallLimiter) allow(tenantID, grantID uuid.UUID) toolCallDecision {
	if l == nil {
		return toolCallDecision{Allowed: false, RetryAfter: time.Minute, Scope: scopeOrganisation}
	}

	now := time.Now()

	l.mu.Lock()
	defer l.mu.Unlock()

	// ---- QUERY ONLY. Nothing below this line is charged until both pass. ----

	tb := l.bucketLocked(l.tenants, tenantID, l.tenantPerMin, now)
	if wait := shortfall(tb.lim, now); wait > 0 {
		return toolCallDecision{Allowed: false, RetryAfter: atLeastASecond(wait), Scope: scopeOrganisation}
	}

	gb := l.bucketLocked(l.grants, grantID, l.grantPerMin, now)
	if wait := shortfall(gb.lim, now); wait > 0 {
		return toolCallDecision{Allowed: false, RetryAfter: atLeastASecond(wait), Scope: scopeConnection}
	}

	// ---- ADMITTED. The one and only mutation site. ----
	tb.lim.AllowN(now, 1)
	gb.lim.AllowN(now, 1)
	return toolCallDecision{Allowed: true}
}

// bucketLocked fetches or creates the bucket for one key, sweeping the map
// first when a new key would push it past the cap. The bucket is touched on
// REFUSAL as well as admission -- deliberately, so a flooding key's bucket
// stays alive rather than being swept and handed back empty, which would reset
// the very limit it is hitting. Caller holds l.mu.
func (l *toolCallLimiter) bucketLocked(
	m map[uuid.UUID]*keyBucket, key uuid.UUID, perMin int, now time.Time,
) *keyBucket {
	b, ok := m[key]
	if !ok {
		sweepLocked(m, l.cap, l.idle, now)
		b = &keyBucket{lim: perMinuteLimiter(perMin)}
		m[key] = b
	}
	b.seen = now
	return b
}

// sweepLocked bounds one key map. Idle buckets go first; if that is not enough
// the map is emptied outright.
//
// Emptying hands every key a fresh bucket, so this layer is fail-open under
// memory pressure -- the same accepted consequence as its sibling, but the
// exposure here is much smaller and it is worth saying why rather than assuming
// it transfers. registrationLimiter's peer keys are IP addresses, which an
// attacker supplies for free, so filling that map is a cheap deliberate act.
// The keys here are tenant and grant UUIDs that only Service.Authenticate
// mints, so filling either map requires authenticating that many distinct real
// credentials, each of which had to pass the mint limiter to exist. The cap is
// memory hygiene against a genuinely large fleet, not a defence against a
// cheap attack.
//
// Caller holds the owning limiter's mutex.
func sweepLocked(m map[uuid.UUID]*keyBucket, cap int, idle time.Duration, now time.Time) {
	if len(m) < cap {
		return
	}
	for k, b := range m {
		if now.Sub(b.seen) > idle {
			delete(m, k)
		}
	}
	if len(m) >= cap {
		clear(m)
	}
}

// atLeastASecond floors a refusal's advertised wait at one second.
//
// Retry-After is expressed in whole seconds, and a sub-second shortfall
// rendered into that header truncates to "0" -- which tells a client to retry
// at once, producing precisely the tight loop the refusal is meant to break.
// Rounding a refusal's wait UP is always safe; rounding it down is not.
func atLeastASecond(d time.Duration) time.Duration {
	if d < time.Second {
		return time.Second
	}
	return d
}
