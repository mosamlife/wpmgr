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
	locker := func(wait time.Duration) *pgSiteLocker {
		return &pgSiteLocker{pool: app, wait: wait, poll: 20 * time.Millisecond}
	}

	// The service built on the real pool uses the Postgres locker.
	svc := NewService(NewRepo(app), nil, nil)
	if _, ok := svc.locks.(*pgSiteLocker); !ok {
		t.Fatalf("a Service on a real pool uses %T, want *pgSiteLocker", svc.locks)
	}

	holdA, err := locker(time.Second).lockSite(ctx, f.siteA)
	if err != nil {
		t.Fatalf("first operation on site A: %v", err)
	}
	if got := advisoryLocks(); got != 1 {
		t.Fatalf("%d advisory lock(s) held, want 1", got)
	}

	// A second operation on site A waits, and gets errSiteEmailBusy once the
	// wait runs out.
	start := time.Now()
	if _, err := locker(300*time.Millisecond).lockSite(ctx, f.siteA); !errors.Is(err, errSiteEmailBusy) {
		t.Fatalf("second operation on site A = %v, want errSiteEmailBusy", err)
	}
	if waited := time.Since(start); waited < 300*time.Millisecond {
		t.Fatalf("the second operation gave up after %v, before its wait ran out", waited)
	}

	// An operation on site B does not wait for site A.
	start = time.Now()
	holdB, err := locker(300*time.Millisecond).lockSite(ctx, f.siteB)
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
	release, err := locker(time.Second).lockSite(cctx, f.siteA)
	if err != nil {
		t.Fatalf("lock under a live context: %v", err)
	}
	cancel()
	release()
	release() // a second release is a no-op
	if got := advisoryLocks(); got != 0 {
		t.Fatalf("%d advisory lock(s) left after every release, want 0", got)
	}
	again, err := locker(200*time.Millisecond).lockSite(ctx, f.siteA)
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
