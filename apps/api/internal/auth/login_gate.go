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
	"math"
	"net/netip"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// ---------------------------------------------------------------------------
// Login admission control (GH #718).
//
// WHAT THIS DOES.
//
// POST /auth/login runs a full argon2id verification at the 19 MiB profile
// (passwordParams, see password.go) for every attempt it admits. This file
// decides, before any account is looked up, whether an attempt is admitted.
// It has two independent parts:
//
//  1. BUDGETS keyed on the source address and on the (source, account) pair.
//     The mode is WPMGR_AUTH_LOGIN_MODE and it defaults to "observe":
//
//     - observe evaluates every budget, logs what enforce would have refused,
//       and refuses nothing. The caller sees no difference at all.
//     - enforce refuses an attempt that is over the pair, source or source /48
//       budget with 429 too_many_attempts and a Retry-After in seconds. It
//       never refuses on a degraded source address; see addrUnresolved.
//
//  2. A CONCURRENCY BOUND on the password verification itself, in force in
//     every mode. It is not a rate limit and it is not keyed on anything the
//     caller controls: it sheds only when more verifications are already in
//     flight than this process is willing to run at once.
//
// THE BUDGET BELONGS TO THE ATTEMPTER, NOT TO THE ACCOUNT.
//
// A budget keyed on the submitted email alone lets anyone who knows an address
// spend that address's budget and lock its owner out of their own account; the
// limiter becomes the denial of service. So the tight budget is the PAIR: this
// source against this account. A stranger naming an account spends only the
// budget for their own source against it, and the owner, on theirs, is
// untouched.
//
// The account scope (login:acct) is evaluated and logged as a signal and
// NEVER refuses, in any mode. Refusing on it would be the lockout again.
//
// WHAT A BUDGET COUNTS.
//
//   - A REFUSED ATTEMPT CHARGES NOTHING, in any scope. See Admit.
//   - A SUCCESSFUL SIGN-IN CHARGES NOTHING AND LEAVES NOTHING. An admitted
//     attempt is charged up front, so concurrent attempts cannot all see the
//     same last token, and the charge is given back when the password
//     verifies. A bucket entry the charge created is removed with it, and
//     nothing is evicted to make room for one, so a successful sign-in leaves
//     every map exactly as it found it. What remains charged, and what remains
//     in the maps, is failed attempts. See loginAdmission and loginBucketCap.
//   - An attempt shed by the concurrency bound charges nothing and creates no
//     bucket entry: the verification slot is taken before anything is
//     charged, so a shed attempt never reaches the charge. It leaves every
//     map exactly as it found it.
//
// BUDGETS ARE PER PROCESS. Each instance keeps its own buckets, so a
// deployment running N instances admits up to N times each budget across the
// fleet. That is a property of this design, not an accident of it.
//
// WHAT THE GATE MUST NOT COST.
//
// This runs on an UNAUTHENTICATED endpoint, before any account lookup:
//
//   - It never branches on whether an account exists. It runs before
//     Service.Login and therefore before GetUserByEmail; it is handed an email
//     string and nothing else, and it cannot tell a real address from a
//     fabricated one. A refusal is identical for an account that exists and
//     for one that does not, because nothing here knows the difference.
//   - In observe mode it changes no status, no body and no header.
//   - It never logs an email address. The account is identified by acctH, a
//     keyed digest, exactly as reset.go and twofa.go identify an account in a
//     log line without naming it.
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
// two purposes never share one derived key. Keying it matters: an unkeyed
// hash of an email address is reversible by anyone with a word list, which
// would put the address back in the log in all but name.
// ---------------------------------------------------------------------------

// LoginMode selects what the gate does with its own verdict.
type LoginMode string

const (
	// LoginModeObserve evaluates every budget and refuses nothing. The default:
	// an install that has never set WPMGR_AUTH_LOGIN_MODE runs this.
	LoginModeObserve LoginMode = "observe"

	// LoginModeEnforce refuses an attempt over the pair, source or source /48
	// budget. It has to be asked for by name in WPMGR_AUTH_LOGIN_MODE.
	LoginModeEnforce LoginMode = "enforce"
)

// LoginModeEnvVar is the variable that selects the mode. Named as a constant so
// the startup line, the periodic reminder and the config layer cannot drift
// apart on its spelling.
const LoginModeEnvVar = "WPMGR_AUTH_LOGIN_MODE"

// ParseLoginMode converts the configured string. An empty value is the default
// rather than an error, so an install that has never heard of this variable
// boots in observe. Anything else is refused rather than silently coerced: a
// typo'd mode must not read as "the safe one" to one reader and as "off" to
// another.
//
// Every mode accepted here must do what LogAdmissionStartup says it does.
// TestStartupLineCannotClaimEnforcementItDoesNotDo drives real requests through
// the real handler in every accepted mode and fails if the startup line and the
// observed behaviour ever disagree.
func ParseLoginMode(s string) (LoginMode, error) {
	switch LoginMode(s) {
	case "", LoginModeObserve:
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

	// loginAcctBudget is per account across all sources. It is a SIGNAL and
	// never refuses, in any mode: a budget anyone can spend by naming an
	// account is a way to lock that account's owner out. It exists to make a
	// distributed run against one account visible in the log.
	loginAcctBudget = 50

	// loginBucketCap bounds each scope's map so a caller varying its key cannot
	// grow it without limit. Mirrors registerPeerCap
	// (internal/mcp/register_limit.go:96-100). Reaching it is a memory bound
	// being enforced, not an error.
	//
	// It bounds the PERMANENT entries, and entries become permanent only by
	// failed attempts: an entry stays in the map only once a failed attempt's
	// charge on it has been kept. A refused attempt and a shed attempt create
	// nothing. A successful sign-in creates only a provisional entry, removed
	// again when its charge is given back, and it never sweeps or evicts
	// anything. So a source adds entries, and evicts others, only as fast as
	// its own failure budget admits failed attempts.
	//
	// Provisional entries sit outside the cap and are bounded separately: one
	// exists only while an admission that charged it is unfinished, and every
	// unfinished admission holds a verification slot. So a map holds at most
	// loginBucketCap entries plus the verification bound.
	loginBucketCap = 4096

	// loginBucketIdle is how long an untouched bucket survives a sweep. It must
	// be at least loginWindow: a bucket swept while its window is still running
	// would be handed back full, which resets the very budget it is recording.
	// TestIdleSweepNeverResetsADrainedPairWithinTheWindow pins it.
	loginBucketIdle = 30 * time.Minute

	// verifyShedRetryAfter is the Retry-After on a shed attempt. Short, because
	// the condition it describes clears in the time one verification takes.
	verifyShedRetryAfter = 2 * time.Second

	// loginOverLogEvery re-states a scope that is STILL over budget once every
	// this many further attempts, on top of the line its first crossing wrote.
	// It samples the observe-mode would-refuse line and the account-signal
	// line. An enforce-mode REFUSAL is not sampled: every refusal writes its
	// own WARN, because those lines are what an operator counts refusals by.
	// Without it a scope that goes over stays over for the rest of the window
	// and writes a line per request forever: measured at ~346 bytes and one
	// synchronous write per attempt, on the unauthenticated path, where the
	// pre-change code wrote nothing. A first crossing plus a periodic repeat
	// carries the same distribution at a bounded cost, and the repeat count is
	// on the line so the volume is not lost either.
	loginOverLogEvery = 100

	// loginModeReminderEvery is how often a non-enforce mode announces itself.
	// A kill switch flipped during an incident is forgotten when nothing keeps
	// saying it is flipped.
	loginModeReminderEvery = 5 * time.Minute
)

// Scope names. These are the strings a refusal is attributed to and the strings
// the log lines carry, so they are declared once.
const (
	loginScopePair  = "login:pair"
	loginScopeSrc   = "login:src"
	loginScopeSrc48 = "login:src48"
	loginScopeAcct  = "login:acct"
)

// addrUnresolved is the reserved key for an attempt whose source address could
// not be resolved at all. Such attempts share one bucket rather than being
// skipped, so they are still measured.
//
// A BUDGET NEVER REFUSES ON A DEGRADED SOURCE ADDRESS, in any mode. There are
// two kinds, told apart by loginAttempt.addrSource:
//
//   - unresolved: the peer address itself does not parse.
//   - peer_fallback: WPMGR_AUTH_PROXY_HOPS says proxies append to
//     X-Forwarded-For, but the chain was shorter than that, so the address
//     is the TCP peer rather than the entry the proxies appended.
//
// Both are properties of the deployment, not choices a caller makes, and in
// both every client arriving that way shares one key. Refusing on that key
// would refuse all of them together, and its pair scope would be keyed on the
// account alone, which is the lockout this design exists to avoid. A short
// chain constrains nobody who wants to avoid it, either: a longer one is keyed
// on its own entries instead. So an attempt on a degraded address is measured
// and charged like any other and admitted even when a refusing scope is over
// budget. The concurrency bound still applies, and the would-refuse is logged
// at WARN, sampled like every other over-budget line, with addr_source on it so
// an operator can see the misconfiguration. loginAttempt.refusable decides.
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
// Either way every client behind that path collapses onto one key, which is why
// no budget refuses on it (see addrUnresolved); the would-refuse line carries
// this value so that collapse is visible.
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

// refusable reports whether a budget may refuse an attempt keyed on this
// address. Only an address read where the configured topology says the client
// is qualifies: the chain entry at the hop position, or the peer when nothing
// appends. Anything else is degraded (see addrUnresolved), and a source added
// here later is degraded until someone decides otherwise.
func (a loginAttempt) refusable() bool {
	switch a.addrSource() {
	case "chain", "peer_configured":
		return true
	default:
		return false
	}
}

// LoginGate is the admission control described at the top of this file.
//
// A nil *LoginGate is safe to call and does nothing at all: no budgets and no
// concurrency bound. That is a wiring failure, not a mode, and it is exactly
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

	// mu makes one evaluation (every query, then every charge) a single step,
	// and orders a give-back against it. Held only for map lookups and bucket
	// arithmetic, never across the password verification or a log write.
	mu sync.Mutex

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

// SetLogger wires the logger the admission lines go to. Unset falls back to
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
	// worthLogging is set on the first attempt that finds this key over budget
	// and every loginOverLogEvery-th one after it. See loginOverLogEvery.
	worthLogging bool
	// overCount is how many attempts have found this key over budget since it
	// last had budget. Carried onto the line so a suppressed run is still
	// countable.
	overCount int
}

// chargedKey is one token an admitted attempt took, so it can be settled. bk
// is the bucket the token came from: settling applies only while the map still
// holds that same bucket under key.
type chargedKey struct {
	b   *keyedBudget
	key string
	bk  *loginBucket
}

// loginAdmission is what an admitted attempt holds: its verification slot and
// the tokens it was charged.
//
// An attempt is charged at admission rather than after its outcome is known,
// because charging afterwards would let every attempt in flight at once see
// the same last token. Its charge is SETTLED when the outcome is known, in one
// of two ways, and the order matters:
//
//   - giveBack, when the password verified. Every token is returned, and a
//     bucket entry that exists only because of this attempt is removed, so a
//     successful sign-in leaves every map exactly as it found it.
//   - finish, always, last. Whatever giveBack did not return stays charged:
//     the attempt failed, and failed attempts are what the budgets are
//     denominated in. finish also frees the verification slot.
//
// So a success calls giveBack and then finish, and a failure calls finish
// alone. A giveBack after finish returns nothing: the charge was already kept.
// (An attempt the concurrency bound sheds is never charged at all; see Admit.)
//
// A nil *loginAdmission is valid, holds no slot and gives nothing back; that
// is what a nil gate returns.
type loginAdmission struct {
	g       *LoginGate
	charged []chargedKey
	// release frees the verification slot. Idempotent (verifySemaphore.acquire
	// wraps it in a sync.Once), so the handler can finish early and still hold
	// a deferred finish for the paths that return first.
	release func()
}

// finish settles every charge this admission still holds as KEPT, then frees
// its verification slot. Idempotent and nil-safe, so the handler can call it
// explicitly and again by defer.
//
// The charge is settled before the slot is freed. That is what bounds the
// provisional entries (see loginBucketCap) by the verification bound: an entry
// is provisional only while an admission that charged it is unfinished, and an
// unfinished admission holds a slot.
func (ad *loginAdmission) finish() {
	if ad == nil {
		return
	}
	ad.settle(false)
	if ad.release != nil {
		ad.release()
	}
}

// giveBack returns every token this admission took, and removes each bucket
// entry that exists only because of it. Call it when the password verified,
// and before finish. Idempotent: a second call finds nothing left to return.
// TestGiveBackIsIdempotent pins that against a bucket another attempt has
// charged in between, where a second return would erase that attempt's charge.
func (ad *loginAdmission) giveBack() {
	if ad == nil {
		return
	}
	ad.settle(true)
}

// settle finishes every charge still held, as given back (success) or as kept.
func (ad *loginAdmission) settle(success bool) {
	if ad.g == nil {
		return
	}
	ad.g.mu.Lock()
	defer ad.g.mu.Unlock()
	if len(ad.charged) == 0 {
		return
	}
	now := ad.g.now()
	for _, c := range ad.charged {
		c.b.settle(c.key, c.bk, now, success)
	}
	ad.charged = nil
}

// loginRefusal is an attempt Admit did not admit: either an enforce-mode budget
// refusal (429), or, in any mode, the verification bound shedding it (503). It
// carries only what the caller is told: which kind of limit refused, and how
// long until it has room again. Nothing in it depends on the account, beyond
// the account string the caller itself submitted.
type loginRefusal struct {
	scope      string
	retryAfter time.Duration
	// shed is set when the verification bound refused the attempt rather than
	// a budget. scope is empty then.
	shed bool
}

// retryAfterSeconds is the Retry-After value: whole seconds, rounded UP, and
// never below one. A sub-second shortfall truncated to "0" would tell a client
// to retry at once, which is the tight loop a refusal exists to break.
func (r *loginRefusal) retryAfterSeconds() int {
	secs := int(math.Ceil(r.retryAfter.Seconds()))
	if secs < 1 {
		secs = 1
	}
	return secs
}

// wireScope is the scope as the API names it. The distinction the caller is
// given is the one that matters to a person reading the message: whether it
// was their own attempts at this account, or their whole connection.
func (r *loginRefusal) wireScope() string {
	switch r.scope {
	case loginScopePair:
		return "pair"
	case loginScopeSrc48:
		return "network"
	default:
		return "source"
	}
}

// domainError is the response body. For a budget refusal it is the 429, and
// its message names the kind of limit so a person behind a shared connection
// is told it is the connection, not them. A shed attempt is a 503 that says
// nothing about the caller, because nothing about the caller decided it.
func (r *loginRefusal) domainError() *domain.Error {
	if r.shed {
		return domain.ServiceUnavailable("server_busy", "server is busy verifying sign-ins; retry shortly")
	}
	var msg string
	switch r.scope {
	case loginScopePair:
		msg = "too many sign-in attempts for this account from your connection; wait before trying again"
	case loginScopeSrc48:
		msg = "too many sign-in attempts from your network; wait before trying again"
	default:
		msg = "too many sign-in attempts from your connection; wait before trying again"
	}
	return domain.RateLimited("too_many_attempts", msg).WithDetails(map[string]any{
		"scope":               r.wireScope(),
		"retry_after_seconds": r.retryAfterSeconds(),
	})
}

// Admit evaluates every budget for one attempt, takes its verification slot,
// and decides whether it goes ahead.
//
// It returns an admission when the attempt may proceed and a refusal when it
// may not; exactly one of the two is non-nil, except for a nil gate, which
// returns neither. Once the password has been checked the caller must give the
// charge back if it verified, and then finish the admission in every case; see
// loginAdmission.
//
// THE DECISION, in order, all under one lock:
//
//  1. Every scope is QUERIED. In enforce mode, an attempt over the pair,
//     source or source /48 budget, keyed on an address a refusal may be keyed
//     on (loginAttempt.refusable), is refused with 429. The account scope is a
//     signal and never refuses.
//  2. A verification slot is taken. If none is free the attempt is shed with
//     503, in every mode.
//  3. Only now, when admission is final, is anything charged. An attempt that
//     observe mode (or a degraded address) let through over budget is not
//     charged: there is no token to take. A key with no bucket gets a
//     provisional one, which evicts nothing and stays only if the attempt
//     fails (see loginBucketCap).
//
// A REFUSED ATTEMPT COSTS NOTHING, IN ANY SCOPE.
//
// Every scope is queried at one instant, and only if no refusing scope is over
// budget are they all charged. This is the shape internal/mcp's
// registrationLimiter.allow arrived at after the reserve-then-release shape
// let a peer that was already over its own budget keep draining the shared one
// on every request it was refused for: being rejected was free and fast, so
// rejection became the attack. Here it means an attacker flooding one account
// from a shared connection is refused on the pair and does not also empty the
// source budget everyone else on that connection signs in against.
//
// A SHED ATTEMPT COSTS NOTHING EITHER, AND CREATES NOTHING.
//
// The slot is taken before the charge, so an attempt the bound sheds never
// charged a token and never created a bucket entry. Charging first and giving
// the tokens back would still leave the entries behind: at zero net budget a
// caller could then add entries until each map's cap evicts the oldest,
// including its own exhausted buckets, which come back full. A budget refusal
// never takes a slot, so being refused spends none of the shared capacity the
// bound protects.
//
// The query, the slot and the charge happen under one lock, so the boundary is
// exact: two concurrent attempts cannot both be admitted on the last token.
// Taking the slot there is safe because it never blocks.
//
// Observe mode runs the same evaluation and the same accounting, so what it
// logs as a would-refuse is what enforce would refuse, and then admits.
func (g *LoginGate) Admit(ctx context.Context, a loginAttempt) (*loginAdmission, *loginRefusal) {
	if g == nil {
		return nil, nil
	}
	// Computed outside the lock: the digest is the only part of this with a
	// cost that grows with the input.
	acctH := g.AccountDigest(a.Email)
	srcKey := srcKeyFor(a.Addr)
	src48Key, hasSrc48 := src48KeyFor(a.Addr)
	pairKey := srcKey + "|" + acctH

	g.mu.Lock()
	now := g.now()

	// ---- 1. QUERY ONLY. Nothing is charged or created here. ----
	var over []verdict
	check := func(b *keyedBudget, key string) {
		if v := b.query(key, now); v.overBudget {
			over = append(over, v)
		}
	}
	check(g.pair, pairKey)
	check(g.src, srcKey)
	if hasSrc48 {
		check(g.src48, src48Key)
	}
	acctV := g.acct.query(acctH, now)

	refuse := len(over) > 0 && g.mode == LoginModeEnforce && a.refusable()

	var ad *loginAdmission
	shed := false
	if !refuse {
		// ---- 2. The verification slot, before anything is charged. ----
		if release, ok := g.verify.acquire(); ok {
			ad = &loginAdmission{g: g, release: release}
		} else {
			shed = true
		}
	}
	if ad != nil && len(over) == 0 {
		// ---- 3. The single mutation site. Reached only when admission is
		// final and nothing refusing was over, in either mode. ----
		charge := func(b *keyedBudget, key string) {
			if bk, ok := b.charge(key, now); ok {
				ad.charged = append(ad.charged, chargedKey{b: b, key: key, bk: bk})
			}
		}
		charge(g.pair, pairKey)
		charge(g.src, srcKey)
		if hasSrc48 {
			charge(g.src48, src48Key)
		}
		// Not charged while over: there is no token to take, and charging
		// resets the logging latch, which would turn the sampled signal line
		// below into a line per attempt.
		if !acctV.overBudget {
			charge(g.acct, acctH)
		}
	}
	g.mu.Unlock()

	// ---- Logging, outside the lock. No email anywhere below, by
	// construction: acctH identifies the account, and the raw address is not
	// carried past AccountDigest. ----
	if acctV.overBudget && acctV.worthLogging {
		g.log().InfoContext(ctx, "login admission: account over budget (signal only; this scope never refuses)",
			"mode", string(g.mode),
			"scope", acctV.scope,
			"limit", acctV.limit,
			"window", loginWindow.String(),
			"acct_h", acctH,
			"over_budget_attempts", acctV.overCount,
			"log_sampling", fmt.Sprintf("first crossing, then 1 in %d", loginOverLogEvery),
			"addr_source", a.addrSource(),
			"proxy_hops", a.Hops,
		)
	}

	if refuse {
		return nil, g.refuse(ctx, a, over, srcKey, src48Key, hasSrc48, acctH)
	}

	if len(over) > 0 {
		g.logNotRefused(ctx, a, over, acctH, shed)
	}

	if shed {
		return nil, &loginRefusal{shed: true, retryAfter: verifyShedRetryAfter}
	}
	return ad, nil
}

// logNotRefused writes the line for an attempt that was over a refusing budget
// and was not refused for it: observe mode, or a degraded source address (see
// addrUnresolved). Nothing was charged for it.
//
// Observe logs at INFO because there it is a measurement. A degraded address
// under enforce logs at WARN because there it is a limit not being applied,
// and the likely cause is a hop count that does not match what sits in front
// of this process. Both are sampled per key (see loginOverLogEvery): neither
// is a refusal, and a degraded key is one every client on that path shares, so
// an unsampled line would be one per sign-in attempt.
func (g *LoginGate) logNotRefused(ctx context.Context, a loginAttempt, over []verdict, acctH string, shed bool) {
	msg, level := "login admission: would refuse (observe mode; not refused)", slog.LevelInfo
	if g.mode == LoginModeEnforce {
		msg, level = "login admission: would refuse, but the source address is degraded and no budget refuses on it (not refused)", slog.LevelWarn
	}
	outcome := "admitted"
	if shed {
		outcome = "shed by the verification bound"
	}
	for _, v := range over {
		if !v.worthLogging {
			continue
		}
		attrs := []any{
			"mode", string(g.mode),
			"scope", v.scope,
			"key", v.key,
			"limit", v.limit,
			"window", loginWindow.String(),
			"retry_after_seconds", int(v.retryAfter.Round(time.Second).Seconds()),
			"acct_h", acctH,
			"over_budget_attempts", v.overCount,
			"log_sampling", fmt.Sprintf("first crossing, then 1 in %d", loginOverLogEvery),
			"addr_source", a.addrSource(),
			"proxy_hops", a.Hops,
			"enforced", false,
			"outcome", outcome,
		}
		if a.addrSource() == "peer_fallback" {
			attrs = append(attrs, "remedy", "the X-Forwarded-For chain was shorter than WPMGR_AUTH_PROXY_HOPS; set it to the number of proxies that append to the chain, and serve this process only through them")
		}
		g.log().Log(ctx, level, msg, attrs...)
	}
}

// refuse builds an enforce-mode budget refusal and writes its WARN.
//
// The caller is told about the scope that will stay closed longest, since that
// is what Retry-After has to cover; on a tie, the widest, so a person behind a
// shared connection is told it is the connection.
// TestRetryAfterCoversTheLongestWait pins the choice.
func (g *LoginGate) refuse(ctx context.Context, a loginAttempt, over []verdict, srcKey, src48Key string, hasSrc48 bool, acctH string) *loginRefusal {
	lead := over[0]
	scopes := make([]string, 0, len(over))
	for _, v := range over {
		scopes = append(scopes, v.scope)
		if v.retryAfter > lead.retryAfter || (v.retryAfter == lead.retryAfter && scopeWidth(v.scope) > scopeWidth(lead.scope)) {
			lead = v
		}
	}
	r := &loginRefusal{scope: lead.scope, retryAfter: lead.retryAfter}

	// Every refusal, unsampled. A refused attempt writes no audit row (it
	// never reaches Service.Login), so this line is the record of it.
	attrs := []any{
		"mode", string(g.mode),
		"scope", lead.scope,
		"over_scopes", strings.Join(scopes, ","),
		"source", srcKey,
	}
	if hasSrc48 {
		attrs = append(attrs, "source_48", src48Key)
	}
	attrs = append(attrs,
		"limit", lead.limit,
		"window", loginWindow.String(),
		"retry_after_seconds", r.retryAfterSeconds(),
		"acct_h", acctH,
		"over_budget_attempts", lead.overCount,
		"addr_source", a.addrSource(),
		"proxy_hops", a.Hops,
		"enforced", true,
	)
	g.log().WarnContext(ctx, "login admission: refused", attrs...)
	return r
}

// scopeWidth orders the refusing scopes from narrowest to widest.
func scopeWidth(scope string) int {
	switch scope {
	case loginScopePair:
		return 0
	case loginScopeSrc:
		return 1
	case loginScopeSrc48:
		return 2
	default:
		return -1
	}
}

// AccountDigest is the stable, keyed, fixed-width identifier for a submitted
// email address. Exported so a test can assert that what the gate keys on and
// what the service authenticates are the same account.
//
// It normalises with normalizeEmail — the SAME function Service.Login uses
// (service.go:549-551), not a copy of it. If the two ever normalised
// differently the budget would be keyed on a different account than the one
// that authenticates, and the pair scope would stop binding in a way nothing
// would show. TestGateNormalisationMatchesService pins it.
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
					"effect", "login budgets are measured and logged but not applied; only the password-verification concurrency bound is in force",
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
// GH #718: no budgets and no concurrency bound.
func (h *Handler) SetLoginGate(g *LoginGate) { h.loginGate = g }

// LogAdmissionStartup states, unconditionally and at Info, what login admission
// control is actually doing in this process.
//
// WHY THIS IS UNCONDITIONAL AND WHY IT NAMES THE WIRING.
//
// Every part of this is optional at the type level and injected after
// construction, which is the house style — and the failure mode of that style
// is silence. In cmd/wpmgr/main.go the auth service receives its rate limiter
// through a single unconditional authSvc.SetMailer call; a refactor that moved
// or dropped that call would remove a throttle with every test still green,
// because a nil limiter reads as "no limit configured" and nothing anywhere
// says so. The same is true of a nil *LoginGate.
//
// So the budgets and the verification bound below are read back out of the
// live objects the gate holds, and gate_wired/verify_bound_wired report what
// the Handler actually holds. An operator who sees gate_wired=false has been
// told, in the first screen of the log, that the code they think is running is
// not.
//
// refusing_scopes is printed only in enforce mode, where it is true. Observe
// prints the same scopes as would_refuse_scopes instead, because nothing
// refuses there.
func (h *Handler) LogAdmissionStartup(logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	g := h.loginGate
	attrs := []any{
		"issue", "GH-718",
		"phase", "1",
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
		// Not a promise: TestStartupLineCannotClaimEnforcementItDoesNotDo
		// drives real requests through the real handler for every mode
		// ParseLoginMode accepts and fails if this field and the observed
		// behaviour ever disagree, in either direction.
		"budgets_enforced", g.mode == LoginModeEnforce,
	)
	budgetScopes := strings.Join([]string{g.pair.scope, g.src.scope, g.src48.scope}, ",")
	if g.mode == LoginModeEnforce {
		attrs = append(attrs,
			"refusing_scopes", budgetScopes,
			"not_refused_on", "unresolved and peer_fallback source addresses (logged at WARN instead)",
		)
	} else {
		attrs = append(attrs, "would_refuse_scopes", budgetScopes)
	}
	attrs = append(attrs,
		"signal_only_scopes", g.acct.scope,
		"over_budget", "429 too_many_attempts with Retry-After (enforce mode only)",
		"window", loginWindow.String(),
		g.pair.scope, g.pair.limit,
		g.src.scope, g.src.limit,
		g.src48.scope, g.src48.limit,
		g.acct.scope, g.acct.limit,
		// Each instance keeps its own buckets, so N instances admit up to N
		// times each budget. Said here so nobody reads these numbers as
		// fleet-wide.
		"budgets_are", "per process",
		"bucket_cap_per_scope", loginBucketCap,
		"verify_bound_wired", g.verify != nil,
		"max_concurrent_password_verifications", g.verify.capacity(),
		"over_verify_bound", fmt.Sprintf("503 server_busy, Retry-After: %d", int(verifyShedRetryAfter.Seconds())),
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
// direction that would let one caller spend another's budget.
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

// keyedBudget is one scope. Its own mutex guards the map for the test-facing
// readers; Admit and loginAdmission.settle additionally hold the gate's mutex
// across a whole evaluation, which is what makes a query and the charge that
// follows it one step.
//
// An entry is PROVISIONAL while no charge on it has been kept (kept == 0):
// every charge it carries belongs to an admission that has not finished. A
// provisional entry does not count against loginBucketCap, is never evicted
// and never swept, and is removed by the give-back that settles its last
// pending charge. It becomes permanent when a failed attempt's charge on it is
// kept, and only then can it cause an eviction. Every entry in the map has
// pending > 0 or kept > 0.
type keyedBudget struct {
	scope string
	limit int

	mu      sync.Mutex
	buckets map[string]*loginBucket
}

type loginBucket struct {
	lim  *windowBucket
	seen time.Time
	// overCount counts consecutive over-budget findings, and resets the moment
	// the key is charged again (which only happens when it has budget). It
	// drives the logging latch, never a decision.
	overCount int
	// pending counts charges on this bucket held by admissions that have not
	// finished. kept counts charges that stayed because their attempt failed.
	pending int
	kept    int
}

func newKeyedBudget(scope string, limit int) *keyedBudget {
	return &keyedBudget{
		scope:   scope,
		limit:   limit,
		buckets: make(map[string]*loginBucket),
	}
}

// query reports whether key has a token to spare at now, WITHOUT taking it and
// WITHOUT creating anything. A key with no bucket has its whole budget, so
// there is nothing to record for it. That is what makes a refused attempt free
// in memory as well as in budget: a refused attempt can never add an entry,
// so a caller cannot use refused attempts to push other keys out of the map.
func (b *keyedBudget) query(key string, now time.Time) verdict {
	b.mu.Lock()
	defer b.mu.Unlock()

	v := verdict{scope: b.scope, key: key, limit: b.limit}
	bk, ok := b.buckets[key]
	if !ok {
		// Full budget, unless the budget is misconfigured to nothing; see
		// newWindowBucket for why that direction refuses.
		if b.limit <= 0 {
			v.overBudget = true
			v.retryAfter = loginWindow
		}
		return v
	}
	// Touched even when over budget, deliberately: a bucket swept while it is
	// being hit would be handed back full, resetting the budget it is recording.
	bk.seen = now

	if wait := bk.lim.shortfall(now); wait > 0 {
		v.overBudget = true
		v.retryAfter = wait
		bk.overCount++
		v.overCount = bk.overCount
		// First crossing, then one line per loginOverLogEvery attempts.
		v.worthLogging = bk.overCount == 1 || bk.overCount%loginOverLogEvery == 0
	}
	return v
}

// charge takes one token for key, creating a provisional bucket if it has none,
// and resets the logging latch because a key with a token to spare is by
// definition no longer over budget. It returns the bucket charged, which the
// charge must later be settled against, and whether a token was taken.
//
// It never sweeps and never evicts. The attempt's outcome is not known yet,
// and a successful one must leave every other entry exactly as it was; making
// room is paid for by a failed attempt, when its charge is kept (see settle).
//
// Called only by Admit, under the gate mutex and only after query found a
// token, so false is not expected; it is reported rather than assumed so a
// token that was not taken is never settled.
func (b *keyedBudget) charge(key string, now time.Time) (*loginBucket, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	bk, existed := b.buckets[key]
	if !existed {
		bk = &loginBucket{lim: newWindowBucket(b.limit, loginWindow, now)}
		b.buckets[key] = bk
	}
	bk.seen = now
	if !bk.lim.take(now) {
		if !existed {
			delete(b.buckets, key)
		}
		return nil, false
	}
	bk.pending++
	bk.overCount = 0
	return bk, true
}

// settle finishes one charge taken on bk under key.
//
// success returns the token, and removes the entry if nothing but finished
// successes ever charged it (pending and kept both zero). Such an entry is at
// its whole budget, since every token taken from it was given back, so
// removing it changes no budget: it leaves the map as it was before the first
// of those attempts was admitted.
//
// Otherwise the charge is kept. The first kept charge makes the entry
// permanent, which is the only point at which an entry starts to count
// against loginBucketCap, and so the only point at which the map sweeps.
//
// A bucket that has left the map since the charge (evicted, or swept) is not
// touched, even if another attempt has since created a new bucket under the
// same key: those are that attempt's tokens, not this one's, and a key with no
// bucket already reads as its whole budget.
func (b *keyedBudget) settle(key string, bk *loginBucket, now time.Time, success bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if bk == nil || b.buckets[key] != bk {
		return
	}
	bk.pending--
	if success {
		bk.lim.give(now)
		if bk.pending == 0 && bk.kept == 0 {
			delete(b.buckets, key)
		}
		return
	}
	bk.kept++
	if bk.kept == 1 {
		b.sweepLocked(now, bk)
	}
}

// tokensAt reports key's remaining tokens at now: the whole budget for a key
// with no bucket. Test-facing.
func (b *keyedBudget) tokensAt(key string, now time.Time) float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	if bk, ok := b.buckets[key]; ok {
		return bk.lim.TokensAt(now)
	}
	return float64(b.limit)
}

// size reports the entry count. Test-facing; the cap it proves is a real memory
// bound and a bound nobody has watched hold is not known to hold.
func (b *keyedBudget) size() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.buckets)
}

// sweepLocked runs when a failed attempt's charge makes entry permanent. It
// drops idle entries, and if more than loginBucketCap entries are then
// permanent, drops the least recently seen permanent entry other than entry
// itself until no more are.
//
// It never touches an entry with a pending charge in the idle pass, and never
// evicts a provisional one: those belong to attempts still in flight, and a
// provisional entry leaves by its own give-back or becomes permanent by its
// own failure.
//
// An idle entry is at its whole budget, so dropping one changes no budget: a
// token is taken only by charge, which also stamps seen, so an entry unseen
// for loginBucketIdle has refilled for at least loginWindow since its last
// take, and that refills any bucket.
//
// EVICTION IS FAIL-OPEN BY CONSTRUCTION: an evicted key comes back as a fresh,
// full bucket. What bounds that is that an entry is permanent only once a
// failed attempt's charge on it has been kept: never because of a refused
// attempt, a shed one or a successful one. So evicting anything takes failed
// attempts, and a source can make them only as fast as its own failure budget
// admits them.
func (b *keyedBudget) sweepLocked(now time.Time, entry *loginBucket) {
	for k, bk := range b.buckets {
		if bk.pending == 0 && now.Sub(bk.seen) > loginBucketIdle {
			delete(b.buckets, k)
		}
	}
	for {
		permanent := 0
		oldestKey := ""
		var oldest time.Time
		for k, bk := range b.buckets {
			if bk.kept == 0 {
				continue
			}
			permanent++
			if bk == entry {
				continue
			}
			if oldestKey == "" || bk.seen.Before(oldest) {
				oldestKey, oldest = k, bk.seen
			}
		}
		if permanent <= loginBucketCap || oldestKey == "" {
			return
		}
		delete(b.buckets, oldestKey)
	}
}

// ---------------------------------------------------------------------------
// windowBucket: a token bucket that can take a token back.
// ---------------------------------------------------------------------------

// windowBucket is a token bucket whose burst is the whole budget, so the limit
// caps any rolling window rather than imposing an inter-arrival gap.
//
// It is hand-rolled rather than a golang.org/x/time/rate.Limiter for one
// reason: a successful sign-in gives its token back, and that limiter has no
// way to return a token once its reservation's time has passed.
//
// Not safe for concurrent use on its own; keyedBudget's mutex guards it.
type windowBucket struct {
	perSec float64
	burst  float64
	tokens float64
	last   time.Time
}

// newWindowBucket starts full. A non-positive limit means "allow nothing",
// never "allow everything": that is the direction a misconfiguration must
// fail in.
func newWindowBucket(limit int, window time.Duration, now time.Time) *windowBucket {
	if limit <= 0 || window <= 0 {
		return &windowBucket{last: now}
	}
	return &windowBucket{
		perSec: float64(limit) / window.Seconds(),
		burst:  float64(limit),
		tokens: float64(limit),
		last:   now,
	}
}

// TokensAt reads the bucket at now without changing it. A now earlier than
// the last update (two attempts reading the clock in the other order) refills
// nothing rather than going negative.
func (w *windowBucket) TokensAt(now time.Time) float64 {
	t := w.tokens
	if now.After(w.last) {
		t += now.Sub(w.last).Seconds() * w.perSec
	}
	if t > w.burst {
		t = w.burst
	}
	return t
}

func (w *windowBucket) advance(now time.Time) {
	w.tokens = w.TokensAt(now)
	if now.After(w.last) {
		w.last = now
	}
}

func (w *windowBucket) take(now time.Time) bool {
	w.advance(now)
	if w.tokens < 1 {
		return false
	}
	w.tokens--
	return true
}

// give returns one token, never past the budget.
func (w *windowBucket) give(now time.Time) {
	w.advance(now)
	w.tokens++
	if w.tokens > w.burst {
		w.tokens = w.burst
	}
}

// shortfall returns how long until the bucket has a token, or 0 if it has one
// now. A pure query.
func (w *windowBucket) shortfall(now time.Time) time.Duration {
	tokens := w.TokensAt(now)
	if tokens >= 1 {
		return 0
	}
	if w.perSec <= 0 {
		// A zero-rate bucket never refills. Report the whole window rather
		// than 0, which would read as "a token is available".
		return loginWindow
	}
	return time.Duration((1 - tokens) / w.perSec * float64(time.Second))
}

// ---------------------------------------------------------------------------
// verifySemaphore: the part that refuses in every mode.
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

// acquire takes a slot for one password verification without blocking, or
// reports that the process is saturated.
//
// THIS ENFORCES, in every mode. It is not keyed on the caller and it is not a
// budget: it refuses only when this process already has as many verifications
// running as it is willing to run at once, which is genuine saturation. Under
// that condition the alternative to shedding is not "serve everyone" — it is
// every request slowing down together while argon2id's memory footprint
// multiplies, which is how a login endpoint takes the rest of the process with
// it.
//
// Non-blocking on purpose: queueing here would convert a CPU shortage into
// unbounded latency and an unbounded queue, and a caller waiting 30 seconds for
// a sign-in has already given up. That is also what makes it safe to call
// under the gate mutex, which is where Admit calls it.
//
// The returned release is idempotent. A nil semaphore bounds nothing, matching
// a nil gate.
func (s *verifySemaphore) acquire() (release func(), ok bool) {
	if s == nil {
		return func() {}, true
	}
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
