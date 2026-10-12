// m157 proofs: the wpmgr/page-create seed exists with its write rules;
// stamp_wpmgr_ability_entry_hash sets a NULL wpmgr hash exactly once and
// refuses a second stamp, a vendor row and a malformed hash; the release
// carrying m177 and m178 re-arms the stamp for exactly the two rows it
// changes, once; and the sites content-editing columns are tenant-isolated
// and site-scoped.
//
// Every transaction goes through the production dispatch and asserts, from
// inside, that it is wpmgr_app with neither SUPERUSER nor BYPASSRLS. The
// statements proven are the generated sqlc methods.
package tests

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mosamlife/wpmgr/apps/api/internal/abilities"
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
// enabled, with the write rules the brief fixes, the description m177 leaves
// it with, and no hash yet (nothing has run the boot stamp).
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
			r.Description != m177Description ||
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

// TestStampWpmgrAbilityEntryHashAsAppRole proves the entry-hash stamp path,
// each part on a database of its own: m157's guards, then the release that
// carries m177 and m178.
func TestStampWpmgrAbilityEntryHashAsAppRole(t *testing.T) {
	t.Run("m157_guards", m157StampGuards)
	t.Run("m177_m178_restamp", m177M178Restamp)
}

// m177M178Names are the two rows the release carrying m177 and m178 changes,
// sorted.
var m177M178Names = []string{"wpmgr/page-create", "wpmgr/page-edit"}

// m177M178Catalogue reads every catalogue row as wpmgr_app through the
// generated query, keyed by name.
func m177M178Catalogue(t *testing.T, w *e2World) map[string]sqlc.AbilityCatalogue {
	t.Helper()
	out := map[string]sqlc.AbilityCatalogue{}
	if err := w.pool.InTenantTx(context.Background(), w.tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (m177/m178 catalogue)")
		rows, err := sqlc.New(tx).ListAbilityCatalogue(context.Background())
		for _, r := range rows {
			out[r.Name] = r
		}
		return err
	}); err != nil {
		t.Fatalf("list catalogue: %v", err)
	}
	for _, n := range m177M178Names {
		if _, ok := out[n]; !ok {
			t.Fatalf("INDETERMINATE: no catalogue row %s among %d rows", n, len(out))
		}
	}
	return out
}

// m177M178Audit reads an entry's audit rows, newest first, as wpmgr_app.
func m177M178Audit(t *testing.T, w *e2World, entry uuid.UUID) []sqlc.AbilityCatalogueAudit {
	t.Helper()
	const limit = 100
	var rows []sqlc.AbilityCatalogueAudit
	if err := w.pool.InTenantTx(context.Background(), w.tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (m177/m178 audit)")
		var err error
		rows, err = sqlc.New(tx).ListAbilityCatalogueAudit(context.Background(),
			sqlc.ListAbilityCatalogueAuditParams{EntryID: entry, RowLimit: limit})
		return err
	}); err != nil {
		t.Fatalf("list audit of %s: %v", entry, err)
	}
	if len(rows) >= limit {
		t.Fatalf("INDETERMINATE: %s has at least %d audit rows; a count is not readable", entry, limit)
	}
	return rows
}

// m177M178Restamp: an install on the release before (page-create on m176's
// copy, page-edit on m169's usage, both stamped by its boot) takes m177 and
// m178 in ordinal order. They clear the hash of the two rows they change and
// of no other row. One boot stamp, through its production dispatch as
// wpmgr_app, then stamps exactly those two with the sums of their new bytes,
// each with one NULL-actor audit row from a NULL hash. The stamp then refuses
// both rows again (55000), and a second boot stamps nothing and writes no
// audit row.
//
// Mutation: dropping the hash clear from m177 or m178 leaves that row on its
// old hash and fires "NOT CLEARED".
func m177M178Restamp(t *testing.T) {
	ctx := context.Background()
	w := newE2World(t, true)

	oldSum := map[string]string{
		"wpmgr/page-create": m176Landed(t, w),
		"wpmgr/page-edit":   m178AssertStamped(t, m178SetUsage(t, w, m169EditUsage)),
	}
	before := m177M178Catalogue(t, w)
	auditBefore := map[string]int{}
	for _, n := range m177M178Names {
		auditBefore[n] = len(m177M178Audit(t, w, before[n].EntryID))
	}

	for _, m := range []struct{ name, body string }{{"m177", m177Body(t)}, {"m178", m178Body(t)}} {
		if code, warnings := m176Apply(t, w, m.body); code != "" || len(warnings) != 0 {
			t.Fatalf("%s on the release before: code %q, warnings %q; want neither", m.name, code, warnings)
		}
	}

	var cleared []string
	for name, r := range m177M178Catalogue(t, w) {
		if b, ok := before[name]; ok && b.EntrySha256 != nil && r.EntrySha256 == nil {
			cleared = append(cleared, name)
		}
	}
	sort.Strings(cleared)
	if !reflect.DeepEqual(cleared, m177M178Names) {
		t.Fatalf("NOT CLEARED: the release cleared the hash of %q, want exactly %q", cleared, m177M178Names)
	}

	stamped, err := abilities.StampOwnEntryHashes(ctx, w.pool, slog.Default())
	if err != nil || stamped != len(m177M178Names) {
		t.Fatalf("one boot after the release stamped %d rows (err %v), want the %d it cleared", stamped, err, len(m177M178Names))
	}
	create, edit := m162Row(t, w), m178Row(t, w)
	m176AssertUsage(t, create, m177Usage)
	m176AssertUsage(t, edit, m178Usage)
	if create.Description != m177Description {
		t.Fatalf("page-create's description is not m177's: %q", create.Description)
	}
	rows := map[string]sqlc.AbilityCatalogue{"wpmgr/page-create": create, "wpmgr/page-edit": edit}
	sum := map[string]string{"wpmgr/page-create": m162AssertStamped(t, create), "wpmgr/page-edit": m178AssertStamped(t, edit)}
	for _, n := range m177M178Names {
		if sum[n] == oldSum[n] {
			t.Fatalf("%s was re-stamped with the old hash %s; the new copy did not reach the entry bytes", n, sum[n])
		}
		audit := m177M178Audit(t, w, rows[n].EntryID)
		if len(audit) != auditBefore[n]+1 {
			t.Fatalf("%s carries %d audit rows after the boot, want %d", n, len(audit), auditBefore[n]+1)
		}
		if a := audit[0]; a.ActorUserID.Valid || a.Action != "update" || a.BeforeEntrySha256 != nil ||
			a.AfterEntrySha256 == nil || *a.AfterEntrySha256 != sum[n] ||
			a.BeforeRowSha256 == nil || *a.BeforeRowSha256 == a.AfterRowSha256 {
			t.Fatalf("%s: the re-stamp's audit row is not a NULL-actor stamp from a NULL hash: %+v", n, a)
		}
	}

	other := strings.Repeat("ef", 32)
	if err := w.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InAgentTx (m177/m178 second stamp)")
		for _, n := range m177M178Names {
			for _, sha := range []string{sum[n], other} {
				if _, code := m157Stamp(t, tx, rows[n].EntryID, sha); code != "55000" {
					t.Fatalf("SECOND STAMP: %s was not refused a second stamp %s with 55000 (code %q)", n, sha, code)
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("second stamps: %v", err)
	}

	if again, err := abilities.StampOwnEntryHashes(ctx, w.pool, slog.Default()); err != nil || again != 0 {
		t.Fatalf("a second boot stamped %d rows (err %v), want 0", again, err)
	}
	after := m177M178Catalogue(t, w)
	for _, n := range m177M178Names {
		if h := after[n].EntrySha256; h == nil || *h != sum[n] {
			t.Fatalf("SECOND STAMP: %s's hash moved from %s to %v", n, sum[n], h)
		}
		if got := len(m177M178Audit(t, w, rows[n].EntryID)); got != auditBefore[n]+1 {
			t.Fatalf("%s carries %d audit rows after a second boot, want %d", n, got, auditBefore[n]+1)
		}
	}
}

// m157StampGuards proves the stamp sets a NULL wpmgr hash once with a
// NULL-actor audit row, then refuses a second stamp (55000), a vendor row
// (42501) and a malformed hash (22023), changing nothing.
//
// Mutation: removing the entry_sha256 IS NULL guard from m157 turns the second
// stamp into an overwrite and fires "SECOND STAMP".
func m157StampGuards(t *testing.T) {
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
