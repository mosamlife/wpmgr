package backup

// billing_gate_test.go — M16 Phase B managed-backup-storage gate tests.
//
// White-box, in-memory fakes; no database. The CRITICAL regression guard here
// is TestPlanRestore_NeverConsultsBillingGate: a customer must always be able
// to get existing data back, even after losing the managed-storage
// entitlement, so restore/download must never call BillingGate at all.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// fakeBillingGate is an in-memory backup.BillingGate double. err (if set) is
// returned by every CheckManagedBackupStorage call; calls records every
// tenantID the gate was invoked with, so a test can assert it was (or was
// NOT) consulted at all.
type fakeBillingGate struct {
	err   error
	calls []uuid.UUID
}

func (f *fakeBillingGate) CheckManagedBackupStorage(_ context.Context, tenantID uuid.UUID) error {
	f.calls = append(f.calls, tenantID)
	return f.err
}

var errGateDenied = domain.PaymentRequired("byo_destination_required", "Free plan backups must go to your own storage.")

// TestServiceSetBillingGate_Wires proves SetBillingGate lands the gate on the
// Service's own `billing` field (unlike site.SetBillingGate, backup.Service
// has no second repo instance to double-wire — see BillingGate's doc comment
// for why that concern doesn't apply here).
func TestServiceSetBillingGate_Wires(t *testing.T) {
	svc := &Service{}
	gate := &fakeBillingGate{}
	svc.SetBillingGate(gate)
	if svc.billing != BillingGate(gate) {
		t.Fatal("SetBillingGate did not wire the gate onto the service")
	}
}

// ---------------------------------------------------------------------------
// destinationIsManaged
// ---------------------------------------------------------------------------

func TestDestinationIsManaged(t *testing.T) {
	tenantID := uuid.New()
	cpDestID := uuid.New()
	localDestID := uuid.New()
	s3DestID := uuid.New()
	missingDestID := uuid.New()

	dl := newFakeDestLookup()
	dl.infoByID[cpDestID] = DestinationInfo{ID: cpDestID, Kind: DestinationKindCP}
	dl.infoByID[localDestID] = DestinationInfo{ID: localDestID, Kind: DestinationKindLocal}
	dl.infoByID[s3DestID] = DestinationInfo{ID: s3DestID, Kind: DestinationKindS3Compat}
	// missingDestID intentionally absent from dl.infoByID -> DestinationInfo errors.

	svc := &Service{destLookup: dl}

	tests := []struct {
		name string
		dest uuid.UUID
		want bool
	}{
		{"nil destination id is the managed default", uuid.Nil, true},
		{"explicit cp destination is managed", cpDestID, true},
		{"local destination is BYO, not managed", localDestID, false},
		{"s3_compat destination is BYO, not managed", s3DestID, false},
		{"a lookup failure fails open (gate skipped)", missingDestID, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := svc.destinationIsManaged(context.Background(), tenantID, tt.dest)
			if got != tt.want {
				t.Errorf("destinationIsManaged(%v) = %v, want %v", tt.dest, got, tt.want)
			}
		})
	}
}

func TestDestinationIsManaged_NoDestLookupWired_NonNilID_NotManaged(t *testing.T) {
	// Cannot happen operationally (SetDestinationLookup pairs with
	// SetBillingGate) but must degrade safely: a non-nil destination id with
	// no destLookup wired is treated as "not managed" (gate skipped), not
	// "managed" (which would incorrectly block the run).
	svc := &Service{}
	if svc.destinationIsManaged(context.Background(), uuid.New(), uuid.New()) {
		t.Fatal("expected false (gate skipped) when destLookup is unwired for a non-nil destination id")
	}
}

// ---------------------------------------------------------------------------
// checkManagedBackupStorageGate
// ---------------------------------------------------------------------------

func TestCheckManagedBackupStorageGate_NilBillingGate_Allowed(t *testing.T) {
	svc := &Service{}
	if err := svc.checkManagedBackupStorageGate(context.Background(), uuid.New(), uuid.Nil); err != nil {
		t.Fatalf("expected nil with no billing gate wired, got %v", err)
	}
}

func TestCheckManagedBackupStorageGate_BYODestination_GateNeverCalled(t *testing.T) {
	tenantID := uuid.New()
	destID := uuid.New()
	dl := newFakeDestLookup()
	dl.infoByID[destID] = DestinationInfo{ID: destID, Kind: DestinationKindLocal}
	gate := &fakeBillingGate{err: errGateDenied}
	svc := &Service{destLookup: dl, billing: gate}

	if err := svc.checkManagedBackupStorageGate(context.Background(), tenantID, destID); err != nil {
		t.Fatalf("a BYO destination must never be gated, got %v", err)
	}
	if len(gate.calls) != 0 {
		t.Fatalf("expected the billing gate to never be called for a BYO destination, got %d calls", len(gate.calls))
	}
}

func TestCheckManagedBackupStorageGate_ManagedDestination_CallsGate(t *testing.T) {
	tenantID := uuid.New()
	gate := &fakeBillingGate{err: errGateDenied}
	svc := &Service{billing: gate}

	err := svc.checkManagedBackupStorageGate(context.Background(), tenantID, uuid.Nil)
	if err != errGateDenied {
		t.Fatalf("expected the gate's error to propagate, got %v", err)
	}
	if len(gate.calls) != 1 || gate.calls[0] != tenantID {
		t.Fatalf("expected exactly 1 gate call for tenant %v, got %v", tenantID, gate.calls)
	}
}

// ---------------------------------------------------------------------------
// CreateBackup (manual/run-now)
// ---------------------------------------------------------------------------

func TestCreateBackup_ManagedDestinationGateDenied_Returns402(t *testing.T) {
	repo := &wiringRepo{}
	enq := &recordingEnqueuer{}
	svc := buildWiringSvc(repo, enq, time.Now())
	gate := &fakeBillingGate{err: errGateDenied}
	svc.billing = gate

	_, err := svc.CreateBackup(context.Background(), uuid.New(), uuid.New(), uuid.New(), KindFull)
	if err != errGateDenied {
		t.Fatalf("expected the gate error to propagate, got %v", err)
	}
	if len(repo.createInputs) != 0 {
		t.Fatalf("expected NO snapshot to be created when the managed-storage gate denies, got %d", len(repo.createInputs))
	}
}

func TestCreateBackup_BYODestination_AllowedEvenWhenGateWouldDeny(t *testing.T) {
	siteID := uuid.New()
	destID := uuid.New()
	dl := newFakeDestLookup()
	dl.defaultBySite[siteID] = destID
	dl.infoByID[destID] = DestinationInfo{ID: destID, Kind: DestinationKindS3Compat}

	repo := &wiringRepo{}
	enq := &recordingEnqueuer{}
	svc := buildWiringSvc(repo, enq, time.Now())
	svc.destLookup = dl
	gate := &fakeBillingGate{err: errGateDenied}
	svc.billing = gate

	if _, err := svc.CreateBackup(context.Background(), uuid.New(), siteID, uuid.New(), KindFull); err != nil {
		t.Fatalf("a BYO destination must never be gated, got %v", err)
	}
	if len(repo.createInputs) != 1 || repo.createInputs[0].DestinationID != destID {
		t.Fatalf("expected 1 snapshot with DestinationID=%v, got %+v", destID, repo.createInputs)
	}
	if len(gate.calls) != 0 {
		t.Fatal("expected the billing gate to never be called for a BYO destination")
	}
}

func TestCreateBackup_NilBillingGate_Allowed(t *testing.T) {
	repo := &wiringRepo{}
	enq := &recordingEnqueuer{}
	svc := buildWiringSvc(repo, enq, time.Now())
	// billing intentionally left nil (self-host / not-yet-wired) — the
	// zero-value default from buildWiringSvc.

	if _, err := svc.CreateBackup(context.Background(), uuid.New(), uuid.New(), uuid.New(), KindFull); err != nil {
		t.Fatalf("expected no error with a nil billing gate, got %v", err)
	}
	if len(repo.createInputs) != 1 {
		t.Fatalf("expected 1 snapshot to be created, got %d", len(repo.createInputs))
	}
}

// ---------------------------------------------------------------------------
// EnqueueScheduledBackup (scheduled path) — gated run records 'skipped', never throws
// ---------------------------------------------------------------------------

func TestEnqueueScheduledBackup_ManagedDestinationGateDenied_RecordsSkippedNotError(t *testing.T) {
	tenantID, siteID, scheduleID := uuid.New(), uuid.New(), uuid.New()
	repo := &wiringRepo{}
	enq := &recordingEnqueuer{}
	runStore := &fakeScheduleRunStore{}
	gate := &fakeBillingGate{err: errGateDenied}

	svc := &Service{
		repo:         repo,
		enqueuer:     enq,
		sites:        fakeSites{info: SiteInfo{Enrolled: true, AgeRecipient: "age1test"}},
		clock:        fakeClock{t: time.Now()},
		scheduleRuns: runStore,
		billing:      gate,
	}

	sched := Schedule{
		ID:        scheduleID,
		TenantID:  tenantID,
		SiteID:    siteID,
		Cadence:   CadenceDaily,
		Kind:      KindFull,
		NextRunAt: time.Now(),
	}

	if err := svc.EnqueueScheduledBackup(context.Background(), sched); err != nil {
		t.Fatalf("EnqueueScheduledBackup must not return an error on a gated scheduled run (must record skipped instead), got %v", err)
	}
	if len(repo.createInputs) != 0 {
		t.Fatalf("expected NO snapshot to be created for a gated scheduled run, got %d", len(repo.createInputs))
	}
	if len(enq.plainCalls) != 0 || len(enq.chainCalls) != 0 {
		t.Fatal("expected no enqueue call for a gated scheduled run")
	}
	if len(runStore.rows) != 1 {
		t.Fatalf("expected exactly 1 schedule_run row, got %d", len(runStore.rows))
	}
	if runStore.rows[0].Status != ScheduleRunStatusSkipped {
		t.Fatalf("schedule run status = %q, want %q", runStore.rows[0].Status, ScheduleRunStatusSkipped)
	}
	if len(gate.calls) != 1 {
		t.Fatalf("expected exactly 1 gate call, got %d", len(gate.calls))
	}
}

func TestEnqueueScheduledBackup_BYODestination_Allowed(t *testing.T) {
	tenantID, siteID, scheduleID := uuid.New(), uuid.New(), uuid.New()
	destID := uuid.New()
	dl := newFakeDestLookup()
	dl.defaultBySite[siteID] = destID
	dl.infoByID[destID] = DestinationInfo{ID: destID, Kind: DestinationKindLocal, PathPrefix: "wpmgr-backups"}
	gate := &fakeBillingGate{err: errGateDenied}

	repo := &wiringRepo{}
	enq := &recordingEnqueuer{}
	runStore := &fakeScheduleRunStore{}
	svc := &Service{
		repo:         repo,
		enqueuer:     enq,
		sites:        fakeSites{info: SiteInfo{Enrolled: true, AgeRecipient: "age1test"}},
		clock:        fakeClock{t: time.Now()},
		scheduleRuns: runStore,
		billing:      gate,
		destLookup:   dl,
	}

	sched := Schedule{ID: scheduleID, TenantID: tenantID, SiteID: siteID, Cadence: CadenceDaily, Kind: KindFull, NextRunAt: time.Now()}
	if err := svc.EnqueueScheduledBackup(context.Background(), sched); err != nil {
		t.Fatalf("EnqueueScheduledBackup: %v", err)
	}
	if len(repo.createInputs) != 1 {
		t.Fatalf("expected 1 snapshot created for a BYO destination even with a denying gate, got %d", len(repo.createInputs))
	}
	if len(gate.calls) != 0 {
		t.Fatalf("expected the gate to never be called for a BYO destination, got %d calls", len(gate.calls))
	}
}

// ---------------------------------------------------------------------------
// Restore is NEVER gated — the core business invariant.
// ---------------------------------------------------------------------------

// TestPlanRestore_NeverConsultsBillingGate is the CRITICAL regression guard:
// a customer must always be able to get an existing backup back, even after
// losing the managed-storage entitlement. A managed (uuid.Nil destination)
// snapshot restore must succeed even when a wired billing gate would deny
// EVERY call, and the gate must never be invoked at all.
func TestPlanRestore_NeverConsultsBillingGate(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()
	snapID := uuid.New()

	repo := &fakeWorkerRepo{fakeRepo: newFakeRepo(), workerManifests: map[uuid.UUID][]ManifestEntry{}}
	snap := Snapshot{
		ID:           snapID,
		TenantID:     tenantID,
		SiteID:       siteID,
		Kind:         KindFull,
		Status:       StatusCompleted,
		AgeRecipient: "age1test",
		// DestinationID intentionally zero (uuid.Nil) — the managed path,
		// exactly the case a CREATE-time gate would fire on.
	}
	repo.setSnapshot(snap)
	repo.workerManifests[snapID] = []ManifestEntry{
		{Path: "wp-content/plugins/foo/foo.php", EntryKind: EntryKindFile, ChunkHashes: []string{"aaa"}, Size: 100},
	}

	fp := &fakePresigner{}
	gate := &fakeBillingGate{err: errGateDenied} // would deny EVERY call if ever consulted
	svc := &Service{
		repo:       repo,
		sites:      &fakeSiteLookup{},
		store:      &tenantPresigner{tenantID: tenantID, inner: fp},
		clock:      fakeClock{t: time.Now()},
		presignTTL: time.Hour,
		billing:    gate,
	}

	if _, _, _, err := svc.PlanRestore(context.Background(), tenantID, snapID, RestoreSelection{Full: true}, "restore-regression", "https://cp.test/progress"); err != nil {
		t.Fatalf("PlanRestore must succeed for a managed snapshot even with a denying billing gate wired, got %v", err)
	}
	if len(gate.calls) != 0 {
		t.Fatalf("PlanRestore must NEVER consult the managed-backup-storage billing gate — restore is never gated — got %d calls", len(gate.calls))
	}
}
