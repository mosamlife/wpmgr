// site_cache_capability_integration_test.go — GH #243: the site-card
// "Page Cache" / "Object Cache" capability dots used to infer state from
// installed-plugin slugs that can never exist (both features ship as
// drop-ins, not plugins), leaving the dots permanently gray. This proves,
// against a real Postgres with the production non-superuser role, that
// site.Repo's Get/List now surface the REAL config state via a PK-keyed
// LEFT JOIN onto site_perf_config.cache_enabled / site_object_cache_config.
// enabled — covering the three states each field can be in (no config row,
// an enabled row, a disabled row) — and that the join cannot be tricked into
// leaking a mismatched tenant's config row (RLS + the ON-clause tenant_id
// match are both defense-in-depth here). Requires Docker; skips when
// unavailable (via startPostgres).
package tests

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

// seedPerfConfigRow inserts a minimal site_perf_config row (site_id PK; every
// other column has a schema DEFAULT) directly under the tenant's own
// InTenantTx, so the RLS WITH CHECK is exercised exactly as a real write
// would be.
func seedPerfConfigRow(t *testing.T, pool interface {
	InTenantTx(ctx context.Context, tenantID uuid.UUID, fn func(pgx.Tx) error) error
}, tenantID, siteID uuid.UUID, cacheEnabled bool) {
	t.Helper()
	ctx := context.Background()
	err := pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx,
			`INSERT INTO site_perf_config (site_id, tenant_id, cache_enabled) VALUES ($1, $2, $3)`,
			siteID, tenantID, cacheEnabled)
		return e
	})
	if err != nil {
		t.Fatalf("seed site_perf_config row: %v", err)
	}
}

// seedObjectCacheConfigRow is seedPerfConfigRow's site_object_cache_config
// counterpart.
func seedObjectCacheConfigRow(t *testing.T, pool interface {
	InTenantTx(ctx context.Context, tenantID uuid.UUID, fn func(pgx.Tx) error) error
}, tenantID, siteID uuid.UUID, enabled bool) {
	t.Helper()
	ctx := context.Background()
	err := pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx,
			`INSERT INTO site_object_cache_config (site_id, tenant_id, enabled) VALUES ($1, $2, $3)`,
			siteID, tenantID, enabled)
		return e
	})
	if err != nil {
		t.Fatalf("seed site_object_cache_config row: %v", err)
	}
}

// TestSiteCacheCapabilityFields_ListAndGet covers the three states each of
// page_cache_enabled / object_cache_enabled can be in: no config row at all
// (COALESCE default false), a config row with the feature explicitly
// disabled (false), and a config row with the feature enabled (true). Both
// site.Repo.List and site.Repo.Get are asserted since GH #243 required both
// the sites list AND the site detail to surface the real state.
func TestSiteCacheCapabilityFields_ListAndGet(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "cachecap-"+uuid.NewString()[:8])

	siteSvc := site.NewService(site.NewRepo(pool), domain.NewValidator(), domain.SystemClock{})

	mk := func(name string) site.Site {
		s, err := siteSvc.Create(ctx, site.CreateInput{TenantID: tenant, URL: "https://" + name + ".example.com", Name: name})
		if err != nil {
			t.Fatalf("create site %s: %v", name, err)
		}
		return s
	}

	sNoConfig := mk("no-config")
	sEnabled := mk("enabled")
	sDisabled := mk("disabled")

	// sNoConfig: no site_perf_config / site_object_cache_config row at all.
	// sEnabled: both features explicitly ON.
	seedPerfConfigRow(t, pool, tenant, sEnabled.ID, true)
	seedObjectCacheConfigRow(t, pool, tenant, sEnabled.ID, true)
	// sDisabled: config rows exist but both features explicitly OFF — this is
	// the case a bare "row exists" check would get wrong; COALESCE + the raw
	// boolean column must both resolve to false.
	seedPerfConfigRow(t, pool, tenant, sDisabled.ID, false)
	seedObjectCacheConfigRow(t, pool, tenant, sDisabled.ID, false)

	want := map[uuid.UUID]struct {
		page, object bool
	}{
		sNoConfig.ID: {false, false},
		sEnabled.ID:  {true, true},
		sDisabled.ID: {false, false},
	}

	t.Run("List", func(t *testing.T) {
		list, err := siteSvc.List(ctx, site.ListInput{TenantID: tenant, Limit: 50})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		byID := make(map[uuid.UUID]site.Site, len(list))
		for _, s := range list {
			byID[s.ID] = s
		}
		for id, w := range want {
			s, ok := byID[id]
			if !ok {
				t.Fatalf("site %s missing from List result", id)
			}
			if s.PageCacheEnabled != w.page {
				t.Errorf("site %s: List PageCacheEnabled = %v, want %v", s.Name, s.PageCacheEnabled, w.page)
			}
			if s.ObjectCacheEnabled != w.object {
				t.Errorf("site %s: List ObjectCacheEnabled = %v, want %v", s.Name, s.ObjectCacheEnabled, w.object)
			}
		}
	})

	t.Run("Get", func(t *testing.T) {
		for id, w := range want {
			s, err := siteSvc.Get(ctx, tenant, id)
			if err != nil {
				t.Fatalf("Get(%s): %v", id, err)
			}
			if s.PageCacheEnabled != w.page {
				t.Errorf("site %s: Get PageCacheEnabled = %v, want %v", s.Name, s.PageCacheEnabled, w.page)
			}
			if s.ObjectCacheEnabled != w.object {
				t.Errorf("site %s: Get ObjectCacheEnabled = %v, want %v", s.Name, s.ObjectCacheEnabled, w.object)
			}
		}
	})
}

// TestSiteCacheCapabilityFields_TenantIsolation proves a second tenant's
// config rows never leak into the join, including the adversarial case of a
// site_perf_config/site_object_cache_config row whose tenant_id does not
// match the owning site's tenant_id (simulating a data-integrity bug
// elsewhere — site_perf_config.site_id is only a plain FK to sites(id), not a
// composite FK enforcing tenant_id equality, so the DB schema alone does not
// rule this out). Both the RLS tenant_isolation policy on the joined tables
// AND the query's explicit `pc.tenant_id = s.tenant_id` / `oc.tenant_id =
// s.tenant_id` ON-clause match are defense-in-depth against exactly this.
func TestSiteCacheCapabilityFields_TenantIsolation(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()

	tenantA := seedTenant(t, pool, "cachecap-iso-a-"+uuid.NewString()[:8])
	tenantB := seedTenant(t, pool, "cachecap-iso-b-"+uuid.NewString()[:8])

	siteSvcA := site.NewService(site.NewRepo(pool), domain.NewValidator(), domain.SystemClock{})
	sA, err := siteSvcA.Create(ctx, site.CreateInput{TenantID: tenantA, URL: "https://iso-a.example.com", Name: "iso-a"})
	if err != nil {
		t.Fatalf("create site for tenant A: %v", err)
	}

	// Tenant B has its own, unrelated site with caching genuinely enabled —
	// proves normal cross-tenant separation (tenant A never sees tenant B's
	// site at all; already covered by sites.tenant_id in every query, but
	// asserted here as a baseline).
	siteSvcB := site.NewService(site.NewRepo(pool), domain.NewValidator(), domain.SystemClock{})
	sB, err := siteSvcB.Create(ctx, site.CreateInput{TenantID: tenantB, URL: "https://iso-b.example.com", Name: "iso-b"})
	if err != nil {
		t.Fatalf("create site for tenant B: %v", err)
	}
	seedPerfConfigRow(t, pool, tenantB, sB.ID, true)
	seedObjectCacheConfigRow(t, pool, tenantB, sB.ID, true)

	// Adversarial case: as the ADMIN (superuser, bypasses RLS), attach a
	// site_perf_config / site_object_cache_config row to tenant A's site sA
	// but stamp it with tenant B's tenant_id — a corrupted/mismatched row that
	// should never be produced by the application, but the schema does not
	// forbid it. The join must NOT surface tenant B's "enabled" state onto
	// tenant A's site.
	if _, err := admin.Exec(ctx,
		`INSERT INTO site_perf_config (site_id, tenant_id, cache_enabled) VALUES ($1, $2, true)`,
		sA.ID, tenantB); err != nil {
		t.Fatalf("admin seed mismatched site_perf_config row: %v", err)
	}
	if _, err := admin.Exec(ctx,
		`INSERT INTO site_object_cache_config (site_id, tenant_id, enabled) VALUES ($1, $2, true)`,
		sA.ID, tenantB); err != nil {
		t.Fatalf("admin seed mismatched site_object_cache_config row: %v", err)
	}

	// Querying sA under tenant A's own scope must NOT pick up tenant B's
	// mismatched config row — page/object cache must read false, not leak
	// tenant B's "true".
	got, err := siteSvcA.Get(ctx, tenantA, sA.ID)
	if err != nil {
		t.Fatalf("Get sA under tenant A: %v", err)
	}
	if got.PageCacheEnabled {
		t.Error("tenant A's site must NOT show page_cache_enabled=true from tenant B's mismatched config row (RLS/ON-clause leak)")
	}
	if got.ObjectCacheEnabled {
		t.Error("tenant A's site must NOT show object_cache_enabled=true from tenant B's mismatched config row (RLS/ON-clause leak)")
	}

	listA, err := siteSvcA.List(ctx, site.ListInput{TenantID: tenantA, Limit: 50})
	if err != nil {
		t.Fatalf("List for tenant A: %v", err)
	}
	if len(listA) != 1 {
		t.Fatalf("expected exactly 1 site for tenant A, got %d", len(listA))
	}
	if listA[0].PageCacheEnabled || listA[0].ObjectCacheEnabled {
		t.Errorf("tenant A's ListSites row leaked tenant B's config: page=%v object=%v",
			listA[0].PageCacheEnabled, listA[0].ObjectCacheEnabled)
	}

	// Sanity: tenant B's own, legitimately-owned site still reports its real
	// (enabled) state — the isolation fix must not be a blanket false.
	listB, err := siteSvcB.List(ctx, site.ListInput{TenantID: tenantB, Limit: 50})
	if err != nil {
		t.Fatalf("List for tenant B: %v", err)
	}
	if len(listB) != 1 {
		t.Fatalf("expected exactly 1 site for tenant B, got %d", len(listB))
	}
	if !listB[0].PageCacheEnabled || !listB[0].ObjectCacheEnabled {
		t.Errorf("tenant B's own site should report its real enabled state: page=%v object=%v",
			listB[0].PageCacheEnabled, listB[0].ObjectCacheEnabled)
	}
}
