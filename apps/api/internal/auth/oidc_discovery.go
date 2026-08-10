package auth

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/sync/singleflight"
)

// oidcDiscovery resolves one issuer's OpenID Connect metadata on first use, and
// keeps the result.
//
// DISCOVERY MUST NOT BE ON THE BOOT PATH. Both callers used to do it inline at
// startup and treat the error as fatal, so an unreachable or merely slow issuer
// stopped the entire control plane from starting: backups, updates, uptime and
// every dashboard down because a third party was having a bad morning, in
// exchange for learning ten seconds earlier that a sign-in button might not
// work. Doing it lazily also means a transient failure is not permanent, where a
// boot-time failure needed a redeploy to clear.
//
// The lazy version then has to answer the question the boot-time one never
// faced: what happens when a lot of people arrive at once and the issuer is
// down. Three things, and each one is here because its absence was a real
// defect:
//
//   - A failure is remembered for cooldown. Without it, every sign-in attempt
//     while the issuer was unreachable emitted its own outbound request, so a
//     burst of sign-ins became a burst at the issuer, the issuer throttled us,
//     and the throttling kept discovery failing. THIS IS THE BOUND ON OUR
//     OUTBOUND DISCOVERY TRAFFIC: at most one request per issuer per cooldown,
//     plus the one in flight, whatever the inbound rate.
//   - Concurrent callers share one attempt (singleflight) instead of queueing
//     behind a mutex. Under a mutex a waiter cannot observe its own request
//     being cancelled, so it held a handler goroutine for the whole discovery
//     timeout after the browser had already gone.
//   - The attempt runs on a context of its own, not the inbound request's.
//     Sharing an attempt means sharing its cancellation, so one client closing a
//     tab would have cancelled the attempt every other caller was waiting on,
//     and (with the negative cache above) refused sign-in for the whole cooldown
//     on the strength of one disconnect.
type oidcDiscovery struct {
	issuer   string
	timeout  time.Duration
	cooldown time.Duration

	// group collapses concurrent get calls into one outbound attempt.
	group singleflight.Group

	// mu guards the memo below only. It is never held across the network call:
	// that was the original defect.
	mu          sync.Mutex
	provider    *oidc.Provider
	failedUntil time.Time
	lastErr     error

	// Test seams. Nothing outside a test replaces either.
	now      func() time.Time
	discover func(ctx context.Context, issuer string) (*oidc.Provider, error)
}

const (
	// discoveryTimeout bounds one outbound discovery attempt.
	discoveryTimeout = 10 * time.Second
	// discoveryCooldown is how long a failed attempt is remembered. Long enough
	// that a flood of sign-ins cannot turn into a flood at the issuer, short
	// enough that an operator who fixes their issuer does not wait on us.
	discoveryCooldown = 30 * time.Second
)

func newOIDCDiscovery(issuer string) *oidcDiscovery {
	return &oidcDiscovery{
		issuer:   issuer,
		timeout:  discoveryTimeout,
		cooldown: discoveryCooldown,
		now:      time.Now,
		discover: func(ctx context.Context, issuer string) (*oidc.Provider, error) {
			return oidc.NewProvider(ctx, issuer)
		},
	}
}

// ready reports whether discovery has already succeeded. Only tests ask.
func (d *oidcDiscovery) ready() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.provider != nil
}

// memo answers from what is already known: a discovered provider, or a recent
// failure still inside its cooldown. ok is false when an attempt is needed.
func (d *oidcDiscovery) memo() (*oidc.Provider, error, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.provider != nil {
		return d.provider, nil, true
	}
	if d.now().Before(d.failedUntil) {
		return nil, d.lastErr, true
	}
	return nil, nil, false
}

// get returns the issuer's metadata, discovering it if needed.
func (d *oidcDiscovery) get(ctx context.Context) (*oidc.Provider, error) {
	if p, err, ok := d.memo(); ok {
		return p, err
	}
	// A caller who has already given up starts nothing. Checked here rather than
	// relying on the select below, so an abandoned request cannot be the thing
	// that occupies the in-flight slot.
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	ch := d.group.DoChan(d.issuer, func() (any, error) {
		// Another flight may have finished between memo() above and this
		// goroutine being scheduled.
		if p, err, ok := d.memo(); ok {
			return p, err
		}
		// context.Background(), deliberately, NOT the caller's context. This
		// attempt is shared, so binding it to one caller's request would let that
		// caller's disconnect fail it for everybody waiting.
		dctx, cancel := context.WithTimeout(context.Background(), d.timeout)
		defer cancel()

		p, err := d.discover(dctx, d.issuer)

		d.mu.Lock()
		defer d.mu.Unlock()
		if err != nil {
			d.lastErr = fmt.Errorf("oidc discovery for issuer %q: %w", d.issuer, err)
			d.failedUntil = d.now().Add(d.cooldown)
			return nil, d.lastErr
		}
		d.provider = p
		d.failedUntil = time.Time{}
		d.lastErr = nil
		return p, nil
	})

	select {
	case <-ctx.Done():
		// The attempt carries on without us; the next caller gets its result.
		return nil, ctx.Err()
	case res := <-ch:
		if res.Err != nil {
			return nil, res.Err
		}
		p, _ := res.Val.(*oidc.Provider)
		if p == nil {
			return nil, fmt.Errorf("oidc discovery for issuer %q returned no provider", d.issuer)
		}
		return p, nil
	}
}
