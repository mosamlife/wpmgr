package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// Enforce-mode proofs for login admission control (GH #718 Phase 1).
//
// Every test here pins the gate's clock, so a budget cannot refill part-way
// through a run and the Retry-After values are exact.

var gateEpoch = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

func newEnforceGate(t *testing.T) (*LoginGate, *bytes.Buffer) {
	t.Helper()
	g, logs := newTestGate(t, LoginModeEnforce, 64)
	g.now = func() time.Time { return gateEpoch }
	return g, logs
}

// refusalWire is the 429 body.
type refusalWire struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details struct {
		Scope             string `json:"scope"`
		RetryAfterSeconds int    `json:"retry_after_seconds"`
	} `json:"details"`
}

func decodeRefusal(t *testing.T, w *httptest.ResponseRecorder) refusalWire {
	t.Helper()
	var r refusalWire
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatalf("429 body is not the error envelope: %v (%s)", err, w.Body.String())
	}
	return r
}

// expectedRetryAfter is what an exhausted bucket of this budget, at a pinned
// clock, must advertise: the time one token takes to refill, rounded up.
func expectedRetryAfter(budget int) int {
	return int(math.Ceil(loginWindow.Seconds() / float64(budget)))
}

func assertRefused(t *testing.T, w *httptest.ResponseRecorder, wantScope string, budget int) {
	t.Helper()
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d, want 429; body %s", w.Code, w.Body.String())
	}
	ra := w.Header().Get("Retry-After")
	secs, err := strconv.Atoi(ra)
	if err != nil {
		t.Fatalf("Retry-After %q is not whole seconds", ra)
	}
	if secs < 1 || secs > int(loginWindow.Seconds()) {
		t.Errorf("Retry-After %d is outside [1, %d]", secs, int(loginWindow.Seconds()))
	}
	if want := expectedRetryAfter(budget); secs != want {
		t.Errorf("Retry-After = %d, want %d (one token of a %d-per-%s budget)", secs, want, budget, loginWindow)
	}
	r := decodeRefusal(t, w)
	if r.Code != "too_many_attempts" {
		t.Errorf("code = %q, want too_many_attempts", r.Code)
	}
	if r.Details.Scope != wantScope {
		t.Errorf("details.scope = %q, want %q", r.Details.Scope, wantScope)
	}
	if r.Details.RetryAfterSeconds != secs {
		t.Errorf("details.retry_after_seconds = %d, header says %d", r.Details.RetryAfterSeconds, secs)
	}
}

func assertAdmitted(t *testing.T, w *httptest.ResponseRecorder, what string) {
	t.Helper()
	if w.Code == http.StatusTooManyRequests || w.Code == http.StatusServiceUnavailable {
		t.Fatalf("%s: refused with %d (%s); it should have been admitted", what, w.Code, w.Body.String())
	}
	if ra := w.Header().Get("Retry-After"); ra != "" {
		t.Fatalf("%s: admitted response carried Retry-After %q", what, ra)
	}
}

// ---------------------------------------------------------------------------
// Each refusing scope refuses at exactly its budget, and not one attempt before.
// ---------------------------------------------------------------------------

func TestEnforceRefusesAtEachBudgetBoundary(t *testing.T) {
	t.Run("pair: one source against one account", func(t *testing.T) {
		g, _ := newEnforceGate(t)
		e := loginHandlerForTest(t, g, 2)
		for i := 0; i < loginPairBudget; i++ {
			assertAdmitted(t, postLogin(e, simulatedClient, httpEmailA), fmt.Sprintf("attempt %d of %d", i+1, loginPairBudget))
		}
		assertRefused(t, postLogin(e, simulatedClient, httpEmailA), "pair", loginPairBudget)
		// The same source against a different account still has budget: the
		// pair refused, not the source.
		assertAdmitted(t, postLogin(e, simulatedClient, httpEmailB), "same source, other account")
	})

	t.Run("source: one address across many accounts", func(t *testing.T) {
		g, _ := newEnforceGate(t)
		e := loginHandlerForTest(t, g, 2)
		for i := 0; i < loginSrcBudget; i++ {
			assertAdmitted(t, postLogin(e, simulatedClient, fmt.Sprintf("user%d[at]example.test", i)), fmt.Sprintf("attempt %d of %d", i+1, loginSrcBudget))
		}
		assertRefused(t, postLogin(e, simulatedClient, "one-more[at]example.test"), "source", loginSrcBudget)
		// Another address is untouched.
		assertAdmitted(t, postLogin(e, simulatedOtherClient, "one-more[at]example.test"), "other source")
	})

	t.Run("network: many /64s inside one IPv6 /48", func(t *testing.T) {
		g, _ := newEnforceGate(t)
		e := loginHandlerForTest(t, g, 2)
		for i := 0; i < loginSrc48Budget; i++ {
			client := fmt.Sprintf("2001:db8:abcd:%x::1", i)
			assertAdmitted(t, postLogin(e, client, fmt.Sprintf("v6user%d[at]example.test", i)), fmt.Sprintf("attempt %d of %d", i+1, loginSrc48Budget))
		}
		// A fresh /64 and a fresh account: only the /48 can refuse this.
		assertRefused(t, postLogin(e, "2001:db8:abcd:ffff::1", "v6-one-more[at]example.test"), "network", loginSrc48Budget)
		// Another /48 is untouched.
		assertAdmitted(t, postLogin(e, "2001:db8:abce:1::1", "v6-one-more[at]example.test"), "other /48")
	})
}

// ---------------------------------------------------------------------------
// P1 — a stranger cannot refuse the owner. (GH #718 review, ported.)
// ---------------------------------------------------------------------------

// TestStrangerCannotRefuseTheOwnerFromAnotherSource is the lockout the rejected
// per-email design had, run against this gate. The stranger drives the
// ACCOUNT scope far past its budget from many sources, and floods its own pair
// until every one of its attempts is refused. The owner, from their own
// source, must still be admitted: the account scope is a signal and never
// refuses on its own.
func TestStrangerCannotRefuseTheOwnerFromAnotherSource(t *testing.T) {
	g, logs := newEnforceGate(t)
	e := loginHandlerForTest(t, g, 2)
	const victim = "victim[at]example.test"

	// Many sources, a full pair budget and then some from each.
	const strangerSources = 20
	refused := 0
	for s := 0; s < strangerSources; s++ {
		src := fmt.Sprintf("198.51.100.%d", s+1)
		for i := 0; i < loginPairBudget*3; i++ {
			if postLogin(e, src, victim).Code == http.StatusTooManyRequests {
				refused++
			}
		}
	}
	// Control: the stranger really was refused, and the account scope really
	// is exhausted, so the owner's admission below is not vacuous.
	if refused == 0 {
		t.Fatal("control: none of the stranger's attempts were refused; the gate is not enforcing")
	}
	if left := g.acct.tokensAt(g.AccountDigest(victim), gateEpoch); left >= 1 {
		t.Fatalf("control: the account scope still has %.1f tokens; the stranger did not exhaust it", left)
	}
	if !strings.Contains(logs.String(), "account over budget") {
		t.Fatalf("control: the account-signal line never fired.\nlog:\n%s", logs.String())
	}

	// The owner, from their own source.
	assertAdmitted(t, postLogin(e, "203.0.113.50", victim), "the owner from their own source")
}

// ---------------------------------------------------------------------------
// P2 — a refused attempt charges nothing. (GH #718 review, ported.)
// ---------------------------------------------------------------------------

// TestRefusedAttemptsChargeNothing: an attacker on a shared connection floods
// one account until the pair refuses, and keeps going. If refusals charged, the
// source budget everyone on that connection shares would be emptied by the
// refusals, and a colleague signing in to their own account would be refused.
func TestRefusedAttemptsChargeNothing(t *testing.T) {
	g, _ := newEnforceGate(t)
	e := loginHandlerForTest(t, g, 2)
	const office = simulatedClient
	const victim = "victim[at]example.test"

	for i := 0; i < loginPairBudget; i++ {
		assertAdmitted(t, postLogin(e, office, victim), "attacker within budget")
	}
	srcKey := srcKeyFor(netip.MustParseAddr(office))
	acctH := g.AccountDigest(victim)
	srcBefore := g.src.tokensAt(srcKey, gateEpoch)
	acctBefore := g.acct.tokensAt(acctH, gateEpoch)
	pairBefore := g.pair.tokensAt(srcKey+"|"+acctH, gateEpoch)

	// Far more refusals than the whole source budget.
	const flood = loginSrcBudget * 5
	for i := 0; i < flood; i++ {
		if w := postLogin(e, office, victim); w.Code != http.StatusTooManyRequests {
			t.Fatalf("flood attempt %d was not refused: %d", i+1, w.Code)
		}
	}

	if got := g.src.tokensAt(srcKey, gateEpoch); got != srcBefore {
		t.Errorf("source scope charged by refused attempts: %.1f -> %.1f tokens", srcBefore, got)
	}
	if got := g.acct.tokensAt(acctH, gateEpoch); got != acctBefore {
		t.Errorf("account scope charged by refused attempts: %.1f -> %.1f tokens", acctBefore, got)
	}
	if got := g.pair.tokensAt(srcKey+"|"+acctH, gateEpoch); got != pairBefore {
		t.Errorf("pair scope charged by refused attempts: %.1f -> %.1f tokens", pairBefore, got)
	}
	// And the consequence: a colleague on the same connection still gets in.
	assertAdmitted(t, postLogin(e, office, "colleague[at]example.test"), "a colleague on the same connection")
}

// successfulAttempt runs one attempt through Admit and completes it the way
// h.login completes a password that verified: giveBack, then finish. It returns
// the refusal, if Admit refused.
func successfulAttempt(g *LoginGate, a loginAttempt) *loginRefusal {
	ad, r := g.Admit(context.Background(), a)
	ad.giveBack()
	ad.finish()
	return r
}

// TestSuccessGivesTheChargeBack pins the outcome that must not count against a
// budget at the gate: a password that verified. The handler's half of it, that
// h.login actually makes the call and in the right order, is
// TestLoginGivesTheChargeBackOnSuccess, and the database-backed proof through
// the real router is in apps/api/tests.
func TestSuccessGivesTheChargeBack(t *testing.T) {
	g, _ := newEnforceGate(t)
	addr := netip.MustParseAddr(simulatedClient)
	a := loginAttempt{Addr: addr, FromChain: true, Hops: 2, Email: "person@example.test"}

	// Many more successful sign-ins than any budget: none may be refused.
	for i := 0; i < loginSrc48Budget*2; i++ {
		if refusal := successfulAttempt(g, a); refusal != nil {
			t.Fatalf("sign-in %d refused (%s) although every earlier one succeeded", i+1, refusal.scope)
		}
	}
	srcKey := srcKeyFor(addr)
	if got := g.src.tokensAt(srcKey, gateEpoch); got != loginSrcBudget {
		t.Errorf("source scope has %.1f tokens after only successes, want %d", got, loginSrcBudget)
	}
	for _, b := range []*keyedBudget{g.pair, g.src, g.src48, g.acct} {
		if got := b.size(); got != 0 {
			t.Errorf("%s map holds %d entries after only successful sign-ins, want 0", b.scope, got)
		}
	}
	if got := g.verify.inFlight(); got != 0 {
		t.Errorf("%d verification slots still held after every admission finished", got)
	}
}

// TestGiveBackIsIdempotent: a second giveBack must return nothing. It is
// checked against a bucket another attempt charged in between, because that is
// the only state in which a second return is visible: against a bucket already
// at its budget the cap in give() would hide it.
func TestGiveBackIsIdempotent(t *testing.T) {
	g, _ := newEnforceGate(t)
	addr := netip.MustParseAddr(simulatedClient)
	a := loginAttempt{Addr: addr, FromChain: true, Hops: 2, Email: "person@example.test"}
	srcKey := srcKeyFor(addr)
	acctH := g.AccountDigest(a.Email)
	pairKey := srcKey + "|" + acctH

	first, r := g.Admit(context.Background(), a)
	if r != nil {
		t.Fatalf("first attempt refused: %+v", r)
	}
	// A second attempt on the same keys, which fails and so stays charged.
	if r := failedAttempt(g, a); r != nil {
		t.Fatalf("second attempt refused: %+v", r)
	}

	first.giveBack()
	first.giveBack()
	first.finish()

	for _, c := range []struct {
		name string
		b    *keyedBudget
		key  string
	}{{"pair", g.pair, pairKey}, {"source", g.src, srcKey}, {"account", g.acct, acctH}} {
		if got, want := c.b.tokensAt(c.key, gateEpoch), float64(c.b.limit-1); got != want {
			t.Errorf("%s scope has %.1f tokens, want %.1f: the second giveBack returned a token the other attempt's charge had taken",
				c.name, got, want)
		}
		// The entry the first attempt created carries the second's kept
		// charge, so the first's give-back must not have removed it.
		if got := c.b.size(); got != 1 {
			t.Errorf("%s map holds %d entries, want 1 (the failed attempt's)", c.name, got)
		}
	}
}

// TestFinishKeepsWhatWasNotGivenBack: finish settles as kept, so a giveBack
// after it returns nothing. That is why h.login must give back BEFORE it
// finishes, and why TestLoginGivesTheChargeBackOnSuccess pins the order.
func TestFinishKeepsWhatWasNotGivenBack(t *testing.T) {
	g, _ := newEnforceGate(t)
	addr := netip.MustParseAddr(simulatedClient)
	a := loginAttempt{Addr: addr, FromChain: true, Hops: 2, Email: "person@example.test"}
	ad, r := g.Admit(context.Background(), a)
	if r != nil {
		t.Fatalf("refused: %+v", r)
	}
	ad.finish()
	ad.giveBack()
	if got, want := g.src.tokensAt(srcKeyFor(addr), gateEpoch), float64(loginSrcBudget-1); got != want {
		t.Errorf("source scope has %.1f tokens after finish then giveBack, want %.1f", got, want)
	}
	if got := g.verify.inFlight(); got != 0 {
		t.Errorf("finish left %d verification slots held", got)
	}
}

// TestShedAttemptsLeaveEveryMapAsItWas: an attempt the verification bound sheds
// must charge nothing and create nothing. If it created bucket entries, a
// caller at zero budget could keep adding them while the bound is saturated
// until each map's cap evicted the oldest entries, its own exhausted pair
// among them, which would come back with a full budget.
func TestShedAttemptsLeaveEveryMapAsItWas(t *testing.T) {
	const victim = "victim[at]example.test"

	occupy := func(t *testing.T, g *LoginGate) (release func()) {
		t.Helper()
		var rs []func()
		for i := 0; i < g.verify.capacity(); i++ {
			r, ok := g.verify.acquire()
			if !ok {
				t.Fatalf("could not occupy verify slot %d", i+1)
			}
			rs = append(rs, r)
		}
		return func() {
			for _, r := range rs {
				r()
			}
		}
	}
	t.Run("sizes and tokens unchanged", func(t *testing.T) {
		g, _ := newEnforceGate(t)
		e := loginHandlerForTest(t, g, 2)
		// Existing state to disturb: a drained pair, and a v6 client.
		for i := 0; i < loginPairBudget; i++ {
			assertAdmitted(t, postLogin(e, simulatedClient, victim), "warm-up")
		}
		assertAdmitted(t, postLogin(e, "2001:db8:1:2::1", httpEmailA), "v6 warm-up")
		before := snapshotGate(g, gateEpoch)

		release := occupy(t, g)
		shed := 0
		for i := 0; i < 200; i++ {
			for _, try := range []struct{ client, email string }{
				{simulatedClient, fmt.Sprintf("new%d[at]example.test", i)},             // new pair, new account
				{fmt.Sprintf("198.51.100.%d", i%250+1), httpEmailA},                    // new source
				{fmt.Sprintf("2001:db8:%x:1::1", i+100), fmt.Sprintf("v6-%d[at]x", i)}, // new /64 and /48
				{"2001:db8:1:2::1", httpEmailA},                                        // existing keys
			} {
				w := postLogin(e, try.client, try.email)
				if w.Code != http.StatusServiceUnavailable {
					t.Fatalf("shed attempt from %s: status %d, want 503 (%s)", try.client, w.Code, w.Body.String())
				}
				shed++
			}
		}
		release()

		assertSameMaps(t, fmt.Sprintf("%d shed attempts", shed), before, snapshotGate(g, gateEpoch))
	})

	t.Run("at the cap they cannot evict an exhausted pair", func(t *testing.T) {
		g, _ := newEnforceGate(t)
		clock := gateEpoch
		g.now = func() time.Time { return clock }
		e := loginHandlerForTest(t, g, 2)

		for i := 0; i < loginPairBudget; i++ {
			assertAdmitted(t, postLogin(e, simulatedClient, victim), "draining the pair")
		}
		// Fill the pair map to its cap with entries seen later, so the drained
		// pair is the least recently seen: the first one an eviction takes.
		clock = gateEpoch.Add(time.Second)
		for i := 0; g.pair.size() < loginBucketCap; i++ {
			keptCharge(t, g.pair, fmt.Sprintf("filler-%d", i), clock)
		}

		clock = gateEpoch.Add(2 * time.Second)
		release := occupy(t, g)
		for i := 0; i < 200; i++ {
			if w := postLogin(e, simulatedClient, fmt.Sprintf("churn%d[at]example.test", i)); w.Code != http.StatusServiceUnavailable {
				t.Fatalf("churn attempt %d: status %d, want 503", i+1, w.Code)
			}
		}
		release()

		if got := g.pair.size(); got != loginBucketCap {
			t.Errorf("pair map holds %d entries after shed attempts, want %d (unchanged)", got, loginBucketCap)
		}
		w := postLogin(e, simulatedClient, victim)
		if w.Code != http.StatusTooManyRequests {
			t.Fatalf("the drained pair was admitted again (%d) after shed attempts at the cap: it was evicted and came back full", w.Code)
		}
		if r := decodeRefusal(t, w); r.Details.Scope != "pair" {
			t.Errorf("refused on %q, want pair", r.Details.Scope)
		}
	})
}

// gateSnap is every entry of every scope's map, with its tokens at one instant.
type gateSnap struct {
	sizes  [4]int
	tokens map[string]float64
}

func snapshotGate(g *LoginGate, now time.Time) gateSnap {
	s := gateSnap{tokens: map[string]float64{}}
	for i, b := range []*keyedBudget{g.pair, g.src, g.src48, g.acct} {
		b.mu.Lock()
		s.sizes[i] = len(b.buckets)
		for k, bk := range b.buckets {
			s.tokens[b.scope+" "+k] = bk.lim.TokensAt(now)
		}
		b.mu.Unlock()
	}
	return s
}

// assertSameMaps requires after to hold exactly the entries before held, each
// with exactly the same tokens.
func assertSameMaps(t *testing.T, what string, before, after gateSnap) {
	t.Helper()
	if after.sizes != before.sizes {
		t.Errorf("%s changed the map sizes [pair src src48 acct] from %v to %v", what, before.sizes, after.sizes)
	}
	for k, v := range before.tokens {
		if got, ok := after.tokens[k]; !ok || got != v {
			t.Errorf("%s: %s held %.2f tokens before, %.2f (present=%v) after", what, k, v, got, ok)
		}
	}
	for k := range after.tokens {
		if _, ok := before.tokens[k]; !ok {
			t.Errorf("%s created %s", what, k)
		}
	}
}

// assertOnlyFailuresRemain requires every entry in every map to carry a kept
// charge and no pending one: once every admission has finished, an entry exists
// only because a failed attempt left it.
func assertOnlyFailuresRemain(t *testing.T, g *LoginGate) {
	t.Helper()
	for _, b := range []*keyedBudget{g.pair, g.src, g.src48, g.acct} {
		b.mu.Lock()
		for k, bk := range b.buckets {
			if bk.pending != 0 || bk.kept == 0 {
				t.Errorf("%s %s: pending %d, kept %d; want no pending charge and at least one kept", b.scope, k, bk.pending, bk.kept)
			}
		}
		b.mu.Unlock()
	}
}

// TestSuccessfulSignInsLeaveEveryMapAsItWas: a successful sign-in must leave
// every map exactly as it found it. Its charge is given back, and an entry the
// charge created must go with it; creating one must not evict anything either.
// Otherwise one account whose password the caller knows, signed into from a
// fresh /64 each time, adds entries without limit, and at each map's cap the
// eviction removes the oldest, other keys' drained buckets among them, which
// come back full.
func TestSuccessfulSignInsLeaveEveryMapAsItWas(t *testing.T) {
	const (
		victim = "victim[at]example.test"
		mine   = "mine@example.test"
	)
	// v6 addresses: distinct /64s inside one /48.
	in48 := func(prefix string, i int) netip.Addr {
		return netip.MustParseAddr(fmt.Sprintf("%s:%x::1", prefix, i))
	}
	success := func(t *testing.T, g *LoginGate, addr netip.Addr, email string) {
		t.Helper()
		if r := successfulAttempt(g, loginAttempt{Addr: addr, FromChain: true, Hops: 2, Email: email}); r != nil {
			t.Fatalf("successful sign-in from %s refused (%s)", addr, r.scope)
		}
	}

	t.Run("sizes and tokens unchanged", func(t *testing.T) {
		g, _ := newEnforceGate(t)
		e := loginHandlerForTest(t, g, 2)
		// Existing state to disturb: a drained pair, a v6 client whose /48 the
		// sign-ins below share, and a failed attempt on the account they use.
		for i := 0; i < loginPairBudget; i++ {
			assertAdmitted(t, postLogin(e, simulatedClient, victim), "warm-up")
		}
		assertAdmitted(t, postLogin(e, "2001:db8:1:2::1", httpEmailA), "v6 warm-up")
		if r := failedAttempt(g, loginAttempt{Addr: netip.MustParseAddr(simulatedOtherClient), FromChain: true, Hops: 2, Email: mine}); r != nil {
			t.Fatalf("warm-up failure refused: %+v", r)
		}
		before := snapshotGate(g, gateEpoch)

		// More sign-ins than the /48 budget, each from its own /64: one /48
		// with an entry already, one without. Then the existing keys.
		n := 0
		for i := 0; i < loginSrc48Budget*2; i++ {
			success(t, g, in48("2001:db8:1", i+0x100), mine)
			success(t, g, in48("2001:db8:2", i), mine)
			n += 2
		}
		for i := 0; i < loginSrcBudget*2; i++ {
			success(t, g, netip.MustParseAddr("2001:db8:1:2::1"), httpEmailA)
			success(t, g, netip.MustParseAddr(simulatedOtherClient), mine)
			n += 2
		}

		assertSameMaps(t, fmt.Sprintf("%d successful sign-ins", n), before, snapshotGate(g, gateEpoch))
		assertOnlyFailuresRemain(t, g)
		if got := g.verify.inFlight(); got != 0 {
			t.Errorf("%d verification slots still held", got)
		}
	})

	t.Run("at the cap they cannot evict a drained pair or source", func(t *testing.T) {
		g, _ := newEnforceGate(t)
		clock := gateEpoch
		g.now = func() time.Time { return clock }
		e := loginHandlerForTest(t, g, 2)
		const drainedSource = "198.51.100.7"

		for i := 0; i < loginPairBudget; i++ {
			assertAdmitted(t, postLogin(e, simulatedClient, victim), "draining the pair")
		}
		for i := 0; i < loginSrcBudget; i++ {
			assertAdmitted(t, postLogin(e, drainedSource, fmt.Sprintf("drain%d[at]example.test", i)), "draining the source")
		}
		// Fill every refusing scope's map to its cap with failed attempts' entries
		// seen later, so the drained pair and source are the least recently
		// seen: the first an eviction takes.
		clock = gateEpoch.Add(time.Second)
		for _, b := range []*keyedBudget{g.pair, g.src, g.src48} {
			for i := 0; b.size() < loginBucketCap; i++ {
				keptCharge(t, b, fmt.Sprintf("filler-%d", i), clock)
			}
		}
		at := gateEpoch.Add(2 * time.Second)
		before := snapshotGate(g, at)

		clock = gateEpoch.Add(2 * time.Second)
		n := 0
		for i := 0; i < loginBucketCap*2; i++ {
			success(t, g, in48("2001:db8:9", i), mine)
			n++
		}

		assertSameMaps(t, fmt.Sprintf("%d successful sign-ins at the cap", n), before, snapshotGate(g, at))
		assertOnlyFailuresRemain(t, g)

		w := postLogin(e, simulatedClient, victim)
		if w.Code != http.StatusTooManyRequests {
			t.Fatalf("the drained pair was admitted again (%d) after successful sign-ins at the cap: it was evicted and came back full", w.Code)
		}
		if r := decodeRefusal(t, w); r.Details.Scope != "pair" {
			t.Errorf("drained pair refused on %q, want pair", r.Details.Scope)
		}
		w = postLogin(e, drainedSource, "someone-new[at]example.test")
		if w.Code != http.StatusTooManyRequests {
			t.Fatalf("the drained source was admitted again (%d) after successful sign-ins at the cap: it was evicted and came back full", w.Code)
		}
		if r := decodeRefusal(t, w); r.Details.Scope != "source" {
			t.Errorf("drained source refused on %q, want source", r.Details.Scope)
		}
	})

	t.Run("interleaved on one new key", func(t *testing.T) {
		g, _ := newEnforceGate(t)
		addr := netip.MustParseAddr(simulatedClient)
		a := loginAttempt{Addr: addr, FromChain: true, Hops: 2, Email: mine}

		// Two successes in flight on the same new keys, finishing in either
		// order: nothing may remain.
		for _, firstDone := range []bool{true, false} {
			x, r1 := g.Admit(context.Background(), a)
			y, r2 := g.Admit(context.Background(), a)
			if r1 != nil || r2 != nil {
				t.Fatalf("refused: %+v %+v", r1, r2)
			}
			if !firstDone {
				x, y = y, x
			}
			x.giveBack()
			x.finish()
			y.giveBack()
			y.finish()
			for _, b := range []*keyedBudget{g.pair, g.src, g.acct} {
				if got := b.size(); got != 0 {
					t.Errorf("%s map holds %d entries after two interleaved successes, want 0", b.scope, got)
				}
			}
		}

		// A success and a failure in flight on the same new keys: the failure's
		// charge stays, on an entry the success created.
		x, _ := g.Admit(context.Background(), a)
		y, _ := g.Admit(context.Background(), a)
		y.finish()
		x.giveBack()
		x.finish()
		srcKey := srcKeyFor(addr)
		if got, want := g.src.tokensAt(srcKey, gateEpoch), float64(loginSrcBudget-1); got != want {
			t.Errorf("source holds %.1f tokens, want %.1f: the failure's charge must stay", got, want)
		}
		if got := g.src.size(); got != 1 {
			t.Errorf("source map holds %d entries, want 1 (the failure's)", got)
		}
		assertOnlyFailuresRemain(t, g)
	})
}

// ---------------------------------------------------------------------------
// P4 — every refusal is logged, at WARN, without the email. (GH #718 review.)
// ---------------------------------------------------------------------------

func TestEveryRefusalLogsAWarnWithoutTheEmail(t *testing.T) {
	g, logs := newEnforceGate(t)
	e := loginHandlerForTest(t, g, 2)
	// Mixed case, so the normalised form differs from what was submitted and
	// both can be looked for.
	const email = "Victim.Person[at]Example.TEST"

	for i := 0; i < loginPairBudget; i++ {
		postLogin(e, simulatedClient, email)
	}
	if strings.Contains(logs.String(), `"level":"WARN"`) {
		t.Fatalf("a WARN was written before anything was refused.\nlog:\n%s", logs.String())
	}

	const refusals = 25
	for i := 0; i < refusals; i++ {
		if w := postLogin(e, simulatedClient, email); w.Code != http.StatusTooManyRequests {
			t.Fatalf("attempt %d past the budget was not refused: %d", i+1, w.Code)
		}
	}

	var warns int
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %v (%q)", err, line)
		}
		if rec["msg"] != "login admission: refused" {
			continue
		}
		warns++
		if rec["level"] != "WARN" {
			t.Errorf("refusal logged at %v, want WARN", rec["level"])
		}
		if rec["scope"] != loginScopePair {
			t.Errorf("refusal line scope = %v, want %s", rec["scope"], loginScopePair)
		}
		if rec["source"] != simulatedClient {
			t.Errorf("refusal line source = %v, want %s", rec["source"], simulatedClient)
		}
		if rec["acct_h"] != g.AccountDigest(email) {
			t.Errorf("refusal line acct_h = %v, want the keyed digest", rec["acct_h"])
		}
	}
	if warns != refusals {
		t.Errorf("%d refusal lines for %d refusals; every refusal must be logged", warns, refusals)
	}
	out := logs.String()
	for _, needle := range []string{email, normalizeEmail(email), strings.ToLower(email)} {
		if strings.Contains(out, needle) {
			t.Fatalf("the submitted email (%q) reached the log.\nlog:\n%s", needle, out)
		}
	}
}

// ---------------------------------------------------------------------------
// The 429 is not an account-existence oracle.
// ---------------------------------------------------------------------------

// TestRefusalIsDecidedWithoutAnyAccountLookup drives refusals through the real
// handler with NO SERVICE behind it. A handler with no service cannot look an
// account up, so a refusal it answers was decided and written without one; if
// the refused path ever reached the service, this panics.
//
// It then compares the refusals for two addresses (one standing for an account
// that exists, one for an address nobody registered: the gate is handed a
// string either way and cannot tell them apart) and requires them to be
// identical in status, body and every header, for both refusing kinds.
func TestRefusalIsDecidedWithoutAnyAccountLookup(t *testing.T) {
	g, _ := newEnforceGate(t)
	gin.SetMode(gin.ReleaseMode)
	h := &Handler{} // no service, no session store
	h.SetProxyHops(2)
	h.SetLoginGate(g)
	e := gin.New()
	e.POST("/auth/login", h.login)

	const existing = "owner@example.test"
	const unknown = "nobody-registered-this@example.test"
	exhaustPair := func(email string) {
		for i := 0; i < loginPairBudget; i++ {
			if r := failedAttempt(g, loginAttempt{Addr: netip.MustParseAddr(simulatedClient), FromChain: true, Hops: 2, Email: email}); r != nil {
				t.Fatalf("warm-up refused early: %s", r.scope)
			}
		}
	}

	compare := func(t *testing.T, a, b *httptest.ResponseRecorder) {
		t.Helper()
		if a.Code != http.StatusTooManyRequests || b.Code != http.StatusTooManyRequests {
			t.Fatalf("want two 429s, got %d and %d", a.Code, b.Code)
		}
		if a.Body.String() != b.Body.String() {
			t.Errorf("bodies differ:\n existing: %s\n unknown:  %s", a.Body.String(), b.Body.String())
		}
		for k, v := range a.Header() {
			if got := b.Header()[k]; strings.Join(got, ",") != strings.Join(v, ",") {
				t.Errorf("header %q differs: %v vs %v", k, v, got)
			}
		}
		for k := range b.Header() {
			if _, ok := a.Header()[k]; !ok {
				t.Errorf("header %q present only on one response", k)
			}
		}
	}

	t.Run("pair", func(t *testing.T) {
		exhaustPair(existing)
		exhaustPair(unknown)
		compare(t, postLogin(e, simulatedClient, existing), postLogin(e, simulatedClient, unknown))
	})

	t.Run("source", func(t *testing.T) {
		// Exhaust a second address's source budget on throwaway accounts.
		other := netip.MustParseAddr(simulatedOtherClient)
		for i := 0; i < loginSrcBudget; i++ {
			failedAttempt(g, loginAttempt{Addr: other, FromChain: true, Hops: 2, Email: fmt.Sprintf("t%d@example.test", i)})
		}
		compare(t, postLogin(e, simulatedOtherClient, existing), postLogin(e, simulatedOtherClient, unknown))
	})
}

// ---------------------------------------------------------------------------
// Observe refuses nothing, however far over every budget it is driven.
// ---------------------------------------------------------------------------

func TestObserveRefusesNothingAtAnyScope(t *testing.T) {
	g, logs := newTestGate(t, LoginModeObserve, 64)
	g.now = func() time.Time { return gateEpoch }
	e := loginHandlerForTest(t, g, 2)

	drive := []struct {
		what   string
		n      int
		client func(i int) string
		email  func(i int) string
	}{
		{"pair", loginPairBudget * 5, func(int) string { return simulatedClient }, func(int) string { return httpEmailA }},
		{"source", loginSrcBudget * 2, func(int) string { return simulatedOtherClient }, func(i int) string { return fmt.Sprintf("o%d[at]example.test", i) }},
		{"network", loginSrc48Budget * 2, func(i int) string { return fmt.Sprintf("2001:db8:beef:%x::1", i) }, func(i int) string { return fmt.Sprintf("n%d[at]example.test", i) }},
	}
	for _, d := range drive {
		for i := 0; i < d.n; i++ {
			w := postLogin(e, d.client(i), d.email(i))
			if w.Code == http.StatusTooManyRequests || w.Header().Get("Retry-After") != "" {
				t.Fatalf("observe mode refused %s attempt %d: %d %s", d.what, i+1, w.Code, w.Body.String())
			}
		}
	}
	out := logs.String()
	for _, scope := range []string{loginScopePair, loginScopeSrc + `"`, loginScopeSrc48} {
		if !strings.Contains(out, scope) {
			t.Errorf("control: scope %s never went over budget, so observe was not tested there.\nlog:\n%s", scope, out)
		}
	}
	if strings.Contains(out, "login admission: refused") {
		t.Errorf("observe mode wrote a refusal line.\nlog:\n%s", out)
	}
}

// TestConcurrentAttemptsCannotOvershootTheBoundary: query and charge are one
// step under the gate lock, so however many attempts arrive at once, exactly
// the budget is admitted.
func TestConcurrentAttemptsCannotOvershootTheBoundary(t *testing.T) {
	g, _ := newEnforceGate(t)
	a := loginAttempt{Addr: netip.MustParseAddr(simulatedClient), FromChain: true, Hops: 2, Email: "target@example.test"}
	const racers = 200
	results := make(chan bool, racers)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		go func() {
			<-start
			r := failedAttempt(g, a)
			results <- r == nil
		}()
	}
	close(start)
	admitted := 0
	for i := 0; i < racers; i++ {
		if <-results {
			admitted++
		}
	}
	if admitted != loginPairBudget {
		t.Errorf("%d of %d concurrent attempts admitted, want exactly %d", admitted, racers, loginPairBudget)
	}
}

// TestUnresolvedSourceIsNeverRefused pins the addrUnresolved rule under
// enforce: every client of a deployment whose peer address does not parse is
// on one key, so refusing on it would refuse all of them, and its pair would
// be keyed on the account alone.
func TestUnresolvedSourceIsNeverRefused(t *testing.T) {
	g, logs := newEnforceGate(t)
	a := loginAttempt{Hops: 2, Email: "someone@example.test"}
	for i := 0; i < loginSrcBudget*2; i++ {
		if r := failedAttempt(g, a); r != nil {
			t.Fatalf("attempt %d from an unresolved source was refused (%s)", i+1, r.scope)
		}
	}
	assertDegradedWarn(t, logs, "unresolved")
}

// assertDegradedWarn requires at least one not-refused line for addrSource at
// WARN, sampled rather than one per attempt, and no refusal line at all.
func assertDegradedWarn(t *testing.T, logs *bytes.Buffer, addrSource string) {
	t.Helper()
	warns := 0
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %v (%q)", err, line)
		}
		msg, _ := rec["msg"].(string)
		if msg == "login admission: refused" {
			t.Errorf("a refusal was logged for a %s address: %s", addrSource, line)
			continue
		}
		if !strings.Contains(msg, "source address is degraded") {
			continue
		}
		warns++
		if rec["level"] != "WARN" {
			t.Errorf("degraded-address line at %v, want WARN", rec["level"])
		}
		if rec["addr_source"] != addrSource {
			t.Errorf("degraded-address line addr_source = %v, want %s", rec["addr_source"], addrSource)
		}
		if addrSource == "peer_fallback" && rec["remedy"] == nil {
			t.Errorf("peer_fallback line carries no remedy: %s", line)
		}
	}
	if warns == 0 {
		t.Errorf("no WARN for a would-refuse on a %s address; the misconfiguration is invisible.\nlog:\n%s", addrSource, logs.String())
	}
}

// postLoginVia sends one attempt with an exact X-Forwarded-For value (empty
// for none) from an exact peer.
func postLoginVia(e *gin.Engine, xff, peer, email string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(loginBody{Email: email, Password: "irrelevant"})
	req := httptest.NewRequest(http.MethodPost, "/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	req.RemoteAddr = peer
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	return w
}

// TestPeerFallbackIsNeverRefused: with WPMGR_AUTH_PROXY_HOPS at 2 and a chain
// shorter than that, every attempt is keyed on the TCP peer. Every client on
// that path shares the key, so refusing on it would let a stranger lock an
// account's owner out from anywhere. It must be measured, logged at WARN, and
// admitted, in enforce mode, however far over budget it goes.
func TestPeerFallbackIsNeverRefused(t *testing.T) {
	const victim = "victim[at]example.test"
	const peer = "10.9.9.9"
	for _, shape := range []struct{ name, xff string }{
		{"one entry for two hops", "198.51.100.66"},
		{"no chain at all", ""},
	} {
		t.Run(shape.name, func(t *testing.T) {
			g, logs := newEnforceGate(t)
			e := loginHandlerForTest(t, g, 2)
			// Past the pair budget on the victim, then past the source budget
			// across other accounts. (An attempt over the pair charges nothing,
			// so the first loop alone leaves the source with budget.)
			const attempts = loginPairBudget*3 + loginSrcBudget*2
			for i := 0; i < loginPairBudget*3; i++ {
				assertAdmitted(t, postLoginVia(e, shape.xff, peer+":4444", victim), fmt.Sprintf("stranger attempt %d on the victim", i+1))
			}
			for i := 0; i < loginSrcBudget*2; i++ {
				assertAdmitted(t, postLoginVia(e, shape.xff, peer+":4444", fmt.Sprintf("spray%d[at]example.test", i)), fmt.Sprintf("stranger attempt %d across accounts", i+1))
			}
			// Control: the shared key really is past both refusing budgets, so
			// the admissions above are not vacuous.
			peerKey := srcKeyFor(netip.MustParseAddr(peer))
			if left := g.pair.tokensAt(peerKey+"|"+g.AccountDigest(victim), gateEpoch); left >= 1 {
				t.Fatalf("control: the pair still has %.1f tokens; the attempts were not keyed on the peer", left)
			}
			if left := g.src.tokensAt(peerKey, gateEpoch); left >= 1 {
				t.Fatalf("control: the source still has %.1f tokens", left)
			}
			// The owner, arriving the same way from somewhere else.
			assertAdmitted(t, postLoginVia(e, "203.0.113.50", peer+":5555", victim), "the owner on the same path")
			assertDegradedWarn(t, logs, "peer_fallback")
			if n := strings.Count(logs.String(), "source address is degraded"); n >= attempts {
				t.Errorf("%d degraded-address lines for %d attempts; the line is not sampled", n, attempts)
			}
		})
	}
}

// TestConfiguredPeerIsStillRefused is the other side of the fallback rule: with
// the hop count at 0 the peer IS the configured source, nothing about it is
// degraded, and enforce refuses on it like any other key.
func TestConfiguredPeerIsStillRefused(t *testing.T) {
	g, _ := newEnforceGate(t)
	e := loginHandlerForTest(t, g, 0)
	for i := 0; i < loginPairBudget; i++ {
		assertAdmitted(t, postLoginVia(e, "", simulatedClient+":4444", httpEmailA), fmt.Sprintf("attempt %d", i+1))
	}
	assertRefused(t, postLoginVia(e, "", simulatedClient+":4444", httpEmailA), "pair", loginPairBudget)
}

// TestRetryAfterCoversTheLongestWait: when more than one refusing scope is
// over, Retry-After must be the longest wait among them and the scope must be
// that one. Answering the first scope checked would tell a client to come back
// before the other has room, and it would be refused again.
//
// The source is charged 50 at t0, the pair drained at t0+7s, the source drained
// at t0+95s. At that instant the pair has room in about 2s and the source in
// about 10s.
// shortfallOf is how long key's bucket waits for a token at now. A missing
// bucket fails the calling test on its own, rather than dereferencing nil and
// taking every later test in the binary down with it.
func shortfallOf(t *testing.T, b *keyedBudget, key string, now time.Time) time.Duration {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	bk, ok := b.buckets[key]
	if !ok {
		t.Fatalf("%s has no bucket for %q", b.scope, key)
	}
	return bk.lim.shortfall(now)
}

func TestRetryAfterCoversTheLongestWait(t *testing.T) {
	g, _ := newEnforceGate(t)
	clock := gateEpoch
	g.now = func() time.Time { return clock }
	e := loginHandlerForTest(t, g, 2)
	const victim = "victim[at]example.test"

	for i := 0; i < 50; i++ {
		assertAdmitted(t, postLogin(e, simulatedClient, fmt.Sprintf("other%d[at]example.test", i)), "t0 source charge")
	}
	clock = gateEpoch.Add(7 * time.Second)
	for i := 0; i < loginPairBudget; i++ {
		assertAdmitted(t, postLogin(e, simulatedClient, victim), "t0+7s pair drain")
	}
	clock = gateEpoch.Add(95 * time.Second)
	drained := false
	for i := 0; i <= loginSrcBudget; i++ {
		if postLogin(e, simulatedClient, fmt.Sprintf("late%d[at]example.test", i)).Code == http.StatusTooManyRequests {
			drained = true
			break
		}
	}
	if !drained {
		t.Fatal("control: the source never refused")
	}

	srcKey := srcKeyFor(netip.MustParseAddr(simulatedClient))
	pairWait := shortfallOf(t, g.pair, srcKey+"|"+g.AccountDigest(victim), clock)
	srcWait := shortfallOf(t, g.src, srcKey, clock)
	// Control: both are over, and the longer wait is NOT the first scope
	// checked, so choosing the first scope would give the wrong answer.
	if pairWait <= 0 || srcWait <= pairWait {
		t.Fatalf("control: want both scopes over with the source waiting longer; pair %v, source %v", pairWait, srcWait)
	}

	w := postLogin(e, simulatedClient, victim)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d, want 429", w.Code)
	}
	secs, _ := strconv.Atoi(w.Header().Get("Retry-After"))
	if want := int(math.Ceil(srcWait.Seconds())); secs != want {
		t.Errorf("Retry-After = %d, want %d (the source's wait; the pair's is %v)", secs, want, pairWait)
	}
	if r := decodeRefusal(t, w); r.Details.Scope != "source" || r.Details.RetryAfterSeconds != secs {
		t.Errorf("details = %+v, want scope source and retry_after_seconds equal to the header's %d", r.Details, secs)
	}

	// The contract a client relies on: waiting exactly Retry-After is enough.
	clock = clock.Add(time.Duration(secs) * time.Second)
	assertAdmitted(t, postLogin(e, simulatedClient, victim), "retry after exactly Retry-After")
}

// TestIdleSweepNeverResetsADrainedPairWithinTheWindow pins loginBucketIdle
// against loginWindow. Any new key runs the sweep; if the idle time were
// shorter than the window, that sweep would drop a drained pair whose window is
// still running and it would come back with its whole budget.
func TestIdleSweepNeverResetsADrainedPairWithinTheWindow(t *testing.T) {
	// Errorf, not Fatalf: the behavioural half below must also run, and fail,
	// when the constant is wrong.
	if loginBucketIdle < loginWindow {
		t.Errorf("loginBucketIdle (%s) is shorter than loginWindow (%s)", loginBucketIdle, loginWindow)
	}
	for _, after := range []time.Duration{2 * time.Minute, loginWindow / 2, loginWindow - time.Second} {
		t.Run(after.String(), func(t *testing.T) {
			g, _ := newEnforceGate(t)
			clock := gateEpoch
			g.now = func() time.Time { return clock }
			e := loginHandlerForTest(t, g, 2)
			for i := 0; i < loginPairBudget; i++ {
				assertAdmitted(t, postLogin(e, simulatedClient, httpEmailA), "draining the pair")
			}

			clock = gateEpoch.Add(after)
			// A new key, from another client against another account: this runs
			// the sweep and touches nothing of the drained pair.
			assertAdmitted(t, postLogin(e, simulatedOtherClient, httpEmailB), "a new key")

			pairKey := srcKeyFor(netip.MustParseAddr(simulatedClient)) + "|" + g.AccountDigest(httpEmailA)
			want := after.Seconds() * float64(loginPairBudget) / loginWindow.Seconds()
			if got := g.pair.tokensAt(pairKey, clock); math.Abs(got-want) > 1e-6 {
				t.Fatalf("drained pair holds %.3f tokens %s later, want %.3f from refill alone: the sweep reset it", got, after, want)
			}
			admitted := 0
			for i := 0; i < loginPairBudget; i++ {
				if postLogin(e, simulatedClient, httpEmailA).Code != http.StatusTooManyRequests {
					admitted++
				}
			}
			if admitted != int(want) {
				t.Errorf("%d attempts admitted on the drained pair %s later, want %d", admitted, after, int(want))
			}
		})
	}
}
