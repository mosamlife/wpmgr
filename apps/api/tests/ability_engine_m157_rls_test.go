// m157 proofs: the wpmgr/page-create seed exists with its write rules;
// stamp_wpmgr_ability_entry_hash sets a NULL wpmgr hash exactly once and
// refuses a second stamp, a vendor row and a malformed hash; and the sites
// content-editing columns are tenant-isolated and site-scoped.
//
// Every transaction goes through the production dispatch and asserts, from
// inside, that it is wpmgr_app with neither SUPERUSER nor BYPASSRLS. The
// statements proven are the generated sqlc methods.
package tests

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

func m157Entry(t *testing.T, tx pgx.Tx, name string) sqlc.AbilityCatalogue {
	t.Helper()
	rows, err := sqlc.New(tx).ListAbilityCatalogue(context.Background())
	if err != nil {
		t.Fatalf("list catalogue: %v", err)
	}
	for _, r := range rows {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no catalogue entry %s (read %d rows)", name, len(rows))
	return sqlc.AbilityCatalogue{}
}

// m157Stamp runs the stamp in a savepoint so a refusal does not abort the
// surrounding transaction, and returns the SQLSTATE ("" on success).
func m157Stamp(t *testing.T, tx pgx.Tx, id uuid.UUID, sha string) (sqlc.AbilityCatalogue, string) {
	t.Helper()
	ctx := context.Background()
	sp, err := tx.Begin(ctx)
	if err != nil {
		t.Fatalf("open savepoint: %v", err)
	}
	row, err := sqlc.New(sp).StampWpmgrAbilityEntryHash(ctx, sqlc.StampWpmgrAbilityEntryHashParams{EntryID: id, EntrySha256: sha})
	if err != nil {
		_ = sp.Rollback(ctx)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) {
			t.Fatalf("stamp returned a non-database error: %v", err)
		}
		return row, pgErr.Code
	}
	if err := sp.Commit(ctx); err != nil {
		t.Fatalf("release savepoint: %v", err)
	}
	return row, ""
}

// TestPageCreateSeedAsAppRole proves the m157 seed row exists, admitted and
// enabled, with the write rules and copy the brief fixes, and no hash yet.
func TestPageCreateSeedAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "m157-seed-"+uuid.NewString()[:8])
	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (m157 seed)")
		r := m157Entry(t, tx, "wpmgr/page-create")
		if r.Source != "wpmgr" || r.Class != "write" || r.Status != "admitted" || !r.Enabled ||
			r.ApprovalMode != "per_call" || r.Snapshot != "created_post_trash" || r.EffectCopy != "draft" ||
			r.OperatorPermission == nil || *r.OperatorPermission != "site.content.edit" ||
			r.MinAgentVersion == nil || *r.MinAgentVersion != "0.61.156" ||
			r.Title != "Create a draft page" ||
			r.Description != "Creates a new draft page or post from a text outline. Nothing is published. Undo moves the draft to the trash." ||
			r.EntrySha256 != nil || r.VersionMin != nil || r.OwnerDir != nil {
			t.Fatalf("wpmgr/page-create seed: %+v", r)
		}
		admitted, err := sqlc.New(tx).ListAdmittedAbilityCatalogue(ctx)
		if err != nil {
			return err
		}
		for _, a := range admitted {
			if a.EntryID == r.EntryID {
				return nil
			}
		}
		t.Fatalf("wpmgr/page-create is not listed as admitted")
		return nil
	}); err != nil {
		t.Fatalf("seed check: %v", err)
	}
}

// TestStampWpmgrAbilityEntryHashAsAppRole proves the stamp sets a NULL wpmgr
// hash once with a NULL-actor audit row, then refuses a second stamp (55000),
// a vendor row (42501) and a malformed hash (22023), changing nothing.
//
// Mutation: removing the entry_sha256 IS NULL guard from m157 turns the second
// stamp into an overwrite and fires "SECOND STAMP".
func TestStampWpmgrAbilityEntryHashAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "m157-stamp-"+uuid.NewString()[:8])

	admin := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, email, is_superadmin) VALUES ($1, $2, true)`,
		admin, "m157-"+admin.String()[:8]+"@example.test"); err != nil {
		t.Fatalf("seed superadmin: %v", err)
	}

	first := strings.Repeat("ab", 32)
	second := strings.Repeat("cd", 32)

	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (m157 stamp)")
		q := sqlc.New(tx)
		pc := m157Entry(t, tx, "wpmgr/page-create")
		if pc.EntrySha256 != nil {
			t.Fatalf("INDETERMINATE: page-create already carries a hash before the first stamp")
		}

		// Malformed first, against a NULL row, so the refusal is the shape check.
		for _, bad := range []string{"", strings.Repeat("AB", 32), strings.Repeat("a", 63), strings.Repeat("g", 64), first + "0"} {
			if _, code := m157Stamp(t, tx, pc.EntryID, bad); code != "22023" {
				t.Fatalf("a malformed hash %q was not refused with 22023 (got %q)", bad, code)
			}
		}

		row, code := m157Stamp(t, tx, pc.EntryID, first)
		if code != "" || row.EntrySha256 == nil || *row.EntrySha256 != first || row.EntryID != pc.EntryID {
			t.Fatalf("first stamp: code=%q row=%+v", code, row)
		}
		audit, err := q.ListAbilityCatalogueAudit(ctx, sqlc.ListAbilityCatalogueAuditParams{EntryID: pc.EntryID, RowLimit: 5})
		if err != nil {
			return err
		}
		if len(audit) != 1 || audit[0].ActorUserID.Valid || audit[0].Action != "update" ||
			audit[0].BeforeEntrySha256 != nil || audit[0].AfterEntrySha256 == nil || *audit[0].AfterEntrySha256 != first ||
			audit[0].BeforeRowSha256 == nil || *audit[0].BeforeRowSha256 == audit[0].AfterRowSha256 {
			t.Fatalf("audit after the first stamp: %+v", audit)
		}

		row, code = m157Stamp(t, tx, pc.EntryID, second)
		if got := m157Entry(t, tx, "wpmgr/page-create"); got.EntrySha256 == nil || *got.EntrySha256 != first {
			t.Fatalf("SECOND STAMP: the hash moved from %s to %v", first, got.EntrySha256)
		}
		if code != "55000" {
			t.Fatalf("SECOND STAMP: a second stamp was not refused with 55000 (code=%q row=%+v)", code, row)
		}
		if _, code := m157Stamp(t, tx, pc.EntryID, first); code != "55000" {
			t.Fatalf("SECOND STAMP: re-stamping the same hash was not refused with 55000 (code=%q)", code)
		}

		// A vendor row with a NULL hash: the superadmin path creates it, the
		// stamp must refuse it and leave it NULL.
		// m159 requires a non-wpmgr read to pin its schema and output shape.
		ownerDir := "some-builder"
		vmin := "1.0.0"
		schemaSha := "0000000000000000000000000000000000000000000000000000000000000002"
		vendor, err := q.AdminUpsertAbilityCatalogueEntry(ctx, sqlc.AdminUpsertAbilityCatalogueEntryParams{
			ActorUserID: admin, Name: "vendor-m157/read-thing", Source: "vendor", Class: "read",
			Status: "detect_only", Enabled: true, ApprovalMode: "none", PermissionMode: "principal",
			OwnerDir: &ownerDir, VersionMin: &vmin,
			DynamicEnumPaths: []string{}, Title: "Vendor read", Description: "A vendor read.", Snapshot: "none",
			ArgRender: []byte(`{}`), EffectCopy: "none", Limits: []byte(`{}`),
			NestedAllow: []string{}, GlobalOptionKeys: []string{}, Admission: []byte(`{}`),
			SchemaStructSha256: &schemaSha, OutputFields: []byte(`{"fields":{"id":"int"}}`),
		})
		if err != nil {
			return err
		}
		if _, code := m157Stamp(t, tx, vendor.EntryID, first); code != "42501" {
			t.Fatalf("VENDOR STAMP: a vendor row was not refused with 42501 (code=%q)", code)
		}
		if got := m157Entry(t, tx, "vendor-m157/read-thing"); got.EntrySha256 != nil {
			t.Fatalf("VENDOR STAMP: the vendor row's hash is now %s", *got.EntrySha256)
		}

		if _, code := m157Stamp(t, tx, uuid.New(), first); code != "P0002" {
			t.Fatalf("an unknown entry was not refused with P0002 (code=%q)", code)
		}

		// One audit row on page-create still: refusals wrote nothing.
		audit, err = q.ListAbilityCatalogueAudit(ctx, sqlc.ListAbilityCatalogueAuditParams{EntryID: pc.EntryID, RowLimit: 5})
		if err != nil {
			return err
		}
		if len(audit) != 1 {
			t.Fatalf("page-create carries %d audit rows after one stamp and refusals, want 1", len(audit))
		}

		// The CHECK that holds a NULL actor to the stamp shape exists.
		// wpmgr_app holds no INSERT on the audit, so it cannot be probed by a
		// write from this role.
		var def string
		if err := tx.QueryRow(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint
			WHERE conrelid = 'public.ability_catalogue_audit'::regclass
			  AND conname = 'ability_catalogue_audit_null_actor_is_system_check'`).Scan(&def); err != nil {
			t.Fatalf("INDETERMINATE: ability_catalogue_audit_null_actor_is_system_check missing: %v", err)
		}

		// EXECUTE is wpmgr_app's and not PUBLIC's.
		var pub bool
		if err := tx.QueryRow(ctx, `SELECT has_function_privilege('public',
			'public.stamp_wpmgr_ability_entry_hash(uuid, text)', 'EXECUTE')`).Scan(&pub); err != nil {
			return err
		}
		if pub {
			t.Fatalf("PUBLIC holds EXECUTE on stamp_wpmgr_ability_entry_hash")
		}
		return nil
	}); err != nil {
		t.Fatalf("stamp checks: %v", err)
	}
}

// TestSiteContentEditingIsolationAsAppRole proves the content-editing columns
// are written and read through the generated queries in the site's tenant,
// invisible and unwritable from another tenant and from a principal scoped to
// another site, and that enabled_at and the principal move together.
func TestSiteContentEditingIsolationAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	tenantA := seedTenant(t, pool, "m157-a-"+uuid.NewString()[:8])
	tenantB := seedTenant(t, pool, "m157-b-"+uuid.NewString()[:8])
	s1 := seedSite(t, pool, tenantA, "")
	s2 := seedSite(t, pool, tenantA, "")

	user := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, email) VALUES ($1, $2)`,
		user, "m157-"+user.String()[:8]+"@example.test"); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	// Positive control: the owning tenant reads NULL state, enables, reads it back.
	if err := pool.RunTenantTx(ctx, acprOrgPrincipal(tenantA), func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "RunTenantTx (m157 enable)")
		q := sqlc.New(tx)
		st, err := q.GetSiteContentEditing(ctx, sqlc.GetSiteContentEditingParams{SiteID: s1, TenantID: tenantA})
		if err != nil {
			return err
		}
		if st.ContentEditingEnabledAt.Valid || st.ContentEditingPrincipalUserID != nil {
			t.Fatalf("a new site reads content editing as enabled: %+v", st)
		}
		got, err := q.MarkSiteContentEditingEnabled(ctx, sqlc.MarkSiteContentEditingEnabledParams{
			PrincipalUserID: 7, EnabledBy: user, SiteID: s1, TenantID: tenantA,
		})
		if err != nil {
			return err
		}
		if !got.ContentEditingEnabledAt.Valid || got.ContentEditingPrincipalUserID == nil || *got.ContentEditingPrincipalUserID != 7 ||
			!got.ContentEditingEnabledBy.Valid || uuid.UUID(got.ContentEditingEnabledBy.Bytes) != user {
			t.Fatalf("enable returned %+v", got)
		}
		st, err = q.GetSiteContentEditing(ctx, sqlc.GetSiteContentEditingParams{SiteID: s1, TenantID: tenantA})
		if err != nil || !st.ContentEditingEnabledAt.Valid {
			t.Fatalf("read after enable: %+v (err=%v)", st, err)
		}

		for what, stmt := range map[string]string{
			"enabled without principal": `UPDATE sites SET content_editing_enabled_at = now(), content_editing_principal_user_id = NULL WHERE id = $1`,
			"principal without enabled": `UPDATE sites SET content_editing_enabled_at = NULL WHERE id = $1`,
			"principal zero":            `UPDATE sites SET content_editing_principal_user_id = 0 WHERE id = $1`,
		} {
			err := m133ExpectRefused(t, tx, stmt, s1)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "sites_content_editing_state_check" {
				t.Fatalf("%s: want 23514 at sites_content_editing_state_check, got %v", what, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("owning tenant: %v", err)
	}

	// Tenant B: reads nothing, writes nothing, even naming tenant A.
	if err := pool.InTenantTx(ctx, tenantB, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (m157 foreign tenant)")
		q := sqlc.New(tx)
		if _, err := q.GetSiteContentEditing(ctx, sqlc.GetSiteContentEditingParams{SiteID: s1, TenantID: tenantA}); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("TENANCY LEAK: tenant B read tenant A's content-editing state (err=%v)", err)
		}
		if _, err := q.MarkSiteContentEditingEnabled(ctx, sqlc.MarkSiteContentEditingEnabledParams{
			PrincipalUserID: 9, EnabledBy: user, SiteID: s2, TenantID: tenantA,
		}); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("TENANCY LEAK: tenant B enabled content editing on tenant A's site (err=%v)", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("foreign tenant: %v", err)
	}

	// A principal scoped to S1 cannot read or enable S2.
	if err := pool.RunTenantTx(ctx, acprSitePrincipal(tenantA, s1), func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "RunTenantTx (m157 single site S1)")
		q := sqlc.New(tx)
		if _, err := q.GetSiteContentEditing(ctx, sqlc.GetSiteContentEditingParams{SiteID: s1, TenantID: tenantA}); err != nil {
			t.Fatalf("the S1 principal cannot read S1 (positive control): %v", err)
		}
		if _, err := q.GetSiteContentEditing(ctx, sqlc.GetSiteContentEditingParams{SiteID: s2, TenantID: tenantA}); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("SITE-SCOPE LEAK: the S1 principal read S2's content-editing state (err=%v)", err)
		}
		if _, err := q.MarkSiteContentEditingEnabled(ctx, sqlc.MarkSiteContentEditingEnabledParams{
			PrincipalUserID: 9, EnabledBy: user, SiteID: s2, TenantID: tenantA,
		}); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("SITE-SCOPE LEAK: the S1 principal enabled content editing on S2 (err=%v)", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("site scope: %v", err)
	}

	// S2 still reads as not enabled from its own tenant.
	if err := pool.RunTenantTx(ctx, acprOrgPrincipal(tenantA), func(tx pgx.Tx) error {
		st, err := sqlc.New(tx).GetSiteContentEditing(ctx, sqlc.GetSiteContentEditingParams{SiteID: s2, TenantID: tenantA})
		if err != nil {
			return err
		}
		if st.ContentEditingEnabledAt.Valid {
			t.Fatalf("LEAK: S2 was enabled by a write that should have been refused: %+v", st)
		}
		return nil
	}); err != nil {
		t.Fatalf("final read: %v", err)
	}
}
