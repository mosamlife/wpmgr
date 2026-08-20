package tests

// gh496_listen_pool_leak_integration_test.go — GH #496.
//
// THE DEFECT. events.Listener took a connection from the SHARED application
// pool, issued `LISTEN wpmgr_site_events` on it, and released it on context
// cancellation with a plain conn.Release() and no UNLISTEN:
//
//	conn, _ := l.pool.Acquire(ctx)
//	defer conn.Release()
//	conn.Exec(ctx, "LISTEN "+notifyChannel)
//
// LISTEN is SESSION state, not statement state, so the connection goes back to
// the pool still subscribed and the next — unrelated — caller gets it. pgx does
// not intervene at any of the three points it might, all verified against the
// pgx v5.9.2 that go.mod pins:
//
//   - the default BuildContextWatcherHandler is DeadlineContextWatcherHandler
//     (pgconn/config.go:314), whose HandleCancel calls SetDeadline. It does not
//     close the connection.
//   - the read that then fails inside WaitForNotification -> receiveMessage ->
//     peekMessage is guarded by `if !(isNetErr && netErr.Timeout()) {
//     pgConn.asyncClose() }` (pgconn/pgconn.go:600-603). A deadline produces
//     exactly a timeout net.Error, so asyncClose is SKIPPED.
//   - pgxpool.Conn.Release destroys only on `IsClosed() || IsBusy() ||
//     TxStatus() != 'I'` (pgxpool/conn.go:33). All three are false.
//
// So the connection is filed as healthy and reused, subscribed. The stray
// subscription is not the worst of it: pgx installs bufferNotifications as the
// connection's notification handler (conn.go:274), appending to a slice that
// ONLY WaitForNotification drains — and the caller who gets the connection next
// is a query or a transaction, which never drains it.
//
// WHY IT IS NOT AN INCIDENT TODAY, AND WHY IT STILL GETS A FIX. Two accidents
// of the current wiring contain it: cmd/wpmgr/main.go starts Run(ctx) with the
// process signal context, so the only cancellation is SIGTERM, when the pool is
// being torn down anyway; and db.ConnectApp sets MaxConnIdleTime = 90s, which
// eventually reaps a leaked idle connection. Neither is a property of the
// listener. Narrow that context — a per-tenant listener, a restartable
// subscriber, a test harness — and the leak is live. This file pins the
// listener's own behaviour so the containment does not have to hold.
//
// THE PROOF IS THROUGH THE APPLICATION POOL, as wpmgr_app (NOSUPERUSER,
// NOBYPASSRLS) — the same *db.Pool the request path uses. A test that opened
// its own connection would be examining a session the listener never touched
// and would pass vacuously.
//
// NOT RUN BY CI (apps/api/tests is excluded from the fast lane by owner
// decision). Run with `make test-integration` from the repository root, or
// directly with the command in each test's doc comment.
//
// NOT PINNED: the Hijack+Close branch inside unsubscribe()'s error path. Every
// assertion here goes through pg_listening_channels(), which proves the
// outcome (no pooled connection is left subscribed) but cannot tell which
// branch produced it — "UNLISTEN succeeded, connection released" and
// "UNLISTEN failed, connection hijacked and closed" both leave zero subscribed
// connections in the pool. On the path exercised here UNLISTEN does not fail,
// so only the success branch runs. Pinning the Hijack branch would need a
// different observable — pool.Stat().NewConnsCount() catching the pool
// re-dialling a connection it lost — not pg_listening_channels(). Left
// unpinned deliberately; do not read coverage of that branch out of these
// tests passing.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
	events "github.com/mosamlife/wpmgr/apps/api/internal/site/events"
)

// gh496NotifyChannel duplicates the unexported events.notifyChannel. It is
// spelled out here on purpose: this test asserts on the literal channel name a
// Postgres session reports through pg_listening_channels(), so importing the
// constant would let a rename of the constant silently move the assertion along
// with the code and keep the test green.
const gh496NotifyChannel = "wpmgr_site_events"

// gh496QuietLogger keeps the listener's Warn/Info lines out of the test output.
// The listener logs on every reconnect, and one of the tests below cancels it
// deliberately.
func gh496QuietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// gh496StartListener starts a real events.Listener against the real pool and
// blocks until it is PROVABLY attached, by publishing an event and waiting for
// it to arrive through the Hub. That round-trip is the readiness gate: a sleep
// would make the subsequent assertions depend on timing, and a listener that
// had not yet issued LISTEN when the context was cancelled would leak nothing
// and prove nothing.
//
// It returns the tenant it published under, the Hub subscription channel (still
// live, for callers that publish again), the unsubscribe func, and a channel
// closed when Run returns.
func gh496StartListener(t *testing.T, ctx context.Context, pool *db.Pool, tenant uuid.UUID) (<-chan site.ConnectionEvent, func(), <-chan struct{}) {
	t.Helper()

	hub := events.NewHub()
	sub, unsubscribe := hub.Subscribe(tenant)

	listener := events.NewListener(pool, hub, gh496QuietLogger())
	done := make(chan struct{})
	go func() {
		defer close(done)
		listener.Run(ctx)
	}()

	// The listener may not have issued LISTEN yet, and a NOTIFY fired before it
	// does is simply lost (LISTEN/NOTIFY has no replay). Publish on a ticker
	// until one lands, so the gate is "a notification made it end to end",
	// never "we waited long enough".
	//
	// ONE deadline governs the whole readiness phase — both the repeated
	// Publish calls AND the wait for delivery — via a single context fed to
	// both. Publish runs before the select on every iteration; left on an
	// unbounded context, a publish that blocks (a wedged connection, a
	// stalled write) would never reach the select at all, so the select's own
	// deadline arm would never run — an unbounded wait hiding inside a loop
	// that looks bounded. A per-publish timeout that reset on every tick would
	// not fix this either: it would let an unbounded number of individually
	// timely-looking publishes add up past 30s in total. One context, one
	// expiry, and it is shared by the call that can block and the select that
	// is supposed to bound it.
	readyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	pub := events.NewPublisher(pool, domain.SystemClock{})
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		if err := pub.Publish(readyCtx, site.ConnectionEvent{
			TenantID: tenant,
			Type:     "gh496.readiness",
		}); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("the readiness publish did not return within the 30s readiness budget "+
					"(a wedged connection or a stalled write on the publish path) — this is a "+
					"publish failure, not evidence the listener failed to deliver: %v", err)
			}
			t.Fatalf("publish the readiness event: %v", err)
		}
		select {
		case <-sub:
			return sub, unsubscribe, done
		case <-tick.C:
		case <-readyCtx.Done():
			t.Fatal("the listener never delivered a notification through the Hub within 30s, " +
				"so it was never attached and nothing below would be attributable to the fix")
		}
	}
}

// gh496SubscribedChannels asks ONE acquired pooled connection what its own
// session is listening to. pg_listening_channels() is session-local, which is
// exactly the property needed: it reports the state of the session the pool
// just handed us, not the state of the cluster.
func gh496SubscribedChannels(t *testing.T, conn *pgxpool.Conn) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(context.Background(),
		`SELECT count(*) FROM pg_listening_channels() AS c WHERE c = $1`,
		gh496NotifyChannel).Scan(&n); err != nil {
		t.Fatalf("read pg_listening_channels() on an acquired pooled connection: %v", err)
	}
	return n
}

// gh496AssertPoolIsClean drains the pool — it acquires MaxConns connections AT
// ONCE and holds them all — then asserts that not one of them is still
// subscribed.
//
// WHY THE WHOLE POOL, AND NOT ONE ACQUIRE. A single Acquire can hand back a
// connection the listener never touched, in which case the assertion passes
// while the leaked connection sits idle next to it — a vacuous green, and the
// exact way this class of test fools you. pgxpool never exceeds MaxConns total,
// so holding MaxConns simultaneously means every connection the pool owns is in
// hand: if the listener's connection was returned to the pool, it is one of
// these. The only other possibility is that it was destroyed rather than
// returned, which is also a pass — a destroyed connection cannot be handed to
// anyone — and pgx replaces it with a freshly dialled, unsubscribed one.
func gh496AssertPoolIsClean(t *testing.T, pool *db.Pool, stage string) {
	t.Helper()

	maxConns := int(pool.Config().MaxConns)
	if maxConns < 1 {
		t.Fatalf("SETUP FAILURE: pool reports MaxConns=%d, so 'drain the pool' is not defined", maxConns)
	}

	acquireCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	held := make([]*pgxpool.Conn, 0, maxConns)
	defer func() {
		for _, c := range held {
			c.Release()
		}
	}()

	for i := 0; i < maxConns; i++ {
		c, err := pool.Acquire(acquireCtx)
		if err != nil {
			t.Fatalf("acquire connection %d/%d while draining the pool at stage %q: %v", i+1, maxConns, stage, err)
		}
		held = append(held, c)
	}

	subscribed := 0
	for _, c := range held {
		subscribed += gh496SubscribedChannels(t, c)
	}
	if subscribed > 0 {
		t.Fatalf("GH #496 (%s): %d of the %d connections in the pool are STILL LISTENing on %q "+
			"after the listener released. The pool hands one of these to the next unrelated "+
			"caller — a request handler, a River worker — which never calls "+
			"WaitForNotification, so pgx's bufferNotifications appends every future site event "+
			"to a slice nobody drains, on somebody else's connection",
			stage, subscribed, len(held), gh496NotifyChannel)
	}
}

// TestGH496_CancelledListenerDoesNotLeakItsSubscriptionIntoThePool is the
// regression. It runs the real listener against real Postgres through the real
// application pool, cancels its context the way a narrowed-scope caller would,
// and then requires that no connection in the pool is still subscribed.
//
// Against the unfixed code (`defer conn.Release()`) this FAILS: the connection
// is returned healthy and subscribed. Against the fix (UNLISTEN on
// db.CleanupContext, Hijack+Close if that cannot be confirmed) it passes.
//
//	go test ./tests/ -run TestGH496_CancelledListenerDoesNotLeakItsSubscriptionIntoThePool -v -count=1
func TestGH496_CancelledListenerDoesNotLeakItsSubscriptionIntoThePool(t *testing.T) {
	pool := startPostgres(t) // wpmgr_app: NOSUPERUSER, NOBYPASSRLS
	tenant := seedTenant(t, pool, "gh496-leak-"+uuid.NewString()[:8])

	// Nothing is subscribed before the listener runs, so anything observed
	// afterwards is attributable to it.
	gh496AssertPoolIsClean(t, pool, "before the listener ran")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, unsubscribe, done := gh496StartListener(t, ctx, pool, tenant)
	defer unsubscribe()

	// This is the whole defect in one line: cancel a context that is NOT the
	// process signal context. Today only SIGTERM does this, which is why the
	// leak is contained in production; the listener must not depend on that.
	cancel()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the listener did not return within 30s of its context being cancelled; " +
			"its connection has not been released yet, so the assertion below would be racing it")
	}

	gh496AssertPoolIsClean(t, pool, "after a cancelled listener released its connection")
}

// TestGH496_ListenerStillDeliversAndSurvivesARestart is the over-fire guard. A
// fix that stops the leak by breaking delivery is worse than the leak, so this
// asserts the two things the fix must NOT cost:
//
//  1. a listener on a live context still receives notifications;
//  2. a SECOND listener started after the first one was cancelled still
//     receives them — which is where a wrong fix lands. UNLISTEN on the
//     caller's own (already-cancelled) context sends nothing and would leave
//     the leak in place; Hijack-on-every-release would churn the pool. (This
//     test does not distinguish UNLISTEN wpmgr_site_events from UNLISTEN * —
//     both clear the leaked subscription on this session either way; see
//     unsubscribe()'s doc comment in listener.go for why the targeted form is
//     preferred regardless.) The restarted listener runs on a pool whose
//     connections have been through the cleanup path at least once, so it is
//     the case that catches a cleanup which corrupts the session.
//
// It passes both before and after the fix — that is the point of it.
//
//	go test ./tests/ -run TestGH496_ListenerStillDeliversAndSurvivesARestart -v -count=1
func TestGH496_ListenerStillDeliversAndSurvivesARestart(t *testing.T) {
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "gh496-live-"+uuid.NewString()[:8])

	pub := events.NewPublisher(pool, domain.SystemClock{})

	// Pass 1: a listener on a live context delivers.
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	sub1, unsubscribe1, done1 := gh496StartListener(t, ctx1, pool, tenant)

	// Same shape as the readiness loop above, and the same fix: bound the
	// call that can block, not just the select that looks like it bounds the
	// wait. context.Background() here would let a wedged Publish sail past
	// the select's 30s deadline arm without ever reaching it.
	pubCtx1, pubCancel1 := context.WithTimeout(context.Background(), 30*time.Second)
	defer pubCancel1()
	if err := pub.Publish(pubCtx1, site.ConnectionEvent{
		TenantID: tenant,
		Type:     "gh496.first",
	}); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("the first publish did not return within its 30s budget — a publish " +
				"failure, not evidence the live listener failed to deliver: %v", err)
		}
		t.Fatalf("publish on the live listener: %v", err)
	}
	select {
	case ev := <-sub1:
		if ev.TenantID != tenant {
			t.Fatalf("the live listener delivered an event for tenant %s, want %s", ev.TenantID, tenant)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("a listener on a LIVE context failed to deliver a published event within 30s: " +
			"the fix has broken notification delivery, which is worse than the leak it fixes")
	}

	cancel1()
	unsubscribe1()
	select {
	case <-done1:
	case <-time.After(30 * time.Second):
		t.Fatal("the first listener did not return within 30s of cancellation")
	}

	// Pass 2: a fresh listener over the same pool — whose connections have now
	// been through the release path — still attaches and still delivers.
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	sub2, unsubscribe2, done2 := gh496StartListener(t, ctx2, pool, tenant)
	defer unsubscribe2()

	// Same fix as the first pass above: bound the publish itself.
	pubCtx2, pubCancel2 := context.WithTimeout(context.Background(), 30*time.Second)
	defer pubCancel2()
	if err := pub.Publish(pubCtx2, site.ConnectionEvent{
		TenantID: tenant,
		Type:     "gh496.second",
	}); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("the second publish did not return within its 30s budget — a publish " +
				"failure, not evidence the restarted listener failed to deliver: %v", err)
		}
		t.Fatalf("publish on the restarted listener: %v", err)
	}
	select {
	case <-sub2:
	case <-time.After(30 * time.Second):
		t.Fatal("the RESTARTED listener failed to deliver a published event within 30s. The " +
			"cleanup on the previous release left the pooled connection unable to LISTEN again")
	}

	cancel2()
	select {
	case <-done2:
	case <-time.After(30 * time.Second):
		t.Fatal("the restarted listener did not return within 30s of cancellation")
	}

	gh496AssertPoolIsClean(t, pool, "after two listener lifecycles")
}
