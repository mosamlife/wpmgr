package backup

// restore_chain_test.go — table-driven unit tests for ADR-049 chain-restore
// planner (PlanRestore chain path). Uses fakeRepo and fakePresigner stubs;
// no database or network required.

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// ---------------------------------------------------------------------------
// chainFakeRepo — extends fakeRepo with the chain-restore methods needed for
// ADR-049 tests. All pre-existing fakeRepo methods are inherited unchanged.
// ---------------------------------------------------------------------------

type chainFakeRepo struct {
	*fakeRepo
	// chainSnaps maps chainID -> ordered []Snapshot (by generation).
	chainSnaps map[uuid.UUID][]Snapshot
	// manifests maps snapshotID -> []ManifestEntry.
	manifests map[uuid.UUID][]ManifestEntry
	// existingChunks maps blake3 hash -> Chunk.
	existingChunks map[string]Chunk
	// presignCalls counts how many times PresignGet was called on the presigner.
	presignCalls int
}

func newChainFakeRepo() *chainFakeRepo {
	return &chainFakeRepo{
		fakeRepo:       newFakeRepo(),
		chainSnaps:     make(map[uuid.UUID][]Snapshot),
		manifests:      make(map[uuid.UUID][]ManifestEntry),
		existingChunks: make(map[string]Chunk),
	}
}

// addChainSnap registers a snapshot as part of a chain and in the snapshots map.
func (r *chainFakeRepo) addChainSnap(chainID uuid.UUID, s Snapshot) {
	r.fakeRepo.setSnapshot(s)
	r.chainSnaps[chainID] = append(r.chainSnaps[chainID], s)
}

// addManifest registers manifest entries for a snapshot.
func (r *chainFakeRepo) addManifest(snapshotID uuid.UUID, entries []ManifestEntry) {
	r.manifests[snapshotID] = entries
}

// addChunk registers a chunk as existing (present in object storage).
func (r *chainFakeRepo) addChunk(hash string) {
	key := "chunks/tenant/" + hash // tenant prefix is a test convention
	r.existingChunks[hash] = Chunk{Blake3: hash, S3Key: key, Size: 128}
}

// ListChainSnapshots returns stored chain snaps up to maxGeneration, ordered
// by generation ASC. Simulates the DB query faithfully.
func (r *chainFakeRepo) ListChainSnapshots(_ context.Context, _ uuid.UUID, chainID uuid.UUID, maxGeneration int) ([]Snapshot, error) {
	all := r.chainSnaps[chainID]
	var out []Snapshot
	for _, s := range all {
		if s.Generation <= maxGeneration {
			out = append(out, s)
		}
	}
	return out, nil
}

// ListCompletedChainSnapshots mirrors ListChainSnapshots filtered to
// status==completed (GH #168), matching the real repo's hardened variant that
// reachableChunks uses. Every fixture snapshot in this file's tests is built
// via mkSnap (always StatusCompleted), so this is behaviourally identical to
// ListChainSnapshots for the existing chain-restore suite; TC-6 deliberately
// constructs a non-completed row but exercises PlanRestore's OWN separate
// ListChainSnapshots call (CHECK 1/2), which runs and errors out BEFORE
// reachableChunks (and therefore this method) is ever reached.
func (r *chainFakeRepo) ListCompletedChainSnapshots(_ context.Context, _ uuid.UUID, chainID uuid.UUID, maxGeneration int) ([]Snapshot, error) {
	all := r.chainSnaps[chainID]
	var out []Snapshot
	for _, s := range all {
		if s.Generation <= maxGeneration && s.Status == StatusCompleted {
			out = append(out, s)
		}
	}
	return out, nil
}

// StreamChainEffectiveFileIndex mirrors the pgRepo merge over the fake's chain
// bookkeeping: walk generations 0..maxGeneration ascending, apply
// latest-version-wins (tombstone => delete), then emit surviving entries sorted
// by file_path. Used by the chain-merged file-index endpoint tests.
func (r *chainFakeRepo) StreamChainEffectiveFileIndex(ctx context.Context, tenantID, chainID uuid.UUID, maxGeneration int, fn func(FileIndexEntry) error) error {
	if maxGeneration < 0 {
		maxGeneration = 0
	}
	chainSnaps, err := r.ListChainSnapshots(ctx, tenantID, chainID, maxGeneration)
	if err != nil {
		return err
	}
	byGen := map[int]Snapshot{}
	maxGen := -1
	for _, cs := range chainSnaps {
		byGen[cs.Generation] = cs
		if cs.Generation > maxGen {
			maxGen = cs.Generation
		}
	}
	winMap := map[string]*FileIndexEntry{}
	for gen := 0; gen <= maxGen; gen++ {
		cs, ok := byGen[gen]
		if !ok {
			continue
		}
		streamErr := r.StreamFileIndex(ctx, tenantID, cs.ID, func(e FileIndexEntry) error {
			eCopy := e
			if e.IsTombstone {
				delete(winMap, e.FilePath)
			} else {
				winMap[e.FilePath] = &eCopy
			}
			return nil
		})
		if streamErr != nil {
			return streamErr
		}
	}
	paths := make([]string, 0, len(winMap))
	for p := range winMap {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		if ferr := fn(*winMap[p]); ferr != nil {
			return ferr
		}
	}
	return nil
}

// ListManifest returns registered manifest entries for a snapshot.
func (r *chainFakeRepo) ListManifest(_ context.Context, _, snapshotID uuid.UUID) ([]ManifestEntry, error) {
	return r.manifests[snapshotID], nil
}

// HasFilesList reports whether the snapshot has a files-list manifest entry
// (ADR-051 archive-delta model detection).
func (r *chainFakeRepo) HasFilesList(_ context.Context, _, snapshotID uuid.UUID) (bool, error) {
	for _, e := range r.manifests[snapshotID] {
		if e.EntryKind == EntryKindFilesList {
			return true, nil
		}
	}
	return false, nil
}

// ExistingChunkHashes returns the subset of hashes that are registered.
func (r *chainFakeRepo) ExistingChunkHashes(_ context.Context, _ uuid.UUID, hashes []string) (map[string]Chunk, error) {
	out := make(map[string]Chunk)
	for _, h := range hashes {
		if c, ok := r.existingChunks[h]; ok {
			out[h] = c
		}
	}
	return out, nil
}
func (r *chainFakeRepo) SetSnapshotLocked(_ context.Context, _, id uuid.UUID, locked bool) (Snapshot, error) {
	if s, ok := r.fakeRepo.snapshots[id]; ok {
		s.Locked = locked
		r.fakeRepo.snapshots[id] = s
		return s, nil
	}
	return Snapshot{}, domain.NotFound("backup_snapshot_not_found", "not found")
}

// ---------------------------------------------------------------------------
// fakePresigner — counts presign calls and returns synthetic URLs.
// ---------------------------------------------------------------------------

type fakePresigner struct {
	calls int
	// forceFail makes PresignGet return an error when set.
	forceFail bool
}

func (p *fakePresigner) PresignPut(_ context.Context, key string, _ time.Duration) (string, error) {
	p.calls++
	return "https://storage.test/put/" + key, nil
}

func (p *fakePresigner) PresignGet(_ context.Context, key string, _ time.Duration) (string, error) {
	p.calls++
	if p.forceFail {
		return "", domain.Internal("presign_failed", "presign failed")
	}
	return "https://storage.test/get/" + key, nil
}

func (p *fakePresigner) Delete(_ context.Context, _ string) error { return nil }

// ---------------------------------------------------------------------------
// fakeSiteLookup — always returns an enrolled site.
// ---------------------------------------------------------------------------

type fakeSiteLookup struct{}

func (f *fakeSiteLookup) GetBackupSiteInfo(_ context.Context, _, _ uuid.UUID) (SiteInfo, error) {
	return SiteInfo{
		ID:           uuid.New(),
		URL:          "https://example.test",
		Enrolled:     true,
		AgeRecipient: "age1testrecipient",
	}, nil
}
func (f *fakeSiteLookup) ListSiteIDs(_ context.Context, _ uuid.UUID) ([]uuid.UUID, error) {
	return nil, nil
}

// ---------------------------------------------------------------------------
// buildChainSvc — builds a Service wired with the chain fakes.
// The presigner's chunk S3Key must match chunkS3Key(tenantID, hash) exactly.
// We use a tenantID-aware presigner wrapper so the key check passes.
// ---------------------------------------------------------------------------

type tenantPresigner struct {
	tenantID uuid.UUID
	inner    *fakePresigner
}

func (p *tenantPresigner) PresignPut(ctx context.Context, key string, ttl time.Duration) (string, error) {
	return p.inner.PresignPut(ctx, key, ttl)
}

func (p *tenantPresigner) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	return p.inner.PresignGet(ctx, key, ttl)
}

func (p *tenantPresigner) Delete(ctx context.Context, key string) error {
	return p.inner.Delete(ctx, key)
}

// buildChainSvc returns a Service backed by a chainFakeRepo for chain tests.
// The tenantID is used to build correct chunkS3Key values in existingChunks.
func buildChainSvc(repo *chainFakeRepo, tenantID uuid.UUID) (*Service, *fakePresigner) {
	fp := &fakePresigner{}
	// Fix the S3 keys in existingChunks to match chunkS3Key(tenantID, hash).
	for hash := range repo.existingChunks {
		c := repo.existingChunks[hash]
		c.S3Key = chunkS3Key(tenantID, hash)
		repo.existingChunks[hash] = c
	}
	svc := &Service{
		repo:       repo,
		sites:      &fakeSiteLookup{},
		store:      &tenantPresigner{tenantID: tenantID, inner: fp},
		clock:      fakeClock{t: time.Now()},
		presignTTL: time.Hour,
	}
	return svc, fp
}

// mkSnap is a helper that builds a minimal completed Snapshot.
func mkSnap(tenantID, siteID, chainID uuid.UUID, gen int, incremental bool) Snapshot {
	id := uuid.New()
	now := time.Now()
	cid := chainID
	return Snapshot{
		ID:            id,
		TenantID:      tenantID,
		SiteID:        siteID,
		Kind:          KindFull,
		Status:        StatusCompleted,
		AgeRecipient:  "age1test",
		IsIncremental: incremental,
		ChainID:       &cid,
		Generation:    gen,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
}

// ---------------------------------------------------------------------------
// TC-1: restore-to-latest (happy path, 3-generation chain)
// ---------------------------------------------------------------------------

func TestPlanRestoreChain_TC1_RestoreToLatest(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()
	chainID := uuid.New()

	gen0 := mkSnap(tenantID, siteID, chainID, 0, false)
	gen1 := mkSnap(tenantID, siteID, chainID, 1, true)
	gen2 := mkSnap(tenantID, siteID, chainID, 2, true)

	repo := newChainFakeRepo()
	repo.addChainSnap(chainID, gen0)
	repo.addChainSnap(chainID, gen1)
	repo.addChainSnap(chainID, gen2)

	// File index rows.
	repo.fileIndexRows[gen0.ID] = []FileIndexEntry{
		{FilePath: "wp-content/plugins/foo/foo.php", FileSize: 100, ChunkHashes: []string{"aaa"}, IsTombstone: false},
		{FilePath: "wp-content/themes/bar/style.css", FileSize: 200, ChunkHashes: []string{"bbb"}, IsTombstone: false},
	}
	repo.fileIndexRows[gen1.ID] = []FileIndexEntry{
		{FilePath: "wp-content/plugins/foo/foo.php", FileSize: 110, ChunkHashes: []string{"ccc"}, IsTombstone: false},
	}
	repo.fileIndexRows[gen2.ID] = []FileIndexEntry{
		{FilePath: "wp-content/themes/bar/style.css", FileSize: 220, ChunkHashes: []string{"ddd"}, IsTombstone: false},
	}

	// No DB entries for any snapshot.
	repo.addManifest(gen0.ID, nil)
	repo.addManifest(gen1.ID, nil)
	repo.addManifest(gen2.ID, nil)

	// All chunk hashes present.
	for _, h := range []string{"aaa", "bbb", "ccc", "ddd"} {
		repo.addChunk(h)
	}

	svc, _ := buildChainSvc(repo, tenantID)

	plan, snap, _, err := svc.PlanRestore(
		context.Background(), tenantID, gen2.ID,
		RestoreSelection{Full: true}, "restore-1", "https://cp.test/progress",
	)
	if err != nil {
		t.Fatalf("PlanRestore returned error: %v", err)
	}
	if !plan.IsChainRestore {
		t.Error("expected IsChainRestore=true")
	}
	if plan.TargetGeneration != 2 {
		t.Errorf("expected TargetGeneration=2, got %d", plan.TargetGeneration)
	}
	if len(plan.TombstonePaths) != 0 {
		t.Errorf("expected no tombstone paths, got %v", plan.TombstonePaths)
	}

	// Verify winning chunk assignments.
	entryByPath := make(map[string]agentcmd.RestoreEntry)
	for _, e := range plan.Manifest.Entries {
		entryByPath[e.LogicalPath] = e
	}
	fooEntry, ok := entryByPath["wp-content/plugins/foo/foo.php"]
	if !ok {
		t.Fatal("manifest missing wp-content/plugins/foo/foo.php")
	}
	if len(fooEntry.Chunks) != 1 || fooEntry.Chunks[0].Hash != "ccc" {
		t.Errorf("foo.php should have chunk 'ccc' (gen1 wins), got %+v", fooEntry.Chunks)
	}
	styleEntry, ok := entryByPath["wp-content/themes/bar/style.css"]
	if !ok {
		t.Fatal("manifest missing wp-content/themes/bar/style.css")
	}
	if len(styleEntry.Chunks) != 1 || styleEntry.Chunks[0].Hash != "ddd" {
		t.Errorf("style.css should have chunk 'ddd' (gen2 wins), got %+v", styleEntry.Chunks)
	}

	// EstimatedBytes should be sum of winning FileSize values.
	expectedBytes := int64(110 + 220)
	if plan.EstimatedBytes != expectedBytes {
		t.Errorf("EstimatedBytes: expected %d, got %d", expectedBytes, plan.EstimatedBytes)
	}
	_ = snap
}

// ---------------------------------------------------------------------------
// TC-2: restore-to-gen-1 (point-in-time)
// ---------------------------------------------------------------------------

func TestPlanRestoreChain_TC2_RestoreToGen1(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()
	chainID := uuid.New()

	gen0 := mkSnap(tenantID, siteID, chainID, 0, false)
	gen1 := mkSnap(tenantID, siteID, chainID, 1, true)
	gen2 := mkSnap(tenantID, siteID, chainID, 2, true)

	repo := newChainFakeRepo()
	repo.addChainSnap(chainID, gen0)
	repo.addChainSnap(chainID, gen1)
	repo.addChainSnap(chainID, gen2)

	repo.fileIndexRows[gen0.ID] = []FileIndexEntry{
		{FilePath: "wp-content/plugins/foo/foo.php", FileSize: 100, ChunkHashes: []string{"aaa"}},
		{FilePath: "wp-content/themes/bar/style.css", FileSize: 200, ChunkHashes: []string{"bbb"}},
	}
	repo.fileIndexRows[gen1.ID] = []FileIndexEntry{
		{FilePath: "wp-content/plugins/foo/foo.php", FileSize: 110, ChunkHashes: []string{"ccc"}},
	}
	repo.fileIndexRows[gen2.ID] = []FileIndexEntry{
		{FilePath: "wp-content/themes/bar/style.css", FileSize: 220, ChunkHashes: []string{"ddd"}},
	}
	repo.addManifest(gen0.ID, nil)
	repo.addManifest(gen1.ID, nil)
	repo.addManifest(gen2.ID, nil)

	for _, h := range []string{"aaa", "bbb", "ccc"} {
		repo.addChunk(h)
	}
	// "ddd" is NOT needed for gen1 restore; should not be queried/required.

	svc, _ := buildChainSvc(repo, tenantID)

	// Restore target is gen1.
	plan, _, _, err := svc.PlanRestore(
		context.Background(), tenantID, gen1.ID,
		RestoreSelection{Full: true}, "restore-2", "https://cp.test/progress",
	)
	if err != nil {
		t.Fatalf("PlanRestore returned error: %v", err)
	}
	if plan.TargetGeneration != 1 {
		t.Errorf("expected TargetGeneration=1, got %d", plan.TargetGeneration)
	}

	entryByPath := make(map[string]agentcmd.RestoreEntry)
	for _, e := range plan.Manifest.Entries {
		entryByPath[e.LogicalPath] = e
	}

	// foo.php: gen1 wins with "ccc".
	fooEntry := entryByPath["wp-content/plugins/foo/foo.php"]
	if len(fooEntry.Chunks) != 1 || fooEntry.Chunks[0].Hash != "ccc" {
		t.Errorf("expected foo.php chunk ccc at gen1, got %+v", fooEntry.Chunks)
	}
	// style.css: gen1 has no entry for it, so gen0 "bbb" should win.
	styleEntry := entryByPath["wp-content/themes/bar/style.css"]
	if len(styleEntry.Chunks) != 1 || styleEntry.Chunks[0].Hash != "bbb" {
		t.Errorf("expected style.css chunk bbb at gen1, got %+v", styleEntry.Chunks)
	}
}

// ---------------------------------------------------------------------------
// TC-3: deleted-file-is-absent (tombstone test)
// ---------------------------------------------------------------------------

func TestPlanRestoreChain_TC3_DeletedFile(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()
	chainID := uuid.New()

	gen0 := mkSnap(tenantID, siteID, chainID, 0, false)
	gen1 := mkSnap(tenantID, siteID, chainID, 1, true)

	repo := newChainFakeRepo()
	repo.addChainSnap(chainID, gen0)
	repo.addChainSnap(chainID, gen1)

	repo.fileIndexRows[gen0.ID] = []FileIndexEntry{
		{FilePath: "wp-content/plugins/old/plugin.php", FileSize: 50, ChunkHashes: []string{"eee"}},
	}
	repo.fileIndexRows[gen1.ID] = []FileIndexEntry{
		{FilePath: "wp-content/plugins/old/plugin.php", IsTombstone: true},
	}
	repo.addManifest(gen0.ID, nil)
	repo.addManifest(gen1.ID, nil)
	// "eee" chunk not needed since the file is tombstoned in gen1.

	svc, _ := buildChainSvc(repo, tenantID)

	plan, _, _, err := svc.PlanRestore(
		context.Background(), tenantID, gen1.ID,
		RestoreSelection{Full: true}, "restore-3", "https://cp.test/progress",
	)
	if err != nil {
		t.Fatalf("PlanRestore returned error: %v", err)
	}

	// Manifest should NOT contain the tombstoned file.
	for _, e := range plan.Manifest.Entries {
		if e.LogicalPath == "wp-content/plugins/old/plugin.php" {
			t.Error("tombstoned file should not appear in manifest entries")
		}
	}

	// TombstonePaths should contain the deleted file.
	found := false
	for _, p := range plan.TombstonePaths {
		if p == "wp-content/plugins/old/plugin.php" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected tombstone path, got %v", plan.TombstonePaths)
	}

	if !plan.IsChainRestore {
		t.Error("expected IsChainRestore=true")
	}
}

// ---------------------------------------------------------------------------
// TC-4: re-added file is NOT in tombstones
// ---------------------------------------------------------------------------

func TestPlanRestoreChain_TC4_ReAddedFile(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()
	chainID := uuid.New()

	gen0 := mkSnap(tenantID, siteID, chainID, 0, false)
	gen1 := mkSnap(tenantID, siteID, chainID, 1, true)
	gen2 := mkSnap(tenantID, siteID, chainID, 2, true)

	repo := newChainFakeRepo()
	repo.addChainSnap(chainID, gen0)
	repo.addChainSnap(chainID, gen1)
	repo.addChainSnap(chainID, gen2)

	repo.fileIndexRows[gen0.ID] = []FileIndexEntry{
		{FilePath: "a.php", FileSize: 30, ChunkHashes: []string{"f1"}},
	}
	repo.fileIndexRows[gen1.ID] = []FileIndexEntry{
		{FilePath: "a.php", IsTombstone: true},
	}
	repo.fileIndexRows[gen2.ID] = []FileIndexEntry{
		{FilePath: "a.php", FileSize: 35, ChunkHashes: []string{"f2"}},
	}
	repo.addManifest(gen0.ID, nil)
	repo.addManifest(gen1.ID, nil)
	repo.addManifest(gen2.ID, nil)
	repo.addChunk("f2")

	svc, _ := buildChainSvc(repo, tenantID)

	plan, _, _, err := svc.PlanRestore(
		context.Background(), tenantID, gen2.ID,
		RestoreSelection{Full: true}, "restore-4", "https://cp.test/progress",
	)
	if err != nil {
		t.Fatalf("PlanRestore returned error: %v", err)
	}

	// a.php should be in manifest with chunk f2.
	found := false
	for _, e := range plan.Manifest.Entries {
		if e.LogicalPath == "a.php" {
			found = true
			if len(e.Chunks) != 1 || e.Chunks[0].Hash != "f2" {
				t.Errorf("a.php should have chunk f2, got %+v", e.Chunks)
			}
		}
	}
	if !found {
		t.Error("a.php should be in manifest (re-added after tombstone)")
	}

	// a.php should NOT be in tombstone paths.
	for _, p := range plan.TombstonePaths {
		if p == "a.php" {
			t.Error("a.php should not be in TombstonePaths (re-added in gen2)")
		}
	}
}

// ---------------------------------------------------------------------------
// TC-5: missing generation rejected
// ---------------------------------------------------------------------------

func TestPlanRestoreChain_TC5_MissingGeneration(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()
	chainID := uuid.New()

	gen0 := mkSnap(tenantID, siteID, chainID, 0, false)
	// gen1 is deliberately omitted.
	gen2 := mkSnap(tenantID, siteID, chainID, 2, true)

	repo := newChainFakeRepo()
	repo.addChainSnap(chainID, gen0)
	repo.addChainSnap(chainID, gen2)
	repo.addManifest(gen0.ID, nil)
	repo.addManifest(gen2.ID, nil)

	fp := &fakePresigner{}
	svc := &Service{
		repo:       repo,
		sites:      &fakeSiteLookup{},
		store:      &tenantPresigner{tenantID: tenantID, inner: fp},
		clock:      fakeClock{t: time.Now()},
		presignTTL: time.Hour,
	}

	_, _, _, err := svc.PlanRestore(
		context.Background(), tenantID, gen2.ID,
		RestoreSelection{Full: true}, "restore-5", "",
	)
	if err == nil {
		t.Fatal("expected chain_integrity_violation error, got nil")
	}
	de, ok := domain.AsDomain(err)
	if !ok || de.Code != "chain_integrity_violation" {
		t.Errorf("expected chain_integrity_violation, got %v", err)
	}
	// No presign calls should have been made.
	if fp.calls > 0 {
		t.Errorf("expected 0 presign calls on integrity failure, got %d", fp.calls)
	}
}

// ---------------------------------------------------------------------------
// TC-6: non-completed generation rejected
// ---------------------------------------------------------------------------

func TestPlanRestoreChain_TC6_NonCompletedGeneration(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()
	chainID := uuid.New()

	gen0 := mkSnap(tenantID, siteID, chainID, 0, false)
	gen1 := mkSnap(tenantID, siteID, chainID, 1, true)
	gen1.Status = StatusRunning // not completed
	gen2 := mkSnap(tenantID, siteID, chainID, 2, true)

	repo := newChainFakeRepo()
	repo.addChainSnap(chainID, gen0)
	repo.addChainSnap(chainID, gen1)
	repo.addChainSnap(chainID, gen2)
	repo.addManifest(gen0.ID, nil)
	repo.addManifest(gen1.ID, nil)
	repo.addManifest(gen2.ID, nil)

	fp := &fakePresigner{}
	svc := &Service{
		repo:       repo,
		sites:      &fakeSiteLookup{},
		store:      &tenantPresigner{tenantID: tenantID, inner: fp},
		clock:      fakeClock{t: time.Now()},
		presignTTL: time.Hour,
	}

	_, _, _, err := svc.PlanRestore(
		context.Background(), tenantID, gen2.ID,
		RestoreSelection{Full: true}, "restore-6", "",
	)
	if err == nil {
		t.Fatal("expected chain_integrity_violation, got nil")
	}
	de, ok := domain.AsDomain(err)
	if !ok || de.Code != "chain_integrity_violation" {
		t.Errorf("expected chain_integrity_violation, got %v", err)
	}
	// Error message should mention the running generation.
	if !strings.Contains(err.Error(), "running") {
		t.Errorf("expected error to mention 'running', got: %v", err)
	}
	if fp.calls > 0 {
		t.Errorf("expected 0 presign calls, got %d", fp.calls)
	}
}

// ---------------------------------------------------------------------------
// TC-7: tombstone-path CP-side sanitization
// ---------------------------------------------------------------------------

func TestSanitizeTombstonePathCP(t *testing.T) {
	tests := []struct {
		path string
		want bool
		name string
	}{
		{path: "../../wp-config.php", want: false, name: "dotdot traversal"},
		{path: "/etc/passwd", want: false, name: "absolute path slash"},
		{path: "valid/path.php\x00", want: false, name: "NUL byte"},
		{path: "wp-content/plugins/legitimate.php", want: true, name: "valid relative path"},
		{path: "", want: false, name: "empty string"},
		{path: "\\etc\\passwd", want: false, name: "absolute backslash"},
		{path: "foo/../bar.php", want: false, name: "dotdot in middle"},
		{path: "wp-content/uploads/image.jpg", want: true, name: "valid uploads path"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeTombstonePathCP(tc.path)
			if got != tc.want {
				t.Errorf("sanitizeTombstonePathCP(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// TC-8: missing chunks rejected (GC-awareness)
// A 2-generation chain where gen1 (incremental) references chunk "zzz" that
// has been garbage-collected. PlanRestore must return chain_chunk_missing
// before any presigned URL is minted.
// ---------------------------------------------------------------------------

func TestPlanRestoreChain_TC8_MissingChunk(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()
	chainID := uuid.New()

	// gen0 is the full base; gen1 is incremental and references the GC'd chunk.
	gen0 := mkSnap(tenantID, siteID, chainID, 0, false)
	gen1 := mkSnap(tenantID, siteID, chainID, 1, true)

	repo := newChainFakeRepo()
	repo.addChainSnap(chainID, gen0)
	repo.addChainSnap(chainID, gen1)

	// gen0 has a file with chunk "ok1" that is present.
	repo.fileIndexRows[gen0.ID] = []FileIndexEntry{
		{FilePath: "wp-content/index.php", FileSize: 30, ChunkHashes: []string{"ok1"}},
	}
	// gen1 updates index.php with chunk "zzz" that has been GC'd.
	repo.fileIndexRows[gen1.ID] = []FileIndexEntry{
		{FilePath: "wp-content/index.php", FileSize: 40, ChunkHashes: []string{"zzz"}},
	}
	repo.addManifest(gen0.ID, nil)
	repo.addManifest(gen1.ID, nil)
	repo.addChunk("ok1")
	// "zzz" chunk is NOT registered (simulates GC'd chunk).

	fp := &fakePresigner{}
	svc := &Service{
		repo:       repo,
		sites:      &fakeSiteLookup{},
		store:      &tenantPresigner{tenantID: tenantID, inner: fp},
		clock:      fakeClock{t: time.Now()},
		presignTTL: time.Hour,
	}

	_, _, _, err := svc.PlanRestore(
		context.Background(), tenantID, gen1.ID,
		RestoreSelection{Full: true}, "restore-8", "",
	)
	if err == nil {
		t.Fatal("expected chain_chunk_missing error, got nil")
	}
	de, ok := domain.AsDomain(err)
	if !ok || de.Code != "chain_chunk_missing" {
		t.Errorf("expected chain_chunk_missing, got %v", err)
	}
	// No presigned URLs should be minted.
	if fp.calls > 0 {
		t.Errorf("expected 0 presign calls on missing chunk, got %d", fp.calls)
	}
}

// ---------------------------------------------------------------------------
// TC-9: non-chain snapshot (regression guard)
// ---------------------------------------------------------------------------

func TestPlanRestore_TC9_NonChainRegression(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()
	snapID := uuid.New()
	now := time.Now()

	// A non-chain (is_incremental=false, chain_id=nil) snapshot.
	snap := Snapshot{
		ID:            snapID,
		TenantID:      tenantID,
		SiteID:        siteID,
		Kind:          KindFull,
		Status:        StatusCompleted,
		AgeRecipient:  "age1test",
		IsIncremental: false,
		ChainID:       nil, // non-chain
		Generation:    0,
		CreatedAt:     now,
		UpdatedAt:     now,
	}

	repo := newChainFakeRepo()
	repo.fakeRepo.setSnapshot(snap)
	// Non-chain manifests use the standard ListManifest.
	chunkHash := "abc123def456abc1"
	repo.addManifest(snapID, []ManifestEntry{
		{
			ID:          uuid.New(),
			SnapshotID:  snapID,
			TenantID:    tenantID,
			Path:        "wp-content.part001.zip",
			EntryKind:   EntryKindFile,
			ChunkHashes: []string{chunkHash},
			Size:        1024,
		},
	})
	repo.addChunk(chunkHash)

	svc, fp := buildChainSvc(repo, tenantID)

	plan, _, _, err := svc.PlanRestore(
		context.Background(), tenantID, snapID,
		RestoreSelection{Full: true}, "restore-9", "",
	)
	if err != nil {
		t.Fatalf("PlanRestore returned error for non-chain snapshot: %v", err)
	}

	// Non-chain fields should be absent / zero.
	if plan.IsChainRestore {
		t.Error("IsChainRestore should be false for non-chain snapshot")
	}
	if len(plan.TombstonePaths) > 0 {
		t.Errorf("TombstonePaths should be empty for non-chain snapshot, got %v", plan.TombstonePaths)
	}
	if plan.EstimatedBytes != 0 {
		t.Errorf("EstimatedBytes should be 0 for non-chain snapshot, got %d", plan.EstimatedBytes)
	}

	// Manifest should have exactly the one entry.
	if len(plan.Manifest.Entries) != 1 {
		t.Errorf("expected 1 manifest entry, got %d", len(plan.Manifest.Entries))
	}

	// Presigner should have been called (non-chain path mints URLs).
	if fp.calls == 0 {
		t.Error("expected presign calls for non-chain restore, got 0")
	}
}
