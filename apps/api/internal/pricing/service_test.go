package pricing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/gomodule/redigo/redis"

	"github.com/mosamlife/wpmgr/apps/api/internal/billing"
)

// errAlwaysFailDial simulates "Redis is unreachable" (mirrors
// internal/billing/entitlements_test.go's alwaysFailDial).
var errAlwaysFailDial = errors.New("simulated redis outage")

// Tier-id string constants, derived from billing's own Tier constants rather
// than a bare paid-tier string literal — internal/billing's
// TestNoPlanLiteralsOutsideBilling grep guard forbids those literals outside
// the billing package itself (single-ownership of the plan-tier vocabulary),
// and that guard scans every .go file, tests included.
var (
	tierStarterID = string(billing.TierStarter)
	tierAgencyID  = string(billing.TierAgency)
	tierScaleID   = string(billing.TierScale)
)

// ---- helpers ----------------------------------------------------------------

// wireTier is a loosely-typed decode target mirroring the wire shape
// TierPricing.MarshalJSON produces, used to assert on a resolved response
// without depending on this package's internal struct shapes.
type wireTier struct {
	ID       string  `json:"id"`
	Amount   *int64  `json:"amount"`
	Currency *string `json:"currency"`
	Interval *string `json:"interval"`
	USD      *struct {
		Amount   int64  `json:"amount"`
		Currency string `json:"currency"`
		Interval string `json:"interval"`
	} `json:"usd"`
	INR *struct {
		Amount   int64  `json:"amount"`
		Currency string `json:"currency"`
		Interval string `json:"interval"`
	} `json:"inr"`
}

type wirePricing struct {
	CurrencyDefault string     `json:"currency_default"`
	Tiers           []wireTier `json:"tiers"`
}

func decodePricing(t *testing.T, raw []byte) wirePricing {
	t.Helper()
	var p wirePricing
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("decode pricing response: %v\nraw: %s", err, raw)
	}
	return p
}

func tierByID(t *testing.T, p wirePricing, id string) wireTier {
	t.Helper()
	for _, tier := range p.Tiers {
		if tier.ID == id {
			return tier
		}
	}
	t.Fatalf("no tier %q in response: %+v", id, p)
	return wireTier{}
}

// ---- live provider resolution ------------------------------------------------

func TestGetPricingResolvesLiveProviderPrices(t *testing.T) {
	stripe := &fakeStripeProvider{priceFn: func(tier billing.Tier) (int64, string, string, error) {
		switch tier {
		case billing.TierStarter:
			return 1500, "usd", "month", nil // lowercase, like real Stripe — proves normalization
		case billing.TierAgency:
			return 5900, "usd", "month", nil
		case billing.TierScale:
			return 16900, "usd", "month", nil
		}
		t.Fatalf("unexpected tier %q", tier)
		return 0, "", "", nil
	}}
	razorpay := &fakeRazorpayProvider{planFn: func(tier billing.Tier, currency string) (int64, string, string, error) {
		amounts := map[billing.Tier]int64{
			billing.TierStarter: 124900,
			billing.TierAgency:  489900,
			billing.TierScale:   1399900,
		}
		return amounts[tier], currency, "month", nil // INR passed through uppercase from the caller
	}}

	registry := billing.NewRegistry(stripe, razorpay)
	svc := NewService(registry, nil, slog.Default())

	raw := svc.GetPricing(context.Background())
	p := decodePricing(t, raw)

	if p.CurrencyDefault != "USD" {
		t.Fatalf("currency_default = %q, want USD", p.CurrencyDefault)
	}

	free := tierByID(t, p, "free")
	if free.Amount == nil || *free.Amount != 0 {
		t.Fatalf("free tier amount = %v, want 0", free.Amount)
	}
	if free.Currency == nil || *free.Currency != "USD" {
		t.Fatalf("free tier currency = %v, want USD", free.Currency)
	}
	if free.USD != nil || free.INR != nil {
		t.Fatalf("free tier must not carry usd/inr sub-objects, got %+v", free)
	}

	starter := tierByID(t, p, tierStarterID)
	if starter.USD == nil || starter.USD.Amount != 1500 || starter.USD.Currency != "USD" {
		t.Fatalf("starter.usd = %+v, want amount=1500 currency=USD (Stripe wins USD)", starter.USD)
	}
	if starter.INR == nil || starter.INR.Amount != 124900 || starter.INR.Currency != "INR" {
		t.Fatalf("starter.inr = %+v, want amount=124900 currency=INR", starter.INR)
	}
	if starter.Amount != nil {
		t.Fatalf("a paid tier must not carry a flat amount field, got %v", starter.Amount)
	}

	agency := tierByID(t, p, tierAgencyID)
	if agency.USD == nil || agency.USD.Amount != 5900 {
		t.Fatalf("agency.usd = %+v, want amount=5900", agency.USD)
	}

	scale := tierByID(t, p, tierScaleID)
	if scale.USD == nil || scale.USD.Amount != 16900 {
		t.Fatalf("scale.usd = %+v, want amount=16900", scale.USD)
	}

	if got := stripe.callCount(); got != 3 {
		t.Fatalf("stripe GetPrice called %d times, want 3 (one per paid tier)", got)
	}
	// Only INR per paid tier: Stripe already supplied the "usd" slot, so
	// resolveFresh never asks Razorpay for USD too (see its doc comment).
	if got := razorpay.callCount(); got != 3 {
		t.Fatalf("razorpay GetPlanAmount called %d times, want 3 (INR only per paid tier — Stripe already filled USD)", got)
	}
}

// TestGetPricingRazorpayOnlyFillsUSDSlot proves that when Stripe is not
// configured at all, Razorpay's own USD plan fills the "usd" slot instead of
// it being omitted — "whichever are configured" from a single-provider
// instance.
func TestGetPricingRazorpayOnlyFillsUSDSlot(t *testing.T) {
	razorpay := &fakeRazorpayProvider{planFn: func(tier billing.Tier, currency string) (int64, string, string, error) {
		if currency == "USD" {
			return 1500, "USD", "month", nil
		}
		return 124900, "INR", "month", nil
	}}
	registry := billing.NewRegistry(razorpay) // no Stripe provider registered
	svc := NewService(registry, nil, slog.Default())

	p := decodePricing(t, svc.GetPricing(context.Background()))
	starter := tierByID(t, p, tierStarterID)
	if starter.USD == nil || starter.USD.Amount != 1500 {
		t.Fatalf("starter.usd = %+v, want Razorpay's USD amount=1500 when Stripe is unconfigured", starter.USD)
	}
	if starter.INR == nil || starter.INR.Amount != 124900 {
		t.Fatalf("starter.inr = %+v, want amount=124900", starter.INR)
	}
}

// TestGetPricingStripeOnlyOmitsINR proves that when Razorpay is not
// configured, the "inr" sub-object is simply absent (never fabricated) —
// "Include only the sub-objects for configured providers/currencies".
func TestGetPricingStripeOnlyOmitsINR(t *testing.T) {
	stripe := &fakeStripeProvider{priceFn: func(billing.Tier) (int64, string, string, error) {
		return 1500, "USD", "month", nil
	}}
	registry := billing.NewRegistry(stripe) // no Razorpay provider registered
	svc := NewService(registry, nil, slog.Default())

	p := decodePricing(t, svc.GetPricing(context.Background()))
	starter := tierByID(t, p, tierStarterID)
	if starter.USD == nil || starter.USD.Amount != 1500 {
		t.Fatalf("starter.usd = %+v, want amount=1500", starter.USD)
	}
	if starter.INR != nil {
		t.Fatalf("starter.inr = %+v, want omitted (no Razorpay provider configured)", starter.INR)
	}
}

// ---- fail-open: provider error / nothing configured -> static fallback ------

func TestGetPricingFallsBackToStaticOnProviderError(t *testing.T) {
	stripe := &fakeStripeProvider{priceFn: func(billing.Tier) (int64, string, string, error) {
		return 0, "", "", context.DeadlineExceeded // simulate a Stripe outage
	}}
	registry := billing.NewRegistry(stripe)
	svc := NewService(registry, nil, slog.Default())

	raw := svc.GetPricing(context.Background()) // must never panic/500
	p := decodePricing(t, raw)

	starter := tierByID(t, p, tierStarterID)
	wantAmount := int64(billing.MonthlyPriceCentsForTier(billing.TierStarter))
	if starter.USD == nil || starter.USD.Amount != wantAmount {
		t.Fatalf("starter.usd = %+v, want the static fallback list price %d", starter.USD, wantAmount)
	}
	free := tierByID(t, p, "free")
	if free.Amount == nil || *free.Amount != 0 {
		t.Fatalf("free tier amount = %v, want 0 even on the static fallback", free.Amount)
	}
}

func TestGetPricingNoProvidersConfiguredFallsBackToStatic(t *testing.T) {
	svc := NewService(billing.NewRegistry(), nil, slog.Default())

	raw := svc.GetPricing(context.Background())
	p := decodePricing(t, raw)

	for _, tier := range []billing.Tier{billing.TierStarter, billing.TierAgency, billing.TierScale} {
		entry := tierByID(t, p, string(tier))
		want := int64(billing.MonthlyPriceCentsForTier(tier))
		if entry.USD == nil || entry.USD.Amount != want {
			t.Fatalf("%s.usd = %+v, want the static fallback list price %d", tier, entry.USD, want)
		}
	}
}

// TestGetPricingNilRegistryNeverPanics proves a nil registry (should not
// happen given cmd/wpmgr's wiring, but this endpoint must be bulletproof) is
// handled exactly like an empty one.
func TestGetPricingNilRegistryNeverPanics(t *testing.T) {
	svc := NewService(nil, nil, slog.Default())
	raw := svc.GetPricing(context.Background())
	_ = decodePricing(t, raw) // must decode cleanly, not panic
}

// ---- cache behavior -----------------------------------------------------------

func TestGetPricingCacheHitSkipsProviderCalls(t *testing.T) {
	srv := newFakeRedisServer()
	seeded := []byte(`{"currency_default":"USD","tiers":[{"id":"free","amount":0,"currency":"USD","interval":"month"}]}`)
	srv.seed(cacheKey, seeded)

	stripe := &fakeStripeProvider{priceFn: func(billing.Tier) (int64, string, string, error) {
		t.Fatal("stripe GetPrice must not be called on a warm cache")
		return 0, "", "", nil
	}}
	razorpay := &fakeRazorpayProvider{planFn: func(billing.Tier, string) (int64, string, string, error) {
		t.Fatal("razorpay GetPlanAmount must not be called on a warm cache")
		return 0, "", "", nil
	}}
	registry := billing.NewRegistry(stripe, razorpay)
	svc := NewService(registry, srv.pool(), slog.Default())

	raw := svc.GetPricing(context.Background())
	if string(raw) != string(seeded) {
		t.Fatalf("cache-hit response = %s, want the exact seeded payload %s", raw, seeded)
	}
	if got := stripe.callCount(); got != 0 {
		t.Fatalf("stripe.callCount() = %d, want 0 on a cache hit", got)
	}
	if got := razorpay.callCount(); got != 0 {
		t.Fatalf("razorpay.callCount() = %d, want 0 on a cache hit", got)
	}
}

func TestGetPricingCacheMissResolvesAndCaches(t *testing.T) {
	srv := newFakeRedisServer()
	stripe := &fakeStripeProvider{priceFn: func(billing.Tier) (int64, string, string, error) {
		return 1500, "USD", "month", nil
	}}
	registry := billing.NewRegistry(stripe)
	svc := NewService(registry, srv.pool(), slog.Default())

	first := svc.GetPricing(context.Background())
	if got := stripe.callCount(); got != 3 {
		t.Fatalf("after a cold cache, stripe.callCount() = %d, want 3 (one per paid tier)", got)
	}

	// A second call must be served entirely from the now-warm cache — no
	// further provider calls.
	second := svc.GetPricing(context.Background())
	if string(first) != string(second) {
		t.Fatalf("second GetPricing = %s, want identical to the first %s", second, first)
	}
	if got := stripe.callCount(); got != 3 {
		t.Fatalf("after a warm cache, stripe.callCount() = %d, want still 3 (no re-resolve)", got)
	}
}

// TestGetPricingCacheMissWritesLongTTLOnLiveSuccess proves a full live
// resolve is cached under cacheTTLSeconds (1h) — the complement of
// TestGetPricingCachesFallbackWithShortTTLOnProviderError's short TTL.
func TestGetPricingCacheMissWritesLongTTLOnLiveSuccess(t *testing.T) {
	srv := newFakeRedisServer()
	stripe := &fakeStripeProvider{priceFn: func(billing.Tier) (int64, string, string, error) {
		return 1500, "USD", "month", nil
	}}
	svc := NewService(billing.NewRegistry(stripe), srv.pool(), slog.Default())

	svc.GetPricing(context.Background())

	gotTTL, ok := srv.lastSetexTTL(cacheKey)
	if !ok {
		t.Fatal("expected the live resolve to be written to the cache")
	}
	if gotTTL != cacheTTLSeconds {
		t.Fatalf("live-success cache TTL = %d, want %d (cacheTTLSeconds)", gotTTL, cacheTTLSeconds)
	}
}

// TestGetPricingCachesFallbackWithShortTTLOnProviderError is the MEDIUM
// hardening this test locks in: when every configured provider errors, the
// STATIC FALLBACK itself is negative-cached under fallbackCacheTTLSeconds
// (60s, not the 1h live-success TTL) — so an immediate second request during
// a provider outage is served from that negative cache instead of retrying
// the (still-down) provider, rate-limiting the outage to roughly one resolve
// attempt per window instead of one per public request.
func TestGetPricingCachesFallbackWithShortTTLOnProviderError(t *testing.T) {
	srv := newFakeRedisServer()
	stripe := &fakeStripeProvider{priceFn: func(billing.Tier) (int64, string, string, error) {
		return 0, "", "", context.DeadlineExceeded // simulated Stripe outage
	}}
	svc := NewService(billing.NewRegistry(stripe), srv.pool(), slog.Default())

	first := svc.GetPricing(context.Background())
	firstCalls := stripe.callCount()
	if firstCalls == 0 {
		t.Fatal("expected at least one provider call attempt to discover the outage")
	}

	gotTTL, ok := srv.lastSetexTTL(cacheKey)
	if !ok {
		t.Fatal("expected the static fallback to be written to the cache (negative caching)")
	}
	if gotTTL != fallbackCacheTTLSeconds {
		t.Fatalf("fallback cache TTL = %d, want %d (fallbackCacheTTLSeconds — short, not the 1h live-success TTL)", gotTTL, fallbackCacheTTLSeconds)
	}

	// An immediate second request must be served from that negative cache —
	// NOT by retrying the still-down provider.
	second := svc.GetPricing(context.Background())
	if string(second) != string(first) {
		t.Fatalf("second GetPricing = %s, want the identical cached fallback %s", second, first)
	}
	if got := stripe.callCount(); got != firstCalls {
		t.Fatalf("stripe.callCount() = %d after a second request, want unchanged %d (the negative cache must absorb it, not the provider)", got, firstCalls)
	}
}

// TestGetPricingConcurrentColdCacheCallsCollapseViaSingleflight is the
// second MEDIUM-hardening lock-in: N concurrent callers that all miss a
// cold cache at once must trigger exactly ONE upstream resolve (one
// GetPrice call per paid tier), not N — singleflight.Group.Do dedups them,
// keyed by the single cache key this package ever resolves.
func TestGetPricingConcurrentColdCacheCallsCollapseViaSingleflight(t *testing.T) {
	stripe := &fakeStripeProvider{
		// Long enough that all N goroutines below reliably issue their
		// GetPricing call while the FIRST (singleflight leader) call is
		// still in flight, so they genuinely collapse rather than racing to
		// each be "first" in turn.
		delay: 150 * time.Millisecond,
		priceFn: func(billing.Tier) (int64, string, string, error) {
			return 1500, "USD", "month", nil
		},
	}
	svc := NewService(billing.NewRegistry(stripe), newFakeRedisServer().pool(), slog.Default())

	const n = 20
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make([][]byte, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i] = svc.GetPricing(context.Background())
		}(i)
	}
	close(start)
	wg.Wait()

	for i := 1; i < n; i++ {
		if string(results[i]) != string(results[0]) {
			t.Fatalf("result %d = %s, want identical to result 0 %s — singleflight must serve every waiter the same payload", i, results[i], results[0])
		}
	}
	// Exactly one resolve's worth of provider calls (one per paid tier),
	// regardless of n concurrent callers.
	if got := stripe.callCount(); got != 3 {
		t.Fatalf("stripe.callCount() = %d, want exactly 3 — %d concurrent cold-cache callers must collapse into ONE upstream resolve, not fan out", got, n)
	}
}

// TestGetPricingHungProviderRespectsBoundedTimeout is the third
// MEDIUM-hardening lock-in: a provider call that never returns must not pin
// GetPricing (or the gin handler calling it) for tens of seconds — the
// resolve is hard-capped at resolveTimeout and falls back to the static
// list, never a 500. resolveTimeout is temporarily shortened so this test
// does not itself take 4+ real seconds to run.
func TestGetPricingHungProviderRespectsBoundedTimeout(t *testing.T) {
	original := resolveTimeout
	resolveTimeout = 50 * time.Millisecond
	t.Cleanup(func() { resolveTimeout = original })

	stripe := &fakeStripeProvider{hang: true}
	svc := NewService(billing.NewRegistry(stripe), nil, slog.Default())

	done := make(chan []byte, 1)
	start := time.Now()
	go func() { done <- svc.GetPricing(context.Background()) }()

	select {
	case raw := <-done:
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("GetPricing took %v, want it bounded by resolveTimeout (%v)", elapsed, resolveTimeout)
		}
		p := decodePricing(t, raw)
		starter := tierByID(t, p, tierStarterID)
		want := int64(billing.MonthlyPriceCentsForTier(billing.TierStarter))
		if starter.USD == nil || starter.USD.Amount != want {
			t.Fatalf("starter.usd = %+v, want the static fallback list price %d (a hung provider must still resolve to the fallback, never hang or 500)", starter.USD, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("GetPricing did not return within 5s — a hung provider must not be able to pin this call past resolveTimeout")
	}
}

// TestGetPricingRedisDownStillResolves proves a Redis outage (every Dial
// fails) degrades to always resolving fresh — never a hard dependency, never
// an error out of GetPricing.
func TestGetPricingRedisDownStillResolves(t *testing.T) {
	stripe := &fakeStripeProvider{priceFn: func(billing.Tier) (int64, string, string, error) {
		return 1500, "USD", "month", nil
	}}
	registry := billing.NewRegistry(stripe)
	alwaysFailPool := &redis.Pool{
		Dial:    func() (redis.Conn, error) { return nil, errAlwaysFailDial },
		MaxIdle: 1,
	}
	svc := NewService(registry, alwaysFailPool, slog.Default())

	raw := svc.GetPricing(context.Background())
	p := decodePricing(t, raw)
	starter := tierByID(t, p, tierStarterID)
	if starter.USD == nil || starter.USD.Amount != 1500 {
		t.Fatalf("starter.usd = %+v, want amount=1500 resolved fresh despite Redis being down", starter.USD)
	}
}

// ---- response shape / no-secrets -----------------------------------------------

// allowedResponseKeys is the exhaustive set of JSON keys this endpoint may
// EVER emit, at any nesting level. Any other key appearing anywhere in the
// response is treated as a potential secret/id leak.
var allowedResponseKeys = map[string]bool{
	"currency_default": true,
	"tiers":            true,
	"id":               true,
	"amount":           true,
	"currency":         true,
	"interval":         true,
	"usd":              true,
	"inr":              true,
}

func TestGetPricingResponseHasNoSecretFields(t *testing.T) {
	stripe := &fakeStripeProvider{priceFn: func(billing.Tier) (int64, string, string, error) {
		return 1500, "USD", "month", nil
	}}
	razorpay := &fakeRazorpayProvider{planFn: func(billing.Tier, string) (int64, string, string, error) {
		return 124900, "INR", "month", nil
	}}
	registry := billing.NewRegistry(stripe, razorpay)
	svc := NewService(registry, nil, slog.Default())

	raw := svc.GetPricing(context.Background())
	var generic interface{}
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	walkKeysAndValues(t, generic)

	// Also assert the static fallback carries no secrets.
	walkKeysAndValues(t, mustDecodeGeneric(t, staticFallbackJSON()))
}

func mustDecodeGeneric(t *testing.T, raw []byte) interface{} {
	t.Helper()
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return v
}

// allowedTierIDs is the exhaustive set of values an "id" field may EVER
// hold, derived from billing's own Tier constants (never a bare paid-tier
// string literal — see the grep-guard note on tierStarterID et al. above).
var allowedTierIDs = map[string]bool{
	string(billing.TierFree): true,
	tierStarterID:            true,
	tierAgencyID:             true,
	tierScaleID:              true,
}

// currencyPattern is what every "currency" field value must match: a bare
// ISO 4217 alpha-3 code, uppercase, nothing else (no provider-internal
// currency code variant, no stray whitespace/casing).
var currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)

// walkKeysAndValues recursively checks a decoded response: every object key
// must be in allowedResponseKeys (catches an unexpected/leaked field), AND
// every "id"/"currency" VALUE must match the expected, narrow shape (catches
// a leaked value hiding behind an otherwise-expected key name — the
// security review's own note that a keys-only check is not sufficient).
func walkKeysAndValues(t *testing.T, v interface{}) {
	t.Helper()
	switch val := v.(type) {
	case map[string]interface{}:
		for k, vv := range val {
			if !allowedResponseKeys[k] {
				t.Fatalf("unexpected response key %q — possible secret/id leak", k)
			}
			switch k {
			case "id":
				s, ok := vv.(string)
				if !ok || !allowedTierIDs[s] {
					t.Fatalf("id value %v is not one of the known tiers %v — possible unexpected/leaked value", vv, allowedTierIDs)
				}
			case "currency":
				s, ok := vv.(string)
				if !ok || !currencyPattern.MatchString(s) {
					t.Fatalf("currency value %v does not match %s — possible unexpected/leaked value", vv, currencyPattern)
				}
			}
			walkKeysAndValues(t, vv)
		}
	case []interface{}:
		for _, vv := range val {
			walkKeysAndValues(t, vv)
		}
	}
}

// ---- TierPricing wire shape ----------------------------------------------------

func TestTierPricingMarshalJSONFreeIsFlat(t *testing.T) {
	tier := TierPricing{ID: "free", Free: &PriceQuote{Amount: 0, Currency: "USD", Interval: "month"}}
	raw, err := json.Marshal(tier)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"id":"free","amount":0,"currency":"USD","interval":"month"}`
	if string(raw) != want {
		t.Fatalf("free tier JSON = %s, want %s", raw, want)
	}
}

func TestTierPricingMarshalJSONPaidIsNested(t *testing.T) {
	tier := TierPricing{
		ID:  tierStarterID,
		USD: &PriceQuote{Amount: 1500, Currency: "USD", Interval: "month"},
	}
	raw, err := json.Marshal(tier)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := fmt.Sprintf(`{"id":%q,"usd":{"amount":1500,"currency":"USD","interval":"month"}}`, tierStarterID)
	if string(raw) != want {
		t.Fatalf("paid tier JSON = %s, want %s (inr omitted when nil)", raw, want)
	}
}
