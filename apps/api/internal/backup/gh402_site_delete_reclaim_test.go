package backup

// gh402_site_delete_reclaim_test.go, GH #402: deleting ONE site out of a live
// tenant left its per-snapshot manifest.json objects in storage forever.
//
// The cardinal test in this file is the FIRST one. It is not about the leak; it
// is about the thing that would be far worse than the leak. Chunks are
// content-addressed and deduplicated TENANT-WIDE, so a chunk that belonged to
// the deleted site may still be the only copy of bytes a DIFFERENT, LIVE site
// needs to restore. Reclaiming the deleted site's objects must not touch it.
//
// The proof is structural rather than argued. Manifests live under
//
//	tenant/<tenantID>/site/<siteID>/backup/<snapshotID>/manifest.json
//
// and chunks live under
//
//	chunks/<tenantID>/<blake3>
//
// Those roots are disjoint, so a prefix-scoped reclaimer under the manifest
// root cannot delete a chunk however badly it is written. These tests assert
// that disjointness holds end to end rather than taking it on trust.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// cascadeSiteDelete models exactly what `DELETE FROM sites WHERE id = $1` does
// to this fake: backup_snapshots.site_id is ON DELETE CASCADE, so every
// snapshot row for the site disappears along with its file_index and
// manifest_entries children. backup_chunks has NO FK to sites, so the chunk
// inventory is deliberately left untouched here, mirroring the real schema.
func cascadeSiteDelete(r *gcFakeRepo, siteID uuid.UUID) {
	for id, s := range r.snaps {
		if s.SiteID != siteID {
			continue
		}
		delete(r.snaps, id)
		delete(r.manifest, id)
		delete(r.fileIndex, id)
	}
	kept := r.snapOrder[:0]
	for _, id := range r.snapOrder {
		if _, ok := r.snaps[id]; ok {
			kept = append(kept, id)
		}
	}
	r.snapOrder = kept
}

// seedSnapshotObjects puts the storage objects a completed snapshot owns: its
// manifest index object, plus a chunk object per referenced hash. Chunk objects
// are shared by key, so seeding the same hash twice is a no-op, which is the
// whole point of content-addressed dedup.
func seedSnapshotObjects(store *gcStore, tenantID, siteID, snapshotID uuid.UUID, hashes ...string) {
	store.put(manifestIndexKey(tenantID, siteID, snapshotID))
	for _, h := range hashes {
		// The fake repo keys chunk rows with uuid.Nil as the tenant (see
		// gcFakeRepo.addChunk), so the object keys must match it or the sweep
		// would appear to delete nothing.
		store.put(chunkS3Key(uuid.Nil, h))
	}
}

// ---------------------------------------------------------------------------
// CARDINAL: a chunk shared with a LIVE site survives the deleted site's
// reclamation, and that live site can still resolve every chunk it needs.
// ---------------------------------------------------------------------------

func TestGH402_SiteDelete_SharedChunkSurvivesForLiveSite(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	tenantID := uuid.New()
	siteA := uuid.New() // deleted
	siteB := uuid.New() // live
	chainA := uuid.New()
	chainB := uuid.New()

	oldA := now.Add(-90 * 24 * time.Hour)
	freshB := now.Add(-24 * time.Hour)

	snapA := gcSnap(tenantID, siteA, chainA, 0, false, oldA)
	snapB := gcSnap(tenantID, siteB, chainB, 0, false, freshB)

	repo := newGCFakeRepo(now)
	repo.addSnap(snapA)
	repo.addSnap(snapB)

	// "shared" is the dangerous one: A introduced it, B still references it via
	// tenant-wide dedup, and it is the only copy of those bytes.
	repo.manifest[snapA.ID] = []ManifestEntry{
		{Path: "a.zip", EntryKind: EntryKindFile, ChunkHashes: []string{"shared", "a_only"}},
	}
	repo.manifest[snapB.ID] = []ManifestEntry{
		{Path: "b.zip", EntryKind: EntryKindFile, ChunkHashes: []string{"shared"}},
	}
	repo.addChunk("shared", oldA)
	repo.addChunk("a_only", oldA)

	store := newGCStore()
	seedSnapshotObjects(store, tenantID, siteA, snapA.ID, "shared", "a_only")
	seedSnapshotObjects(store, tenantID, siteB, snapB.ID, "shared")

	// DELETE /sites/{siteA}. The cascade destroys every DB row that named site
	// A's objects; the chunk inventory survives because backup_chunks has no FK
	// to sites.
	cascadeSiteDelete(repo, siteA)

	// The reclaim task recorded in the same transaction as the delete is now
	// worked: it lists the site's manifest root and deletes what is under it.
	w := NewReclaimWorker(nil, store, nil)
	if err := w.ReclaimPrefix(context.Background(), tenantID, siteA); err != nil {
		t.Fatalf("ReclaimPrefix: %v", err)
	}

	// A full retention pass then runs over the surviving tenant.
	svc := buildGCSvc(repo, store, now)
	if _, _, err := svc.RunRetentionGC(context.Background(), tenantID); err != nil {
		t.Fatalf("RunRetentionGC error: %v", err)
	}

	// 1. The shared chunk survives, row AND object. This is the assertion that
	//    matters: losing it silently destroys site B's backups.
	if _, ok := repo.chunks["shared"]; !ok {
		t.Error("shared chunk ROW was deleted; live site B still references it")
	}
	if !store.has(chunkS3Key(uuid.Nil, "shared")) {
		t.Error("shared chunk OBJECT was deleted; live site B can no longer restore")
	}

	// 2. Site B is still restorable: every chunk its retained snapshot needs
	//    resolves to an object that is still there.
	for _, e := range repo.manifest[snapB.ID] {
		for _, h := range e.ChunkHashes {
			if _, ok := repo.chunks[h]; !ok {
				t.Errorf("site B restore broken: chunk row %q is gone", h)
			}
			if !store.has(chunkS3Key(uuid.Nil, h)) {
				t.Errorf("site B restore broken: chunk object for %q is gone", h)
			}
		}
	}
	if !store.has(manifestIndexKey(tenantID, siteB, snapB.ID)) {
		t.Error("live site B's manifest object was deleted by the deleted site's reclamation")
	}

	// 3. The deleted site's EXCLUSIVE chunk is reclaimed by the ordinary sweep.
	if _, ok := repo.chunks["a_only"]; ok {
		t.Error("site-A-only chunk row should have been swept")
	}
	if store.has(chunkS3Key(uuid.Nil, "a_only")) {
		t.Error("site-A-only chunk object should have been swept")
	}

	// 4. THE REPORTED BUG: the deleted site's manifest objects are gone.
	if store.has(manifestIndexKey(tenantID, siteA, snapA.ID)) {
		t.Error("deleted site's manifest object is still in storage (GH #402 leak)")
	}
	for _, k := range store.list("tenant/" + tenantID.String() + "/site/" + siteA.String() + "/") {
		t.Errorf("orphan left under the deleted site's prefix: %s", k)
	}
}

// ---------------------------------------------------------------------------
// The reclaimer deletes ONLY under the derived manifest prefix. Chunk objects
// sit under a disjoint root and must be untouched no matter what.
// ---------------------------------------------------------------------------

func TestGH402_ReclaimWorker_DeletesOnlyManifestPrefix(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()
	snap1, snap2 := uuid.New(), uuid.New()

	store := newGCStore()
	seedSnapshotObjects(store, tenantID, siteID, snap1, "h1", "h2")
	seedSnapshotObjects(store, tenantID, siteID, snap2, "h2", "h3")

	w := NewReclaimWorker(nil, store, nil)
	if err := w.ReclaimPrefix(context.Background(), tenantID, siteID); err != nil {
		t.Fatalf("ReclaimPrefix: %v", err)
	}

	prefix, perr := SiteObjectPrefix(ReclaimKindBackupManifest, tenantID, siteID)
	if perr != nil {
		t.Fatalf("SiteObjectPrefix: %v", perr)
	}
	if got := store.list(prefix); len(got) != 0 {
		t.Errorf("manifest prefix not drained, %d keys remain: %v", len(got), got)
	}
	for _, h := range []string{"h1", "h2", "h3"} {
		if !store.has(chunkS3Key(uuid.Nil, h)) {
			t.Errorf("chunk object %q was deleted by the manifest reclaimer", h)
		}
	}
	for k := range store.deleted {
		if !strings.HasPrefix(k, "tenant/") {
			t.Errorf("reclaimer deleted a key outside the tenant manifest root: %s", k)
		}
	}
}

// ---------------------------------------------------------------------------
// The one-character trap: "tenant/" holds backup manifests, "tenants/" holds
// white-label client report PDFs with client PII. A sibling live site's prefix
// must survive too.
// ---------------------------------------------------------------------------

func TestGH402_ReclaimWorker_NeverDeletesOutsideDerivedPrefix(t *testing.T) {
	tenantID := uuid.New()
	deleted := uuid.New()
	live := uuid.New()
	snapDeleted, snapLive := uuid.New(), uuid.New()

	store := newGCStore()
	seedSnapshotObjects(store, tenantID, deleted, snapDeleted, "hd")
	seedSnapshotObjects(store, tenantID, live, snapLive, "hl")
	// The plural root: client report PDFs and HTML.
	pii := "tenants/" + tenantID.String() + "/reports/2026-08/report.pdf"
	store.put(pii)
	// Another tenant's manifest, same site id shape.
	otherTenant := uuid.New()
	otherKey := manifestIndexKey(otherTenant, deleted, uuid.New())
	store.put(otherKey)

	w := NewReclaimWorker(nil, store, nil)
	if err := w.ReclaimPrefix(context.Background(), tenantID, deleted); err != nil {
		t.Fatalf("ReclaimPrefix: %v", err)
	}

	if !store.has(pii) {
		t.Error("a client report PDF under the PLURAL tenants/ root was deleted")
	}
	if !store.has(manifestIndexKey(tenantID, live, snapLive)) {
		t.Error("a sibling LIVE site's manifest was deleted")
	}
	if !store.has(otherKey) {
		t.Error("another tenant's manifest was deleted")
	}
	if store.has(manifestIndexKey(tenantID, deleted, snapDeleted)) {
		t.Error("the deleted site's own manifest was not reclaimed")
	}
}

// ---------------------------------------------------------------------------
// The derived prefix is built from a code constant plus two validated UUIDs, so
// a corrupt or zero-valued row can never become an arbitrary-prefix delete.
// ---------------------------------------------------------------------------

func TestGH402_ReclaimWorker_RefusesDegeneratePrefix(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()

	store := newGCStore()
	store.put(manifestIndexKey(tenantID, siteID, uuid.New()))
	store.put("tenant/" + tenantID.String() + "/site/" + siteID.String() + "/backup/x/manifest.json")

	w := NewReclaimWorker(nil, store, nil)

	cases := []struct {
		name     string
		tenantID uuid.UUID
		siteID   uuid.UUID
	}{
		{"nil tenant", uuid.Nil, siteID},
		{"nil site", tenantID, uuid.Nil},
		{"both nil", uuid.Nil, uuid.Nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := w.ReclaimPrefix(context.Background(), tc.tenantID, tc.siteID); err == nil {
				t.Fatal("expected a refusal, got nil error")
			}
			if len(store.deleted) != 0 {
				t.Fatalf("a refused reclaim deleted %d objects", len(store.deleted))
			}
		})
	}
}
