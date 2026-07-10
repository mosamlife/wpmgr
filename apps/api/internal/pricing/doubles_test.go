package pricing

// doubles_test.go — shared test doubles for this package's tests: fake
// payment-provider adapters (implementing billing.Provider plus the
// optional StripePriceReader/RazorpayPlanReader capabilities) and a tiny
// in-memory fake Redis server (implementing redigo's redis.Conn), so
// Service.GetPricing's cache/resolve/fallback logic is testable without a
// live payment provider or a real Redis instance.

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gomodule/redigo/redis"

	"github.com/mosamlife/wpmgr/apps/api/internal/billing"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// ---- fake Stripe provider ---------------------------------------------------

// fakeStripeProvider implements billing.Provider (stubbed, unused methods)
// plus billing.StripePriceReader (the method this package's tests actually
// exercise). priceFn is nil-safe: a nil priceFn makes GetPrice always error.
//
// hang and delay simulate a slow/hung upstream, exactly like a real HTTP
// client bound to ctx would behave: hang blocks until ctx is done and
// returns ctx.Err() (proves resolveTimeout actually bounds the call); delay
// sleeps (or returns early on ctx.Done()) before calling priceFn — long
// enough to make concurrent singleflight callers reliably overlap in a test.
type fakeStripeProvider struct {
	mu      sync.Mutex
	calls   int
	hang    bool
	delay   time.Duration
	priceFn func(tier billing.Tier) (int64, string, string, error)
}

func (f *fakeStripeProvider) Name() string { return "stripe" }

func (f *fakeStripeProvider) CreateCheckout(context.Context, billing.CheckoutInput) (billing.CheckoutSession, error) {
	return billing.CheckoutSession{}, domain.Unavailable("unused", "unused in this test double")
}

func (f *fakeStripeProvider) CreatePortalSession(context.Context, string) (billing.PortalSession, error) {
	return billing.PortalSession{}, domain.Unavailable("unused", "unused in this test double")
}

func (f *fakeStripeProvider) CancelSubscription(context.Context, string) error { return nil }

func (f *fakeStripeProvider) GetSubscription(context.Context, string) (billing.Subscription, error) {
	return billing.Subscription{}, domain.Unavailable("unused", "unused in this test double")
}

func (f *fakeStripeProvider) VerifyWebhook([]byte, http.Header) (billing.Event, error) {
	return billing.Event{}, domain.Unavailable("unused", "unused in this test double")
}

func (f *fakeStripeProvider) MapPriceToPlan(string) (billing.Tier, bool) { return "", false }

func (f *fakeStripeProvider) HasPortal() bool { return true }

// GetPrice implements billing.StripePriceReader.
func (f *fakeStripeProvider) GetPrice(ctx context.Context, tier billing.Tier) (int64, string, string, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()

	if f.hang {
		<-ctx.Done()
		return 0, "", "", ctx.Err()
	}
	if f.delay > 0 {
		select {
		case <-ctx.Done():
			return 0, "", "", ctx.Err()
		case <-time.After(f.delay):
		}
	}
	if f.priceFn == nil {
		return 0, "", "", domain.Internal("fake_stripe_unconfigured", "fakeStripeProvider.priceFn is nil")
	}
	return f.priceFn(tier)
}

func (f *fakeStripeProvider) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// ---- fake Razorpay provider -------------------------------------------------

// fakeRazorpayProvider implements billing.Provider (stubbed, unused methods)
// plus billing.RazorpayPlanReader.
type fakeRazorpayProvider struct {
	mu     sync.Mutex
	calls  int
	planFn func(tier billing.Tier, currency string) (int64, string, string, error)
}

func (f *fakeRazorpayProvider) Name() string { return "razorpay" }

func (f *fakeRazorpayProvider) CreateCheckout(context.Context, billing.CheckoutInput) (billing.CheckoutSession, error) {
	return billing.CheckoutSession{}, domain.Unavailable("unused", "unused in this test double")
}

func (f *fakeRazorpayProvider) CreatePortalSession(context.Context, string) (billing.PortalSession, error) {
	return billing.PortalSession{}, domain.Unavailable("razorpay_no_portal", "no hosted portal")
}

func (f *fakeRazorpayProvider) CancelSubscription(context.Context, string) error { return nil }

func (f *fakeRazorpayProvider) GetSubscription(context.Context, string) (billing.Subscription, error) {
	return billing.Subscription{}, domain.Unavailable("unused", "unused in this test double")
}

func (f *fakeRazorpayProvider) VerifyWebhook([]byte, http.Header) (billing.Event, error) {
	return billing.Event{}, domain.Unavailable("unused", "unused in this test double")
}

func (f *fakeRazorpayProvider) MapPriceToPlan(string) (billing.Tier, bool) { return "", false }

func (f *fakeRazorpayProvider) HasPortal() bool { return false }

// GetPlanAmount implements billing.RazorpayPlanReader.
func (f *fakeRazorpayProvider) GetPlanAmount(ctx context.Context, tier billing.Tier, currency string) (int64, string, string, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.planFn == nil {
		return 0, "", "", domain.Internal("fake_razorpay_unconfigured", "fakeRazorpayProvider.planFn is nil")
	}
	return f.planFn(tier, currency)
}

func (f *fakeRazorpayProvider) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// ---- fake Redis --------------------------------------------------------------

// fakeRedisServer is a tiny in-memory Redis stand-in: just enough of GET/
// SETEX/DEL for Service's cache methods, backed by a shared map so every
// connection dialed from the SAME server sees the SAME data (mirroring how a
// real redis.Pool's connections all talk to the same server). It does NOT
// actually expire keys after their TTL (there is no real clock involved) —
// it only RECORDS the TTL each SETEX was called with (see lastSetexTTL), so
// tests can assert the negative-cache-vs-live-success TTL split without a
// real 1h/60s wait.
type fakeRedisServer struct {
	mu   sync.Mutex
	data map[string][]byte
	ttls map[string]int
}

func newFakeRedisServer() *fakeRedisServer {
	return &fakeRedisServer{data: map[string][]byte{}, ttls: map[string]int{}}
}

func (s *fakeRedisServer) dial() (redis.Conn, error) {
	return &fakeRedisConn{srv: s}, nil
}

func (s *fakeRedisServer) pool() *redis.Pool {
	return &redis.Pool{Dial: s.dial, MaxIdle: 1}
}

// seed pre-populates key with raw (as SETEX would have), for a cache-hit test.
func (s *fakeRedisServer) seed(key string, raw []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = raw
}

// lastSetexTTL returns the TTL (seconds) the most recent SETEX for key was
// called with, or (0, false) if key has never been SETEX'd.
func (s *fakeRedisServer) lastSetexTTL(key string) (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ttl, ok := s.ttls[key]
	return ttl, ok
}

type fakeRedisConn struct {
	srv *fakeRedisServer
}

func (c *fakeRedisConn) Close() error                      { return nil }
func (c *fakeRedisConn) Err() error                        { return nil }
func (c *fakeRedisConn) Send(string, ...interface{}) error { return nil }
func (c *fakeRedisConn) Flush() error                      { return nil }
func (c *fakeRedisConn) Receive() (interface{}, error)     { return nil, nil }

func (c *fakeRedisConn) Do(cmd string, args ...interface{}) (interface{}, error) {
	c.srv.mu.Lock()
	defer c.srv.mu.Unlock()
	switch cmd {
	case "GET":
		key, _ := args[0].(string)
		v, ok := c.srv.data[key]
		if !ok {
			return nil, redis.ErrNil
		}
		return v, nil
	case "SETEX":
		if len(args) != 3 {
			return nil, fmt.Errorf("fakeRedisConn: SETEX wants 3 args, got %d", len(args))
		}
		key, _ := args[0].(string)
		c.srv.data[key] = toBytes(args[2])
		c.srv.ttls[key] = toInt(args[1])
		return "OK", nil
	case "DEL":
		key, _ := args[0].(string)
		if _, ok := c.srv.data[key]; ok {
			delete(c.srv.data, key)
			return int64(1), nil
		}
		return int64(0), nil
	default:
		return nil, fmt.Errorf("fakeRedisConn: unsupported command %q", cmd)
	}
}

func toBytes(v interface{}) []byte {
	switch t := v.(type) {
	case []byte:
		return t
	case string:
		return []byte(t)
	default:
		return []byte(fmt.Sprint(t))
	}
}

// toInt converts a SETEX TTL argument (an int, passed as `interface{}` via
// redigo's variadic Do(...args)) to a plain int for lastSetexTTL.
func toInt(v interface{}) int {
	switch t := v.(type) {
	case int:
		return t
	case int64:
		return int(t)
	default:
		return 0
	}
}
