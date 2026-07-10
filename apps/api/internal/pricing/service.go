// Package pricing serves the PUBLIC, unauthenticated GET /api/v1/pricing
// endpoint the marketing site reads to show accurate hosted-plan prices. It
// is HOSTED-ONLY (mounted only when WPMGR_HOSTED is enabled — see
// internal/server/server.go's Deps.PricingH and cmd/wpmgr/main.go's wiring)
// and never touches Postgres: it reads live prices from whichever payment
// providers are configured (billing.Registry's optional StripePriceReader/
// RazorpayPlanReader capabilities), behind a short Redis cache, with a
// static in-Go fallback — so a marketing page load can NEVER 500 or hammer a
// payment provider, even during a Stripe/Razorpay outage.
//
// Three things make this endpoint safe under public, unauthenticated fan-out
// (see resolveAndCache):
//
//  1. NEGATIVE CACHING — the static fallback is cached too, just under a
//     short TTL (fallbackCacheTTLSeconds), so a provider outage is rate
//     limited to roughly one resolve attempt per window instead of one per
//     public request.
//  2. SINGLEFLIGHT — concurrent cold-cache callers collapse into ONE
//     upstream resolve via golang.org/x/sync/singleflight, so a burst of N
//     simultaneous requests never fans out to N*(3-6) provider calls.
//  3. A BOUNDED resolve timeout (resolveTimeout), independent of each
//     individual provider call's own HTTP client timeout, so one
//     slow/hanging provider can never pin a gin handler (or every caller
//     piggybacking on the same singleflight call) for tens of seconds.
//
// Together these protect the Stripe/Razorpay account's rate limit — and
// therefore any OTHER caller of those same accounts, e.g. real checkout/
// webhook traffic — from collateral damage caused by this public endpoint.
//
// The response carries ONLY amount/currency/interval/tier — never a
// provider secret, key, or id (see PriceQuote) — and is identical for every
// caller (no session, no tenant).
package pricing

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/gomodule/redigo/redis"
	"golang.org/x/sync/singleflight"

	"github.com/mosamlife/wpmgr/apps/api/internal/billing"
)

// cacheKey is the single Redis key the resolved pricing payload is cached
// under (both a live success and the static fallback use this same key —
// see cacheTTLSeconds / fallbackCacheTTLSeconds). It also doubles as the
// singleflight.Group key: this package only ever resolves one thing.
const cacheKey = "pricing:v1"

// cacheTTLSeconds is the Redis TTL for a FULL LIVE resolve — mirrored by the
// handler's HTTP `Cache-Control: max-age` (see Handler.handle).
const cacheTTLSeconds = 3600 // 1 hour

// fallbackCacheTTLSeconds is the Redis TTL used when GetPricing serves the
// static fallback (no provider configured, a provider call errored, a
// resolve timed out, or a marshal failure) — deliberately SHORT. Caching the
// fallback at all (not just a live success) is what turns a Stripe/Razorpay
// outage into "at most one resolve attempt every ~60s" instead of "one
// resolve attempt per public request", which is what protects the payment
// provider account's rate limit from a fan-out of concurrent public callers.
const fallbackCacheTTLSeconds = 60

// resolveTimeout bounds the ENTIRE fresh-provider resolve (every Stripe/
// Razorpay call combined), independent of each individual provider call's
// own ~15s HTTP client timeout. A public, unauthenticated gin handler must
// never be pinned for tens of seconds by one hung upstream call — see
// resolveAndCache. A `var`, not a `const`, SOLELY so the test suite can
// temporarily shorten it (see TestGetPricingHungProviderRespectsBoundedTimeout)
// rather than a unit test genuinely waiting out a real 4s timeout; production
// code never mutates it.
var resolveTimeout = 4 * time.Second

// paidTiers are the three purchasable tiers this endpoint prices. Free is
// handled separately (always a flat, zero-amount quote — never priced by a
// payment provider). Uses billing's own Tier constants, never a bare string
// literal, so this package carries no second copy of the plan-tier
// vocabulary (see internal/billing's package doc + grep_guard_test.go).
var paidTiers = []billing.Tier{billing.TierStarter, billing.TierAgency, billing.TierScale}

// PriceQuote is one concrete, live price point: the per-billing-cycle charge
// in the currency's smallest unit (cents/paise), its ISO 4217 currency code,
// and the billing interval (e.g. "month"). Carries NO provider secret, key,
// or id.
type PriceQuote struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
	Interval string `json:"interval"`
}

// TierPricing is one subscription tier's resolved public pricing.
//
//   - The free tier sets Free (a flat, always-zero-amount PriceQuote) and
//     renders as flat {"id","amount","currency","interval"} fields.
//   - A paid tier sets USD and/or INR — ONLY the sub-objects for whichever
//     provider/currency pairs this instance actually resolved a price for
//     (Stripe may not be configured; Razorpay may not be configured) — and
//     renders as {"id","usd":{...},"inr":{...}}.
//
// See MarshalJSON for the flat-vs-nested wire shape.
type TierPricing struct {
	ID   string
	Free *PriceQuote
	USD  *PriceQuote
	INR  *PriceQuote
}

// MarshalJSON renders the free tier as flat {"id","amount","currency",
// "interval"} fields, and a paid tier as {"id","usd":{...},"inr":{...}}
// (only the sub-objects this instance actually resolved a price for) — the
// wire shape the marketing site's pricing page consumes.
func (t TierPricing) MarshalJSON() ([]byte, error) {
	if t.Free != nil {
		return json.Marshal(struct {
			ID       string `json:"id"`
			Amount   int64  `json:"amount"`
			Currency string `json:"currency"`
			Interval string `json:"interval"`
		}{ID: t.ID, Amount: t.Free.Amount, Currency: t.Free.Currency, Interval: t.Free.Interval})
	}
	return json.Marshal(struct {
		ID  string      `json:"id"`
		USD *PriceQuote `json:"usd,omitempty"`
		INR *PriceQuote `json:"inr,omitempty"`
	}{ID: t.ID, USD: t.USD, INR: t.INR})
}

// Pricing is the full GET /api/v1/pricing response body.
type Pricing struct {
	CurrencyDefault string        `json:"currency_default"`
	Tiers           []TierPricing `json:"tiers"`
}

// Service resolves the public pricing payload: Redis cache -> live provider
// reads (deduped + timeout-bounded) -> a static in-Go fallback. Every method
// is safe to call even when redis is nil (cache always a miss, still
// correct — just resolves fresh every time) or registry is nil/empty (falls
// straight to the static fallback).
type Service struct {
	registry *billing.Registry
	redis    *redis.Pool // optional; nil disables the pricing cache (still correct, just uncached)
	logger   *slog.Logger

	// group dedups concurrent cold-cache resolves (see resolveAndCache) —
	// its zero value is ready to use, mirroring internal/rucss/service's
	// identical singleflight.Group field.
	group singleflight.Group
}

// NewService builds a pricing Service. redisPool is expected to be the SAME
// pool billing.Service uses for its own entitlements cache (a distinct
// "pricing:" key prefix keeps the two from colliding) — may be nil.
func NewService(registry *billing.Registry, redisPool *redis.Pool, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{registry: registry, redis: redisPool, logger: logger}
}

// GetPricing returns the exact JSON bytes to serve for GET /api/v1/pricing.
// It NEVER errors, NEVER blocks for more than resolveTimeout, and NEVER
// calls a payment provider on a cache hit:
//
//  1. A warm Redis cache ("pricing:v1") is served as-is — no provider calls.
//  2. On a cache miss (or a Redis outage), it resolves a fresh payload (or
//     the static fallback) via resolveAndCache — concurrent callers that
//     miss the cache at the same time collapse into ONE such resolve via
//     singleflight, keyed by the single cache key this package resolves.
func (s *Service) GetPricing(ctx context.Context) []byte {
	if raw, ok := s.getCached(ctx); ok {
		return raw
	}

	v, err, _ := s.group.Do(cacheKey, func() (interface{}, error) {
		return s.resolveAndCache(ctx), nil
	})
	if err != nil {
		// resolveAndCache never itself returns an error — this is a pure
		// defensive backstop so a hypothetical singleflight-internal failure
		// still can never turn into a 500.
		s.logger.Warn("pricing: singleflight resolve failed, serving the static fallback", slog.Any("error", err))
		return staticFallbackJSON()
	}
	raw, ok := v.([]byte)
	if !ok {
		return staticFallbackJSON()
	}
	return raw
}

// resolveAndCache resolves a fresh pricing payload — or, on any failure, the
// static fallback — and writes whichever it produced to the Redis cache
// before returning it (a live success under cacheTTLSeconds, everything
// else under the much shorter fallbackCacheTTLSeconds — see that constant's
// doc comment for why caching the fallback matters).
//
// The resolve itself runs against a context that is (a) DECOUPLED from
// parentCtx's cancellation via context.WithoutCancel, so one singleflight
// caller's client disconnecting can never cut off every OTHER caller
// piggybacking on the same in-flight resolve, and (b) hard-capped at
// resolveTimeout regardless — a slow/hanging provider can therefore never
// pin this call, or any gin handler waiting on it, for tens of seconds.
func (s *Service) resolveAndCache(parentCtx context.Context) []byte {
	cacheCtx := context.WithoutCancel(parentCtx)

	resolveCtx, cancel := context.WithTimeout(cacheCtx, resolveTimeout)
	defer cancel()

	if p, ok := s.resolveFresh(resolveCtx); ok {
		raw, err := json.Marshal(p)
		if err == nil {
			s.setCached(cacheCtx, raw, cacheTTLSeconds)
			return raw
		}
		s.logger.Warn("pricing: failed to marshal resolved pricing, serving the static fallback", slog.Any("error", err))
	}

	// Every fallback path — nothing configured, a provider error, a resolve
	// timeout, or a marshal failure — is negative-cached under the SAME key
	// with the short TTL: this is what rate-limits a provider incident to
	// roughly one resolve attempt per fallbackCacheTTLSeconds instead of one
	// attempt per public request.
	raw := staticFallbackJSON()
	s.setCached(cacheCtx, raw, fallbackCacheTTLSeconds)
	return raw
}

// resolveFresh reads live prices from whichever payment providers are
// configured. ok=false when there is nothing live to resolve (no provider
// configured at all) or ANY configured provider call errors — a partial
// result is never served; the caller falls back to the cache-miss/static
// path instead, so a customer never sees a half-resolved price list.
func (s *Service) resolveFresh(ctx context.Context) (Pricing, bool) {
	stripeReader, hasStripe := stripePriceReader(s.registry)
	razorpayReader, hasRazorpay := razorpayPlanReader(s.registry)
	if !hasStripe && !hasRazorpay {
		return Pricing{}, false
	}

	out := Pricing{
		CurrencyDefault: "USD",
		Tiers: []TierPricing{
			{ID: string(billing.TierFree), Free: &PriceQuote{Amount: 0, Currency: "USD", Interval: "month"}},
		},
	}

	for _, tier := range paidTiers {
		entry := TierPricing{ID: string(tier)}

		if hasStripe {
			amount, currency, interval, err := stripeReader.GetPrice(ctx, tier)
			if err != nil {
				s.logger.Warn("pricing: stripe price lookup failed", slog.String("tier", string(tier)), slog.Any("error", err))
				return Pricing{}, false
			}
			entry.USD = &PriceQuote{Amount: amount, Currency: strings.ToUpper(currency), Interval: interval}
		}

		if hasRazorpay {
			// Stripe (when configured) is the USD source of truth — Razorpay
			// only fills the "usd" slot when Stripe itself is not configured.
			// INR has no Stripe equivalent, so Razorpay always supplies it.
			if entry.USD == nil {
				amount, currency, interval, err := razorpayReader.GetPlanAmount(ctx, tier, "USD")
				if err != nil {
					s.logger.Warn("pricing: razorpay USD plan lookup failed", slog.String("tier", string(tier)), slog.Any("error", err))
					return Pricing{}, false
				}
				entry.USD = &PriceQuote{Amount: amount, Currency: strings.ToUpper(currency), Interval: interval}
			}
			amount, currency, interval, err := razorpayReader.GetPlanAmount(ctx, tier, "INR")
			if err != nil {
				s.logger.Warn("pricing: razorpay INR plan lookup failed", slog.String("tier", string(tier)), slog.Any("error", err))
				return Pricing{}, false
			}
			entry.INR = &PriceQuote{Amount: amount, Currency: strings.ToUpper(currency), Interval: interval}
		}

		out.Tiers = append(out.Tiers, entry)
	}

	return out, true
}

// stripePriceReader resolves the registered "stripe" provider's optional
// billing.StripePriceReader capability. ok=false when no Stripe provider is
// registered (not configured on this instance) — handled gracefully, never
// an error: the "usd" slot is simply filled by Razorpay instead, or omitted
// entirely if neither is configured.
func stripePriceReader(registry *billing.Registry) (billing.StripePriceReader, bool) {
	if registry == nil {
		return nil, false
	}
	p, ok := registry.Provider("stripe")
	if !ok {
		return nil, false
	}
	reader, ok := p.(billing.StripePriceReader)
	return reader, ok
}

// razorpayPlanReader resolves the registered "razorpay" provider's optional
// billing.RazorpayPlanReader capability. ok=false when no Razorpay provider
// is registered — handled gracefully, never an error.
func razorpayPlanReader(registry *billing.Registry) (billing.RazorpayPlanReader, bool) {
	if registry == nil {
		return nil, false
	}
	p, ok := registry.Provider("razorpay")
	if !ok {
		return nil, false
	}
	reader, ok := p.(billing.RazorpayPlanReader)
	return reader, ok
}

// staticFallbackJSON returns the hardcoded, always-available list-price
// payload used when neither the Redis cache nor a fresh provider resolve is
// available (every payment provider is down, or none is configured yet).
// Built from billing's own plan-ladder list prices
// (MonthlyPriceCentsForTier) — the single source of truth for what each
// tier costs — so this fallback can never drift from the real list price.
// USD only: without a live Razorpay read there is no authoritative INR
// amount to show, so the "inr" sub-object is simply omitted here.
//
// The caller (resolveAndCache) negative-caches this result under
// fallbackCacheTTLSeconds — this function itself is pure/uncached.
func staticFallbackJSON() []byte {
	tiers := []TierPricing{
		{ID: string(billing.TierFree), Free: &PriceQuote{Amount: 0, Currency: "USD", Interval: "month"}},
	}
	for _, tier := range paidTiers {
		tiers = append(tiers, TierPricing{
			ID:  string(tier),
			USD: &PriceQuote{Amount: int64(billing.MonthlyPriceCentsForTier(tier)), Currency: "USD", Interval: "month"},
		})
	}
	raw, err := json.Marshal(Pricing{CurrencyDefault: "USD", Tiers: tiers})
	if err != nil {
		// json.Marshal on this fixed, always-well-formed shape cannot
		// realistically fail; this keeps the endpoint at a safe, non-500
		// empty-list response rather than panicking if it somehow did.
		return []byte(`{"currency_default":"USD","tiers":[]}`)
	}
	return raw
}

// getCached returns the cached pricing payload, or (nil, false) on a cache
// miss OR any Redis failure. Every failure path logs a warning and falls
// through to a fresh resolve — Redis must never be a hard dependency for a
// public, unauthenticated endpoint.
func (s *Service) getCached(ctx context.Context) ([]byte, bool) {
	if s.redis == nil {
		return nil, false
	}
	conn, err := s.redis.GetContext(ctx)
	if err != nil {
		s.logger.Warn("pricing: redis unavailable, resolving pricing fresh", slog.Any("error", err))
		return nil, false
	}
	defer conn.Close()

	raw, err := redis.Bytes(conn.Do("GET", cacheKey))
	if err != nil {
		if err != redis.ErrNil {
			s.logger.Warn("pricing: redis GET failed, resolving pricing fresh", slog.Any("error", err))
		}
		return nil, false
	}
	if len(raw) == 0 {
		return nil, false
	}
	return raw, true
}

// setCached best-effort writes raw to Redis under cacheKey with the given
// TTL — cacheTTLSeconds for a full live resolve, fallbackCacheTTLSeconds for
// the static fallback (see resolveAndCache). Any failure is logged and
// swallowed: a cache write is never allowed to fail the request that just
// resolved pricing correctly.
func (s *Service) setCached(ctx context.Context, raw []byte, ttlSeconds int) {
	if s.redis == nil {
		return
	}
	conn, err := s.redis.GetContext(ctx)
	if err != nil {
		s.logger.Warn("pricing: redis unavailable, skipping pricing cache write", slog.Any("error", err))
		return
	}
	defer conn.Close()
	if _, err := conn.Do("SETEX", cacheKey, ttlSeconds, raw); err != nil {
		s.logger.Warn("pricing: redis SETEX failed", slog.Any("error", err))
	}
}
