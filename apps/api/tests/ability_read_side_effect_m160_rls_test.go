// m160 proofs (owner ruling 4, amended 2026-10-02):
// record_ability_read_side_effect disables a vendor read for the reporting
// tenant at once, and fleet-wide only when three distinct qualified (paid or
// aged) tenants have reported it since the last superadmin re-enable. The
// per-tenant disable is tenant-isolated; the counting table refuses the app
// role outright; the definer refuses a tenant or site that is not the
// transaction's; and the audit's NULL-actor CHECK admits the auto-disable
// shape and nothing broader.
//
// Every record goes through the generated query on the wpmgr_app pool inside
// InTenantTx, the path the Go repo takes. Tenants and sites are seeded on the
// admin pool, and qualification is set there on the tenants row.
package tests

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

func m160Record(ctx context.Context, app *db.Pool, tenant, entry, site uuid.UUID) (int32, error) {
	var n int32
	err := app.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		var err error
		n, err = sqlc.New(tx).RecordAbilityReadSideEffect(ctx, sqlc.RecordAbilityReadSideEffectParams{
			EntryID: entry, SiteID: site, TenantID: tenant,
		})
		return err
	})
	return n, err
}

// m160 tenant kinds. "young" is created now; "aged" 31 days ago; "aged29" 29.
const (
	m160FreeYoung     = "free-young"
	m160PaidActive    = "paid-active"
	m160PastDueGrace  = "past-due-in-grace"
	m160Aged          = "aged"
	m160Aged29        = "aged-29-days"
	m160TrialingYoung = "trialing-young"
	m160CompedYoung   = "comped-young"
)

// m160Tenant seeds a tenant of the given kind with one site and returns both.
func m160Tenant(t *testing.T, adm *db.Pool, kind string) (uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	tenant := seedTenant(t, adm, "m160-"+uuid.NewString()[:8])
	var stmt string
	switch kind {
	case m160FreeYoung:
	case m160PaidActive:
		stmt = `UPDATE tenants SET plan = 'starter', plan_status = 'active' WHERE id = $1`
	case m160PastDueGrace:
		stmt = `UPDATE tenants SET plan = 'agency', plan_status = 'past_due', grace_until = now() + interval '3 days' WHERE id = $1`
	case m160Aged:
		stmt = `UPDATE tenants SET created_at = now() - interval '31 days' WHERE id = $1`
	case m160Aged29:
		stmt = `UPDATE tenants SET created_at = now() - interval '29 days' WHERE id = $1`
	case m160TrialingYoung:
		stmt = `UPDATE tenants SET plan = 'starter', plan_status = 'trialing' WHERE id = $1`
	case m160CompedYoung:
		stmt = `UPDATE tenants SET plan = 'scale', plan_status = 'comped' WHERE id = $1`
	default:
		t.Fatalf("unknown m160 tenant kind %q", kind)
	}
	if stmt != "" {
		if _, err := adm.Exec(ctx, stmt, tenant); err != nil {
			t.Fatalf("set tenant kind %s: %v", kind, err)
		}
	}
	return tenant, m160Site(t, adm, tenant)
}

func m160Site(t *testing.T, adm *db.Pool, tenant uuid.UUID) uuid.UUID {
	t.Helper()
	return seedSiteOnAdminPool(t, adm, tenant, "https://m160-"+uuid.NewString()+".example.com")
}

func m160VendorRead(t *testing.T, ctx context.Context, app *db.Pool, root uuid.UUID) sqlc.AbilityCatalogue {
	t.Helper()
	e, err := c1Upsert(ctx, app, root, c1VendorRead(root, "m160-"+uuid.NewString()[:8]+"/read", "1.0", "2.0"))
	if err != nil {
		t.Fatalf("seed vendor read: %v", err)
	}
	return e
}

// m160Reenable is the superadmin re-enable: the admin upsert with enabled
// true, the path the admin screen takes.
func m160Reenable(t *testing.T, ctx context.Context, app *db.Pool, root, id uuid.UUID) {
	t.Helper()
	cur := m160Entry(t, ctx, app, root, id)
	p := c1VendorRead(root, cur.Name, "1.0", "2.0")
	p.EntryID = pgtype.UUID{Bytes: cur.EntryID, Valid: true}
	if _, err := c1Upsert(ctx, app, root, p); err != nil {
		t.Fatalf("superadmin re-enable: %v", err)
	}
	if !m160Entry(t, ctx, app, root, id).Enabled {
		t.Fatal("INDETERMINATE: the superadmin re-enable left the entry disabled")
	}
}

func m160Entry(t *testing.T, ctx context.Context, app *db.Pool, actor, id uuid.UUID) sqlc.AbilityCatalogue {
	t.Helper()
	var e sqlc.AbilityCatalogue
	if err := app.InUserTx(ctx, actor, func(tx pgx.Tx) error {
		var err error
		e, err = sqlc.New(tx).GetAbilityCatalogueEntry(ctx, id)
		return err
	}); err != nil {
		t.Fatalf("read entry %s: %v", id, err)
	}
	return e
}

func m160Audit(t *testing.T, ctx context.Context, app *db.Pool, actor, id uuid.UUID) []sqlc.AbilityCatalogueAudit {
	t.Helper()
	var rows []sqlc.AbilityCatalogueAudit
	if err := app.InUserTx(ctx, actor, func(tx pgx.Tx) error {
		var err error
		rows, err = sqlc.New(tx).ListAbilityCatalogueAudit(ctx, sqlc.ListAbilityCatalogueAuditParams{EntryID: id, RowLimit: 50})
		return err
	}); err != nil {
		t.Fatalf("read audit %s: %v", id, err)
	}
	return rows
}

// m160SystemDisables counts the audit rows the auto-disable wrote.
func m160SystemDisables(rows []sqlc.AbilityCatalogueAudit) int {
	n := 0
	for _, r := range rows {
		if !r.ActorUserID.Valid && r.BeforeEnabled != nil && *r.BeforeEnabled && !r.AfterEnabled {
			n++
		}
	}
	return n
}

// m160DisabledFor asks, inside viewer's tenant transaction, whether the entry
// is disabled for tenant: the read path's query.
func m160DisabledFor(t *testing.T, ctx context.Context, app *db.Pool, viewer, tenant, entry uuid.UUID) bool {
	t.Helper()
	var off bool
	if err := app.InTenantTx(ctx, viewer, func(tx pgx.Tx) error {
		var err error
		off, err = sqlc.New(tx).IsAbilityDisabledForTenant(ctx, sqlc.IsAbilityDisabledForTenantParams{TenantID: tenant, EntryID: entry})
		return err
	}); err != nil {
		t.Fatalf("read tenant disable: %v", err)
	}
	return off
}

func m160Want(t *testing.T, label string, got int32, err error, want int32) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: record failed: %v", label, err)
	}
	if got != want {
		t.Fatalf("%s: qualified-tenant count %d, want %d", label, got, want)
	}
}

func m160WantEnabled(t *testing.T, ctx context.Context, app *db.Pool, root, id uuid.UUID, label string, want bool) {
	t.Helper()
	if got := m160Entry(t, ctx, app, root, id).Enabled; got != want {
		t.Fatalf("%s: fleet entry enabled=%t, want %t", label, got, want)
	}
}

// TestAbilityReadSideEffectM160OneTenantManySites: one paid tenant reporting
// from three sites is ONE contribution. The fleet entry stays enabled; the
// entry is off for that tenant from the first report, and on for a tenant
// that never reported.
//
// Mutation: key the reports on a site (PRIMARY KEY (entry_id, epoch,
// first_site_id)) and the third site reads 3 and disables the fleet entry.
// Mutation: drop the R4 INSERT into ability_tenant_disables and DISABLED FOR
// THE REPORTER goes red.
func TestAbilityReadSideEffectM160OneTenantManySites(t *testing.T) {
	ctx, app, adm, root := c1World(t)
	entry := m160VendorRead(t, ctx, app, root)
	tenant, s1 := m160Tenant(t, adm, m160PaidActive)
	s2, s3 := m160Site(t, adm, tenant), m160Site(t, adm, tenant)
	other, _ := m160Tenant(t, adm, m160PaidActive)

	if m160DisabledFor(t, ctx, app, tenant, tenant, entry.EntryID) {
		t.Fatal("INDETERMINATE: disabled for the tenant before any report")
	}
	for i, s := range []uuid.UUID{s1, s2, s3, s1} {
		n, err := m160Record(ctx, app, tenant, entry.EntryID, s)
		m160Want(t, "one tenant, report "+string(rune('1'+i)), n, err, 1)
	}
	m160WantEnabled(t, ctx, app, root, entry.EntryID, "ONE TENANT, THREE SITES", true)
	if got := m160SystemDisables(m160Audit(t, ctx, app, root, entry.EntryID)); got != 0 {
		t.Fatalf("ONE TENANT, THREE SITES: %d system disables, want 0", got)
	}
	if !m160DisabledFor(t, ctx, app, tenant, tenant, entry.EntryID) {
		t.Fatal("DISABLED FOR THE REPORTER: the reporting tenant still has the entry on")
	}
	if m160DisabledFor(t, ctx, app, other, other, entry.EntryID) {
		t.Fatal("OTHER TENANT: a tenant that never reported has the entry off")
	}
}

// TestAbilityReadSideEffectM160ThreeQualifiedTenants: paid-active, past-due in
// grace, and aged (free, 31 days) are each a qualified tenant; the third
// disables the fleet entry with one NULL-actor audit row, and a repeat
// report from a counted tenant writes nothing more.
//
// Mutation: threshold `v_count >= 4` and THIRD TENANT goes red. Mutation:
// count every row instead of `AND qualified`, or drop the one-row-per-tenant
// key, and REPEAT reads 4.
func TestAbilityReadSideEffectM160ThreeQualifiedTenants(t *testing.T) {
	ctx, app, adm, root := c1World(t)
	entry := m160VendorRead(t, ctx, app, root)
	a, sa := m160Tenant(t, adm, m160PaidActive)
	b, sb := m160Tenant(t, adm, m160PastDueGrace)
	c, sc := m160Tenant(t, adm, m160Aged)

	n, err := m160Record(ctx, app, a, entry.EntryID, sa)
	m160Want(t, "tenant 1 (paid)", n, err, 1)
	n, err = m160Record(ctx, app, b, entry.EntryID, sb)
	m160Want(t, "tenant 2 (past due in grace)", n, err, 2)
	m160WantEnabled(t, ctx, app, root, entry.EntryID, "TWO TENANTS", true)

	n, err = m160Record(ctx, app, c, entry.EntryID, sc)
	m160Want(t, "tenant 3 (aged)", n, err, 3)
	m160WantEnabled(t, ctx, app, root, entry.EntryID, "THIRD TENANT", false)
	audit := m160Audit(t, ctx, app, root, entry.EntryID)
	if len(audit) != 2 || m160SystemDisables(audit) != 1 {
		t.Fatalf("THIRD TENANT: %d audit rows with %d system disables, want 2 and 1", len(audit), m160SystemDisables(audit))
	}
	top := audit[0]
	if top.ActorUserID.Valid || top.Action != "update" || top.BeforeRowSha256 == nil ||
		*top.BeforeRowSha256 == top.AfterRowSha256 {
		t.Fatalf("THIRD TENANT: the disable's audit row has the wrong shape: %+v", top)
	}

	n, err = m160Record(ctx, app, a, entry.EntryID, m160Site(t, adm, a))
	m160Want(t, "tenant 1 again, new site", n, err, 3)
	if got := m160SystemDisables(m160Audit(t, ctx, app, root, entry.EntryID)); got != 1 {
		t.Fatalf("REPEAT: %d system disables, want 1", got)
	}
}

// TestAbilityReadSideEffectM160UnqualifiedTenantsDoNotCount: a free young
// tenant, a trialing young tenant, a comped young tenant and a free tenant
// aged 29 days each get the entry off for themselves and add nothing to the
// fleet count. A tenant that reported while unqualified counts once it
// reports again after qualifying.
//
// Mutation: make ability_side_effect_tenant_qualifies return true and
// UNQUALIFIED goes red. Mutation: min age '28 days' and the 29-day tenant
// counts. Mutation: admit plan_status 'trialing' and the trial counts.
func TestAbilityReadSideEffectM160UnqualifiedTenantsDoNotCount(t *testing.T) {
	ctx, app, adm, root := c1World(t)
	entry := m160VendorRead(t, ctx, app, root)
	a, sa := m160Tenant(t, adm, m160PaidActive)
	b, sb := m160Tenant(t, adm, m160Aged)
	n, err := m160Record(ctx, app, a, entry.EntryID, sa)
	m160Want(t, "qualified 1", n, err, 1)
	n, err = m160Record(ctx, app, b, entry.EntryID, sb)
	m160Want(t, "qualified 2", n, err, 2)

	var young uuid.UUID
	for _, kind := range []string{m160FreeYoung, m160TrialingYoung, m160CompedYoung, m160Aged29} {
		u, su := m160Tenant(t, adm, kind)
		if kind == m160FreeYoung {
			young = u
		}
		n, err := m160Record(ctx, app, u, entry.EntryID, su)
		m160Want(t, "UNQUALIFIED "+kind, n, err, 2)
		m160WantEnabled(t, ctx, app, root, entry.EntryID, "UNQUALIFIED "+kind, true)
		if !m160DisabledFor(t, ctx, app, u, u, entry.EntryID) {
			t.Fatalf("UNQUALIFIED %s: the reporting tenant still has the entry on", kind)
		}
	}

	// The free young tenant ages past the threshold and reports again.
	if _, err := adm.Exec(ctx, `UPDATE tenants SET created_at = now() - ability_side_effect_tenant_min_age() - interval '1 minute' WHERE id = $1`, young); err != nil {
		t.Fatalf("age tenant: %v", err)
	}
	n, err = m160Record(ctx, app, young, entry.EntryID, m160Site(t, adm, young))
	m160Want(t, "young tenant, now aged", n, err, 3)
	m160WantEnabled(t, ctx, app, root, entry.EntryID, "NOW QUALIFIED", false)
}

// TestAbilityReadSideEffectM160EpochReset: after a superadmin re-enables the
// entry, reports from before do not count. The same three tenants are needed
// again, each reporting after the re-enable, and a repeat inside the new
// epoch counts once.
//
// Mutation: drop `AND epoch = v_epoch` from the count and the first report
// after the re-enable reads 4 and disables at once. Mutation: compute v_epoch
// as 0 always and the same.
func TestAbilityReadSideEffectM160EpochReset(t *testing.T) {
	ctx, app, adm, root := c1World(t)
	entry := m160VendorRead(t, ctx, app, root)
	a, sa := m160Tenant(t, adm, m160PaidActive)
	b, sb := m160Tenant(t, adm, m160PaidActive)
	c, sc := m160Tenant(t, adm, m160Aged)
	for i, p := range [][2]uuid.UUID{{a, sa}, {b, sb}, {c, sc}} {
		n, err := m160Record(ctx, app, p[0], entry.EntryID, p[1])
		m160Want(t, "epoch 0", n, err, int32(i+1))
	}
	m160WantEnabled(t, ctx, app, root, entry.EntryID, "EPOCH 0", false)

	m160Reenable(t, ctx, app, root, entry.EntryID)

	n, err := m160Record(ctx, app, a, entry.EntryID, sa)
	m160Want(t, "epoch 1, tenant a", n, err, 1)
	m160WantEnabled(t, ctx, app, root, entry.EntryID, "EPOCH 1, ONE TENANT", true)
	n, err = m160Record(ctx, app, a, entry.EntryID, m160Site(t, adm, a))
	m160Want(t, "epoch 1, tenant a again", n, err, 1)
	n, err = m160Record(ctx, app, b, entry.EntryID, sb)
	m160Want(t, "epoch 1, tenant b", n, err, 2)
	m160WantEnabled(t, ctx, app, root, entry.EntryID, "EPOCH 1, TWO TENANTS", true)
	n, err = m160Record(ctx, app, c, entry.EntryID, sc)
	m160Want(t, "epoch 1, tenant c", n, err, 3)
	m160WantEnabled(t, ctx, app, root, entry.EntryID, "EPOCH 1, THREE TENANTS", false)
	if got := m160SystemDisables(m160Audit(t, ctx, app, root, entry.EntryID)); got != 2 {
		t.Fatalf("EPOCH 1: %d system disables, want 2", got)
	}
}

// TestAbilityReadSideEffectM160TenantDisableIsolation: the per-tenant disable
// is invisible to another tenant and cannot be re-enabled from it; the owner
// tenant can re-enable it, and its next report disables it again. wpmgr_app
// cannot insert or delete a row, or move one to another tenant.
//
// Mutation: drop the ability_tenant_disables_tenant_isolation policy (or
// make it USING (true)) and OTHER TENANT SEES goes red. Mutation: re-grant
// INSERT to wpmgr_app and SELF-DISABLE goes red.
func TestAbilityReadSideEffectM160TenantDisableIsolation(t *testing.T) {
	ctx, app, adm, root := c1World(t)
	entry := m160VendorRead(t, ctx, app, root)
	a, sa := m160Tenant(t, adm, m160FreeYoung)
	b, _ := m160Tenant(t, adm, m160FreeYoung)
	admin := seedUserRow(t, adm, "m160-admin-"+uuid.NewString()[:8]+"@example.test")
	if _, err := m160Record(ctx, app, a, entry.EntryID, sa); err != nil {
		t.Fatalf("report: %v", err)
	}
	if !m160DisabledFor(t, ctx, app, a, a, entry.EntryID) {
		t.Fatal("POSITIVE CONTROL: tenant a does not see its own disable")
	}
	if m160DisabledFor(t, ctx, app, b, a, entry.EntryID) {
		t.Fatal("OTHER TENANT SEES: tenant b's transaction reads tenant a's disable")
	}
	var listed []uuid.UUID
	var seen int
	var reenabled int64
	if err := app.InTenantTx(ctx, b, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		var err error
		if listed, err = q.ListAbilityTenantDisabledEntryIDs(ctx, a); err != nil {
			return err
		}
		if err = tx.QueryRow(ctx, `SELECT count(*) FROM ability_tenant_disables`).Scan(&seen); err != nil {
			return err
		}
		reenabled, err = q.ReenableAbilityForTenant(ctx, sqlc.ReenableAbilityForTenantParams{UserID: admin, TenantID: a, EntryID: entry.EntryID})
		return err
	}); err != nil {
		t.Fatalf("tenant b reads: %v", err)
	}
	if len(listed) != 0 || seen != 0 || reenabled != 0 {
		t.Fatalf("OTHER TENANT SEES: listed %d, counted %d, re-enabled %d rows of tenant a, want 0, 0, 0", len(listed), seen, reenabled)
	}
	if !m160DisabledFor(t, ctx, app, a, a, entry.EntryID) {
		t.Fatal("OTHER TENANT RE-ENABLE: tenant b switched tenant a's entry back on")
	}

	for _, stmt := range []string{
		`INSERT INTO ability_tenant_disables (tenant_id, entry_id) VALUES ($1, $2)`,
		`DELETE FROM ability_tenant_disables WHERE tenant_id = $1 AND entry_id = $2`,
		`UPDATE ability_tenant_disables SET tenant_id = $1 WHERE entry_id = $2`,
	} {
		err := app.InTenantTx(ctx, a, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, stmt, a, entry.EntryID)
			return err
		})
		if c1Code(err) != "42501" {
			t.Fatalf("SELF-DISABLE / DIRECT WRITE: %q answered %v, want 42501", stmt, err)
		}
	}

	var own int64
	if err := app.InTenantTx(ctx, a, func(tx pgx.Tx) error {
		var err error
		own, err = sqlc.New(tx).ReenableAbilityForTenant(ctx, sqlc.ReenableAbilityForTenantParams{UserID: admin, TenantID: a, EntryID: entry.EntryID})
		return err
	}); err != nil || own != 1 {
		t.Fatalf("OWN RE-ENABLE: %d rows, %v; want 1, nil", own, err)
	}
	if m160DisabledFor(t, ctx, app, a, a, entry.EntryID) {
		t.Fatal("OWN RE-ENABLE: still disabled for tenant a")
	}
	if _, err := m160Record(ctx, app, a, entry.EntryID, sa); err != nil {
		t.Fatalf("report again: %v", err)
	}
	if !m160DisabledFor(t, ctx, app, a, a, entry.EntryID) {
		t.Fatal("REPORT AFTER RE-ENABLE: a new report did not disable it for tenant a again")
	}
}

// TestAbilityReadSideEffectM160CrossTenantRefused: the definer takes the
// tenant from app.tenant_id. A site of another tenant, or a tenant argument
// that is not the transaction's, is refused with 42501 and records nothing.
//
// Mutation: drop the sites lookup and CROSS-TENANT SITE answers nil.
// Mutation: drop `OR v_tenant <> p_tenant_id` and TENANT ARGUMENT answers nil.
func TestAbilityReadSideEffectM160CrossTenantRefused(t *testing.T) {
	ctx, app, adm, root := c1World(t)
	entry := m160VendorRead(t, ctx, app, root)
	a, _ := m160Tenant(t, adm, m160PaidActive)
	b, sb := m160Tenant(t, adm, m160PaidActive)

	if _, err := m160Record(ctx, app, a, entry.EntryID, sb); c1Code(err) != "42501" {
		t.Fatalf("CROSS-TENANT SITE: answered %v, want 42501", err)
	}
	err := app.InTenantTx(ctx, a, func(tx pgx.Tx) error {
		_, err := sqlc.New(tx).RecordAbilityReadSideEffect(ctx, sqlc.RecordAbilityReadSideEffectParams{
			EntryID: entry.EntryID, SiteID: sb, TenantID: b,
		})
		return err
	})
	if c1Code(err) != "42501" {
		t.Fatalf("TENANT ARGUMENT: a call in tenant a's transaction naming tenant b answered %v, want 42501", err)
	}
	err = app.InTenantTx(ctx, a, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', '', true)`)
		if err != nil {
			return err
		}
		_, err = sqlc.New(tx).RecordAbilityReadSideEffect(ctx, sqlc.RecordAbilityReadSideEffectParams{
			EntryID: entry.EntryID, SiteID: sb, TenantID: b,
		})
		return err
	})
	if c1Code(err) != "42501" {
		t.Fatalf("NO TENANT GUC: answered %v, want 42501", err)
	}
	var reports, disables int
	if err := adm.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM ability_read_side_effect_reports WHERE entry_id = $1),
			(SELECT count(*) FROM ability_tenant_disables WHERE entry_id = $1)`, entry.EntryID).
		Scan(&reports, &disables); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if reports != 0 || disables != 0 {
		t.Fatalf("REFUSED CALLS RECORDED: %d reports and %d tenant disables, want 0 and 0", reports, disables)
	}
	// Positive control: the same site from its own tenant is accepted.
	n, err := m160Record(ctx, app, b, entry.EntryID, sb)
	m160Want(t, "POSITIVE CONTROL", n, err, 1)
}

// TestAbilityReadSideEffectM160Refusals: not a vendor read, no entry, a NULL
// argument. Nothing recorded.
func TestAbilityReadSideEffectM160Refusals(t *testing.T) {
	ctx, app, adm, root := c1World(t)
	entry := m160VendorRead(t, ctx, app, root)
	tenant, site := m160Tenant(t, adm, m160PaidActive)
	var wpmgrID uuid.UUID
	if err := app.InUserTx(ctx, root, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT entry_id FROM ability_catalogue WHERE name = 'wpmgr/site-facts'`).Scan(&wpmgrID)
	}); err != nil {
		t.Fatalf("INDETERMINATE: wpmgr/site-facts seed missing: %v", err)
	}
	if _, err := m160Record(ctx, app, tenant, wpmgrID, site); c1Code(err) != "42501" {
		t.Fatalf("WPMGR ENTRY: answered %v, want 42501", err)
	}
	if _, err := m160Record(ctx, app, tenant, uuid.New(), site); c1Code(err) != "P0002" {
		t.Fatalf("UNKNOWN ENTRY: answered %v, want P0002", err)
	}
	err := app.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT record_ability_read_side_effect($1, NULL, $2)`, entry.EntryID, tenant)
		return err
	})
	if c1Code(err) != "22023" {
		t.Fatalf("NULL SITE: answered %v, want 22023", err)
	}
	var stored int
	if err := adm.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM ability_read_side_effect_reports WHERE entry_id = $1)
		  + (SELECT count(*) FROM ability_tenant_disables WHERE entry_id = $1)`, wpmgrID).Scan(&stored); err != nil {
		t.Fatalf("count wpmgr rows: %v", err)
	}
	if stored != 0 {
		t.Fatalf("WPMGR ENTRY: %d rows recorded for a refused entry, want 0", stored)
	}
}

// TestAbilityReadSideEffectM160ConcurrentThird: three qualified tenants
// reporting at once disable the entry exactly once. The per-name advisory
// lock is what makes the last count see the other two.
func TestAbilityReadSideEffectM160ConcurrentThird(t *testing.T) {
	ctx, app, adm, root := c1World(t)
	entry := m160VendorRead(t, ctx, app, root)
	type rep struct{ tenant, site uuid.UUID }
	reps := make([]rep, 3)
	for i := range reps {
		reps[i].tenant, reps[i].site = m160Tenant(t, adm, m160PaidActive)
	}
	var wg sync.WaitGroup
	errs := make([]error, 3)
	start := make(chan struct{})
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = m160Record(ctx, app, reps[i].tenant, entry.EntryID, reps[i].site)
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("reporter %d: %v", i, err)
		}
	}
	m160WantEnabled(t, ctx, app, root, entry.EntryID, "CONCURRENT", false)
	if got := m160SystemDisables(m160Audit(t, ctx, app, root, entry.EntryID)); got != 1 {
		t.Fatalf("CONCURRENT: %d system disables, want exactly 1", got)
	}
}

// TestAbilityReadSideEffectM160DirectAccessRefused: wpmgr_app reaches the
// counting table only through the definer. Every direct statement is refused,
// and the function is EXECUTE-able by wpmgr_app and not by PUBLIC.
func TestAbilityReadSideEffectM160DirectAccessRefused(t *testing.T) {
	ctx, app, adm, root := c1World(t)
	entry := m160VendorRead(t, ctx, app, root)
	tenant, site := m160Tenant(t, adm, m160PaidActive)
	if _, err := m160Record(ctx, app, tenant, entry.EntryID, site); err != nil {
		t.Fatalf("POSITIVE CONTROL: the definer path failed: %v", err)
	}
	for _, stmt := range []string{
		`SELECT count(*) FROM ability_read_side_effect_reports`,
		`INSERT INTO ability_read_side_effect_reports (entry_id, epoch, tenant_id, first_site_id, qualified, qualified_at) VALUES ($1, 99, $2, gen_random_uuid(), true, now())`,
		`UPDATE ability_read_side_effect_reports SET qualified = true, qualified_at = now() WHERE entry_id = $1 AND tenant_id = $2`,
		`DELETE FROM ability_read_side_effect_reports WHERE entry_id = $1 AND tenant_id = $2`,
	} {
		err := app.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
			var err error
			if strings.HasPrefix(stmt, "SELECT") {
				_, err = tx.Exec(ctx, stmt)
			} else {
				_, err = tx.Exec(ctx, stmt, entry.EntryID, tenant)
			}
			return err
		})
		if c1Code(err) != "42501" {
			t.Fatalf("DIRECT ACCESS: %q answered %v, want 42501", stmt, err)
		}
	}
	var appExec, pubExec, definer bool
	if err := adm.QueryRow(ctx, `SELECT
			has_function_privilege('wpmgr_app', 'public.record_ability_read_side_effect(uuid, uuid, uuid)', 'EXECUTE'),
			has_function_privilege('public', 'public.record_ability_read_side_effect(uuid, uuid, uuid)', 'EXECUTE'),
			(SELECT prosecdef AND proconfig @> ARRAY['search_path=public, pg_temp']
			   FROM pg_proc WHERE oid = 'public.record_ability_read_side_effect(uuid, uuid, uuid)'::regprocedure)`).
		Scan(&appExec, &pubExec, &definer); err != nil {
		t.Fatalf("read function privileges: %v", err)
	}
	if !appExec || pubExec || !definer {
		t.Fatalf("FUNCTION: wpmgr_app EXECUTE=%t (want true), PUBLIC EXECUTE=%t (want false), definer with pinned search_path=%t (want true)", appExec, pubExec, definer)
	}
}

// TestAbilityReadSideEffectM160AuditCheck: a NULL-actor audit row is admitted
// in exactly two shapes, m157's stamp and m160's disable. wpmgr_app holds no
// INSERT on the audit, so the CHECK is probed as wpmgr_owner, the migration
// role that owns the table and runs the definers. Every probe rolls back.
func TestAbilityReadSideEffectM160AuditCheck(t *testing.T) {
	ctx := context.Background()
	app := startPostgres(t)
	owner := connectOwner(t, app)
	t.Cleanup(owner.Close)

	a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
	type shape struct {
		label                   string
		action                  string
		beforeRow               *string
		beforeEntry, afterEntry *string
		beforeEnabled           *bool
		afterEnabled            bool
		ok                      bool
	}
	tr, fa := true, false
	cases := []shape{
		{"stamp (m157)", "update", &a, nil, &b, &tr, true, true},
		{"disable (m160)", "update", &a, &b, &b, &tr, false, true},
		{"disable, both hashes NULL", "update", &a, nil, nil, &tr, false, true},
		{"insert", "insert", nil, nil, &b, nil, true, false},
		{"enable false->true", "update", &a, &b, &b, &fa, true, false},
		{"disable with a hash change", "update", &a, &a, &b, &tr, false, false},
		{"disable clearing the hash", "update", &a, &b, nil, &tr, false, false},
		{"disable from NULL enabled", "update", &a, &b, &b, nil, false, false},
		{"no change at all", "update", &a, &b, &b, &tr, true, false},
		{"stamp that also disables", "update", &a, nil, &b, &tr, false, false},
	}
	for _, c := range cases {
		tx, err := owner.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		_, err = tx.Exec(ctx, `INSERT INTO ability_catalogue_audit (
				entry_id, name, action, actor_user_id, before_row_sha256, after_row_sha256,
				before_entry_sha256, after_entry_sha256, before_enabled, after_enabled)
			VALUES (gen_random_uuid(), 'm160/probe', $1, NULL, $2, $3, $4, $5, $6, $7)`,
			c.action, c.beforeRow, b, c.beforeEntry, c.afterEntry, c.beforeEnabled, c.afterEnabled)
		_ = tx.Rollback(ctx)
		if c.ok && err != nil {
			t.Fatalf("%s: an admitted NULL-actor shape was refused: %v", c.label, err)
		}
		if !c.ok && c1Code(err) != "23514" {
			t.Fatalf("%s: a NULL-actor row of another shape answered %v, want 23514", c.label, err)
		}
	}
}
