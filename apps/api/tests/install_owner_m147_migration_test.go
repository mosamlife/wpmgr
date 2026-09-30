// install_owner_m147_migration_test.go: m147 creates install_owner and
// backfills it from the earliest bootstrap audit_log row. These tests apply it
// the way a real install does, by a table owner that is neither a superuser nor
// BYPASSRLS, against a database that already holds audit rows, and then try to
// tamper with the result as wpmgr_app, the role every API path runs as.
//
// Two migrator setups are covered, because both exist in production:
//   - a separate owner role (wpmgr_owner) with wpmgr_app as a grantee, the
//     WPMGR_DB_MIGRATION_DSN setup;
//   - wpmgr_app as the table owner and the migrator, the single-DSN setup.
//
// Seed rows are written as the bootstrap superuser because the tests need
// exact created_at values. That does not weaken the proof: what is under test
// is what the migrating role can read, and the premise checks below show that
// role sees none of those rows without the setting m147 applies.
package tests

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mosamlife/wpmgr/apps/api/internal/auth"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/migrations"
)

const m147InstallOwner = "20260910000000_m147_install_owner"

// beforeM147 applies everything that sorts before m147: the schema every
// install has when this migration first boots.
func beforeM147(version string) bool { return version < m147InstallOwner }

type m147OwnerRow struct {
	UserID     uuid.UUID
	TenantID   uuid.UUID
	Source     string
	RecordedAt time.Time
}

func (r m147OwnerRow) same(o m147OwnerRow) bool {
	return r.UserID == o.UserID && r.TenantID == o.TenantID &&
		r.Source == o.Source && r.RecordedAt.Equal(o.RecordedAt)
}

// m147Row reads install_owner as the superuser. found is false when the table
// holds no row. It fails the test if the table holds more than one.
func m147Row(t *testing.T, admin *db.Pool) (m147OwnerRow, bool) {
	t.Helper()
	ctx := context.Background()
	if n := scopePrefillCount(t, admin, `SELECT count(*) FROM install_owner`); n > 1 {
		t.Fatalf("install_owner holds %d rows; the singleton key should make that impossible", n)
	}
	var r m147OwnerRow
	err := admin.QueryRow(ctx,
		`SELECT user_id, tenant_id, source, recorded_at FROM install_owner`).
		Scan(&r.UserID, &r.TenantID, &r.Source, &r.RecordedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return m147OwnerRow{}, false
	}
	if err != nil {
		t.Fatalf("read install_owner: %v", err)
	}
	return r, true
}

// m147SeedAudit writes one audit_log row with an exact created_at.
func m147SeedAudit(t *testing.T, admin *db.Pool, tenant uuid.UUID, actor, target, metadata string, at time.Time) {
	t.Helper()
	if _, err := admin.Exec(context.Background(),
		`INSERT INTO audit_log (tenant_id, actor_type, actor_id, action, target_type, target_id, metadata, hash, created_at)
		 VALUES ($1, 'user', $2, 'auth.register', 'user', $3, $4::jsonb, 'seed', $5)`,
		tenant, actor, target, metadata, at); err != nil {
		t.Fatalf("seed audit_log row: %v", err)
	}
}

// m147SeedBootstrapAudit writes the row first-run bootstrap writes
// (internal/auth/service.go): actor and target are the user, metadata
// {"bootstrap": true}.
func m147SeedBootstrapAudit(t *testing.T, admin *db.Pool, tenant, user uuid.UUID, at time.Time) {
	t.Helper()
	m147SeedAudit(t, admin, tenant, user.String(), user.String(), `{"bootstrap": true}`, at)
}

// m147AssertAuditHiddenFromMigrator is the premise every backfill test rests
// on: the migrating role, with no setting in scope, reads none of the audit
// rows the superuser can see. If this ever fails, the role is not subject to
// row security and a backfill that forgot app.agent would pass here.
func m147AssertAuditHiddenFromMigrator(t *testing.T, admin, migrator *db.Pool) {
	t.Helper()
	all := scopePrefillCount(t, admin, `SELECT count(*) FROM audit_log`)
	if all == 0 {
		t.Fatal("no audit_log rows seeded; this test's premise is wrong")
	}
	if seen := scopePrefillCount(t, migrator, `SELECT count(*) FROM audit_log`); seen != 0 {
		t.Fatalf("the migrating role reads %d of %d audit_log rows with no setting in scope, want 0; "+
			"it is not subject to row security, so this test proves nothing", seen, all)
	}
}

// m147RerunBody executes m147's file body a second time, as pool's role, in
// one transaction, without touching schema_migrations. The runner never does
// this; it is how the file's own idempotency is checked.
func m147RerunBody(t *testing.T, pool *db.Pool) {
	t.Helper()
	ctx := context.Background()
	body, err := migrations.FS.ReadFile(m147InstallOwner + ".sql")
	if err != nil {
		t.Fatalf("read m147: %v", err)
	}
	if err := pgx.BeginFunc(ctx, pool.Pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, string(body))
		return err
	}); err != nil {
		t.Fatalf("re-run m147 body: %v", err)
	}
}

// m147AssertTamperRefused tries, as wpmgr_app, every write an API path could
// issue against install_owner, and requires each to be refused or to change
// nothing. The row must be byte-for-byte the same afterwards.
func m147AssertTamperRefused(t *testing.T, admin, app *db.Pool) {
	t.Helper()
	ctx := context.Background()

	before, ok := m147Row(t, admin)
	if !ok {
		t.Fatal("install_owner has no row to tamper with; this test's premise is wrong")
	}
	intruder := uuid.New()
	otherTenant := uuid.New()

	wantCode := func(name, code, q string, args ...any) {
		t.Helper()
		_, err := app.Exec(ctx, q, args...)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != code {
			t.Errorf("%s as wpmgr_app: got %v, want SQLSTATE %s", name, err, code)
		}
	}

	// Re-pointing the record.
	wantCode("UPDATE", "42501",
		`UPDATE install_owner SET user_id = $1, tenant_id = $2`, intruder, otherTenant)
	wantCode("INSERT ... ON CONFLICT DO UPDATE", "42501",
		`INSERT INTO install_owner (singleton, user_id, tenant_id, source) VALUES (true, $1, $2, 'bootstrap')
		 ON CONFLICT (singleton) DO UPDATE SET user_id = EXCLUDED.user_id, tenant_id = EXCLUDED.tenant_id`,
		intruder, otherTenant)

	// Removing it.
	wantCode("DELETE", "42501", `DELETE FROM install_owner`)
	wantCode("TRUNCATE", "42501", `TRUNCATE install_owner`)

	// A second row, by either value of the key.
	wantCode("second INSERT", "23505",
		`INSERT INTO install_owner (singleton, user_id, tenant_id, source) VALUES (true, $1, $2, 'bootstrap')`,
		intruder, otherTenant)
	wantCode("INSERT with singleton false", "23514",
		`INSERT INTO install_owner (singleton, user_id, tenant_id, source) VALUES (false, $1, $2, 'bootstrap')`,
		intruder, otherTenant)

	// The form bootstrap uses succeeds and changes nothing.
	tag, err := app.Exec(ctx,
		`INSERT INTO install_owner (singleton, user_id, tenant_id, source) VALUES (true, $1, $2, 'bootstrap')
		 ON CONFLICT (singleton) DO NOTHING`, intruder, otherTenant)
	if err != nil {
		t.Errorf("INSERT ... ON CONFLICT DO NOTHING as wpmgr_app: %v", err)
	} else if tag.RowsAffected() != 0 {
		t.Errorf("INSERT ... ON CONFLICT DO NOTHING as wpmgr_app affected %d rows, want 0", tag.RowsAffected())
	}

	after, ok := m147Row(t, admin)
	if !ok {
		t.Fatal("install_owner row is gone after the tamper attempts")
	}
	if !after.same(before) {
		t.Fatalf("install_owner changed under wpmgr_app: before %+v, after %+v", before, after)
	}
}

// TestInstallOwnerM147_EarliestBootstrapRowWins covers the backfill under the
// separate-owner migrator: the earliest bootstrap row is recorded even though
// its user has been deleted, the next one does not inherit, a second run
// changes nothing, a malformed earliest row records nothing rather than
// falling through, and wpmgr_app cannot alter the result.
func TestInstallOwnerM147_EarliestBootstrapRowWins(t *testing.T) {
	d := startPostgresAsOwner(t, beforeM147)
	ctx := context.Background()

	if scopePrefillCount(t, d.admin, `SELECT count(*) FROM pg_class WHERE oid = to_regclass('public.install_owner')`) != 0 {
		t.Fatal("install_owner exists before m147 applied; this test's premise is wrong")
	}

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tenantA := seedTenant(t, d.admin, "m147-a-"+uuid.NewString()[:8])
	tenantB := seedTenant(t, d.admin, "m147-b-"+uuid.NewString()[:8])
	userA := seedUserRow(t, d.admin, "m147-a-"+uuid.NewString()[:8]+"@example.test")
	userB := seedUserRow(t, d.admin, "m147-b-"+uuid.NewString()[:8]+"@example.test")
	selfServe := seedUserRow(t, d.admin, "m147-s-"+uuid.NewString()[:8]+"@example.test")

	// A self-serve registration older than every bootstrap row: it is not
	// proof of setting up the install and must be ignored.
	m147SeedAudit(t, d.admin, tenantB, selfServe.String(), selfServe.String(),
		`{"self_serve": true}`, base.Add(-time.Hour))
	// A bootstrap-shaped row whose actor is not its target: not the row
	// bootstrap writes, so ignored too.
	m147SeedAudit(t, d.admin, tenantB, selfServe.String(), userB.String(),
		`{"bootstrap": true}`, base.Add(-30*time.Minute))
	// The first bootstrap, then a later one (an install whose owners were all
	// removed and which was bootstrapped again).
	m147SeedBootstrapAudit(t, d.admin, tenantA, userA, base)
	m147SeedBootstrapAudit(t, d.admin, tenantB, userB, base.Add(24*time.Hour))

	// The first account is deleted before m147 ever runs.
	if _, err := d.admin.Exec(ctx, `DELETE FROM users WHERE id = $1`, userA); err != nil {
		t.Fatalf("delete first user: %v", err)
	}
	if n := scopePrefillCount(t, d.admin, `SELECT count(*) FROM users WHERE id = $1`, userA); n != 0 {
		t.Fatalf("first user still present (%d rows); this test's premise is wrong", n)
	}

	m147AssertAuditHiddenFromMigrator(t, d.admin, d.owner)

	if err := d.owner.Migrate(ctx); err != nil {
		t.Fatalf("migrate as wpmgr_owner: %v", err)
	}

	got, ok := m147Row(t, d.admin)
	if !ok {
		t.Fatal("m147 recorded no install owner; the earliest bootstrap row was not read " +
			"(was audit_log visible to the migrating role?)")
	}
	if got.UserID != userA {
		switch got.UserID {
		case userB:
			t.Fatalf("m147 recorded the SECOND bootstrap user %s, want the first %s: "+
				"the earliest row must be chosen before anything checks whether its user still exists", userB, userA)
		default:
			t.Fatalf("m147 recorded user %s, want the first bootstrap user %s", got.UserID, userA)
		}
	}
	if got.TenantID != tenantA || got.Source != "backfill" {
		t.Fatalf("m147 row = %+v, want tenant %s and source backfill", got, tenantA)
	}

	// A second run over a database that now holds an even earlier bootstrap
	// row changes nothing: a recorded owner is never replaced.
	userC := seedUserRow(t, d.admin, "m147-c-"+uuid.NewString()[:8]+"@example.test")
	m147SeedBootstrapAudit(t, d.admin, tenantB, userC, base.Add(-48*time.Hour))
	m147RerunBody(t, d.owner)
	if again, ok := m147Row(t, d.admin); !ok || !again.same(got) {
		t.Fatalf("second run of m147 changed install_owner: before %+v, after %+v (present=%t)", got, again, ok)
	}

	m147AssertTamperRefused(t, d.admin, d.app)

	// A malformed actor id on the earliest row records nothing. It does not
	// fall through to the next row. The table is emptied as the superuser,
	// which no API path can do, only to set up this case.
	if _, err := d.admin.Exec(ctx, `DELETE FROM install_owner`); err != nil {
		t.Fatalf("empty install_owner as superuser: %v", err)
	}
	m147SeedAudit(t, d.admin, tenantB, "not-a-uuid", "not-a-uuid", `{"bootstrap": true}`, base.Add(-72*time.Hour))
	m147RerunBody(t, d.owner)
	if row, ok := m147Row(t, d.admin); ok {
		t.Fatalf("m147 recorded %+v although the earliest bootstrap row's actor is not a uuid; "+
			"it must record nothing rather than fall through to a later row", row)
	}
}

// TestInstallOwnerM147_NoBootstrapRowRecordsNothing: with no bootstrap audit
// row, m147 records no owner, never the earliest user or owner in its place.
// The bootstrap path (wpmgr_app, one insert under the install lock) can then
// record one, exactly once.
func TestInstallOwnerM147_NoBootstrapRowRecordsNothing(t *testing.T) {
	d := startPostgresAsOwner(t, beforeM147)
	ctx := context.Background()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tenantX := seedTenant(t, d.admin, "m147-x-"+uuid.NewString()[:8])
	tenantY := seedTenant(t, d.admin, "m147-y-"+uuid.NewString()[:8])
	userX := seedUserRow(t, d.admin, "m147-x-"+uuid.NewString()[:8]+"@example.test")
	userY := seedUserRow(t, d.admin, "m147-y-"+uuid.NewString()[:8]+"@example.test")
	seedMembershipRow(t, d.admin, userX, tenantX)
	seedMembershipRow(t, d.admin, userY, tenantY)
	m147SeedAudit(t, d.admin, tenantX, userX.String(), userX.String(),
		`{"self_serve": true}`, base)
	m147SeedAudit(t, d.admin, tenantY, userY.String(), userY.String(),
		`{"self_serve": true}`, base.Add(time.Hour))

	m147AssertAuditHiddenFromMigrator(t, d.admin, d.owner)

	if err := d.owner.Migrate(ctx); err != nil {
		t.Fatalf("migrate as wpmgr_owner: %v", err)
	}
	if row, ok := m147Row(t, d.admin); ok {
		t.Fatalf("m147 recorded %+v with no bootstrap audit row; it must record nothing", row)
	}

	// The bootstrap write: first insert lands, under the same lock and role
	// BootstrapInstall uses.
	first := func() (int64, error) {
		var n int64
		err := d.app.InInstallLockTx(ctx, auth.InstallBootstrapLockKey,
			func(tx pgx.Tx, _ func(uuid.UUID) error) error {
				tag, err := tx.Exec(ctx,
					`INSERT INTO install_owner (singleton, user_id, tenant_id, source)
					 VALUES (true, $1, $2, 'bootstrap') ON CONFLICT (singleton) DO NOTHING`, userX, tenantX)
				n = tag.RowsAffected()
				return err
			})
		return n, err
	}
	if n, err := first(); err != nil || n != 1 {
		t.Fatalf("bootstrap insert into an empty install_owner as wpmgr_app: rows=%d err=%v, want 1 row", n, err)
	}
	if row, ok := m147Row(t, d.admin); !ok || row.UserID != userX || row.TenantID != tenantX || row.Source != "bootstrap" {
		t.Fatalf("install_owner after bootstrap insert = %+v (present=%t), want user %s tenant %s source bootstrap",
			row, ok, userX, tenantX)
	}

	m147AssertTamperRefused(t, d.admin, d.app)
}

// TestInstallOwnerM147_AppRoleAsTableOwner covers the single-DSN setup:
// wpmgr_app owns every table and runs the migrations itself. The backfill
// must still see audit_log, and the REVOKE must still bind wpmgr_app although
// it owns install_owner.
//
// The schema before m147 is built by wpmgr_owner and then handed to wpmgr_app
// with REASSIGN OWNED, so the tables m147 depends on are owned by wpmgr_app
// and m147 itself runs as wpmgr_app, creating install_owner as that role.
func TestInstallOwnerM147_AppRoleAsTableOwner(t *testing.T) {
	d := startPostgresAsOwner(t, beforeM147)
	ctx := context.Background()

	if _, err := d.admin.Exec(ctx, `REASSIGN OWNED BY wpmgr_owner TO wpmgr_app`); err != nil {
		t.Fatalf("hand the database to wpmgr_app: %v", err)
	}

	var super, bypass bool
	if err := d.app.QueryRow(ctx,
		`SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&super, &bypass); err != nil {
		t.Fatalf("read wpmgr_app attributes: %v", err)
	}
	if super || bypass {
		t.Fatalf("wpmgr_app has rolsuper=%t rolbypassrls=%t; this test's premise is a role row security applies to", super, bypass)
	}
	var auditOwner string
	if err := d.admin.QueryRow(ctx,
		`SELECT tableowner FROM pg_tables WHERE schemaname = 'public' AND tablename = 'audit_log'`).Scan(&auditOwner); err != nil {
		t.Fatalf("read audit_log owner: %v", err)
	}
	if auditOwner != "wpmgr_app" {
		t.Fatalf("audit_log is owned by %q, want wpmgr_app; this test's premise is wrong", auditOwner)
	}

	tenantA := seedTenant(t, d.admin, "m147-owner-"+uuid.NewString()[:8])
	userA := seedUserRow(t, d.admin, "m147-owner-"+uuid.NewString()[:8]+"@example.test")
	seedMembershipRow(t, d.admin, userA, tenantA)
	m147SeedBootstrapAudit(t, d.admin, tenantA, userA, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))

	m147AssertAuditHiddenFromMigrator(t, d.admin, d.app)

	if err := d.app.Migrate(ctx); err != nil {
		t.Fatalf("migrate as wpmgr_app: %v", err)
	}

	var owner string
	if err := d.admin.QueryRow(ctx,
		`SELECT tableowner FROM pg_tables WHERE schemaname = 'public' AND tablename = 'install_owner'`).Scan(&owner); err != nil {
		t.Fatalf("read install_owner owner: %v", err)
	}
	if owner != "wpmgr_app" {
		t.Fatalf("install_owner is owned by %q, want wpmgr_app; this test's premise is wrong", owner)
	}

	got, ok := m147Row(t, d.admin)
	if !ok {
		t.Fatal("m147 run as wpmgr_app recorded no install owner; the bootstrap audit row was not read")
	}
	if got.UserID != userA || got.TenantID != tenantA || got.Source != "backfill" {
		t.Fatalf("m147 row = %+v, want user %s tenant %s source backfill", got, userA, tenantA)
	}

	m147AssertTamperRefused(t, d.admin, d.app)
}
