// Package tests holds integration tests that exercise the real Postgres schema
// (migrations + RLS) via testcontainers-go. They require Docker; if Docker is
// unavailable the tests skip rather than fail.
package tests

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

// startPostgres spins up an ephemeral Postgres, applies the embedded
// migrations as the bootstrap superuser, then provisions a dedicated
// NON-superuser application role and returns a pool connected as that role.
//
// This mirrors the production requirement: Postgres superusers (and roles with
// BYPASSRLS) ignore RLS policies entirely, so the application MUST connect as a
// plain, non-superuser role for the sites_tenant_isolation policy to take
// effect. The default container user is a superuser, hence the extra role.
func startPostgres(t *testing.T) *db.Pool {
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

	// Apply migrations as the bootstrap superuser.
	adminPool, err := db.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	if err := adminPool.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// The auth migration already created the wpmgr_app role (NOLOGIN, no
	// password) and granted it table privileges. Here we just give it a login +
	// password so the test can connect AS that non-superuser role. (Grants for
	// any pre-auth-migration tables are reasserted to be safe.)
	for _, stmt := range []string{
		"ALTER ROLE wpmgr_app LOGIN PASSWORD 'app'",
		"GRANT USAGE ON SCHEMA public TO wpmgr_app",
		"GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO wpmgr_app",
		// audit_log is append-only: re-revoke mutation in case the blanket grant
		// above re-added it.
		"REVOKE UPDATE, DELETE, TRUNCATE ON audit_log FROM wpmgr_app",
	} {
		if _, err := adminPool.Exec(ctx, stmt); err != nil {
			t.Fatalf("provision app role (%q): %v", stmt, err)
		}
	}
	adminPool.Close()

	appDSN := strings.Replace(adminDSN, "wpmgr:wpmgr@", "wpmgr_app:app@", 1)
	pool, err := db.Connect(ctx, appDSN)
	if err != nil {
		t.Fatalf("connect app: %v", err)
	}
	t.Cleanup(pool.Close)

	adminDSNs[pool] = adminDSN
	t.Cleanup(func() { delete(adminDSNs, pool) })
	return pool
}

// adminDSNs maps an app pool to its container's superuser DSN, so tests that
// must act outside the app's RLS/privilege constraints (e.g. tampering with the
// append-only audit_log) can open a superuser connection.
var adminDSNs = map[*db.Pool]string{}

// connectAdmin opens a superuser pool for the most recently started container.
// It is only used to simulate out-of-band tampering in tests.
func connectAdmin(t *testing.T, app *db.Pool) *db.Pool {
	t.Helper()
	dsn, ok := adminDSNs[app]
	if !ok {
		t.Fatal("no admin DSN recorded for this pool")
	}
	pool, err := db.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	return pool
}

// seedTenant inserts a tenant row directly (tenants are not RLS-scoped).
func seedTenant(t *testing.T, pool *db.Pool, slug string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pool.QueryRow(context.Background(),
		"INSERT INTO tenants (name, slug) VALUES ($1, $2) RETURNING id", slug, slug).Scan(&id)
	if err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	return id
}

// TestRLSIsolation proves that the sites_tenant_isolation policy prevents a
// query running under one tenant's app.tenant_id from seeing another tenant's
// rows, even when the explicit WHERE filter is bypassed.
func TestRLSIsolation(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()

	tenantA := seedTenant(t, pool, "tenant-a")
	tenantB := seedTenant(t, pool, "tenant-b")

	repo := site.NewRepo(pool)

	siteA, err := repo.Create(ctx, site.CreateInput{TenantID: tenantA, URL: "https://a.example.com", Name: "A"})
	if err != nil {
		t.Fatalf("create site A: %v", err)
	}
	if _, err := repo.Create(ctx, site.CreateInput{TenantID: tenantB, URL: "https://b.example.com", Name: "B"}); err != nil {
		t.Fatalf("create site B: %v", err)
	}

	// Tenant A lists: must see only its own site.
	listA, err := repo.List(ctx, site.ListInput{TenantID: tenantA, Limit: 100})
	if err != nil {
		t.Fatalf("list A: %v", err)
	}
	if len(listA) != 1 || listA[0].TenantID != tenantA {
		t.Fatalf("tenant A leaked rows: %+v", listA)
	}

	// Tenant B cannot fetch tenant A's site by ID (RLS hides it -> not found).
	if _, err := repo.Get(ctx, tenantB, siteA.ID); err == nil {
		t.Fatalf("tenant B was able to read tenant A's site")
	}

	// Direct cross-tenant SELECT under tenant B's GUC returns zero rows even
	// though we query for tenant A's id with no tenant filter at all.
	err = pool.InTenantTx(ctx, tenantB, func(tx pgx.Tx) error {
		var count int
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM sites WHERE id = $1", siteA.ID).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			t.Fatalf("RLS failed: cross-tenant SELECT returned %d rows", count)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("cross-tenant select tx: %v", err)
	}
}
