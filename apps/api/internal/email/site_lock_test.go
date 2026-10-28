// site_lock_test.go: GH #968, the per-site email lock. These drive the
// in-process locker a Service without a pool uses; the Postgres locker is
// proved as wpmgr_app in site_lock_integration_test.go.
package email

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// holdSiteLock takes the site's lock as another operation would, with a short
// wait for everything that comes after it.
func holdSiteLock(t *testing.T, f *removeFixture) func() {
	t.Helper()
	f.svc.locks = newLocalSiteLocker(50 * time.Millisecond)
	release, err := f.svc.locks.lockSite(context.Background(), f.siteID)
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
	if de.Message != "Another email change for this site is in progress. Try again in a moment." {
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

func TestLocalSiteLockerExcludesOneSiteOnly(t *testing.T) {
	l := newLocalSiteLocker(50 * time.Millisecond)
	ctx := context.Background()
	a, b := uuid.New(), uuid.New()

	releaseA, err := l.lockSite(ctx, a)
	if err != nil {
		t.Fatalf("lock A: %v", err)
	}
	if _, err := l.lockSite(ctx, a); err != errSiteEmailBusy {
		t.Fatalf("second lock on A = %v, want errSiteEmailBusy", err)
	}
	releaseB, err := l.lockSite(ctx, b)
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
	again, err := l.lockSite(ctx, a)
	if err != nil {
		t.Fatalf("lock A after the panic released it: %v", err)
	}
	again()

	// A waiter whose request is cancelled stops waiting.
	hold, _ := l.lockSite(ctx, a)
	defer hold()
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := newLocalSiteLocker(time.Minute).lockSite(cctx, a); err != nil {
		t.Fatalf("a fresh locker is not held: %v", err)
	}
	if _, err := l.lockSite(cctx, a); err != context.Canceled {
		t.Fatalf("cancelled waiter = %v, want context.Canceled", err)
	}
}
