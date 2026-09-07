package auth

// Regression pins for the login brute-force throttle. Interactive login was
// the sole unthrottled credential endpoint (reset, 2FA and verify-resend all
// had limiters), leaving password spray and argon2 CPU exhaustion open.
//
// The throttle runs BEFORE input validation and the account lookup, which is
// what lets these tests drive it without a database: an under-cap attempt
// with an empty password returns a validation error before any repo access,
// an over-cap attempt returns KindRateLimited. The wiring of the handler's
// address derivation is pinned by TestDecisionSitesUseLimiterAddr; the
// behaviour of limiterAddr itself by the tests alongside it.

import (
	"context"
	"fmt"
	"net/netip"
	"testing"

	"github.com/mosamlife/wpmgr/apps/api/internal/autologin"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

func throttleTestService(lim *autologin.MemoryLimiter) *Service {
	return &Service{limiter: lim, validator: domain.NewValidator()}
}

func kindOf(t *testing.T, err error) domain.Kind {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error, got nil")
	}
	de, ok := domain.AsDomain(err)
	if !ok {
		t.Fatalf("expected a domain error, got %T: %v", err, err)
	}
	return de.Kind
}

func TestLoginPerUserThrottleFires(t *testing.T) {
	lim := autologin.NewMemoryLimiter()
	defer lim.Stop()
	svc := throttleTestService(lim)
	ctx := context.Background()

	var last error
	for i := 0; i < loginUserPerMinute+1; i++ {
		_, last = svc.Login(ctx, "victim@example.com", "", netip.Addr{})
	}
	if kind := kindOf(t, last); kind != domain.KindRateLimited {
		t.Fatalf("attempt %d on one account: kind = %v, want KindRateLimited",
			loginUserPerMinute+1, kind)
	}
}

func TestLoginPerUserThrottleSeparatesAccounts(t *testing.T) {
	// Over-fire control: exhausting one account's budget must not refuse a
	// different account (distinct emails get distinct buckets).
	lim := autologin.NewMemoryLimiter()
	defer lim.Stop()
	svc := throttleTestService(lim)
	ctx := context.Background()

	for i := 0; i < loginUserPerMinute+1; i++ {
		_, _ = svc.Login(ctx, "victim@example.com", "", netip.Addr{})
	}
	_, err := svc.Login(ctx, "bystander@example.com", "", netip.Addr{})
	if kind := kindOf(t, err); kind != domain.KindValidation {
		t.Fatalf("other account after victim's budget spent: kind = %v, want KindValidation", kind)
	}
}

func TestLoginPerIPThrottleFires(t *testing.T) {
	// Spraying distinct accounts from one address must trip the per-IP cap
	// even though no single account reaches the per-user cap.
	lim := autologin.NewMemoryLimiter()
	defer lim.Stop()
	svc := throttleTestService(lim)
	ctx := context.Background()
	ip := netip.MustParseAddr("203.0.113.200")

	var last error
	for i := 0; i < loginIPPerMinute+1; i++ {
		_, last = svc.Login(ctx, fmt.Sprintf("user%d@example.com", i), "", ip)
	}
	if kind := kindOf(t, last); kind != domain.KindRateLimited {
		t.Fatalf("attempt %d from one address: kind = %v, want KindRateLimited",
			loginIPPerMinute+1, kind)
	}
}

func TestLoginPerIPThrottleSeparatesClients(t *testing.T) {
	// Over-fire control: distinct addresses get distinct buckets — a second
	// client is not refused by the first client's spray.
	lim := autologin.NewMemoryLimiter()
	defer lim.Stop()
	svc := throttleTestService(lim)
	ctx := context.Background()

	for i := 0; i < loginIPPerMinute+1; i++ {
		_, _ = svc.Login(ctx, fmt.Sprintf("user%d@example.com", i), "", netip.MustParseAddr("203.0.113.200"))
	}
	_, err := svc.Login(ctx, "fresh@example.com", "", netip.MustParseAddr("203.0.113.201"))
	if kind := kindOf(t, err); kind != domain.KindValidation {
		t.Fatalf("distinct client after spray: kind = %v, want KindValidation", kind)
	}
}

func TestLoginZeroIPSkipsPerIPLimit(t *testing.T) {
	// A zero netip.Addr must skip the per-IP limit (matching
	// checkCrossChallengeLimits), not key every caller onto one bucket.
	lim := autologin.NewMemoryLimiter()
	defer lim.Stop()
	svc := throttleTestService(lim)
	ctx := context.Background()

	for i := 0; i < loginIPPerMinute+1; i++ {
		if _, err := svc.Login(ctx, fmt.Sprintf("user%d@example.com", i), "", netip.Addr{}); err != nil {
			if kind := kindOf(t, err); kind == domain.KindRateLimited {
				t.Fatalf("attempt %d with zero addr rate limited; zero addr must skip the per-IP limit", i+1)
			}
		}
	}
}
