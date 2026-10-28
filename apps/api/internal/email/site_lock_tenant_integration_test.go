// site_lock_tenant_integration_test.go: GH #968, the per-site email lock's
// admission on one instance, against a real Postgres as wpmgr_app. One
// tenant's slow site must hold back only that tenant's email changes on the
// instance, and the locked operations together must leave the pool room for
// every other request. The harness is repo_site_scope_integration_test.go's;
// it skips without Docker.
package email

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// stallingAgent answers at once, except a sync for stallSite, which waits for
// release: a site whose agent is slow, offline behind a slow dial, or
// deliberately stalling. A site is not trusted to answer quickly.
type stallingAgent struct {
	orderedAgent
	stallSite uuid.UUID
	entered   chan struct{}
	release   chan struct{}
}

func (a *stallingAgent) SyncEmailConfig(ctx context.Context, siteID uuid.UUID, url string, req agentcmd.EmailConfigRequest) (agentcmd.EmailConfigResult, error) {
	if siteID == a.stallSite {
		close(a.entered)
		<-a.release
		return agentcmd.EmailConfigResult{OK: true}, nil
	}
	return a.orderedAgent.SyncEmailConfig(ctx, siteID, url, req)
}

// instanceLocker is one API instance's locker over pool, with its own
// admission, the shape production builds.
func instanceLocker(pool *db.Pool, wait time.Duration) *pgSiteLocker {
	return &pgSiteLocker{pool: pool, slots: newEmailSlots(instanceEmailCap), wait: wait, poll: 20 * time.Millisecond}
}

// addSiteRow gives tenant its own site with its own smtp config in the
// fixture's repository.
func addSiteRow(f *removeFixture, tenant uuid.UUID) uuid.UUID {
	site := uuid.New()
	sid := site
	f.repo.site[siteKey(tenant, site)] = Config{
		ID: uuid.New(), TenantID: tenant, SiteID: &sid, Provider: "smtp",
		FromAddress: "b@example.com", Config: map[string]any{"host": "smtp.b.example"},
		SecretSet: true, UpdatedAt: readAt,
	}
	return site
}

func TestSiteEmailLockLetsOtherTenantsChangeEmailWhileOneTenantsSiteStalls(t *testing.T) {
	app, _ := startScopeTestPostgres(t)
	f := newRemoveFixture(t) // tenant A, site A, with its own row
	tenantB := uuid.New()
	siteB := addSiteRow(f, tenantB)
	siteA2 := addSiteRow(f, f.tenantID) // tenant A's second site

	agent := &stallingAgent{orderedAgent: orderedAgent{log: f.log}, stallSite: f.siteID,
		entered: make(chan struct{}), release: make(chan struct{})}
	f.svc.SetAgentClient(agent, &fakeSiteLookup{})
	f.svc.locks = instanceLocker(app, 500*time.Millisecond)
	ctx := context.Background()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = f.svc.SyncConfigToAgent(ctx, f.tenantID, f.siteID) // tenant A: Sync to site
	}()
	released := false
	release := func() {
		if !released {
			released = true
			close(agent.release)
			<-done
		}
	}
	t.Cleanup(release)
	select {
	case <-agent.entered: // tenant A's push is on the wire to its stalling site
	case <-time.After(5 * time.Second):
		t.Fatal("tenant A's sync never reached its agent")
	}

	// Tenant B, on the same instance, is not held back by tenant A's site.
	secret := "b-secret"
	sb := siteB
	start := time.Now()
	if _, err := f.svc.UpsertSiteConfig(ctx, UpsertInput{TenantID: tenantB, SiteID: &sb, Provider: "smtp",
		FromAddress: "b@example.com", Config: map[string]any{"host": "smtp.b.example"},
		SecretRaw: &secret, RetentionDays: 30}); err != nil {
		t.Fatalf("tenant B's save while tenant A's site stalls: %v", err)
	}
	if res, err := f.svc.SyncConfigToAgent(ctx, tenantB, siteB); err != nil || !res.OK {
		t.Fatalf("tenant B's Sync to site while tenant A's site stalls = %+v, %v", res, err)
	}
	if _, err := f.svc.RemoveSiteConfig(ctx, tenantB, siteB); err != nil {
		t.Fatalf("tenant B's removal while tenant A's site stalls: %v", err)
	}
	if waited := time.Since(start); waited > 400*time.Millisecond {
		t.Fatalf("tenant B's three changes took %v; they waited on tenant A", waited)
	}

	// Tenant A's own next change on this instance, on another of its sites,
	// waits, then answers busy.
	sa2 := siteA2
	start = time.Now()
	_, err := f.svc.UpsertSiteConfig(ctx, UpsertInput{TenantID: f.tenantID, SiteID: &sa2, Provider: "smtp",
		FromAddress: "a2@example.com", Config: map[string]any{"host": "smtp.a2.example"},
		SecretRaw: &secret, RetentionDays: 30})
	de, ok := domain.AsDomain(err)
	if !ok || de.Kind != domain.KindConflict || de.Code != "email_change_in_progress" {
		t.Fatalf("tenant A's second change while its first runs = %v, want 409 email_change_in_progress", err)
	}
	if waited := time.Since(start); waited < 500*time.Millisecond {
		t.Fatalf("tenant A's second change gave up after %v, before its wait ran out", waited)
	}

	// Once tenant A's sync finishes, its next change goes through.
	release()
	if _, err := f.svc.UpsertSiteConfig(ctx, UpsertInput{TenantID: f.tenantID, SiteID: &sa2, Provider: "smtp",
		FromAddress: "a2@example.com", Config: map[string]any{"host": "smtp.a2.example"},
		SecretRaw: &secret, RetentionDays: 30}); err != nil {
		t.Fatalf("tenant A's change after its sync finished: %v", err)
	}
}

// With instanceEmailCap operations holding their locks, for as many tenants,
// a further tenant answers busy, and the pool, sized as production sizes it,
// still serves other requests: two at once.
func TestSiteEmailLockCapsAnInstanceAndLeavesThePoolTwoConnections(t *testing.T) {
	app, _ := startScopeTestPostgres(t)
	ctx := context.Background()

	cfg := app.Config()
	cfg.MinConns, cfg.MaxConns = 0, 5 // db.ConnectApp's MaxConns
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open a pool of five: %v", err)
	}
	t.Cleanup(pool.Close)
	l := instanceLocker(&db.Pool{Pool: pool}, 300*time.Millisecond)

	for i := 0; i < instanceEmailCap; i++ {
		release, err := l.lockSite(ctx, uuid.New(), uuid.New())
		if err != nil {
			t.Fatalf("operation %d of %d, each for its own tenant: %v", i+1, instanceEmailCap, err)
		}
		t.Cleanup(release)
	}

	// A further tenant waits, then answers busy.
	gotD := make(chan error, 1)
	start := time.Now()
	go func() {
		release, err := l.lockSite(ctx, uuid.New(), uuid.New())
		if err == nil {
			release()
		}
		gotD <- err
	}()

	// Meanwhile two plain requests hold a connection each, at once.
	qctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	c1, err := pool.Acquire(qctx)
	if err != nil {
		t.Fatalf("a first plain request found no free connection: %v", err)
	}
	defer c1.Release()
	c2, err := pool.Acquire(qctx)
	if err != nil {
		t.Fatalf("a second plain request found no free connection: %v", err)
	}
	defer c2.Release()
	var one int
	if err := c2.QueryRow(qctx, `SELECT 1`).Scan(&one); err != nil {
		t.Fatalf("a plain query: %v", err)
	}

	select {
	case err := <-gotD:
		if !errors.Is(err, errSiteEmailBusy) {
			t.Fatalf("a further tenant at the cap = %v, want errSiteEmailBusy", err)
		}
		if waited := time.Since(start); waited < 300*time.Millisecond {
			t.Fatalf("the further tenant gave up after %v, before its wait ran out", waited)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the further tenant never answered")
	}
	if got := l.slots.inUse(); got != instanceEmailCap {
		t.Fatalf("%d slot(s) in use, want %d", got, instanceEmailCap)
	}
}

// A request for a site locked from another instance holds no slot while it
// waits. Here the waiting instance has room for one operation only, so if the
// waiter kept its slot between tries, another tenant's change there would wait
// it out and answer busy.
func TestSiteEmailLockHoldsNoSlotWhileWaitingForASiteLockedElsewhere(t *testing.T) {
	app, _ := startScopeTestPostgres(t)
	ctx := context.Background()
	tenant, site := uuid.New(), uuid.New()

	other := instanceLocker(app, time.Second) // another instance
	holdElsewhere, err := other.lockSite(ctx, tenant, site)
	if err != nil {
		t.Fatalf("the other instance's operation: %v", err)
	}
	t.Cleanup(holdElsewhere)

	here := instanceLocker(app, time.Second)
	here.slots = newEmailSlots(1)
	waiting := make(chan error, 1)
	go func() {
		release, err := here.lockSite(ctx, tenant, site)
		if err == nil {
			release()
		}
		waiting <- err
	}()
	time.Sleep(100 * time.Millisecond) // the waiter is polling

	start := time.Now()
	release, err := here.lockSite(ctx, uuid.New(), uuid.New())
	if err != nil {
		t.Fatalf("another tenant on the waiting instance: %v", err)
	}
	if waited := time.Since(start); waited > 500*time.Millisecond {
		t.Fatalf("another tenant took %v to start behind a waiter", waited)
	}
	release()

	select {
	case err := <-waiting:
		if !errors.Is(err, errSiteEmailBusy) {
			t.Fatalf("the waiter for the site locked elsewhere = %v, want errSiteEmailBusy", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiter never answered")
	}
	if got := here.slots.inUse(); got != 0 {
		t.Fatalf("%d slot(s) in use after the waiter gave up, want 0", got)
	}
}
