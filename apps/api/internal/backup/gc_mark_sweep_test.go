package backup

// gc_mark_sweep_test.go — ADR-050 MARK-AND-SWEEP retention GC tests.
//
// These run white-box against an in-memory gcFakeRepo + recording store; no DB.
// The cardinal property under test: a chunk is deleted ONLY when it is
// unreachable from EVERY retained snapshot (tenant-globally) AND predates the
// grace floor. refcount is never consulted. The agent only re-submits
// changed/new files, so a carry-forward chunk's origin file_index row lives in
// exactly one (possibly OLD) generation — the chain-aware expansion must keep
// that origin generation reachable under a live tip.

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// ---------------------------------------------------------------------------
// gcStore — records every Delete(key) so a test can assert which objects were
// swept. Optionally fails Delete for a given key (crash-mid-sweep simulation).
// ---------------------------------------------------------------------------

type gcStore struct {
	deleted map[string]bool
	// objects models what is ACTUALLY in the bucket (GH #402). The sweep tests
	// predate it and only assert on `deleted`, so it stays empty for them and
	// costs them nothing; the reclaim tests seed it via put() and then assert on
	// presence, which is the only way to catch an object that was never deleted
	// at all (the leak) as opposed to one that was deleted wrongly.
	objects  map[string]bool
	failKey  string // when non-empty, Delete(failKey) returns an error
	failOnce bool   // when true, only the first Delete(failKey) fails
	failed   bool
	// failEvery, when > 0, makes every Nth Delete call fail regardless of key.
	// Models a storage fault partway through draining a prefix.
	failEvery int
	calls     int
}

func newGCStore() *gcStore {
	return &gcStore{deleted: map[string]bool{}, objects: map[string]bool{}}
}

// put seeds an object into the modelled bucket.
func (s *gcStore) put(key string) { s.objects[key] = true }

// has reports whether the object is still in the modelled bucket.
func (s *gcStore) has(key string) bool { return s.objects[key] }

// list returns the modelled bucket's keys under prefix, mirroring
// blobstore.Store.List semantics (a plain key-prefix scan).
func (s *gcStore) list(prefix string) []string {
	var out []string
	for k := range s.objects {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// List satisfies the reclaim worker's object-store interface.
func (s *gcStore) List(_ context.Context, prefix string) ([]string, error) {
	return s.list(prefix), nil
}

func (s *gcStore) PresignPut(_ context.Context, key string, _ time.Duration) (string, error) {
	return "https://put/" + key, nil
}
func (s *gcStore) PresignGet(_ context.Context, key string, _ time.Duration) (string, error) {
	return "https://get/" + key, nil
}
func (s *gcStore) Delete(_ context.Context, key string) error {
	s.calls++
	if s.failEvery > 0 && s.calls%s.failEvery == 0 {
		return errors.New("simulated object delete failure")
	}
	if s.failKey != "" && key == s.failKey && !(s.failOnce && s.failed) {
		s.failed = true
		return errors.New("simulated object delete failure")
	}
	s.deleted[key] = true
	delete(s.objects, key)
	return nil
}

// ---------------------------------------------------------------------------
// gcFakeRepo — full in-memory Repo implementation for the GC tests.
// ---------------------------------------------------------------------------

type gcFakeRepo struct {
	// snapshots by ID.
	snaps map[uuid.UUID]Snapshot
	// snapOrder records addSnap insertion order (a plain Go map has no stable
	// iteration order, so ListCompletedChainSnapshots — GH #168 — replays THIS
	// order for a chain's rows to give the P1a/P1b tests a deterministic,
	// reproducible physical-scan-order stand-in, rather than depending on Go's
	// randomized map iteration).
	snapOrder []uuid.UUID
	// chunks: blake3 -> (s3_key, created_at).
	chunks map[string]gcChunk
	// fileIndex: snapshotID -> rows.
	fileIndex map[uuid.UUID][]FileIndexEntry
	// manifest: snapshotID -> manifest entries (DB dumps / legacy fulls).
	manifest map[uuid.UUID][]ManifestEntry
	// schedules: siteID -> Schedule.
	schedules map[uuid.UUID]Schedule
	// dbNow is the DB clock returned by DBNow.
	dbNow time.Time
	// inflightFloor is the value ListInFlightSnapshotFloor returns (zero = none).
	inflightFloor time.Time
	// lockHeld simulates another sweep already holding the advisory lock.
	lockHeld bool
	// failReachOf, when set, makes reachability error for that snapshot ID
	// (simulates crash-mid-mark / fail-closed).
	failReachOf uuid.UUID
	// deletedSnaps records metadata-pruned snapshot IDs.
	deletedSnaps map[uuid.UUID]bool
	// beforeDecide, when set, is invoked by SweepTenantChunks for each candidate
	// AFTER the (stale) page-read projection is handed out but BEFORE the per-chunk
	// re-read-under-lock + floor re-check. It models a dedup touch committing
	// between the sweep's page read and its FOR-UPDATE delete decision: the hook
	// mutates r.chunks[hash].lastReferencedAt so the re-read sees the fresh value
	// and FIX A's re-check keeps the chunk (object never deleted).
	beforeDecide func(hash string)
}

type gcChunk struct {
	s3Key            string
	createdAt        time.Time
	lastReferencedAt time.Time
}

func newGCFakeRepo(now time.Time) *gcFakeRepo {
	return &gcFakeRepo{
		snaps:        map[uuid.UUID]Snapshot{},
		chunks:       map[string]gcChunk{},
		fileIndex:    map[uuid.UUID][]FileIndexEntry{},
		manifest:     map[uuid.UUID][]ManifestEntry{},
		schedules:    map[uuid.UUID]Schedule{},
		dbNow:        now,
		deletedSnaps: map[uuid.UUID]bool{},
	}
}

func (r *gcFakeRepo) addSnap(s Snapshot) {
	if _, exists := r.snaps[s.ID]; !exists {
		r.snapOrder = append(r.snapOrder, s.ID)
	}
	r.snaps[s.ID] = s
}
func (r *gcFakeRepo) addChunk(hash string, created time.Time) {
	// last_referenced_at defaults to created_at (the m47 backfill semantics): an
	// untouched chunk's liveness boundary is just its creation time.
	r.chunks[hash] = gcChunk{s3Key: chunkS3Key(uuid.Nil, hash), createdAt: created, lastReferencedAt: created}
}

// addChunkRef adds a chunk with an explicit last_referenced_at, modelling the
// ADR-050 touch-on-dedup: an OLD chunk (ancient created_at) whose
// last_referenced_at was bumped to ~now by an in-flight backup's dedup decision.
func (r *gcFakeRepo) addChunkRef(hash string, created, lastRef time.Time) {
	r.chunks[hash] = gcChunk{s3Key: chunkS3Key(uuid.Nil, hash), createdAt: created, lastReferencedAt: lastRef}
}

// --- Repo methods the GC actually calls -----------------------------------

func (r *gcFakeRepo) ListSiteIDsWithSnapshots(_ context.Context, _ uuid.UUID) ([]uuid.UUID, error) {
	seen := map[uuid.UUID]bool{}
	var out []uuid.UUID
	for _, s := range r.snaps {
		if !seen[s.SiteID] {
			seen[s.SiteID] = true
			out = append(out, s.SiteID)
		}
	}
	return out, nil
}

func (r *gcFakeRepo) ListCompletedSnapshotsForSite(_ context.Context, _, siteID uuid.UUID) ([]SnapshotMeta, error) {
	var out []SnapshotMeta
	for _, s := range r.snaps {
		if s.SiteID == siteID && s.Status == StatusCompleted {
			out = append(out, SnapshotMeta{
				ID: s.ID, CreatedAt: s.CreatedAt, Archived: s.Archived,
				ChainID: s.ChainID, Generation: s.Generation, IsIncremental: s.IsIncremental,
			})
		}
	}
	// newest-first
	sortMetasNewestFirst(out)
	return out, nil
}

func (r *gcFakeRepo) GetSchedule(_ context.Context, _, siteID uuid.UUID) (Schedule, error) {
	if sch, ok := r.schedules[siteID]; ok {
		return sch, nil
	}
	return Schedule{}, domain.NotFound("backup_schedule_not_found", "no schedule")
}

func (r *gcFakeRepo) SetSnapshotArchived(_ context.Context, _, snapshotID uuid.UUID, archived bool) error {
	if s, ok := r.snaps[snapshotID]; ok {
		s.Archived = archived
		r.snaps[snapshotID] = s
	}
	return nil
}

func (r *gcFakeRepo) DBNow(_ context.Context, _ uuid.UUID) (time.Time, error) { return r.dbNow, nil }

func (r *gcFakeRepo) ListInFlightSnapshotFloor(_ context.Context, _ uuid.UUID) (time.Time, error) {
	return r.inflightFloor, nil
}

func (r *gcFakeRepo) ListChainSnapshots(_ context.Context, _ uuid.UUID, chainID uuid.UUID, maxGen int) ([]Snapshot, error) {
	var out []Snapshot
	for _, s := range r.snaps {
		if s.ChainID != nil && *s.ChainID == chainID && s.Generation <= maxGen {
			out = append(out, s)
		}
	}
	return out, nil
}

// ListCompletedChainSnapshots is the GH #168 hardened variant real gc.go /
// reachableChunks now call exclusively: status=='completed' only, replaying
// snapOrder (insertion order) rather than Go's randomized map iteration so a
// duplicate-generation regression test is reproducible instead of flaky.
func (r *gcFakeRepo) ListCompletedChainSnapshots(_ context.Context, _ uuid.UUID, chainID uuid.UUID, maxGen int) ([]Snapshot, error) {
	var out []Snapshot
	for _, id := range r.snapOrder {
		s := r.snaps[id]
		if s.ChainID != nil && *s.ChainID == chainID && s.Generation <= maxGen && s.Status == StatusCompleted {
			out = append(out, s)
		}
	}
	return out, nil
}

// chunkStillReferenced is the GH #168 P2 guard's ground truth in this fake:
// mirrors the real repo exactly — r.manifest (backup_manifest_entries) ONLY,
// for any snapshot still present in r.snaps (a metadata-pruned snapshot's
// manifest rows are gone too, mirroring the real FK cascade). Deliberately
// does NOT consult r.fileIndex (backup_file_index): see chunkStillReferencedOnTx
// (repo.go) for why that table's per-file HISTORY rows (a still-retained
// generation can legitimately carry a SUPERSEDED path version) would make the
// guard unsound — TestGC_CarryForwardOrigin_KeepsOldChunk's "g0only" is
// exactly such a superseded-but-still-present row, and must remain sweepable.
// Called via the lazy closure SweepTenantChunks hands to del below, mirroring
// how the real repo binds it to the pinned tx instead of a second connection.
func (r *gcFakeRepo) chunkStillReferenced(blake3 string) (bool, error) {
	for snapID := range r.snaps {
		for _, e := range r.manifest[snapID] {
			for _, h := range e.ChunkHashes {
				if h == blake3 {
					return true, nil
				}
			}
		}
	}
	return false, nil
}

func (r *gcFakeRepo) GetSnapshot(_ context.Context, _, snapshotID uuid.UUID) (Snapshot, error) {
	if s, ok := r.snaps[snapshotID]; ok {
		return s, nil
	}
	return Snapshot{}, domain.NotFound("backup_snapshot_not_found", "not found")
}

func (r *gcFakeRepo) ListManifest(_ context.Context, _, snapshotID uuid.UUID) ([]ManifestEntry, error) {
	if snapshotID == r.failReachOf {
		return nil, domain.Internal("forced_reach_failure", "simulated reachability failure")
	}
	return r.manifest[snapshotID], nil
}

func (r *gcFakeRepo) HasFilesList(_ context.Context, _, snapshotID uuid.UUID) (bool, error) {
	for _, e := range r.manifest[snapshotID] {
		if e.EntryKind == EntryKindFilesList {
			return true, nil
		}
	}
	return false, nil
}

func (r *gcFakeRepo) StreamFileIndex(_ context.Context, _, snapshotID uuid.UUID, fn func(FileIndexEntry) error) error {
	if snapshotID == r.failReachOf {
		return domain.Internal("forced_reach_failure", "simulated reachability failure")
	}
	for _, e := range r.fileIndex[snapshotID] {
		if err := fn(e); err != nil {
			return err
		}
	}
	return nil
}

func (r *gcFakeRepo) StreamChainEffectiveFileIndex(_ context.Context, _, _ uuid.UUID, _ int, _ func(FileIndexEntry) error) error {
	panic("gcFakeRepo.StreamChainEffectiveFileIndex not implemented")
}

func (r *gcFakeRepo) DeleteSnapshot(_ context.Context, _, snapshotID uuid.UUID) error {
	r.deletedSnaps[snapshotID] = true
	delete(r.snaps, snapshotID)
	return nil
}

func (r *gcFakeRepo) SweepTenantChunks(_ context.Context, _ uuid.UUID, floor time.Time, acquired *bool, del func(SweepChunk, func() (bool, error)) (bool, error)) error {
	if r.lockHeld {
		*acquired = false
		return nil
	}
	*acquired = true
	// Model the real repo's per-chunk FIX-A critical section (sweepOneChunk):
	// the page read hands out a (possibly stale) projection, but the delete
	// decision is made on a FRESH re-read UNDER the row lock, and the floor is
	// re-checked there BEFORE the object delete. We snapshot the candidate hashes
	// first (the page read) so a mid-pass mutation to r.chunks is observed only by
	// the per-chunk re-read, exactly as a FOR UPDATE re-read would observe a touch
	// that committed after the page read.
	var candidates []string
	for hash := range r.chunks {
		candidates = append(candidates, hash)
	}
	for _, hash := range candidates {
		// Page-read projection handed out (stale read). beforeDecide simulates a
		// dedup touch committing on this chunk between the page read and the
		// FOR-UPDATE delete decision.
		if r.beforeDecide != nil {
			r.beforeDecide(hash)
		}
		// FOR UPDATE re-read: fresh created_at / last_referenced_at. A row deleted
		// by a prior candidate (or never present) is a no-op.
		c, ok := r.chunks[hash]
		if !ok {
			continue
		}
		// Floor re-check UNDER the lock, BEFORE the object delete. A touch that won
		// the race raised last_referenced_at >= floor, so we keep the chunk and
		// NEVER call del — the object is never deleted (this is the FIX-A guarantee).
		boundary := c.createdAt
		if c.lastReferencedAt.After(boundary) {
			boundary = c.lastReferencedAt
		}
		if !boundary.Before(floor) {
			continue // a touch won the race — keep object + row, no object delete.
		}
		// Still deletable under the lock: del consults the live set + floor on the
		// FRESH projection and does the object delete while the row is "locked".
		// stillReferenced is a lazy closure (mirrors the real repo's pinned-tx
		// binding — this fake has no connection to pin, but the CALL CONTRACT
		// — evaluated only if/when del needs it — is what matters here).
		hashCopy := hash
		stillReferenced := func() (bool, error) { return r.chunkStillReferenced(hashCopy) }
		remove, err := del(SweepChunk{
			Blake3: hash, S3Key: c.s3Key, CreatedAt: c.createdAt, LastReferencedAt: c.lastReferencedAt,
		}, stillReferenced)
		if err != nil {
			return err
		}
		if remove {
			delete(r.chunks, hash) // row-SECOND, still under the lock.
		}
	}
	return nil
}

// --- Unused-by-GC stubs (panic if hit) ------------------------------------

func (r *gcFakeRepo) CreateSnapshot(context.Context, CreateSnapshotInput) (Snapshot, error) {
	panic("unused")
}
func (r *gcFakeRepo) GetSnapshotScoped(context.Context, db.ScopedPrincipal, uuid.UUID, uuid.UUID) (Snapshot, error) {
	panic("unused")
}
func (r *gcFakeRepo) ListSnapshotsForSite(context.Context, uuid.UUID, uuid.UUID, int32, int32) ([]Snapshot, error) {
	panic("unused")
}
func (r *gcFakeRepo) MarkSnapshotRunning(context.Context, uuid.UUID, uuid.UUID) (Snapshot, bool, error) {
	panic("unused")
}
func (r *gcFakeRepo) CompleteSnapshot(context.Context, uuid.UUID, uuid.UUID, int64, int64) (Snapshot, bool, error) {
	panic("unused")
}
func (r *gcFakeRepo) FailSnapshot(context.Context, uuid.UUID, uuid.UUID, string) (Snapshot, bool, error) {
	panic("unused")
}
func (r *gcFakeRepo) FailStalledSnapshot(context.Context, uuid.UUID, uuid.UUID, string) (int64, error) {
	panic("unused")
}
func (r *gcFakeRepo) UpdateSnapshotProgress(context.Context, uuid.UUID, uuid.UUID, []byte) (Snapshot, error) {
	panic("unused")
}
func (r *gcFakeRepo) ListStalledRunningSnapshots(context.Context, time.Duration, time.Duration) ([]StalledSnapshot, error) {
	panic("unused")
}
func (r *gcFakeRepo) MarkSnapshotStalled(context.Context, uuid.UUID, uuid.UUID) (bool, error) {
	panic("unused")
}
func (r *gcFakeRepo) ClearSnapshotStalled(context.Context, uuid.UUID, uuid.UUID) (bool, error) {
	panic("unused")
}
func (r *gcFakeRepo) GetLatestCompletedSnapshot(context.Context, uuid.UUID, uuid.UUID) (Snapshot, error) {
	panic("unused")
}
func (r *gcFakeRepo) RecordManifest(context.Context, RecordManifestInput) (int64, int64, error) {
	panic("unused")
}
func (r *gcFakeRepo) ExistingChunkHashes(context.Context, uuid.UUID, []string) (map[string]Chunk, error) {
	panic("unused")
}
func (r *gcFakeRepo) UpsertSchedule(context.Context, UpsertScheduleInput) (Schedule, error) {
	panic("unused")
}
func (r *gcFakeRepo) ListDueSchedules(context.Context, time.Time, int32) ([]Schedule, error) {
	panic("unused")
}
func (r *gcFakeRepo) ListTenantsForGC(context.Context) ([]uuid.UUID, error) { panic("unused") }
func (r *gcFakeRepo) AdvanceScheduleRun(context.Context, uuid.UUID, uuid.UUID, time.Time) error {
	panic("unused")
}
func (r *gcFakeRepo) ListExpiredSnapshots(context.Context, uuid.UUID, time.Time) ([]Snapshot, error) {
	panic("unused")
}
func (r *gcFakeRepo) InsertFileIndexBatch(context.Context, uuid.UUID, uuid.UUID, []FileIndexEntry) error {
	panic("unused")
}
func (r *gcFakeRepo) CountFileIndex(context.Context, uuid.UUID, uuid.UUID) (int64, error) {
	panic("unused")
}
func (r *gcFakeRepo) UpdateSnapshotCycleStats(context.Context, uuid.UUID, uuid.UUID, CycleStatsInput) error {
	panic("unused")
}
func (r *gcFakeRepo) CompleteIncrementalManifest(context.Context, CompleteIncrementalInput) (int64, int64, error) {
	panic("unused")
}
func (r *gcFakeRepo) SetSnapshotLocked(context.Context, uuid.UUID, uuid.UUID, bool) (Snapshot, error) {
	panic("unused")
}
func (r *gcFakeRepo) GetBackupSettings(_ context.Context, _, _ uuid.UUID) (SiteBackupSettings, error) {
	return SiteBackupSettings{}, domain.NotFound("backup_settings_not_found", "no settings")
}
func (r *gcFakeRepo) UpsertBackupSettings(_ context.Context, _ uuid.UUID, in SiteBackupSettings) (SiteBackupSettings, error) {
	panic("unused")
}
func (r *gcFakeRepo) FleetListSnapshots(_ context.Context, _ db.ScopedPrincipal, _ uuid.UUID, _ FleetListFilter) (FleetSnapshotPage, error) {
	panic("gcFakeRepo.FleetListSnapshots not implemented")
}
func (r *gcFakeRepo) FleetBackupHealth(_ context.Context, _ db.ScopedPrincipal, _ uuid.UUID, _ []uuid.UUID) ([]FleetBackupHealthItem, error) {
	panic("gcFakeRepo.FleetBackupHealth not implemented")
}
func (r *gcFakeRepo) ClaimAndAdvanceDueSchedules(_ context.Context, _ time.Time, _ map[uuid.UUID]time.Time) ([]Schedule, error) {
	panic("gcFakeRepo.ClaimAndAdvanceDueSchedules not implemented")
}
func (r *gcFakeRepo) CountInFlightSnapshots(_ context.Context, _, _ uuid.UUID) (int64, error) {
	return 0, nil
}
func (r *gcFakeRepo) HealOverdueSchedules(_ context.Context, _ time.Time, _ func(Schedule, time.Time) time.Time) (int, error) {
	return 0, nil
}
func (r *gcFakeRepo) ReconcileDuplicateInflightSnapshots(_ context.Context) (int, error) {
	return 0, nil
}
func (r *gcFakeRepo) GetSnapshotsByIDs(_ context.Context, _ uuid.UUID, _ []uuid.UUID) (map[uuid.UUID]Snapshot, error) {
	panic("gcFakeRepo.GetSnapshotsByIDs not implemented")
}
func (r *gcFakeRepo) HasActiveRestore(_ context.Context, _ uuid.UUID, _ []uuid.UUID) (map[uuid.UUID]bool, error) {
	panic("gcFakeRepo.HasActiveRestore not implemented")
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func sortMetasNewestFirst(metas []SnapshotMeta) {
	for i := 1; i < len(metas); i++ {
		for j := i; j > 0 && metas[j].CreatedAt.After(metas[j-1].CreatedAt); j-- {
			metas[j], metas[j-1] = metas[j-1], metas[j]
		}
	}
}

func buildGCSvc(repo *gcFakeRepo, store Presigner, now time.Time) *Service {
	return &Service{
		repo:               repo,
		store:              store,
		clock:              fakeClock{t: now},
		retentionDays:      30,
		monthlyArchiveKeep: 0,
	}
}

// gcSnap builds a completed snapshot created `age` before now.
func gcSnap(tenantID, siteID, chainID uuid.UUID, gen int, incremental bool, createdAt time.Time) Snapshot {
	cid := chainID
	return Snapshot{
		ID:            uuid.New(),
		TenantID:      tenantID,
		SiteID:        siteID,
		Kind:          KindFull,
		Status:        StatusCompleted,
		IsIncremental: incremental,
		ChainID:       &cid,
		Generation:    gen,
		CreatedAt:     createdAt,
		UpdatedAt:     createdAt,
	}
}

// ---------------------------------------------------------------------------
// TEST 1 (CARDINAL): carry-forward-origin.
// Delete gen0 (which HOLDS a carry-forward chunk still live at gen2) while gen2
// tip is retained -> the carry-forward chunk MUST be KEPT, and reachableChunks
// for gen2 must include it.
// ---------------------------------------------------------------------------

func TestGC_CarryForwardOrigin_KeepsOldChunk(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	tenantID := uuid.New()
	siteID := uuid.New()
	chainID := uuid.New()

	// gen0 created 90d ago (expired by 30d window), gen1 60d ago, gen2 1d ago.
	old := now.Add(-90 * 24 * time.Hour)
	mid := now.Add(-60 * 24 * time.Hour)
	fresh := now.Add(-24 * time.Hour)

	gen0 := gcSnap(tenantID, siteID, chainID, 0, false, old)
	gen1 := gcSnap(tenantID, siteID, chainID, 1, true, mid)
	gen2 := gcSnap(tenantID, siteID, chainID, 2, true, fresh)

	repo := newGCFakeRepo(now)
	repo.addSnap(gen0)
	repo.addSnap(gen1)
	repo.addSnap(gen2)

	// gen0 introduces "carry" (a file never changed since) + "g0only" (overwritten
	// at gen1). gen1 overwrites that path with "g1new". gen2 adds "g2new".
	// "carry" therefore lives ONLY in gen0's file index but is still the WINNING
	// version at gen2 -> it is a carry-forward chunk whose origin is the expired
	// gen0.
	repo.fileIndex[gen0.ID] = []FileIndexEntry{
		{FilePath: "wp-content/carry.php", ChunkHashes: []string{"carry"}},
		{FilePath: "wp-content/changing.php", ChunkHashes: []string{"g0only"}},
	}
	repo.fileIndex[gen1.ID] = []FileIndexEntry{
		{FilePath: "wp-content/changing.php", ChunkHashes: []string{"g1new"}},
	}
	repo.fileIndex[gen2.ID] = []FileIndexEntry{
		{FilePath: "wp-content/added.php", ChunkHashes: []string{"g2new"}},
	}

	// Chunks: carry + g1new + g2new are live; g0only is dead (overwritten, not the
	// winning version at the retained tip).
	repo.addChunk("carry", old)
	repo.addChunk("g0only", old)
	repo.addChunk("g1new", mid)
	repo.addChunk("g2new", fresh)

	store := newGCStore()
	svc := buildGCSvc(repo, store, now)

	snapsDel, chunksDel, err := svc.RunRetentionGC(context.Background(), tenantID)
	if err != nil {
		t.Fatalf("RunRetentionGC error: %v", err)
	}

	// gen0 metadata may be pruned (it is age-expired) BUT its carry-forward chunk
	// must survive because gen2 (retained) still wins that path.
	if _, ok := repo.chunks["carry"]; !ok {
		t.Error("carry-forward chunk 'carry' was swept but is still live at gen2")
	}
	if store.deleted[chunkS3Key(uuid.Nil, "carry")] {
		t.Error("carry-forward chunk object was deleted")
	}
	// g0only is genuinely dead -> swept.
	if _, ok := repo.chunks["g0only"]; ok {
		t.Error("dead chunk 'g0only' should have been swept")
	}
	if !store.deleted[chunkS3Key(uuid.Nil, "g0only")] {
		t.Error("dead chunk object 'g0only' should have been deleted")
	}
	if chunksDel != 1 {
		t.Errorf("expected exactly 1 chunk swept (g0only), got %d", chunksDel)
	}

	// reachableChunks(gen2, retainedMaxGen=2) must include carry + g1new + g2new.
	reach, rerr := svc.reachableChunks(context.Background(), tenantID, gen2, 2)
	if rerr != nil {
		t.Fatalf("reachableChunks error: %v", rerr)
	}
	for _, h := range []string{"carry", "g1new", "g2new"} {
		if _, ok := reach[h]; !ok {
			t.Errorf("reachableChunks(gen2) missing %q", h)
		}
	}
	if _, ok := reach["g0only"]; ok {
		t.Error("reachableChunks(gen2) should NOT include overwritten g0only")
	}
	_ = snapsDel
}

// ---------------------------------------------------------------------------
// TEST 2: cross-site shared chunk.
// Site A expires entirely; site B retains a snapshot that shares a chunk with A.
// The shared chunk MUST be KEPT; the A-only chunk MUST be swept.
// ---------------------------------------------------------------------------

func TestGC_CrossSiteSharedChunk(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	tenantID := uuid.New()
	siteA := uuid.New()
	siteB := uuid.New()
	chainA := uuid.New()
	chainB := uuid.New()

	oldA := now.Add(-90 * 24 * time.Hour) // expired
	freshB := now.Add(-24 * time.Hour)    // retained

	// Site A: a single full-base anchor (gen0, non-incremental) -> manifest-only.
	snapA := gcSnap(tenantID, siteA, chainA, 0, false, oldA)
	// Site B: a single full-base anchor (gen0, non-incremental) -> manifest-only.
	snapB := gcSnap(tenantID, siteB, chainB, 0, false, freshB)

	repo := newGCFakeRepo(now)
	repo.addSnap(snapA)
	repo.addSnap(snapB)

	// A references "shared" + "a_only" via its manifest; B references "shared".
	repo.manifest[snapA.ID] = []ManifestEntry{
		{Path: "a.zip", EntryKind: EntryKindFile, ChunkHashes: []string{"shared", "a_only"}},
	}
	repo.manifest[snapB.ID] = []ManifestEntry{
		{Path: "b.zip", EntryKind: EntryKindFile, ChunkHashes: []string{"shared"}},
	}
	repo.addChunk("shared", oldA)
	repo.addChunk("a_only", oldA)

	store := newGCStore()
	svc := buildGCSvc(repo, store, now)

	_, chunksDel, err := svc.RunRetentionGC(context.Background(), tenantID)
	if err != nil {
		t.Fatalf("RunRetentionGC error: %v", err)
	}

	if _, ok := repo.chunks["shared"]; !ok {
		t.Error("cross-site shared chunk was swept; site B still references it")
	}
	if _, ok := repo.chunks["a_only"]; ok {
		t.Error("A-only chunk should have been swept")
	}
	if chunksDel != 1 {
		t.Errorf("expected 1 chunk swept (a_only), got %d", chunksDel)
	}
}

// ---------------------------------------------------------------------------
// TEST 3: whole-chain drop. Every generation of a chain expires and no other
// snapshot references its chunks -> all chunks swept.
// ---------------------------------------------------------------------------

func TestGC_WholeChainDrop(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	tenantID := uuid.New()
	siteID := uuid.New()
	chainID := uuid.New()

	old := now.Add(-120 * 24 * time.Hour)
	old1 := now.Add(-110 * 24 * time.Hour)

	gen0 := gcSnap(tenantID, siteID, chainID, 0, false, old)
	gen1 := gcSnap(tenantID, siteID, chainID, 1, true, old1)

	repo := newGCFakeRepo(now)
	repo.addSnap(gen0)
	repo.addSnap(gen1)
	repo.fileIndex[gen0.ID] = []FileIndexEntry{{FilePath: "x.php", ChunkHashes: []string{"c0"}}}
	repo.fileIndex[gen1.ID] = []FileIndexEntry{{FilePath: "y.php", ChunkHashes: []string{"c1"}}}
	repo.addChunk("c0", old)
	repo.addChunk("c1", old1)

	store := newGCStore()
	svc := buildGCSvc(repo, store, now)

	snapsDel, chunksDel, err := svc.RunRetentionGC(context.Background(), tenantID)
	if err != nil {
		t.Fatalf("RunRetentionGC error: %v", err)
	}
	if len(repo.chunks) != 0 {
		t.Errorf("expected all chunks swept on whole-chain drop, %d remain", len(repo.chunks))
	}
	if chunksDel != 2 {
		t.Errorf("expected 2 chunks swept, got %d", chunksDel)
	}
	if snapsDel != 2 {
		t.Errorf("expected 2 snapshots pruned, got %d", snapsDel)
	}
}

// ---------------------------------------------------------------------------
// TEST 4: crash-mid-mark fail-closed. If reachability errors for ANY retained
// snapshot, NOTHING is deleted (no chunk swept, no snapshot pruned).
// ---------------------------------------------------------------------------

func TestGC_CrashMidMark_FailClosed(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	tenantID := uuid.New()
	siteID := uuid.New()
	chainID := uuid.New()

	old := now.Add(-90 * 24 * time.Hour)
	fresh := now.Add(-24 * time.Hour)

	gen0 := gcSnap(tenantID, siteID, chainID, 0, false, old)
	gen1 := gcSnap(tenantID, siteID, chainID, 1, true, fresh)

	repo := newGCFakeRepo(now)
	repo.addSnap(gen0)
	repo.addSnap(gen1)
	repo.fileIndex[gen0.ID] = []FileIndexEntry{{FilePath: "x.php", ChunkHashes: []string{"c0"}}}
	repo.fileIndex[gen1.ID] = []FileIndexEntry{{FilePath: "y.php", ChunkHashes: []string{"c1"}}}
	repo.addChunk("c0", old)
	repo.addChunk("c1", fresh)

	// Force reachability to fail when walking the chain (gen1 is the retained tip
	// whose file index walk includes gen0..gen1). We fail on gen0's stream which
	// is part of the gen1 chain walk.
	repo.failReachOf = gen0.ID

	store := newGCStore()
	svc := buildGCSvc(repo, store, now)

	snapsDel, chunksDel, err := svc.RunRetentionGC(context.Background(), tenantID)
	if err == nil {
		t.Fatal("expected fail-closed error, got nil")
	}
	if chunksDel != 0 || snapsDel != 0 {
		t.Errorf("fail-closed must delete nothing, got snaps=%d chunks=%d", snapsDel, chunksDel)
	}
	if len(repo.chunks) != 2 {
		t.Errorf("fail-closed must keep all chunks, %d remain", len(repo.chunks))
	}
	if len(store.deleted) != 0 {
		t.Errorf("fail-closed must delete no objects, got %d", len(store.deleted))
	}
}

// ---------------------------------------------------------------------------
// TEST 5: crash-mid-sweep self-heal. The object is already gone (Delete fails
// once mid-sweep), the row remains; the NEXT idempotent sweep deletes it
// cleanly (404-as-success path is modelled by a successful retry).
// ---------------------------------------------------------------------------

func TestGC_CrashMidSweep_SelfHeal(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	tenantID := uuid.New()
	siteID := uuid.New()
	chainID := uuid.New()

	old := now.Add(-120 * 24 * time.Hour)

	gen0 := gcSnap(tenantID, siteID, chainID, 0, false, old)
	repo := newGCFakeRepo(now)
	repo.addSnap(gen0)
	repo.fileIndex[gen0.ID] = []FileIndexEntry{{FilePath: "x.php", ChunkHashes: []string{"live"}}}
	// "dead" is unreferenced -> will be swept.
	repo.addChunk("live", old)
	repo.addChunk("dead", old)
	// gen0 references "live" via file index; but gen0 is gen0/non-incremental ->
	// it is manifest-only. So give it a manifest referencing "live".
	repo.manifest[gen0.ID] = []ManifestEntry{
		{Path: "x.zip", EntryKind: EntryKindFile, ChunkHashes: []string{"live"}},
	}

	store := newGCStore()
	// First sweep: the object delete for "dead" fails -> row must remain.
	store.failKey = chunkS3Key(uuid.Nil, "dead")
	store.failOnce = true
	svc := buildGCSvc(repo, store, now)

	// gen0 is fresh enough to be RETAINED (created 120d ago but it's the only
	// snapshot; with monthlyArchiveKeep=0 and retentionDays=30 it is age-expired).
	// To keep gen0 retained so "live" stays reachable, give it an archive keep.
	repo.schedules[siteID] = Schedule{RetentionDays: 30, MonthlyArchiveKeep: 12, KeepLast: 5}

	_, _, err := svc.RunRetentionGC(context.Background(), tenantID)
	if err == nil {
		t.Fatal("expected the first sweep to surface the object-delete failure")
	}
	// Row for "dead" must still be present (object-first/row-second: delete failed
	// so the row was NOT removed).
	if _, ok := repo.chunks["dead"]; !ok {
		t.Error("dead chunk row was removed despite object-delete failure")
	}
	// "live" must remain (reachable from retained gen0).
	if _, ok := repo.chunks["live"]; !ok {
		t.Error("live chunk was swept")
	}

	// Second sweep: object delete now succeeds (idempotent self-heal).
	store.failKey = ""
	_, chunksDel2, err2 := svc.RunRetentionGC(context.Background(), tenantID)
	if err2 != nil {
		t.Fatalf("second sweep error: %v", err2)
	}
	if _, ok := repo.chunks["dead"]; ok {
		t.Error("dead chunk row should be removed on the self-healing second sweep")
	}
	if chunksDel2 != 1 {
		t.Errorf("expected 1 chunk swept on second pass, got %d", chunksDel2)
	}
}

// ---------------------------------------------------------------------------
// TEST 6: grace-floor stability. An old, re-referenced chunk must NOT be swept
// while an in-flight backup floor (or markStart) sits below the chunk's
// created_at boundary. We model an in-flight floor older than the dead chunk so
// the chunk is within the grace window and survives even though unreachable.
// ---------------------------------------------------------------------------

func TestGC_GraceFloorStability(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	tenantID := uuid.New()
	siteID := uuid.New()
	chainID := uuid.New()

	old := now.Add(-120 * 24 * time.Hour)

	gen0 := gcSnap(tenantID, siteID, chainID, 0, false, old)
	repo := newGCFakeRepo(now)
	repo.addSnap(gen0)
	repo.manifest[gen0.ID] = []ManifestEntry{
		{Path: "x.zip", EntryKind: EntryKindFile, ChunkHashes: []string{"live"}},
	}
	repo.schedules[siteID] = Schedule{RetentionDays: 30, MonthlyArchiveKeep: 12, KeepLast: 5}

	// "recent_dead" is unreachable but was created AFTER the grace floor, so it
	// must be protected from sweeping (an in-flight backup could re-reference it).
	recent := now.Add(-30 * time.Minute)
	repo.addChunk("live", old)
	repo.addChunk("recent_dead", recent)

	// Set the in-flight floor to BEFORE recent_dead's created_at so the effective
	// floor (min(markStart=now, inflightFloor)) sits below recent_dead.
	repo.inflightFloor = now.Add(-1 * time.Hour)

	store := newGCStore()
	svc := buildGCSvc(repo, store, now)

	_, chunksDel, err := svc.RunRetentionGC(context.Background(), tenantID)
	if err != nil {
		t.Fatalf("RunRetentionGC error: %v", err)
	}
	if _, ok := repo.chunks["recent_dead"]; !ok {
		t.Error("recent_dead chunk was swept despite being within the grace floor")
	}
	if chunksDel != 0 {
		t.Errorf("expected 0 chunks swept under the grace floor, got %d", chunksDel)
	}
}

// ---------------------------------------------------------------------------
// TEST 7: advisory-lock contention -> tenant skipped, nothing swept.
// ---------------------------------------------------------------------------

func TestGC_AdvisoryLockContention_SkipsSweep(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	tenantID := uuid.New()
	siteID := uuid.New()
	chainID := uuid.New()

	old := now.Add(-120 * 24 * time.Hour)
	gen0 := gcSnap(tenantID, siteID, chainID, 0, false, old)
	repo := newGCFakeRepo(now)
	repo.addSnap(gen0)
	repo.manifest[gen0.ID] = []ManifestEntry{
		{Path: "x.zip", EntryKind: EntryKindFile, ChunkHashes: []string{"live"}},
	}
	repo.addChunk("live", old)
	repo.addChunk("dead", old)
	repo.lockHeld = true // another sweep holds the lock.

	store := newGCStore()
	svc := buildGCSvc(repo, store, now)

	_, chunksDel, err := svc.RunRetentionGC(context.Background(), tenantID)
	if err != nil {
		t.Fatalf("RunRetentionGC error: %v", err)
	}
	if chunksDel != 0 {
		t.Errorf("locked tenant must sweep nothing, got %d", chunksDel)
	}
	if len(repo.chunks) != 2 {
		t.Errorf("locked tenant must keep all chunks, %d remain", len(repo.chunks))
	}
}

// ---------------------------------------------------------------------------
// TEST 8 (ADR-050 DATA-LOSS REGRESSION): dedup-old-chunk-in-flight.
//
// THE BUG this guards: an OLD chunk (ancient created_at) whose ONLY completed
// referrer expires on THIS GC run is re-referenced by an IN-FLIGHT backup via
// tenant-global dedup. PresignChunks/ExistingChunkHashes told the agent "already
// stored, skip upload" WITHOUT re-uploading, so created_at stays ancient; the
// in-flight snapshot is status='running' so it is NOT in the mark set and its
// file_index rows are not yet visible. A created_at-only floor would delete the
// chunk; the in-flight backup then completes referencing a deleted chunk ->
// unrestorable. The fix is touch-on-dedup (last_referenced_at = now()) + a
// GREATEST(created_at, last_referenced_at) < floor sweep predicate.
//
// Here the dedup touch is simulated via addChunkRef (last_referenced_at = ~now).
// The chunk's created_at is far below the effective floor AND below the in-flight
// floor, so ONLY the last_referenced_at term can save it -> it MUST survive.
// ---------------------------------------------------------------------------

func TestGC_DedupOldChunkInFlight_GreatestFloorProtects(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	tenantID := uuid.New()
	siteID := uuid.New()
	chainID := uuid.New()

	old := now.Add(-120 * 24 * time.Hour) // the chunk + its only referrer
	fresh := now.Add(-24 * time.Hour)     // the new full base that retains nothing of old

	// The only COMPLETED referrer of "shared_old" is gen0, which is age-expired
	// (120d > 30d window) and references "shared_old" via its manifest. A separate
	// fresh full base ("newbase") is retained but references its OWN chunk only —
	// so "shared_old" is genuinely unreachable from the retained set this run.
	gen0 := gcSnap(tenantID, siteID, chainID, 0, false, old)
	newBaseChain := uuid.New()
	newBase := gcSnap(tenantID, siteID, newBaseChain, 0, false, fresh)

	repo := newGCFakeRepo(now)
	repo.addSnap(gen0)
	repo.addSnap(newBase)
	repo.manifest[gen0.ID] = []ManifestEntry{
		{Path: "old.zip", EntryKind: EntryKindFile, ChunkHashes: []string{"shared_old"}},
	}
	repo.manifest[newBase.ID] = []ManifestEntry{
		{Path: "new.zip", EntryKind: EntryKindFile, ChunkHashes: []string{"newbase_only"}},
	}

	// An IN-FLIGHT (pending/running) snapshot exists; it is NOT in the mark set
	// (no completed status) and its file_index is not yet written — its only
	// reference to "shared_old" is the presign-time dedup decision. Model its
	// floor as recent (it just started), so the created_at term alone would NOT
	// protect "shared_old" (created_at=old << inflightFloor).
	inflight := gcSnap(tenantID, siteID, chainID, 1, true, now.Add(-5*time.Minute))
	inflight.Status = StatusRunning
	repo.addSnap(inflight)
	repo.inflightFloor = now.Add(-5 * time.Minute)

	// "shared_old": ancient created_at, but last_referenced_at bumped to ~now by
	// the in-flight dedup touch. "newbase_only" is live. "truly_dead" is an old,
	// untouched, unreferenced chunk that MUST still be swept (no leak).
	repo.addChunkRef("shared_old", old, now.Add(-1*time.Minute))
	repo.addChunk("newbase_only", fresh)
	repo.addChunk("truly_dead", old)

	store := newGCStore()
	svc := buildGCSvc(repo, store, now)

	_, chunksDel, err := svc.RunRetentionGC(context.Background(), tenantID)
	if err != nil {
		t.Fatalf("RunRetentionGC error: %v", err)
	}

	// The dedup-touched old chunk MUST survive (object + row) — the GREATEST floor
	// protects it even though it is unreachable and its created_at is ancient.
	if _, ok := repo.chunks["shared_old"]; !ok {
		t.Error("DATA LOSS: dedup-touched old chunk 'shared_old' was swept while an in-flight backup re-references it")
	}
	if store.deleted[chunkS3Key(uuid.Nil, "shared_old")] {
		t.Error("DATA LOSS: dedup-touched old chunk object was deleted")
	}
	// The genuinely-dead old chunk (no touch, no referrer) MUST still be swept.
	if _, ok := repo.chunks["truly_dead"]; ok {
		t.Error("no-leak check failed: a genuinely-dead old chunk was NOT swept")
	}
	if !store.deleted[chunkS3Key(uuid.Nil, "truly_dead")] {
		t.Error("no-leak check failed: genuinely-dead chunk object was NOT deleted")
	}
	if _, ok := repo.chunks["newbase_only"]; !ok {
		t.Error("live chunk 'newbase_only' was swept")
	}
	if chunksDel != 1 {
		t.Errorf("expected exactly 1 chunk swept (truly_dead), got %d", chunksDel)
	}
}

// ---------------------------------------------------------------------------
// TEST 9 (no-leak twin): WITHOUT the touch and WITHOUT a completed referrer, a
// genuinely-dead OLD chunk IS swept. This proves the GREATEST fix does not leak:
// last_referenced_at == created_at (the m47 backfill default) leaves the
// deletion boundary exactly at the old created_at, so the chunk is reaped.
// ---------------------------------------------------------------------------

func TestGC_UntouchedOldDeadChunk_StillSwept(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	tenantID := uuid.New()
	siteID := uuid.New()
	chainID := uuid.New()

	old := now.Add(-120 * 24 * time.Hour)

	// A single age-expired base whose chunk is unreferenced after expiry. No
	// in-flight snapshot, so the effective floor is markStart (now). The dead
	// chunk's boundary (GREATEST(old, old) = old) is well below the floor.
	gen0 := gcSnap(tenantID, siteID, chainID, 0, false, old)
	repo := newGCFakeRepo(now)
	repo.addSnap(gen0)
	repo.manifest[gen0.ID] = []ManifestEntry{
		{Path: "x.zip", EntryKind: EntryKindFile, ChunkHashes: []string{"live"}},
	}
	// gen0 stays retained (archive keep) so "live" is reachable; "dead_old" is
	// unreferenced and untouched -> must be swept.
	repo.schedules[siteID] = Schedule{RetentionDays: 30, MonthlyArchiveKeep: 12, KeepLast: 5}
	repo.addChunk("live", old)
	repo.addChunk("dead_old", old) // last_referenced_at == created_at == old

	store := newGCStore()
	svc := buildGCSvc(repo, store, now)

	_, chunksDel, err := svc.RunRetentionGC(context.Background(), tenantID)
	if err != nil {
		t.Fatalf("RunRetentionGC error: %v", err)
	}
	if _, ok := repo.chunks["dead_old"]; ok {
		t.Error("untouched dead old chunk should have been swept (GREATEST fix must not leak)")
	}
	if !store.deleted[chunkS3Key(uuid.Nil, "dead_old")] {
		t.Error("untouched dead old chunk object should have been deleted")
	}
	if _, ok := repo.chunks["live"]; !ok {
		t.Error("live chunk was swept")
	}
	if chunksDel != 1 {
		t.Errorf("expected exactly 1 chunk swept (dead_old), got %d", chunksDel)
	}
}

// ---------------------------------------------------------------------------
// TEST 10 (ADR-050 FIX A — object-orphan TOCTOU): a dedup touch that COMMITS
// BETWEEN the sweep's page read and its per-chunk delete decision must leave the
// object intact.
//
// THE BUG this guards: the page read sees the chunk with an ancient
// last_referenced_at and decides "delete"; before the object delete fires, an
// in-flight backup's dedup touch on the chunk commits (last_referenced_at=now);
// a pre-FIX sweep would still delete the OBJECT (stale decision) while the row
// re-check then KEEPS the row -> object gone, row kept, the in-flight backup
// (which skipped upload via dedup) references a missing object -> unrestorable.
//
// FIX A serializes the object delete under a per-chunk FOR UPDATE row lock and
// re-reads + re-checks the floor UNDER that lock BEFORE the object delete. Here
// beforeDecide simulates the touch landing after the (stale) page-read
// projection is handed out but before the FOR-UPDATE re-read; the fresh
// last_referenced_at then clears the floor and the object MUST survive.
//
// Distinct from TEST 8 (which PRE-applies the touch via addChunkRef and so never
// exercises the mid-sweep race): here the chunk is ancient/untouched at page-read
// time and only becomes fresh DURING the sweep.
// ---------------------------------------------------------------------------

func TestGC_DedupTouchMidSweep_ObjectNotOrphaned(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	tenantID := uuid.New()
	siteID := uuid.New()
	chainID := uuid.New()

	old := now.Add(-120 * 24 * time.Hour)

	// A single age-expired base; with archive-keep it stays retained so "live"
	// stays reachable. "racing" is ancient + unreferenced at page-read time, so a
	// stale page read would decide to sweep it. "truly_dead" is the no-leak twin:
	// ancient, never touched -> still swept (proves the hook does not over-protect).
	gen0 := gcSnap(tenantID, siteID, chainID, 0, false, old)
	repo := newGCFakeRepo(now)
	repo.addSnap(gen0)
	repo.manifest[gen0.ID] = []ManifestEntry{
		{Path: "x.zip", EntryKind: EntryKindFile, ChunkHashes: []string{"live"}},
	}
	repo.schedules[siteID] = Schedule{RetentionDays: 30, MonthlyArchiveKeep: 12, KeepLast: 5}
	repo.addChunk("live", old)
	repo.addChunk("racing", old)     // ancient at page-read; touched mid-sweep.
	repo.addChunk("truly_dead", old) // ancient, never touched -> swept.

	// An in-flight (running) backup is what performs the racing dedup touch. Its
	// start sets the effective floor (min(markStart, inflightFloor)) to ~5m ago, so
	// a touch stamped just AFTER it started clears the floor.
	inflight := gcSnap(tenantID, siteID, chainID, 1, true, now.Add(-5*time.Minute))
	inflight.Status = StatusRunning
	repo.addSnap(inflight)
	repo.inflightFloor = now.Add(-5 * time.Minute)

	// Simulate the in-flight backup's dedup touch landing between the page read and
	// the per-chunk delete decision for ONLY the "racing" chunk: bump its
	// last_referenced_at above the effective floor so the FOR-UPDATE re-read keeps
	// it. ("racing" was ancient — << floor — at page-read time.)
	repo.beforeDecide = func(hash string) {
		if hash == "racing" {
			c := repo.chunks[hash]
			c.lastReferencedAt = now.Add(-1 * time.Minute)
			repo.chunks[hash] = c
		}
	}

	store := newGCStore()
	svc := buildGCSvc(repo, store, now)

	_, chunksDel, err := svc.RunRetentionGC(context.Background(), tenantID)
	if err != nil {
		t.Fatalf("RunRetentionGC error: %v", err)
	}

	// The chunk whose touch landed mid-sweep MUST keep BOTH its row and its object
	// (no orphan). This is the property a stale-decision object delete would break.
	if _, ok := repo.chunks["racing"]; !ok {
		t.Error("OBJECT ORPHAN: 'racing' row was deleted despite a mid-sweep dedup touch")
	}
	if store.deleted[chunkS3Key(uuid.Nil, "racing")] {
		t.Error("OBJECT ORPHAN: 'racing' object was deleted despite a mid-sweep dedup touch (FIX A re-check-under-lock should have caught it)")
	}
	// No over-protection: the genuinely-dead untouched chunk is still reaped.
	if _, ok := repo.chunks["truly_dead"]; ok {
		t.Error("no-leak check failed: untouched dead chunk 'truly_dead' was NOT swept")
	}
	if !store.deleted[chunkS3Key(uuid.Nil, "truly_dead")] {
		t.Error("no-leak check failed: 'truly_dead' object was NOT deleted")
	}
	if _, ok := repo.chunks["live"]; !ok {
		t.Error("live chunk was swept")
	}
	if chunksDel != 1 {
		t.Errorf("expected exactly 1 chunk swept (truly_dead), got %d", chunksDel)
	}
}
