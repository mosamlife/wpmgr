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

// TestSuccessAndShedGiveTheChargeBack pins the other two outcomes that must not
// count against a budget: a password that verified, and an attempt the
// concurrency bound shed before any password was checked.
func TestSuccessAndShedGiveTheChargeBack(t *testing.T) {
	g, _ := newEnforceGate(t)
	addr := netip.MustParseAddr(simulatedClient)
	a := loginAttempt{Addr: addr, FromChain: true, Hops: 2, Email: "person@example.test"}

	// Many more successful sign-ins than any budget: none may be refused.
	for i := 0; i < loginSrc48Budget*2; i++ {
		ad, refusal := g.Admit(context.Background(), a)
		if refusal != nil {
			t.Fatalf("sign-in %d refused (%s) although every earlier one succeeded", i+1, refusal.scope)
		}
		ad.giveBack()
		ad.giveBack() // idempotent: a second call returns nothing more
	}
	srcKey := srcKeyFor(addr)
	if got := g.src.tokensAt(srcKey, gateEpoch); got != loginSrcBudget {
		t.Errorf("source scope has %.1f tokens after only successes, want %d", got, loginSrcBudget)
	}

	// Through the handler: a shed attempt gives its charge back.
	e := loginHandlerForTest(t, g, 2)
	var releases []func()
	for i := 0; i < g.verify.capacity(); i++ {
		r, ok := g.verify.acquire()
		if !ok {
			t.Fatalf("could not occupy verify slot %d", i+1)
		}
		releases = append(releases, r)
	}
	for i := 0; i < loginSrcBudget*2; i++ {
		if w := postLogin(e, simulatedClient, httpEmailA); w.Code != http.StatusServiceUnavailable {
			t.Fatalf("shed attempt %d: status %d, want 503", i+1, w.Code)
		}
	}
	for _, r := range releases {
		r()
	}
	if got := g.src.tokensAt(srcKey, gateEpoch); got != loginSrcBudget {
		t.Errorf("source scope has %.1f tokens after only shed attempts, want %d", got, loginSrcBudget)
	}
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
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
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
			if _, r := g.Admit(context.Background(), loginAttempt{Addr: netip.MustParseAddr(simulatedClient), FromChain: true, Hops: 2, Email: email}); r != nil {
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
			g.Admit(context.Background(), loginAttempt{Addr: other, FromChain: true, Hops: 2, Email: fmt.Sprintf("t%d@example.test", i)})
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

// TestUnresolvedSourceIsNeverRefused pins the addrUnresolved rule under
// enforce: every client of a deployment whose peer address does not parse is
// on one key, so refusing on it would refuse all of them, and its pair would
// be keyed on the account alone.
func TestUnresolvedSourceIsNeverRefused(t *testing.T) {
	g, logs := newEnforceGate(t)
	a := loginAttempt{Hops: 2, Email: "someone@example.test"}
	for i := 0; i < loginSrcBudget*2; i++ {
		if _, r := g.Admit(context.Background(), a); r != nil {
			t.Fatalf("attempt %d from an unresolved source was refused (%s)", i+1, r.scope)
		}
	}
	if !strings.Contains(logs.String(), "source address is unresolved") {
		t.Errorf("a would-refuse on an unresolved source was not logged.\nlog:\n%s", logs.String())
	}
}
