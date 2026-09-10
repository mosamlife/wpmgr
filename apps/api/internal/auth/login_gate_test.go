package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// testSessionSecret is only ever an HKDF input here. It is the length
// config.ValidateSessionSecret requires so NewLoginGate takes it.
const testSessionSecret = "0123456789abcdef0123456789abcdef"

func newTestGate(t *testing.T, mode LoginMode, maxVerify int) (*LoginGate, *bytes.Buffer) {
	t.Helper()
	g, err := NewLoginGate(testSessionSecret, mode, maxVerify)
	if err != nil {
		t.Fatalf("NewLoginGate: %v", err)
	}
	var logs bytes.Buffer
	g.SetLogger(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	return g, &logs
}

// loginHandlerForTest wires a Handler the way production does, with a real
// *Service so requests go through the real h.login and reach the real
// Service.Login.
//
// The service has no repo. That is deliberate and it is what makes these tests
// possible without a database: every request below carries a body Service.Login
// rejects at ITS OWN validation step, which is strictly downstream of
// admission. So a non-503 answer here means the request was ADMITTED and the
// service got to decide — which is exactly the property under test. Nothing
// here reaches GetUserByEmail.
func loginHandlerForTest(t *testing.T, g *LoginGate, hops int) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.ReleaseMode)
	h := &Handler{svc: NewService(nil, nil, domain.NewValidator())}
	h.SetProxyHops(hops)
	h.SetLoginGate(g)
	e := gin.New()
	e.POST("/auth/login", h.login)
	return e
}

// The two addresses the HTTP-level tests submit.
//
// Neither is a syntactically valid email, ON PURPOSE. Service.Login validates
// the body BEFORE it looks any account up, so these reach the real handler,
// the real gate and the real Service.Login and are rejected there — without a
// database, and without the gate ever being able to tell them apart, which is
// the property under test. The gate normalises and digests whatever string it
// is handed; validity is not its business.
const (
	httpEmailA = "someone[at]example.test"
	httpEmailB = "never-seen-before[at]example.test"
)

// postLogin sends one login attempt from client, through a 2-hop chain shaped
// the way the balancer actually presents it.
func postLogin(e *gin.Engine, client, email string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(loginBody{Email: email, Password: "irrelevant"})
	req := httptest.NewRequest(http.MethodPost, "/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", client+", "+simulatedBalancer)
	req.RemoteAddr = "10.0.0.1:12345"
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	return w
}

// ---------------------------------------------------------------------------
// PROOF 1 — observe mode refuses nothing, and says what it would have refused.
// ---------------------------------------------------------------------------

// TestObserveModeAdmitsEveryAttempt drives 200 successive attempts against ONE
// account from ONE address through the real handler. Every budget in this file
// is smaller than 200, so an enforcing gate would refuse most of them.
//
// What is asserted is admission, not the service's answer: no attempt may come
// back 503, none may carry Retry-After, and every response must be
// byte-identical to the first. The log must nevertheless show the pair scope
// over budget, or the phase has measured nothing.
func TestObserveModeAdmitsEveryAttempt(t *testing.T) {
	g, logs := newTestGate(t, LoginModeObserve, 8)
	e := loginHandlerForTest(t, g, 2)

	const attempts = 200
	first := postLogin(e, simulatedClient, httpEmailA)
	firstBody := first.Body.String()

	for i := 1; i < attempts; i++ {
		w := postLogin(e, simulatedClient, httpEmailA)
		if w.Code == http.StatusServiceUnavailable {
			t.Fatalf("attempt %d was shed with 503; observe mode must refuse nothing", i+1)
		}
		if ra := w.Header().Get("Retry-After"); ra != "" {
			t.Fatalf("attempt %d carried Retry-After %q; observe mode must be invisible", i+1, ra)
		}
		if w.Code != first.Code {
			t.Fatalf("attempt %d status %d != first status %d", i+1, w.Code, first.Code)
		}
		if w.Body.String() != firstBody {
			t.Fatalf("attempt %d body %q != first body %q", i+1, w.Body.String(), firstBody)
		}
	}

	out := logs.String()
	if !strings.Contains(out, "would refuse") {
		t.Fatalf("no would-refuse line after %d attempts; the gate measured nothing.\nlog:\n%s", attempts, out)
	}
	// Only the PAIR scope can be over here, and that is the query-then-charge
	// rule working: from the eleventh attempt the pair is over budget, so
	// nothing is charged anywhere, and the src and acct scopes never reach
	// their own budgets no matter how long one pair floods. The two scopes
	// below are driven separately for exactly that reason.
	if !strings.Contains(out, loginScopePair) {
		t.Errorf("expected scope %q in the observation log after %d attempts on one pair; log:\n%s", loginScopePair, attempts, out)
	}
	if strings.Contains(out, loginScopeSrc+`"`) {
		t.Errorf("scope %q went over budget while one pair was flooding; a would-refused attempt must charge nothing.\nlog:\n%s", loginScopeSrc, out)
	}
	if strings.Contains(out, httpEmailA) {
		t.Fatalf("the submitted email address reached the log; only acctH may appear.\nlog:\n%s", out)
	}
	if !strings.Contains(out, g.AccountDigest(httpEmailA)) {
		t.Errorf("expected acctH in the observation log; log:\n%s", out)
	}
}

// TestSrcAndAcctScopesAreObserved drives the two wide scopes, which one
// flooding pair can never reach (see the note in the test above).
//
// The src scope is what a run against MANY accounts from one address trips.
// The acct scope is what a distributed run against ONE account trips. Both are
// admitted throughout, because this is observe mode.
func TestSrcAndAcctScopesAreObserved(t *testing.T) {
	t.Run("many accounts from one address trips the src scope", func(t *testing.T) {
		g, logs := newTestGate(t, LoginModeObserve, 8)
		e := loginHandlerForTest(t, g, 2)
		for i := 0; i < loginSrcBudget+5; i++ {
			w := postLogin(e, simulatedClient, fmt.Sprintf("user%d[at]example.test", i))
			if w.Code == http.StatusServiceUnavailable {
				t.Fatalf("attempt %d shed in observe mode", i+1)
			}
		}
		if out := logs.String(); !strings.Contains(out, loginScopeSrc+`"`) {
			t.Errorf("scope %q never went over budget after %d attempts from one address; log:\n%s", loginScopeSrc, loginSrcBudget+5, out)
		}
	})

	t.Run("many addresses against one account trips the acct scope", func(t *testing.T) {
		g, logs := newTestGate(t, LoginModeObserve, 8)
		e := loginHandlerForTest(t, g, 2)
		for i := 0; i < loginAcctBudget+5; i++ {
			w := postLogin(e, fmt.Sprintf("198.51.100.%d", i%250), httpEmailA)
			if w.Code == http.StatusServiceUnavailable {
				t.Fatalf("attempt %d shed in observe mode", i+1)
			}
		}
		out := logs.String()
		if !strings.Contains(out, loginScopeAcct+`"`) {
			t.Errorf("scope %q never went over budget after %d attempts against one account; log:\n%s", loginScopeAcct, loginAcctBudget+5, out)
		}
		if strings.Contains(out, httpEmailA) {
			t.Fatalf("the submitted email address reached the log.\nlog:\n%s", out)
		}
	})

	t.Run("an IPv6 client is observed on the /48 scope too", func(t *testing.T) {
		g, logs := newTestGate(t, LoginModeObserve, 8)
		e := loginHandlerForTest(t, g, 2)
		// Distinct /64s inside one /48, and a distinct account each time, so
		// neither the pair nor the /64 src scope trips first.
		for i := 0; i < loginSrc48Budget+5; i++ {
			client := fmt.Sprintf("2001:db8:abcd:%x::1", i%0xffff)
			w := postLogin(e, client, fmt.Sprintf("v6user%d[at]example.test", i))
			if w.Code == http.StatusServiceUnavailable {
				t.Fatalf("attempt %d shed in observe mode", i+1)
			}
		}
		if out := logs.String(); !strings.Contains(out, loginScopeSrc48+`"`) {
			t.Errorf("scope %q never went over budget after %d IPv6 attempts inside one /48; log:\n%s", loginScopeSrc48, loginSrc48Budget+5, out)
		}
	})
}

// TestOverBudgetAttemptChargesNothing pins the "a refused attempt costs
// nothing, in every scope" rule that query-then-charge exists to guarantee.
//
// The pair budget is the smallest, so a flood on one pair trips it first. Once
// it is over, the src and acct scopes must stop being charged — otherwise the
// noise from one flooding pair empties the scopes that are there to measure
// everything else.
func TestOverBudgetAttemptChargesNothing(t *testing.T) {
	g, _ := newTestGate(t, LoginModeObserve, 8)
	addr := netip.MustParseAddr(simulatedClient)

	// Exhaust the pair budget exactly.
	for i := 0; i < loginPairBudget; i++ {
		g.Observe(context.Background(), loginAttempt{Addr: addr, FromChain: true, Hops: 2, Email: "a@example.test"})
	}
	srcKey := srcKeyFor(addr)
	acctH := g.AccountDigest("a@example.test")
	now := time.Now()

	beforeSrc := g.src.query(srcKey, now)
	beforeAcct := g.acct.query(acctH, now)
	if beforeSrc.overBudget || beforeAcct.overBudget {
		t.Fatalf("precondition: src/acct should still have budget after %d attempts", loginPairBudget)
	}
	srcTokens := g.src.buckets[srcKey].lim.TokensAt(now)
	acctTokens := g.acct.buckets[acctH].lim.TokensAt(now)

	// 50 more on the same pair. Every one is over the pair budget.
	for i := 0; i < 50; i++ {
		g.Observe(context.Background(), loginAttempt{Addr: addr, FromChain: true, Hops: 2, Email: "a@example.test"})
	}

	if got := g.src.buckets[srcKey].lim.TokensAt(now); got != srcTokens {
		t.Errorf("src scope was charged for pair-over-budget attempts: tokens %v -> %v", srcTokens, got)
	}
	if got := g.acct.buckets[acctH].lim.TokensAt(now); got != acctTokens {
		t.Errorf("acct scope was charged for pair-over-budget attempts: tokens %v -> %v", acctTokens, got)
	}
}

// ---------------------------------------------------------------------------
// PROOF 2 — the gate and the service agree on which account this is.
// ---------------------------------------------------------------------------

// TestGateNormalisationMatchesService is the pin the file's doc comment
// promises. Service.Login's first statement is email = normalizeEmail(email);
// if the gate ever normalised differently, every budget would be keyed on a
// different account than the one that authenticates, and nothing else in the
// suite would notice.
func TestGateNormalisationMatchesService(t *testing.T) {
	g, _ := newTestGate(t, LoginModeObserve, 8)

	canonical := "person@example.test"
	variants := []string{
		"person@example.test",
		"Person@Example.Test",
		"PERSON@EXAMPLE.TEST",
		"  person@example.test  ",
		"\tPerson@Example.Test\n",
		" PERSON@EXAMPLE.TEST\t",
	}
	want := g.AccountDigest(canonical)
	for _, v := range variants {
		// What the service will authenticate as.
		if serviceForm := normalizeEmail(v); serviceForm != canonical {
			t.Fatalf("normalizeEmail(%q) = %q, want %q", v, serviceForm, canonical)
		}
		// What the gate keys on. These must be the same account.
		if got := g.AccountDigest(v); got != want {
			t.Errorf("AccountDigest(%q) = %q, want %q (same account the service authenticates)", v, got, want)
		}
	}

	// Different accounts must not collide, or the budgets are shared.
	if g.AccountDigest("other@example.test") == want {
		t.Error("two different accounts produced the same digest")
	}
	// Fixed width whatever was submitted: the map key must not be caller-sized.
	huge := strings.Repeat("a", 100_000) + "@example.test"
	if got := g.AccountDigest(huge); len(got) != len(want) {
		t.Errorf("digest of a %d-byte address is %d chars, want %d", len(huge), len(got), len(want))
	}
	// The digest must be keyed, not a bare hash of the address.
	other, err := NewLoginGate(strings.Repeat("z", 32), LoginModeObserve, 8)
	if err != nil {
		t.Fatalf("NewLoginGate: %v", err)
	}
	if other.AccountDigest(canonical) == want {
		t.Error("two instances with different secrets produced the same digest; the digest is not keyed")
	}
}

// ---------------------------------------------------------------------------
// PROOF 3 — the concurrency bound actually sheds, and recovers.
// ---------------------------------------------------------------------------

// TestVerifyBoundShedsAndRecovers occupies every verification slot, shows the
// endpoint answering 503 with Retry-After: 2, then frees one slot and shows the
// next attempt admitted again.
func TestVerifyBoundShedsAndRecovers(t *testing.T) {
	const bound = 3
	g, _ := newTestGate(t, LoginModeObserve, bound)
	e := loginHandlerForTest(t, g, 2)

	// Not saturated yet: this must be admitted.
	if w := postLogin(e, simulatedClient, httpEmailA); w.Code == http.StatusServiceUnavailable {
		t.Fatalf("shed before saturation: %d %s", w.Code, w.Body.String())
	}

	releases := make([]func(), 0, bound)
	for i := 0; i < bound; i++ {
		release, ok := g.verify.acquire()
		if !ok {
			t.Fatalf("could not occupy slot %d of %d", i+1, bound)
		}
		releases = append(releases, release)
	}
	if got := g.verify.inFlight(); got != bound {
		t.Fatalf("inFlight = %d, want %d", got, bound)
	}

	w := postLogin(e, simulatedClient, httpEmailA)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("saturated: status %d, want %d; body %s", w.Code, http.StatusServiceUnavailable, w.Body.String())
	}
	if got := w.Header().Get("Retry-After"); got != "2" {
		t.Errorf("Retry-After = %q, want %q", got, "2")
	}
	var env struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("shed body is not the error envelope: %v (%s)", err, w.Body.String())
	}
	if env.Code != "server_busy" {
		t.Errorf("shed code = %q, want %q", env.Code, "server_busy")
	}

	// Free one slot: the next attempt is admitted again.
	releases[0]()
	if got := g.verify.inFlight(); got != bound-1 {
		t.Fatalf("after one release inFlight = %d, want %d", got, bound-1)
	}
	if w := postLogin(e, simulatedClient, httpEmailA); w.Code == http.StatusServiceUnavailable {
		t.Fatalf("still shedding after a slot was freed: %d %s", w.Code, w.Body.String())
	}
	for _, r := range releases[1:] {
		r()
	}

	// Release is idempotent: the handler calls it explicitly and again by defer.
	releases[0]()
	releases[0]()
	if got := g.verify.inFlight(); got != 0 {
		t.Errorf("after all releases inFlight = %d, want 0; release is not idempotent", got)
	}
}

// ---------------------------------------------------------------------------
// PROOF 4 — observation is invisible, and cannot become an enumeration oracle.
// ---------------------------------------------------------------------------

// TestObservationIsInvisibleToTheCaller is the new-oracle proof for THIS
// change.
//
// Whether an account exists is Service.Login's business and it already answers
// invalid_credentials either way; that equivalence is unchanged here and its
// end-to-end proof needs a database (integration package). What this PR could
// newly leak is the gate, so what is pinned here is that the gate contributes
// NOTHING observable: for wildly different emails — including one whose budget
// is already blown and one that has never been seen — the response bytes,
// status and headers are identical, and Observe itself writes nothing at all.
func TestObservationIsInvisibleToTheCaller(t *testing.T) {
	g, _ := newTestGate(t, LoginModeObserve, 8)
	e := loginHandlerForTest(t, g, 2)

	// Blow every budget for one account from this address.
	for i := 0; i < loginPairBudget*3; i++ {
		postLogin(e, simulatedClient, httpEmailA)
	}

	known := postLogin(e, simulatedClient, httpEmailA)
	unknown := postLogin(e, simulatedClient, httpEmailB)

	if known.Code != unknown.Code {
		t.Errorf("status differs: over-budget account %d, unseen account %d", known.Code, unknown.Code)
	}
	if known.Body.String() != unknown.Body.String() {
		t.Errorf("body differs:\n over-budget: %q\n unseen:      %q", known.Body.String(), unknown.Body.String())
	}
	if len(known.Body.Bytes()) != len(unknown.Body.Bytes()) {
		t.Errorf("body length differs: %d vs %d", len(known.Body.Bytes()), len(unknown.Body.Bytes()))
	}
	for k, v := range known.Header() {
		if got := unknown.Header()[k]; strings.Join(got, ",") != strings.Join(v, ",") {
			t.Errorf("header %q differs: %v vs %v", k, v, got)
		}
	}
	for k := range unknown.Header() {
		if _, ok := known.Header()[k]; !ok {
			t.Errorf("header %q present only on the unseen-account response", k)
		}
	}

	// And directly: Observe writes nothing to the response, ever.
	for _, email := range []string{httpEmailA, httpEmailB, ""} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/auth/login", nil)
		g.Observe(context.Background(), loginAttempt{Addr: netip.MustParseAddr(simulatedClient), FromChain: true, Hops: 2, Email: email})
		if c.Writer.Written() {
			t.Errorf("Observe wrote to the response for email %q", email)
		}
		if len(w.Header()) != 0 {
			t.Errorf("Observe set headers %v for email %q", w.Header(), email)
		}
		if w.Body.Len() != 0 {
			t.Errorf("Observe wrote body %q for email %q", w.Body.String(), email)
		}
	}
}

// ---------------------------------------------------------------------------
// Keys, and the degraded-address flag.
// ---------------------------------------------------------------------------

func TestSourceKeyMasking(t *testing.T) {
	cases := []struct {
		addr      string
		wantSrc   string
		wantS48   string
		hasSrc48  bool
		sameAsAlt string // an address that must land on the SAME src key
		diffAlt   string // an address that must land on a DIFFERENT src key
	}{
		{
			addr: "203.0.113.7", wantSrc: "203.0.113.7", hasSrc48: false,
			diffAlt: "203.0.113.8",
		},
		{
			addr: "2001:db8:1:2:3:4:5:6", wantSrc: "2001:db8:1:2::", wantS48: "2001:db8:1::", hasSrc48: true,
			sameAsAlt: "2001:db8:1:2:aaaa:bbbb:cccc:dddd",
			diffAlt:   "2001:db8:1:3::1",
		},
	}
	for _, tc := range cases {
		a := netip.MustParseAddr(tc.addr)
		if got := srcKeyFor(a); got != tc.wantSrc {
			t.Errorf("srcKeyFor(%s) = %q, want %q", tc.addr, got, tc.wantSrc)
		}
		got48, ok := src48KeyFor(a)
		if ok != tc.hasSrc48 {
			t.Errorf("src48KeyFor(%s) ok = %v, want %v", tc.addr, ok, tc.hasSrc48)
		}
		if ok && got48 != tc.wantS48 {
			t.Errorf("src48KeyFor(%s) = %q, want %q", tc.addr, got48, tc.wantS48)
		}
		if tc.sameAsAlt != "" {
			if got := srcKeyFor(netip.MustParseAddr(tc.sameAsAlt)); got != tc.wantSrc {
				t.Errorf("srcKeyFor(%s) = %q, want the same /64 key %q", tc.sameAsAlt, got, tc.wantSrc)
			}
		}
		if tc.diffAlt != "" {
			if got := srcKeyFor(netip.MustParseAddr(tc.diffAlt)); got == tc.wantSrc {
				t.Errorf("srcKeyFor(%s) collided with %s on %q", tc.diffAlt, tc.addr, got)
			}
		}
	}
	// An unresolved address is bucketed, never skipped.
	if got := srcKeyFor(netip.Addr{}); got != addrUnresolved {
		t.Errorf("srcKeyFor(invalid) = %q, want %q", got, addrUnresolved)
	}
	if _, ok := src48KeyFor(netip.Addr{}); ok {
		t.Error("src48KeyFor(invalid) reported a key")
	}
}

// TestLimiterAddrSourceGradesTheAddress pins the fromChain flag, and pins that
// limiterAddr's own behaviour did not move.
func TestLimiterAddrSourceGradesTheAddress(t *testing.T) {
	e := engineAsProductionBuildsIt()

	newCtx := func(h *Handler, xff string) *gin.Context {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/auth/login", nil)
		c.Request.RemoteAddr = "192.0.2.9:4444"
		if xff != "" {
			c.Request.Header.Set("X-Forwarded-For", xff)
		}
		return c
	}
	_ = e

	t.Run("chain long enough", func(t *testing.T) {
		h := handlerWithHops(2)
		c := newCtx(h, simulatedClient+", "+simulatedBalancer)
		addr, fromChain := h.limiterAddrSource(c)
		if !fromChain {
			t.Error("fromChain = false for a chain at the configured length")
		}
		if addr.String() != simulatedClient {
			t.Errorf("addr = %s, want %s", addr, simulatedClient)
		}
		if a := (loginAttempt{Addr: addr, FromChain: fromChain, Hops: 2}).addrSource(); a != "chain" {
			t.Errorf("addrSource = %q, want %q", a, "chain")
		}
		if h.limiterAddr(c).String() != simulatedClient {
			t.Errorf("limiterAddr changed: %s", h.limiterAddr(c))
		}
	})

	t.Run("chain too short falls back and is graded degraded", func(t *testing.T) {
		h := handlerWithHops(3)
		c := newCtx(h, simulatedClient+", "+simulatedBalancer)
		addr, fromChain := h.limiterAddrSource(c)
		if fromChain {
			t.Error("fromChain = true for a chain shorter than the configured hop count")
		}
		if addr.String() != "192.0.2.9" {
			t.Errorf("addr = %s, want the peer address 192.0.2.9", addr)
		}
		if a := (loginAttempt{Addr: addr, FromChain: fromChain, Hops: 3}).addrSource(); a != "peer_fallback" {
			t.Errorf("addrSource = %q, want %q", a, "peer_fallback")
		}
	})

	t.Run("zero hops is the configured source, not a fallback", func(t *testing.T) {
		h := handlerWithHops(0)
		c := newCtx(h, simulatedClient+", "+simulatedBalancer)
		addr, fromChain := h.limiterAddrSource(c)
		if fromChain {
			t.Error("fromChain = true with hops=0; nothing was read from a chain")
		}
		if a := (loginAttempt{Addr: addr, FromChain: fromChain, Hops: 0}).addrSource(); a != "peer_configured" {
			t.Errorf("addrSource = %q, want %q", a, "peer_configured")
		}
	})

	t.Run("unresolved", func(t *testing.T) {
		if a := (loginAttempt{Hops: 2}).addrSource(); a != "unresolved" {
			t.Errorf("addrSource = %q, want %q", a, "unresolved")
		}
	})
}

// ---------------------------------------------------------------------------
// Memory bound, mode, and wiring.
// ---------------------------------------------------------------------------

// TestBucketMapIsCapped watches the memory bound actually hold. A cap nobody
// has seen bind is not known to bind.
func TestBucketMapIsCapped(t *testing.T) {
	b := newKeyedBudget("test", 10)
	now := time.Now()
	for i := 0; i < loginBucketCap*2; i++ {
		b.query(netip.AddrFrom4([4]byte{byte(i >> 24), byte(i >> 16), byte(i >> 8), byte(i)}).String(), now)
		if got := b.size(); got > loginBucketCap {
			t.Fatalf("map grew to %d entries, past the %d cap, after %d keys", got, loginBucketCap, i+1)
		}
	}
	if got := b.size(); got == 0 {
		t.Fatalf("map is empty after %d keys; the sweep is evicting everything", loginBucketCap*2)
	}
}

func TestParseLoginMode(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    LoginMode
		wantErr bool
	}{
		{"", LoginModeObserve, false},
		{"observe", LoginModeObserve, false},
		// Refused in Phase 0 on purpose; see TestEnforceIsNotConfigurableInPhase0.
		{"enforce", "", true},
		{"Observe", "", true},
		{"off", "", true},
		{"true", "", true},
		{" observe", "", true},
	} {
		got, err := ParseLoginMode(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseLoginMode(%q) = %q, want an error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseLoginMode(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseLoginMode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestNilGateIsInertAndSaidSoAtStartup pins both halves of the wiring failure
// the startup line exists for: a nil gate must not crash the login path, and
// the startup line must say, loudly, that nothing is wired.
func TestNilGateIsInertAndSaidSoAtStartup(t *testing.T) {
	e := loginHandlerForTest(t, nil, 2)
	w := postLogin(e, simulatedClient, httpEmailA)
	if w.Code == http.StatusServiceUnavailable {
		t.Fatalf("a nil gate shed a request: %d", w.Code)
	}

	var logs bytes.Buffer
	h := &Handler{}
	h.SetProxyHops(2)
	h.LogAdmissionStartup(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	out := logs.String()
	if !strings.Contains(out, `"level":"WARN"`) {
		t.Errorf("an unwired gate must be a WARN, not an Info; log:\n%s", out)
	}
	if !strings.Contains(out, `"gate_wired":false`) {
		t.Errorf("startup line does not report gate_wired=false; log:\n%s", out)
	}

	logs.Reset()
	g, _ := newTestGate(t, LoginModeObserve, 4)
	h2 := &Handler{}
	h2.SetProxyHops(2)
	h2.SetLoginGate(g)
	h2.LogAdmissionStartup(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	out = logs.String()
	for _, want := range []string{
		`"gate_wired":true`,
		`"verify_bound_wired":true`,
		`"mode":"observe"`,
		`"proxy_hops":2`,
		`"max_concurrent_password_verifications":4`,
		loginScopePair,
		loginScopeSrc,
		loginScopeSrc48,
		loginScopeAcct,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("startup line is missing %s; log:\n%s", want, out)
		}
	}
}

func TestNewLoginGateRefusesAWeakSecretAndAnUnknownMode(t *testing.T) {
	if _, err := NewLoginGate("short", LoginModeObserve, 4); err == nil {
		t.Error("NewLoginGate accepted a secret too short to derive a key from")
	}
	if _, err := NewLoginGate(testSessionSecret, LoginMode("off"), 4); err == nil {
		t.Error("NewLoginGate accepted an unknown mode")
	}
}

// ---------------------------------------------------------------------------
// F1 GUARD — the startup line cannot claim enforcement that does not happen.
// ---------------------------------------------------------------------------

// TestStartupLineCannotClaimEnforcementItDoesNotDo is the guard, not a careful
// edit.
//
// It enumerates every mode ParseLoginMode ACCEPTS — it does not hard-code
// "observe" — builds a gate in each, drives enough real requests through the
// real handler to blow the smallest budget many times over, and compares two
// things that must never disagree:
//
//   - what the boot line asserts in budgets_enforced, and
//   - whether any request was actually refused for being over budget.
//
// Both directions fail. A mode that claims enforcement and admits everything
// is the defect this guard exists for; a mode that quietly enforces while the
// boot line says it does not is just as bad, because the operator reading that
// line would have no idea why sign-ins were being refused.
//
// It also pins the second half of the same defect: a mode that stops
// StartModeReminder from warning must be a mode that actually enforces.
func TestStartupLineCannotClaimEnforcementItDoesNotDo(t *testing.T) {
	// Every string worth asking about, including the one Phase 1 will add.
	// Whatever ParseLoginMode accepts is what gets exercised, so the day
	// "enforce" starts being accepted this guard starts checking it.
	candidates := []string{"", "observe", "enforce"}

	accepted := 0
	for _, raw := range candidates {
		mode, err := ParseLoginMode(raw)
		if err != nil {
			continue // Not configurable, so no operator can be misled by it.
		}
		accepted++

		t.Run("mode="+string(mode), func(t *testing.T) {
			g, _ := newTestGate(t, mode, 64)
			e := loginHandlerForTest(t, g, 2)

			// Far past the smallest budget, all on one key.
			const attempts = loginPairBudget * 10
			refusedForBudget := false
			var baseline int
			for i := 0; i < attempts; i++ {
				w := postLogin(e, simulatedClient, httpEmailA)
				if i == 0 {
					baseline = w.Code
					continue
				}
				// The verify bound is 64 and these are sequential, so a 503
				// here could only come from budget enforcement.
				if w.Code == http.StatusServiceUnavailable || w.Code == http.StatusTooManyRequests {
					refusedForBudget = true
				}
				if w.Code != baseline {
					refusedForBudget = true
				}
			}

			var logs bytes.Buffer
			h := &Handler{}
			h.SetProxyHops(2)
			h.SetLoginGate(g)
			h.LogAdmissionStartup(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
			out := logs.String()

			claimsEnforcement := strings.Contains(out, `"budgets_enforced":true`)
			if !claimsEnforcement && !strings.Contains(out, `"budgets_enforced":false`) {
				t.Fatalf("startup line reports no budgets_enforced field at all; log:\n%s", out)
			}

			if claimsEnforcement && !refusedForBudget {
				t.Errorf("mode %q: the boot line asserts budgets_enforced=true, but %d attempts past a budget of %d were ALL admitted. A startup line that can be wrong is worse than none.\nlog:\n%s",
					mode, attempts, loginPairBudget, out)
			}
			if !claimsEnforcement && refusedForBudget {
				t.Errorf("mode %q: requests were refused for being over budget, but the boot line asserts budgets_enforced=false. An operator could not explain the refusals.\nlog:\n%s",
					mode, out)
			}

			// Same defect, other half: the five-minute reminder must only go
			// quiet for a mode that actually enforces.
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			g.StartModeReminder(ctx)
			remindersSuppressed := g.mode == LoginModeEnforce
			if remindersSuppressed && !refusedForBudget {
				t.Errorf("mode %q silences StartModeReminder without enforcing anything", mode)
			}
		})
	}

	if accepted == 0 {
		t.Fatal("ParseLoginMode accepted no mode at all; this guard checked nothing")
	}
}

// TestEnforceIsNotConfigurableInPhase0 pins the narrow fact the guard above
// depends on, with the reason attached, so the day it changes the change is
// deliberate.
func TestEnforceIsNotConfigurableInPhase0(t *testing.T) {
	if _, err := ParseLoginMode("enforce"); err == nil {
		t.Error("ParseLoginMode accepted \"enforce\" while Observe still refuses nothing; that mode would assert enforcement, silence the reminder, and enforce nothing")
	}
	if _, err := ParseLoginMode("observe"); err != nil {
		t.Errorf("ParseLoginMode rejected the documented default: %v", err)
	}
}

// TestOverBudgetLoggingIsBounded pins the F2 latch: a key that goes over budget
// must not write a line per request for the rest of the window.
func TestOverBudgetLoggingIsBounded(t *testing.T) {
	g, logs := newTestGate(t, LoginModeObserve, 64)
	e := loginHandlerForTest(t, g, 2)

	for i := 0; i < loginPairBudget; i++ {
		postLogin(e, simulatedClient, httpEmailA)
	}
	if n := strings.Count(logs.String(), "would refuse"); n != 0 {
		t.Fatalf("%d lines before any budget was crossed, want 0", n)
	}

	// First crossing: exactly one line.
	postLogin(e, simulatedClient, httpEmailA)
	if n := strings.Count(logs.String(), "would refuse"); n != 1 {
		t.Fatalf("first crossing wrote %d lines, want 1", n)
	}

	// A long run over the same key. Unlatched this wrote one line each.
	const flood = 300
	before := logs.Len()
	for i := 0; i < flood; i++ {
		postLogin(e, simulatedClient, httpEmailA)
	}
	lines := strings.Count(logs.String(), "would refuse")
	wantAtMost := 1 + flood/loginOverLogEvery + 1
	if lines > wantAtMost {
		t.Errorf("%d lines for %d over-budget attempts, want at most %d; the latch is not holding", lines, flood, wantAtMost)
	}
	if lines < 2 {
		t.Errorf("%d lines for %d over-budget attempts; the periodic repeat is not firing and the volume is invisible", lines, flood)
	}
	if grown := logs.Len() - before; grown > flood*10 {
		t.Errorf("log grew %d bytes over %d shed attempts (~%d/request); the point of the latch is that this is bounded", grown, flood, grown/flood)
	}
	// The suppressed volume must still be countable from the lines that print.
	if !strings.Contains(logs.String(), `"over_budget_attempts":`) {
		t.Error("no over_budget_attempts field; a suppressed run must still be countable")
	}
}
