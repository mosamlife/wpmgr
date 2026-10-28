// site_lock_integration_test.go: GH #968, the per-site email lock against a
// real Postgres, as the non-superuser wpmgr_app role every install runs as.
// Each operation takes its own pooled connection, exactly as two API
// instances would, so a lock that only excluded within one session would
// fail here. The harness is repo_site_scope_integration_test.go's; it skips
// without Docker.
package email

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

func TestSiteEmailLockSerialisesOneSiteAcrossConnections(t *testing.T) {
	app, admin := startScopeTestPostgres(t)
	f := seedScopeFixture(t, admin)
	ctx := context.Background()

	advisoryLocks := func() int {
		t.Helper()
		var n int
		if err := admin.QueryRow(ctx,
			`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND granted`).Scan(&n); err != nil {
			t.Fatalf("count advisory locks: %v", err)
		}
		return n
	}
	// Each locker is its own instance, with its own slot, so only the advisory
	// lock stands between them.
	locker := func(wait time.Duration) *pgSiteLocker {
		return &pgSiteLocker{pool: app, gate: make(chan struct{}, 1), wait: wait, poll: 20 * time.Millisecond}
	}
	// take locks a site and registers the release, so a failed assertion
	// cannot leave a pinned connection that stops the harness closing its pool.
	take := func(l *pgSiteLocker, c context.Context, site uuid.UUID) (func(), error) {
		release, err := l.lockSite(c, site)
		if err == nil {
			t.Cleanup(release)
		}
		return release, err
	}

	// The service built on the real pool uses the Postgres locker.
	svc := NewService(NewRepo(app), nil, nil)
	if _, ok := svc.locks.(*pgSiteLocker); !ok {
		t.Fatalf("a Service on a real pool uses %T, want *pgSiteLocker", svc.locks)
	}

	holdA, err := take(locker(time.Second), ctx, f.siteA)
	if err != nil {
		t.Fatalf("first operation on site A: %v", err)
	}
	if got := advisoryLocks(); got != 1 {
		t.Fatalf("%d advisory lock(s) held, want 1", got)
	}

	// A second operation on site A waits, and gets errSiteEmailBusy once the
	// wait runs out.
	start := time.Now()
	if _, err := take(locker(300*time.Millisecond), ctx, f.siteA); !errors.Is(err, errSiteEmailBusy) {
		t.Fatalf("second operation on site A = %v, want errSiteEmailBusy", err)
	}
	if waited := time.Since(start); waited < 300*time.Millisecond {
		t.Fatalf("the second operation gave up after %v, before its wait ran out", waited)
	}

	// An operation on site B does not wait for site A.
	start = time.Now()
	holdB, err := take(locker(300*time.Millisecond), ctx, f.siteB)
	if err != nil {
		t.Fatalf("site B waited on site A: %v", err)
	}
	if waited := time.Since(start); waited > 250*time.Millisecond {
		t.Fatalf("site B took %v to lock while site A was held", waited)
	}
	holdB()

	// Through the service: a removal of site A while another change holds it
	// answers 409 and removes nothing.
	svc.locks = locker(200 * time.Millisecond)
	_, rmErr := svc.RemoveSiteConfig(orgMemberCtx(f.tenant), f.tenant, f.siteA)
	de, ok := domain.AsDomain(rmErr)
	if !ok || de.Kind != domain.KindConflict || de.Code != "email_change_in_progress" {
		t.Fatalf("removal while site A is held = %v, want 409 email_change_in_progress", rmErr)
	}
	var rows int
	if err := admin.QueryRow(ctx,
		`SELECT count(*) FROM site_email_config WHERE tenant_id = $1 AND site_id = $2`, f.tenant, f.siteA).Scan(&rows); err != nil {
		t.Fatalf("count site A rows: %v", err)
	}
	if rows != 1 {
		t.Fatalf("a removal that never got the lock left %d row(s), want 1", rows)
	}

	// A waiter gets the lock as soon as the holder lets go.
	got := make(chan error, 1)
	go func() {
		release, err := locker(5*time.Second).lockSite(ctx, f.siteA)
		if err == nil {
			release()
		}
		got <- err
	}()
	time.Sleep(100 * time.Millisecond)
	holdA()
	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("the waiter did not get site A after it was released: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiter never got site A")
	}

	// A holder whose request was cancelled still unlocks.
	cctx, cancel := context.WithCancel(ctx)
	release, err := take(locker(time.Second), cctx, f.siteA)
	if err != nil {
		t.Fatalf("lock under a live context: %v", err)
	}
	cancel()
	release()
	release() // a second release is a no-op
	if got := advisoryLocks(); got != 0 {
		t.Fatalf("%d advisory lock(s) left after every release, want 0", got)
	}
	again, err := take(locker(200*time.Millisecond), ctx, f.siteA)
	if err != nil {
		t.Fatalf("site A after a cancelled holder released it: %v", err)
	}
	again()

	// The role is the application role, not one that skips RLS.
	var super, bypass bool
	if err := app.QueryRow(ctx,
		`SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&super, &bypass); err != nil {
		t.Fatalf("read the role: %v", err)
	}
	if super || bypass {
		t.Fatalf("the app pool connects as a superuser (%v) or BYPASSRLS (%v) role", super, bypass)
	}
	var _ *db.Pool = app
}

// The lock pins one pooled connection for the whole operation, agent calls
// included, so one instance runs one such operation at a time: a second, on
// any site, waits for the first without holding a connection, and answers
// busy when the wait runs out. Without that, a few email changes at once
// would hold most of the instance's pool for the length of their agent calls.

func TestSiteEmailLockRunsOneOperationAtATimePerInstance(t *testing.T) {
	app, admin := startScopeTestPostgres(t)
	f := seedScopeFixture(t, admin)
	ctx := context.Background()

	// One instance: the locker the Service builds on its pool, with a short wait.
	svc := NewService(NewRepo(app), nil, nil)
	l, ok := svc.locks.(*pgSiteLocker)
	if !ok {
		t.Fatalf("a Service on a real pool uses %T, want *pgSiteLocker", svc.locks)
	}
	l.wait, l.poll = 300*time.Millisecond, 20*time.Millisecond

	holdA, err := l.lockSite(ctx, f.siteA)
	if err != nil {
		t.Fatalf("first operation, on site A: %v", err)
	}
	t.Cleanup(holdA)

	// A second operation, on another site, waits and then answers busy.
	start := time.Now()
	releaseB, err := l.lockSite(ctx, f.siteB)
	if err == nil {
		releaseB()
	}
	if !errors.Is(err, errSiteEmailBusy) {
		t.Fatalf("site B on the same instance while site A runs = %v, want errSiteEmailBusy", err)
	}
	if waited := time.Since(start); waited < 300*time.Millisecond {
		t.Fatalf("site B gave up after %v, before its wait ran out", waited)
	}

	// Through the service it is the 409, worded so it is true for either cause.
	_, rmErr := svc.RemoveSiteConfig(orgMemberCtx(f.tenant), f.tenant, f.siteB)
	de, ok := domain.AsDomain(rmErr)
	if !ok || de.Kind != domain.KindConflict || de.Code != "email_change_in_progress" {
		t.Fatalf("removal on site B while site A runs = %v, want 409 email_change_in_progress", rmErr)
	}
	if de.Message != "Another email change is in progress. Try again in a moment." {
		t.Fatalf("message = %q", de.Message)
	}

	// Once the first operation is done, the next one on this instance proceeds.
	holdA()
	releaseB, err = l.lockSite(ctx, f.siteB)
	if err != nil {
		t.Fatalf("site B after site A finished: %v", err)
	}
	releaseB()
}

func TestSiteEmailLockLeavesThePoolToOtherRequests(t *testing.T) {
	app, admin := startScopeTestPostgres(t)
	f := seedScopeFixture(t, admin)
	ctx := context.Background()

	// A pool of two, as wpmgr_app: room for one operation's pinned connection
	// and one more.
	cfg := app.Config()
	cfg.MinConns, cfg.MaxConns = 0, 2
	small, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open a pool of two: %v", err)
	}
	t.Cleanup(small.Close)
	svc := NewService(NewRepo(&db.Pool{Pool: small}), nil, nil)
	l, ok := svc.locks.(*pgSiteLocker)
	if !ok {
		t.Fatalf("a Service on a real pool uses %T, want *pgSiteLocker", svc.locks)
	}
	l.wait, l.poll = time.Second, 20*time.Millisecond

	holdA, err := l.lockSite(ctx, f.siteA)
	if err != nil {
		t.Fatalf("first operation, on site A: %v", err)
	}
	t.Cleanup(holdA)

	// A second operation, on another site, starts while the first runs. If it
	// gets its lock it keeps it, as one in its agent calls would, until the
	// test ends.
	finish := make(chan struct{})
	gotB := make(chan error, 1)
	go func() {
		release, err := l.lockSite(ctx, f.siteB)
		gotB <- err
		if err == nil {
			<-finish
			release()
		}
	}()
	t.Cleanup(func() { close(finish) })
	time.Sleep(200 * time.Millisecond)

	// A plain query on the pool still runs.
	qctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var one int
	if err := small.QueryRow(qctx, `SELECT 1`).Scan(&one); err != nil {
		t.Fatalf("a plain query found no free connection while email operations ran: %v", err)
	}

	// The second operation answered busy once its wait ran out.
	select {
	case err := <-gotB:
		if !errors.Is(err, errSiteEmailBusy) {
			t.Fatalf("site B while site A runs = %v, want errSiteEmailBusy", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("site B never answered")
	}
}
