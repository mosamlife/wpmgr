package tests

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/backup"
	"github.com/mosamlife/wpmgr/apps/api/internal/blobstore"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/httpclient"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

// --- test doubles --------------------------------------------------------

// stubSiteLookup returns a fixed enrolled site with an age recipient.
type stubSiteLookup struct {
	info backup.SiteInfo
	err  error
}

func (s stubSiteLookup) GetBackupSiteInfo(_ context.Context, _, _ uuid.UUID) (backup.SiteInfo, error) {
	return s.info, s.err
}

// stubEnqueuer captures enqueued jobs without a River round-trip.
type stubEnqueuer struct {
	mu       sync.Mutex
	backups  []uuid.UUID
	restores []backup.RestoreSelection
}

func (e *stubEnqueuer) EnqueueBackup(_ context.Context, _, snapshotID uuid.UUID) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.backups = append(e.backups, snapshotID)
	return nil
}

func (e *stubEnqueuer) EnqueueRestore(_ context.Context, _, _ uuid.UUID, sel backup.RestoreSelection, _ uuid.UUID) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.restores = append(e.restores, sel)
	return nil
}

const testRecipient = "age1ql3z7hjy54pw3hyww5ayyfg7zqgvc7w3j2elw8zmrj2kg5sfn9aqmcac8p"

// newBackupService wires a backup service against a real DB + real MinIO store.
func newBackupService(t *testing.T, pool *db.Pool, store *blobstore.Store, lookup backup.SiteLookup, enq backup.Enqueuer) *backup.Service {
	t.Helper()
	svc := backup.NewService(backup.NewRepo(pool), lookup, enq, store, domain.SystemClock{}, backup.Config{
		PresignTTL:         10 * time.Minute,
		RetentionDays:      30,
		MonthlyArchiveKeep: 12,
	})
	return svc
}

// chunkHashes are valid-looking blake3 hex digests for tests.
func chunkHashes(n int) []string {
	out := make([]string, 0, n)
	base := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	for i := 0; i < n; i++ {
		h := []byte(base)
		h[63] = "0123456789abcdef"[i%16]
		h[62] = "0123456789abcdef"[(i/16)%16]
		out = append(out, string(h))
	}
	return out
}

// putChunkObjects uploads placeholder ciphertext for each hash so GC has real
// objects to delete (content-addressed, tenant-namespaced keys).
func putChunkObjects(t *testing.T, store *blobstore.Store, tenantID uuid.UUID, hashes []string) {
	t.Helper()
	for _, h := range hashes {
		key := "chunks/" + tenantID.String() + "/" + h
		if err := store.Put(context.Background(), key, strings.NewReader("ct"), 2); err != nil {
			t.Fatalf("put chunk object: %v", err)
		}
	}
}

func submitManifest(t *testing.T, svc *backup.Service, tenantID, snapshotID uuid.UUID, entries []agentcmd.ManifestEntry) (int64, int64) {
	t.Helper()
	refs, stored, err := svc.SubmitManifest(context.Background(), tenantID, snapshotID, agentcmd.SubmitManifestRequest{
		SnapshotID:   snapshotID.String(),
		AgeRecipient: testRecipient,
		Entries:      entries,
	})
	if err != nil {
		t.Fatalf("submit manifest: %v", err)
	}
	return refs, stored
}

func enrolledSiteInfo(id uuid.UUID, url string) backup.SiteInfo {
	return backup.SiteInfo{ID: id, URL: url, Enrolled: true, AgeRecipient: testRecipient}
}

// --- tests ---------------------------------------------------------------

// TestBackupCreateAndManifestDedup: a first backup records its chunks; a second
// backup with identical chunk hashes does NOT re-store the chunks (stored_count
// is 0) but increments refcounts (chunk refs grow). This proves incremental
// dedup.
func TestBackupCreateAndManifestDedup(t *testing.T) {
	pool := startPostgres(t)
	store := startBlobstore(t)
	tenant := seedTenant(t, pool, "bk-dedup")
	siteID := seedSite(t, pool, tenant, "https://dedup.example.com")

	enq := &stubEnqueuer{}
	svc := newBackupService(t, pool, store, stubSiteLookup{info: enrolledSiteInfo(siteID, "https://dedup.example.com")}, enq)

	hashes := chunkHashes(3)
	entries := []agentcmd.ManifestEntry{
		{Path: "wp-config.php", EntryKind: "file", Mode: 0o644, Size: 12,
			Chunks: []agentcmd.ChunkRef{{Blake3: hashes[0], Size: 4}, {Blake3: hashes[1], Size: 4}, {Blake3: hashes[2], Size: 4}}},
	}

	// First backup.
	snap1, err := svc.CreateBackup(context.Background(), tenant, siteID, uuid.Nil, "full")
	if err != nil {
		t.Fatalf("create backup 1: %v", err)
	}
	refs1, stored1 := submitManifest(t, svc, tenant, snap1.ID, entries)
	if refs1 != 3 || stored1 != 3 {
		t.Fatalf("first backup refs=%d stored=%d, want 3/3", refs1, stored1)
	}

	// Second backup with identical chunk hashes.
	snap2, err := svc.CreateBackup(context.Background(), tenant, siteID, uuid.Nil, "full")
	if err != nil {
		t.Fatalf("create backup 2: %v", err)
	}
	refs2, stored2 := submitManifest(t, svc, tenant, snap2.ID, entries)
	if refs2 != 3 {
		t.Fatalf("second backup refs=%d, want 3", refs2)
	}
	if stored2 != 0 {
		t.Fatalf("second backup stored=%d, want 0 (dedup: identical chunks must not be re-stored)", stored2)
	}

	// Each chunk's refcount must now be 2 (referenced by both snapshots).
	existing, err := backup.NewRepo(pool).ExistingChunkHashes(context.Background(), tenant, hashes)
	if err != nil {
		t.Fatalf("existing chunks: %v", err)
	}
	for _, h := range hashes {
		c, ok := existing[h]
		if !ok {
			t.Fatalf("chunk %s missing after dedup", h)
		}
		if c.Refcount != 2 {
			t.Fatalf("chunk %s refcount=%d, want 2", h, c.Refcount)
		}
	}

	// The snapshot is completed.
	got, _, err := svc.GetSnapshot(context.Background(), tenant, snap2.ID)
	if err != nil {
		t.Fatalf("get snapshot: %v", err)
	}
	if got.Status != backup.StatusCompleted {
		t.Fatalf("snapshot status=%s, want completed", got.Status)
	}
}

// TestRestorePlanAssembly: full, partial-by-path, and partial-by-table restore
// selections resolve to the correct set of presigned GET chunks.
func TestRestorePlanAssembly(t *testing.T) {
	pool := startPostgres(t)
	store := startBlobstore(t)
	tenant := seedTenant(t, pool, "bk-restore")
	siteID := seedSite(t, pool, tenant, "https://restore.example.com")

	svc := newBackupService(t, pool, store, stubSiteLookup{info: enrolledSiteInfo(siteID, "https://restore.example.com")}, &stubEnqueuer{})

	h := chunkHashes(5)
	entries := []agentcmd.ManifestEntry{
		{Path: "wp-config.php", EntryKind: "file", Mode: 0o644, Size: 8,
			Chunks: []agentcmd.ChunkRef{{Blake3: h[0], Size: 4}, {Blake3: h[1], Size: 4}}},
		{Path: "index.php", EntryKind: "file", Mode: 0o644, Size: 4,
			Chunks: []agentcmd.ChunkRef{{Blake3: h[2], Size: 4}}},
		{Path: "database.sql", EntryKind: "db", TableName: "wp_posts", Mode: 0, Size: 8,
			Chunks: []agentcmd.ChunkRef{{Blake3: h[3], Size: 4}, {Blake3: h[4], Size: 4}}},
	}

	snap, err := svc.CreateBackup(context.Background(), tenant, siteID, uuid.Nil, "full")
	if err != nil {
		t.Fatalf("create backup: %v", err)
	}
	submitManifest(t, svc, tenant, snap.ID, entries)

	const restoreID = "11111111-1111-1111-1111-111111111111"
	const progressEndpoint = "https://cp.example.com/agent/v1/backups/x/progress"

	// Full restore → 3 entries, 5 chunks.
	full, _, _, err := svc.PlanRestore(context.Background(), tenant, snap.ID, backup.RestoreSelection{Full: true}, restoreID, progressEndpoint)
	if err != nil {
		t.Fatalf("plan full: %v", err)
	}
	if len(full.Manifest.Entries) != 3 || countChunks(full) != 5 {
		t.Fatalf("full restore entries=%d chunks=%d, want 3/5", len(full.Manifest.Entries), countChunks(full))
	}
	if full.RestoreID != restoreID {
		t.Fatalf("restore_id not echoed: got %q want %q", full.RestoreID, restoreID)
	}
	if full.ProgressEndpoint != progressEndpoint {
		t.Fatalf("progress endpoint not echoed: got %q want %q", full.ProgressEndpoint, progressEndpoint)
	}

	// Partial-by-path: only wp-config.php → 1 entry, 2 chunks.
	byPath, _, _, err := svc.PlanRestore(context.Background(), tenant, snap.ID, backup.RestoreSelection{Paths: []string{"wp-config.php"}}, restoreID, progressEndpoint)
	if err != nil {
		t.Fatalf("plan by path: %v", err)
	}
	if len(byPath.Manifest.Entries) != 1 || byPath.Manifest.Entries[0].LogicalPath != "wp-config.php" || countChunks(byPath) != 2 {
		t.Fatalf("by-path restore entries=%d chunks=%d, want 1/2", len(byPath.Manifest.Entries), countChunks(byPath))
	}

	// Partial-by-table: only wp_posts → 1 db entry, 2 chunks. The CP still uses
	// entry_kind+table_name to route the selection internally, but the wire only
	// carries the entry's logical_path (here: "database.sql").
	byTable, _, _, err := svc.PlanRestore(context.Background(), tenant, snap.ID, backup.RestoreSelection{DBTables: []string{"wp_posts"}}, restoreID, progressEndpoint)
	if err != nil {
		t.Fatalf("plan by table: %v", err)
	}
	if len(byTable.Manifest.Entries) != 1 || byTable.Manifest.Entries[0].LogicalPath != "database.sql" || countChunks(byTable) != 2 {
		t.Fatalf("by-table restore entries=%d chunks=%d logical_path=%q, want 1/2/database.sql", len(byTable.Manifest.Entries), countChunks(byTable), byTable.Manifest.Entries[0].LogicalPath)
	}

	// Every presigned GET URL must target this tenant's chunk prefix.
	prefix := "chunks/" + tenant.String() + "/"
	for _, e := range full.Manifest.Entries {
		for _, c := range e.Chunks {
			if c.URL == "" {
				t.Fatal("restore chunk missing presigned GET URL")
			}
			if !strings.Contains(c.URL, prefix) {
				t.Fatalf("presigned GET URL %q is not namespaced to tenant prefix %q", c.URL, prefix)
			}
		}
	}

	// A selection that matches nothing is a 422.
	if _, _, _, err := svc.PlanRestore(context.Background(), tenant, snap.ID, backup.RestoreSelection{Paths: []string{"nope.php"}}, restoreID, progressEndpoint); err == nil {
		t.Fatal("expected error for empty restore selection")
	}
}

// TestPresignNamespacing: PresignChunks for a tenant only mints URLs for that
// tenant's chunk prefix, and dedup omits already-stored hashes.
func TestPresignNamespacing(t *testing.T) {
	pool := startPostgres(t)
	store := startBlobstore(t)
	tenantA := seedTenant(t, pool, "bk-ns-a")
	tenantB := seedTenant(t, pool, "bk-ns-b")
	siteA := seedSite(t, pool, tenantA, "https://a.example.com")

	svc := newBackupService(t, pool, store, stubSiteLookup{info: enrolledSiteInfo(siteA, "https://a.example.com")}, &stubEnqueuer{})

	snap, err := svc.CreateBackup(context.Background(), tenantA, siteA, uuid.Nil, "files")
	if err != nil {
		t.Fatalf("create backup: %v", err)
	}
	h := chunkHashes(2)
	uploads, err := svc.PresignChunks(context.Background(), tenantA, snap.ID, h)
	if err != nil {
		t.Fatalf("presign chunks: %v", err)
	}
	if len(uploads) != 2 {
		t.Fatalf("presign uploads=%d, want 2", len(uploads))
	}
	prefixA := "chunks/" + tenantA.String() + "/"
	prefixB := "chunks/" + tenantB.String() + "/"
	for hash, url := range uploads {
		if !strings.Contains(url, prefixA+hash) {
			t.Fatalf("presigned PUT %q is not in tenant A's prefix %q", url, prefixA)
		}
		if strings.Contains(url, prefixB) {
			t.Fatalf("presigned PUT %q leaks into tenant B's prefix %q", url, prefixB)
		}
	}

	// After recording the chunks, a second presign for the same hashes returns
	// nothing (dedup).
	submitManifest(t, svc, tenantA, snap.ID, []agentcmd.ManifestEntry{
		{Path: "f.php", EntryKind: "file", Size: 8, Chunks: []agentcmd.ChunkRef{{Blake3: h[0], Size: 4}, {Blake3: h[1], Size: 4}}},
	})
	snap2, _ := svc.CreateBackup(context.Background(), tenantA, siteA, uuid.Nil, "files")
	uploads2, err := svc.PresignChunks(context.Background(), tenantA, snap2.ID, h)
	if err != nil {
		t.Fatalf("presign chunks 2: %v", err)
	}
	if len(uploads2) != 0 {
		t.Fatalf("dedup presign uploads=%d, want 0 (chunks already stored)", len(uploads2))
	}
}

// TestRetentionGC: an expired snapshot is deleted, its chunks' refcounts are
// decremented, orphaned chunks (refcount 0) are deleted from S3, and chunks
// still shared with a surviving snapshot are retained.
func TestRetentionGC(t *testing.T) {
	pool := startPostgres(t)
	store := startBlobstore(t)
	tenant := seedTenant(t, pool, "bk-gc")
	siteID := seedSite(t, pool, tenant, "https://gc.example.com")

	// MonthlyArchiveKeep=0 isolates the rolling-window prune (the monthly-archive
	// rule is exercised separately).
	svc := backup.NewService(backup.NewRepo(pool), stubSiteLookup{info: enrolledSiteInfo(siteID, "https://gc.example.com")}, &stubEnqueuer{}, store, domain.SystemClock{}, backup.Config{
		PresignTTL:         10 * time.Minute,
		RetentionDays:      30,
		MonthlyArchiveKeep: 0,
	})
	repo := backup.NewRepo(pool)

	h := chunkHashes(3)
	putChunkObjects(t, store, tenant, h)

	// Old snapshot references h[0], h[1], h[2].
	oldSnap := seedBackupSnapshotAt(t, pool, tenant, siteID, time.Now().Add(-60*24*time.Hour))
	submitManifest(t, svc, tenant, oldSnap, []agentcmd.ManifestEntry{
		{Path: "old.php", EntryKind: "file", Size: 12, Chunks: refs(h[0], h[1], h[2])},
	})
	// Recent snapshot references h[2] only (shared with old).
	newSnap, err := svc.CreateBackup(context.Background(), tenant, siteID, uuid.Nil, "files")
	if err != nil {
		t.Fatalf("create new snapshot: %v", err)
	}
	submitManifest(t, svc, tenant, newSnap.ID, []agentcmd.ManifestEntry{
		{Path: "new.php", EntryKind: "file", Size: 4, Chunks: refs(h[2])},
	})

	// Run GC: the old snapshot (60 days, >30 day window) is pruned.
	snaps, chunks, err := svc.RunRetentionGC(context.Background(), tenant)
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if snaps != 1 {
		t.Fatalf("gc snapshots deleted=%d, want 1", snaps)
	}
	if chunks != 2 {
		t.Fatalf("gc chunks deleted=%d, want 2 (h0,h1 orphaned; h2 shared)", chunks)
	}

	// h[0] and h[1] are gone from S3; h[2] (shared) remains.
	for _, gone := range []string{h[0], h[1]} {
		if exists, _, _ := store.Head(context.Background(), "chunks/"+tenant.String()+"/"+gone); exists {
			t.Fatalf("orphan chunk %s still in S3", gone)
		}
	}
	if exists, _, _ := store.Head(context.Background(), "chunks/"+tenant.String()+"/"+h[2]); !exists {
		t.Fatalf("shared chunk %s wrongly deleted from S3", h[2])
	}

	// h[2] retained with refcount 1 (only the new snapshot now).
	existing, _ := repo.ExistingChunkHashes(context.Background(), tenant, h)
	if c, ok := existing[h[2]]; !ok || c.Refcount != 1 {
		t.Fatalf("shared chunk h2 refcount=%v (present=%v), want 1", existing[h[2]].Refcount, ok)
	}
	if _, ok := existing[h[0]]; ok {
		t.Fatalf("orphan chunk h0 row still present")
	}

	// The old snapshot is gone.
	if _, _, err := svc.GetSnapshot(context.Background(), tenant, oldSnap); err == nil {
		t.Fatal("old snapshot still present after GC")
	}
}

// TestRetentionMonthlyArchive: an expired snapshot that is the newest in its
// calendar month is flagged as a monthly archive and SURVIVES the rolling-window
// prune (its chunks are retained).
func TestRetentionMonthlyArchive(t *testing.T) {
	pool := startPostgres(t)
	store := startBlobstore(t)
	tenant := seedTenant(t, pool, "bk-archive")
	siteID := seedSite(t, pool, tenant, "https://archive.example.com")

	// MonthlyArchiveKeep=12 (default) keeps the newest snapshot per month.
	svc := newBackupService(t, pool, store, stubSiteLookup{info: enrolledSiteInfo(siteID, "https://archive.example.com")}, &stubEnqueuer{})

	h := chunkHashes(1)
	putChunkObjects(t, store, tenant, h)
	oldSnap := seedBackupSnapshotAt(t, pool, tenant, siteID, time.Now().Add(-90*24*time.Hour))
	submitManifest(t, svc, tenant, oldSnap, []agentcmd.ManifestEntry{
		{Path: "old.php", EntryKind: "file", Size: 4, Chunks: refs(h[0])},
	})

	snaps, chunks, err := svc.RunRetentionGC(context.Background(), tenant)
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if snaps != 0 || chunks != 0 {
		t.Fatalf("monthly-archive snapshot was pruned (snaps=%d chunks=%d), want 0/0", snaps, chunks)
	}
	// The snapshot survives and is flagged archived; its chunk object remains.
	got, _, err := svc.GetSnapshot(context.Background(), tenant, oldSnap)
	if err != nil {
		t.Fatalf("get archived snapshot: %v", err)
	}
	if !got.Archived {
		t.Fatal("expired newest-in-month snapshot was not flagged as a monthly archive")
	}
	if exists, _, _ := store.Head(context.Background(), "chunks/"+tenant.String()+"/"+h[0]); !exists {
		t.Fatal("archived snapshot's chunk wrongly deleted from S3")
	}
}

// TestBackupRLSIsolation: tenant A's backup snapshots and chunks are invisible
// to tenant B (RLS on backup_snapshots/backup_chunks/backup_manifest_entries).
func TestBackupRLSIsolation(t *testing.T) {
	pool := startPostgres(t)
	store := startBlobstore(t)
	tenantA := seedTenant(t, pool, "bk-rls-a")
	tenantB := seedTenant(t, pool, "bk-rls-b")
	siteA := seedSite(t, pool, tenantA, "https://a.example.com")

	svc := newBackupService(t, pool, store, stubSiteLookup{info: enrolledSiteInfo(siteA, "https://a.example.com")}, &stubEnqueuer{})
	repo := backup.NewRepo(pool)

	snapA, err := svc.CreateBackup(context.Background(), tenantA, siteA, uuid.Nil, "full")
	if err != nil {
		t.Fatalf("create backup A: %v", err)
	}
	h := chunkHashes(2)
	submitManifest(t, svc, tenantA, snapA.ID, []agentcmd.ManifestEntry{
		{Path: "a.php", EntryKind: "file", Size: 8, Chunks: refs(h[0], h[1])},
	})

	// Tenant B cannot read tenant A's snapshot.
	if _, _, err := svc.GetSnapshot(context.Background(), tenantB, snapA.ID); err == nil {
		t.Fatal("tenant B read tenant A's snapshot (RLS breach)")
	}
	// Tenant B sees none of tenant A's chunks.
	existingB, err := repo.ExistingChunkHashes(context.Background(), tenantB, h)
	if err != nil {
		t.Fatalf("existing chunks B: %v", err)
	}
	if len(existingB) != 0 {
		t.Fatalf("tenant B saw %d of tenant A's chunks (RLS breach)", len(existingB))
	}
	// Tenant B lists no snapshots for tenant A's site id.
	listB, err := svc.ListSnapshots(context.Background(), tenantB, siteA, 50, 0)
	if err != nil {
		t.Fatalf("list B: %v", err)
	}
	if len(listB) != 0 {
		t.Fatalf("tenant B listed %d of tenant A's snapshots (RLS breach)", len(listB))
	}
}

// TestBackupCommandHasNoDecryptionKey: the CP->agent `backup` command payload
// carries only the age PUBLIC recipient + callback endpoints and NEVER any
// private key/identity. We capture the request a real BackupWorker sends to a
// fake agent and assert the recipient is the public one and no private-key
// material appears anywhere in the JSON body.
func TestBackupCommandHasNoDecryptionKey(t *testing.T) {
	pool := startPostgres(t)
	store := startBlobstore(t)
	tenant := seedTenant(t, pool, "bk-trust")
	siteID := seedSite(t, pool, tenant, "")

	// Fake agent records the raw backup command body.
	var mu sync.Mutex
	var rawBody []byte
	var sawReq agentcmd.BackupRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		mu.Lock()
		rawBody = body
		_ = json.Unmarshal(body, &sawReq)
		mu.Unlock()
		writeJSON(w, agentcmd.BackupResponse{OK: true, Detail: "queued"})
	}))
	t.Cleanup(srv.Close)

	lookup := stubSiteLookup{info: backup.SiteInfo{ID: siteID, URL: srv.URL, Enrolled: true, AgeRecipient: testRecipient}}
	svc := newBackupService(t, pool, store, lookup, &stubEnqueuer{})

	// Create a snapshot and run the backup worker against the fake agent.
	snap, err := svc.CreateBackup(context.Background(), tenant, siteID, uuid.Nil, "full")
	if err != nil {
		t.Fatalf("create backup: %v", err)
	}
	commander := newTestBackupCommander(t)
	worker := backup.NewBackupWorker(svc, commander, nil, nil, "https://cp.example.com", 30*time.Second)
	if err := worker.Work(context.Background(), backupJob(snap.ID, tenant)); err != nil {
		t.Fatalf("backup worker: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if sawReq.AgeRecipient != testRecipient {
		t.Fatalf("backup command age_recipient = %q, want the PUBLIC recipient %q", sawReq.AgeRecipient, testRecipient)
	}
	if !strings.HasPrefix(sawReq.AgeRecipient, "age1") {
		t.Fatalf("recipient %q is not an age PUBLIC recipient", sawReq.AgeRecipient)
	}
	// The body must not contain any age IDENTITY (private key) material.
	lower := strings.ToLower(string(rawBody))
	for _, forbidden := range []string{"age-secret-key", "identity", "private", "secret_key", "secretkey"} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("backup command body contains forbidden key material %q: %s", forbidden, rawBody)
		}
	}
	// Presign + manifest callback endpoints must be set (agent uploads ciphertext
	// directly to S3 and submits the manifest to the CP).
	if sawReq.PresignEndpoint == "" || sawReq.ManifestEndpoint == "" {
		t.Fatal("backup command must carry presign + manifest callback endpoints")
	}
}

// --- helpers -------------------------------------------------------------

// newTestBackupCommander builds a real agentcmd.Client over a loopback-
// permitting SSRF client and an ephemeral signer (so commands reach the
// httptest fake agent).
func newTestBackupCommander(t *testing.T) backup.Commander {
	t.Helper()
	c := httpclient.New(httpclient.Config{AllowPrivateNetworks: true, Timeout: 5 * time.Second})
	signer, err := agentcmd.NewSigner(genEd25519PrivBase64(t))
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	return agentcmd.NewClient(c, signer)
}

// backupJob builds a River job carrying BackupArgs; the worker reads only
// job.Args, so a nil JobRow is fine for a direct Work() call.
func backupJob(snapshotID, tenant uuid.UUID) *river.Job[backup.BackupArgs] {
	return &river.Job[backup.BackupArgs]{Args: backup.BackupArgs{TenantID: tenant, SnapshotID: snapshotID}}
}

func countChunks(r agentcmd.RestoreRequest) int {
	n := 0
	for _, e := range r.Manifest.Entries {
		n += len(e.Chunks)
	}
	return n
}

func refs(hashes ...string) []agentcmd.ChunkRef {
	out := make([]agentcmd.ChunkRef, 0, len(hashes))
	for _, h := range hashes {
		out = append(out, agentcmd.ChunkRef{Blake3: h, Size: 4})
	}
	return out
}

// seedSite inserts an enrolled site row directly (tenant-scoped insert via a
// short RLS transaction) and returns its id.
func seedSite(t *testing.T, pool *db.Pool, tenant uuid.UUID, url string) uuid.UUID {
	t.Helper()
	if url == "" {
		url = "https://seed-" + uuid.NewString() + ".example.com"
	}
	svc := site.NewService(site.NewRepo(pool), domain.NewValidator(), domain.SystemClock{})
	s, err := svc.Create(context.Background(), site.CreateInput{TenantID: tenant, URL: url, Name: "seed"})
	if err != nil {
		t.Fatalf("seed site: %v", err)
	}
	// Mark enrolled so backup creation is permitted, and set the recipient.
	_, err = svc.SetAgeRecipient(context.Background(), tenant, s.ID, testRecipient)
	if err != nil {
		t.Fatalf("set recipient: %v", err)
	}
	admin := connectAdmin(t, pool)
	defer admin.Close()
	if _, err := admin.Exec(context.Background(), "UPDATE sites SET enrolled_at = now() WHERE id = $1", s.ID); err != nil {
		t.Fatalf("mark enrolled: %v", err)
	}
	return s.ID
}

// seedBackupSnapshotAt inserts a pending snapshot with a backdated created_at so
// the retention GC sees it as expired. Returns its id.
func seedBackupSnapshotAt(t *testing.T, pool *db.Pool, tenant, siteID uuid.UUID, createdAt time.Time) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pool.InTenantTx(context.Background(), tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`INSERT INTO backup_snapshots (tenant_id, site_id, kind, status, age_recipient, created_at)
			 VALUES ($1, $2, 'files', 'pending', $3, $4) RETURNING id`,
			tenant, siteID, testRecipient, createdAt).Scan(&id)
	})
	if err != nil {
		t.Fatalf("seed backdated snapshot: %v", err)
	}
	return id
}
