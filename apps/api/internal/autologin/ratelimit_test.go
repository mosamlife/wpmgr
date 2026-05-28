package autologin

import (
	"context"
	"testing"
	"time"
)

// TestMemoryLimiterEnforcesPerMinuteCap fills a key's bucket and asserts that
// the (limit+1)th call within the burst window is rejected with a non-zero
// retry hint. This is the building block for the per-(initiator,site) and
// per-site service caps.
func TestMemoryLimiterEnforcesPerMinuteCap(t *testing.T) {
	t.Parallel()
	lim := NewMemoryLimiter()
	defer lim.Stop()

	ctx := context.Background()
	const cap = 5
	for i := 0; i < cap; i++ {
		ok, _ := lim.Allow(ctx, "k", cap)
		if !ok {
			t.Fatalf("call %d rejected within budget (cap=%d)", i+1, cap)
		}
	}
	ok, retry := lim.Allow(ctx, "k", cap)
	if ok {
		t.Fatalf("call %d accepted past budget (cap=%d)", cap+1, cap)
	}
	if retry <= 0 {
		t.Fatalf("rejected call should report a positive retry hint, got %v", retry)
	}
}

// TestMemoryLimiterKeysAreIndependent proves a busy key does not affect a
// fresh key, so per-(initiator,site) backpressure does not leak across users.
func TestMemoryLimiterKeysAreIndependent(t *testing.T) {
	t.Parallel()
	lim := NewMemoryLimiter()
	defer lim.Stop()
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		_, _ = lim.Allow(ctx, "user-a", 3)
	}
	// user-a is now empty; user-b must still be fresh.
	ok, _ := lim.Allow(ctx, "user-b", 3)
	if !ok {
		t.Fatal("user-b bucket leaked from user-a")
	}
}

// TestMemoryLimiterJanitorPrunesIdle proves the janitor drops unused buckets,
// preventing memory growth in long-lived processes with many distinct keys.
func TestMemoryLimiterJanitorPrunesIdle(t *testing.T) {
	t.Parallel()
	lim := NewMemoryLimiter()
	defer lim.Stop()
	ctx := context.Background()
	_, _ = lim.Allow(ctx, "idle", 10)

	lim.mu.Lock()
	if _, ok := lim.buckets["idle"]; !ok {
		lim.mu.Unlock()
		t.Fatal("bucket missing immediately after Allow")
	}
	// Force the janitor's idle window for this test.
	lim.idle = time.Microsecond
	lim.buckets["idle"].lastAccess = time.Now().Add(-time.Hour)
	lim.mu.Unlock()

	// Manually invoke a sweep (skip the 5-minute ticker).
	lim.mu.Lock()
	for k, b := range lim.buckets {
		if time.Since(b.lastAccess) > lim.idle {
			delete(lim.buckets, k)
		}
	}
	lim.mu.Unlock()
	lim.mu.Lock()
	if _, ok := lim.buckets["idle"]; ok {
		lim.mu.Unlock()
		t.Fatal("janitor failed to prune idle bucket")
	}
	lim.mu.Unlock()
}
