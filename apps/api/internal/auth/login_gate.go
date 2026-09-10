package auth

import (
	"context"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"runtime"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// ---------------------------------------------------------------------------
// Login admission control — PHASE 0 (GH #718).
//
// WHAT THIS PHASE DOES AND DOES NOT DO.
//
// POST /auth/login admits every attempt it is given, and each admitted attempt
// runs a full argon2id verification at the 19 MiB profile (passwordParams, see
// password.go). Phase 0 adds two things and deliberately nothing else:
//
//  1. MEASUREMENT. Every key a later phase would key a refusal on is computed
//     here, and every budget is evaluated here, but no attempt is refused and
//     no budget is charged on a would-refuse. The point is to learn the real
//     distribution before choosing numbers, rather than to guess them. The
//     mode is WPMGR_AUTH_LOGIN_MODE and it defaults to "observe".
//
//  2. A CONCURRENCY BOUND on the password verification itself, which DOES
//     enforce from day one. It is not a rate limit and it is not keyed on
//     anything the caller controls: it sheds only when more verifications are
//     already in flight than this process is willing to run at once. That is
//     saturation, which is observable without any prior measurement, so it
//     needs no observation period behind it.
//
// WHY THE IDENTITY-KEYED BUDGETS ARE NOT ENFORCED YET.
//
// An earlier attempt keyed a bucket on the submitted email alone. Anyone who
// knew an address could then spend that address's budget and lock its owner
// out of their own account — the limiter became the denial of service. The
// budgets below are shaped to avoid that (the pair scope is the tight one, the
// account scope is loose and exists to catch a distributed attempt), but which
// numbers are safe is an empirical question. Phase 1 answers it with the lines
// this file emits.
//
// WHAT OBSERVATION MUST NOT COST.
//
// This runs on an UNAUTHENTICATED endpoint, before any account lookup, so it
// must be invisible to the caller:
//
//   - It never branches on whether an account exists. It runs before
//     Service.Login and therefore before GetUserByEmail; it is handed an email
//     string and nothing else, and it cannot tell a real address from a
//     fabricated one. There is no new enumeration oracle here because there is
//     nothing here that knows the answer.
//   - It changes no status, no body and no header in observe mode.
//   - It never logs an email address. The account scope is identified by
//     acctH, a keyed digest, exactly as reset.go and twofa.go identify an
//     account in a log line without naming it.
//
// WHY THE EMAIL IS DIGESTED RATHER THAN USED AS A KEY.
//
// loginInput.Email carries `validate:"required,email"` with no max length, and
// no auth route installs a MaxBytesReader, so the submitted address is
// attacker-sized. A raw email map key would let a caller choose how much memory
// each of its attempts retains. acctH is 16 hex characters whatever was
// submitted, so the map's memory is a function of its entry count alone, and
// that count is capped.
//
// The digest is keyed with a process key derived from the instance session
// secret via HKDF under its own info string, following newHandshakeCodec
// (social_handshake.go:127-146) and obeying the rule stated at :80-83 there:
// two purposes never share one derived key. Keying it matters — an unkeyed
// hash of an email address is reversible by anyone with a word list, which
// would put the address back in the log in all but name.
// ---------------------------------------------------------------------------

// LoginMode selects what the gate does with its own verdict.
type LoginMode string

const (
	// LoginModeObserve evaluates every budget and refuses nothing. Phase 0's
	// default and, until Phase 1 lands, the only mode with numbers behind it.
	LoginModeObserve LoginMode = "observe"

	// LoginModeEnforce is Phase 1's mode. It is accepted here so the switch
	// exists and is testable, and it is NOT reachable by accident: it has to be
	// asked for by name in WPMGR_AUTH_LOGIN_MODE.
	LoginModeEnforce LoginMode = "enforce"
)

// LoginModeEnvVar is the variable that selects the mode. Named as a constant so
// the startup line, the periodic reminder and the config layer cannot drift
// apart on its spelling.
const LoginModeEnvVar = "WPMGR_AUTH_LOGIN_MODE"

// ParseLoginMode converts the configured string. An empty value is the default
// rather than an error, so an install that has never heard of this variable
// boots in observe. Anything else is refused rather than silently coerced: a
// typo'd mode must not read as "the safe one" in Phase 0 and then as "off" in
// Phase 1.
func ParseLoginMode(s string) (LoginMode, error) {
	switch LoginMode(s) {
	case "":
		return LoginModeObserve, nil
	case LoginModeObserve:
		return LoginModeObserve, nil
	case LoginModeEnforce:
		return LoginModeEnforce, nil
	default:
		return "", fmt.Errorf("%s: %q is not a mode; use %q or %q", LoginModeEnvVar, s, LoginModeObserve, LoginModeEnforce)
	}
}

const (
	// loginGateKeyInfo separates the account-digest key from every other use of
	// the session secret. See social_handshake.go:80-83.
	loginGateKeyInfo = "wpmgr/login-gate/account-digest/v1"

	// loginWindow is the window every budget below is expressed over.
	loginWindow = 15 * time.Minute

	// loginPairBudget is the tight one: this source, this account. A person
	// mistyping their own password from their own machine does not reach 10 in
	// a quarter of an hour; a guessing run reaches it immediately.
	loginPairBudget = 10

	// loginSrcBudget is per source address across all accounts. Sized for a
	// shared egress (an office, a mobile carrier NAT) rather than for one
	// person, which is why it is six times the pair budget.
	loginSrcBudget = 60

	// loginSrc48Budget is per IPv6 /48 across all accounts. An IPv6 client is
	// routinely handed a /64 and often a /48, so a per-/64 budget alone is a
	// budget the client can multiply by moving a bit. This is the scope that
	// makes the v6 numbers mean anything.
	loginSrc48Budget = 240

	// loginAcctBudget is per account across all sources. Deliberately loose: in
	// Phase 1 this is the scope that could be turned into a denial of service
	// against a named person, so it exists to catch a distributed run against
	// one account, not to police one person's typing.
	loginAcctBudget = 50

	// loginBucketCap bounds each scope's map so a caller varying its key cannot
	// grow it without limit. Mirrors registerPeerCap
	// (internal/mcp/register_limit.go:96-100). Reaching it is a memory bound
	// being enforced, not an error.
	loginBucketCap = 4096

	// loginBucketIdle is how long an untouched bucket survives a sweep. It is
	// deliberately longer than loginWindow: a bucket swept while its window is
	// still running would be handed back full, which resets the very budget it
	// is recording.
	loginBucketIdle = 30 * time.Minute

	// loginModeReminderEvery is how often a non-enforce mode announces itself.
	// A kill switch flipped during an incident is forgotten when nothing keeps
	// saying it is flipped.
	loginModeReminderEvery = 5 * time.Minute
)

// Scope names. These are the strings a Phase 1 refusal would be attributed to
// and the strings the observation lines carry, so they are declared once.
const (
	loginScopePair  = "login:pair"
	loginScopeSrc   = "login:src"
	loginScopeSrc48 = "login:src48"
	loginScopeAcct  = "login:acct"
)

// addrUnresolved is the reserved key for an attempt whose source address could
// not be resolved at all. Such attempts share one bucket rather than being
// skipped: "we could not identify the source" must never be the cheapest way
// to be admitted. Note for Phase 1 — enforcing on this shared key is a
// deliberate decision to make, not a default to inherit, because a topology
// that resolves no addresses would put the entire fleet in it.
const addrUnresolved = "\x00unresolved"

// loginAttempt is what the handler hands the gate. It is everything the gate is
// allowed to know, which is deliberately less than the handler knows: no user
// id, no account existence, no password, no request body beyond the address
// that was submitted as the identity.
type loginAttempt struct {
	// Addr is limiterAddr's result: the address a rate-limit DECISION may be
	// keyed on. Invalid when it could not be resolved.
	Addr netip.Addr
	// FromChain reports whether Addr was read from the forwarded chain at the
	// configured hop position. See Handler.limiterAddrSource.
	FromChain bool
	// Hops is the effective proxy hop count, needed only to describe how Addr
	// was obtained in the log line.
	Hops int
	// Email is the address as submitted. It is normalised and digested here and
	// never retained, never logged, and never compared against anything.
	Email string
}

// addrSource describes, for an operator reading the log, where Addr came from.
// The distinction that matters is peer_fallback: it means the forwarded chain
// was shorter than WPMGR_AUTH_PROXY_HOPS claims, so either the hop count is
// wrong or this process is reachable without the proxies it is configured for.
// Either way every client behind that path collapses onto one key, and in
// Phase 1 that would refuse them all together.
func (a loginAttempt) addrSource() string {
	switch {
	case !a.Addr.IsValid():
		return "unresolved"
	case a.Hops == 0:
		return "peer_configured"
	case a.FromChain:
		return "chain"
	default:
		return "peer_fallback"
	}
}

// LoginGate is the admission control described at the top of this file.
//
// A nil *LoginGate is safe to call and does nothing at all — no observation and
// no concurrency bound. That is a wiring failure, not a mode, and it is exactly
// what LogAdmissionStartup exists to make visible in the first screen of the
// log rather than in a quiet absence of lines.
type LoginGate struct {
	mode    LoginMode
	procKey []byte

	pair  *keyedBudget
	src   *keyedBudget
	src48 *keyedBudget
	acct  *keyedBudget

	verify *verifySemaphore

	logger *slog.Logger

	// now is time.Now except in tests. Every budget in one evaluation reads it
	// once, so the four verdicts describe a single instant.
	now func() time.Time
}

// NewLoginGate builds the gate.
//
// sessionSecret keys the account digest. There is no unkeyed fallback: the
// secret is already required to exist and to be non-trivial before the process
// boots (config.ValidateSessionSecret), so a gate either has a real key or is
// not built.
//
// maxConcurrentVerify bounds concurrent password verifications; see
// defaultVerifyConcurrency for what a non-positive value means.
func NewLoginGate(sessionSecret string, mode LoginMode, maxConcurrentVerify int) (*LoginGate, error) {
	if len(sessionSecret) < 32 {
		return nil, errors.New("login gate: session secret too short to derive a key from")
	}
	if mode != LoginModeObserve && mode != LoginModeEnforce {
		return nil, fmt.Errorf("login gate: unknown mode %q", mode)
	}
	// No salt, for the same reason newHandshakeCodec has none: the input is a
	// high-entropy instance secret, and a random salt would have to be stored
	// for a later process to derive the same key.
	key, err := hkdf.Key(sha256.New, []byte(sessionSecret), nil, loginGateKeyInfo, 32)
	if err != nil {
		return nil, err
	}
	return &LoginGate{
		mode:    mode,
		procKey: key,
		pair:    newKeyedBudget(loginScopePair, loginPairBudget),
		src:     newKeyedBudget(loginScopeSrc, loginSrcBudget),
		src48:   newKeyedBudget(loginScopeSrc48, loginSrc48Budget),
		acct:    newKeyedBudget(loginScopeAcct, loginAcctBudget),
		verify:  newVerifySemaphore(maxConcurrentVerify),
		now:     time.Now,
	}, nil
}

// SetLogger wires the logger the observation lines go to. Unset falls back to
// slog.Default(), which cmd/wpmgr configures, so a gate nobody wired a logger
// into still produces the lines rather than silence.
func (g *LoginGate) SetLogger(l *slog.Logger) {
	if g != nil {
		g.logger = l
	}
}

func (g *LoginGate) log() *slog.Logger {
	if g == nil || g.logger == nil {
		return slog.Default()
	}
	return g.logger
}

// Mode reports the configured mode. LoginModeObserve for a nil gate, because a
// gate that is not there enforces nothing.
func (g *LoginGate) Mode() LoginMode {
	if g == nil {
		return LoginModeObserve
	}
	return g.mode
}

// verdict is one scope's evaluation of one attempt.
type verdict struct {
	scope      string
	key        string
	limit      int
	retryAfter time.Duration
	overBudget bool
}

// Observe evaluates every budget for one attempt and logs what a later phase
// would have refused.
//
// PHASE 0 CONTRACT, and the reviewer should hold this file to it: this function
// returns nothing, writes no header, touches no response, and its only effect
// outside this file is a log line. Its return type is what makes that
// structural rather than a promise — there is no verdict for a caller to act
// on, so no future edit to the handler can start acting on one without also
// changing this signature and being noticed.
//
// The shadow counters DO advance. They have to: a budget nothing is ever
// charged against is never exceeded, and the whole point of this phase is to
// find out whether these numbers are exceeded in practice. What "charges
// nothing" means precisely is the rule below, which is also the rule Phase 1
// will enforce under.
//
// A WOULD-BE-REFUSED ATTEMPT COSTS NOTHING, IN ANY SCOPE.
//
// Every scope is QUERIED first, at one instant, and only if all four have a
// token to spare are all four charged. This is the shape internal/mcp's
// registrationLimiter.allow arrived at after the reserve-then-release shape
// let a peer that was already over its own budget keep draining the shared one
// on every request it was refused for — being rejected was free and fast, so
// rejection became the attack. Here it means a flood that trips the pair scope
// does not also burn the src and acct scopes, so those two keep measuring what
// they are there to measure instead of being emptied by the noise.
func (g *LoginGate) Observe(ctx context.Context, a loginAttempt) {
	if g == nil {
		return
	}
	now := g.now()
	acctH := g.AccountDigest(a.Email)
	srcKey := srcKeyFor(a.Addr)
	src48Key, hasSrc48 := src48KeyFor(a.Addr)

	// ---- QUERY ONLY. Nothing is charged until every scope has passed. ----
	pending := make([]*keyedBudget, 0, 4)
	keys := make([]string, 0, 4)
	over := make([]verdict, 0, 4)

	check := func(b *keyedBudget, key string) {
		v := b.query(key, now)
		if v.overBudget {
			over = append(over, v)
			return
		}
		pending = append(pending, b)
		keys = append(keys, key)
	}

	check(g.pair, srcKey+"|"+acctH)
	check(g.src, srcKey)
	if hasSrc48 {
		check(g.src48, src48Key)
	}
	check(g.acct, acctH)

	if len(over) == 0 {
		// ---- The single mutation site. Reached only when nothing was over. ----
		for i, b := range pending {
			b.charge(keys[i], now)
		}
		return
	}

	// Over budget in at least one scope. Charge nothing, refuse nothing, and
	// say so — at INFO, because in observe mode this is a measurement and not
	// yet an incident.
	//
	// No email here, by construction: acctH is what identifies the account, and
	// the raw address is not carried past AccountDigest.
	for _, v := range over {
		g.log().InfoContext(ctx, "login admission: would refuse (observe mode; request was admitted)",
			"mode", string(g.mode),
			"scope", v.scope,
			"key", v.key,
			"limit", v.limit,
			"window", loginWindow.String(),
			"retry_after_seconds", int(v.retryAfter.Round(time.Second).Seconds()),
			"acct_h", acctH,
			"addr_source", a.addrSource(),
			"proxy_hops", a.Hops,
			"enforced", false,
		)
	}
}

// AccountDigest is the stable, keyed, fixed-width identifier for a submitted
// email address. Exported so a test can assert that what the gate keys on and
// what the service authenticates are the same account.
//
// It normalises with normalizeEmail — the SAME function Service.Login uses
// (service.go:549-551), not a copy of it. If the two ever normalised
// differently the observation would be of a different account than the one that
// authenticates, and every number this phase produces would be wrong in a way
// nothing would show. TestLoginGateNormalisationMatchesService pins it.
func (g *LoginGate) AccountDigest(email string) string {
	if g == nil {
		return ""
	}
	mac := hmac.New(sha256.New, g.procKey)
	// HMAC never returns an error and consumes the input streaming, so an
	// oversized submitted address costs one hash and retains nothing.
	_, _ = mac.Write([]byte(normalizeEmail(email)))
	return hex.EncodeToString(mac.Sum(nil))[:16]
}

// AcquireVerify takes a slot for one password verification.
//
// THIS ENFORCES, in every mode. It is not keyed on the caller and it is not a
// budget: it refuses only when this process already has maxConcurrentVerify
// verifications running, which is genuine saturation. Under that condition the
// alternative to shedding is not "serve everyone" — it is every request slowing
// down together while argon2id's memory footprint multiplies, which is how a
// login endpoint takes the rest of the process with it.
//
// The returned release is idempotent, so a caller may release early (to free
// the slot before the session work that follows a successful verify) and still
// hold a deferred release for the paths that return first.
func (g *LoginGate) AcquireVerify(ctx context.Context) (release func(), ok bool) {
	if g == nil || g.verify == nil {
		return func() {}, true
	}
	return g.verify.acquire()
}

// StartModeReminder logs a WARN every five minutes for as long as the mode is
// not enforce and ctx is live. It returns immediately when the mode already is
// enforce, so nothing runs and nothing needs stopping on a fully-enforcing
// install.
//
// This exists because a temporary state that produces no output is a permanent
// state nobody notices. The startup line says the mode once; this says it for
// as long as it is true.
func (g *LoginGate) StartModeReminder(ctx context.Context) {
	if g == nil || g.mode == LoginModeEnforce {
		return
	}
	go func() {
		t := time.NewTicker(loginModeReminderEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				g.log().Warn("login admission is NOT enforcing",
					"mode", string(g.mode),
					"env_var", LoginModeEnvVar,
					"effect", "per-source and per-account login budgets are measured and logged but not applied; only the password-verification concurrency bound is in force",
					"remedy", "set "+LoginModeEnvVar+"=enforce once the observed numbers have been reviewed",
				)
			}
		}
	}()
}

// ---------------------------------------------------------------------------
// Handler wiring.
// ---------------------------------------------------------------------------

// SetLoginGate wires login admission control. Call it after NewHandler, before
// serving. Leaving it unset leaves POST /auth/login exactly as it was before
// GH #718: no measurement and no concurrency bound.
func (h *Handler) SetLoginGate(g *LoginGate) { h.loginGate = g }

// LogAdmissionStartup states, unconditionally and at Info, what login admission
// control is actually doing in this process.
//
// WHY THIS IS UNCONDITIONAL AND WHY IT NAMES THE WIRING.
//
// Every part of this is optional at the type level and injected after
// construction, which is the house style — and the failure mode of that style
// is silence. cmd/wpmgr/main.go:2275 is a single unconditional call that hands
// the auth service its rate limiter; a refactor that moved or dropped that call
// would remove a throttle with every test still green, because a nil limiter
// reads as "no limit configured" and nothing anywhere says so. The same is true
// of a nil *LoginGate.
//
// So the numbers below are read back out of the live objects rather than from
// the constants, and gate_wired/verify_bound_wired report what the Handler
// actually holds. An operator who sees gate_wired=false has been told, in the
// first screen of the log, that the code they think is running is not.
func (h *Handler) LogAdmissionStartup(logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	g := h.loginGate
	attrs := []any{
		"issue", "GH-718",
		"phase", "0",
		"env_var", LoginModeEnvVar,
		"proxy_hops", h.effectiveProxyHops(),
		"proxy_hops_env_var", "WPMGR_AUTH_PROXY_HOPS",
		"gate_wired", g != nil,
	}
	if g == nil {
		attrs = append(attrs,
			"mode", "none",
			"verify_bound_wired", false,
			"effect", "POST /auth/login has NO admission control: no budgets are measured and the number of concurrent argon2id verifications is unbounded",
			"remedy", "call Handler.SetLoginGate at startup",
		)
		logger.Warn("login admission control", attrs...)
		return
	}
	attrs = append(attrs,
		"mode", string(g.mode),
		"budgets_enforced", g.mode == LoginModeEnforce,
		"window", loginWindow.String(),
		loginScopePair, loginPairBudget,
		loginScopeSrc, loginSrcBudget,
		loginScopeSrc48, loginSrc48Budget,
		loginScopeAcct, loginAcctBudget,
		"bucket_cap_per_scope", loginBucketCap,
		"verify_bound_wired", g.verify != nil,
		"max_concurrent_password_verifications", g.verify.capacity(),
		"over_verify_bound", "503 server_busy, Retry-After: 2",
	)
	logger.Info("login admission control", attrs...)
}

// ---------------------------------------------------------------------------
// Key derivation from the source address.
// ---------------------------------------------------------------------------

// srcKeyFor masks the decision address: IPv4 to /32 (the address itself), IPv6
// to /64. A /64 rather than a /128 because a single IPv6 client is normally
// handed a whole /64 and can move within it for free, so a /128 key is a key
// the caller chooses.
func srcKeyFor(a netip.Addr) string {
	if !a.IsValid() {
		return addrUnresolved
	}
	a = a.Unmap()
	if a.Is4() {
		return a.String()
	}
	return maskTo(a, 64)
}

// src48KeyFor masks to /48, which is the common delegation to a single
// subscriber site. IPv4 has no equivalent aggregate that is safe to assume, so
// this scope is v6-only and reports false for v4.
func src48KeyFor(a netip.Addr) (string, bool) {
	if !a.IsValid() {
		return "", false
	}
	a = a.Unmap()
	if a.Is4() {
		return "", false
	}
	return maskTo(a, 48), true
}

// maskTo returns the network address of a's /bits prefix. A prefix that cannot
// be formed falls back to the full address rather than to a shared key: failing
// towards a NARROWER key cannot merge two callers into one bucket, which is the
// direction that would let one caller spend another's budget in Phase 1.
func maskTo(a netip.Addr, bits int) string {
	p, err := a.Prefix(bits)
	if err != nil {
		return a.String()
	}
	return p.Addr().String()
}

// ---------------------------------------------------------------------------
// keyedBudget: one scope's capped map of token buckets.
// ---------------------------------------------------------------------------

type keyedBudget struct {
	scope string
	limit int

	mu      sync.Mutex
	buckets map[string]*loginBucket
}

type loginBucket struct {
	lim  *rate.Limiter
	seen time.Time
}

func newKeyedBudget(scope string, limit int) *keyedBudget {
	return &keyedBudget{
		scope:   scope,
		limit:   limit,
		buckets: make(map[string]*loginBucket),
	}
}

// query reports whether key has a token to spare at now, WITHOUT taking it.
// The bucket is created and touched here, which is why charge can assume it
// exists: creating on query keeps the two halves of one evaluation looking at
// the same bucket even if a sweep runs in between.
func (b *keyedBudget) query(key string, now time.Time) verdict {
	b.mu.Lock()
	defer b.mu.Unlock()

	bk, ok := b.buckets[key]
	if !ok {
		b.sweepLocked(now)
		bk = &loginBucket{lim: windowLimiter(b.limit, loginWindow)}
		b.buckets[key] = bk
	}
	// Touched even when over budget, deliberately: a bucket swept while it is
	// being hit would be handed back full, resetting the budget it is recording.
	bk.seen = now

	v := verdict{scope: b.scope, key: key, limit: b.limit}
	if wait := budgetShortfall(bk.lim, now); wait > 0 {
		v.overBudget = true
		v.retryAfter = wait
	}
	return v
}

// charge takes one token. It is only ever reached after every scope's query
// returned overBudget=false at the same instant, and TokensAt cannot fall
// between the two under this mutex, so the token is guaranteed to be there.
func (b *keyedBudget) charge(key string, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if bk, ok := b.buckets[key]; ok {
		bk.lim.AllowN(now, 1)
	}
}

// size reports the entry count. Test-facing; the cap it proves is a real memory
// bound and a bound nobody has watched hold is not known to hold.
func (b *keyedBudget) size() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.buckets)
}

// sweepLocked drops idle entries, and if the map is still at its cap drops the
// least recently seen until it is not.
//
// EVICTION IS FAIL-OPEN FOR THIS LAYER BY CONSTRUCTION: an evicted key comes
// back as a fresh, full bucket. That is tolerable in Phase 0 because nothing is
// enforced. It is a REAL CONSIDERATION FOR PHASE 1 and is called out here so it
// is not discovered later: with 4096 live keys per scope, a caller that can
// cycle through more than 4096 distinct keys faster than loginBucketIdle can
// evict its own earlier bucket and get a fresh budget. Phase 1's answer is a
// scope that cannot be evicted (a process-wide bound, as internal/mcp's global
// bucket is) sitting behind these, not a bigger map.
func (b *keyedBudget) sweepLocked(now time.Time) {
	for k, bk := range b.buckets {
		if now.Sub(bk.seen) > loginBucketIdle {
			delete(b.buckets, k)
		}
	}
	for len(b.buckets) >= loginBucketCap {
		oldestKey := ""
		var oldest time.Time
		for k, bk := range b.buckets {
			if oldestKey == "" || bk.seen.Before(oldest) {
				oldestKey, oldest = k, bk.seen
			}
		}
		if oldestKey == "" {
			return
		}
		delete(b.buckets, oldestKey)
	}
}

// windowLimiter turns "limit per window" into a token bucket whose burst is the
// whole budget, so the limit caps any rolling window rather than imposing an
// inter-arrival gap. Same shape as internal/mcp's perMinuteLimiter, over a
// configurable window.
//
// A non-positive limit means "allow nothing", never "allow everything": that is
// the direction a misconfiguration must fail in.
func windowLimiter(limit int, window time.Duration) *rate.Limiter {
	if limit <= 0 || window <= 0 {
		return rate.NewLimiter(0, 0)
	}
	return rate.NewLimiter(rate.Limit(float64(limit)/window.Seconds()), limit)
}

// budgetShortfall returns how long until the limiter has a token, or 0 if it
// has one now. It is a pure query: TokensAt reads the bucket without advancing
// it, which is what lets query and charge be separate steps.
func budgetShortfall(l *rate.Limiter, now time.Time) time.Duration {
	tokens := l.TokensAt(now)
	if tokens >= 1 {
		return 0
	}
	perSecond := float64(l.Limit())
	if perSecond <= 0 {
		// A zero-rate limiter never refills. Report the whole window rather
		// than 0, which would read as "a token is available".
		return loginWindow
	}
	return time.Duration((1 - tokens) / perSecond * float64(time.Second))
}

// ---------------------------------------------------------------------------
// verifySemaphore: the one part of Phase 0 that refuses.
// ---------------------------------------------------------------------------

// defaultVerifyConcurrency is the bound when none is configured.
//
// argon2id at the 19 MiB profile with Parallelism 1 is one core and 19 MiB for
// the duration of a verify, so N concurrent verifies is N cores and N*19 MiB.
// GOMAXPROCS is therefore the honest ceiling — more in flight than there are
// cores does not verify anyone faster, it just multiplies the memory and slows
// every one of them down together. The floor of 2 keeps a single-core container
// from serialising sign-ins behind one slot.
func defaultVerifyConcurrency() int {
	n := runtime.GOMAXPROCS(0)
	if n < 2 {
		return 2
	}
	return n
}

type verifySemaphore struct {
	slots chan struct{}
}

func newVerifySemaphore(max int) *verifySemaphore {
	if max <= 0 {
		max = defaultVerifyConcurrency()
	}
	return &verifySemaphore{slots: make(chan struct{}, max)}
}

// acquire takes a slot without blocking, or reports that the process is
// saturated. Non-blocking on purpose: queueing here would convert a CPU
// shortage into unbounded latency and an unbounded queue, and a caller waiting
// 30 seconds for a sign-in has already given up.
func (s *verifySemaphore) acquire() (release func(), ok bool) {
	select {
	case s.slots <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-s.slots }) }, true
	default:
		return func() {}, false
	}
}

// capacity and inFlight are test-facing, and nil-safe so the startup line can
// report an unwired bound as 0 rather than crashing the boot it is describing.
func (s *verifySemaphore) capacity() int {
	if s == nil {
		return 0
	}
	return cap(s.slots)
}

func (s *verifySemaphore) inFlight() int {
	if s == nil {
		return 0
	}
	return len(s.slots)
}
