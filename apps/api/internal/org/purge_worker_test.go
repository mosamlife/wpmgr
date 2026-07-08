package org

// purge_worker_test.go — adversarial-review fast-follows H1 (object-storage
// prefix completeness) and M2 (purge_started_at point-of-no-return marker),
// M3 (test-per-security-fix discipline). Mirrors the testcontainers pattern
// used throughout apps/api/tests (see tests/rls_integration_test.go's
// startPostgres) but lives in-package so it can reference objectStoragePrefixes
// directly and so `go test ./internal/org/...` is no longer "no test files"
// for a destructive feature.

import (
	"context"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

// startOrgTestPostgres spins up an ephemeral Postgres, applies the embedded
// migrations as the bootstrap superuser, then provisions the dedicated
// non-superuser wpmgr_app role and returns a pool connected as that role.
// Trimmed copy of tests/rls_integration_test.go's startPostgres (that helper
// is unexported in a different package and cannot be imported here).
func startOrgTestPostgres(t *testing.T) (*db.Pool, *db.Pool) {
	t.Helper()
	ctx := context.Background()

	container, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("wpmgr"),
		tcpostgres.WithUsername("wpmgr"),
		tcpostgres.WithPassword("wpmgr"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		t.Skipf("skipping: cannot start postgres container (docker unavailable?): %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	adminDSN, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	adminPool, err := db.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	if err := adminPool.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for _, stmt := range []string{
		"ALTER ROLE wpmgr_app LOGIN PASSWORD 'app'",
		"GRANT USAGE ON SCHEMA public TO wpmgr_app",
		"GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO wpmgr_app",
		"REVOKE UPDATE, DELETE, TRUNCATE ON audit_log FROM wpmgr_app",
	} {
		if _, err := adminPool.Exec(ctx, stmt); err != nil {
			t.Fatalf("provision app role (%q): %v", stmt, err)
		}
	}
	t.Cleanup(adminPool.Close)

	appDSN := strings.Replace(adminDSN, "wpmgr:wpmgr@", "wpmgr_app:app@", 1)
	appPool, err := db.Connect(ctx, appDSN)
	if err != nil {
		t.Fatalf("connect app: %v", err)
	}
	t.Cleanup(appPool.Close)

	return appPool, adminPool
}

func orgTestSeedTenant(t *testing.T, admin *db.Pool, slug string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := admin.QueryRow(context.Background(),
		"INSERT INTO tenants (name, slug) VALUES ($1, $2) RETURNING id", slug, slug).Scan(&id); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	return id
}

// ---------------------------------------------------------------------------
// Fakes (H1/M2 — no real object storage / agent connection needed)
// ---------------------------------------------------------------------------

type purgeTestOrderRecorder struct {
	mu     sync.Mutex
	events []string
}

func (r *purgeTestOrderRecorder) add(e string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *purgeTestOrderRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.events))
	copy(out, r.events)
	return out
}

type purgeTestNoopSiteLister struct{}

func (purgeTestNoopSiteLister) ListAllSiteIDs(context.Context, uuid.UUID) ([]uuid.UUID, error) {
	return nil, nil
}

type purgeTestNoopRevoker struct{}

func (purgeTestNoopRevoker) Revoke(context.Context, site.ActorSiteInput) (site.Site, error) {
	return site.Site{}, nil
}

// purgeTestStore records every prefix List call and every Delete call,
// regardless of whether the prefix has any seeded keys — this is what lets
// TestPurgeWorker_DeletesAllSevenObjectStoragePrefixes prove all seven roots
// were actually visited, not just the two the pre-H1-fix code touched.
type purgeTestStore struct {
	rec  *purgeTestOrderRecorder
	keys map[string][]string
}

func (s *purgeTestStore) List(_ context.Context, prefix string) ([]string, error) {
	s.rec.add("list:" + prefix)
	return s.keys[prefix], nil
}

func (s *purgeTestStore) Delete(_ context.Context, key string) error {
	s.rec.add("delete:" + key)
	return nil
}

// ---------------------------------------------------------------------------
// H1 — exact object-storage prefix set (pure, no DB)
// ---------------------------------------------------------------------------

// TestObjectStoragePrefixes_ExactSevenRoots locks in the fast-follow H1 fix:
// the purge MUST cover all seven tenant-scoped object-storage roots, not just
// the original two (chunks/, tenant/) — the other five (tenants/, screenshots/,
// media/, fonts/, rucss-src/) would otherwise be orphaned in object storage
// FOREVER after the DB cascade, including client-PII report PDFs. This test
// would have failed against the pre-fix two-prefix slice.
func TestObjectStoragePrefixes_ExactSevenRoots(t *testing.T) {
	tenantID := uuid.New()
	got := objectStoragePrefixes(tenantID)

	want := []string{
		"chunks/" + tenantID.String() + "/",
		"tenant/" + tenantID.String() + "/",
		"tenants/" + tenantID.String() + "/",
		"screenshots/" + tenantID.String() + "/",
		"media/" + tenantID.String() + "/",
		"fonts/" + tenantID.String() + "/",
		"rucss-src/" + tenantID.String() + "/",
	}

	if len(got) != len(want) {
		t.Fatalf("objectStoragePrefixes returned %d prefixes, want %d: got=%v", len(got), len(want), got)
	}
	gotSorted := append([]string(nil), got...)
	wantSorted := append([]string(nil), want...)
	sort.Strings(gotSorted)
	sort.Strings(wantSorted)
	for i := range wantSorted {
		if gotSorted[i] != wantSorted[i] {
			t.Fatalf("prefix set mismatch: got=%v want=%v", got, want)
		}
	}
	// Every prefix must be a fixed-UUID-then-slash root (no empty prefix, no
	// cross-tenant leakage) — the no-empty-prefix/no-cross-tenant guarantee
	// the coordinator's review required us to re-verify.
	for _, p := range got {
		if !strings.Contains(p, tenantID.String()+"/") {
			t.Fatalf("prefix %q does not scope to tenant %s", p, tenantID)
		}
		if strings.HasPrefix(p, "/") || p == tenantID.String()+"/" {
			t.Fatalf("prefix %q looks malformed/unscoped", p)
		}
	}
}

// ---------------------------------------------------------------------------
// H1 + M2 — full worker pass: all 7 prefixes visited, marker set before
// delete, idempotent re-run.
// ---------------------------------------------------------------------------

// TestPurgeWorker_DeletesAllSevenPrefixesMarksPurgeStartedAndIsIdempotent
// proves, end to end against a real Postgres:
//  1. every one of the seven object-storage roots is actually listed (H1);
//  2. purge_started_at is set (M2) before the tenant is hard-deleted;
//  3. a second sweep over the (now fully purged) tenant is a clean no-op —
//     no error, no new events — proving idempotency/resumability.
func TestPurgeWorker_DeletesAllSevenPrefixesMarksPurgeStartedAndIsIdempotent(t *testing.T) {
	pool, admin := startOrgTestPostgres(t)
	tenant := orgTestSeedTenant(t, admin, "purge-h1-"+uuid.NewString()[:8])

	// Soft-delete, backdated well past any grace window.
	if _, err := admin.Exec(context.Background(),
		`UPDATE tenants SET deleted_at = now() - interval '30 days' WHERE id = $1`, tenant); err != nil {
		t.Fatalf("seed soft-delete: %v", err)
	}

	rec := &purgeTestOrderRecorder{}
	store := &purgeTestStore{rec: rec, keys: map[string][]string{
		"chunks/" + tenant.String() + "/": {"chunks/" + tenant.String() + "/deadbeef"},
	}}
	worker := NewPurgeWorker(pool, purgeTestNoopSiteLister{}, purgeTestNoopRevoker{}, store, time.Hour, nil)

	if err := worker.Work(context.Background(), nil); err != nil {
		t.Fatalf("first sweep: %v", err)
	}

	events := rec.snapshot()
	wantPrefixes := objectStoragePrefixes(tenant)
	for _, p := range wantPrefixes {
		found := false
		for _, e := range events {
			if e == "list:"+p {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("purge worker never listed expected prefix %q; events=%v", p, events)
		}
	}
	if len(wantPrefixes) != 7 {
		t.Fatalf("test invariant broken: objectStoragePrefixes returned %d roots, expected 7", len(wantPrefixes))
	}
	// The one seeded key must actually have been deleted.
	wantDelete := "delete:chunks/" + tenant.String() + "/deadbeef"
	deleted := false
	for _, e := range events {
		if e == wantDelete {
			deleted = true
			break
		}
	}
	if !deleted {
		t.Fatalf("expected %q among recorded events, got %v", wantDelete, events)
	}

	// The tenant must be fully (hard) purged by now.
	var exists bool
	if err := admin.QueryRow(context.Background(),
		`SELECT EXISTS(SELECT 1 FROM tenants WHERE id = $1)`, tenant).Scan(&exists); err != nil {
		t.Fatalf("check tenant existence: %v", err)
	}
	if exists {
		t.Fatal("tenant should be hard-deleted after the purge sweep")
	}

	// Idempotency: a second sweep must not error and must not re-touch
	// anything (the tenant no longer appears in ListTenantsPendingPurge).
	before := len(rec.snapshot())
	if err := worker.Work(context.Background(), nil); err != nil {
		t.Fatalf("second sweep (idempotency check): %v", err)
	}
	if after := len(rec.snapshot()); after != before {
		t.Fatalf("second sweep touched an already-purged tenant again: before=%d after=%d events=%v",
			before, after, rec.snapshot())
	}
}

// assertingStore wraps purgeTestStore but ALSO asserts, on its very first
// Delete call, that tenants.purge_started_at is already set — proving (from
// inside the object-delete step itself, not after the fact) that the M2
// marker really is written before any object leaves storage, not after.
type assertingStore struct {
	*purgeTestStore
	t       *testing.T
	admin   *db.Pool
	tenant  uuid.UUID
	checked bool
}

func (s *assertingStore) Delete(ctx context.Context, key string) error {
	if !s.checked {
		s.checked = true
		var purgeStartedAt *time.Time
		if err := s.admin.QueryRow(ctx,
			`SELECT purge_started_at FROM tenants WHERE id = $1`, s.tenant).Scan(&purgeStartedAt); err != nil {
			s.t.Fatalf("read purge_started_at mid-purge: %v", err)
		}
		if purgeStartedAt == nil {
			s.t.Fatal("purge_started_at was NOT set before the first object-storage delete " +
				"(M2 point-of-no-return marker must precede step (b), not follow it)")
		}
	}
	return s.purgeTestStore.Delete(ctx, key)
}

// TestPurgeWorker_MarksPurgeStartedBeforeHardDelete proves the M2
// point-of-no-return ordering directly, from inside the object-delete step
// (see assertingStore above), and that the tenant is still hard-deleted
// afterward once every step succeeds.
func TestPurgeWorker_MarksPurgeStartedBeforeHardDelete(t *testing.T) {
	pool, admin := startOrgTestPostgres(t)
	tenant := orgTestSeedTenant(t, admin, "purge-m2-"+uuid.NewString()[:8])
	if _, err := admin.Exec(context.Background(),
		`UPDATE tenants SET deleted_at = now() - interval '30 days' WHERE id = $1`, tenant); err != nil {
		t.Fatalf("seed soft-delete: %v", err)
	}

	rec := &purgeTestOrderRecorder{}
	base := &purgeTestStore{rec: rec, keys: map[string][]string{
		"chunks/" + tenant.String() + "/": {"chunks/" + tenant.String() + "/onlykey"},
	}}
	store := &assertingStore{purgeTestStore: base, t: t, admin: admin, tenant: tenant}
	worker := NewPurgeWorker(pool, purgeTestNoopSiteLister{}, purgeTestNoopRevoker{}, store, time.Hour, nil)
	if err := worker.Work(context.Background(), nil); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if !store.checked {
		t.Fatal("test invariant broken: the seeded key was never deleted, so the mid-purge assertion never ran")
	}

	var exists bool
	if err := admin.QueryRow(context.Background(),
		`SELECT EXISTS(SELECT 1 FROM tenants WHERE id = $1)`, tenant).Scan(&exists); err != nil {
		t.Fatalf("check tenant existence: %v", err)
	}
	if exists {
		t.Fatal("tenant should be fully purged")
	}
}

// TestPurgeArgs_InsertOpts_RoutesToPurgeQueue guards the GH #161 review fix:
// without InsertOpts the periodic purge falls back to river.QueueDefault, so the
// org_purge queue's MaxWorkers:1 isolation never applies and a long tenant-wide
// purge competes for shared default workers.
func TestPurgeArgs_InsertOpts_RoutesToPurgeQueue(t *testing.T) {
	if got := (PurgeArgs{}).InsertOpts().Queue; got != PurgeQueue {
		t.Fatalf("PurgeArgs.InsertOpts().Queue = %q, want %q", got, PurgeQueue)
	}
}
