package tests

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/migrations"
)

const ds01Migration = "20261013010000_m179_delete_lifecycle_privileges"

// ds01Postgres uses the embedded migrator and its grants without provisioning
// extra table privileges. The bootstrap connection only provisions login roles
// and database ownership; fixtures are written by the application role.
func ds01Postgres(t *testing.T, singleRole, before bool) (app, owner *db.Pool) {
	t.Helper()
	ctx := context.Background()
	skipIfDockerUnavailable(t, ctx, "DS-01 postgres")
	c, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("wpmgr"), tcpostgres.WithUsername("wpmgr"), tcpostgres.WithPassword("wpmgr"),
		testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2).WithStartupTimeout(time.Minute)))
	if c != nil {
		t.Cleanup(func() { _ = c.Terminate(ctx) })
	}
	if err != nil {
		setupFatalfOrSkipIfDaemonDied(t, ctx, err, "DS-01 container start")
	}
	dsn, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		setupFatalf(t, err, "DS-01 connection string")
	}
	bootstrap, err := db.Connect(ctx, dsn)
	if err != nil {
		setupFatalf(t, err, "DS-01 bootstrap connect")
	}
	t.Cleanup(bootstrap.Close)
	role, password := "wpmgr_owner", "owner"
	if singleRole {
		role, password = "wpmgr_app", "app"
	}
	for _, stmt := range []string{
		fmt.Sprintf("CREATE ROLE %s LOGIN PASSWORD '%s' NOSUPERUSER NOBYPASSRLS CREATEROLE", role, password),
		"ALTER DATABASE wpmgr OWNER TO " + role,
	} {
		if _, err := bootstrap.Exec(ctx, stmt); err != nil {
			setupFatalf(t, err, "DS-01 provision migrating role")
		}
	}
	bootstrap.Close()
	owner, err = db.Connect(ctx, strings.Replace(dsn, "wpmgr:wpmgr@", role+":"+password+"@", 1))
	if err != nil {
		setupFatalf(t, err, "DS-01 owner connect")
	}
	t.Cleanup(owner.Close)
	var super, bypass bool
	if err := owner.QueryRow(ctx, `SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&super, &bypass); err != nil {
		t.Fatal(err)
	}
	if super || bypass {
		t.Fatalf("migration role is superuser=%t bypassrls=%t", super, bypass)
	}
	if before {
		scopePrefillApply(t, owner, func(version string) bool { return version < ds01Migration })
	} else if err := owner.Migrate(ctx); err != nil {
		setupFatalf(t, err, "DS-01 embedded migrations")
	}
	if singleRole {
		return owner, owner
	}
	if _, err := owner.Exec(ctx, `ALTER ROLE wpmgr_app LOGIN PASSWORD 'app'`); err != nil {
		setupFatalf(t, err, "DS-01 app login")
	}
	app, err = db.Connect(ctx, strings.Replace(dsn, "wpmgr:wpmgr@", "wpmgr_app:app@", 1))
	if err != nil {
		setupFatalf(t, err, "DS-01 app connect")
	}
	t.Cleanup(app.Close)
	return app, owner
}

var ds01Tables = []struct {
	name, immutableColumn string
	siteRows              bool
}{
	{"audit_log", "hash", false},
	{"org_context_versions", "restrictions", false},
	{"site_context_versions", "restrictions", true},
	{"assistant_update_proposals", "expires_at", true},
	{"assistant_cache_purge_requests", "expires_at", true},
	{"site_content_inventory_runs", "", true},
	{"site_ability_inventory_runs", "", true},
	{"assistant_ability_requests", "expires_at", true},
	{"ability_tenant_disables", "", false},
}

func ds01Seed(t *testing.T, app *db.Pool) (tenant, siteID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	tenant = seedTenant(t, app, "ds01-"+uuid.NewString())
	if err := app.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "DS-01 seed")
		if err := tx.QueryRow(ctx, `INSERT INTO sites (tenant_id, url, name) VALUES ($1, $2, 'DS-01') RETURNING id`,
			tenant, "https://"+uuid.NewString()+".example.test").Scan(&siteID); err != nil {
			return err
		}
		for _, stmt := range []string{
			`INSERT INTO audit_log (tenant_id, actor_type, action, hash) VALUES ($1, 'system', 'test', 'seed')`,
			`INSERT INTO org_context_versions (tenant_id, version, author_type, provenance) VALUES ($1, 1, 'system', 'manual')`,
		} {
			if _, err := tx.Exec(ctx, stmt, tenant); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO site_context_versions (tenant_id, site_id, version, author_type, provenance)
			VALUES ($1, $2, 1, 'system', 'manual')`, tenant, siteID); err != nil {
			return err
		}
		q := sqlc.New(tx)
		if err := q.UpsertSiteContentInventoryRun(ctx, sqlc.UpsertSiteContentInventoryRunParams{
			TenantID: tenant, SiteID: siteID, CheckedAt: time.Now().UTC(), PagesStored: 1,
		}); err != nil {
			return err
		}
		return q.UpsertSiteAbilityInventoryRun(ctx, sqlc.UpsertSiteAbilityInventoryRunParams{
			TenantID: tenant, SiteID: siteID, CheckedAt: time.Now().UTC(), SnapshotID: uuid.New(), ApiPresent: true,
		})
	}); err != nil {
		t.Fatalf("seed tenant and site records: %v", err)
	}
	m133InsertPending(t, app, tenant, siteID, uuid.New(), "ds01")
	acprInsert(t, app, acprOrgPrincipal(tenant), acprParams(tenant, siteID, uuid.New(), "ds01"))
	aarInsert(t, app, acprOrgPrincipal(tenant), aarParams(tenant, siteID, uuid.New(), "ds01"))
	root := uuid.New()
	if _, err := app.Exec(ctx, `INSERT INTO users (id, email, is_superadmin) VALUES ($1, $2, true)`,
		root, root.String()+"@example.test"); err != nil {
		t.Fatal(err)
	}
	entry := m160VendorRead(t, ctx, app, root)
	if _, err := m160Record(ctx, app, tenant, entry.EntryID, siteID); err != nil {
		t.Fatalf("seed tenant ability disable: %v", err)
	}
	ds01Counts(t, app, tenant, false)
	return tenant, siteID
}

func ds01Counts(t *testing.T, app *db.Pool, tenant uuid.UUID, siteDeleted bool) {
	t.Helper()
	if err := app.InTenantTx(context.Background(), tenant, func(tx pgx.Tx) error {
		for _, table := range ds01Tables {
			var n int
			if err := tx.QueryRow(context.Background(), "SELECT count(*) FROM "+table.name+" WHERE tenant_id = $1", tenant).Scan(&n); err != nil {
				return err
			}
			want := 1
			if siteDeleted && table.siteRows {
				want = 0
			}
			if n != want {
				t.Fatalf("%s rows = %d, want %d", table.name, n, want)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func ds01Delete(app *db.Pool, tenant, siteID uuid.UUID) error {
	return app.InTenantTx(context.Background(), tenant, func(tx pgx.Tx) error {
		n, err := sqlc.New(tx).DeleteSite(context.Background(), sqlc.DeleteSiteParams{ID: siteID, TenantID: tenant})
		if err == nil && n != 1 {
			return fmt.Errorf("DeleteSite affected %d rows, want 1", n)
		}
		return err
	})
}

func ds01DirectMutations(t *testing.T, app *db.Pool, tenant uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	for _, agent := range []bool{false, true} {
		if err := app.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
			mcpAssertAndReportRole(t, tx, "DS-01 direct mutations")
			if agent {
				if _, err := tx.Exec(ctx, `SELECT set_config('app.agent', 'on', true)`); err != nil {
					return err
				}
			}
			for _, table := range ds01Tables {
				statements := []string{"DELETE FROM " + table.name + " WHERE tenant_id = $1"}
				if table.immutableColumn != "" {
					statements = append(statements, "UPDATE "+table.name+" SET "+table.immutableColumn+" = "+table.immutableColumn+" WHERE tenant_id = $1")
				}
				for _, stmt := range statements {
					sp, err := tx.Begin(ctx)
					if err != nil {
						return err
					}
					tag, execErr := sp.Exec(ctx, stmt, tenant)
					if err := sp.Rollback(ctx); err != nil {
						return err
					}
					var pgErr *pgconn.PgError
					if execErr != nil && (!errors.As(execErr, &pgErr) || pgErr.Code != "42501") {
						t.Fatalf("%s: unexpected refusal: %v", stmt, execErr)
					}
					if execErr == nil && tag.RowsAffected() != 0 {
						t.Fatalf("direct mutation allowed (agent=%t): %s: %d rows", agent, stmt, tag.RowsAffected())
					}
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	ds01Counts(t, app, tenant, false)
	if err := app.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		for _, mutable := range []struct{ table, column string }{
			{"assistant_update_proposals", "note"},
			{"assistant_cache_purge_requests", "site_reported_text"},
			{"assistant_ability_requests", "site_reported_text"},
			{"site_content_inventory_runs", "checked_at"},
			{"site_ability_inventory_runs", "checked_at"},
			{"ability_tenant_disables", "reenabled_at"},
		} {
			tag, err := tx.Exec(ctx, "UPDATE "+mutable.table+" SET "+mutable.column+" = "+mutable.column+" WHERE tenant_id = $1", tenant)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				t.Fatalf("permitted update on %s affected %d rows, want 1", mutable.table, tag.RowsAffected())
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("permitted workflow and inventory updates: %v", err)
	}
}

func TestDS01DeleteLifecycle(t *testing.T) {
	for _, singleRole := range []bool{true, false} {
		name := "two_role"
		if singleRole {
			name = "single_role"
		}
		t.Run(name, func(t *testing.T) {
			app, owner := ds01Postgres(t, singleRole, false)
			ds01Ownership(t, app, singleRole)
			ds01ReferentialPrivileges(t, app)
			body, err := migrations.FS.ReadFile(ds01Migration + ".sql")
			if err != nil {
				t.Fatal(err)
			}
			// Reapplying the migration must preserve its complete contract.
			if err := pgx.BeginFunc(context.Background(), owner.Pool, func(tx pgx.Tx) error {
				_, err := tx.Exec(context.Background(), string(body))
				return err
			}); err != nil {
				t.Fatalf("reapply migration: %v", err)
			}
			tenant, siteID := ds01Seed(t, app)
			other, _ := ds01Seed(t, app)
			ds01DirectMutations(t, app, tenant)
			deleted, err := sqlc.New(app.Pool).AdminDeleteEmptyTenant(context.Background(), other)
			if err != nil || deleted {
				t.Fatalf("empty-tenant guard with a site: deleted=%t err=%v", deleted, err)
			}
			ds01MembershipGuard(t, app)
			if err := ds01Delete(app, tenant, siteID); err != nil {
				t.Fatalf("DeleteSite: %v", err)
			}
			ds01Counts(t, app, tenant, true)
			ds01Counts(t, app, other, false)
			purgeTenant, _ := ds01Seed(t, app)
			if err := app.InTenantTx(context.Background(), other, func(tx pgx.Tx) error {
				purged, err := sqlc.New(tx).AdminPurgeTenant(context.Background(), purgeTenant)
				if err != nil {
					return err
				}
				if !purged {
					t.Fatal("AdminPurgeTenant did not delete its target")
				}
				var tenantSetting string
				if err := tx.QueryRow(context.Background(), `SELECT current_setting('app.tenant_id')`).Scan(&tenantSetting); err != nil {
					return err
				}
				if tenantSetting != other.String() {
					t.Fatalf("AdminPurgeTenant left tenant setting %q, want %q", tenantSetting, other)
				}
				return nil
			}); err != nil {
				t.Fatalf("AdminPurgeTenant: %v", err)
			}
			ds01AssertPurged(t, app, purgeTenant)
			deleted, err = sqlc.New(app.Pool).AdminDeleteEmptyTenant(context.Background(), tenant)
			if err != nil || !deleted {
				t.Fatalf("AdminDeleteEmptyTenant: deleted=%t err=%v", deleted, err)
			}
			ds01AssertPurged(t, app, tenant)
			var reclaim bool
			if err := app.InAgentTx(context.Background(), func(tx pgx.Tx) error {
				return tx.QueryRow(context.Background(), `SELECT EXISTS (
					SELECT 1 FROM tenant_object_reclaim WHERE tenant_id = $1)`, tenant).Scan(&reclaim)
			}); err != nil || !reclaim {
				t.Fatalf("empty-tenant reclamation record: exists=%t err=%v", reclaim, err)
			}
			ds01Counts(t, app, other, false)
		})
	}
}

// Walk only CASCADE edges recursively: SET NULL updates its child without
// deleting that child's descendants. Catalog checks cover every reached table,
// including tables without a fixture in the append-only mutation proofs.
func ds01ReferentialPrivileges(t *testing.T, app *db.Pool) {
	t.Helper()
	rows, err := app.Query(context.Background(), `WITH RECURSIVE deleting(relid) AS (
		SELECT unnest(ARRAY['public.sites'::regclass::oid, 'public.tenants'::regclass::oid])
		UNION
		SELECT fk.conrelid FROM pg_constraint fk JOIN deleting d ON d.relid = fk.confrelid
		WHERE fk.contype = 'f' AND fk.confdeltype = 'c'
	), actions AS (
		SELECT fk.conrelid, fk.confrelid, fk.confdeltype
		FROM pg_constraint fk JOIN deleting d ON d.relid = fk.confrelid
		WHERE fk.contype = 'f' AND fk.confdeltype IN ('c', 'n')
	)
	SELECT c.relname, parent.relname, a.confdeltype::text,
		pg_get_userbyid(c.relowner), c.relrowsecurity, c.relforcerowsecurity,
		has_table_privilege(c.relowner, c.oid, CASE a.confdeltype WHEN 'c' THEN 'DELETE' ELSE 'UPDATE' END),
		coalesce((SELECT string_agg(p.polname || ':' || p.polcmd::text || ':' ||
			CASE WHEN p.polpermissive THEN 'permissive' ELSE 'restrictive' END, ', ' ORDER BY p.polname)
			FROM pg_policy p WHERE p.polrelid = c.oid), ''),
		coalesce((SELECT string_agg(pg_get_triggerdef(tr.oid), '; ' ORDER BY tr.tgname)
			FROM pg_trigger tr WHERE tr.tgrelid = c.oid AND NOT tr.tgisinternal), '')
	FROM actions a JOIN pg_class c ON c.oid = a.conrelid
	JOIN pg_class parent ON parent.oid = a.confrelid
	ORDER BY c.relname, parent.relname, a.confdeltype`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := false
	for rows.Next() {
		seen = true
		var table, parent, action, owner, policies, triggers string
		var enabled, forced, privilege bool
		if err := rows.Scan(&table, &parent, &action, &owner, &enabled, &forced, &privilege, &policies, &triggers); err != nil {
			t.Fatal(err)
		}
		t.Logf("FK %s -> %s action=%s owner=%s RLS=%t FORCE=%t policies=[%s] triggers=[%s]",
			parent, table, action, owner, enabled, forced, policies, triggers)
		if !privilege {
			t.Errorf("owner %s lacks referential-action privilege on %s (action=%s)", owner, table, action)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !seen {
		t.Fatal("no site or tenant foreign-key actions found")
	}
}

func ds01Ownership(t *testing.T, app *db.Pool, singleRole bool) {
	t.Helper()
	expectedOwner := "wpmgr_owner"
	if singleRole {
		expectedOwner = "wpmgr_app"
	}
	for _, table := range ds01Tables {
		var owner string
		var enabled, forced, ownerDelete bool
		if err := app.QueryRow(context.Background(), `SELECT pg_get_userbyid(relowner), relrowsecurity,
			relforcerowsecurity, has_table_privilege(relowner, oid, 'DELETE')
			FROM pg_class WHERE oid = $1::regclass`, "public."+table.name).
			Scan(&owner, &enabled, &forced, &ownerDelete); err != nil {
			t.Fatal(err)
		}
		if owner != expectedOwner || !enabled || !forced || !ownerDelete {
			t.Fatalf("%s owner=%s RLS=%t FORCE=%t owner DELETE=%t", table.name, owner, enabled, forced, ownerDelete)
		}
	}
}

func ds01MembershipGuard(t *testing.T, app *db.Pool) {
	t.Helper()
	ctx := context.Background()
	tenant := seedTenant(t, app, "ds01-member-"+uuid.NewString())
	user := uuid.New()
	if _, err := app.Exec(ctx, `INSERT INTO users (id, email) VALUES ($1, $2)`, user, user.String()+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if err := app.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO memberships (tenant_id, user_id, role) VALUES ($1, $2, 'owner')`, tenant, user)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	deleted, err := sqlc.New(app.Pool).AdminDeleteEmptyTenant(ctx, tenant)
	if err != nil || deleted {
		t.Fatalf("empty-tenant guard with a membership: deleted=%t err=%v", deleted, err)
	}
	if err := app.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM memberships WHERE tenant_id = $1)`, tenant).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			t.Fatal("empty-tenant guard removed a membership")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func ds01AssertPurged(t *testing.T, app *db.Pool, tenant uuid.UUID) {
	t.Helper()
	if err := app.InTenantTx(context.Background(), tenant, func(tx pgx.Tx) error {
		for _, table := range append([]string{"tenants", "sites"}, ds01TableNames()...) {
			column := "tenant_id"
			if table == "tenants" {
				column = "id"
			}
			var n int
			if err := tx.QueryRow(context.Background(), "SELECT count(*) FROM "+table+" WHERE "+column+" = $1", tenant).Scan(&n); err != nil {
				return err
			}
			if n != 0 {
				t.Fatalf("%s retained %d rows after purge", table, n)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func ds01TableNames() []string {
	var names []string
	for _, table := range ds01Tables {
		names = append(names, table.name)
	}
	return names
}

func TestDS01DeleteLifecycleUpgrade(t *testing.T) {
	app, owner := ds01Postgres(t, true, true)
	tenant, siteID := ds01Seed(t, app)
	for name, run := range map[string]func() error{
		"DeleteSite": func() error { return ds01Delete(app, tenant, siteID) },
		"AdminPurgeTenant": func() error {
			_, err := sqlc.New(app.Pool).AdminPurgeTenant(context.Background(), tenant)
			return err
		},
	} {
		err := run()
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" || !strings.Contains(pgErr.Message, "permission denied for table") {
			t.Fatalf("before migration %s: want table permission denial, got %v", name, err)
		}
		t.Logf("before migration %s: %v", name, err)
	}
	ds01Counts(t, app, tenant, false)
	if err := owner.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	ds01DirectMutations(t, app, tenant)
	if err := ds01Delete(app, tenant, siteID); err != nil {
		t.Fatalf("after migration DeleteSite: %v", err)
	}
	ds01Counts(t, app, tenant, true)
	purged, err := sqlc.New(app.Pool).AdminPurgeTenant(context.Background(), tenant)
	if err != nil || !purged {
		t.Fatalf("after migration AdminPurgeTenant: purged=%t err=%v", purged, err)
	}
	ds01AssertPurged(t, app, tenant)
}
