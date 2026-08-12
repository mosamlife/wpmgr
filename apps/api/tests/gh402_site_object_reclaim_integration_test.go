package tests

// gh402_site_object_reclaim_integration_test.go, GH #402 against real Postgres:
// real migrations, real RLS, a real non-superuser application role.
//
// The unit tests in internal/backup prove the reclaim worker's decisions with
// fakes. What only a database can prove is the part the report was actually
// about: that the reclaim record survives the very cascade that destroys
// everything else naming the site's objects, that it commits and rolls back
// with the delete rather than beside it, that the new table's RLS is real, and
// that the widened GC roster query returns what it claims to.
//
// NOT RUN BY CI (apps/api/tests is excluded from the fast lane by owner
// decision because it takes about 18 minutes). Run with `make test-integration`.

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	// adminpkg, not admin: every test in this file already names its superuser
	// pool `admin`.
	adminpkg "github.com/mosamlife/wpmgr/apps/api/internal/admin"
	"github.com/mosamlife/wpmgr/apps/api/internal/backup"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func gh402SeedSite(t *testing.T, admin *db.Pool, tenant uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := admin.QueryRow(context.Background(),
		`INSERT INTO sites (tenant_id, url, name) VALUES ($1, $2, 'gh402') RETURNING id`,
		tenant, "https://"+uuid.NewString()+".example.com").Scan(&id); err != nil {
		t.Fatalf("seed site: %v", err)
	}
	return id
}

// gh402SeedSnapshot inserts a completed snapshot and its manifest object key is
// derived the same way the control plane derives it.
func gh402SeedSnapshot(t *testing.T, admin *db.Pool, tenant, siteID uuid.UUID, status string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := admin.QueryRow(context.Background(),
		`INSERT INTO backup_snapshots (tenant_id, site_id, kind, status)
		 VALUES ($1, $2, 'full', $3) RETURNING id`,
		tenant, siteID, status).Scan(&id); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
	return id
}

func gh402SeedChunk(t *testing.T, admin *db.Pool, tenant uuid.UUID, blake3 string) {
	t.Helper()
	if _, err := admin.Exec(context.Background(),
		`INSERT INTO backup_chunks (tenant_id, blake3, s3_key, size)
		 VALUES ($1, $2, $3, 1)
		 ON CONFLICT (tenant_id, blake3) DO NOTHING`,
		tenant, blake3, "chunks/"+tenant.String()+"/"+blake3); err != nil {
		t.Fatalf("seed chunk: %v", err)
	}
}

func gh402ManifestKey(tenant, siteID, snapshot uuid.UUID) string {
	return "tenant/" + tenant.String() + "/site/" + siteID.String() +
		"/backup/" + snapshot.String() + "/manifest.json"
}

func gh402CountTasks(t *testing.T, admin *db.Pool, tenant, siteID uuid.UUID) int {
	t.Helper()
	var n int
	if err := admin.QueryRow(context.Background(),
		`SELECT count(*) FROM site_object_reclaim WHERE tenant_id = $1 AND site_id = $2`,
		tenant, siteID).Scan(&n); err != nil {
		t.Fatalf("count reclaim tasks: %v", err)
	}
	return n
}

// gh402Store is an in-memory object store implementing backup.ObjectReclaimer.
type gh402Store struct {
	objects map[string]bool
	failOn  string
}

func newGH402Store() *gh402Store { return &gh402Store{objects: map[string]bool{}} }

func (s *gh402Store) put(key string) { s.objects[key] = true }
func (s *gh402Store) has(key string) bool {
	return s.objects[key]
}

func (s *gh402Store) List(_ context.Context, prefix string) ([]string, error) {
	var out []string
	for k := range s.objects {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	// Sorted, because Go randomises map iteration and S3 and GCS both list
	// lexicographically. Unsorted, this fake returned a different order every
	// run, so a test that injects a fault on one key could see that key first
	// and observe no progress before the fault. That failed about two runs in
	// three and looked like a flake in the drain rather than what it was: the
	// fake being less predictable than the thing it stands in for.
	sort.Strings(out)
	return out, nil
}

func (s *gh402Store) Delete(_ context.Context, key string) error {
	if s.failOn != "" && key == s.failOn {
		return errors.New("simulated storage fault")
	}
	delete(s.objects, key)
	return nil
}

// ---------------------------------------------------------------------------
// 1. The reclaim record survives the cascade that destroys everything else.
// ---------------------------------------------------------------------------

func TestGH402_ReclaimTaskSurvivesSiteCascade(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()

	tenant := seedTenant(t, pool, "gh402-cascade-"+uuid.NewString()[:8])
	siteID := gh402SeedSite(t, admin, tenant)
	snapA := gh402SeedSnapshot(t, admin, tenant, siteID, "completed")
	snapB := gh402SeedSnapshot(t, admin, tenant, siteID, "completed")

	if err := site.NewRepo(pool).Delete(ctx, tenant, siteID); err != nil {
		t.Fatalf("delete site: %v", err)
	}

	// The cascade did what the report said it does.
	for _, id := range []uuid.UUID{snapA, snapB} {
		var n int
		if err := admin.QueryRow(ctx,
			`SELECT count(*) FROM backup_snapshots WHERE id = $1`, id).Scan(&n); err != nil {
			t.Fatalf("count snapshots: %v", err)
		}
		if n != 0 {
			t.Fatalf("snapshot %s survived the site delete; this test's premise is wrong", id)
		}
	}

	// And the reclaim record did NOT go with it. If someone adds a site_id
	// foreign key to site_object_reclaim, this is where it shows up.
	if got := gh402CountTasks(t, admin, tenant, siteID); got != 1 {
		t.Fatalf("expected exactly 1 surviving reclaim task, got %d. "+
			"A foreign key to sites would cascade it away in the same statement, "+
			"which is the entire defect of GH #402", got)
	}

	var kind string
	var completedAt *time.Time
	if err := admin.QueryRow(ctx,
		`SELECT kind, completed_at FROM site_object_reclaim WHERE tenant_id = $1 AND site_id = $2`,
		tenant, siteID).Scan(&kind, &completedAt); err != nil {
		t.Fatalf("read reclaim task: %v", err)
	}
	if kind != backup.ReclaimKindBackupManifest {
		t.Errorf("task kind = %q, want %q", kind, backup.ReclaimKindBackupManifest)
	}
	if completedAt != nil {
		t.Error("a freshly enqueued task must be open, not already completed")
	}
}

// ---------------------------------------------------------------------------
// 2. Enqueue and delete are ONE transaction.
//
// This is the test that catches someone "simplifying" the enqueue into a
// separate transaction written before the cascade. If the delete does not
// happen, the record must not exist either: a durable instruction to delete a
// LIVE site's manifests is worse than the leak it was meant to fix.
// ---------------------------------------------------------------------------

func TestGH402_SiteDelete_EnqueuesReclaimInSameTx(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()

	tenant := seedTenant(t, pool, "gh402-atomic-"+uuid.NewString()[:8])
	siteID := gh402SeedSite(t, admin, tenant)

	// (a) A delete that finds nothing must leave no record. Same tenant, a site
	// id that does not exist: the rows-affected check fails the transaction, so
	// the insert can never have happened.
	ghost := uuid.New()
	err := site.NewRepo(pool).Delete(ctx, tenant, ghost)
	if err == nil {
		t.Fatal("deleting a nonexistent site should fail")
	}
	if de, ok := domain.AsDomain(err); !ok || de.Kind != domain.KindNotFound {
		t.Fatalf("expected a not-found domain error, got %v", err)
	}
	if got := gh402CountTasks(t, admin, tenant, ghost); got != 0 {
		t.Fatalf("a failed delete left %d reclaim task(s) behind; the record must exist "+
			"if and ONLY if the site row is really gone", got)
	}

	// (b) A delete that rolls back after the insert must leave no record
	// either. Force the rollback from inside the same transaction the repo
	// uses by making the DELETE itself fail: a foreign key that refuses the
	// cascade does exactly that. site_object_reclaim's own row is written after
	// the DELETE, so if the two were in separate transactions the record would
	// survive this.
	blocked := gh402SeedSite(t, admin, tenant)
	if _, ferr := admin.Exec(ctx,
		`CREATE TABLE gh402_blocker (
		    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
		    site_id uuid NOT NULL REFERENCES sites (id) ON DELETE RESTRICT
		 )`); ferr != nil {
		t.Fatalf("create blocker table: %v", ferr)
	}
	if _, ferr := admin.Exec(ctx,
		`INSERT INTO gh402_blocker (site_id) VALUES ($1)`, blocked); ferr != nil {
		t.Fatalf("seed blocker row: %v", ferr)
	}
	if derr := site.NewRepo(pool).Delete(ctx, tenant, blocked); derr == nil {
		t.Fatal("expected the blocked delete to fail")
	}
	var stillThere int
	if qerr := admin.QueryRow(ctx,
		`SELECT count(*) FROM sites WHERE id = $1`, blocked).Scan(&stillThere); qerr != nil {
		t.Fatalf("count sites: %v", qerr)
	}
	if stillThere != 1 {
		t.Fatal("the blocked site was deleted; this test's premise is wrong")
	}
	if got := gh402CountTasks(t, admin, tenant, blocked); got != 0 {
		t.Fatalf("a rolled-back delete left %d reclaim task(s) behind: the insert is NOT "+
			"in the same transaction as the delete (GH #402)", got)
	}

	// (c) The successful path does write the record.
	if derr := site.NewRepo(pool).Delete(ctx, tenant, siteID); derr != nil {
		t.Fatalf("delete site: %v", derr)
	}
	if got := gh402CountTasks(t, admin, tenant, siteID); got != 1 {
		t.Fatalf("expected 1 reclaim task after a successful delete, got %d", got)
	}
}

// ---------------------------------------------------------------------------
// 3. The new table's RLS is real.
// ---------------------------------------------------------------------------

func TestGH402_ReclaimTaskRLS(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()

	tenantA := seedTenant(t, pool, "gh402-rls-a-"+uuid.NewString()[:8])
	tenantB := seedTenant(t, pool, "gh402-rls-b-"+uuid.NewString()[:8])
	siteA := gh402SeedSite(t, admin, tenantA)

	if err := site.NewRepo(pool).Delete(ctx, tenantA, siteA); err != nil {
		t.Fatalf("delete site: %v", err)
	}

	// Tenant B cannot see tenant A's task.
	var seenByB int
	if err := pool.InTenantTx(ctx, tenantB, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM site_object_reclaim`).Scan(&seenByB)
	}); err != nil {
		t.Fatalf("tenant B read: %v", err)
	}
	if seenByB != 0 {
		t.Errorf("tenant B sees %d of tenant A's reclaim tasks; tenant isolation is broken", seenByB)
	}

	// Tenant A can see its own.
	var seenByA int
	if err := pool.InTenantTx(ctx, tenantA, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM site_object_reclaim`).Scan(&seenByA)
	}); err != nil {
		t.Fatalf("tenant A read: %v", err)
	}
	if seenByA != 1 {
		t.Errorf("tenant A sees %d of its own reclaim tasks, want 1", seenByA)
	}

	// Tenant B cannot WRITE a row into tenant A's scope: WITH CHECK refuses it.
	werr := pool.InTenantTx(ctx, tenantB, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx,
			`INSERT INTO site_object_reclaim (tenant_id, site_id, kind) VALUES ($1, $2, 'backup_manifest')`,
			tenantA, uuid.New())
		return e
	})
	if werr == nil {
		t.Error("tenant B inserted a reclaim task into tenant A's scope; WITH CHECK is missing")
	}

	// The cross-tenant worker path sees it under app.agent.
	var seenByAgent int
	if err := pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM site_object_reclaim`).Scan(&seenByAgent)
	}); err != nil {
		t.Fatalf("agent read: %v", err)
	}
	if seenByAgent != 1 {
		t.Errorf("the agent policy sees %d tasks, want 1; the reclaim worker could never run", seenByAgent)
	}
}

// ---------------------------------------------------------------------------
// 4. The site-existence guard reads the RAW sites table.
//
// An ARCHIVED site is LIVE and restorable. site.ListAllSiteIDs filters
// connection_state <> 'archived' and is the obvious-looking helper to reach
// for; using it would make an archived site look orphaned and wipe its backups'
// manifests. This asserts the query the worker actually uses does not.
// ---------------------------------------------------------------------------

func TestGH402_ReclaimSiteExistsSeesArchivedSites(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()

	tenant := seedTenant(t, pool, "gh402-archived-"+uuid.NewString()[:8])
	archived := gh402SeedSite(t, admin, tenant)
	if _, err := admin.Exec(ctx,
		`UPDATE sites SET connection_state = 'archived' WHERE id = $1`, archived); err != nil {
		t.Fatalf("archive site: %v", err)
	}

	state := backup.NewReclaimStore(pool)
	exists, err := state.SiteExists(ctx, tenant, archived)
	if err != nil {
		t.Fatalf("SiteExists: %v", err)
	}
	if !exists {
		t.Fatal("the guard reports an ARCHIVED site as gone. An archived site is live and " +
			"restorable; reclaiming its prefix destroys its backups' manifests (GH #402)")
	}

	// And a genuinely deleted site does read as gone.
	deleted := gh402SeedSite(t, admin, tenant)
	if derr := site.NewRepo(pool).Delete(ctx, tenant, deleted); derr != nil {
		t.Fatalf("delete site: %v", derr)
	}
	gone, gerr := state.SiteExists(ctx, tenant, deleted)
	if gerr != nil {
		t.Fatalf("SiteExists: %v", gerr)
	}
	if gone {
		t.Error("a deleted site still reads as present")
	}
}

// ---------------------------------------------------------------------------
// 5. End to end: the reported field scenario.
// ---------------------------------------------------------------------------

func TestGH402_SiteDeleteLeavesNoOrphanedManifests(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()

	tenant := seedTenant(t, pool, "gh402-e2e-"+uuid.NewString()[:8])
	doomed := gh402SeedSite(t, admin, tenant)
	live := gh402SeedSite(t, admin, tenant)

	store := newGH402Store()
	// The reported shape: many completed snapshots on the site being deleted.
	var doomedKeys []string
	for i := 0; i < 12; i++ {
		snap := gh402SeedSnapshot(t, admin, tenant, doomed, "completed")
		k := gh402ManifestKey(tenant, doomed, snap)
		store.put(k)
		doomedKeys = append(doomedKeys, k)
	}
	liveSnap := gh402SeedSnapshot(t, admin, tenant, live, "completed")
	liveKey := gh402ManifestKey(tenant, live, liveSnap)
	store.put(liveKey)

	// Chunks are content-addressed and tenant-wide: this one is shared.
	sharedChunk := "chunks/" + tenant.String() + "/sharedblake3"
	gh402SeedChunk(t, admin, tenant, "sharedblake3")
	store.put(sharedChunk)
	// And the client report PDFs under the PLURAL root, one character away.
	pii := "tenants/" + tenant.String() + "/reports/2026-08/client.pdf"
	store.put(pii)

	if err := site.NewRepo(pool).Delete(ctx, tenant, doomed); err != nil {
		t.Fatalf("delete site: %v", err)
	}

	worker := backup.NewReclaimWorker(backup.NewReclaimStore(pool), store, nil)
	if err := worker.Work(ctx, nil); err != nil {
		t.Fatalf("reclaim sweep: %v", err)
	}

	prefix := "tenant/" + tenant.String() + "/site/" + doomed.String() + "/"
	left, _ := store.List(ctx, prefix)
	if len(left) != 0 {
		t.Errorf("%d orphaned manifest objects remain under %s: %v", len(left), prefix, left)
	}
	for _, k := range doomedKeys {
		if store.has(k) {
			t.Errorf("orphan not reclaimed: %s", k)
		}
	}
	if !store.has(liveKey) {
		t.Error("the LIVE site's manifest was deleted")
	}
	if !store.has(sharedChunk) {
		t.Error("a deduplicated chunk object was deleted by the manifest reclaimer")
	}
	if !store.has(pii) {
		t.Error("a client report PDF under the plural tenants/ root was deleted")
	}
	// The chunk ROW is untouched too: it has no foreign key to sites, which is
	// what lets the tenant-wide sweep make the keep-or-delete decision properly.
	var chunkRows int
	if err := admin.QueryRow(ctx,
		`SELECT count(*) FROM backup_chunks WHERE tenant_id = $1`, tenant).Scan(&chunkRows); err != nil {
		t.Fatalf("count chunks: %v", err)
	}
	if chunkRows != 1 {
		t.Errorf("backup_chunks rows = %d, want 1; the cascade must not touch the chunk inventory", chunkRows)
	}

	// The task closed.
	var completedAt *time.Time
	if err := admin.QueryRow(ctx,
		`SELECT completed_at FROM site_object_reclaim WHERE tenant_id = $1 AND site_id = $2`,
		tenant, doomed).Scan(&completedAt); err != nil {
		t.Fatalf("read task: %v", err)
	}
	if completedAt == nil {
		t.Error("the task did not close after a clean drain")
	}

	// Re-running is harmless.
	if err := worker.Work(ctx, nil); err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if !store.has(liveKey) || !store.has(sharedChunk) || !store.has(pii) {
		t.Error("a second sweep deleted something it should not have")
	}
}

// A storage fault leaves the task open with the reason recorded, and the next
// sweep finishes the job.
func TestGH402_ReclaimResumesAfterStorageFault(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()

	tenant := seedTenant(t, pool, "gh402-resume-"+uuid.NewString()[:8])
	siteID := gh402SeedSite(t, admin, tenant)

	store := newGH402Store()
	var keys []string
	for i := 0; i < 4; i++ {
		snap := gh402SeedSnapshot(t, admin, tenant, siteID, "completed")
		k := gh402ManifestKey(tenant, siteID, snap)
		store.put(k)
		keys = append(keys, k)
	}
	store.failOn = keys[2]

	if err := site.NewRepo(pool).Delete(ctx, tenant, siteID); err != nil {
		t.Fatalf("delete site: %v", err)
	}

	worker := backup.NewReclaimWorker(backup.NewReclaimStore(pool), store, nil)
	if err := worker.Work(ctx, nil); err != nil {
		t.Fatalf("first sweep: %v", err)
	}

	var completedAt *time.Time
	var attempts int
	var lastError *string
	var nextAttempt time.Time
	if err := admin.QueryRow(ctx,
		`SELECT completed_at, attempts, last_error, next_attempt_at
		 FROM site_object_reclaim WHERE tenant_id = $1 AND site_id = $2`,
		tenant, siteID).Scan(&completedAt, &attempts, &lastError, &nextAttempt); err != nil {
		t.Fatalf("read task: %v", err)
	}
	if completedAt != nil {
		t.Error("a partially drained prefix was marked complete")
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}
	if lastError == nil || *lastError == "" {
		t.Error("the failure reason was not recorded on the task")
	}
	if !nextAttempt.After(time.Now()) {
		t.Error("the task was not backed off")
	}

	// The backoff means the next sweep skips it; clear it the way an operator
	// or a later tick would reach it, then heal storage and finish.
	if _, err := admin.Exec(ctx,
		`UPDATE site_object_reclaim SET next_attempt_at = now() - interval '1 minute'
		 WHERE tenant_id = $1 AND site_id = $2`, tenant, siteID); err != nil {
		t.Fatalf("clear backoff: %v", err)
	}
	store.failOn = ""
	if err := worker.Work(ctx, nil); err != nil {
		t.Fatalf("second sweep: %v", err)
	}

	if err := admin.QueryRow(ctx,
		`SELECT completed_at FROM site_object_reclaim WHERE tenant_id = $1 AND site_id = $2`,
		tenant, siteID).Scan(&completedAt); err != nil {
		t.Fatalf("read task: %v", err)
	}
	if completedAt == nil {
		t.Error("the resumed sweep did not close the task")
	}
	left, _ := store.List(ctx, "tenant/"+tenant.String()+"/site/"+siteID.String()+"/")
	if len(left) != 0 {
		t.Errorf("%d keys remain after resume: %v", len(left), left)
	}
}

// ---------------------------------------------------------------------------
// 6. The widened GC roster, against the real SQL.
//
// Deleting the site that held a tenant's LAST completed snapshot used to drop
// that tenant off the roster permanently, so its chunk bytes leaked too. The
// union with backup_chunks is what reaches it.
// ---------------------------------------------------------------------------

func TestGH402_GCRoster_IncludesTenantWithChunksButNoCompletedSnapshot(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()

	emptied := seedTenant(t, pool, "gh402-roster-"+uuid.NewString()[:8])
	siteID := gh402SeedSite(t, admin, emptied)
	gh402SeedSnapshot(t, admin, emptied, siteID, "completed")
	gh402SeedChunk(t, admin, emptied, "orphanblake3")

	repo := backup.NewRepo(pool)

	before, err := repo.ListTenantsForGC(ctx)
	if err != nil {
		t.Fatalf("ListTenantsForGC: %v", err)
	}
	if !containsTenant(before, emptied) {
		t.Fatal("the tenant is not on the roster while it still has a completed snapshot; " +
			"this test's premise is wrong")
	}

	// Delete the only site. The snapshot cascades; the chunk row does not.
	if derr := site.NewRepo(pool).Delete(ctx, emptied, siteID); derr != nil {
		t.Fatalf("delete site: %v", derr)
	}
	var snapCount, chunkCount int
	if qerr := admin.QueryRow(ctx,
		`SELECT (SELECT count(*) FROM backup_snapshots WHERE tenant_id = $1),
		        (SELECT count(*) FROM backup_chunks    WHERE tenant_id = $1)`,
		emptied).Scan(&snapCount, &chunkCount); qerr != nil {
		t.Fatalf("counts: %v", qerr)
	}
	if snapCount != 0 || chunkCount != 1 {
		t.Fatalf("after the delete: %d snapshots, %d chunks; want 0 and 1", snapCount, chunkCount)
	}

	after, err := repo.ListTenantsForGC(ctx)
	if err != nil {
		t.Fatalf("ListTenantsForGC: %v", err)
	}
	if !containsTenant(after, emptied) {
		t.Fatal("the emptied tenant fell off the GC roster; its chunk bytes would leak forever (GH #402)")
	}
}

func containsTenant(ids []uuid.UUID, want uuid.UUID) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// The roster must not return duplicates: a tenant with both a completed
// snapshot and chunk rows appears once, or every GC pass does its work twice.
func TestGH402_GCRoster_NoDuplicateTenants(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()

	tenant := seedTenant(t, pool, "gh402-dupes-"+uuid.NewString()[:8])
	siteID := gh402SeedSite(t, admin, tenant)
	gh402SeedSnapshot(t, admin, tenant, siteID, "completed")
	gh402SeedSnapshot(t, admin, tenant, siteID, "completed")
	gh402SeedChunk(t, admin, tenant, "a1")
	gh402SeedChunk(t, admin, tenant, "a2")

	ids, err := backup.NewRepo(pool).ListTenantsForGC(ctx)
	if err != nil {
		t.Fatalf("ListTenantsForGC: %v", err)
	}
	seen := 0
	for _, id := range ids {
		if id == tenant {
			seen++
		}
	}
	if seen != 1 {
		t.Errorf("tenant appears %d times on the GC roster, want exactly 1", seen)
	}
}

// A tenant with neither snapshots nor chunks is not visited at all.
func TestGH402_GCRoster_ExcludesTenantWithNoBackupState(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()

	bare := seedTenant(t, pool, "gh402-bare-"+uuid.NewString()[:8])
	gh402SeedSite(t, admin, bare)

	ids, err := backup.NewRepo(pool).ListTenantsForGC(ctx)
	if err != nil {
		t.Fatalf("ListTenantsForGC: %v", err)
	}
	if containsTenant(ids, bare) {
		t.Error("a tenant with no snapshots and no chunks is on the GC roster")
	}
}

// ---------------------------------------------------------------------------
// 7. THE BLOCKING FINDING OF THE SECOND REVIEW ROUND.
//
// The first version of m113 gave site_object_reclaim a tenant foreign key with
// ON DELETE CASCADE, on the reasoning that a tenant hard-purge happens only
// after the org purge worker has swept every tenant-scoped object root. That is
// true of ONE of the two tenant-delete lanes.
//
//	Lane B, the grace-window org.PurgeWorker, deletes all seven tenant object
//	roots (tenant/<id>/ among them) and only then calls admin_purge_tenant.
//
//	Lane A, admin_delete_empty_tenant, frees NOTHING. It is reached from
//	DELETE /orgs/{orgId} for an org with no sites and no other members, and
//	from the superadmin orphan cleanup. Its guard is "no memberships and no
//	sites", which was read as "owns no objects". An org whose sites were all
//	deleted first satisfies that guard and owns objects.
//
// So the cascade destroyed the reclaim record by exactly the operation that
// should have triggered it, and GH #402 came back one level up. This is the
// reviewer's probe, kept as a test: seed a tenant, a site, a completed snapshot
// and its manifest object; delete the site; confirm the record is there; run
// Lane A; confirm the record is STILL there and still does its job.
// ---------------------------------------------------------------------------

func TestGH402_ReclaimTaskSurvivesTenantHardDelete(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()

	tenant := seedTenant(t, pool, "gh402-lane-a-"+uuid.NewString()[:8])
	siteID := gh402SeedSite(t, admin, tenant)
	snap := gh402SeedSnapshot(t, admin, tenant, siteID, "completed")

	store := newGH402Store()
	manifest := gh402ManifestKey(tenant, siteID, snap)
	store.put(manifest)
	// A neighbouring tenant's manifest, and a client report PDF under the
	// PLURAL tenants/ root that is one character away. Both must survive a
	// sweep that runs with no tenant row left to check anything against.
	neighbour := seedTenant(t, pool, "gh402-lane-a-nb-"+uuid.NewString()[:8])
	neighbourSite := gh402SeedSite(t, admin, neighbour)
	neighbourKey := gh402ManifestKey(neighbour, neighbourSite, uuid.New())
	store.put(neighbourKey)
	pii := "tenants/" + tenant.String() + "/reports/2026-08/client.pdf"
	store.put(pii)

	// BEFORE: the site delete writes the record in its own transaction.
	if err := site.NewRepo(pool).Delete(ctx, tenant, siteID); err != nil {
		t.Fatalf("delete site: %v", err)
	}
	if got := gh402CountTasks(t, admin, tenant, siteID); got != 1 {
		t.Fatalf("expected 1 reclaim task after the site delete, got %d; this probe's premise is wrong", got)
	}
	if !store.has(manifest) {
		t.Fatal("the manifest object vanished before the tenant delete; this probe's premise is wrong")
	}

	// LANE A. Called as the app role through the EXECUTE grant, exactly how
	// admin.Repo.DeleteEmptyTenant and org's delete handler both reach it.
	if deleted := callDeleteEmptyTenant(t, pool, tenant); !deleted {
		t.Fatal("admin_delete_empty_tenant refused a tenant with no sites and no memberships; " +
			"this probe's premise is wrong")
	}
	if tenantExists(t, admin, tenant) {
		t.Fatal("the tenant row survived admin_delete_empty_tenant; this probe's premise is wrong")
	}

	// AFTER: the record must have OUTLIVED its tenant. With a tenant foreign
	// key this count is 0 and the manifest is orphaned with nothing left in the
	// database that names it.
	if got := gh402CountTasks(t, admin, tenant, siteID); got != 1 {
		t.Fatalf("the reclaim record did not survive admin_delete_empty_tenant (count %d, want 1). "+
			"Lane A frees no object storage at all, so destroying the record destroys the only "+
			"thing that names those objects, and GH #402 returns at tenant level", got)
	}

	// And it must still be actionable: a missing tenant row is a RECLAIM
	// signal, not a skip. An earlier worker read it as "already purged" and
	// would have left every one of these prefixes in the bucket forever.
	worker := backup.NewReclaimWorker(backup.NewReclaimStore(pool), store, nil)
	if err := worker.Work(ctx, nil); err != nil {
		t.Fatalf("reclaim sweep: %v", err)
	}
	if store.has(manifest) {
		t.Error("the manifest of a site whose TENANT was then hard-deleted was not reclaimed")
	}
	if !store.has(neighbourKey) {
		t.Error("the sweep reached into another tenant's prefix")
	}
	if !store.has(pii) {
		t.Error("the sweep reached the plural tenants/ root, which holds client report PDFs")
	}
	var completedAt *time.Time
	if err := admin.QueryRow(ctx,
		`SELECT completed_at FROM site_object_reclaim WHERE tenant_id = $1 AND site_id = $2`,
		tenant, siteID).Scan(&completedAt); err != nil {
		t.Fatalf("read task: %v", err)
	}
	if completedAt == nil {
		t.Error("the task did not close after a clean drain")
	}
}

// The same, driven through the SUPERADMIN ORPHAN CLEANUP rather than the SQL
// function directly: deleting the last account in an organisation deletes the
// organisation it orphans, by the same Lane A route, with the same consequence
// for anything cascading off the tenant row.
func TestGH402_ReclaimTaskSurvivesSuperadminOrphanCleanup(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()

	tenant := seedTenant(t, pool, "gh402-orphan-"+uuid.NewString()[:8])
	siteID := gh402SeedSite(t, admin, tenant)
	snap := gh402SeedSnapshot(t, admin, tenant, siteID, "completed")

	store := newGH402Store()
	manifest := gh402ManifestKey(tenant, siteID, snap)
	store.put(manifest)

	// The sole member, whose deletion orphans the org.
	sole := seedUserRow(t, admin, "gh402-sole-"+uuid.NewString()[:8]+"@example.com")
	seedMembershipRow(t, admin, sole, tenant)
	actor := seedUserRow(t, admin, "gh402-actor-"+uuid.NewString()[:8]+"@example.com")

	if err := site.NewRepo(pool).Delete(ctx, tenant, siteID); err != nil {
		t.Fatalf("delete site: %v", err)
	}
	if got := gh402CountTasks(t, admin, tenant, siteID); got != 1 {
		t.Fatalf("expected 1 reclaim task after the site delete, got %d", got)
	}

	// The real path: admin.Service.DeleteUser captures the orgs the delete
	// orphans and calls DeleteEmptyTenant on each one that owns no sites.
	res, err := adminpkg.NewService(adminpkg.NewRepo(pool), nil).DeleteUser(ctx, actor, sole)
	if err != nil {
		t.Fatalf("superadmin DeleteUser: %v", err)
	}
	if res.DeletedOrgs != 1 {
		t.Fatalf("the orphan cleanup deleted %d orgs, want 1; this probe's premise is wrong", res.DeletedOrgs)
	}
	if tenantExists(t, admin, tenant) {
		t.Fatal("the orphaned org survived the cleanup; this probe's premise is wrong")
	}

	if got := gh402CountTasks(t, admin, tenant, siteID); got != 1 {
		t.Fatalf("the reclaim record did not survive the superadmin orphan cleanup (count %d, want 1). "+
			"That path frees no object storage either, so the manifests would be orphaned forever", got)
	}

	worker := backup.NewReclaimWorker(backup.NewReclaimStore(pool), store, nil)
	if err := worker.Work(ctx, nil); err != nil {
		t.Fatalf("reclaim sweep: %v", err)
	}
	if store.has(manifest) {
		t.Error("the manifest of a site whose org was removed by the orphan cleanup was not reclaimed")
	}
}

// ---------------------------------------------------------------------------
// 8. Lane B is unchanged, and the leftover record is self-closing.
//
// The grace-window purge worker deletes the whole tenant/<id>/ root itself, so
// while a tenant is pending purge this sweep stands off rather than racing it
// across two advisory-lock namespaces. Dropping the tenant foreign key means a
// record can now outlive a Lane B purge too; that record must not become
// permanent litter, so the next sweep after the purge must close it.
// ---------------------------------------------------------------------------

func TestGH402_ReclaimStandsOffPendingPurgeThenSelfCloses(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()

	tenant := seedTenant(t, pool, "gh402-lane-b-"+uuid.NewString()[:8])
	siteID := gh402SeedSite(t, admin, tenant)
	snap := gh402SeedSnapshot(t, admin, tenant, siteID, "completed")

	store := newGH402Store()
	manifest := gh402ManifestKey(tenant, siteID, snap)
	store.put(manifest)

	if err := site.NewRepo(pool).Delete(ctx, tenant, siteID); err != nil {
		t.Fatalf("delete site: %v", err)
	}
	// The org is now soft-deleted and inside its grace window: Lane B owns the
	// whole root from here.
	if _, err := admin.Exec(ctx, `UPDATE tenants SET deleted_at = now() WHERE id = $1`, tenant); err != nil {
		t.Fatalf("soft-delete tenant: %v", err)
	}

	worker := backup.NewReclaimWorker(backup.NewReclaimStore(pool), store, nil)
	if err := worker.Work(ctx, nil); err != nil {
		t.Fatalf("sweep during grace window: %v", err)
	}
	if !store.has(manifest) {
		t.Error("the sweep deleted objects inside a tenant the purge worker owns; " +
			"that is the two-deleters race the stand-off exists to avoid")
	}
	var completedAt *time.Time
	if err := admin.QueryRow(ctx,
		`SELECT completed_at FROM site_object_reclaim WHERE tenant_id = $1 AND site_id = $2`,
		tenant, siteID).Scan(&completedAt); err != nil {
		t.Fatalf("read task: %v", err)
	}
	if completedAt != nil {
		t.Error("a skipped task was closed; nothing swept its objects")
	}

	// Lane B completes: it clears the root and hard-deletes the tenant. The
	// record survives that too, and the next sweep finds an empty prefix and
	// closes it instead of leaving it open forever.
	for k := range store.objects {
		if strings.HasPrefix(k, "tenant/"+tenant.String()+"/") {
			delete(store.objects, k)
		}
	}
	if _, err := admin.Exec(ctx, `DELETE FROM tenants WHERE id = $1`, tenant); err != nil {
		t.Fatalf("hard-delete tenant: %v", err)
	}
	if err := worker.Work(ctx, nil); err != nil {
		t.Fatalf("sweep after purge: %v", err)
	}
	if err := admin.QueryRow(ctx,
		`SELECT completed_at FROM site_object_reclaim WHERE tenant_id = $1 AND site_id = $2`,
		tenant, siteID).Scan(&completedAt); err != nil {
		t.Fatalf("read task: %v", err)
	}
	if completedAt == nil {
		t.Error("a record left behind by a Lane B purge never closes; " +
			"dropping the foreign key must not turn these rows into permanent litter")
	}
}

// ---------------------------------------------------------------------------
// 9. Re-deleting a site id that came back must not be silently dropped.
//
// The enqueue's conflict clause was ON CONFLICT DO NOTHING, which quietly
// falsified the invariant the whole table is built on. Against an already
// COMPLETED row for the same (tenant, site, kind) it discarded the new work, so
// a site id that returned (a restored dump, an operator recreating a site with
// a preserved id) and was deleted again left its second generation of manifests
// orphaned exactly as before.
// ---------------------------------------------------------------------------

func TestGH402_ReDeletedSiteReopensACompletedTask(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()

	tenant := seedTenant(t, pool, "gh402-reopen-"+uuid.NewString()[:8])
	siteID := gh402SeedSite(t, admin, tenant)
	firstSnap := gh402SeedSnapshot(t, admin, tenant, siteID, "completed")

	store := newGH402Store()
	store.put(gh402ManifestKey(tenant, siteID, firstSnap))

	if err := site.NewRepo(pool).Delete(ctx, tenant, siteID); err != nil {
		t.Fatalf("first delete: %v", err)
	}
	worker := backup.NewReclaimWorker(backup.NewReclaimStore(pool), store, nil)
	if err := worker.Work(ctx, nil); err != nil {
		t.Fatalf("first sweep: %v", err)
	}

	// The same site id comes back and takes another backup.
	if _, err := admin.Exec(ctx,
		`INSERT INTO sites (id, tenant_id, url, name) VALUES ($1, $2, $3, 'gh402-restored')`,
		siteID, tenant, "https://"+uuid.NewString()+".example.com"); err != nil {
		t.Fatalf("restore site row: %v", err)
	}
	secondSnap := gh402SeedSnapshot(t, admin, tenant, siteID, "completed")
	secondKey := gh402ManifestKey(tenant, siteID, secondSnap)
	store.put(secondKey)

	if err := site.NewRepo(pool).Delete(ctx, tenant, siteID); err != nil {
		t.Fatalf("second delete: %v", err)
	}

	var completedAt *time.Time
	var attempts int
	if err := admin.QueryRow(ctx,
		`SELECT completed_at, attempts FROM site_object_reclaim WHERE tenant_id = $1 AND site_id = $2`,
		tenant, siteID).Scan(&completedAt, &attempts); err != nil {
		t.Fatalf("read task: %v", err)
	}
	if completedAt != nil {
		t.Fatal("the second delete did not reopen the completed task. ON CONFLICT DO NOTHING " +
			"drops the new work on the floor, and this generation of manifests is orphaned " +
			"exactly as GH #402 described")
	}
	if attempts != 0 {
		t.Errorf("a reopened task carries %d attempts from its previous life, want 0", attempts)
	}

	if err := worker.Work(ctx, nil); err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if store.has(secondKey) {
		t.Error("the second generation of manifests was not reclaimed")
	}
}
