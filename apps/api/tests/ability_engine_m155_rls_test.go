// m154 and m155 proofs: site_ability_inventory and its run record are
// invisible outside their tenant and their site scope; ability_catalogue is
// read-only for wpmgr_app, its one write path refuses a non-superadmin actor
// and audits a superadmin one; the three seed entries exist after migration;
// and `mcp:site` plus the two ability capabilities are storable while a
// value outside each vocabulary is refused at the named constraint.
//
// Every transaction goes through the production dispatch (RunTenantTx /
// InTenantTx / InAgentTx / InMCPClientRegisterTx) and asserts, from inside,
// that it is wpmgr_app with neither SUPERUSER nor BYPASSRLS. Statements that
// exist in db/query/ability_engine.sql are called through the generated sqlc
// methods, so the statement proven is the statement shipped.
package tests

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

// m155Refresh replaces one site's ability inventory the way the refresh job
// will: bulk upsert, stale delete and run record, one checked_at, one tenant
// transaction.
func m155Refresh(t *testing.T, pool *db.Pool, tenant, site uuid.UUID, names ...string) {
	t.Helper()
	ctx := context.Background()
	arg := sqlc.UpsertSiteAbilityInventoryParams{TenantID: tenant, SiteID: site, CheckedAt: time.Now().UTC()}
	for _, n := range names {
		arg.Names = append(arg.Names, n)
		arg.OwnerKinds = append(arg.OwnerKinds, "plugin")
		arg.OwnerDirs = append(arg.OwnerDirs, "some-plugin")
		arg.OwnerOks = append(arg.OwnerOks, "true")
		arg.OwnerVersions = append(arg.OwnerVersions, "1.2.3")
		arg.SchemaStructSha256s = append(arg.SchemaStructSha256s, "")
		arg.InputSchemas = append(arg.InputSchemas, `{"type":"object"}`)
		arg.OutputSchemas = append(arg.OutputSchemas, "")
		arg.Annotations = append(arg.Annotations, "")
		arg.SiteLabels = append(arg.SiteLabels, "Label")
		arg.SiteDescriptions = append(arg.SiteDescriptions, "")
	}
	if err := pool.RunTenantTx(ctx, acprOrgPrincipal(tenant), func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "RunTenantTx (m155 refresh)")
		q := sqlc.New(tx)
		got, err := q.UpsertSiteAbilityInventory(ctx, arg)
		if err != nil {
			return err
		}
		if got != int64(len(names)) {
			t.Fatalf("upsert wrote %d rows, want %d", got, len(names))
		}
		if _, err := q.DeleteStaleSiteAbilityInventory(ctx, sqlc.DeleteStaleSiteAbilityInventoryParams{
			TenantID: tenant, SiteID: site, CheckedAt: arg.CheckedAt,
		}); err != nil {
			return err
		}
		return q.UpsertSiteAbilityInventoryRun(ctx, sqlc.UpsertSiteAbilityInventoryRunParams{
			TenantID: tenant, SiteID: site, CheckedAt: arg.CheckedAt, SnapshotID: uuid.New(),
			ApiPresent: true, AbilitiesStored: int32(len(names)),
		})
	}); err != nil {
		t.Fatalf("refresh ability inventory for site %s: %v", site, err)
	}
}

func m155List(t *testing.T, tx pgx.Tx, tenant, site uuid.UUID, ns *string) []sqlc.SiteAbilityInventory {
	t.Helper()
	rows, err := sqlc.New(tx).ListSiteAbilityInventory(context.Background(), sqlc.ListSiteAbilityInventoryParams{
		TenantID: tenant, SiteID: site, AfterName: "", Namespace: ns, RowLimit: 50,
	})
	if err != nil {
		t.Fatalf("list ability inventory for site %s: %v", site, err)
	}
	return rows
}

func m155Count(t *testing.T, tx pgx.Tx, table string, site uuid.UUID) int {
	t.Helper()
	var n int
	if err := tx.QueryRow(context.Background(),
		`SELECT count(*) FROM `+pgx.Identifier{table}.Sanitize()+` WHERE site_id = $1`, site).Scan(&n); err != nil {
		t.Fatalf("count %s rows: %v", table, err)
	}
	return n
}

// TestAbilityInventoryIsolationAsAppRole proves the permissive tenant policy
// and the RESTRICTIVE site-scope policy in both directions on
// site_ability_inventory and site_ability_inventory_runs, the per-site
// replace, the paged list with its namespace filter, and that the agent
// context reads and writes nothing.
//
// Mutation, planted and watched: removing site_ability_inventory_site_scope
// from m155 fires "SITE-SCOPE LEAK".
func TestAbilityInventoryIsolationAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)

	tenantA := seedTenant(t, pool, "m155-a-"+uuid.NewString()[:8])
	tenantB := seedTenant(t, pool, "m155-b-"+uuid.NewString()[:8])
	s1 := seedSite(t, pool, tenantA, "")
	s2 := seedSite(t, pool, tenantA, "")

	m155Refresh(t, pool, tenantA, s1, "core/get-site-info", "vendor-x/read-page", "vendor-x/write-page")
	m155Refresh(t, pool, tenantA, s2, "vendor-y/list")

	// Positive control first, so an empty result below means a policy fired
	// and not that nothing was written.
	if err := pool.RunTenantTx(ctx, acprOrgPrincipal(tenantA), func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "RunTenantTx (org, positive control)")
		if n := m155Count(t, tx, "site_ability_inventory", s2); n != 1 {
			t.Fatalf("the org principal sees %d inventory rows of site %s, want 1", n, s2)
		}
		if rows := m155List(t, tx, tenantA, s1, nil); len(rows) != 3 || rows[0].Name != "core/get-site-info" {
			t.Fatalf("list of S1 returned %+v, want 3 rows in name order", rows)
		}
		ns := "vendor-x"
		if rows := m155List(t, tx, tenantA, s1, &ns); len(rows) != 2 {
			t.Fatalf("namespace filter vendor-x returned %d rows, want 2", len(rows))
		}
		page, err := sqlc.New(tx).ListSiteAbilityInventory(ctx, sqlc.ListSiteAbilityInventoryParams{
			TenantID: tenantA, SiteID: s1, AfterName: "core/get-site-info", RowLimit: 1,
		})
		if err != nil || len(page) != 1 || page[0].Name != "vendor-x/read-page" {
			t.Fatalf("keyset page after core/get-site-info: %+v (err=%v), want vendor-x/read-page", page, err)
		}
		return nil
	}); err != nil {
		t.Fatalf("org read: %v", err)
	}

	// Tenant B sees nothing of tenant A.
	if err := pool.InTenantTx(ctx, tenantB, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (foreign tenant)")
		if n := m155Count(t, tx, "site_ability_inventory", s1); n != 0 {
			t.Fatalf("TENANCY LEAK: tenant B sees %d inventory rows of tenant A's site %s", n, s1)
		}
		if rows := m155List(t, tx, tenantA, s1, nil); len(rows) != 0 {
			t.Fatalf("TENANCY LEAK: ListSiteAbilityInventory named tenant A and returned %d rows to tenant B", len(rows))
		}
		if _, err := sqlc.New(tx).GetSiteAbilityInventoryRun(ctx, sqlc.GetSiteAbilityInventoryRunParams{
			TenantID: tenantA, SiteID: s1}); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("TENANCY LEAK: tenant B read tenant A's run record (err=%v)", err)
		}
		err := m133ExpectRefused(t, tx,
			`INSERT INTO site_ability_inventory (tenant_id, site_id, name, owner_kind, checked_at)
			 VALUES ($1, $2, 'evil/ability', 'plugin', now())`, tenantA, s1)
		if err == nil {
			t.Fatalf("TENANCY LEAK: tenant B inserted an inventory row for tenant A")
		}
		return nil
	}); err != nil {
		t.Fatalf("foreign tenant read: %v", err)
	}

	// A principal scoped to S1 sees S1 and not S2, and cannot write S2.
	if err := pool.RunTenantTx(ctx, acprSitePrincipal(tenantA, s1), func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "RunTenantTx (single site S1)")
		if n := m155Count(t, tx, "site_ability_inventory", s2); n != 0 {
			t.Fatalf("SITE-SCOPE LEAK: a principal scoped to site %s sees %d ability rows of site %s; "+
				"site_ability_inventory_site_scope is missing or not RESTRICTIVE", s1, n, s2)
		}
		if rows := m155List(t, tx, tenantA, s2, nil); len(rows) != 0 {
			t.Fatalf("SITE-SCOPE LEAK: ListSiteAbilityInventory for %s returned %d rows to the S1 principal", s2, len(rows))
		}
		if n := m155Count(t, tx, "site_ability_inventory_runs", s2); n != 0 {
			t.Fatalf("SITE-SCOPE LEAK: the S1 principal sees %d run records of site %s", n, s2)
		}
		if n := m155Count(t, tx, "site_ability_inventory", s1); n != 3 {
			t.Fatalf("OVER-FIRING: the S1 principal sees %d rows of its own site, want 3", n)
		}
		err := m133ExpectRefused(t, tx,
			`INSERT INTO site_ability_inventory (tenant_id, site_id, name, owner_kind, checked_at)
			 VALUES ($1, $2, 'evil/ability', 'plugin', now())`, tenantA, s2)
		if err == nil {
			t.Fatalf("SITE-SCOPE LEAK: the S1-scoped principal inserted an ability row for site %s", s2)
		}
		return nil
	}); err != nil {
		t.Fatalf("site-scoped read: %v", err)
	}

	// Replace: a refresh that no longer reports two abilities drops them.
	m155Refresh(t, pool, tenantA, s1, "vendor-x/read-page")
	if err := pool.RunTenantTx(ctx, acprOrgPrincipal(tenantA), func(tx pgx.Tx) error {
		rows := m155List(t, tx, tenantA, s1, nil)
		if len(rows) != 1 || rows[0].Name != "vendor-x/read-page" || rows[0].Namespace == nil || *rows[0].Namespace != "vendor-x" {
			t.Fatalf("after replace the site holds %+v, want vendor-x/read-page only", rows)
		}
		run, err := sqlc.New(tx).GetSiteAbilityInventoryRun(ctx, sqlc.GetSiteAbilityInventoryRunParams{TenantID: tenantA, SiteID: s1})
		if err != nil || run.AbilitiesStored != 1 {
			t.Fatalf("run record after replace: %+v (err=%v), want 1 stored", run, err)
		}
		return nil
	}); err != nil {
		t.Fatalf("read after replace: %v", err)
	}

	// No cross-tenant policy: the agent context reads and deletes nothing.
	if err := pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InAgentTx (no cross-tenant access)")
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM site_ability_inventory`).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			t.Fatalf("CROSS-TENANT READ: an InAgentTx session reads %d ability rows", n)
		}
		tag, err := tx.Exec(ctx, `DELETE FROM site_ability_inventory WHERE site_id = $1`, s2)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 0 {
			t.Fatalf("the agent context deleted %d rows; no cross-tenant write may exist", tag.RowsAffected())
		}
		return nil
	}); err != nil {
		t.Fatalf("agent session: %v", err)
	}
}

// TestAbilityCatalogueReadOnlyForAppRole proves the seed entries exist, that
// wpmgr_app can read and cannot write ability_catalogue or its audit, that the
// definer refuses a non-superadmin actor, and that a superadmin write is
// audited with before and after entry hashes and flips the kill switch.
func TestAbilityCatalogueReadOnlyForAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "m155-cat-"+uuid.NewString()[:8])

	var siteFacts sqlc.AbilityCatalogue
	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (catalogue read)")
		rows, err := sqlc.New(tx).ListAdmittedAbilityCatalogue(ctx)
		if err != nil {
			return err
		}
		want := map[string]bool{"wpmgr/abilities-inventory": true, "wpmgr/site-facts": true, "wpmgr/content-read": true}
		for _, r := range rows {
			if want[r.Name] {
				if r.Source != "wpmgr" || r.Class != "read" || r.Status != "admitted" || !r.Enabled || r.ApprovalMode != "none" {
					t.Fatalf("seed entry %s: %+v, want wpmgr/read/admitted/enabled/none", r.Name, r)
				}
				if r.Name == "wpmgr/site-facts" {
					siteFacts = r
				}
				delete(want, r.Name)
			}
		}
		if len(want) != 0 {
			t.Fatalf("seed entries missing after migration: %v (read %d admitted rows)", want, len(rows))
		}

		for what, stmt := range map[string]string{
			"INSERT":   `INSERT INTO ability_catalogue (name, source, class, status, approval_mode, title, description) VALUES ('x/y', 'wpmgr', 'read', 'admitted', 'none', 'X', 'X')`,
			"UPDATE":   `UPDATE ability_catalogue SET enabled = false WHERE name = 'wpmgr/site-facts'`,
			"DELETE":   `DELETE FROM ability_catalogue WHERE name = 'wpmgr/site-facts'`,
			"TRUNCATE": `TRUNCATE ability_catalogue`,
			"AUDIT":    `INSERT INTO ability_catalogue_audit (entry_id, name, action, actor_user_id, after_row_sha256, after_enabled) VALUES (gen_random_uuid(), 'x/y', 'insert', gen_random_uuid(), repeat('a', 64), true)`,
		} {
			err := m133ExpectRefused(t, tx, stmt)
			var pgErr *pgconn.PgError
			if err == nil || !errors.As(err, &pgErr) || pgErr.Code != "42501" {
				t.Fatalf("WRITE LEAK: wpmgr_app %s on ability_catalogue was not refused with 42501: %v", what, err)
			}
			t.Logf("wpmgr_app %s refused: %s", what, pgErr.Code)
		}
		for _, priv := range []string{"INSERT", "UPDATE", "DELETE", "TRUNCATE"} {
			for _, table := range []string{"ability_catalogue", "ability_catalogue_audit"} {
				var has bool
				if err := tx.QueryRow(ctx, `SELECT has_table_privilege('wpmgr_app', $1, $2)`, table, priv).Scan(&has); err != nil {
					return err
				}
				if has {
					t.Fatalf("WRITE LEAK: wpmgr_app holds %s on %s", priv, table)
				}
			}
		}

		sp, err := tx.Begin(ctx)
		if err != nil {
			return err
		}
		_, err = sqlc.New(sp).AdminUpsertAbilityCatalogueEntry(ctx, m155SiteFactsUpdate(uuid.New(), siteFacts, false))
		_ = sp.Rollback(ctx)
		var pgErr *pgconn.PgError
		if err == nil || !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Fatalf("the definer accepted a non-superadmin actor: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("catalogue checks: %v", err)
	}

	admin := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, email, is_superadmin) VALUES ($1, $2, true)`,
		admin, "m155-"+admin.String()[:8]+"@example.test"); err != nil {
		t.Fatalf("seed superadmin: %v", err)
	}
	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (definer write)")
		q := sqlc.New(tx)
		row, err := q.AdminUpsertAbilityCatalogueEntry(ctx, m155SiteFactsUpdate(admin, siteFacts, false))
		if err != nil {
			return err
		}
		if row.Enabled || row.EntryID != siteFacts.EntryID || row.EntrySha256 == nil {
			t.Fatalf("definer returned %+v, want the same entry disabled with a stamped hash", row)
		}
		audit, err := q.ListAbilityCatalogueAudit(ctx, sqlc.ListAbilityCatalogueAuditParams{EntryID: siteFacts.EntryID, RowLimit: 5})
		if err != nil {
			return err
		}
		if len(audit) != 1 || !audit[0].ActorUserID.Valid || uuid.UUID(audit[0].ActorUserID.Bytes) != admin || audit[0].Action != "update" ||
			audit[0].BeforeRowSha256 == nil || *audit[0].BeforeRowSha256 == audit[0].AfterRowSha256 ||
			audit[0].BeforeEntrySha256 != nil || audit[0].AfterEntrySha256 == nil ||
			audit[0].BeforeEnabled == nil || !*audit[0].BeforeEnabled || audit[0].AfterEnabled {
			t.Fatalf("audit after one superadmin update: %+v", audit)
		}
		admitted, err := q.ListAdmittedAbilityCatalogue(ctx)
		if err != nil {
			return err
		}
		for _, r := range admitted {
			if r.EntryID == siteFacts.EntryID {
				t.Fatalf("a disabled entry is still listed as admitted: kill switch 1 does not hold")
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("superadmin write: %v", err)
	}
}

func m155SiteFactsUpdate(actor uuid.UUID, e sqlc.AbilityCatalogue, enabled bool) sqlc.AdminUpsertAbilityCatalogueEntryParams {
	hash := "0000000000000000000000000000000000000000000000000000000000000001"
	id := e.EntryID
	return sqlc.AdminUpsertAbilityCatalogueEntryParams{
		ActorUserID: actor, EntryID: pgtype.UUID{Bytes: id, Valid: true}, Name: e.Name, Source: e.Source, Class: e.Class,
		Status: e.Status, Enabled: enabled, ApprovalMode: e.ApprovalMode, PermissionMode: e.PermissionMode,
		DynamicEnumPaths: []string{}, Title: e.Title, Description: e.Description, Snapshot: e.Snapshot,
		ArgRender: []byte(`{}`), EffectCopy: e.EffectCopy, Limits: []byte(`{}`),
		NestedAllow: []string{}, GlobalOptionKeys: []string{}, Admission: []byte(`{}`), EntrySha256: &hash,
	}
}

// TestM154SiteScopeAndAbilityCapabilitiesAsAppRole proves the three widened
// constraints: `mcp:site` is stored on a grant and on a client registration,
// the two ability capabilities are stored on a grant, a bogus value is refused
// with 23514 at each named constraint, and all three are convalidated.
func TestM154SiteScopeAndAbilityCapabilitiesAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "m154-"+uuid.NewString()[:8])

	for _, s := range []struct{ table, constraint string }{
		{"mcp_grants", "mcp_grants_oauth_scopes_vocabulary_check"},
		{"mcp_grants", "mcp_grants_capabilities_vocabulary_check"},
		{"mcp_oauth_clients", "mcp_oauth_clients_registered_scopes_vocabulary_check"},
	} {
		var validated bool
		if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
			mcpAssertAndReportRole(t, tx, "InTenantTx (constraint probe)")
			return tx.QueryRow(ctx, `SELECT convalidated FROM pg_constraint
				WHERE conrelid = ('public.' || $1)::regclass AND contype = 'c' AND conname = $2`,
				s.table, s.constraint).Scan(&validated)
		}); err != nil {
			t.Fatalf("INDETERMINATE: %s carries no CHECK named %q (%v)", s.table, s.constraint, err)
		}
		if !validated {
			t.Fatalf("%s.%s is NOT VALID; m154 re-adds it validated", s.table, s.constraint)
		}
	}

	grantInsert := func(caps, scopes []string) error {
		return pool.RunTenantTx(ctx, acprOrgPrincipal(tenant), func(tx pgx.Tx) error {
			mcpAssertAndReportRole(t, tx, "RunTenantTx (grant)")
			_, err := tx.Exec(ctx, `INSERT INTO mcp_grants
				    (tenant_id, name, status, site_scope_mode, capabilities, oauth_scopes, expires_at)
				VALUES ($1, $2, 'active', 'all', $3::text[], $4::text[], $5)`,
				tenant, "m154 "+uuid.NewString()[:8], caps, scopes, time.Now().UTC().Add(90*24*time.Hour))
			return err
		})
	}
	clientInsert := func(scopes []string) error {
		return pool.InMCPClientRegisterTx(ctx, func(tx pgx.Tx) error {
			mcpAssertAndReportRole(t, tx, "InMCPClientRegisterTx (client scopes)")
			_, err := sqlc.New(tx).RegisterMCPOAuthClient(ctx, sqlc.RegisterMCPOAuthClientParams{
				ClientID:                "m154-" + uuid.NewString(),
				TokenEndpointAuthMethod: "none",
				RedirectUris:            []string{"https://client.example/cb"},
				RegisteredScopes:        scopes,
			})
			return err
		})
	}

	caps := []string{"mcp.sites.read", "mcp.ability.read", "mcp.ability.request"}
	if err := grantInsert(caps, []string{"mcp:read", "mcp:site"}); err != nil {
		t.Fatalf("a grant holding mcp:site and the ability capabilities was refused: %v", err)
	}
	if err := clientInsert([]string{"mcp:read", "mcp:cache", "mcp:site"}); err != nil {
		t.Fatalf("a client registering mcp:site was refused: %v", err)
	}
	t.Log("mcp:site, mcp.ability.read and mcp.ability.request stored")

	for _, c := range []struct {
		what, constraint string
		err              error
	}{
		{"grant with mcp:sites", "mcp_grants_oauth_scopes_vocabulary_check",
			grantInsert([]string{"mcp.sites.read"}, []string{"mcp:read", "mcp:sites"})},
		{"grant with mcp.ability.write", "mcp_grants_capabilities_vocabulary_check",
			grantInsert([]string{"mcp.ability.write"}, []string{"mcp:site"})},
		{"client with mcp:sites", "mcp_oauth_clients_registered_scopes_vocabulary_check",
			clientInsert([]string{"mcp:read", "mcp:sites"})},
	} {
		var pgErr *pgconn.PgError
		if !errors.As(c.err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != c.constraint {
			t.Fatalf("%s: want 23514 at %s, got %v", c.what, c.constraint, c.err)
		}
		t.Logf("%s: refused 23514 at %s", c.what, c.constraint)
	}
}
