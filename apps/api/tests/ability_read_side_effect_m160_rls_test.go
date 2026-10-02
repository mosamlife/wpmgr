// m160 proofs (owner ruling 4): record_ability_read_side_effect counts
// distinct sites per vendor read and disables the entry fleet-wide at the
// third; ability_read_side_effect_sites refuses the app role outright; and
// the audit's NULL-actor CHECK admits the auto-disable shape and nothing
// broader. Every record goes through the generated query on the wpmgr_app
// pool inside InTenantTx, the path reportReadSideEffect will take.
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

func m160Want(t *testing.T, label string, got int32, err error, want int32) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: record failed: %v", label, err)
	}
	if got != want {
		t.Fatalf("%s: distinct-site count %d, want %d", label, got, want)
	}
}

// TestAbilityReadSideEffectM160Threshold: two sites leave the entry enabled,
// a repeat site counts once, the third distinct site disables it with one
// NULL-actor audit row, and after a superadmin re-enables it only a site not
// seen before disables it again.
func TestAbilityReadSideEffectM160Threshold(t *testing.T) {
	ctx, app, adm, root := c1World(t)
	tenant := seedTenant(t, adm, "m160-"+uuid.NewString()[:8])
	entry, err := c1Upsert(ctx, app, root, c1VendorRead(root, "m160-"+uuid.NewString()[:8]+"/read", "1.0", "2.0"))
	if err != nil {
		t.Fatalf("seed vendor read: %v", err)
	}
	s1, s2, s3, s4 := uuid.New(), uuid.New(), uuid.New(), uuid.New()

	n, err := m160Record(ctx, app, tenant, entry.EntryID, s1)
	m160Want(t, "site 1", n, err, 1)
	n, err = m160Record(ctx, app, tenant, entry.EntryID, s1)
	m160Want(t, "site 1 again", n, err, 1)
	n, err = m160Record(ctx, app, tenant, entry.EntryID, s2)
	m160Want(t, "site 2", n, err, 2)
	n, err = m160Record(ctx, app, tenant, entry.EntryID, s2)
	m160Want(t, "site 2 again", n, err, 2)
	if e := m160Entry(t, ctx, app, root, entry.EntryID); !e.Enabled {
		t.Fatal("TWO SITES: the entry is disabled after two distinct sites, want enabled")
	}
	if got := len(m160Audit(t, ctx, app, root, entry.EntryID)); got != 1 {
		t.Fatalf("TWO SITES: %d audit rows, want 1 (the superadmin insert)", got)
	}

	n, err = m160Record(ctx, app, tenant, entry.EntryID, s3)
	m160Want(t, "site 3", n, err, 3)
	if e := m160Entry(t, ctx, app, root, entry.EntryID); e.Enabled {
		t.Fatal("THIRD SITE: the entry is still enabled after three distinct sites")
	}
	audit := m160Audit(t, ctx, app, root, entry.EntryID)
	if len(audit) != 2 || m160SystemDisables(audit) != 1 {
		t.Fatalf("THIRD SITE: %d audit rows with %d system disables, want 2 and 1", len(audit), m160SystemDisables(audit))
	}
	top := audit[0]
	if top.ActorUserID.Valid || top.Action != "update" || top.BeforeRowSha256 == nil ||
		*top.BeforeRowSha256 == top.AfterRowSha256 {
		t.Fatalf("THIRD SITE: the disable's audit row has the wrong shape: %+v", top)
	}

	n, err = m160Record(ctx, app, tenant, entry.EntryID, s3)
	m160Want(t, "site 3 again", n, err, 3)
	if got := m160SystemDisables(m160Audit(t, ctx, app, root, entry.EntryID)); got != 1 {
		t.Fatalf("REPEAT: a repeat report wrote another disable (%d system disables, want 1)", got)
	}

	// A superadmin re-enables it. A counted site does not override that.
	cur := m160Entry(t, ctx, app, root, entry.EntryID)
	p := c1VendorRead(root, cur.Name, "1.0", "2.0")
	p.EntryID = pgtype.UUID{Bytes: cur.EntryID, Valid: true}
	if _, err := c1Upsert(ctx, app, root, p); err != nil {
		t.Fatalf("superadmin re-enable: %v", err)
	}
	n, err = m160Record(ctx, app, tenant, entry.EntryID, s1)
	m160Want(t, "re-enabled, site 1 again", n, err, 3)
	if e := m160Entry(t, ctx, app, root, entry.EntryID); !e.Enabled {
		t.Fatal("RE-ENABLED: a repeat report from a counted site disabled the entry again")
	}
	n, err = m160Record(ctx, app, tenant, entry.EntryID, s4)
	m160Want(t, "re-enabled, site 4", n, err, 4)
	if e := m160Entry(t, ctx, app, root, entry.EntryID); e.Enabled {
		t.Fatal("RE-ENABLED: a new fourth site did not disable the entry")
	}
	if got := m160SystemDisables(m160Audit(t, ctx, app, root, entry.EntryID)); got != 2 {
		t.Fatalf("RE-ENABLED: %d system disables, want 2", got)
	}

	// Refusals: not a vendor read, no entry, NULL argument. Nothing recorded.
	var wpmgrID uuid.UUID
	if err := app.InUserTx(ctx, root, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT entry_id FROM ability_catalogue WHERE name = 'wpmgr/site-facts'`).Scan(&wpmgrID)
	}); err != nil {
		t.Fatalf("INDETERMINATE: wpmgr/site-facts seed missing: %v", err)
	}
	if _, err := m160Record(ctx, app, tenant, wpmgrID, s1); c1Code(err) != "42501" {
		t.Fatalf("WPMGR ENTRY: answered %v, want 42501", err)
	}
	if _, err := m160Record(ctx, app, tenant, uuid.New(), s1); c1Code(err) != "P0002" {
		t.Fatalf("UNKNOWN ENTRY: answered %v, want P0002", err)
	}
	err = app.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT record_ability_read_side_effect($1, NULL, $2)`, entry.EntryID, tenant)
		return err
	})
	if c1Code(err) != "22023" {
		t.Fatalf("NULL SITE: answered %v, want 22023", err)
	}
	var stored int
	if err := adm.QueryRow(ctx, `SELECT count(*) FROM ability_read_side_effect_sites WHERE entry_id = $1`, wpmgrID).Scan(&stored); err != nil {
		t.Fatalf("count wpmgr rows: %v", err)
	}
	if stored != 0 {
		t.Fatalf("WPMGR ENTRY: %d rows recorded for a refused entry, want 0", stored)
	}
}

// TestAbilityReadSideEffectM160ConcurrentThird: three new sites reported at
// once disable the entry exactly once. The per-name advisory lock is what
// makes the last count see the other two.
func TestAbilityReadSideEffectM160ConcurrentThird(t *testing.T) {
	ctx, app, adm, root := c1World(t)
	tenant := seedTenant(t, adm, "m160c-"+uuid.NewString()[:8])
	entry, err := c1Upsert(ctx, app, root, c1VendorRead(root, "m160c-"+uuid.NewString()[:8]+"/read", "1.0", "2.0"))
	if err != nil {
		t.Fatalf("seed vendor read: %v", err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 3)
	start := make(chan struct{})
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = m160Record(ctx, app, tenant, entry.EntryID, uuid.New())
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("reporter %d: %v", i, err)
		}
	}
	if e := m160Entry(t, ctx, app, root, entry.EntryID); e.Enabled {
		t.Fatal("CONCURRENT: three concurrent distinct sites left the entry enabled")
	}
	if got := m160SystemDisables(m160Audit(t, ctx, app, root, entry.EntryID)); got != 1 {
		t.Fatalf("CONCURRENT: %d system disables, want exactly 1", got)
	}
}

// TestAbilityReadSideEffectM160DirectAccessRefused: wpmgr_app reaches the
// table only through the definer. Every direct statement is refused, and the
// function is EXECUTE-able by wpmgr_app and not by PUBLIC.
func TestAbilityReadSideEffectM160DirectAccessRefused(t *testing.T) {
	ctx, app, adm, root := c1World(t)
	tenant := seedTenant(t, adm, "m160d-"+uuid.NewString()[:8])
	entry, err := c1Upsert(ctx, app, root, c1VendorRead(root, "m160d-"+uuid.NewString()[:8]+"/read", "1.0", "2.0"))
	if err != nil {
		t.Fatalf("seed vendor read: %v", err)
	}
	if _, err := m160Record(ctx, app, tenant, entry.EntryID, uuid.New()); err != nil {
		t.Fatalf("POSITIVE CONTROL: the definer path failed: %v", err)
	}
	for _, stmt := range []string{
		`SELECT count(*) FROM ability_read_side_effect_sites`,
		`INSERT INTO ability_read_side_effect_sites (entry_id, site_id, tenant_id) VALUES ($1, gen_random_uuid(), $2)`,
		`UPDATE ability_read_side_effect_sites SET first_seen = now() WHERE entry_id = $1 AND tenant_id = $2`,
		`DELETE FROM ability_read_side_effect_sites WHERE entry_id = $1 AND tenant_id = $2`,
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
