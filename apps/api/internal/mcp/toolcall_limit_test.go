package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

// ---------------------------------------------------------------------------
// Harness for the tool-call budget.
//
// Every proof below drives the REAL MOUNT: TransportHandler.Register is called
// with the same argument shape server.go passes it, and requests arrive as HTTP
// through gin. The limiter is only reachable that way, so a test that called
// toolCallLimiter.allow directly would prove the bucket arithmetic and nothing
// about whether POST /mcp is actually gated.
//
// WHY THERE IS NO wpmgr_app PROOF IN THIS FILE, stated rather than left as an
// omission for a reader to trip over: the tool-call limiter holds no database
// state and issues no query. It refuses BEFORE Service.RecordActivity, which is
// the first statement the tools/* path would otherwise run, and PROOF 6 asserts
// exactly that no write is attempted. There is therefore no policy for a
// database role to be subject to, and connecting as wpmgr_app here would prove
// nothing that this file does not already prove in process. The RLS-backed
// proofs belong to the per-tenant enablement state, which does not exist yet --
// see the report accompanying this change.
// ---------------------------------------------------------------------------

// limiterRouter builds the real router over a store, and hands back the handler
// so a test can reach the limiter it was constructed with.
func limiterRouter(t *testing.T, store Store) (*gin.Engine, *TransportHandler) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewTransportHandler(NewService(store), slog.New(slog.DiscardHandler), "test-version")
	h.Register(r)
	return r, h
}

// twoTenantStore resolves a DIFFERENT tenant and grant per bearer token, so one
// router and one limiter can serve two organisations. Tenant isolation is a
// claim about two tenants sharing a process; a test using two separate routers
// would have two separate limiters and could not observe a leak between them.
type twoTenantStore struct {
	*fakeStore
	tenants map[string]uuid.UUID
	grants  map[string]uuid.UUID
	// current is the bearer the next request will carry. LookupConnectionToken
	// receives only a hash and cannot reverse it, so steering by an explicit
	// field keeps the fake honest about what it can actually see.
	current string
	// rotateGrants makes every request resolve to a FRESH grant id, modelling
	// one organisation holding many connections. Without it a tenant can never
	// consume more than one grant's fairness share and its tenant-wide bound is
	// unreachable, which makes a tenant-isolation proof vacuous.
	rotateGrants bool
}

func newTwoTenantStore(bearers ...string) *twoTenantStore {
	s := &twoTenantStore{
		fakeStore: liveGrantStore(uuid.New()),
		tenants:   map[string]uuid.UUID{},
		grants:    map[string]uuid.UUID{},
	}
	for _, b := range bearers {
		s.tenants[b] = uuid.New()
		s.grants[b] = uuid.New()
	}
	return s
}

// forBearer selects which organisation the next request resolves to.
func (s *twoTenantStore) forBearer(b string) {
	s.current = b
}

func (s *twoTenantStore) LookupConnectionToken(_ context.Context, _ string) (sqlc.GetMCPConnectionTokenByHashForLookupRow, error) {
	return sqlc.GetMCPConnectionTokenByHashForLookupRow{
		ID:       uuid.New(),
		TenantID: s.tenants[s.current],
	}, nil
}

func (s *twoTenantStore) ReCheckAuthorization(
	_ context.Context, _ uuid.UUID, _ uuid.UUID,
) (sqlc.ReCheckMCPRequestAuthorizationInTenantTxRow, error) {
	grant := s.grants[s.current]
	if s.rotateGrants {
		grant = uuid.New()
	}
	return sqlc.ReCheckMCPRequestAuthorizationInTenantTxRow{
		Authorized:        true,
		GrantID:           grant,
		TokenID:           uuid.New(),
		GrantCapabilities: []string{string(CapSitesRead)},
		GrantExpiresAt:    time.Now().UTC().Add(90 * 24 * time.Hour),
	}, nil
}

// THE TWO GATED METHODS. Every HTTP proof below runs against BOTH.
//
// This is not thoroughness for its own sake. An earlier version of this file
// drove tools/list only, and the mutation sweep showed that deleting the
// tools/call gate outright left the whole suite green -- the budget could have
// been removed from the one method the task names and nothing would have
// noticed. A limiter with two call sites needs both asserted, separately.
var gatedMethods = []string{
	`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
	`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_sites","arguments":{}}}`,
}

func methodName(body string) string {
	if strings.Contains(body, "tools/call") {
		return "tools/call"
	}
	return "tools/list"
}

// postWith issues one gated request with arbitrary extra headers.
func postWith(t *testing.T, r *gin.Engine, bearer, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, TransportPath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// limitData is the refusal's data object, decoded. Every proof asserts on these
// VALUES rather than on "an error happened": three tests in this package were
// found passing for the wrong reason because they only checked that Error was
// non-nil.
type limitData struct {
	RetryAfterSeconds        int    `json:"retry_after_seconds"`
	LimitScope               string `json:"limit_scope"`
	LimitPerMinute           int    `json:"limit_per_minute"`
	RetryImmediatelyWillFail bool   `json:"retry_immediately_will_fail"`
}

func decodeLimitData(t *testing.T, resp jsonrpcResponse) limitData {
	t.Helper()
	if resp.Error == nil {
		t.Fatalf("expected a JSON-RPC error, got result: %v", resp.Result)
	}
	var d limitData
	if err := json.Unmarshal(resp.Error.Data, &d); err != nil {
		t.Fatalf("refusal data is not decodable: %v (raw: %s)", err, string(resp.Error.Data))
	}
	return d
}

// drain issues n requests and returns the first refused recorder, or nil.
func drain(t *testing.T, r *gin.Engine, bearer, body string, n int) *httptest.ResponseRecorder {
	t.Helper()
	for i := 0; i < n; i++ {
		w := postWith(t, r, bearer, body, nil)
		if w.Code == http.StatusTooManyRequests {
			return w
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// PROOF 1 -- THE CONNECTION BUDGET REFUSES, AND SAYS SO BY VALUE.
//
// One grant driving one tenant hits the grant's fairness share
// (ToolCallGrantPerMin) before the tenant bound, so this is the refusal a
// single looping client actually receives. Every field is asserted: status,
// JSON-RPC code, scope, the advertised number, and the flag that tells a model
// not to retry at once.
// ---------------------------------------------------------------------------

func TestToolCall_ConnectionBudgetRefusesWithTypedValues(t *testing.T) {
	for _, body := range gatedMethods {
		t.Run(methodName(body), func(t *testing.T) {
			assertConnectionBudgetRefuses(t, body)
		})
	}
}

func assertConnectionBudgetRefuses(t *testing.T, body string) {
	t.Helper()
	r, _ := limiterRouter(t, liveGrantStore(uuid.New()))

	w := drain(t, r, testBearer, body, ToolCallGrantPerMin+1)
	if w == nil {
		t.Fatalf("no request was refused within %d calls; the budget is not enforced on %s",
			ToolCallGrantPerMin+1, methodName(body))
	}

	if w.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want %d", w.Code, http.StatusTooManyRequests)
	}

	resp := decodeRPC(t, w)
	if resp.Error == nil {
		t.Fatalf("refusal carried no JSON-RPC error: %s", w.Body.String())
	}
	if resp.Error.Code != codeRateLimited {
		t.Errorf("error code = %d, want %d", resp.Error.Code, codeRateLimited)
	}

	d := decodeLimitData(t, resp)
	if d.LimitScope != string(scopeConnection) {
		t.Errorf("limit_scope = %q, want %q", d.LimitScope, scopeConnection)
	}
	if d.LimitPerMinute != ToolCallGrantPerMin {
		t.Errorf("limit_per_minute = %d, want %d", d.LimitPerMinute, ToolCallGrantPerMin)
	}
	if !d.RetryImmediatelyWillFail {
		t.Error("retry_immediately_will_fail = false; a model reads that as permission to retry at once")
	}

	// THE REFUSAL MUST BE LEGIBLE TO A MODEL, not only to a status-code parser.
	// An MCP client surfaces the error content, so the words have to carry the
	// instruction. Asserted on the message because that is what the model reads.
	if !strings.Contains(resp.Error.Message, "retrying immediately will not succeed") {
		t.Errorf("refusal message does not tell the model that an immediate retry fails: %q", resp.Error.Message)
	}
}

// ---------------------------------------------------------------------------
// PROOF 2 -- Retry-After IS PRESENT AND IS NEVER ZERO.
//
// The header is whole seconds. A sub-second shortfall truncating to "0" tells a
// client to retry at once, which is the loop the refusal exists to break, so
// the floor is part of the contract and not an implementation detail.
// ---------------------------------------------------------------------------

func TestToolCall_RetryAfterIsAtLeastOneSecond(t *testing.T) {
	r, _ := limiterRouter(t, liveGrantStore(uuid.New()))

	w := drain(t, r, testBearer, gatedMethods[1], ToolCallGrantPerMin+1)
	if w == nil {
		t.Fatal("nothing was refused; cannot assert Retry-After")
	}

	raw := w.Header().Get("Retry-After")
	if raw == "" {
		t.Fatal("Retry-After header is absent on a 429")
	}
	secs, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("Retry-After = %q, which is not whole seconds: %v", raw, err)
	}
	if secs < 1 {
		t.Errorf("Retry-After = %d, want >= 1; zero instructs an immediate retry", secs)
	}

	d := decodeLimitData(t, decodeRPC(t, w))
	if d.RetryAfterSeconds != secs {
		t.Errorf("retry_after_seconds = %d but Retry-After = %d; the two readers disagree",
			d.RetryAfterSeconds, secs)
	}
}

// atLeastASecond IS PROVEN DIRECTLY, not only through the endpoint.
//
// The handler applies its own floor to the number it renders, so a broken
// helper is invisible from the HTTP surface -- the mutation sweep caught
// exactly that: replacing this function's body with `return 0` left every
// endpoint proof green. Two independent floors is deliberate defence, and the
// consequence is that each has to be asserted where it lives.
func TestToolCall_AtLeastASecondFloorsSubSecondWaits(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want time.Duration
	}{
		{0, time.Second},
		{time.Nanosecond, time.Second},
		{500 * time.Millisecond, time.Second},
		{999 * time.Millisecond, time.Second},
		{time.Second, time.Second},
		{90 * time.Second, 90 * time.Second},
	}
	for _, c := range cases {
		if got := atLeastASecond(c.in); got != c.want {
			t.Errorf("atLeastASecond(%v) = %v, want %v; a sub-second wait rendered as whole "+
				"seconds truncates to 0 and instructs an immediate retry", c.in, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// PROOF 3 -- NO CLIENT-SUPPLIED HEADER CAN OBTAIN A FRESH BUDGET.
//
// This is the defect two other limiters in this tree were recently found to
// have. gin's defaults trust every proxy, so ClientIP returns the LEFTMOST
// X-Forwarded-For entry -- a value the caller wrote. A limiter keyed on it
// hands out a new bucket per forged header, silently and at full speed.
//
// The test EXECUTES the bypass rather than asserting the code does not contain
// a call: it exhausts the budget, then varies every header a limiter in this
// codebase has ever been keyed on, and requires the refusal to survive all of
// them.
// ---------------------------------------------------------------------------

func TestToolCall_BudgetIsImmuneToClientSuppliedHeaders(t *testing.T) {
	for _, body := range gatedMethods {
		t.Run(methodName(body), func(t *testing.T) {
			assertHeaderImmunity(t, body)
		})
	}
}

func assertHeaderImmunity(t *testing.T, body string) {
	t.Helper()
	r, _ := limiterRouter(t, liveGrantStore(uuid.New()))

	if w := drain(t, r, testBearer, body, ToolCallGrantPerMin+1); w == nil {
		t.Fatal("budget never refused; the bypass test would be vacuous")
	}

	spoofs := []map[string]string{
		{"X-Forwarded-For": "203.0.113.9"},
		{"X-Forwarded-For": "198.51.100.7, 10.0.0.1"},
		{"X-Real-IP": "203.0.113.44"},
		{"X-Forwarded-For": "203.0.113.9", "X-Real-IP": "198.51.100.7"},
		{"Forwarded": "for=203.0.113.60"},
		{"X-Forwarded-Host": "elsewhere.example"},
		{"True-Client-IP": "203.0.113.77"},
		{"CF-Connecting-IP": "203.0.113.88"},
	}

	for _, hdrs := range spoofs {
		w := postWith(t, r, testBearer, body, hdrs)
		if w.Code != http.StatusTooManyRequests {
			t.Errorf("headers %v obtained status %d; the budget was bypassed by a client-supplied header",
				hdrs, w.Code)
			continue
		}
		resp := decodeRPC(t, w)
		if resp.Error == nil || resp.Error.Code != codeRateLimited {
			t.Errorf("headers %v produced a non-rate-limit refusal: %+v", hdrs, resp.Error)
		}
	}
}

// ---------------------------------------------------------------------------
// PROOF 4 -- ONE TENANT'S EXHAUSTED BUDGET DOES NOT REFUSE ANOTHER TENANT.
//
// Both tenants share ONE router, ONE handler and ONE limiter, which is the only
// arrangement in which a leak between them is observable. This is also the
// proof that there is no global bucket: a process-wide layer would refuse the
// second tenant here, which is the cross-tenant denial of service the design
// note rejects.
// ---------------------------------------------------------------------------

func TestToolCall_OneTenantsBudgetDoesNotAffectAnother(t *testing.T) {
	const bearerA, bearerB = "tenant-a-token", "tenant-b-token"
	store := newTwoTenantStore(bearerA, bearerB)
	// TENANT A MUST SATURATE ITS TENANT BUCKET, NOT MERELY ITS GRANT BUCKET.
	//
	// An earlier version of this proof drove one grant, which stops at the
	// grant fairness cap (60) and leaves the tenant bucket (120) half full --
	// so keying every tenant on one shared constant still passed, because
	// tenant B was served out of the slack. The mutation sweep found it.
	// Rotating the grant id models a tenant holding many connections, which is
	// the only way its tenant-wide bound is reachable.
	store.rotateGrants = true
	r, _ := limiterRouter(t, store)

	store.forBearer(bearerA)
	w := drain(t, r, bearerA, gatedMethods[1], ToolCallTenantPerMin+1)
	if w == nil {
		t.Fatal("tenant A was never refused; the isolation claim would be vacuous")
	}
	// The refusal must be the TENANT layer, or this proof is testing the wrong
	// bucket and the isolation claim below means nothing.
	if d := decodeLimitData(t, decodeRPC(t, w)); d.LimitScope != string(scopeOrganisation) {
		t.Fatalf("tenant A refused at scope %q, want %q; the tenant bound was never reached",
			d.LimitScope, scopeOrganisation)
	}

	// Tenant A is now saturated at the tenant layer. Tenant B must be
	// untouched -- first request, full budget.
	store.forBearer(bearerB)
	w = postWith(t, r, bearerB, gatedMethods[1], nil)
	if w.Code != http.StatusOK {
		t.Fatalf("tenant B status = %d, want %d; tenant A's exhaustion leaked across the tenant boundary (body: %s)",
			w.Code, http.StatusOK, w.Body.String())
	}
	resp := decodeRPC(t, w)
	if resp.Error != nil {
		t.Fatalf("tenant B was refused: code=%d msg=%q", resp.Error.Code, resp.Error.Message)
	}
}

// ---------------------------------------------------------------------------
// PROOF 5 -- THE OVER-FIRE CHECK. A CONNECTION UNDER THE LIMIT IS UNAFFECTED.
//
// A limiter that reddens correct work gets switched off, and then it bounds
// nothing. This drives a full budget's worth of legitimate calls and requires
// every one to succeed with a real answer -- not merely "not a 429".
// ---------------------------------------------------------------------------

func TestToolCall_UnderTheLimitNothingIsRefused(t *testing.T) {
	for _, body := range gatedMethods {
		t.Run(methodName(body), func(t *testing.T) {
			assertNoOverFire(t, body)
		})
	}
}

// realisticBurst is a HARD-CODED workload well inside any defensible budget: an
// interactive model answering one question about a fleet, a few times over.
//
// It is deliberately NOT expressed as ToolCallGrantPerMin. An over-fire proof
// that loops the very constant it is guarding is vacuous -- shrinking the
// budget to 1 shrinks the loop to 1 and the test still passes, which the
// mutation sweep demonstrated. The number here has to be independent of the
// thing under test, and the budget has to be at least this generous.
const realisticBurst = 20

func assertNoOverFire(t *testing.T, body string) {
	t.Helper()
	if ToolCallGrantPerMin < realisticBurst {
		t.Fatalf("ToolCallGrantPerMin = %d, which is below the %d-call burst an ordinary "+
			"interactive session produces; this budget refuses correct work",
			ToolCallGrantPerMin, realisticBurst)
	}

	r, _ := limiterRouter(t, liveGrantStore(uuid.New()))

	for i := 0; i < realisticBurst; i++ {
		w := postWith(t, r, testBearer, body, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("call %d/%d status = %d, want %d; the limiter refused work inside its own budget",
				i+1, realisticBurst, w.Code, http.StatusOK)
		}
		resp := decodeRPC(t, w)
		if resp.Error != nil {
			t.Fatalf("call %d/%d was refused: code=%d msg=%q",
				i+1, realisticBurst, resp.Error.Code, resp.Error.Message)
		}
		// A 200 carrying no tools would be a limiter that silently emptied the
		// answer, which is a different failure wearing a success code.
		if resp.Result == nil {
			t.Fatalf("call %d/%d succeeded with an empty result", i+1, realisticBurst)
		}
	}
}

// ---------------------------------------------------------------------------
// PROOF 6 -- A REFUSED REQUEST PERFORMS NO WRITE.
//
// The tools/* path stamps activity through Service.RecordActivity BEFORE it
// answers, so a limiter placed after the stamp would bound the read while
// leaving the write unbounded -- the database cost, which is the expensive
// half, would still be paid by every refused request. The gate is therefore
// ahead of the stamp, and this counts the calls to prove it.
// ---------------------------------------------------------------------------

func TestToolCall_RefusedRequestWritesNothing(t *testing.T) {
	for _, body := range gatedMethods {
		t.Run(methodName(body), func(t *testing.T) {
			assertRefusalWritesNothing(t, body)
		})
	}
}

func assertRefusalWritesNothing(t *testing.T, body string) {
	t.Helper()
	store := liveGrantStore(uuid.New())
	r, _ := limiterRouter(t, store)

	if w := drain(t, r, testBearer, body, ToolCallGrantPerMin+1); w == nil {
		t.Fatal("nothing was refused; the no-write claim would be vacuous")
	}

	before := len(store.touchCalls)

	// Twenty more refusals. None may reach the activity stamp.
	for i := 0; i < 20; i++ {
		w := postWith(t, r, testBearer, body, nil)
		if w.Code != http.StatusTooManyRequests {
			t.Fatalf("call %d was admitted (status %d); the budget stopped refusing mid-test", i+1, w.Code)
		}
	}

	if got := len(store.touchCalls); got != before {
		t.Errorf("activity stamps went from %d to %d across 20 refused requests; "+
			"a refused request is still writing to the database", before, got)
	}
}

// ---------------------------------------------------------------------------
// PROOF 7 -- THE TENANT LAYER IS THE BOUND, NOT THE GRANT LAYER.
//
// Several connections under ONE tenant must not sum to more than the tenant's
// budget. If the grant layer were the bound, a tenant would buy more capacity
// by minting more connections, and the limit would be escapable by anyone
// willing to hold N credentials.
//
// Driven at the limiter directly, because the claim is about many grants under
// one tenant and the HTTP fixture resolves one grant per bearer. The transport
// wiring that reaches this function is proven by PROOFS 1-6.
// ---------------------------------------------------------------------------

func TestToolCall_ManyGrantsCannotExceedTheTenantBound(t *testing.T) {
	l := newToolCallLimiter(ToolCallTenantPerMin, ToolCallGrantPerMin)
	tenant := uuid.New()

	admitted := 0
	// Twenty distinct grants, each asking for a full tenant budget's worth. If
	// the tenant layer binds, the total admitted is the tenant budget.
	for i := 0; i < 20; i++ {
		grant := uuid.New()
		for j := 0; j < ToolCallTenantPerMin; j++ {
			if l.allow(tenant, grant).Allowed {
				admitted++
			}
		}
	}

	if admitted > ToolCallTenantPerMin {
		t.Errorf("admitted %d calls across 20 grants under one tenant, want at most %d; "+
			"the bound is escapable by minting more connections", admitted, ToolCallTenantPerMin)
	}
	if admitted == 0 {
		t.Error("admitted 0 calls; the limiter refuses everything and the ceiling assertion is vacuous")
	}
}

// ---------------------------------------------------------------------------
// PROOF 8 -- A REFUSAL COSTS NOTHING IN EITHER BUCKET.
//
// A connection over its own fairness share must not spend its organisation's
// budget on the requests it is REFUSED for. Without this the fairness layer
// becomes the denial of service: one looping connection drains the tenant
// bucket by being rejected, which is free and fast, and starves its siblings.
// ---------------------------------------------------------------------------

func TestToolCall_RefusalDoesNotChargeTheTenantBucket(t *testing.T) {
	l := newToolCallLimiter(ToolCallTenantPerMin, ToolCallGrantPerMin)
	tenant := uuid.New()
	noisy := uuid.New()

	// Exhaust the noisy connection's share, then keep hammering it well past
	// the tenant budget. Every one of these is refused at the grant layer.
	for i := 0; i < ToolCallGrantPerMin+(ToolCallTenantPerMin*3); i++ {
		l.allow(tenant, noisy)
	}

	// A quiet sibling must still be served out of what the tenant bucket has
	// left, which is the tenant budget minus only the noisy connection's
	// ADMITTED calls.
	quiet := uuid.New()
	admitted := 0
	for i := 0; i < ToolCallTenantPerMin; i++ {
		if l.allow(tenant, quiet).Allowed {
			admitted++
		}
	}

	want := ToolCallTenantPerMin - ToolCallGrantPerMin
	if admitted < want {
		t.Errorf("quiet connection admitted %d calls, want at least %d; "+
			"refused requests are draining the tenant bucket and starving siblings", admitted, want)
	}
}

// ---------------------------------------------------------------------------
// PROOF 9 -- AN UNCONSTRUCTED LIMITER REFUSES.
//
// A handler built by a struct literal that skipped the limiter must serve
// nothing rather than serve unlimited. "Absence coerced into a plausible value"
// is the shape this endpoint is most likely to fail in.
// ---------------------------------------------------------------------------

func TestToolCall_NilLimiterRefuses(t *testing.T) {
	var l *toolCallLimiter
	d := l.allow(uuid.New(), uuid.New())
	if d.Allowed {
		t.Error("a nil limiter admitted a request; an unwired limiter reads as no limit configured")
	}
	if d.RetryAfter <= 0 {
		t.Errorf("nil-limiter RetryAfter = %v, want positive", d.RetryAfter)
	}
}

// ---------------------------------------------------------------------------
// PROOF 10 -- A NEW TENANT'S ARRIVAL DOES NOT GIVE A SATURATED TENANT ITS
// BUDGET BACK.
//
// This is the assertion whose ABSENCE was the real finding. Both key maps were
// originally swept by one function copied in shape from registrationLimiter,
// which clears its map under pressure. That is safe there because an unswept
// global bucket sits behind it; this limiter has no global bucket by design, so
// clearing the tenant map surrendered the bound outright -- and the whole suite
// stayed green when that sweep was made to clear unconditionally, because
// nothing ever re-checked a saturated tenant after a new one appeared.
//
// Driven at the limiter rather than over HTTP because the claim is about the
// map's eviction behaviour and needs thousands of distinct organisations to
// reach it. PROOFS 1-6 already establish that the transport reaches this code.
// ---------------------------------------------------------------------------

func TestToolCall_NewTenantsDoNotResetASaturatedTenantsBound(t *testing.T) {
	l := newToolCallLimiter(ToolCallTenantPerMin, ToolCallGrantPerMin)
	victim := uuid.New()

	// Saturate the victim's TENANT bucket. The grant id is rotated so the
	// fairness layer never binds first -- otherwise this would saturate the
	// wrong bucket and the proof would be about the wrong map.
	var last toolCallDecision
	for i := 0; i < ToolCallTenantPerMin+1; i++ {
		last = l.allow(victim, uuid.New())
	}
	if last.Allowed {
		t.Fatalf("victim was still admitted after %d calls; its tenant bound was never reached",
			ToolCallTenantPerMin+1)
	}
	if last.Scope != scopeOrganisation {
		t.Fatalf("victim refused at scope %q, want %q; the wrong bucket is saturated and this "+
			"proof would be about the fairness layer", last.Scope, scopeOrganisation)
	}

	// Now flood the process with distinct organisations, well past the map cap,
	// which is the condition that used to trigger the clearing sweep.
	//
	// THE FLOOD TAKES REAL TIME AND THE BUCKET REFILLS WHILE IT RUNS, so the
	// assertion below cannot be "the victim is still refused outright". allow()
	// reads time.Now(), and at ToolCallTenantPerMin per minute the victim
	// legitimately earns back a token every few hundred milliseconds. Asserting
	// a flat refusal would be asserting that a token bucket does not refill,
	// which would be a false property and a flaky test.
	//
	// What IS asserted is the thing that actually distinguishes the two
	// behaviours, and the gap between them is enormous: a preserved bucket
	// yields only what elapsed time paid for (a token or two), while a cleared
	// one yields a whole fresh burst of ToolCallTenantPerMin. The ceiling is
	// computed from the measured elapsed time rather than hard-coded, so the
	// test stays exact on a slow machine instead of being given slack.
	start := time.Now()
	for i := 0; i < toolCallKeyCap*2; i++ {
		l.allow(uuid.New(), uuid.New())
	}
	elapsed := time.Since(start)

	// Tokens the victim is ENTITLED to have earned back, plus one to absorb the
	// boundary between the last measurement and the first call below.
	earned := int(elapsed.Seconds()*float64(ToolCallTenantPerMin)/60.0) + 1

	admitted := 0
	for i := 0; i < ToolCallTenantPerMin; i++ {
		if !l.allow(victim, uuid.New()).Allowed {
			break
		}
		admitted++
	}

	if admitted > earned {
		t.Errorf("the victim tenant was admitted %d times after %d other organisations "+
			"authenticated, but only %d token(s) were earned in the %v the flood took; "+
			"its exhausted budget was handed back by the sweep, so the bound resets under load",
			admitted, toolCallKeyCap*2, earned, elapsed)
	}
	// The refusal that ends the loop must still come from the TENANT layer. If
	// it came from the grant layer the bound could have been reset and this
	// proof would not notice.
	if d := l.allow(victim, uuid.New()); d.Allowed || d.Scope != scopeOrganisation {
		t.Errorf("victim decision after the flood = {allowed:%v scope:%q}, want {false %q}",
			d.Allowed, d.Scope, scopeOrganisation)
	}
}

// ---------------------------------------------------------------------------
// PROOF 11 -- sweepFullOnly STILL RECLAIMS, SO THE MAP IS NOT A LEAK.
//
// The over-fire half of PROOF 10. "Never surrender the bound" must not degrade
// into "never reclaim anything". A bucket at its burst ceiling is
// indistinguishable from a fresh one, so dropping it is lossless and is exactly
// what this must still do -- while a bucket with tokens still spent has to
// survive, because that is the one carrying the bound.
// ---------------------------------------------------------------------------

func TestToolCall_SweepFullOnlyReclaimsRefilledButKeepsIndebtedBuckets(t *testing.T) {
	now := time.Now()
	m := map[uuid.UUID]*keyBucket{}

	full := uuid.New()
	m[full] = &keyBucket{lim: perMinuteLimiter(ToolCallTenantPerMin), seen: now}

	indebt := uuid.New()
	spent := perMinuteLimiter(ToolCallTenantPerMin)
	for i := 0; i < ToolCallTenantPerMin; i++ {
		spent.AllowN(now, 1)
	}
	m[indebt] = &keyBucket{lim: spent, seen: now}

	// A cap of 1 forces the sweep to run over this two-entry map.
	sweepFullOnly(m, 1, now)

	if _, still := m[full]; still {
		t.Error("a fully refilled bucket survived the sweep; nothing is ever reclaimed and the map leaks")
	}
	if _, still := m[indebt]; !still {
		t.Error("a bucket with tokens still spent was evicted; that hands a saturated tenant its budget back")
	}
}

// ---------------------------------------------------------------------------
// PROOF 12 -- A BROWSER-HOSTED CLIENT CAN READ Retry-After.
//
// The header is useless to browser JavaScript unless it is named in
// Access-Control-Expose-Headers: a fetch response exposes the status and
// nothing else. A client that sees 429 but cannot read the interval either
// gives up or retries at once, which is the loop the refusal exists to break.
// ---------------------------------------------------------------------------

func TestToolCall_RetryAfterIsExposedToBrowserClients(t *testing.T) {
	r, _ := limiterRouter(t, liveGrantStore(uuid.New()))

	w := drain(t, r, testBearer, gatedMethods[1], ToolCallGrantPerMin+1)
	if w == nil {
		t.Fatal("nothing was refused; cannot assert the exposed header")
	}
	if got := w.Header().Get("Retry-After"); got == "" {
		t.Fatal("Retry-After is not set on the 429 at all")
	}

	exposed := w.Header().Get("Access-Control-Expose-Headers")
	if exposed == "" {
		t.Fatal("Access-Control-Expose-Headers is absent; browser JS can read no header at all")
	}
	if !exposesHeader(exposed, "Retry-After") {
		t.Errorf("Access-Control-Expose-Headers = %q, which does not name Retry-After; "+
			"a browser-hosted client is refused and cannot learn for how long", exposed)
	}
	// The other refusal headers this file sets must stay exposed: a fix that
	// swapped one for another would otherwise pass.
	for _, want := range []string{"WWW-Authenticate", "Allow"} {
		if !exposesHeader(exposed, want) {
			t.Errorf("Access-Control-Expose-Headers = %q no longer names %s", exposed, want)
		}
	}
}

// exposesHeader reports whether an Access-Control-Expose-Headers value names
// the given header. Comparison is case-insensitive because HTTP header names
// are, so a correct list spelled differently must not read as a failure.
func exposesHeader(list, name string) bool {
	for _, part := range strings.Split(list, ",") {
		if strings.EqualFold(strings.TrimSpace(part), name) {
			return true
		}
	}
	return false
}
