package backup

// gh282_site_state_guard_test.go: GH #282 regression, archiving a site did
// not stop its backup schedule (scheduled backups kept firing/failing/
// emailing for an unmanaged site). The scheduler-query guard itself lives in
// SQL (repo.go: ListDueSchedules / ClaimAndAdvanceDueSchedules /
// HealOverdueSchedules all now JOIN sites + tenants and skip archived/revoked
// sites and soft-deleted tenants) and is covered by the real-Postgres
// integration tests in apps/api/tests/backup_gh282_scheduler_guard_test.go.
//
// This file covers the two in-memory-fake-testable defense-in-depth guards
// that sit above the SQL layer:
//  1. CreateBackup (manual "run now") refuses an archived or revoked site.
//  2. sendBackupEmail is suppressed for an archived or revoked site, even
//     when notify_on_completion is configured (covers a manual backup or an
//     in-flight scheduled backup that fails AFTER the site was archived).
//
// All tests use in-memory fakes and a deterministic fakeClock; no database.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// ---------------------------------------------------------------------------
// CreateBackup refuses a manual backup on an archived/revoked site.
// ---------------------------------------------------------------------------

func TestCreateBackup_ArchivedSite_Refused(t *testing.T) {
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	tenantID, siteID := uuid.New(), uuid.New()

	repo := newSchedulerTestRepo()
	svc := &Service{
		repo: repo,
		sites: fakeSites{info: SiteInfo{
			Enrolled: true, AgeRecipient: "age1test",
			ConnectionState: ConnStateArchived,
		}},
		clock:    fakeClock{t: now},
		enqueuer: &recordingEnqueuer{},
	}

	_, err := svc.CreateBackup(context.Background(), tenantID, siteID, uuid.New(), KindFull)
	if err == nil {
		t.Fatal("CreateBackup on an archived site: expected error, got nil")
	}
	var de *domain.Error
	if !asError(err, &de) {
		t.Fatalf("expected a domain.Error, got: %v", err)
	}
	if de.Kind != domain.KindValidation {
		t.Errorf("expected KindValidation, got %v", de.Kind)
	}
	if de.Code != "site_not_manageable" {
		t.Errorf("expected code site_not_manageable, got %q", de.Code)
	}
}

func TestCreateBackup_RevokedSite_Refused(t *testing.T) {
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	tenantID, siteID := uuid.New(), uuid.New()

	repo := newSchedulerTestRepo()
	svc := &Service{
		repo: repo,
		sites: fakeSites{info: SiteInfo{
			Enrolled: true, AgeRecipient: "age1test",
			ConnectionState: ConnStateRevoked,
		}},
		clock:    fakeClock{t: now},
		enqueuer: &recordingEnqueuer{},
	}

	_, err := svc.CreateBackup(context.Background(), tenantID, siteID, uuid.New(), KindFull)
	if err == nil {
		t.Fatal("CreateBackup on a revoked site: expected error, got nil")
	}
	var de *domain.Error
	if !asError(err, &de) {
		t.Fatalf("expected a domain.Error, got: %v", err)
	}
	if de.Kind != domain.KindValidation {
		t.Errorf("expected KindValidation, got %v", de.Kind)
	}
	if de.Code != "site_not_manageable" {
		t.Errorf("expected code site_not_manageable, got %q", de.Code)
	}
}

// TestCreateBackup_ConnectedSite_NotRefused proves the new guard is narrowly
// scoped: a connected site (the common case) is unaffected.
func TestCreateBackup_ConnectedSite_NotRefused(t *testing.T) {
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	tenantID, siteID := uuid.New(), uuid.New()

	repo := newSchedulerTestRepo()
	svc := &Service{
		repo: repo,
		sites: fakeSites{info: SiteInfo{
			Enrolled: true, AgeRecipient: "age1test",
			ConnectionState: "connected",
		}},
		clock:    fakeClock{t: now},
		enqueuer: &recordingEnqueuer{},
	}

	if _, err := svc.CreateBackup(context.Background(), tenantID, siteID, uuid.New(), KindFull); err != nil {
		t.Fatalf("CreateBackup on a connected site: unexpected error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// sendBackupEmail suppresses the notification for an archived/revoked site.
// ---------------------------------------------------------------------------

// gh282FakeSiteLookup is a SiteLookup whose ConnectionState is settable per
// test, unlike the package's zero-field fakeSettingsSiteLookup.
type gh282FakeSiteLookup struct {
	state string
}

func (f gh282FakeSiteLookup) GetBackupSiteInfo(_ context.Context, _, _ uuid.UUID) (SiteInfo, error) {
	return SiteInfo{URL: "https://example.com", Enrolled: true, ConnectionState: f.state}, nil
}
func (f gh282FakeSiteLookup) ListSiteIDs(_ context.Context, _ uuid.UUID) ([]uuid.UUID, error) {
	return nil, nil
}

func TestSendBackupEmail_ArchivedSite_Suppressed(t *testing.T) {
	repo := newSettingsFakeRepo()
	siteID := uuid.New()
	repo.rows[siteID] = SiteBackupSettings{
		SiteID:             siteID,
		NotifyOnCompletion: "always",
		NotifyRecipients:   []string{"admin@example.com"},
		CreatedAt:          time.Now(),
		UpdatedAt:          time.Now(),
	}
	mailer := &fakeBackupMailer{}
	svc := &Service{
		repo: repo, mailer: mailer,
		sites: gh282FakeSiteLookup{state: ConnStateArchived},
		clock: fakeClock{t: time.Now()},
	}

	snap := Snapshot{ID: uuid.New(), TenantID: uuid.New(), SiteID: siteID}
	svc.sendBackupEmail(context.Background(), snap, "backup_completed")
	if mailer.calls != 0 {
		t.Errorf("archived site: notification must be suppressed; got %d calls", mailer.calls)
	}
}

func TestSendBackupEmail_RevokedSite_Suppressed(t *testing.T) {
	repo := newSettingsFakeRepo()
	siteID := uuid.New()
	repo.rows[siteID] = SiteBackupSettings{
		SiteID:             siteID,
		NotifyOnCompletion: "on_failure",
		NotifyRecipients:   []string{"admin@example.com"},
		CreatedAt:          time.Now(),
		UpdatedAt:          time.Now(),
	}
	mailer := &fakeBackupMailer{}
	svc := &Service{
		repo: repo, mailer: mailer,
		sites: gh282FakeSiteLookup{state: ConnStateRevoked},
		clock: fakeClock{t: time.Now()},
	}

	// A failed run against a revoked site (e.g. an in-flight run that lost
	// the race with the operator revoking mid-flight) must still be
	// suppressed even though on_failure + backup_failed would otherwise send.
	snap := Snapshot{ID: uuid.New(), TenantID: uuid.New(), SiteID: siteID}
	svc.sendBackupEmail(context.Background(), snap, "backup_failed")
	if mailer.calls != 0 {
		t.Errorf("revoked site: notification must be suppressed; got %d calls", mailer.calls)
	}
}

// TestSendBackupEmail_ConnectedSite_StillSent proves the new guard is
// narrowly scoped: a connected site's "always" notification still sends.
func TestSendBackupEmail_ConnectedSite_StillSent(t *testing.T) {
	repo := newSettingsFakeRepo()
	siteID := uuid.New()
	repo.rows[siteID] = SiteBackupSettings{
		SiteID:             siteID,
		NotifyOnCompletion: "always",
		NotifyRecipients:   []string{"admin@example.com"},
		CreatedAt:          time.Now(),
		UpdatedAt:          time.Now(),
	}
	mailer := &fakeBackupMailer{}
	svc := &Service{
		repo: repo, mailer: mailer,
		sites: gh282FakeSiteLookup{state: "connected"},
		clock: fakeClock{t: time.Now()},
	}

	snap := Snapshot{ID: uuid.New(), TenantID: uuid.New(), SiteID: siteID}
	svc.sendBackupEmail(context.Background(), snap, "backup_completed")
	if mailer.calls != 1 {
		t.Errorf("connected site + always policy: expected 1 email, got %d", mailer.calls)
	}
}

// TestSendBackupEmail_SiteLookupError_StillSent proves a site-lookup failure
// falls through to the pre-existing best-effort behaviour (still send) rather
// than blocking the notification. We cannot confirm archived/revoked, but we
// must not regress every existing "no site info" delivery either.
func TestSendBackupEmail_SiteLookupError_StillSent(t *testing.T) {
	repo := newSettingsFakeRepo()
	siteID := uuid.New()
	repo.rows[siteID] = SiteBackupSettings{
		SiteID:             siteID,
		NotifyOnCompletion: "always",
		NotifyRecipients:   []string{"admin@example.com"},
		CreatedAt:          time.Now(),
		UpdatedAt:          time.Now(),
	}
	mailer := &fakeBackupMailer{}
	svc := &Service{repo: repo, mailer: mailer, sites: fakeSitesError{err: domain.NotFound("site_not_found", "gone")}, clock: fakeClock{t: time.Now()}}

	snap := Snapshot{ID: uuid.New(), TenantID: uuid.New(), SiteID: siteID}
	svc.sendBackupEmail(context.Background(), snap, "backup_completed")
	if mailer.calls != 1 {
		t.Errorf("site lookup error: expected the pre-existing best-effort send (1 call), got %d", mailer.calls)
	}
}
