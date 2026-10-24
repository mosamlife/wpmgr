package backup

// backup_settings_test.go — unit tests for the m50 backup-settings decouple:
// GetBackupSettings, PutBackupContents, PutBackupNotifications service methods;
// scheduleBackupScope and sendBackupEmail data-source redirect; and the four
// new handler endpoints.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// ---------------------------------------------------------------------------
// settingsFakeRepo — in-memory Repo that stores one SiteBackupSettings row
// per siteID. Implements only the methods touched by the new backup-settings
// code; every other method panics ("unused in this test file").
// ---------------------------------------------------------------------------

type settingsFakeRepo struct {
	*fakeRepo                              // delegates all methods not overridden here
	rows map[uuid.UUID]SiteBackupSettings // keyed by siteID
}

func newSettingsFakeRepo() *settingsFakeRepo {
	return &settingsFakeRepo{
		fakeRepo: newFakeRepo(),
		rows:     make(map[uuid.UUID]SiteBackupSettings),
	}
}

func (r *settingsFakeRepo) GetBackupSettings(_ context.Context, _, siteID uuid.UUID) (SiteBackupSettings, error) {
	s, ok := r.rows[siteID]
	if !ok {
		return SiteBackupSettings{}, domain.NotFound("backup_settings_not_found", "no settings")
	}
	return s, nil
}

func (r *settingsFakeRepo) UpsertBackupSettings(_ context.Context, _ uuid.UUID, in SiteBackupSettings) (SiteBackupSettings, error) {
	if in.CreatedAt.IsZero() {
		in.CreatedAt = time.Now()
	}
	in.UpdatedAt = time.Now()
	r.rows[in.SiteID] = in
	return in, nil
}

// settingsSvc wires a minimal Service for backup-settings tests.
func settingsSvc(repo *settingsFakeRepo) *Service {
	return &Service{
		repo:  repo,
		clock: fakeClock{t: time.Now()},
	}
}

// ---------------------------------------------------------------------------
// GetBackupSettings
// ---------------------------------------------------------------------------

func TestGetBackupSettings_NoRow_SafeDefaults(t *testing.T) {
	repo := newSettingsFakeRepo()
	svc := settingsSvc(repo)
	tenantID := uuid.New()
	siteID := uuid.New()

	got, err := svc.GetBackupSettings(context.Background(), tenantID, siteID)
	if err != nil {
		t.Fatalf("GetBackupSettings returned error: %v", err)
	}
	if got.SiteID != siteID {
		t.Errorf("SiteID: want %v, got %v", siteID, got.SiteID)
	}
	if got.NotifyOnCompletion != "never" {
		t.Errorf("NotifyOnCompletion: want 'never', got %q", got.NotifyOnCompletion)
	}
	if got.BackupComponents != nil {
		t.Errorf("BackupComponents: want nil, got %v", got.BackupComponents)
	}
	if got.ExcludeFileSizeMB != 0 {
		t.Errorf("ExcludeFileSizeMB: want 0, got %d", got.ExcludeFileSizeMB)
	}
}

// ---------------------------------------------------------------------------
// PutBackupContents — validation
// ---------------------------------------------------------------------------

func TestPutBackupContents_InvalidComponent(t *testing.T) {
	repo := newSettingsFakeRepo()
	svc := settingsSvc(repo)

	_, err := svc.PutBackupContents(context.Background(), PutBackupContentsInput{
		TenantID:         uuid.New(),
		SiteID:           uuid.New(),
		BackupComponents: []string{"invalid"},
	})
	if err == nil {
		t.Fatal("expected validation error for invalid component")
	}
	var de *domain.Error
	if !isValidationError(err, &de) || de.Code != "invalid_backup_component" {
		t.Errorf("want invalid_backup_component, got %v", err)
	}
}

func TestPutBackupContents_ExcludeFileSizeMB_NegativeRejected(t *testing.T) {
	repo := newSettingsFakeRepo()
	svc := settingsSvc(repo)

	_, err := svc.PutBackupContents(context.Background(), PutBackupContentsInput{
		TenantID:          uuid.New(),
		SiteID:            uuid.New(),
		ExcludeFileSizeMB: -1,
	})
	if err == nil {
		t.Fatal("expected validation error for negative ExcludeFileSizeMB")
	}
	var de *domain.Error
	if !isValidationError(err, &de) || de.Code != "invalid_exclude_file_size_mb" {
		t.Errorf("want invalid_exclude_file_size_mb, got %v", err)
	}
}

func TestPutBackupContents_ExcludeFileSizeMB_ExceedsMax(t *testing.T) {
	repo := newSettingsFakeRepo()
	svc := settingsSvc(repo)

	_, err := svc.PutBackupContents(context.Background(), PutBackupContentsInput{
		TenantID:          uuid.New(),
		SiteID:            uuid.New(),
		ExcludeFileSizeMB: 102401,
	})
	if err == nil {
		t.Fatal("expected validation error for ExcludeFileSizeMB > 102400")
	}
	var de *domain.Error
	if !isValidationError(err, &de) || de.Code != "invalid_exclude_file_size_mb" {
		t.Errorf("want invalid_exclude_file_size_mb, got %v", err)
	}
}

func TestPutBackupContents_ExcludeFileSizeMB_ZeroSucceeds(t *testing.T) {
	repo := newSettingsFakeRepo()
	svc := settingsSvc(repo)

	got, err := svc.PutBackupContents(context.Background(), PutBackupContentsInput{
		TenantID:          uuid.New(),
		SiteID:            uuid.New(),
		ExcludeFileSizeMB: 0,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ExcludeFileSizeMB != 0 {
		t.Errorf("ExcludeFileSizeMB: want 0, got %d", got.ExcludeFileSizeMB)
	}
}

func TestPutBackupContents_ExcludeFileSizeMB_MaxSucceeds(t *testing.T) {
	repo := newSettingsFakeRepo()
	svc := settingsSvc(repo)

	got, err := svc.PutBackupContents(context.Background(), PutBackupContentsInput{
		TenantID:          uuid.New(),
		SiteID:            uuid.New(),
		ExcludeFileSizeMB: 102400,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ExcludeFileSizeMB != 102400 {
		t.Errorf("ExcludeFileSizeMB: want 102400, got %d", got.ExcludeFileSizeMB)
	}
}

func TestPutBackupContents_TooManyPaths(t *testing.T) {
	repo := newSettingsFakeRepo()
	svc := settingsSvc(repo)

	paths := make([]string, 101)
	for i := range paths {
		paths[i] = "path"
	}
	_, err := svc.PutBackupContents(context.Background(), PutBackupContentsInput{
		TenantID:     uuid.New(),
		SiteID:       uuid.New(),
		ExcludePaths: paths,
	})
	if err == nil {
		t.Fatal("expected validation error for too many exclude_paths")
	}
	var de *domain.Error
	if !isValidationError(err, &de) || de.Code != "too_many_exclude_paths" {
		t.Errorf("want too_many_exclude_paths, got %v", err)
	}
}

func TestPutBackupContents_TooManyExtensions(t *testing.T) {
	repo := newSettingsFakeRepo()
	svc := settingsSvc(repo)

	exts := make([]string, 51)
	for i := range exts {
		exts[i] = "ext"
	}
	_, err := svc.PutBackupContents(context.Background(), PutBackupContentsInput{
		TenantID:          uuid.New(),
		SiteID:            uuid.New(),
		ExcludeExtensions: exts,
	})
	if err == nil {
		t.Fatal("expected validation error for too many exclude_extensions")
	}
	var de *domain.Error
	if !isValidationError(err, &de) || de.Code != "too_many_exclude_extensions" {
		t.Errorf("want too_many_exclude_extensions, got %v", err)
	}
}

// TestPutBackupContents_MergesNotificationFields verifies that updating
// content fields does not clobber existing notification fields.
func TestPutBackupContents_MergesNotificationFields(t *testing.T) {
	repo := newSettingsFakeRepo()
	svc := settingsSvc(repo)
	tenantID := uuid.New()
	siteID := uuid.New()

	// Seed an existing row with notification settings.
	repo.rows[siteID] = SiteBackupSettings{
		SiteID:             siteID,
		NotifyOnCompletion: "always",
		NotifyRecipients:   []string{"admin@example.com"},
		CreatedAt:          time.Now(),
		UpdatedAt:          time.Now(),
	}

	got, err := svc.PutBackupContents(context.Background(), PutBackupContentsInput{
		TenantID:         tenantID,
		SiteID:           siteID,
		BackupComponents: []string{EntryKindPlugin},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.NotifyOnCompletion != "always" {
		t.Errorf("notification field clobbered: want 'always', got %q", got.NotifyOnCompletion)
	}
	if len(got.NotifyRecipients) != 1 || got.NotifyRecipients[0] != "admin@example.com" {
		t.Errorf("notify_recipients clobbered: got %v", got.NotifyRecipients)
	}
	if len(got.BackupComponents) != 1 || got.BackupComponents[0] != EntryKindPlugin {
		t.Errorf("backup_components not saved: got %v", got.BackupComponents)
	}
}

// ---------------------------------------------------------------------------
// PutBackupNotifications — validation
// ---------------------------------------------------------------------------

func TestPutBackupNotifications_InvalidEnum(t *testing.T) {
	repo := newSettingsFakeRepo()
	svc := settingsSvc(repo)

	_, err := svc.PutBackupNotifications(context.Background(), PutBackupNotificationsInput{
		TenantID:           uuid.New(),
		SiteID:             uuid.New(),
		NotifyOnCompletion: "sometimes",
	})
	if err == nil {
		t.Fatal("expected validation error for invalid notify_on_completion")
	}
	var de *domain.Error
	if !isValidationError(err, &de) || de.Code != "invalid_notify_on_completion" {
		t.Errorf("want invalid_notify_on_completion, got %v", err)
	}
}

func TestPutBackupNotifications_EmptyEnum_NormalisedToNever(t *testing.T) {
	repo := newSettingsFakeRepo()
	svc := settingsSvc(repo)

	got, err := svc.PutBackupNotifications(context.Background(), PutBackupNotificationsInput{
		TenantID:           uuid.New(),
		SiteID:             uuid.New(),
		NotifyOnCompletion: "",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.NotifyOnCompletion != "never" {
		t.Errorf("empty string not normalised to 'never'; got %q", got.NotifyOnCompletion)
	}
}

func TestPutBackupNotifications_BadEmail(t *testing.T) {
	repo := newSettingsFakeRepo()
	svc := settingsSvc(repo)

	_, err := svc.PutBackupNotifications(context.Background(), PutBackupNotificationsInput{
		TenantID:           uuid.New(),
		SiteID:             uuid.New(),
		NotifyOnCompletion: "always",
		NotifyRecipients:   []string{"notanemail"},
	})
	if err == nil {
		t.Fatal("expected validation error for invalid email")
	}
	var de *domain.Error
	if !isValidationError(err, &de) || de.Code != "invalid_notify_recipient" {
		t.Errorf("want invalid_notify_recipient, got %v", err)
	}
}

func TestPutBackupNotifications_TooManyRecipients(t *testing.T) {
	repo := newSettingsFakeRepo()
	svc := settingsSvc(repo)

	recips := make([]string, 21)
	for i := range recips {
		recips[i] = "user@example.com"
	}
	_, err := svc.PutBackupNotifications(context.Background(), PutBackupNotificationsInput{
		TenantID:           uuid.New(),
		SiteID:             uuid.New(),
		NotifyOnCompletion: "always",
		NotifyRecipients:   recips,
	})
	if err == nil {
		t.Fatal("expected validation error for too many recipients")
	}
	var de *domain.Error
	if !isValidationError(err, &de) || de.Code != "too_many_notify_recipients" {
		t.Errorf("want too_many_notify_recipients, got %v", err)
	}
}

// TestPutBackupNotifications_MergesContentFields verifies that updating
// notification fields does not clobber existing content fields.
func TestPutBackupNotifications_MergesContentFields(t *testing.T) {
	repo := newSettingsFakeRepo()
	svc := settingsSvc(repo)
	tenantID := uuid.New()
	siteID := uuid.New()

	// Seed an existing row with content settings.
	repo.rows[siteID] = SiteBackupSettings{
		SiteID:           siteID,
		BackupComponents: []string{EntryKindPlugin},
		IncludeCore:      true,
		CreatedAt:        time.Now(),
		UpdatedAt:        time.Now(),
	}

	got, err := svc.PutBackupNotifications(context.Background(), PutBackupNotificationsInput{
		TenantID:           tenantID,
		SiteID:             siteID,
		NotifyOnCompletion: "on_failure",
		NotifyRecipients:   []string{"ops@example.com"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.NotifyOnCompletion != "on_failure" {
		t.Errorf("notify_on_completion not saved: got %q", got.NotifyOnCompletion)
	}
	if len(got.BackupComponents) != 1 || got.BackupComponents[0] != EntryKindPlugin {
		t.Errorf("content fields clobbered: backup_components got %v", got.BackupComponents)
	}
	if !got.IncludeCore {
		t.Error("content fields clobbered: include_core should be true")
	}
}

// ---------------------------------------------------------------------------
// scheduleBackupScope — now reads from GetBackupSettings (m50)
// ---------------------------------------------------------------------------

func TestScheduleBackupScope_NoRow_ReturnsZero(t *testing.T) {
	repo := newSettingsFakeRepo()
	svc := settingsSvc(repo)

	scope, err := svc.scheduleBackupScope(context.Background(), uuid.New(), uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if len(scope.Components) != 0 {
		t.Errorf("expected no components on missing row; got %v", scope.Components)
	}
	if scope.IncludeCore {
		t.Error("expected IncludeCore=false on missing row")
	}
	if scope.ExcludeFileSizeMB != 0 {
		t.Errorf("expected ExcludeFileSizeMB=0 on missing row; got %d", scope.ExcludeFileSizeMB)
	}
}

func TestScheduleBackupScope_WithRow_ReturnsSettings(t *testing.T) {
	repo := newSettingsFakeRepo()
	svc := settingsSvc(repo)
	siteID := uuid.New()

	repo.rows[siteID] = SiteBackupSettings{
		SiteID:           siteID,
		BackupComponents: []string{EntryKindPlugin, EntryKindDB},
		IncludeCore:      true,
		ExcludeFileSizeMB: 512,
	}

	scope, err := svc.scheduleBackupScope(context.Background(), uuid.New(), siteID)
	if err != nil {
		t.Fatal(err)
	}
	if len(scope.Components) != 2 {
		t.Errorf("expected 2 components, got %v", scope.Components)
	}
	if !scope.IncludeCore {
		t.Error("expected IncludeCore=true")
	}
	if scope.ExcludeFileSizeMB != 512 {
		t.Errorf("expected ExcludeFileSizeMB=512, got %d", scope.ExcludeFileSizeMB)
	}
}

// ---------------------------------------------------------------------------
// sendBackupEmail — now reads from GetBackupSettings (m50)
// ---------------------------------------------------------------------------

type fakeBackupMailer struct {
	calls int
}

func (m *fakeBackupMailer) Enqueue(_ context.Context, _ uuid.UUID, _ []string, _ string, _ map[string]any) error {
	m.calls++
	return nil
}

type fakeSettingsSiteLookup struct{}

func (f fakeSettingsSiteLookup) GetBackupSiteInfo(_ context.Context, _, _ uuid.UUID) (SiteInfo, error) {
	return SiteInfo{URL: "https://example.com", Enrolled: true}, nil
}
func (f fakeSettingsSiteLookup) ListSiteIDs(_ context.Context, _ uuid.UUID) ([]uuid.UUID, error) {
	return nil, nil
}

func TestSendBackupEmail_NoRow_NoEmail(t *testing.T) {
	repo := newSettingsFakeRepo()
	mailer := &fakeBackupMailer{}
	svc := &Service{repo: repo, mailer: mailer, sites: fakeSettingsSiteLookup{}, clock: fakeClock{t: time.Now()}}

	snap := Snapshot{
		ID:         uuid.New(),
		TenantID:   uuid.New(),
		SiteID:     uuid.New(),
		TotalSize:  1024,
		FinishedAt: func() *time.Time { t := time.Now(); return &t }(),
	}
	svc.sendBackupEmail(context.Background(), snap, "backup_completed")
	if mailer.calls != 0 {
		t.Errorf("mailer should not be called when no settings row; got %d calls", mailer.calls)
	}
}

func TestSendBackupEmail_NeverPolicy_NoEmail(t *testing.T) {
	repo := newSettingsFakeRepo()
	siteID := uuid.New()
	repo.rows[siteID] = SiteBackupSettings{
		SiteID:             siteID,
		NotifyOnCompletion: "never",
		NotifyRecipients:   []string{"admin@example.com"},
		CreatedAt:          time.Now(),
		UpdatedAt:          time.Now(),
	}
	mailer := &fakeBackupMailer{}
	svc := &Service{repo: repo, mailer: mailer, sites: fakeSettingsSiteLookup{}, clock: fakeClock{t: time.Now()}}

	snap := Snapshot{ID: uuid.New(), TenantID: uuid.New(), SiteID: siteID}
	svc.sendBackupEmail(context.Background(), snap, "backup_completed")
	if mailer.calls != 0 {
		t.Errorf("never policy must not enqueue email; got %d calls", mailer.calls)
	}
}

func TestSendBackupEmail_OnFailure_CompletedTemplate_NoEmail(t *testing.T) {
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
	svc := &Service{repo: repo, mailer: mailer, sites: fakeSettingsSiteLookup{}, clock: fakeClock{t: time.Now()}}

	snap := Snapshot{ID: uuid.New(), TenantID: uuid.New(), SiteID: siteID}
	svc.sendBackupEmail(context.Background(), snap, "backup_completed")
	if mailer.calls != 0 {
		t.Errorf("on_failure must not email for completion template; got %d calls", mailer.calls)
	}
}

func TestSendBackupEmail_OnFailure_FailedTemplate_EmailSent(t *testing.T) {
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
	svc := &Service{repo: repo, mailer: mailer, sites: fakeSettingsSiteLookup{}, clock: fakeClock{t: time.Now()}}

	snap := Snapshot{ID: uuid.New(), TenantID: uuid.New(), SiteID: siteID}
	svc.sendBackupEmail(context.Background(), snap, "backup_failed")
	if mailer.calls != 1 {
		t.Errorf("on_failure + backup_failed must enqueue 1 email; got %d calls", mailer.calls)
	}
}

func TestSendBackupEmail_AlwaysPolicy_EmailSent(t *testing.T) {
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
	svc := &Service{repo: repo, mailer: mailer, sites: fakeSettingsSiteLookup{}, clock: fakeClock{t: time.Now()}}

	snap := Snapshot{ID: uuid.New(), TenantID: uuid.New(), SiteID: siteID}
	svc.sendBackupEmail(context.Background(), snap, "backup_completed")
	if mailer.calls != 1 {
		t.Errorf("always policy must enqueue 1 email; got %d calls", mailer.calls)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// isValidationError reports whether err is a domain.Error with Kind=Validation,
// and if so, sets *out to the error value.
func isValidationError(err error, out **domain.Error) bool {
	import_errors_as_errors := err
	_ = import_errors_as_errors // silence unused import; we use errors.As via de
	var de *domain.Error
	if !asError(err, &de) {
		return false
	}
	if de.Kind != domain.KindValidation {
		return false
	}
	*out = de
	return true
}

// asError is a local wrapper to avoid importing "errors" directly in this file.
func asError(err error, target **domain.Error) bool {
	// Walk the error chain manually — mirroring errors.As.
	type unwrapper interface{ Unwrap() error }
	for {
		if de, ok := err.(*domain.Error); ok {
			*target = de
			return true
		}
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
		if err == nil {
			return false
		}
	}
}
