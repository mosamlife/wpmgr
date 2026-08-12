package tests

// gh408_tenant_object_reclaim_integration_test.go, GH #408 finding 1: the chunk
// inventory dies with the tenant.
//
// backup_chunks.tenant_id is ON DELETE CASCADE (m4), so admin_delete_empty_tenant
// (org delete Lane A, and the superadmin orphan cleanup) destroys the only
// database rows naming chunks/<tenant>/ while freeing zero object storage. m113
// fixed exactly this shape one table across, for a site's manifests, by writing a
// durable record in the delete's own transaction; backup_chunks was left alone.
//
// THE CARDINAL TEST IN THIS FILE is
// TestGH408_ChunkSharedWithALiveSiteInAnotherTenantSurvivesTheDrain. Chunks are
// content-addressed and deduplicated TENANT-WIDE, so the whole design turns on
// one claim: a chunk object is namespaced by the tenant that stored it
// (chunkS3Key, backup/model.go), and dedup never crosses a tenant (the oracle is
// WHERE tenant_id = $1 AND blake3 = ANY($2), backups.sql). Two organisations
// holding byte-identical WordPress core files therefore hold two DISTINCT
// objects, and draining one tenant's chunk root cannot reach the other's copy.
// That test is the executable form of that proof. It fails loudly the day
// somebody keys the drain on the hash instead of the tenant-namespaced prefix,
// or makes dedup global.
//
// NOT RUN BY CI (apps/api/tests is excluded from the fast lane by owner
// decision). Run with `make test-integration` from the repository root.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	adminpkg "github.com/mosamlife/wpmgr/apps/api/internal/admin"
	"github.com/mosamlife/wpmgr/apps/api/internal/backup"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

// gh408DrainTenantTasks runs the tenant-storage drain the way the control plane
// runs it: the real worker, the real Postgres store, the real application-role
// pool. Nothing here reaches into object storage on its own, and it must not:
// a test that deleted the objects itself would prove nothing about the worker.
func gh408DrainTenantTasks(t *testing.T, pool *db.Pool, store *gh402Store) {
	t.Helper()
	w := backup.NewTenantReclaimWorker(backup.NewTenantReclaimStore(pool), store, gh408Logger())
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("tenant drain: %v", err)
	}
}

// gh408Logger keeps the worker's Error lines out of the test output unless a
// test wants them; the assertions are on effects, not on logs.
func gh408Logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// gh408Hash returns a 64-character lowercase hex string usable as a BLAKE3-256
// digest. Content-addressing is the whole subject here, so the shape matters.
func gh408Hash(seed string) string {
	h := strings.ReplaceAll(uuid.NewSHA1(uuid.NameSpaceOID, []byte(seed)).String(), "-", "")
	return h + h // 32 hex + 32 hex = 64
}

func gh408ChunkKey(tenant uuid.UUID, blake3 string) string {
	return "chunks/" + tenant.String() + "/" + blake3
}

// gh408SeedManifestEntry gives a snapshot a manifest row that REFERENCES a chunk
// hash. This is the ground truth the retention collector consults, and the thing
// that makes tenant B's copy of a shared hash genuinely live.
func gh408SeedManifestEntry(t *testing.T, admin *db.Pool, tenant, snapshot uuid.UUID, hashes ...string) {
	t.Helper()
	if _, err := admin.Exec(context.Background(),
		`INSERT INTO backup_manifest_entries (snapshot_id, tenant_id, path, chunk_hashes, size)
		 VALUES ($1, $2, 'wp-includes/version.php', $3, 1)`,
		snapshot, tenant, hashes); err != nil {
		t.Fatalf("seed manifest entry: %v", err)
	}
}

func gh408CountChunks(t *testing.T, admin *db.Pool, tenant uuid.UUID) int {
	t.Helper()
	var n int
	if err := admin.QueryRow(context.Background(),
		`SELECT count(*) FROM backup_chunks WHERE tenant_id = $1`, tenant).Scan(&n); err != nil {
		t.Fatalf("count chunks: %v", err)
	}
	return n
}

// gh408CountTenantTasks counts the durable records of tenant-wide storage that
// must OUTLIVE the tenant row. Before the fix this query fails because the table
// does not exist, which is finding 1 stated as a schema fact.
func gh408CountTenantTasks(t *testing.T, admin *db.Pool, tenant uuid.UUID) int {
	t.Helper()
	var n int
	if err := admin.QueryRow(context.Background(),
		`SELECT count(*) FROM tenant_object_reclaim WHERE tenant_id = $1 AND completed_at IS NULL`,
		tenant).Scan(&n); err != nil {
		t.Fatalf("count tenant reclaim tasks: %v\n"+
			"GH #408 finding 1: nothing durable names this tenant's chunk objects after "+
			"admin_delete_empty_tenant, so they are orphaned permanently", err)
	}
	return n
}

// gh408NextAttemptIn reports how far in the future a tenant task is scheduled.
func gh408NextAttemptIn(t *testing.T, admin *db.Pool, tenant uuid.UUID) time.Duration {
	t.Helper()
	var at time.Time
	if err := admin.QueryRow(context.Background(),
		`SELECT next_attempt_at FROM tenant_object_reclaim WHERE tenant_id = $1`, tenant).Scan(&at); err != nil {
		t.Fatalf("read next_attempt_at: %v", err)
	}
	return time.Until(at)
}

// gh408MakeTenantTaskDue advances past the 24 hour floor. Tests must not be able
// to skip the floor by accident, so this is an explicit, named step.
func gh408MakeTenantTaskDue(t *testing.T, admin *db.Pool, tenant uuid.UUID) {
	t.Helper()
	if _, err := admin.Exec(context.Background(),
		`UPDATE tenant_object_reclaim SET next_attempt_at = now() - interval '1 minute'
		 WHERE tenant_id = $1 AND completed_at IS NULL`, tenant); err != nil {
		t.Fatalf("make the tenant task due: %v", err)
	}
}

// ---------------------------------------------------------------------------
// THE CARDINAL TEST
// ---------------------------------------------------------------------------

func TestGH408_ChunkSharedWithALiveSiteInAnotherTenantSurvivesTheDrain(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	authSvc, rec := odNewAuthSvc(pool)
	ctx := context.Background()

	// Two organisations whose sites run byte-identical WordPress core files, so
	// they store the SAME BLAKE3. Dedup is per tenant, so that is two objects.
	slugA := "gh408-a-" + uuid.NewString()[:8]
	tenantA := seedTenant(t, pool, slugA)
	tenantB := seedTenant(t, pool, "gh408-b-"+uuid.NewString()[:8])

	shared := gh408Hash("wordpress-core-version-php")
	ownA := gh408Hash("tenant-a-only-" + tenantA.String())
	gh402SeedChunk(t, admin, tenantA, shared)
	gh402SeedChunk(t, admin, tenantA, ownA)
	gh402SeedChunk(t, admin, tenantB, shared)

	store := newGH402Store()
	keySharedA := gh408ChunkKey(tenantA, shared)
	keyOwnA := gh408ChunkKey(tenantA, ownA)
	keySharedB := gh408ChunkKey(tenantB, shared)
	store.put(keySharedA)
	store.put(keyOwnA)
	store.put(keySharedB)

	// Tenant B is LIVE: a site, a completed snapshot, and a manifest row that
	// names the shared hash. Its copy must survive whatever happens to A.
	siteB := gh402SeedSite(t, admin, tenantB)
	snapB := gh402SeedSnapshot(t, admin, tenantB, siteB, "completed")
	gh408SeedManifestEntry(t, admin, tenantB, snapB, shared)
	manifestB := gh402ManifestKey(tenantB, siteB, snapB)
	store.put(manifestB)

	// Tenant A's last site is deleted first. That is the realistic sequence and
	// the one m113 was written for: the site delete records its own manifest
	// reclamation, and leaves the organisation empty.
	siteA := gh402SeedSite(t, admin, tenantA)
	snapA := gh402SeedSnapshot(t, admin, tenantA, siteA, "completed")
	manifestA := gh402ManifestKey(tenantA, siteA, snapA)
	store.put(manifestA)
	if err := site.NewRepo(pool).Delete(ctx, tenantA, siteA); err != nil {
		t.Fatalf("delete tenant A's last site: %v", err)
	}

	// The emptied organisation now goes through the REAL DELETE /orgs/{orgId}
	// handler, which is Lane A: a hard delete that sweeps no storage at all.
	owner := seedUserRow(t, admin, "gh408-owner-"+uuid.NewString()[:8]+"@example.com")
	odSeedMembership(t, admin, owner, tenantA, "owner")
	home := seedTenant(t, pool, "gh408-home-"+uuid.NewString()[:8])
	odSeedMembership(t, admin, owner, home, "owner")
	p := domain.Principal{Type: domain.PrincipalUser, UserID: owner, TenantID: home, Role: "owner", Scope: domain.ScopeOrg}
	engine := buildOrgEngine(t, pool, authSvc, rec, false, p)

	w := odDo(engine, http.MethodDelete, "/api/v1/orgs/"+tenantA.String(), `{"confirm_name":"`+slugA+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("delete org status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp struct {
		Lane string `json:"lane"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode delete response: %v", err)
	}
	if resp.Lane != "hard" {
		t.Fatalf("lane = %q, want %q: this test's premise is the Lane A hard delete", resp.Lane, "hard")
	}
	if tenantExists(t, admin, tenantA) {
		t.Fatal("tenant A should be hard-deleted by Lane A")
	}

	// The cascade destroyed the chunk inventory. This is the defect, stated as a
	// fact about the database rather than an argument.
	if n := gh408CountChunks(t, admin, tenantA); n != 0 {
		t.Fatalf("tenant A still has %d backup_chunks rows; this test's premise is the m4 cascade", n)
	}

	// So the reclaim record is now the ONLY thing in the world that names
	// chunks/<A>/, and it must exist.
	if n := gh408CountTenantTasks(t, admin, tenantA); n != 1 {
		t.Fatalf("expected exactly 1 open tenant_object_reclaim row for the hard-deleted tenant, got %d. "+
			"Without it the chunk objects are named by nothing, permanently", n)
	}

	// The 24 hour floor is live: a drain right now must delete NOTHING, so an
	// operator who deleted the wrong organisation has a day to restore a
	// pre-delete dump. Lane B effectively has seven days; Lane A had none.
	gh408DrainTenantTasks(t, pool, store)
	if !store.has(keySharedA) {
		t.Fatal("the drain deleted tenant A's storage immediately. The 24 hour floor on " +
			"next_attempt_at is what gives an operator time to undo a wrong delete")
	}
	if wait := gh408NextAttemptIn(t, admin, tenantA); wait < 23*time.Hour {
		t.Errorf("next_attempt_at is only %s away, want at least 23h (the m116 floor)", wait)
	}

	// Let the floor elapse, then drain for real.
	gh408MakeTenantTaskDue(t, admin, tenantA)
	gh408DrainTenantTasks(t, pool, store)

	// A's objects are gone: the leak is closed.
	for _, k := range []string{keySharedA, keyOwnA, manifestA} {
		if store.has(k) {
			t.Errorf("object %q survived the drain; the tenant's storage was not reclaimed", k)
		}
	}

	// B's objects are untouched. This is the assertion the whole design turns
	// on: the shared hash is the SAME 64 characters in both tenants.
	if !store.has(keySharedB) {
		t.Fatalf("CATASTROPHIC: %q was deleted. That object is the only copy of bytes a LIVE site in "+
			"another organisation still needs to restore. The drain must be keyed on the "+
			"tenant-namespaced prefix chunks/<tenant>/, never on the content hash", keySharedB)
	}
	if !store.has(manifestB) {
		t.Errorf("tenant B's manifest %q was deleted by a drain of tenant A", manifestB)
	}

	// And B's database side is untouched too, so its restore planner still
	// resolves the full chunk set for that snapshot.
	if n := gh408CountChunks(t, admin, tenantB); n != 1 {
		t.Errorf("tenant B has %d backup_chunks rows, want 1", n)
	}
	var resolvable int
	if err := admin.QueryRow(ctx,
		`SELECT count(*) FROM backup_chunks WHERE tenant_id = $1 AND blake3 = ANY($2::text[])`,
		tenantB, []string{shared}).Scan(&resolvable); err != nil {
		t.Fatalf("resolve tenant B's chunk set: %v", err)
	}
	if resolvable != 1 {
		t.Errorf("tenant B's snapshot resolves %d of its 1 chunk; its backups are no longer restorable", resolvable)
	}
	var entries int
	if err := admin.QueryRow(ctx,
		`SELECT count(*) FROM backup_manifest_entries WHERE tenant_id = $1 AND $2 = ANY(chunk_hashes)`,
		tenantB, shared).Scan(&entries); err != nil {
		t.Fatalf("read tenant B's manifest entries: %v", err)
	}
	if entries != 1 {
		t.Errorf("tenant B has %d manifest rows naming the shared hash, want 1", entries)
	}
}

// ---------------------------------------------------------------------------
// The record: written by the function, for BOTH Lane A callers, and only when
// the delete really happened.
// ---------------------------------------------------------------------------

// TestGH408_LaneA_OrgDelete_RecordsTenantReclaimWork is the DELETE /orgs caller.
func TestGH408_LaneA_OrgDelete_RecordsTenantReclaimWork(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	authSvc, rec := odNewAuthSvc(pool)

	slug := "gh408-lanea-" + uuid.NewString()[:8]
	tenant := seedTenant(t, pool, slug)
	gh402SeedChunk(t, admin, tenant, gh408Hash("lanea-"+tenant.String()))

	owner := seedUserRow(t, admin, "gh408-lanea-"+uuid.NewString()[:8]+"@example.com")
	odSeedMembership(t, admin, owner, tenant, "owner")
	home := seedTenant(t, pool, "gh408-laneahome-"+uuid.NewString()[:8])
	odSeedMembership(t, admin, owner, home, "owner")
	p := domain.Principal{Type: domain.PrincipalUser, UserID: owner, TenantID: home, Role: "owner", Scope: domain.ScopeOrg}
	engine := buildOrgEngine(t, pool, authSvc, rec, false, p)

	w := odDo(engine, http.MethodDelete, "/api/v1/orgs/"+tenant.String(), `{"confirm_name":"`+slug+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if tenantExists(t, admin, tenant) {
		t.Fatal("the tenants row survived a Lane A delete")
	}
	if n := gh408CountChunks(t, admin, tenant); n != 0 {
		t.Fatalf("backup_chunks did not cascade: %d rows left", n)
	}
	if n := gh408CountTenantTasks(t, admin, tenant); n != 1 {
		t.Fatalf("expected exactly 1 tenant reclaim record, got %d", n)
	}
}

// TestGH408_LaneA_SuperadminOrphanCleanup_RecordsTenantReclaimWork is the OTHER
// Lane A caller, and the reason the INSERT is in the function body rather than
// in one Go call site. It fails if anyone later "simplifies" this into
// internal/org.
func TestGH408_LaneA_SuperadminOrphanCleanup_RecordsTenantReclaimWork(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()

	tenant := seedTenant(t, pool, "gh408-orphan-"+uuid.NewString()[:8])
	gh402SeedChunk(t, admin, tenant, gh408Hash("orphan-"+tenant.String()))

	deleted, err := adminpkg.NewRepo(pool).DeleteEmptyTenant(ctx, tenant)
	if err != nil {
		t.Fatalf("superadmin orphan cleanup: %v", err)
	}
	if !deleted {
		t.Fatal("the empty tenant was not deleted; this test's premise is wrong")
	}
	if n := gh408CountTenantTasks(t, admin, tenant); n != 1 {
		t.Fatalf("expected exactly 1 tenant reclaim record from the superadmin cleanup, got %d. "+
			"The INSERT belongs in admin_delete_empty_tenant's body precisely so neither Lane A "+
			"caller can be forgotten", n)
	}
}

// TestGH408_NoTenantRecordWhenTheDeleteDidNotHappen proves the same-transaction
// invariant in the other direction: the record exists if and ONLY if the tenants
// row really went. A spurious record is a standing instruction to erase a live
// organisation's storage.
func TestGH408_NoTenantRecordWhenTheDeleteDidNotHappen(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()

	// A tenant with a SITE: not empty, so the function refuses.
	withSite := seedTenant(t, pool, "gh408-notempty-a-"+uuid.NewString()[:8])
	gh402SeedSite(t, admin, withSite)
	if deleted, err := adminpkg.NewRepo(pool).DeleteEmptyTenant(ctx, withSite); err != nil || deleted {
		t.Fatalf("DeleteEmptyTenant on a tenant with a site: deleted=%v err=%v, want false/nil", deleted, err)
	}
	if n := gh408CountTenantTasks(t, admin, withSite); n != 0 {
		t.Errorf("a refused delete wrote %d reclaim records. That row is a standing instruction to "+
			"erase a LIVE organisation's storage", n)
	}
	if !tenantExists(t, admin, withSite) {
		t.Error("the tenant was deleted despite owning a site")
	}

	// A tenant with a MEMBERSHIP: same.
	withMember := seedTenant(t, pool, "gh408-notempty-b-"+uuid.NewString()[:8])
	user := seedUserRow(t, admin, "gh408-nm-"+uuid.NewString()[:8]+"@example.com")
	odSeedMembership(t, admin, user, withMember, "owner")
	if deleted, err := adminpkg.NewRepo(pool).DeleteEmptyTenant(ctx, withMember); err != nil || deleted {
		t.Fatalf("DeleteEmptyTenant on a tenant with a member: deleted=%v err=%v, want false/nil", deleted, err)
	}
	if n := gh408CountTenantTasks(t, admin, withMember); n != 0 {
		t.Errorf("a refused delete wrote %d reclaim records", n)
	}
}

// TestGH408_TenantReclaimRecordSurvivesTheCascade is the m113 sibling: the row
// has no foreign key and is still standing after the delete it describes. It
// fails loudly if anyone "tidies up" the parentless row.
func TestGH408_TenantReclaimRecordSurvivesTheCascade(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()

	tenant := seedTenant(t, pool, "gh408-nofk-"+uuid.NewString()[:8])
	if deleted, err := adminpkg.NewRepo(pool).DeleteEmptyTenant(ctx, tenant); err != nil || !deleted {
		t.Fatalf("delete empty tenant: deleted=%v err=%v", deleted, err)
	}
	if n := gh408CountTenantTasks(t, admin, tenant); n != 1 {
		t.Fatalf("the reclaim record did not survive the tenant delete (%d rows). A foreign key "+
			"here destroys the record in the very operation it exists to survive", n)
	}

	// And the absence of the FK is asserted directly, so a future migration that
	// adds one fails here rather than in production.
	var fks int
	if err := admin.QueryRow(ctx,
		`SELECT count(*) FROM pg_constraint
		 WHERE conrelid = 'public.tenant_object_reclaim'::regclass AND contype = 'f'`).Scan(&fks); err != nil {
		t.Fatalf("count foreign keys: %v", err)
	}
	if fks != 0 {
		t.Errorf("tenant_object_reclaim has %d foreign key(s); it must have none", fks)
	}
}

// ---------------------------------------------------------------------------
// The drain's guards. Each asserted independently so a refactor cannot silently
// drop one.
// ---------------------------------------------------------------------------

// gh408SeedTenantTask records a due tenant task directly, for the guard tests.
func gh408SeedTenantTask(t *testing.T, admin *db.Pool, tenant uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := admin.QueryRow(context.Background(),
		`INSERT INTO tenant_object_reclaim (tenant_id, next_attempt_at)
		 VALUES ($1, now() - interval '1 minute') RETURNING id`, tenant).Scan(&id); err != nil {
		t.Fatalf("seed tenant task: %v", err)
	}
	return id
}

func gh408TaskState(t *testing.T, admin *db.Pool, id uuid.UUID) (attempts int32, completed bool, lastErr string) {
	t.Helper()
	var le *string
	var at *time.Time
	if err := admin.QueryRow(context.Background(),
		`SELECT attempts, completed_at, last_error FROM tenant_object_reclaim WHERE id = $1`,
		id).Scan(&attempts, &at, &le); err != nil {
		t.Fatalf("read task state: %v", err)
	}
	if le != nil {
		lastErr = *le
	}
	return attempts, at != nil, lastErr
}

// TestGH408_DrainRefusesWhenTheTenantRowIsBack is the restored-dump valve, the
// tenant-level analogue of m113's GUARD 3.
func TestGH408_DrainRefusesWhenTheTenantRowIsBack(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()

	tenant := seedTenant(t, pool, "gh408-back-"+uuid.NewString()[:8])
	task := gh408SeedTenantTask(t, admin, tenant)

	store := newGH402Store()
	key := gh408ChunkKey(tenant, gh408Hash("back-"+tenant.String()))
	store.put(key)

	// The tenants row EXISTS (it was never deleted here, which is exactly the
	// state a restored dump produces).
	gh408DrainTenantTasks(t, pool, store)

	if !store.has(key) {
		t.Fatal("the drain deleted a LIVE organisation's storage. This is the restored-dump and " +
			"staging-pointed-at-production case, and the control-plane store has no path prefix, " +
			"so every key sits at bucket root")
	}
	attempts, completed, lastErr := gh408TaskState(t, admin, task)
	if completed {
		t.Error("a refused task was closed; it must stay open so the objects are not forgotten")
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}
	if !strings.Contains(lastErr, "EXISTS") {
		t.Errorf("last_error does not say why it refused: %q", lastErr)
	}
	_ = ctx
}

// TestGH408_DrainRefusesWhenSitesOrChunkRowsStillNameTheTenant covers the other
// two guards, each on its own.
func TestGH408_DrainRefusesWhenSitesOrChunkRowsStillNameTheTenant(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()

	// A site row naming a tenant that no longer exists cannot be produced by the
	// cascade, so it is produced here directly: this guard is belt and braces
	// against a partial dump restore, and it must be real belt and braces.
	t.Run("sites", func(t *testing.T) {
		tenant := seedTenant(t, pool, "gh408-gsite-"+uuid.NewString()[:8])
		siteID := gh402SeedSite(t, admin, tenant)
		task := gh408SeedTenantTask(t, admin, tenant)
		// Drop the tenant row but keep the site, by detaching the FK for this one
		// row. DEFERRABLE is not available here, so the site is re-pointed after
		// the tenant goes: the effect on the guard is identical.
		if _, err := admin.Exec(ctx, `ALTER TABLE sites DROP CONSTRAINT sites_tenant_id_fkey`); err != nil {
			t.Skipf("cannot detach the sites FK in this container: %v", err)
		}
		defer func() {
			_, _ = admin.Exec(ctx, `DELETE FROM sites WHERE id = $1`, siteID)
			_, _ = admin.Exec(ctx, `ALTER TABLE sites ADD CONSTRAINT sites_tenant_id_fkey
			   FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE`)
		}()
		if _, err := admin.Exec(ctx, `DELETE FROM tenants WHERE id = $1`, tenant); err != nil {
			t.Fatalf("delete tenant: %v", err)
		}

		store := newGH402Store()
		key := gh408ChunkKey(tenant, gh408Hash("gsite-"+tenant.String()))
		store.put(key)
		gh408DrainTenantTasks(t, pool, store)

		if !store.has(key) {
			t.Error("the drain deleted storage while a sites row still named the tenant")
		}
		if _, completed, lastErr := gh408TaskState(t, admin, task); completed || !strings.Contains(lastErr, "sites") {
			t.Errorf("guard 3 did not refuse: completed=%v last_error=%q", completed, lastErr)
		}
	})

	t.Run("chunk rows", func(t *testing.T) {
		tenant := seedTenant(t, pool, "gh408-gchunk-"+uuid.NewString()[:8])
		hash := gh408Hash("gchunk-" + tenant.String())
		gh402SeedChunk(t, admin, tenant, hash)
		task := gh408SeedTenantTask(t, admin, tenant)
		if _, err := admin.Exec(ctx, `ALTER TABLE backup_chunks DROP CONSTRAINT backup_chunks_tenant_id_fkey`); err != nil {
			t.Skipf("cannot detach the backup_chunks FK in this container: %v", err)
		}
		defer func() {
			_, _ = admin.Exec(ctx, `DELETE FROM backup_chunks WHERE tenant_id = $1`, tenant)
			_, _ = admin.Exec(ctx, `ALTER TABLE backup_chunks ADD CONSTRAINT backup_chunks_tenant_id_fkey
			   FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE`)
		}()
		if _, err := admin.Exec(ctx, `DELETE FROM tenants WHERE id = $1`, tenant); err != nil {
			t.Fatalf("delete tenant: %v", err)
		}

		store := newGH402Store()
		key := gh408ChunkKey(tenant, hash)
		store.put(key)
		gh408DrainTenantTasks(t, pool, store)

		if !store.has(key) {
			t.Error("the drain deleted a chunk the inventory still names; the retention collector " +
				"owns those objects, not this drain")
		}
		if _, completed, lastErr := gh408TaskState(t, admin, task); completed || !strings.Contains(lastErr, "backup_chunks") {
			t.Errorf("guard 4 did not refuse: completed=%v last_error=%q", completed, lastErr)
		}
	})
}

// TestGH408_DrainResumesAfterAPartialFailure proves the resume property, and
// proves a partial drain is NEVER reported as done. Reporting a partial drain as
// complete is GH #402 recreated exactly.
func TestGH408_DrainResumesAfterAPartialFailure(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()

	tenant := seedTenant(t, pool, "gh408-resume-"+uuid.NewString()[:8])
	if deleted, err := adminpkg.NewRepo(pool).DeleteEmptyTenant(ctx, tenant); err != nil || !deleted {
		t.Fatalf("delete empty tenant: deleted=%v err=%v", deleted, err)
	}
	gh408MakeTenantTaskDue(t, admin, tenant)
	var task uuid.UUID
	if err := admin.QueryRow(ctx,
		`SELECT id FROM tenant_object_reclaim WHERE tenant_id = $1`, tenant).Scan(&task); err != nil {
		t.Fatalf("read task id: %v", err)
	}

	store := newGH402Store()
	var keys []string
	for i := 0; i < 5; i++ {
		k := gh408ChunkKey(tenant, gh408Hash(fmt.Sprintf("resume-%d-%s", i, tenant)))
		store.put(k)
		keys = append(keys, k)
	}
	sort.Strings(keys)
	store.failOn = keys[2]

	gh408DrainTenantTasks(t, pool, store)

	if _, completed, _ := gh408TaskState(t, admin, task); completed {
		t.Fatal("a drain that hit a storage fault was marked COMPLETE. A partially drained tenant " +
			"reported as done is GH #402 recreated exactly")
	}
	if !store.has(keys[2]) {
		t.Error("the key the store refused was deleted anyway")
	}
	remaining := 0
	for _, k := range keys {
		if store.has(k) {
			remaining++
		}
	}
	if remaining == len(keys) {
		t.Error("nothing was deleted at all; the drain must make progress up to the failing key")
	}

	// Next tick, healthy store: it re-lists the SHORTER set and finishes.
	store.failOn = ""
	gh408MakeTenantTaskDue(t, admin, tenant)
	gh408DrainTenantTasks(t, pool, store)

	for _, k := range keys {
		if store.has(k) {
			t.Errorf("object %q survived the resumed drain", k)
		}
	}
	if _, completed, _ := gh408TaskState(t, admin, task); !completed {
		t.Error("the resumed drain emptied every root but did not close the task")
	}
}

// TestGH408_StuckTenantTaskIsNeverDeletedAndIsReReportedEveryTick is the GH #256
// lesson asserted: prefer leaving it behind over guessing.
func TestGH408_StuckTenantTaskIsNeverDeletedAndIsReReportedEveryTick(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()

	tenant := seedTenant(t, pool, "gh408-stuck-"+uuid.NewString()[:8])
	task := gh408SeedTenantTask(t, admin, tenant)
	if _, err := admin.Exec(ctx,
		`UPDATE tenant_object_reclaim SET attempts = 99, last_error = 'storage unreachable' WHERE id = $1`,
		task); err != nil {
		t.Fatalf("push the task past the cap: %v", err)
	}

	var logs strings.Builder
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelError}))
	w := backup.NewTenantReclaimWorker(backup.NewTenantReclaimStore(pool), newGH402Store(), logger)
	for i := 0; i < 2; i++ {
		logs.Reset()
		if err := w.Work(ctx, nil); err != nil {
			t.Fatalf("sweep %d: %v", i+1, err)
		}
		var still int
		if err := admin.QueryRow(ctx,
			`SELECT count(*) FROM tenant_object_reclaim WHERE id = $1`, task).Scan(&still); err != nil {
			t.Fatalf("count: %v", err)
		}
		if still != 1 {
			t.Fatalf("sweep %d deleted the stuck row. It is the last record naming those objects", i+1)
		}
		if !strings.Contains(logs.String(), task.String()) {
			t.Errorf("sweep %d did not re-report the stuck task; a permanently stuck prefix must not "+
				"look like a healthy fleet", i+1)
		}
		if !strings.Contains(logs.String(), "wpmgr-cli reclaim retry") {
			t.Errorf("sweep %d reported a stuck task without naming the supported recovery command", i+1)
		}
	}
}

// ---------------------------------------------------------------------------
// Schema: RLS, and boot safety.
// ---------------------------------------------------------------------------

// TestGH408_TenantObjectReclaimHasFullRLS goes THROUGH the repository layer for
// the cross-tenant proof, not around it. m112's proofs built their own
// connections and were inert while every test passed.
func TestGH408_TenantObjectReclaimHasFullRLS(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()

	var enabled, forced bool
	if err := admin.QueryRow(ctx,
		`SELECT relrowsecurity, relforcerowsecurity FROM pg_class
		 WHERE oid = 'public.tenant_object_reclaim'::regclass`).Scan(&enabled, &forced); err != nil {
		t.Fatalf("read RLS flags: %v", err)
	}
	if !enabled || !forced {
		t.Fatalf("ENABLE=%v FORCE=%v, want both true. Without FORCE the table owner is not subject "+
			"to the policies at all", enabled, forced)
	}

	want := map[string]bool{
		"tenant_object_reclaim_tenant_isolation": false,
		"tenant_object_reclaim_agent":            false,
	}
	rows, err := admin.Query(ctx,
		`SELECT policyname, qual IS NOT NULL, with_check IS NOT NULL FROM pg_policies
		 WHERE schemaname = 'public' AND tablename = 'tenant_object_reclaim'`)
	if err != nil {
		t.Fatalf("read policies: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var hasQual, hasCheck bool
		if err := rows.Scan(&name, &hasQual, &hasCheck); err != nil {
			t.Fatalf("scan policy: %v", err)
		}
		if _, ok := want[name]; !ok {
			t.Errorf("unexpected policy %q on tenant_object_reclaim", name)
			continue
		}
		want[name] = true
		if !hasQual || !hasCheck {
			t.Errorf("policy %q has USING=%v WITH CHECK=%v; a missing WITH CHECK lets an INSERT or "+
				"UPDATE write a row the same policy would refuse to read", name, hasQual, hasCheck)
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("policy %q is missing", name)
		}
	}

	// Cross-tenant read, through the repository layer. A tenant-scoped caller
	// must see nothing here, because every row's tenant is gone by construction.
	tenantA := seedTenant(t, pool, "gh408-rls-a-"+uuid.NewString()[:8])
	tenantB := seedTenant(t, pool, "gh408-rls-b-"+uuid.NewString()[:8])
	gh408SeedTenantTask(t, admin, tenantA)
	var visible int
	if err := pool.InTenantTx(ctx, tenantB, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM tenant_object_reclaim`).Scan(&visible)
	}); err != nil {
		t.Fatalf("tenant-scoped read: %v", err)
	}
	if visible != 0 {
		t.Errorf("a caller scoped to tenant B can see %d of tenant A's reclaim rows", visible)
	}

	// And the agent lane, which is the one the drain and the CLI use, does see it.
	if err := pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM tenant_object_reclaim WHERE tenant_id = $1`, tenantA).Scan(&visible)
	}); err != nil {
		t.Fatalf("agent read: %v", err)
	}
	if visible != 1 {
		t.Errorf("the agent lane sees %d rows for tenant A, want 1; the drain cannot work", visible)
	}
}

// TestGH408_M116AppliesTwiceAndDoesNotBlockBoot. Migrations run inside main()
// before the server serves, so a failure here takes production down.
func TestGH408_M116AppliesTwiceAndDoesNotBlockBoot(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()

	const version = "20260818000000_m116_tenant_object_reclaim"
	for i := 0; i < 2; i++ {
		if _, err := admin.Exec(ctx, `DELETE FROM schema_migrations WHERE version = $1`, version); err != nil {
			t.Fatalf("unmark m116: %v", err)
		}
		if err := admin.Migrate(ctx); err != nil {
			t.Fatalf("re-apply m116 (round %d) failed. Migrations run inside main() and a failure "+
				"takes the control plane down: %v", i+1, err)
		}
	}

	// Re-application must not have duplicated the policies or the constraint.
	var policies, constraints int
	if err := admin.QueryRow(ctx,
		`SELECT count(*) FROM pg_policies WHERE tablename = 'tenant_object_reclaim'`).Scan(&policies); err != nil {
		t.Fatalf("count policies: %v", err)
	}
	if policies != 2 {
		t.Errorf("tenant_object_reclaim has %d policies after re-application, want 2", policies)
	}
	if err := admin.QueryRow(ctx,
		`SELECT count(*) FROM pg_constraint
		 WHERE conrelid = 'public.tenant_object_reclaim'::regclass AND contype = 'c'`).Scan(&constraints); err != nil {
		t.Fatalf("count constraints: %v", err)
	}
	if constraints != 1 {
		t.Errorf("tenant_object_reclaim has %d check constraints, want 1", constraints)
	}

	// And the function still records, after being replaced twice.
	tenant := seedTenant(t, pool, "gh408-reapply-"+uuid.NewString()[:8])
	if deleted, err := adminpkg.NewRepo(pool).DeleteEmptyTenant(ctx, tenant); err != nil || !deleted {
		t.Fatalf("delete empty tenant after re-application: deleted=%v err=%v", deleted, err)
	}
	if n := gh408CountTenantTasks(t, admin, tenant); n != 1 {
		t.Errorf("after re-application the function recorded %d rows, want 1", n)
	}
}

// TestGH408_BackfillFromSystemAuditLogFindsLaneAOrgDeletes covers the tenants
// already deleted before m116: recovered from the database, not a bucket scan.
//
// EVERY EVENT THIS TEST READS WAS WRITTEN BY THE REAL DELETE /orgs/{orgId}
// HANDLER. That is the whole point, because the recovery claim is not "the query
// finds rows shaped like this", it is "the query finds the rows PRODUCTION
// WROTE". A hand-seeded event proves the query matches the test's own idea of an
// org.deleted record and nothing about the action name, the metadata key, or the
// column the tenant id lands in. The earlier version of this test seeded all
// three events by hand and could not run at all: jsonb_build_object('lane', $3)
// gave Postgres nothing to infer the parameter type from, so it failed on its
// own fixture with 42P18 and this path was never exercised once.
//
// The three populations, all produced by driving the real endpoint:
//
//	gone    Lane A hard delete, m116 record then removed, which is exactly the
//	        state of a tenant deleted BEFORE m116 shipped: the audit event is the
//	        only surviving evidence anywhere in the database.
//	soft    Lane B soft delete whose grace-window purge later took the tenants
//	        row. Its row being GONE is what makes this a real test of the lane
//	        filter: with the row still present the query's NOT EXISTS would
//	        exclude it and the lane clause would never be reached.
//	alive   Lane A hard delete whose tenants row is BACK, the restored dump.
//
// It ends by draining, because a backfill that enqueues an id nothing acts on is
// not a recovery path.
func TestGH408_BackfillFromSystemAuditLogFindsLaneAOrgDeletes(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	authSvc, rec := odNewAuthSvc(pool)
	ctx := context.Background()

	// One owner drives every delete below through the real endpoint. Their active
	// org is a separate one, so the post-commit session reassignment never fires
	// and buildOrgEngine's nil SessionManager stays safe.
	owner := seedUserRow(t, admin, "gh408-bf-"+uuid.NewString()[:8]+"@example.com")
	home := seedTenant(t, pool, "gh408-bf-home-"+uuid.NewString()[:8])
	odSeedMembership(t, admin, owner, home, "owner")
	p := domain.Principal{Type: domain.PrincipalUser, UserID: owner, TenantID: home, Role: "owner", Scope: domain.ScopeOrg}
	engine := buildOrgEngine(t, pool, authSvc, rec, false, p)

	deleteOrg := func(orgID uuid.UUID, slug string) string {
		t.Helper()
		w := odDo(engine, http.MethodDelete, "/api/v1/orgs/"+orgID.String(), `{"confirm_name":"`+slug+`"}`)
		if w.Code != http.StatusOK {
			t.Fatalf("delete %s: status = %d, body = %s", slug, w.Code, w.Body.String())
		}
		var resp struct {
			Lane string `json:"lane"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode delete response for %s: %v", slug, err)
		}
		return resp.Lane
	}

	// gone: the population this command exists for.
	goneSlug := "gh408-bf-gone-" + uuid.NewString()[:8]
	gone := seedTenant(t, pool, goneSlug)
	odSeedMembership(t, admin, owner, gone, "owner")
	goneHash := gh408Hash("bf-gone-" + gone.String())
	gh402SeedChunk(t, admin, gone, goneHash)
	if lane := deleteOrg(gone, goneSlug); lane != "hard" {
		t.Fatalf("lane = %q for an empty org, want %q: this test's premise is the Lane A hard delete", lane, "hard")
	}
	if tenantExists(t, admin, gone) {
		t.Fatal("the tenants row survived a Lane A delete")
	}
	if n := gh408CountChunks(t, admin, gone); n != 0 {
		t.Fatalf("backup_chunks did not cascade: %d rows left, so nothing was orphaned to recover", n)
	}
	// Rewind to the pre-m116 world: the record the function now writes in the
	// delete's own transaction did not exist for these tenants, which is why they
	// need recovering at all.
	if _, err := admin.Exec(ctx, `DELETE FROM tenant_object_reclaim WHERE tenant_id = $1`, gone); err != nil {
		t.Fatalf("remove the m116 record to model a pre-m116 delete: %v", err)
	}
	if n := gh408CountTenantTasks(t, admin, gone); n != 0 {
		t.Fatalf("the pre-m116 state was not reached: %d tasks still open, so the backfill has nothing to prove", n)
	}

	// soft: Lane B, then its grace-window purge.
	softSlug := "gh408-bf-soft-" + uuid.NewString()[:8]
	soft := seedTenant(t, pool, softSlug)
	odSeedMembership(t, admin, owner, soft, "owner")
	gh402SeedSite(t, admin, soft) // not empty, so the handler takes Lane B
	if lane := deleteOrg(soft, softSlug); lane != "soft" {
		t.Fatalf("lane = %q for an org with a site, want %q", lane, "soft")
	}
	// Lane B sweeps all seven roots itself and then hard deletes the tenants row
	// at the end of the grace window. Only that final row delete is modelled here.
	if _, err := admin.Exec(ctx, `DELETE FROM tenants WHERE id = $1`, soft); err != nil {
		t.Fatalf("model the completed grace-window purge: %v", err)
	}

	// alive: hard deleted, then restored from a dump under the same id.
	aliveSlug := "gh408-bf-alive-" + uuid.NewString()[:8]
	alive := seedTenant(t, pool, aliveSlug)
	odSeedMembership(t, admin, owner, alive, "owner")
	if lane := deleteOrg(alive, aliveSlug); lane != "hard" {
		t.Fatalf("lane = %q for the restored-dump fixture, want %q", lane, "hard")
	}
	if _, err := admin.Exec(ctx, `DELETE FROM tenant_object_reclaim WHERE tenant_id = $1`, alive); err != nil {
		t.Fatalf("remove the m116 record for the restored-dump fixture: %v", err)
	}
	if _, err := admin.Exec(ctx,
		`INSERT INTO tenants (id, name, slug) VALUES ($1, $2, $2)`, alive, aliveSlug); err != nil {
		t.Fatalf("restore the tenants row from a dump: %v", err)
	}

	// The one shape the real handler cannot produce: a legacy org.deleted event
	// with no lane recorded. It must NOT be enqueued, because an event that does
	// not say which lane it took is not evidence of a Lane A delete, and this
	// command grants chunk-delete authority.
	//
	// The ::jsonb cast is not decoration. Postgres infers a parameter's type from
	// the context it appears in, and jsonb_build_object's value argument is
	// "any", so an uncast $n there fails with 42P18 before it ever reaches the
	// table. That is what left this test never run.
	noLane := uuid.New()
	if _, err := admin.Exec(ctx,
		`INSERT INTO system_audit_log (actor_type, action, tenant_id, tenant_name, metadata)
		 VALUES ('user', 'org.deleted', $1, $2, $3::jsonb)`,
		noLane, "gh408-bf-nolane", `{}`); err != nil {
		t.Fatalf("seed a legacy event with no lane: %v", err)
	}

	// The evidence, as production wrote it. If the handler ever renames the
	// action or stops recording the lane, this reads short and every assertion
	// below would otherwise pass vacuously.
	var hardEvents int
	if err := admin.QueryRow(ctx,
		`SELECT count(*) FROM system_audit_log
		 WHERE action = 'org.deleted' AND metadata ->> 'lane' = 'hard' AND tenant_id = ANY($1)`,
		[]uuid.UUID{gone, alive}).Scan(&hardEvents); err != nil {
		t.Fatalf("read the hard-delete audit trail: %v", err)
	}
	if hardEvents != 2 {
		t.Fatalf("the real DELETE /orgs handler left %d events matching action='org.deleted' and "+
			"metadata->>'lane'='hard' for the two orgs it hard deleted, want 2. The backfill query "+
			"reads exactly that shape, so it recovers nothing until this holds", hardEvents)
	}

	ids, err := backup.BackfillHardDeletedTenants(ctx, pool)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if len(ids) != 1 || ids[0] != gone {
		t.Fatalf("backfill enqueued %v, want exactly [%s]\n  hard-deleted: %s\n  soft then purged: %s\n"+
			"  hard-deleted then restored: %s\n  legacy event with no lane: %s",
			ids, gone, gone, soft, alive, noLane)
	}
	if n := gh408CountTenantTasks(t, admin, gone); n != 1 {
		t.Errorf("the hard-deleted tenant has %d tasks, want 1", n)
	}
	if n := gh408CountTenantTasks(t, admin, soft); n != 0 {
		t.Errorf("a SOFT deleted org was enqueued (%d tasks); Lane B owns those roots", n)
	}
	if n := gh408CountTenantTasks(t, admin, alive); n != 0 {
		t.Errorf("a tenant whose row still exists was enqueued (%d tasks)", n)
	}
	if n := gh408CountTenantTasks(t, admin, noLane); n != 0 {
		t.Errorf("an org.deleted event with no lane recorded was enqueued (%d tasks); the query must "+
			"not guess which lane an event took", n)
	}

	// A recovered id nothing acts on is not a recovery path, so the drain runs
	// for real from the state the backfill left behind.
	store := newGH402Store()
	goneKey := gh408ChunkKey(gone, goneHash)
	softKey := gh408ChunkKey(soft, gh408Hash("bf-soft-"+soft.String()))
	aliveKey := gh408ChunkKey(alive, gh408Hash("bf-alive-"+alive.String()))
	store.put(goneKey)
	store.put(softKey)
	store.put(aliveKey)

	gh408DrainTenantTasks(t, pool, store)

	if store.has(goneKey) {
		t.Errorf("the backfilled task did not drain %q. The backfill sets next_attempt_at to now() "+
			"rather than the 24 hour floor precisely so the operator's own instruction is actionable "+
			"on the next tick", goneKey)
	}
	if !store.has(aliveKey) {
		t.Errorf("CATASTROPHIC: %q was deleted. That tenants row exists again, so those objects belong "+
			"to a LIVE organisation", aliveKey)
	}
	if !store.has(softKey) {
		t.Errorf("%q was deleted by a drain that should never have been given a soft-deleted org", softKey)
	}

	// And it is re-runnable, which is the ON CONFLICT contract the query comment
	// claims: a second backfill reopens the closed task, and the drain re-lists
	// an already empty root and closes it again rather than reporting a fault.
	again, aerr := backup.BackfillHardDeletedTenants(ctx, pool)
	if aerr != nil {
		t.Fatalf("second backfill: %v", aerr)
	}
	if len(again) != 1 || again[0] != gone {
		t.Fatalf("second backfill enqueued %v, want exactly [%s]", again, gone)
	}
	gh408DrainTenantTasks(t, pool, store)
	if n := gh408CountTenantTasks(t, admin, gone); n != 0 {
		t.Errorf("the reopened task is still open after a drain of an already empty root (%d), so a "+
			"re-run of the recovery command leaves work on the books forever", n)
	}
}
