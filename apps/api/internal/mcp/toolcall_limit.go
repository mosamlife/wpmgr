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

	// ---------------------------------------------------------------------
	// THE BURST IS A SEPARATE NUMBER AND IS PART OF THE PUBLISHED CONTRACT.
	//
	// These buckets are token buckets, so the sustained rate above is NOT the
	// most calls that can occur in a 60-second window. A bucket starting full
	// admits its whole burst immediately and then admits the refill on top, so
	// the true worst case over the first minute is BURST + SUSTAINED --
	// measured, not reasoned: 240 for the tenant bucket and 120 for the grant
	// bucket, exactly twice the sustained figures, which is what the refusal
	// used to advertise as though it were the ceiling.
	//
	// WHY THE FIX IS TO PUBLISH BOTH NUMBERS RATHER THAN SHRINK THE BURST.
	// For burst B and sustained N, the worst-case 60s window is B + N. Making
	// the single advertised number true would require B = 0, which is not a
	// smaller burst but no burst at all: the limiter degrades into a hard
	// inter-arrival gap of one call every 500ms, and an ordinary interactive
	// turn -- a model making several tool calls to answer one question -- is
	// refused halfway through. A single number that is true of neither the
	// first minute nor the steady state is the actual defect; the mechanism is
	// fine and was simply being described wrongly.
	//
	// THE BURST IS SIZED TO AN INTERACTIVE TURN, NOT TO THE WHOLE MINUTE.
	//
	// Leaving burst equal to the sustained allowance -- which is what the
	// shared helper did implicitly -- has two costs. It lets a bucket that has
	// been idle admit a full minute's traffic in one instant, which on this
	// endpoint is that many database writes at once. And it makes the two
	// published numbers identical, so a client cannot tell them apart and the
	// pair carries no more information than the single figure it replaced.
	//
	// These values are sized from the workload the over-fire proof already
	// pins: an ordinary interactive session is around twenty calls, so a grant
	// burst of thirty covers a generous turn with margin, and a tenant burst of
	// sixty covers two connections doing that at once. The worst-case first
	// minute falls to 90 for a connection and 160 for an organisation, against
	// 120 and 240 before -- so this also narrows the gap between the advertised
	// sustained rate and the true ceiling, rather than only describing it.
	ToolCallTenantBurst = 60
	ToolCallGrantBurst  = 30

	// toolCallKeyCap bounds each key map. Reaching it is a memory bound being
	// enforced, not an error.
	toolCallKeyCap = 4096

	// toolCallIdle is how long an untouched bucket survives a sweep.
	toolCallIdle = 10 * time.Minute
)

// toolCallBucket builds one bucket from an EXPLICIT sustained rate and burst.
//
// Deliberately not perMinuteLimiter, which is shared with the unauthenticated
// registration limiter: that endpoint's tuning is not in this slice, and
// changing a helper underneath another endpoint to fix this one's advertised
// numbers would be a silent behaviour change to /oauth/mcp/register.
//
// A non-positive rate or burst means "allow nothing", never "allow everything"
// -- the direction a misconfiguration has to fail in.
func toolCallBucket(sustainedPerMin, burst int) *rate.Limiter {
	if sustainedPerMin <= 0 || burst <= 0 {
		return rate.NewLimiter(0, 0)
	}
	return rate.NewLimiter(rate.Limit(float64(sustainedPerMin)/60.0), burst)
}

// toolCallLimiter is the two-layer bucket described above. The zero value is
// NOT usable: construct it with newToolCallLimiter.
type toolCallLimiter struct {
	mu           sync.Mutex
	tenants      map[uuid.UUID]*keyBucket
	grants       map[uuid.UUID]*keyBucket
	tenantPerMin int
	grantPerMin  int
	tenantBurst  int
	grantBurst   int
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
func newToolCallLimiter(tenantPerMin, grantPerMin, tenantBurst, grantBurst int) *toolCallLimiter {
	return &toolCallLimiter{
		tenants:      make(map[uuid.UUID]*keyBucket),
		grants:       make(map[uuid.UUID]*keyBucket),
		tenantPerMin: tenantPerMin,
		grantPerMin:  grantPerMin,
		tenantBurst:  tenantBurst,
		grantBurst:   grantBurst,
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

	l.mu.Lock()
	defer l.mu.Unlock()

	// THE CLOCK IS SAMPLED AFTER THE LOCK, AND THAT ORDERING IS LOAD-BEARING.
	//
	// rate.Limiter advances its internal state monotonically from whatever
	// timestamp it is handed and refuses to go backwards. Sampling before the
	// lock lets goroutine A read t1, block on the mutex while goroutine B
	// advances the bucket to t2 > t1, and then evaluate its request against t1
	// -- a state the limiter has already moved past. The accounting comes out
	// wrong, in whichever direction the interleaving happens to produce, and
	// NO SINGLE-THREADED TEST CAN EVER SHOW IT.
	//
	// Reading the clock inside the critical section makes the timestamp and the
	// bucket state consistent by construction. It is one syscall-free
	// monotonic read inside a lock this function already holds for the whole
	// decision, so it costs nothing worth trading for.
	now := time.Now()

	// ---- QUERY ONLY. Nothing below this line is charged until both pass. ----

	tb := l.tenantBucketLocked(tenantID, now)
	if wait := shortfall(tb.lim, now); wait > 0 {
		return toolCallDecision{Allowed: false, RetryAfter: atLeastASecond(wait), Scope: scopeOrganisation}
	}

	gb := l.grantBucketLocked(grantID, now)
	if wait := shortfall(gb.lim, now); wait > 0 {
		return toolCallDecision{Allowed: false, RetryAfter: atLeastASecond(wait), Scope: scopeConnection}
	}

	// ---- ADMITTED. The one and only mutation site. ----
	tb.lim.AllowN(now, 1)
	gb.lim.AllowN(now, 1)
	return toolCallDecision{Allowed: true}
}

// tenantBucketLocked fetches or creates one tenant's bucket -- THE BOUND.
//
// It sweeps with sweepFullOnly, which can only ever evict a bucket that has
// already refilled. There is deliberately no way to reach the clearing sweep
// from here: the two accessors exist as separate functions, rather than one
// taking the map as a parameter, precisely so a later edit cannot hand the
// bound map the fairness map's sweeper by passing the wrong argument.
//
// The bucket is touched on REFUSAL as well as admission, so a flooding key's
// bucket stays alive rather than being swept and handed back empty, which would
// reset the very limit it is hitting. Caller holds l.mu.
func (l *toolCallLimiter) tenantBucketLocked(key uuid.UUID, now time.Time) *keyBucket {
	b, ok := l.tenants[key]
	if !ok {
		sweepFullOnly(l.tenants, l.cap, now)
		b = &keyBucket{lim: toolCallBucket(l.tenantPerMin, l.tenantBurst)}
		l.tenants[key] = b
	}
	b.seen = now
	return b
}

// grantBucketLocked fetches or creates one connection's bucket -- FAIRNESS.
//
// It sweeps with sweep, which may clear the map outright. That is safe here and
// only here: the tenant bucket behind this layer is swept losslessly and still
// refuses, so the ceiling survives any eviction of this map. Caller holds l.mu.
func (l *toolCallLimiter) grantBucketLocked(key uuid.UUID, now time.Time) *keyBucket {
	b, ok := l.grants[key]
	if !ok {
		sweep(l.grants, l.cap, l.idle, now)
		b = &keyBucket{lim: toolCallBucket(l.grantPerMin, l.grantBurst)}
		l.grants[key] = b
	}
	b.seen = now
	return b
}

// THE TWO MAPS ARE SWEPT BY DIFFERENT RULES, BECAUSE THEY HAVE DIFFERENT JOBS.
//
// This is the correction to a real defect. Both maps were originally swept by
// one function copied in shape from registrationLimiter.sweepLocked, which
// clears its map outright under pressure. That file justifies clearing in one
// sentence -- it is "safe ONLY because the global bucket above is not swept and
// still refuses" -- and this limiter deliberately HAS NO GLOBAL BUCKET, for the
// starvation reason argued at the top of this file. So the shape was inherited
// without the property that made it safe: clearing the tenant map resets the
// bound itself, and a saturated tenant's budget came back the moment enough
// distinct organisations were active on one process.
//
// The consequence was scale degradation of a denial-of-service control rather
// than an isolation break -- it needs thousands of real authenticated
// organisations, not something one caller can mint -- but a bound that resets
// under load is not a bound, and the argument for clearing was never true here.
//
// THE RULE THAT REPLACES IT RESTS ON ONE OBSERVATION: A FULL BUCKET CARRIES NO
// STATE. A limiter at its burst ceiling is indistinguishable from a freshly
// constructed one, so deleting it and rebuilding it on the next request yields
// exactly the same object. Evicting full buckets is therefore LOSSLESS, and
// evicting anything below full is precisely what loses the bound.
//
// sweepFullOnly is what the tenant map gets: it can only ever drop buckets that
// have already refilled, so no tenant's budget is ever handed back early, at
// any cap and under any load. A bucket refills in at most one window (60s for a
// per-minute budget), so in steady state this evicts everything an idle sweep
// would have, and it does it with a proof instead of a hope.
//
// WHAT HAPPENS WHEN NOTHING IS EVICTABLE is the other half of the decision, and
// the map is allowed to GROW PAST THE CAP rather than take either alternative.
// Clearing would reset the bound, which is fail-open on a DoS control. Refusing
// the new tenant would fail closed on legitimate work and would be a
// cross-tenant denial of service -- one busy organisation locking new ones out
// -- which is the same defect the "no global layer" decision exists to avoid.
// Growth is the only option that is wrong in neither direction, and its cost is
// bounded by something real: a key is minted only by Service.Authenticate, so
// every entry cost an attacker a genuine authenticated credential that had to
// pass the mint limiter to exist. The cap is memory hygiene against a large
// fleet, not a defence against a cheap attack, and a soft cap is the honest
// shape for that.
//
// Caller holds the owning limiter's mutex.
func sweepFullOnly(m map[uuid.UUID]*keyBucket, cap int, now time.Time) {
	if len(m) < cap {
		return
	}
	for k, b := range m {
		// Burst is the ceiling, so tokens >= burst means fully refilled. Read
		// with TokensAt, which is a pure query and does not advance the bucket.
		if b.lim.TokensAt(now) >= float64(b.lim.Burst()) {
			delete(m, k)
		}
	}
	// Deliberately NO clear() fallback. See above: a map that cannot be swept
	// losslessly grows instead of surrendering the bound.
}

// sweep is what the GRANT map gets, and clearing is still correct there.
//
// The grant map is the FAIRNESS layer, not the bound. Handing a connection a
// fresh share early is a loss of fairness only: the tenant bucket behind it is
// swept losslessly and still refuses, so the ceiling holds however this map is
// evicted. That is the same argument registrationLimiter makes for its per-peer
// map, and here -- unlike in the tenant case above -- the property it depends
// on is actually present.
//
// Idle buckets go first; if that is not enough the map is emptied outright.
//
// Caller holds the owning limiter's mutex.
func sweep(m map[uuid.UUID]*keyBucket, cap int, idle time.Duration, now time.Time) {
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
