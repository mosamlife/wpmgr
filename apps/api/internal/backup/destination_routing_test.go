package backup

// destination_routing_test.go — ADR-036 P1 (GH #146) unit tests: default-
// destination resolution at backup-creation time (CreateBackup /
// EnqueueScheduledBackup), destination-kind/config threading on backup
// dispatch (BackupWorker.Work), and destination-aware restore planning
// (PlanRestore) — local skips presign, and the managed (cp/nil) path is
// provably byte-for-byte unchanged, including when destLookup is never wired.
//
// White-box, in-memory fakes; no database or network required.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// ---------------------------------------------------------------------------
// fakeDestLookup — in-memory DestinationLookup double.
// ---------------------------------------------------------------------------

type fakeDestLookup struct {
	defaultBySite map[uuid.UUID]uuid.UUID // siteID -> destination id (absent = none configured)
	infoByID      map[uuid.UUID]DestinationInfo
}

func newFakeDestLookup() *fakeDestLookup {
	return &fakeDestLookup{
		defaultBySite: map[uuid.UUID]uuid.UUID{},
		infoByID:      map[uuid.UUID]DestinationInfo{},
	}
}

func (f *fakeDestLookup) DefaultDestinationForSite(_ context.Context, _, siteID uuid.UUID) (uuid.UUID, error) {
	return f.defaultBySite[siteID], nil
}

func (f *fakeDestLookup) DestinationInfo(_ context.Context, _, destinationID uuid.UUID) (DestinationInfo, error) {
	info, ok := f.infoByID[destinationID]
	if !ok {
		return DestinationInfo{}, domain.NotFound("site_destination_not_found", "not found")
	}
	return info, nil
}

// ---------------------------------------------------------------------------
// (a)/(c) — CreateBackup / EnqueueScheduledBackup stamp the resolved default
// destination id, or uuid.Nil when none is configured / destLookup is unwired.
// ---------------------------------------------------------------------------

func TestCreateBackup_DefaultDestination_Stamped(t *testing.T) {
	tenantID, siteID := uuid.New(), uuid.New()
	destID := uuid.New()
	dl := newFakeDestLookup()
	dl.defaultBySite[siteID] = destID

	repo := &wiringRepo{}
	enq := &recordingEnqueuer{}
	svc := buildWiringSvc(repo, enq, time.Now())
	svc.destLookup = dl

	if _, err := svc.CreateBackup(context.Background(), tenantID, siteID, uuid.New(), KindFull); err != nil {
		t.Fatalf("CreateBackup error: %v", err)
	}
	if len(repo.createInputs) != 1 {
		t.Fatalf("expected 1 CreateSnapshot call, got %d", len(repo.createInputs))
	}
	if repo.createInputs[0].DestinationID != destID {
		t.Errorf("DestinationID = %v, want %v", repo.createInputs[0].DestinationID, destID)
	}
}

func TestCreateBackup_NoDefaultDestination_Nil(t *testing.T) {
	tenantID, siteID := uuid.New(), uuid.New()
	dl := newFakeDestLookup() // no default configured for any site

	repo := &wiringRepo{}
	enq := &recordingEnqueuer{}
	svc := buildWiringSvc(repo, enq, time.Now())
	svc.destLookup = dl

	if _, err := svc.CreateBackup(context.Background(), tenantID, siteID, uuid.New(), KindFull); err != nil {
		t.Fatalf("CreateBackup error: %v", err)
	}
	if repo.createInputs[0].DestinationID != uuid.Nil {
		t.Errorf("DestinationID = %v, want uuid.Nil (managed/legacy path)", repo.createInputs[0].DestinationID)
	}
}

func TestCreateBackup_DestLookupUnwired_Nil(t *testing.T) {
	// destLookup is never set (nil) — the byte-for-byte pre-feature behaviour.
	repo := &wiringRepo{}
	enq := &recordingEnqueuer{}
	svc := buildWiringSvc(repo, enq, time.Now())

	if _, err := svc.CreateBackup(context.Background(), uuid.New(), uuid.New(), uuid.New(), KindFull); err != nil {
		t.Fatalf("CreateBackup error: %v", err)
	}
	if repo.createInputs[0].DestinationID != uuid.Nil {
		t.Errorf("DestinationID = %v, want uuid.Nil when destLookup is unwired", repo.createInputs[0].DestinationID)
	}
}

func TestEnqueueScheduledBackup_DefaultDestination_Stamped(t *testing.T) {
	tenantID, siteID := uuid.New(), uuid.New()
	destID := uuid.New()
	dl := newFakeDestLookup()
	dl.defaultBySite[siteID] = destID

	repo := &wiringRepo{}
	enq := &recordingEnqueuer{}
	svc := buildWiringSvc(repo, enq, time.Now())
	svc.destLookup = dl

	sched := Schedule{
		ID:        uuid.New(),
		TenantID:  tenantID,
		SiteID:    siteID,
		Cadence:   CadenceDaily,
		Kind:      KindFull,
		NextRunAt: time.Now(),
	}
	if err := svc.EnqueueScheduledBackup(context.Background(), sched); err != nil {
		t.Fatalf("EnqueueScheduledBackup error: %v", err)
	}
	if len(repo.createInputs) != 1 || repo.createInputs[0].DestinationID != destID {
		t.Errorf("DestinationID = %v (creates=%d), want %v", repo.createInputs[0].DestinationID, len(repo.createInputs), destID)
	}
}

func TestEnqueueScheduledBackup_NoDefaultDestination_Nil(t *testing.T) {
	tenantID, siteID := uuid.New(), uuid.New()
	dl := newFakeDestLookup()

	repo := &wiringRepo{}
	enq := &recordingEnqueuer{}
	svc := buildWiringSvc(repo, enq, time.Now())
	svc.destLookup = dl

	sched := Schedule{
		ID:        uuid.New(),
		TenantID:  tenantID,
		SiteID:    siteID,
		Cadence:   CadenceDaily,
		Kind:      KindFull,
		NextRunAt: time.Now(),
	}
	if err := svc.EnqueueScheduledBackup(context.Background(), sched); err != nil {
		t.Fatalf("EnqueueScheduledBackup error: %v", err)
	}
	if repo.createInputs[0].DestinationID != uuid.Nil {
		t.Errorf("DestinationID = %v, want uuid.Nil (managed/legacy path)", repo.createInputs[0].DestinationID)
	}
}

// ---------------------------------------------------------------------------
// BackupWorker.Work — DestinationKind/DestinationConfig threading onto the
// CP->agent BackupRequest.
// ---------------------------------------------------------------------------

func TestBackupWorker_DestinationKindLocal_DispatchesConfig(t *testing.T) {
	repo := &fakeWorkerRepo{fakeRepo: newFakeRepo()}
	cmd := &fakeCommander{ok: true}
	tenantID := uuid.New()
	snapshotID := uuid.New()
	destID := uuid.New()

	snap := Snapshot{
		ID:            snapshotID,
		TenantID:      tenantID,
		SiteID:        uuid.New(),
		Kind:          KindFull,
		Status:        StatusPending,
		AgeRecipient:  "age1ql3z7hjy54pw3hyww5ayyfg7zqgvc7w3j2elw8zmrj2kg5sfn9aqmcac8p",
		DestinationID: destID,
	}
	repo.setSnapshot(snap)

	dl := newFakeDestLookup()
	dl.infoByID[destID] = DestinationInfo{ID: destID, Kind: DestinationKindLocal, PathPrefix: "wpmgr-backups-custom"}

	svc := &Service{repo: repo, sites: fakeWorkerSiteLookup{}, clock: fakeClock{t: time.Now()}, destLookup: dl}
	worker := NewBackupWorker(svc, cmd, nil, nil, "https://cp.example.com", 0)
	job := &river.Job[BackupArgs]{Args: BackupArgs{TenantID: tenantID, SnapshotID: snapshotID}}

	if err := worker.Work(context.Background(), job); err != nil {
		t.Fatalf("Work() error: %v", err)
	}
	if cmd.lastBackup == nil {
		t.Fatal("expected Backup to be called")
	}
	if cmd.lastBackup.DestinationKind != DestinationKindLocal {
		t.Errorf("DestinationKind = %q, want %q", cmd.lastBackup.DestinationKind, DestinationKindLocal)
	}
	if cmd.lastBackup.DestinationConfig.LocalPathPrefix != "wpmgr-backups-custom" {
		t.Errorf("LocalPathPrefix = %q, want wpmgr-backups-custom", cmd.lastBackup.DestinationConfig.LocalPathPrefix)
	}
}

func TestBackupWorker_ManagedDestination_EmitsCPKind(t *testing.T) {
	repo := &fakeWorkerRepo{fakeRepo: newFakeRepo()}
	cmd := &fakeCommander{ok: true}
	tenantID := uuid.New()
	snapshotID := uuid.New()

	snap := Snapshot{
		ID:           snapshotID,
		TenantID:     tenantID,
		SiteID:       uuid.New(),
		Kind:         KindFull,
		Status:       StatusPending,
		AgeRecipient: "age1ql3z7hjy54pw3hyww5ayyfg7zqgvc7w3j2elw8zmrj2kg5sfn9aqmcac8p",
		// DestinationID intentionally zero (uuid.Nil) — the managed/legacy path.
	}
	repo.setSnapshot(snap)

	// destLookup intentionally NOT wired — proves the managed dispatch never
	// depends on it.
	svc := &Service{repo: repo, sites: fakeWorkerSiteLookup{}, clock: fakeClock{t: time.Now()}}
	worker := NewBackupWorker(svc, cmd, nil, nil, "https://cp.example.com", 0)
	job := &river.Job[BackupArgs]{Args: BackupArgs{TenantID: tenantID, SnapshotID: snapshotID}}

	if err := worker.Work(context.Background(), job); err != nil {
		t.Fatalf("Work() error: %v", err)
	}
	if cmd.lastBackup == nil {
		t.Fatal("expected Backup to be called")
	}
	if cmd.lastBackup.DestinationKind != DestinationKindCP {
		t.Errorf("DestinationKind = %q, want %q (managed/legacy default)", cmd.lastBackup.DestinationKind, DestinationKindCP)
	}
	if cmd.lastBackup.DestinationConfig.LocalPathPrefix != "" {
		t.Errorf("expected empty DestinationConfig for the managed destination, got %+v", cmd.lastBackup.DestinationConfig)
	}
}

// ---------------------------------------------------------------------------
// PlanRestore (non-chain path) — destination-aware chunk fetch.
// ---------------------------------------------------------------------------

func TestPlanRestore_LocalDestination_SkipsPresign(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()
	snapID := uuid.New()
	destID := uuid.New()

	repo := &fakeWorkerRepo{fakeRepo: newFakeRepo(), workerManifests: map[uuid.UUID][]ManifestEntry{}}
	snap := Snapshot{
		ID:            snapID,
		TenantID:      tenantID,
		SiteID:        siteID,
		Kind:          KindFull,
		Status:        StatusCompleted,
		AgeRecipient:  "age1test",
		DestinationID: destID,
	}
	repo.setSnapshot(snap)
	repo.workerManifests[snapID] = []ManifestEntry{
		{Path: "wp-content/plugins/foo/foo.php", EntryKind: EntryKindFile, ChunkHashes: []string{"aaa"}, Size: 100},
	}

	dl := newFakeDestLookup()
	dl.infoByID[destID] = DestinationInfo{ID: destID, Kind: DestinationKindLocal, PathPrefix: "custom/backups"}

	svc := &Service{
		repo:       repo,
		sites:      &fakeSiteLookup{},
		clock:      fakeClock{t: time.Now()},
		presignTTL: time.Hour,
		destLookup: dl,
	}

	plan, _, _, err := svc.PlanRestore(context.Background(), tenantID, snapID, RestoreSelection{Full: true}, "restore-local", "https://cp.test/progress")
	if err != nil {
		t.Fatalf("PlanRestore error: %v", err)
	}
	if plan.DestinationKind != DestinationKindLocal {
		t.Errorf("DestinationKind = %q, want %q", plan.DestinationKind, DestinationKindLocal)
	}
	if plan.DestinationConfig.LocalPathPrefix != "custom/backups" {
		t.Errorf("LocalPathPrefix = %q, want custom/backups", plan.DestinationConfig.LocalPathPrefix)
	}
	if len(plan.Manifest.Entries) != 1 || len(plan.Manifest.Entries[0].Chunks) != 1 {
		t.Fatalf("expected 1 entry/1 chunk, got %+v", plan.Manifest.Entries)
	}
	chunk := plan.Manifest.Entries[0].Chunks[0]
	if chunk.Hash != "aaa" {
		t.Errorf("chunk hash = %q, want aaa", chunk.Hash)
	}
	if chunk.URL != "" {
		t.Errorf("expected NO presigned URL for a local destination, got %q", chunk.URL)
	}
}

// TestPlanRestore_ManagedDestination_Unchanged is the CRITICAL regression
// proof: a snapshot with DestinationID == uuid.Nil AND an unwired destLookup
// presigns via the legacy s.store path exactly as it did before ADR-036 P1 —
// one presign call, a non-empty URL, and DestinationKind reported as "cp".
func TestPlanRestore_ManagedDestination_Unchanged(t *testing.T) {
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
		// DestinationID intentionally zero (uuid.Nil).
	}
	repo.setSnapshot(snap)
	repo.workerManifests[snapID] = []ManifestEntry{
		{Path: "wp-content/plugins/foo/foo.php", EntryKind: EntryKindFile, ChunkHashes: []string{"aaa"}, Size: 100},
	}

	fp := &fakePresigner{}
	svc := &Service{
		repo:       repo,
		sites:      &fakeSiteLookup{},
		store:      &tenantPresigner{tenantID: tenantID, inner: fp},
		clock:      fakeClock{t: time.Now()},
		presignTTL: time.Hour,
		// destLookup intentionally NOT wired (nil) — proves the managed path
		// never depends on it.
	}

	plan, _, _, err := svc.PlanRestore(context.Background(), tenantID, snapID, RestoreSelection{Full: true}, "restore-managed", "https://cp.test/progress")
	if err != nil {
		t.Fatalf("PlanRestore error: %v", err)
	}
	if plan.DestinationKind != DestinationKindCP {
		t.Errorf("DestinationKind = %q, want %q (managed/legacy)", plan.DestinationKind, DestinationKindCP)
	}
	if plan.DestinationConfig.LocalPathPrefix != "" {
		t.Errorf("expected empty LocalPathPrefix for the managed destination, got %q", plan.DestinationConfig.LocalPathPrefix)
	}
	if len(plan.Manifest.Entries) != 1 || len(plan.Manifest.Entries[0].Chunks) != 1 {
		t.Fatalf("expected 1 entry/1 chunk, got %+v", plan.Manifest.Entries)
	}
	chunk := plan.Manifest.Entries[0].Chunks[0]
	if chunk.URL == "" {
		t.Error("expected a presigned URL for the managed (cp) destination")
	}
	if fp.calls != 1 {
		t.Errorf("expected exactly 1 presign call, got %d", fp.calls)
	}
}

// ---------------------------------------------------------------------------
// PresignParentFilesList (ADR-051 incremental diff-source) — the PARENT
// snapshot's OWN destination must drive routing, not the increment currently
// being dispatched.
// ---------------------------------------------------------------------------

// fakeRegistry is a minimal PresignerForSnapshot double: it returns a distinct
// Presigner keyed by DestinationID (uuid.Nil routes to defaultStore), so a
// test can prove a call routed to the destination-specific presigner and NOT
// the CP-global default — the same routing contract blobstore.Registry
// implements in production (see internal/blobstore/registry_test.go for the
// real bucket-selection proof).
type fakeRegistry struct {
	byDestination map[uuid.UUID]Presigner
	defaultStore  Presigner
}

func (r *fakeRegistry) PresignerForSnapshot(_ context.Context, snap Snapshot) (Presigner, error) {
	if snap.DestinationID == uuid.Nil {
		return r.defaultStore, nil
	}
	if p, ok := r.byDestination[snap.DestinationID]; ok {
		return p, nil
	}
	return r.defaultStore, nil
}

func TestPresignParentFilesList_S3CompatParent_RoutesToCustomerPresigner(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()
	parentID := uuid.New()
	destID := uuid.New()
	flHash := "abc123def456abc123def456abc123def456abc123def456abc123def456abcd"

	repo := &fakeWorkerRepo{fakeRepo: newFakeRepo(), workerManifests: map[uuid.UUID][]ManifestEntry{}}
	repo.setSnapshot(Snapshot{
		ID:            parentID,
		TenantID:      tenantID,
		SiteID:        siteID,
		Kind:          KindFull,
		Status:        StatusCompleted,
		AgeRecipient:  "age1test",
		DestinationID: destID,
	})
	repo.workerManifests[parentID] = []ManifestEntry{
		{Path: "files.list", EntryKind: EntryKindFilesList, ChunkHashes: []string{flHash}, Size: 200},
	}

	defaultFP := &fakePresigner{}
	customerFP := &fakePresigner{}
	registry := &fakeRegistry{
		defaultStore: &tenantPresigner{tenantID: tenantID, inner: defaultFP},
		byDestination: map[uuid.UUID]Presigner{
			destID: &tenantPresigner{tenantID: tenantID, inner: customerFP},
		},
	}

	dl := newFakeDestLookup()
	dl.infoByID[destID] = DestinationInfo{ID: destID, Kind: DestinationKindS3Compat}

	svc := &Service{
		repo:       repo,
		sites:      &fakeSiteLookup{},
		store:      &tenantPresigner{tenantID: tenantID, inner: defaultFP}, // legacy fallback; must NOT be used
		registry:   registry,
		clock:      fakeClock{t: time.Now()},
		presignTTL: time.Hour,
		destLookup: dl,
	}

	chunks, err := svc.PresignParentFilesList(context.Background(), tenantID, parentID)
	if err != nil {
		t.Fatalf("PresignParentFilesList error: %v", err)
	}
	if len(chunks) != 1 || chunks[0].Hash != flHash {
		t.Fatalf("expected 1 chunk %q, got %+v", flHash, chunks)
	}
	if chunks[0].URL == "" {
		t.Error("expected a presigned URL for an s3_compat parent")
	}
	if customerFP.calls != 1 {
		t.Errorf("expected exactly 1 presign call on the CUSTOMER presigner, got %d", customerFP.calls)
	}
	if defaultFP.calls != 0 {
		t.Errorf("expected the CP-global default presigner NOT to be called, got %d calls", defaultFP.calls)
	}
}

func TestPresignParentFilesList_LocalParent_SkipsPresign(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()
	parentID := uuid.New()
	destID := uuid.New()
	flHash := "def456abc123def456abc123def456abc123def456abc123def456abc123def4"

	repo := &fakeWorkerRepo{fakeRepo: newFakeRepo(), workerManifests: map[uuid.UUID][]ManifestEntry{}}
	repo.setSnapshot(Snapshot{
		ID:            parentID,
		TenantID:      tenantID,
		SiteID:        siteID,
		Kind:          KindFull,
		Status:        StatusCompleted,
		AgeRecipient:  "age1test",
		DestinationID: destID,
	})
	repo.workerManifests[parentID] = []ManifestEntry{
		{Path: "files.list", EntryKind: EntryKindFilesList, ChunkHashes: []string{flHash}, Size: 200},
	}

	dl := newFakeDestLookup()
	dl.infoByID[destID] = DestinationInfo{ID: destID, Kind: DestinationKindLocal, PathPrefix: "custom/backups"}

	// registry/store intentionally nil — a local parent must never need
	// either (StoreForSnapshot would hard-error for a local destination).
	svc := &Service{
		repo:       repo,
		sites:      &fakeSiteLookup{},
		clock:      fakeClock{t: time.Now()},
		presignTTL: time.Hour,
		destLookup: dl,
	}

	chunks, err := svc.PresignParentFilesList(context.Background(), tenantID, parentID)
	if err != nil {
		t.Fatalf("PresignParentFilesList error: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	if chunks[0].Hash != flHash {
		t.Errorf("Hash = %q, want %q", chunks[0].Hash, flHash)
	}
	if chunks[0].URL != "" {
		t.Errorf("expected NO presigned URL for a local parent, got %q", chunks[0].URL)
	}
}
