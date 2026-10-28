// report_schedule_overview_m185_repair_test.go - m185 turns the client report
// Overview section back on for every saved report schedule that stores it as
// off (GH #972, 20261028000000_m185_report_schedule_overview_repair.sql).
//
// Until GH #972 the dashboard's schedule form never sent sections.overview
// and the server read the missing flag as false, so every schedule saved from
// the dashboard stored "overview": false in report_schedules.sections.
//
// report_schedules is ENABLE and FORCE ROW LEVEL SECURITY. cmd/wpmgr applies
// migrations with db.Pool.Migrate on the WPMGR_DB_MIGRATION_DSN pool, which in
// production is a NOSUPERUSER NOBYPASSRLS table owner, and Migrate sets no
// app.* setting. A plain UPDATE backfill run that way reads no rows and
// changes nothing, and a test on an empty database cannot tell. So this test
// migrates as such an owner, seeds rows in two tenants as wpmgr_app through
// InTenantTx and the report repository, boots the real runner as the owner,
// and compares every row before and after as the bootstrap superuser, which
// sees every row. The superuser only reads; it writes nothing and applies no
// migration.
package tests

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/report"
)

// m185MigrationVersion is the embedded migration (without .sql) under test.
const m185MigrationVersion = "20261028000000_m185_report_schedule_overview_repair"

// m185Seed is one seeded report_schedules row and what m185 must do to it.
type m185Seed struct {
	tenant uuid.UUID
	client uuid.UUID
	label  string
	// repair is true when the row stores "overview": false, so m185 must
	// turn it on. Every other row must come through byte for byte.
	repair bool
	// want is what the report repository must read for a repaired row.
	want report.SectionFlags
}

// m185Row is one report_schedules row as the bootstrap superuser reads it.
// Every field is a value, so two rows compare equal exactly when they read
// the same.
type m185Row struct {
	tenant uuid.UUID
	// xmin is the row version. It changes exactly when a statement rewrites
	// the row, so an unchanged xmin proves the migration never wrote it.
	xmin        string
	sections    string // sections::text
	rest        string // (sections - 'overview')::text
	hasOverview bool   // sections carries an "overview" key
	overview    string // (sections -> 'overview')::text when it does
	columns     string // (to_jsonb(row) - 'sections')::text: every other column
}

// m185Snapshot reads every report_schedules row, keyed by client_id, which is
// unique.
func m185Snapshot(t *testing.T, admin *db.Pool) map[uuid.UUID]m185Row {
	t.Helper()
	rows, err := admin.Query(context.Background(), `
		SELECT rs.client_id, rs.tenant_id, rs.xmin::text, rs.sections::text,
		       (rs.sections - 'overview')::text,
		       (rs.sections -> 'overview')::text,
		       (to_jsonb(rs) - 'sections')::text
		FROM report_schedules rs`)
	if err != nil {
		t.Fatalf("snapshot report_schedules: %v", err)
	}
	defer rows.Close()
	out := map[uuid.UUID]m185Row{}
	for rows.Next() {
		var client uuid.UUID
		var r m185Row
		var overview *string
		if err := rows.Scan(&client, &r.tenant, &r.xmin, &r.sections, &r.rest, &overview, &r.columns); err != nil {
			t.Fatalf("scan report_schedules snapshot: %v", err)
		}
		if overview != nil {
			r.hasOverview, r.overview = true, *overview
		}
		out[client] = r
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read report_schedules snapshot: %v", err)
	}
	return out
}

// m185RLSState is report_schedules' row security as the catalog records it:
// whether ENABLE and FORCE are both on, and every policy with its clauses.
func m185RLSState(t *testing.T, admin *db.Pool) (bool, string) {
	t.Helper()
	ctx := context.Background()
	var forced bool
	if err := admin.QueryRow(ctx,
		`SELECT relrowsecurity AND relforcerowsecurity FROM pg_class WHERE oid = 'public.report_schedules'::regclass`,
	).Scan(&forced); err != nil {
		t.Fatalf("read report_schedules row security flags: %v", err)
	}
	var policies string
	if err := admin.QueryRow(ctx, `
		SELECT coalesce(string_agg(
		           policyname || ' ' || permissive || ' ' || cmd || ' USING ' || coalesce(qual, '-')
		           || ' CHECK ' || coalesce(with_check, '-'), E'\n' ORDER BY policyname), '')
		FROM pg_policies WHERE schemaname = 'public' AND tablename = 'report_schedules'`,
	).Scan(&policies); err != nil {
		t.Fatalf("read report_schedules policies: %v", err)
	}
	return forced, policies
}

func m185SeedClient(t *testing.T, app *db.Pool, tenant uuid.UUID, name string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	if err := app.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO clients (id, tenant_id, name) VALUES ($1, $2, $3)`, id, tenant, name)
		return err
	}); err != nil {
		t.Fatalf("seed client %s: %v", name, err)
	}
	return id
}

// m185SeedViaRepo writes a schedule through the report repository, the path
// the schedule PUT takes, so the stored sections are exactly what the server
// wrote: all six keys, as json.Marshal renders report.SectionFlags.
func m185SeedViaRepo(t *testing.T, app *db.Pool, tenant uuid.UUID, label string, flags report.SectionFlags) m185Seed {
	t.Helper()
	client := m185SeedClient(t, app, tenant, label)
	next := time.Now().Add(72 * time.Hour).UTC().Truncate(time.Second)
	if _, err := report.NewRepo(app).UpsertSchedule(context.Background(), report.UpsertScheduleInput{
		TenantID:         tenant,
		ClientID:         client,
		Enabled:          true,
		Cadence:          "monthly",
		SendDay:          3,
		SendHour:         9,
		Recipients:       []string{"owner@" + label + ".example"},
		Sections:         flags,
		IntroText:        "intro for " + label,
		ClosingText:      "closing for " + label,
		PoweredByRemoved: true,
		NextRunAt:        &next,
	}); err != nil {
		t.Fatalf("seed schedule %s through the report repository: %v", label, err)
	}
	return m185Seed{tenant: tenant, client: client, label: label}
}

// m185SeedRaw writes a schedule with sections given verbatim, as wpmgr_app in
// the tenant's transaction, for shapes the repository never writes: a missing
// key, an unknown key, a non-boolean value. A nil sections leaves the column
// to its default.
func m185SeedRaw(t *testing.T, app *db.Pool, tenant uuid.UUID, label string, sections *string) m185Seed {
	t.Helper()
	ctx := context.Background()
	client := m185SeedClient(t, app, tenant, label)
	err := app.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		if sections == nil {
			_, err := tx.Exec(ctx, `
				INSERT INTO report_schedules (tenant_id, client_id, enabled, cadence, send_day, send_hour, recipients, intro_text)
				VALUES ($1, $2, false, 'weekly', 2, 7, '["ops@example.com"]'::jsonb, $3)`,
				tenant, client, "intro for "+label)
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO report_schedules (tenant_id, client_id, enabled, cadence, send_day, send_hour, recipients, sections, intro_text, next_run_at)
			VALUES ($1, $2, true, 'weekly', 2, 7, '["ops@example.com"]'::jsonb, $3::jsonb, $4, now() + interval '1 day')`,
			tenant, client, *sections, "intro for "+label)
		return err
	})
	if err != nil {
		t.Fatalf("seed schedule %s: %v", label, err)
	}
	return m185Seed{tenant: tenant, client: client, label: label}
}

func m185Ptr(s string) *string { return &s }

// TestM185TurnsOverviewBackOnUnderTheProductionMigrator seeds schedules in two
// tenants, three of them storing "overview": false, then applies m185 the way
// production does and checks every row.
func TestM185TurnsOverviewBackOnUnderTheProductionMigrator(t *testing.T) {
	// Every migration that sorts before m185 is applied as the owner, so the
	// rows below are seeded into the schema m185 meets in production.
	d := startPostgresAsOwner(t, func(version string) bool { return version < m185MigrationVersion })
	ctx := context.Background()

	// The premise: neither the migrator nor the application bypasses row
	// security. startPostgresAsOwner checks the owner; this checks wpmgr_app.
	var appSuper, appBypass bool
	if err := d.app.QueryRow(ctx,
		`SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).
		Scan(&appSuper, &appBypass); err != nil {
		t.Fatalf("read wpmgr_app role attributes: %v", err)
	}
	if appSuper || appBypass {
		t.Fatalf("wpmgr_app has rolsuper=%t rolbypassrls=%t; this test's premise is a role row security applies to", appSuper, appBypass)
	}

	tenantA := seedTenant(t, d.app, "m185-agency-a")
	tenantB := seedTenant(t, d.app, "m185-agency-b")

	allOn := report.DefaultSectionFlags()
	var seeds []m185Seed
	add := func(s m185Seed, repair bool, want report.SectionFlags) {
		s.repair, s.want = repair, want
		seeds = append(seeds, s)
	}

	// Tenant A.
	add(m185SeedViaRepo(t, d.app, tenantA, "a-form-save",
		report.SectionFlags{Overview: false, Uptime: true, Backups: false, Updates: true, Performance: false, Email: true}),
		true, report.SectionFlags{Overview: true, Uptime: true, Backups: false, Updates: true, Performance: false, Email: true})
	add(m185SeedRaw(t, d.app, tenantA, "a-false-extra-key",
		m185Ptr(`{"overview": false, "uptime": false, "backups": true, "updates": false, "performance": true, "email": false, "zz_later_flag": {"nested": [1, "two", null, false]}}`)),
		true, report.SectionFlags{Overview: true, Uptime: false, Backups: true, Updates: false, Performance: true, Email: false})
	add(m185SeedRaw(t, d.app, tenantA, "a-true",
		m185Ptr(`{"overview": true, "uptime": false, "backups": false, "updates": true, "performance": true, "email": true}`)),
		false, report.SectionFlags{})
	add(m185SeedRaw(t, d.app, tenantA, "a-no-key", m185Ptr(`{"uptime": false, "backups": true}`)), false, report.SectionFlags{})
	add(m185SeedRaw(t, d.app, tenantA, "a-default-empty", nil), false, report.SectionFlags{})
	add(m185SeedRaw(t, d.app, tenantA, "a-null", m185Ptr(`{"overview": null, "uptime": false}`)), false, report.SectionFlags{})
	add(m185SeedRaw(t, d.app, tenantA, "a-string-false", m185Ptr(`{"overview": "false", "email": false}`)), false, report.SectionFlags{})
	add(m185SeedRaw(t, d.app, tenantA, "a-not-an-object", m185Ptr(`[]`)), false, report.SectionFlags{})

	// Tenant B.
	formOff := allOn
	formOff.Overview = false
	add(m185SeedViaRepo(t, d.app, tenantB, "b-form-save", formOff), true, allOn)
	add(m185SeedViaRepo(t, d.app, tenantB, "b-form-save-on",
		report.SectionFlags{Overview: true, Uptime: false, Backups: true, Updates: true, Performance: true, Email: false}),
		false, report.SectionFlags{})
	add(m185SeedRaw(t, d.app, tenantB, "b-no-key", m185Ptr(`{"email": false}`)), false, report.SectionFlags{})

	wantRepaired := 0
	for _, s := range seeds {
		if s.repair {
			wantRepaired++
		}
	}

	before := m185Snapshot(t, d.admin)
	if len(before) != len(seeds) {
		t.Fatalf("superuser sees %d report_schedules rows, seeded %d; this test's premise is wrong", len(before), len(seeds))
	}
	var falseRows int
	for _, r := range before {
		if r.hasOverview && r.overview == "false" {
			falseRows++
		}
	}
	if falseRows != wantRepaired {
		t.Fatalf("superuser sees %d rows storing overview false, seeded %d; this test's premise is wrong", falseRows, wantRepaired)
	}
	forcedBefore, policiesBefore := m185RLSState(t, d.admin)
	if !forcedBefore {
		t.Fatalf("report_schedules is not ENABLE and FORCE ROW LEVEL SECURITY before m185; this test's premise is wrong")
	}

	// The trap, reproduced. As the owner, with no app.* setting, FORCE ROW
	// LEVEL SECURITY hides every row, so the plain UPDATE a backfill would
	// write changes nothing. If the owner ever sees a row here, the harness no
	// longer reproduces the production migrator and the proof below is void.
	var ownerSees int
	if err := d.owner.QueryRow(ctx, `SELECT count(*) FROM report_schedules`).Scan(&ownerSees); err != nil {
		t.Fatalf("count report_schedules as the owner: %v", err)
	}
	if ownerSees != 0 {
		t.Fatalf("the owner sees %d report_schedules rows with no app.* setting, want 0; the harness does not reproduce the production migrator", ownerSees)
	}
	errRollBack := errors.New("roll the plain backfill back")
	var plainUpdated int64 = -1
	err := pgx.BeginFunc(ctx, d.owner.Pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE report_schedules
			SET sections = jsonb_set(sections, '{overview}', 'true'::jsonb, false)
			WHERE sections -> 'overview' = 'false'::jsonb`)
		if err != nil {
			return err
		}
		plainUpdated = tag.RowsAffected()
		return errRollBack
	})
	if !errors.Is(err, errRollBack) {
		t.Fatalf("plain UPDATE as the owner: %v", err)
	}
	if plainUpdated != 0 {
		t.Fatalf("a plain UPDATE as the owner changed %d rows, want 0; the harness does not reproduce the production migrator", plainUpdated)
	}

	// Apply m185 the way cmd/wpmgr does: the real runner, as the owner.
	if err := d.owner.Migrate(ctx); err != nil {
		t.Fatalf("Migrate as the owner: %v", err)
	}

	after := m185Snapshot(t, d.admin)
	if len(after) != len(before) {
		t.Fatalf("report_schedules holds %d rows after m185, %d before", len(after), len(before))
	}

	changed := 0
	for _, s := range seeds {
		b, inBefore := before[s.client]
		a, inAfter := after[s.client]
		if !inBefore || !inAfter {
			t.Fatalf("%s: no report_schedules row for client %s (before %t, after %t)", s.label, s.client, inBefore, inAfter)
		}
		if b.tenant != s.tenant {
			t.Fatalf("%s: seeded row belongs to tenant %s, want %s; this test's premise is wrong", s.label, b.tenant, s.tenant)
		}
		if a.xmin != b.xmin {
			changed++
		}
		if !s.repair {
			if a != b {
				t.Errorf("%s: row changed, want it untouched:\n before %+v\n after  %+v", s.label, b, a)
			}
			continue
		}
		if !b.hasOverview || b.overview != "false" || strings.Count(b.sections, `"overview": false`) != 1 {
			t.Fatalf("%s: seeded sections %s do not store overview false once; this test's premise is wrong", s.label, b.sections)
		}
		if !a.hasOverview || a.overview != "true" {
			t.Errorf("%s: overview after m185 = %q (present %t), want true; sections %s", s.label, a.overview, a.hasOverview, a.sections)
		}
		if want := strings.Replace(b.sections, `"overview": false`, `"overview": true`, 1); a.sections != want {
			t.Errorf("%s: sections after m185\n got  %s\n want %s", s.label, a.sections, want)
		}
		if a.rest != b.rest {
			t.Errorf("%s: sections other than overview changed:\n before %s\n after  %s", s.label, b.rest, a.rest)
		}
		if a.columns != b.columns {
			t.Errorf("%s: a column other than sections changed:\n before %s\n after  %s", s.label, b.columns, a.columns)
		}
		if a.tenant != b.tenant {
			t.Errorf("%s: tenant changed from %s to %s", s.label, b.tenant, a.tenant)
		}
	}
	t.Logf("m185 changed %d of %d report_schedules rows across 2 tenants; %d were seeded storing overview false", changed, len(after), wantRepaired)
	if changed != wantRepaired {
		t.Errorf("m185 changed %d rows, want %d (the rows seeded storing overview false)", changed, wantRepaired)
	}
	var stillFalse int
	if err := d.admin.QueryRow(ctx,
		`SELECT count(*) FROM report_schedules WHERE sections -> 'overview' = 'false'::jsonb`).Scan(&stillFalse); err != nil {
		t.Fatalf("count rows still storing overview false: %v", err)
	}
	if stillFalse != 0 {
		t.Errorf("%d report_schedules rows still store overview false after m185, want 0", stillFalse)
	}

	// What a report reads: the repository, as wpmgr_app, in each tenant.
	repo := report.NewRepo(d.app)
	for _, s := range seeds {
		if !s.repair {
			continue
		}
		got, found, err := repo.GetSchedule(ctx, s.tenant, s.client)
		if err != nil || !found {
			t.Errorf("%s: GetSchedule as wpmgr_app: found=%t err=%v", s.label, found, err)
			continue
		}
		if got.Sections != s.want {
			t.Errorf("%s: repository reads sections %+v, want %+v", s.label, got.Sections, s.want)
		}
	}

	var recorded int
	if err := d.admin.QueryRow(ctx,
		`SELECT count(*) FROM schema_migrations WHERE version = $1`, m185MigrationVersion).Scan(&recorded); err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	if recorded != 1 {
		t.Errorf("schema_migrations holds %d rows for %s after Migrate, want 1", recorded, m185MigrationVersion)
	}
	if t.Failed() {
		t.FailNow()
	}

	// Row security is as it was: ENABLE and FORCE, the same policies, the
	// owner still sees nothing, and each tenant sees exactly its own rows.
	forcedAfter, policiesAfter := m185RLSState(t, d.admin)
	if !forcedAfter {
		t.Errorf("report_schedules lost ENABLE or FORCE ROW LEVEL SECURITY across m185")
	}
	if policiesAfter != policiesBefore {
		t.Errorf("report_schedules policies changed across m185:\n before %s\n after  %s", policiesBefore, policiesAfter)
	}
	if err := d.owner.QueryRow(ctx, `SELECT count(*) FROM report_schedules`).Scan(&ownerSees); err != nil {
		t.Fatalf("count report_schedules as the owner after m185: %v", err)
	}
	if ownerSees != 0 {
		t.Errorf("the owner sees %d report_schedules rows after m185, want 0: FORCE ROW LEVEL SECURITY is not back in force", ownerSees)
	}
	for _, tenant := range []uuid.UUID{tenantA, tenantB} {
		want := 0
		for _, s := range seeds {
			if s.tenant == tenant {
				want++
			}
		}
		var got int
		if err := d.app.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM report_schedules`).Scan(&got)
		}); err != nil {
			t.Fatalf("count report_schedules as wpmgr_app in tenant %s: %v", tenant, err)
		}
		if got != want {
			t.Errorf("wpmgr_app in tenant %s sees %d report_schedules rows, want %d", tenant, got, want)
		}
	}

	// A second run changes nothing: not a value, not a row version.
	scopePrefillUnmark(t, d.owner, m185MigrationVersion)
	if err := d.owner.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate as the owner: %v", err)
	}
	again := m185Snapshot(t, d.admin)
	if len(again) != len(after) {
		t.Errorf("report_schedules holds %d rows after the second run, %d after the first", len(again), len(after))
	}
	for client, r := range after {
		if again[client] != r {
			t.Errorf("schedule for client %s changed on the second run:\n first  %+v\n second %+v", client, r, again[client])
		}
	}
}
