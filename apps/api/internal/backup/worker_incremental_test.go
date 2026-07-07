package backup

// worker_incremental_test.go — unit tests for BackupWorker.Work dispatch with
// ADR-048 incremental fields. Uses mock/stub doubles; no database required.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// ---------------------------------------------------------------------------
// fakeCommander records what was dispatched.
// ---------------------------------------------------------------------------

type fakeCommander struct {
	lastBackup            *agentcmd.BackupRequest
	lastIncrementalBackup *agentcmd.IncrementalBackupRequest
	ok                    bool
}

func (f *fakeCommander) Backup(_ context.Context, _ uuid.UUID, _ string, req agentcmd.BackupRequest) (agentcmd.BackupResponse, error) {
	f.lastBackup = &req
	return agentcmd.BackupResponse{OK: f.ok}, nil
}

func (f *fakeCommander) IncrementalBackup(_ context.Context, _ uuid.UUID, _ string, req agentcmd.IncrementalBackupRequest) (agentcmd.BackupResponse, error) {
	f.lastIncrementalBackup = &req
	return agentcmd.BackupResponse{OK: f.ok}, nil
}

func (f *fakeCommander) Restore(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.RestoreRequest) (agentcmd.RestoreResponse, error) {
	return agentcmd.RestoreResponse{}, nil
}

// ---------------------------------------------------------------------------
// fakeWorkerRepo embeds fakeRepo and adds MarkSnapshotRunning/FailSnapshot.
// ---------------------------------------------------------------------------

type fakeWorkerRepo struct {
	*fakeRepo
	markRunningCalled bool
	workerManifests   map[uuid.UUID][]ManifestEntry
}

func (r *fakeWorkerRepo) MarkSnapshotRunning(_ context.Context, _, snapshotID uuid.UUID) (Snapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.markRunningCalled = true
	s, ok := r.snapshots[snapshotID]
	if !ok {
		return Snapshot{}, domain.NotFound("backup_snapshot_not_found", "not found")
	}
	s.Status = StatusRunning
	r.snapshots[snapshotID] = s
	return s, nil
}

func (r *fakeWorkerRepo) FailSnapshot(_ context.Context, _, snapshotID uuid.UUID, msg string) (Snapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.snapshots[snapshotID]
	s.Status = StatusFailed
	s.Error = msg
	r.snapshots[snapshotID] = s
	return s, nil
}

// manifests lets a worker test register a parent snapshot's manifest entries so
// the ADR-051 dispatch can resolve + presign its files-list chunks.
func (r *fakeWorkerRepo) ListManifest(_ context.Context, _, snapshotID uuid.UUID) ([]ManifestEntry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.workerManifests[snapshotID], nil
}

func (r *fakeWorkerRepo) ExistingChunkHashes(_ context.Context, tenantID uuid.UUID, hashes []string) (map[string]Chunk, error) {
	out := map[string]Chunk{}
	for _, h := range hashes {
		out[h] = Chunk{Blake3: h, S3Key: chunkS3Key(tenantID, h), Size: 64}
	}
	return out, nil
}
func (r *fakeWorkerRepo) SetSnapshotLocked(_ context.Context, _, id uuid.UUID, locked bool) (Snapshot, error) {
	s := r.fakeRepo.snapshots[id]
	s.Locked = locked
	r.fakeRepo.snapshots[id] = s
	return s, nil
}

// fakeWorkerSiteLookup returns a fixed enrolled site.
type fakeWorkerSiteLookup struct{}

func (fakeWorkerSiteLookup) GetBackupSiteInfo(_ context.Context, _, _ uuid.UUID) (SiteInfo, error) {
	return SiteInfo{
		ID:           uuid.New(),
		URL:          "https://example.com",
		Enrolled:     true,
		AgeRecipient: "age1ql3z7hjy54pw3hyww5ayyfg7zqgvc7w3j2elw8zmrj2kg5sfn9aqmcac8p",
	}, nil
}
func (fakeWorkerSiteLookup) ListSiteIDs(_ context.Context, _ uuid.UUID) ([]uuid.UUID, error) {
	return nil, nil
}

// ---------------------------------------------------------------------------
// TestBackupWorker_DispatchesFullRequest — when IsIncremental=false (or zero),
// Work() calls cmd.Backup (NOT cmd.IncrementalBackup).
// ---------------------------------------------------------------------------

func TestBackupWorker_DispatchesFullRequest(t *testing.T) {
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
	}
	repo.setSnapshot(snap)

	svc := &Service{repo: repo, sites: fakeWorkerSiteLookup{}, clock: fakeClock{t: time.Now()}}

	worker := NewBackupWorker(svc, cmd, nil, nil, "https://cp.example.com", 0)
	job := &river.Job[BackupArgs]{
		Args: BackupArgs{
			TenantID:      tenantID,
			SnapshotID:    snapshotID,
			IsIncremental: false,
		},
	}
	if err := worker.Work(context.Background(), job); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	if cmd.lastIncrementalBackup != nil {
		t.Error("expected IncrementalBackup NOT to be called for a full backup job")
	}
	if cmd.lastBackup == nil {
		t.Fatal("expected Backup to be called")
	}
	if cmd.lastBackup.SnapshotID != snapshotID.String() {
		t.Errorf("snapshot_id mismatch: got %q", cmd.lastBackup.SnapshotID)
	}
}

// ---------------------------------------------------------------------------
// TestBackupWorker_DispatchesIncrementalRequest — when IsIncremental=true,
// Work() calls cmd.IncrementalBackup with is_incremental=true and a non-empty
// file_index_endpoint.
// ---------------------------------------------------------------------------

func TestBackupWorker_DispatchesIncrementalRequest(t *testing.T) {
	repo := &fakeWorkerRepo{fakeRepo: newFakeRepo(), workerManifests: map[uuid.UUID][]ManifestEntry{}}
	cmd := &fakeCommander{ok: true}
	tenantID := uuid.New()
	snapshotID := uuid.New()
	parentID := uuid.New()
	baseID := uuid.New()
	chainID := uuid.New()

	snap := Snapshot{
		ID:               snapshotID,
		TenantID:         tenantID,
		SiteID:           uuid.New(),
		Kind:             KindFull,
		Status:           StatusPending,
		AgeRecipient:     "age1ql3z7hjy54pw3hyww5ayyfg7zqgvc7w3j2elw8zmrj2kg5sfn9aqmcac8p",
		IsIncremental:    true,
		ParentSnapshotID: &parentID,
		BaseSnapshotID:   &baseID,
		ChainID:          &chainID,
		Generation:       1,
	}
	repo.setSnapshot(snap)
	// ADR-036 P1 (GH #146): PresignParentFilesList now loads the parent
	// snapshot's own row to resolve ITS destination routing — register a
	// completed parent (DestinationID zero == managed/cp, same as the
	// increment above) so the lookup succeeds.
	repo.setSnapshot(Snapshot{
		ID:           parentID,
		TenantID:     tenantID,
		SiteID:       snap.SiteID,
		Kind:         KindFull,
		Status:       StatusCompleted,
		AgeRecipient: snap.AgeRecipient,
	})
	// ADR-051: the parent must carry a files-list manifest entry so the dispatch
	// can presign its chunks for the agent's prev-map.
	flHash := "abc123def456abc123def456abc123def456abc123def456abc123def456abcd"
	repo.workerManifests[parentID] = []ManifestEntry{
		{Path: "files.list", EntryKind: EntryKindFilesList, ChunkHashes: []string{flHash}, Size: 200},
	}

	fp := &fakePresigner{}
	svc := &Service{
		repo:       repo,
		sites:      fakeWorkerSiteLookup{},
		store:      &tenantPresigner{tenantID: tenantID, inner: fp},
		clock:      fakeClock{t: time.Now()},
		presignTTL: time.Hour,
	}

	worker := NewBackupWorker(svc, cmd, nil, nil, "https://cp.example.com", 0)
	job := &river.Job[BackupArgs]{
		Args: BackupArgs{
			TenantID:         tenantID,
			SnapshotID:       snapshotID,
			IsIncremental:    true,
			ParentSnapshotID: parentID,
			BaseSnapshotID:   baseID,
			ChainID:          chainID,
			Generation:       1,
		},
	}
	if err := worker.Work(context.Background(), job); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	if cmd.lastBackup != nil {
		t.Error("expected Backup NOT to be called for an incremental job")
	}
	if cmd.lastIncrementalBackup == nil {
		t.Fatal("expected IncrementalBackup to be called")
	}
	req := cmd.lastIncrementalBackup
	if !req.IsIncremental {
		t.Error("IncrementalBackupRequest.IsIncremental must be true")
	}
	if req.ParentSnapshotID != parentID.String() {
		t.Errorf("ParentSnapshotID mismatch: got %q", req.ParentSnapshotID)
	}
	if req.Generation != 1 {
		t.Errorf("Generation must be 1, got %d", req.Generation)
	}
	// ADR-051: the dispatch carries the parent's presigned files-list chunks
	// (replacing the retired file_index_endpoint) so the agent can rebuild the
	// prev[rel]=>{size,mtime} map.
	if len(req.PrevFilesListChunks) != 1 {
		t.Fatalf("expected 1 prev files-list chunk, got %d", len(req.PrevFilesListChunks))
	}
	if req.PrevFilesListChunks[0].Hash != flHash {
		t.Errorf("prev files-list chunk hash mismatch: got %q", req.PrevFilesListChunks[0].Hash)
	}
	if req.PrevFilesListChunks[0].URL == "" {
		t.Error("prev files-list chunk must carry a presigned URL")
	}
}

// ---------------------------------------------------------------------------
// TestBackupWorker_DispatchesBaseIncrement — a no-parent gen-0 base-increment
// (ADR-048 bootstrap) takes the IncrementalBackup path with an EMPTY
// file_index_endpoint, which the agent treats as "scan everything as new".
// ---------------------------------------------------------------------------

func TestBackupWorker_DispatchesBaseIncrement(t *testing.T) {
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
		// gen-0 base-increment: no parent/base/chain.
		IsIncremental: true,
		Generation:    0,
	}
	repo.setSnapshot(snap)

	svc := &Service{repo: repo, sites: fakeWorkerSiteLookup{}, clock: fakeClock{t: time.Now()}}

	worker := NewBackupWorker(svc, cmd, nil, nil, "https://cp.example.com", 0)
	job := &river.Job[BackupArgs]{
		Args: BackupArgs{
			TenantID:      tenantID,
			SnapshotID:    snapshotID,
			IsIncremental: true,
			Generation:    0,
			// ParentSnapshotID/BaseSnapshotID/ChainID intentionally zero.
		},
	}
	if err := worker.Work(context.Background(), job); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	if cmd.lastBackup != nil {
		t.Error("expected Backup NOT to be called for a gen-0 base-increment")
	}
	if cmd.lastIncrementalBackup == nil {
		t.Fatal("expected IncrementalBackup to be called for a gen-0 base-increment")
	}
	req := cmd.lastIncrementalBackup
	if !req.IsIncremental {
		t.Error("IncrementalBackupRequest.IsIncremental must be true")
	}
	if req.Generation != 0 {
		t.Errorf("Generation must be 0 for a base, got %d", req.Generation)
	}
	// ADR-051: a no-parent base-increment carries NO prev files-list chunks (the
	// empty list is the base signal — scan everything as new).
	if len(req.PrevFilesListChunks) != 0 {
		t.Errorf("PrevFilesListChunks must be EMPTY for a no-parent base, got %d", len(req.PrevFilesListChunks))
	}
	// ParentSnapshotID is the zero UUID stringified for a base; the empty prev
	// files-list is what signals the base scan to the agent.
	if req.ParentSnapshotID != uuid.Nil.String() {
		t.Errorf("expected nil-UUID parent for a base, got %q", req.ParentSnapshotID)
	}
}

// ---------------------------------------------------------------------------
// TestBackupWorker_SelectiveComponents_IncludeDB — MUST-FIX #187 LOW:
// verifies the CP->agent path for selective-component backup.
//
// A schedule with BackupComponents = ["plugin","db"] must produce a
// BackupRequest where:
//   - Components = ["plugin","db"]  (singular vocabulary)
//   - IncludeDB  = &true           (explicit DB-inclusion signal)
//
// A schedule with BackupComponents = ["plugin","theme"] (no "db") must produce:
//   - Components = ["plugin","theme"]
//   - IncludeDB  = &false          (explicit DB-exclusion signal)
//
// A schedule with no BackupComponents (nil) must produce:
//   - Components = nil/empty
//   - IncludeDB  = nil             (no filter; agent follows snapshot kind)
// ---------------------------------------------------------------------------

// fakeSettingsWorkerRepo wraps fakeWorkerRepo and returns preset backup settings.
// After m50, scheduleBackupScope reads from GetBackupSettings (site_backup_settings),
// not GetSchedule, so the fake implements GetBackupSettings instead.
type fakeSettingsWorkerRepo struct {
	*fakeWorkerRepo
	settings *SiteBackupSettings
}

func (r *fakeSettingsWorkerRepo) GetBackupSettings(_ context.Context, _, _ uuid.UUID) (SiteBackupSettings, error) {
	if r.settings == nil {
		return SiteBackupSettings{}, domain.NotFound("backup_settings_not_found", "not found")
	}
	return *r.settings, nil
}

func (r *fakeSettingsWorkerRepo) UpsertBackupSettings(_ context.Context, _ uuid.UUID, in SiteBackupSettings) (SiteBackupSettings, error) {
	r.settings = &in
	return in, nil
}

func TestBackupWorker_SelectiveComponents_IncludeDB(t *testing.T) {
	falsePtr := func() *bool { v := false; return &v }
	truePtr := func() *bool { v := true; return &v }

	tests := []struct {
		name           string
		components     []string
		wantComponents []string
		wantIncludeDB  *bool
	}{
		{
			name:           "plugin+db => IncludeDB=true",
			components:     []string{EntryKindPlugin, KindDB},
			wantComponents: []string{EntryKindPlugin, KindDB},
			wantIncludeDB:  truePtr(),
		},
		{
			name:           "plugin+theme (no db) => IncludeDB=false",
			components:     []string{EntryKindPlugin, EntryKindTheme},
			wantComponents: []string{EntryKindPlugin, EntryKindTheme},
			wantIncludeDB:  falsePtr(),
		},
		{
			name:           "nil components => IncludeDB=nil",
			components:     nil,
			wantComponents: nil,
			wantIncludeDB:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inner := &fakeWorkerRepo{fakeRepo: newFakeRepo()}
			var settingsPtr *SiteBackupSettings
			if tt.components != nil {
				s := SiteBackupSettings{BackupComponents: tt.components}
				settingsPtr = &s
			}
			repo := &fakeSettingsWorkerRepo{
				fakeWorkerRepo: inner,
				settings:       settingsPtr,
			}
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
				IsIncremental: false,
			}
			inner.setSnapshot(snap)

			svc := &Service{repo: repo, sites: fakeWorkerSiteLookup{}, clock: fakeClock{t: time.Now()}}
			worker := NewBackupWorker(svc, cmd, nil, nil, "https://cp.example.com", 0)

			job := &river.Job[BackupArgs]{
				Args: BackupArgs{
					TenantID:   tenantID,
					SnapshotID: snapshotID,
				},
			}
			if err := worker.Work(context.Background(), job); err != nil {
				t.Fatalf("Work() error: %v", err)
			}
			if cmd.lastBackup == nil {
				t.Fatal("expected Backup to be called")
			}
			req := cmd.lastBackup

			// Assert components (nil vs non-nil handled gracefully).
			if len(tt.wantComponents) == 0 {
				if len(req.Components) != 0 {
					t.Errorf("want no components, got %v", req.Components)
				}
			} else {
				if len(req.Components) != len(tt.wantComponents) {
					t.Errorf("components length mismatch: got %v, want %v", req.Components, tt.wantComponents)
				} else {
					got := map[string]bool{}
					for _, c := range req.Components {
						got[c] = true
					}
					for _, c := range tt.wantComponents {
						if !got[c] {
							t.Errorf("component %q missing from request; got %v", c, req.Components)
						}
					}
				}
			}

			// Assert include_db signal.
			if tt.wantIncludeDB == nil {
				if req.IncludeDB != nil {
					t.Errorf("IncludeDB: want nil, got %v", *req.IncludeDB)
				}
			} else {
				if req.IncludeDB == nil {
					t.Errorf("IncludeDB: want %v, got nil", *tt.wantIncludeDB)
				} else if *req.IncludeDB != *tt.wantIncludeDB {
					t.Errorf("IncludeDB: want %v, got %v", *tt.wantIncludeDB, *req.IncludeDB)
				}
			}
		})
	}
}

// ADR-051: the agent-facing GET /file-index NDJSON endpoint + its soft-cap are
// RETIRED (change detection now rides on the parent's presigned files-list
// chunks). The endpoint streaming tests that exercised them have been removed.
