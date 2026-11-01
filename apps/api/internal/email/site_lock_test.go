// site_lock_test.go: GH #968, the per-site email lock. These drive the
// in-process locker a Service without a pool uses; the Postgres locker is
// proved as wpmgr_app in site_lock_integration_test.go.
package email

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// holdSiteLock takes the site's lock as another operation would, with a short
// wait for everything that comes after it.
func holdSiteLock(t *testing.T, f *removeFixture) func() {
	t.Helper()
	f.svc.locks = newLocalSiteLocker(50 * time.Millisecond)
	release, err := f.svc.locks.lockSite(context.Background(), f.tenantID, f.siteID)
	if err != nil {
		t.Fatalf("take the site's lock: %v", err)
	}
	t.Cleanup(release)
	return release
}

func TestRemoveSiteConfigAnswers409WhenAnotherChangeHoldsTheSite(t *testing.T) {
	f := newRemoveFixture(t)
	holdSiteLock(t, f)

	_, err := f.remove()
	de := wantDomainError(t, err, domain.KindConflict, "email_change_in_progress")
	if de.Message != "Another email change is in progress. Try again in a moment." {
		t.Fatalf("message = %q", de.Message)
	}
	if len(f.log.events) != 0 {
		t.Fatalf("a removal that never got the lock still did %q", f.log.events)
	}
	if _, ok := f.siteRow(); !ok {
		t.Fatal("a removal that never got the lock deleted the row")
	}
}

func TestPushesAnswerBusyWhenAnotherChangeHoldsTheSite(t *testing.T) {
	f := newRemoveFixture(t)
	holdSiteLock(t, f)
	before, _ := f.siteRow()

	sid := f.siteID
	_, err := f.svc.UpsertSiteConfig(context.Background(), UpsertInput{
		TenantID: f.tenantID, SiteID: &sid, Provider: "smtp", FromAddress: "new@example.com",
		Config: map[string]any{"host": "smtp.site.example", "port": 587, "username": "site"}, RetentionDays: 30,
	})
	wantDomainError(t, err, domain.KindConflict, "email_change_in_progress")
	if after, _ := f.siteRow(); after.FromAddress != before.FromAddress {
		t.Fatalf("a save that never got the lock wrote the row: from %q -> %q", before.FromAddress, after.FromAddress)
	}

	res, err := f.svc.SyncConfigToAgent(context.Background(), f.tenantID, f.siteID)
	if err != nil || res.OK || res.Detail != siteBusyMessage {
		t.Fatalf("sync = %+v, %v; want ok=false with %q", res, err, siteBusyMessage)
	}
	res, err = f.svc.SendTest(context.Background(), f.tenantID, f.siteID, TestSendInput{To: "ops@example.com"})
	if err != nil || res.OK || res.Detail != siteBusyMessage {
		t.Fatalf("send test = %+v, %v; want ok=false with %q", res, err, siteBusyMessage)
	}
	if len(f.log.events) != 0 {
		t.Fatalf("pushes that never got the lock still reached the agent: %q", f.log.events)
	}
}

func connectionInput(f *removeFixture, configID uuid.UUID) ConnectionUpsertInput {
	sid := f.siteID
	return ConnectionUpsertInput{
		TenantID: f.tenantID, SiteID: &sid, ConfigID: configID, ConnectionKey: "billing",
		Provider: "smtp", FromAddress: "billing@example.com",
		Config: map[string]any{"host": "smtp.billing.example", "port": 587, "username": "billing"},
	}
}

func TestUpsertConnectionWaitsOutARemovalInProgress(t *testing.T) {
	f := newRemoveFixture(t)
	row, _ := f.siteRow()
	holdSiteLock(t, f)

	_, err := f.svc.UpsertConnection(context.Background(), connectionInput(f, row.ID))
	wantDomainError(t, err, domain.KindConflict, "email_change_in_progress")
	if f.repo.connUpserts != 0 {
		t.Fatalf("a connection save that never got the lock wrote %d row(s)", f.repo.connUpserts)
	}
}

func TestUpsertConnectionRefusesAParentARemovalDeleted(t *testing.T) {
	f := newRemoveFixture(t)
	row, _ := f.siteRow() // what the route resolved before the removal ran
	if _, err := f.remove(); err != nil {
		t.Fatalf("removal: %v", err)
	}

	_, err := f.svc.UpsertConnection(context.Background(), connectionInput(f, row.ID))
	de := wantDomainError(t, err, domain.KindConflict, "email_config_changed")
	if de.Message != connectionParentChangedMessage {
		t.Fatalf("message = %q", de.Message)
	}
	if f.repo.connUpserts != 0 {
		t.Fatalf("a connection was saved under a parent that is gone (%d write(s))", f.repo.connUpserts)
	}
}

func TestUpsertConnectionSavesUnderTheLock(t *testing.T) {
	f := newRemoveFixture(t)
	row, _ := f.siteRow()
	conn, err := f.svc.UpsertConnection(context.Background(), connectionInput(f, row.ID))
	if err != nil {
		t.Fatalf("UpsertConnection: %v", err)
	}
	if conn.ConfigID != row.ID || f.repo.connUpserts != 1 {
		t.Fatalf("saved %+v with %d write(s), want one under config %s", conn, f.repo.connUpserts, row.ID)
	}
	// The lock was released: a removal goes straight through.
	if _, err := f.remove(); err != nil {
		t.Fatalf("removal after the save: %v", err)
	}
}

// A tenant's second operation on an instance where its first is running waits,
// then answers busy, and never asks the pool for a connection: this locker has
// no pool, so asking would panic. A cancelled waiter stops waiting.
func TestPgSiteLockerHoldsATenantsSecondOperationWithoutAConnection(t *testing.T) {
	l := &pgSiteLocker{slots: newEmailSlots(instanceEmailCap), wait: 50 * time.Millisecond, poll: 10 * time.Millisecond}
	tenant := uuid.New()
	if !l.slots.tryTake(tenant) { // the tenant's operation on another site is running
		t.Fatal("take the tenant's slot")
	}

	start := time.Now()
	if _, err := l.lockSite(context.Background(), tenant, uuid.New()); !errors.Is(err, errSiteEmailBusy) {
		t.Fatalf("the tenant's second operation = %v, want errSiteEmailBusy", err)
	}
	if waited := time.Since(start); waited < 50*time.Millisecond {
		t.Fatalf("gave up after %v, before the wait ran out", waited)
	}
	if got := l.slots.inUse(); got != 1 {
		t.Fatalf("%d slot(s) in use after a waiter that never got one, want 1", got)
	}

	l.wait = time.Minute
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := l.lockSite(cctx, tenant, uuid.New()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled waiter = %v, want context.Canceled", err)
	}
}

// With instanceEmailCap operations running, for as many tenants, a further
// tenant waits, then answers busy, without asking the pool for a connection.
func TestPgSiteLockerCapsTheInstanceAcrossTenants(t *testing.T) {
	l := &pgSiteLocker{slots: newEmailSlots(instanceEmailCap), wait: 50 * time.Millisecond, poll: 10 * time.Millisecond}
	for i := 0; i < instanceEmailCap; i++ {
		if !l.slots.tryTake(uuid.New()) {
			t.Fatalf("slot %d of %d refused", i+1, instanceEmailCap)
		}
	}
	if l.slots.tryTake(uuid.New()) {
		t.Fatalf("a slot past the cap of %d was given", instanceEmailCap)
	}
	if _, err := l.lockSite(context.Background(), uuid.New(), uuid.New()); !errors.Is(err, errSiteEmailBusy) {
		t.Fatalf("a further tenant at the cap = %v, want errSiteEmailBusy", err)
	}
	if got := l.slots.inUse(); got != instanceEmailCap {
		t.Fatalf("%d slot(s) in use, want %d", got, instanceEmailCap)
	}
}

// stuckAgent never answers a config command on its own; it returns only when
// the call's context ends, as a site that accepted the connection and then
// stalled would.
type stuckAgent struct{ orderedAgent }

func (a *stuckAgent) SyncEmailConfig(ctx context.Context, _ uuid.UUID, _ string, _ agentcmd.EmailConfigRequest) (agentcmd.EmailConfigResult, error) {
	<-ctx.Done()
	return agentcmd.EmailConfigResult{}, ctx.Err()
}

func (a *stuckAgent) ClearEmailConfig(ctx context.Context, _ uuid.UUID, _ string) (agentcmd.ClearEmailConfigResult, error) {
	<-ctx.Done()
	return agentcmd.ClearEmailConfigResult{}, ctx.Err()
}

// An agent call made under the site's lock is cut off at the locked-call
// timeout, so a stalled site holds the lock no longer than that, and the
// next change on the site starts.
func TestLockedAgentCallsAreCutOffAtTheTimeout(t *testing.T) {
	if lockedAgentCallTimeout > 15*time.Second {
		t.Fatalf("lockedAgentCallTimeout = %v, want at most 15s", lockedAgentCallTimeout)
	}
	f := newRemoveFixture(t)
	f.svc.SetAgentClient(&stuckAgent{orderedAgent{log: f.log}}, &fakeSiteLookup{})
	f.svc.lockedCallTimeout = 50 * time.Millisecond
	ctx := context.Background()

	start := time.Now()
	res, err := f.svc.SyncConfigToAgent(ctx, f.tenantID, f.siteID)
	if err != nil || res.OK {
		t.Fatalf("sync to a stalled site = %+v, %v; want ok=false", res, err)
	}
	if waited := time.Since(start); waited > 2*time.Second {
		t.Fatalf("sync to a stalled site held on for %v", waited)
	}

	start = time.Now()
	_, err = f.remove()
	wantDomainError(t, err, domain.KindServiceUnavailable, "email_agent_clear_failed")
	if waited := time.Since(start); waited > 2*time.Second {
		t.Fatalf("a removal on a stalled site held on for %v", waited)
	}
	if _, ok := f.siteRow(); !ok {
		t.Fatal("a removal whose clear was never confirmed deleted the row")
	}

	// The lock was let go: the next change on the site starts at once.
	f.svc.SetAgentClient(f.agent, &fakeSiteLookup{})
	if _, err := f.remove(); err != nil {
		t.Fatalf("a removal after the stalled calls were cut off: %v", err)
	}
}

func TestEmailSlotsAdmitOnePerTenant(t *testing.T) {
	s := newEmailSlots(3)
	a, b := uuid.New(), uuid.New()
	if !s.tryTake(a) || s.tryTake(a) {
		t.Fatal("tenant A: want its first slot given and its second refused")
	}
	if !s.tryTake(b) {
		t.Fatal("tenant B was refused while only tenant A ran")
	}
	s.give(a)
	if !s.tryTake(a) {
		t.Fatal("tenant A was refused after it gave its slot back")
	}
	s.give(a)
	s.give(b)
	if got := s.inUse(); got != 0 {
		t.Fatalf("%d slot(s) in use after every give, want 0", got)
	}
}

func TestLocalSiteLockerExcludesOneSiteOnly(t *testing.T) {
	l := newLocalSiteLocker(50 * time.Millisecond)
	ctx := context.Background()
	tenant, a, b := uuid.New(), uuid.New(), uuid.New()

	releaseA, err := l.lockSite(ctx, tenant, a)
	if err != nil {
		t.Fatalf("lock A: %v", err)
	}
	if _, err := l.lockSite(ctx, tenant, a); err != errSiteEmailBusy {
		t.Fatalf("second lock on A = %v, want errSiteEmailBusy", err)
	}
	releaseB, err := l.lockSite(ctx, tenant, b)
	if err != nil {
		t.Fatalf("site B waited on site A: %v", err)
	}
	releaseB()

	// A panic inside the operation still releases the lock, and a second
	// release is a no-op.
	func() {
		defer func() { _ = recover() }()
		defer releaseA()
		panic("operation failed")
	}()
	releaseA()
	again, err := l.lockSite(ctx, tenant, a)
	if err != nil {
		t.Fatalf("lock A after the panic released it: %v", err)
	}
	again()

	// A waiter whose request is cancelled stops waiting.
	hold, _ := l.lockSite(ctx, tenant, a)
	defer hold()
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := newLocalSiteLocker(time.Minute).lockSite(cctx, tenant, a); err != nil {
		t.Fatalf("a fresh locker is not held: %v", err)
	}
	if _, err := l.lockSite(cctx, tenant, a); err != context.Canceled {
		t.Fatalf("cancelled waiter = %v, want context.Canceled", err)
	}
}
