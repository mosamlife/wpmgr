// register_limit_test.go: proofs for the anonymous-registration defence and
// for the routing shape of the four OAuth endpoints.
//
// POST /oauth/mcp/register is unauthenticated by RFC 7591. Mounting it is a
// deliberate security decision, and these are the proofs that the decision came
// with a bound rather than with a comment claiming one.
package mcp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// ---------------------------------------------------------------------------
// THE LIMITER ITSELF.
// ---------------------------------------------------------------------------

// The global bucket is the security bound, so it must actually refuse. Spends
// the whole per-minute budget from DIFFERENT peers -- source diversity is
// exactly what a real attacker has and what the per-peer layer cannot bound.
func TestRegisterLimiter_GlobalCapRefusesRegardlessOfSourceDiversity(t *testing.T) {
	const global = 5
	l := newRegistrationLimiter(global, 1000) // per-peer set high so it cannot be what refuses

	for i := range global {
		peer := "10.0.0." + itoa(i)
		if ok, _ := l.allow(peer); !ok {
			t.Fatalf("request %d from a fresh peer %q was refused while the global "+
				"budget of %d was not yet spent", i, peer, global)
		}
	}

	// One more, from a peer that has never been seen. Only the global bucket can
	// refuse this.
	ok, retryAfter := l.allow("10.0.0.99")
	if ok {
		t.Fatalf("the %dth registration was ALLOWED after a global budget of %d was "+
			"spent; a limiter that only bounds per-peer bounds nothing against a "+
			"caller that varies its source address", global+1, global)
	}
	if retryAfter <= 0 {
		t.Fatalf("refused with retryAfter=%v; a 429 with no wait tells a client "+
			"nothing and invites an immediate retry loop", retryAfter)
	}
}

// The per-peer bucket must refuse a single noisy source before the global
// budget is anywhere near spent -- that is the fairness property it exists for.
func TestRegisterLimiter_PerPeerCapRefusesOneNoisySource(t *testing.T) {
	const perPeer = 3
	l := newRegistrationLimiter(1000, perPeer) // global set high so it cannot be what refuses

	for i := range perPeer {
		if ok, _ := l.allow("10.0.0.1"); !ok {
			t.Fatalf("request %d from one peer was refused inside its budget of %d", i, perPeer)
		}
	}
	if ok, _ := l.allow("10.0.0.1"); ok {
		t.Fatalf("the %dth request from one peer was allowed past a per-peer budget of %d", perPeer+1, perPeer)
	}
	// A DIFFERENT peer must still get through: a fairness layer that starves
	// everyone once one source misbehaves is a denial of service, not a defence.
	if ok, _ := l.allow("10.0.0.2"); !ok {
		t.Fatalf("a second, quiet peer was refused because a first peer exhausted " +
			"its own bucket; the per-peer layer is not per-peer")
	}
}

// A rejected request must not spend the caller's own fair-share budget.
func TestRegisterLimiter_GlobalRefusalDoesNotChargeThePeer(t *testing.T) {
	l := newRegistrationLimiter(1, 10)

	if ok, _ := l.allow("10.0.0.1"); !ok {
		t.Fatal("the first request was refused with a global budget of 1")
	}
	// Global is now empty; this refusal must not consume 10.0.0.2's tokens.
	if ok, _ := l.allow("10.0.0.2"); ok {
		t.Fatal("global budget of 1 allowed a second request")
	}

	b, ok := l.peers["10.0.0.2"]
	if ok && b.lim.Tokens() < 10 {
		t.Fatalf("a globally-refused request charged the peer's bucket: %v tokens "+
			"left of 10; repeated global pressure would silently exhaust every "+
			"innocent caller's budget", b.lim.Tokens())
	}
}

// A nil limiter must refuse. This is the defect class this whole slice is most
// exposed to: an absent guard coerced into "no limit configured, therefore
// permitted".
func TestRegisterLimiter_NilRefusesRatherThanAllows(t *testing.T) {
	var l *registrationLimiter
	if ok, _ := l.allow("10.0.0.1"); ok {
		t.Fatal("a nil limiter ALLOWED the request; an unwired limiter must refuse, " +
			"never read as absence of a limit")
	}
}

// A non-positive budget is a misconfiguration, and it must fail closed.
func TestRegisterLimiter_ZeroBudgetRefusesEverything(t *testing.T) {
	for _, tc := range []struct{ global, perPeer int }{{0, 10}, {10, 0}, {-1, -1}} {
		l := newRegistrationLimiter(tc.global, tc.perPeer)
		if ok, _ := l.allow("10.0.0.1"); ok {
			t.Fatalf("global=%d perPeer=%d allowed a request; a zero or negative "+
				"budget must mean 'allow nothing', never 'no limit'", tc.global, tc.perPeer)
		}
	}
}

// An unparseable source must be MORE restrictive than a parseable one, not
// exempt from the limit.
func TestRegisterLimiter_UnknownPeerIsBucketedNotSkipped(t *testing.T) {
	l := newRegistrationLimiter(1000, 2)
	for i := range 2 {
		if ok, _ := l.allow(""); !ok {
			t.Fatalf("request %d with an unknown source was refused inside the budget", i)
		}
	}
	if ok, _ := l.allow(""); ok {
		t.Fatal("an unknown source was allowed past the per-peer budget; requests " +
			"whose origin cannot be identified would be unlimited")
	}
}

// The peer map must not grow without bound.
func TestRegisterLimiter_PeerMapIsBounded(t *testing.T) {
	l := newRegistrationLimiter(1_000_000, 1000)
	for i := range registerPeerCap * 2 {
		l.allow("10.0." + itoa(i/256) + "." + itoa(i%256))
	}
	if len(l.peers) > registerPeerCap {
		t.Fatalf("peer map holds %d entries with a cap of %d; an attacker varying "+
			"source addresses would grow it until the process died",
			len(l.peers), registerPeerCap)
	}
}

// ---------------------------------------------------------------------------
// WHY THE KEY IS RemoteIP. This is the red half, executed rather than asserted.
//
// gin's engine here trusts every proxy (defaultTrustedCIDRs is 0.0.0.0/0 + ::/0
// and SetTrustedProxies is never called in this repository), so ClientIP
// returns the leftmost X-Forwarded-For entry -- a caller-supplied string. This
// test drives the REAL mounted endpoint and demonstrates both halves: keying on
// ClientIP would have been bypassable per request, and the shipped code, keyed
// on RemoteIP, refuses.
// ---------------------------------------------------------------------------

func TestRegisterLimiter_ClientIPWouldHaveBeenSpoofableAndRemoteIPIsNot(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// RED: the key that was NOT chosen. Every request varies X-Forwarded-For and
	// every request lands in a different bucket, so a per-source limit keyed here
	// never refuses anything.
	seenClientIPs := map[string]bool{}
	seenRemoteIPs := map[string]bool{}
	r := gin.New()
	r.POST("/probe", func(c *gin.Context) {
		seenClientIPs[c.ClientIP()] = true
		seenRemoteIPs[c.RemoteIP()] = true
		c.Status(http.StatusOK)
	})

	const attempts = 20
	for i := range attempts {
		req := httptest.NewRequest(http.MethodPost, "/probe", strings.NewReader("{}"))
		req.RemoteAddr = "203.0.113.7:5555" // ONE real TCP peer for every request
		req.Header.Set("X-Forwarded-For", "198.51.100."+itoa(i))
		r.ServeHTTP(httptest.NewRecorder(), req)
	}

	if len(seenClientIPs) != attempts {
		t.Fatalf("ClientIP() yielded %d distinct values across %d requests from ONE "+
			"TCP peer; this test's premise is that it yields %d, and if gin's "+
			"trusted-proxy configuration has changed then register_limit.go's "+
			"reasoning must be re-read, not this number adjusted",
			len(seenClientIPs), attempts, attempts)
	}
	if len(seenRemoteIPs) != 1 {
		t.Fatalf("RemoteIP() yielded %d distinct values across %d requests from ONE "+
			"TCP peer; the limiter key is supposed to be unspoofable",
			len(seenRemoteIPs), attempts)
	}

	// GREEN: the shipped handler, keyed on RemoteIP, refuses the same traffic.
	h := &Handler{svc: NewService(&fakeStore{registerRows: 1}),
		regLimit: newRegistrationLimiter(1000, 3)}
	eng := gin.New()
	h.RegisterPublic(eng.Group("/api/v1"))

	var refused int
	for i := range attempts {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/oauth/mcp/register",
			strings.NewReader(`{"redirect_uris":["https://example.test/cb"],`+
				`"token_endpoint_auth_method":"none"}`))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "203.0.113.7:5555"
		req.Header.Set("X-Forwarded-For", "198.51.100."+itoa(i))
		w := httptest.NewRecorder()
		eng.ServeHTTP(w, req)
		if w.Code == http.StatusTooManyRequests {
			refused++
		}
	}
	if refused == 0 {
		t.Fatalf("%d registrations from one TCP peer, each with a different "+
			"X-Forwarded-For, were ALL accepted; the limiter is keyed on a "+
			"caller-controlled header and enforces nothing", attempts)
	}
	t.Logf("one TCP peer, %d distinct X-Forwarded-For values, per-peer budget 3: "+
		"%d refused with 429", attempts, refused)
}

// ---------------------------------------------------------------------------
// THE MOUNTED ROUTES.
// ---------------------------------------------------------------------------

// A 429 must carry Retry-After, and must be a 429 rather than a 400.
func TestRegister_RefusalIs429WithRetryAfter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &Handler{svc: NewService(&fakeStore{registerRows: 1}),
		regLimit: newRegistrationLimiter(1, 1)}
	eng := gin.New()
	h.RegisterPublic(eng.Group("/api/v1"))

	body := `{"redirect_uris":["https://example.test/cb"],"token_endpoint_auth_method":"none"}`
	newReq := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/oauth/mcp/register",
			strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "203.0.113.7:5555"
		return req
	}

	eng.ServeHTTP(httptest.NewRecorder(), newReq()) // spends the budget

	w := httptest.NewRecorder()
	eng.ServeHTTP(w, newReq())
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("a rate-limited registration answered %d, want 429", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("a 429 with no Retry-After header")
	}
}

// The body cap must engage before the decoder reads an unbounded stream.
func TestRegister_OversizeBodyIsRefused(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &Handler{svc: NewService(&fakeStore{registerRows: 1}),
		regLimit: newRegistrationLimiter(1000, 1000)}
	eng := gin.New()
	h.RegisterPublic(eng.Group("/api/v1"))

	huge := `{"client_name":"` + strings.Repeat("A", maxOAuthBodyBytes*2) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/oauth/mcp/register",
		strings.NewReader(huge))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "203.0.113.7:5555"
	w := httptest.NewRecorder()
	eng.ServeHTTP(w, req)

	if w.Code == http.StatusCreated {
		t.Fatalf("a %d-byte body was accepted past a %d-byte cap",
			len(huge), maxOAuthBodyBytes)
	}
}

// EVERY VERB ON EVERY OAUTH PATH MUST ANSWER 405, NEVER 404.
//
// A 404 on a published endpoint reads as "not deployed" and sends an operator
// to check the deploy instead of their request. That was exactly the S6b-2
// blocker, on the transport path, so it is proven here per verb per route
// rather than assumed to have been fixed once.
func TestOAuthRoutes_UnsupportedVerbsAre405Not404(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &Handler{svc: NewService(&fakeStore{registerRows: 1}),
		regLimit: newRegistrationLimiter(1000, 1000)}
	eng := gin.New()
	pub := eng.Group("/api/v1")
	h.RegisterPublic(pub)
	h.Register(pub) // same group: this test is about routing, not about auth

	paths := map[string]string{
		"/api/v1/oauth/mcp/register":  http.MethodPost,
		"/api/v1/oauth/mcp/token":     http.MethodPost,
		"/api/v1/oauth/mcp/authorize": http.MethodGet,
		"/api/v1/oauth/mcp/consent":   http.MethodPost,
	}

	for path, supported := range paths {
		for _, verb := range allVerbs {
			if verb == supported {
				continue
			}
			req := httptest.NewRequest(verb, path, nil)
			req.RemoteAddr = "203.0.113.7:5555"
			w := httptest.NewRecorder()
			eng.ServeHTTP(w, req)

			if w.Code == http.StatusNotFound {
				t.Errorf("%s %s answered 404; an operator reads that as 'not "+
					"deployed' rather than 'wrong verb'", verb, path)
				continue
			}
			if w.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s answered %d, want 405", verb, path, w.Code)
				continue
			}
			if got := w.Header().Get("Allow"); got != supported {
				t.Errorf("%s %s: Allow=%q, want %q", verb, path, got, supported)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// A REFUSED REQUEST MUST COST NOTHING, IN EITHER BUCKET.
//
// The accounting bug this exists for: allow() took and KEPT a global
// reservation before consulting the peer bucket, so a peer that had already
// exhausted its own budget still spent process-wide capacity on every request
// it was REFUSED for. That inverts the whole design -- the fairness layer
// becomes the thing that lets one peer deny everyone else, and an attacker does
// not have to beat the limiter, they beat it by being rejected, which is free
// and fast.
//
// A SINGLE-PEER TEST CANNOT SEE THIS, which is why nothing caught it: the
// flooding peer is refused either way, so only a SECOND, unrelated peer reveals
// where the tokens went.
// ---------------------------------------------------------------------------

func TestRegisterLimiter_OnePeerFloodingDoesNotStarveAnother(t *testing.T) {
	const (
		global  = 60
		perPeer = 5
		flood   = 300 // far more than global, so a leak drains it many times over
	)
	l := newRegistrationLimiter(global, perPeer)

	// The quiet peer goes first so it cannot be accused of arriving too late.
	if ok, _ := l.allow("198.51.100.2"); !ok {
		t.Fatal("the quiet peer was refused on its very first request")
	}

	// The flooding peer exhausts its own bucket and then keeps going. Every
	// request past the first perPeer is refused, and each refusal must cost
	// nothing anywhere.
	var floodAdmitted int
	for range flood {
		if ok, _ := l.allow("198.51.100.1"); ok {
			floodAdmitted++
		}
	}
	if floodAdmitted > perPeer {
		t.Fatalf("the flooding peer was admitted %d times against a per-peer "+
			"budget of %d", floodAdmitted, perPeer)
	}

	// THE ASSERTION. The quiet peer is well inside its own budget, and the
	// global budget was never legitimately spent, so every one of these must be
	// admitted. Under the defect they are all refused.
	for i := range perPeer - 1 {
		ok, wait := l.allow("198.51.100.2")
		if !ok {
			t.Fatalf("request %d from a QUIET peer was refused (retry in %v) after "+
				"another peer was rejected %d times. Only %d requests were ever "+
				"admitted out of a global budget of %d, so the REJECTIONS consumed "+
				"the global capacity: one peer can deny registration to everyone "+
				"else by being refused quickly, which costs it nothing",
				i+1, wait, flood-floodAdmitted, floodAdmitted+1, global)
		}
	}

	// And the books balance: global is charged once per ADMITTED request and
	// never for a refusal.
	admitted := float64(floodAdmitted + perPeer)
	left := l.global.TokensAt(time.Now())
	if left < float64(global)-admitted-0.5 {
		t.Fatalf("global bucket has %.1f tokens left; %.0f admitted requests should "+
			"leave about %.1f. The difference was spent on refusals",
			left, admitted, float64(global)-admitted)
	}
	t.Logf("one peer refused %d times, a quiet peer admitted throughout; global "+
		"tokens left %.1f of %d after %.0f admitted requests",
		flood-floodAdmitted, left, global, admitted)
}

// itoa avoids pulling strconv into this file's namespace for a loop counter.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}
