package auth

// oidc_discovery_test.go: the lazy issuer discovery that both the generic OIDC
// relying party and the Google adapter sit on.
//
// The three assertions here are the three ways the first version of this was
// wrong, and each one is a live incident rather than a hypothetical:
//
//  1. It cached success but not failure, so while the issuer was unreachable
//     EVERY sign-in attempt emitted its own outbound discovery request. A flood
//     of sign-ins became a flood at the issuer, the issuer throttled us, and the
//     throttling kept discovery failing.
//  2. It held a plain mutex across the network call, so concurrent callers
//     queued behind one another and each then made its OWN request on reaching
//     the front.
//  3. It built the discovery context from the inbound request's context, so a
//     browser that closed its tab cancelled an attempt every other waiting
//     caller was depending on.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// newDiscoveredProvider builds a real *oidc.Provider from a local discovery
// document, so the fake discover functions below hand back the genuine article
// rather than a nil the code under test could accidentally accept.
func newDiscoveredProvider(t *testing.T) *oidc.Provider {
	t.Helper()
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"issuer":%q,"authorization_endpoint":%q,"token_endpoint":%q,"jwks_uri":%q,"id_token_signing_alg_values_supported":["RS256"]}`,
			srv.URL, srv.URL+"/authorize", srv.URL+"/token", srv.URL+"/jwks")
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	p, err := oidc.NewProvider(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("build test provider: %v", err)
	}
	return p
}

// newTestDiscovery returns a discovery wired to a counting fake and a clock the
// test controls.
func newTestDiscovery(fn func(ctx context.Context) (*oidc.Provider, error)) (*oidcDiscovery, *atomic.Int64, *fakeClock) {
	var attempts atomic.Int64
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0).UTC()}
	d := newOIDCDiscovery("https://issuer.test")
	d.now = clk.Now
	d.discover = func(ctx context.Context, _ string) (*oidc.Provider, error) {
		attempts.Add(1)
		return fn(ctx)
	}
	return d, &attempts, clk
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

var errIssuerDown = errors.New("issuer unreachable")

// A failure must be remembered for a cooldown, so an unreachable issuer costs
// ONE outbound request per cooldown window rather than one per sign-in attempt.
//
// This is the only thing bounding our outbound discovery traffic, and it is why
// the sign-in handshake needs no shared rate limiter to protect the issuer from
// us. Remove the negative cache and this fails at the second call.
func TestOIDCDiscoveryRemembersFailureForACooldown(t *testing.T) {
	d, attempts, clk := newTestDiscovery(func(context.Context) (*oidc.Provider, error) {
		return nil, errIssuerDown
	})

	for i := range 20 {
		if _, err := d.get(context.Background()); err == nil {
			t.Fatalf("call %d: want the issuer failure to surface", i)
		}
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("20 sign-in attempts against an unreachable issuer made %d outbound discovery requests, want 1", got)
	}

	// Not permanent either: past the cooldown the next caller retries, so a
	// transient outage clears itself without a redeploy.
	clk.advance(discoveryCooldown + time.Second)
	if _, err := d.get(context.Background()); err == nil {
		t.Fatal("want the failure to surface again after the cooldown")
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("after the cooldown expired the outbound attempt count is %d, want 2", got)
	}
}

// Concurrent callers must share ONE attempt rather than queueing behind a mutex
// and each making their own on reaching the front.
//
// The failing issuer is the case that matters, and the case the mutex version
// got wrong: on success it happened to look fine, because the first caller
// through the lock cached a provider the rest then found. On failure it cached
// nothing, so all thirty two ran in turn, each waiting out every attempt before
// it and then adding one more request to an issuer that was already refusing us.
func TestOIDCDiscoveryConcurrentCallersShareOneAttempt(t *testing.T) {
	release := make(chan struct{})
	d, attempts, _ := newTestDiscovery(func(context.Context) (*oidc.Provider, error) {
		<-release
		return nil, errIssuerDown
	})

	const callers = 32
	var wg sync.WaitGroup
	errs := make([]error, callers)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = d.get(context.Background())
		}()
	}
	// Let every caller arrive before the single attempt completes.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	for i := range callers {
		if errs[i] == nil {
			t.Fatalf("caller %d: want the issuer failure to surface", i)
		}
	}
	if n := attempts.Load(); n != 1 {
		t.Fatalf("%d concurrent sign-ins made %d outbound discovery requests, want 1", callers, n)
	}
}

// A caller whose request context is cancelled must return promptly.
//
// The mutex version could not do this: a goroutine parked in mu.Lock() cannot
// observe cancellation, so it held a handler goroutine and an inbound slot for
// the whole ten second discovery timeout no matter what the browser did.
func TestOIDCDiscoveryWaiterObservesItsOwnCancellation(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	d, _, _ := newTestDiscovery(func(context.Context) (*oidc.Provider, error) {
		<-release
		return nil, errIssuerDown
	})

	// First caller occupies the in-flight attempt.
	started := make(chan struct{})
	go func() {
		close(started)
		_, _ = d.get(context.Background())
	}()
	<-started
	time.Sleep(20 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := d.get(ctx); done <- err }()
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("a cancelled waiter returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a cancelled waiter stayed parked; it cannot be observing cancellation")
	}
}

// A caller disconnecting must not cancel the attempt every OTHER caller is
// waiting on. The attempt therefore runs on a context of its own.
//
// Without this a single client that starts a sign-in and closes the tab poisons
// the shared attempt, and because the failure is then negatively cached, that
// one disconnect would refuse every sign-in for the whole cooldown.
func TestOIDCDiscoverySurvivesTheStartingCallerDisconnecting(t *testing.T) {
	want := newDiscoveredProvider(t)
	release := make(chan struct{})
	discoveryCtx := make(chan context.Context, 1)
	d, attempts, _ := newTestDiscovery(func(ctx context.Context) (*oidc.Provider, error) {
		discoveryCtx <- ctx
		<-release
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return want, nil
	})

	// The caller that starts the attempt, and then goes away.
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _, _ = d.get(ctx) }()
	inner := <-discoveryCtx
	cancel()
	time.Sleep(20 * time.Millisecond)

	if err := inner.Err(); err != nil {
		t.Fatalf("the in-flight discovery context was cancelled by the caller going away: %v", err)
	}
	close(release)

	// A later caller sees the cached success, with no second outbound request.
	got, err := d.get(context.Background())
	if err != nil {
		t.Fatalf("discovery that the disconnect should not have touched failed: %v", err)
	}
	if got != want {
		t.Fatal("discovery did not cache the provider the surviving attempt produced")
	}
	if n := attempts.Load(); n != 1 {
		t.Fatalf("outbound discovery attempts = %d, want 1", n)
	}
}
