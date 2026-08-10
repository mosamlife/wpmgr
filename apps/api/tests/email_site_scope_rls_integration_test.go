// email_site_scope_rls_integration_test.go: m112 (GH #380). The email
// domain's site-scope isolation, asserted against the REAL schema (migrations
// + RLS) as a NON-SUPERUSER role.
//
// WHY THESE TESTS ARE HERE AND NOT IN internal/email.
//
// Three review rounds on this issue closed seven privilege-escalation doors,
// each one in a handler, and a fourth round would have found an eighth. The
// tests those rounds produced all assert against fakes, so they prove what the
// handler does and say nothing about what the DATABASE permits. m112 moved the
// boundary into the database, so the proof has to run against the database:
// a fake cannot fail to have a policy.
//
// Everything below therefore goes AROUND the repo and queries the tables
// directly under the exact GUC combinations the real transaction wrappers set
// (db.Pool.InScopedTenantTx for a site-scoped collaborator, InTenantTx for an
// org member, InAgentTx for the agent). The tests are written as the ATTACK,
// not as the feature: the question each one asks is "can this principal reach
// this row", answered by Postgres.
//
// The non-superuser role matters and is not incidental. A superuser, or any
// role with BYPASSRLS, ignores every policy in this file and all of it would
// pass vacuously; startPostgres provisions the plain wpmgr_app role for
// exactly this reason.
package tests

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/migrations"
)

// emailScopeFixture is one tenant with an ORG-WIDE email config (site_id IS
// NULL, the row that inheritance serves and that every door tried to write),
// two sites, and a per-site config on siteA only. siteB deliberately has no
// config row of its own, so it is a genuinely INHERITING site.
type emailScopeFixture struct {
	tenant       uuid.UUID
	siteA, siteB uuid.UUID
	orgConfig    uuid.UUID
	siteAConfig  uuid.UUID
	orgConn      uuid.UUID
	siteAConn    uuid.UUID
	orgSuppress  uuid.UUID
}

// seedEmailScopeFixture seeds via the superuser pool. Seeding is out-of-band
// setup; every assertion below runs as the app role.
func seedEmailScopeFixture(t *testing.T, app *db.Pool) emailScopeFixture {
	t.Helper()
	ctx := context.Background()
	admin := connectAdmin(t, app)
	defer admin.Close()

	f := emailScopeFixture{
		siteA: uuid.New(), siteB: uuid.New(),
	}
	if err := admin.QueryRow(ctx,
		"INSERT INTO tenants (name, slug) VALUES ('email-scope', 'email-scope') RETURNING id").
		Scan(&f.tenant); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	for _, s := range []struct {
		id  uuid.UUID
		url string
	}{{f.siteA, "https://a.email.example"}, {f.siteB, "https://b.email.example"}} {
		if _, err := admin.Exec(ctx,
			"INSERT INTO sites (id, tenant_id, url, name) VALUES ($1, $2, $3, $3)",
			s.id, f.tenant, s.url); err != nil {
			t.Fatalf("seed site: %v", err)
		}
	}

	// The ORG-WIDE row: site_id IS NULL. This is the row every door led to.
	if err := admin.QueryRow(ctx,
		`INSERT INTO site_email_config (tenant_id, site_id, provider, from_address, config, provider_secret_encrypted)
		 VALUES ($1, NULL, 'smtp', 'fleet@example.com', '{"host":"smtp.org-relay.example","username":"fleet"}'::jsonb, '\x4f5247')
		 RETURNING id`, f.tenant).Scan(&f.orgConfig); err != nil {
		t.Fatalf("seed org config: %v", err)
	}
	// siteA's own row.
	if err := admin.QueryRow(ctx,
		`INSERT INTO site_email_config (tenant_id, site_id, provider, from_address, config, provider_secret_encrypted)
		 VALUES ($1, $2, 'smtp', 'a@example.com', '{"host":"smtp.a.example","username":"a"}'::jsonb, '\x534954454141')
		 RETURNING id`, f.tenant, f.siteA).Scan(&f.siteAConfig); err != nil {
		t.Fatalf("seed siteA config: %v", err)
	}
	// A named connection under each config. These carry their own
	// provider_secret_encrypted, which is why the child table needed gating too.
	if err := admin.QueryRow(ctx,
		`INSERT INTO site_email_connection (tenant_id, config_id, connection_key, provider, config, provider_secret_encrypted)
		 VALUES ($1, $2, 'billing', 'smtp', '{"host":"smtp.org-relay.example"}'::jsonb, '\x4f52474330')
		 RETURNING id`, f.tenant, f.orgConfig).Scan(&f.orgConn); err != nil {
		t.Fatalf("seed org connection: %v", err)
	}
	if err := admin.QueryRow(ctx,
		`INSERT INTO site_email_connection (tenant_id, config_id, connection_key, provider, config)
		 VALUES ($1, $2, 'billing', 'smtp', '{"host":"smtp.a.example"}'::jsonb)
		 RETURNING id`, f.tenant, f.siteAConfig).Scan(&f.siteAConn); err != nil {
		t.Fatalf("seed siteA connection: %v", err)
	}
	// A FLEET-WIDE suppression entry (site_id IS NULL): what stops the whole
	// organisation mailing an address that complained.
	if err := admin.QueryRow(ctx,
		`INSERT INTO email_suppression (tenant_id, site_id, email_hash, reason)
		 VALUES ($1, NULL, '\xdeadbeef', 'complaint')
		 RETURNING id`, f.tenant).Scan(&f.orgSuppress); err != nil {
		t.Fatalf("seed fleet suppression: %v", err)
	}
	// One log row per site, so cross-site log reads can be probed.
	for _, sid := range []uuid.UUID{f.siteA, f.siteB} {
		if _, err := admin.Exec(ctx,
			`INSERT INTO site_email_log (tenant_id, site_id, subject, status)
			 VALUES ($1, $2, 'hello', 'sent')`, f.tenant, sid); err != nil {
			t.Fatalf("seed log: %v", err)
		}
	}
	return f
}

// asCollaborator runs fn under the exact GUCs db.Pool.InScopedTenantTx sets for
// a site-scoped collaborator granted only the listed sites.
func asCollaborator(t *testing.T, pool *db.Pool, tenant uuid.UUID, sites []uuid.UUID, fn func(tx pgx.Tx)) {
	t.Helper()
	err := pool.InScopedTenantTx(context.Background(), tenant, uuid.Nil, sites, func(tx pgx.Tx) error {
		fn(tx)
		return nil
	})
	if err != nil {
		t.Fatalf("scoped tx: %v", err)
	}
}

// orgSecretCiphertext reads the org row's stored credential as the superuser,
// which is the only way to see the ground truth after an attack.
func orgSecretCiphertext(t *testing.T, app *db.Pool, configID uuid.UUID) []byte {
	t.Helper()
	admin := connectAdmin(t, app)
	defer admin.Close()
	var ct []byte
	if err := admin.QueryRow(context.Background(),
		"SELECT provider_secret_encrypted FROM site_email_config WHERE id = $1", configID).Scan(&ct); err != nil {
		t.Fatalf("read org ciphertext: %v", err)
	}
	return ct
}

// TestInheritingReadOfTheOrgConfigStillWorks is the compatibility half, and it
// is deliberately FIRST. Inheritance is a shipped feature: a site with no
// config of its own sends with the organisation's, and GET /email/config
// legitimately surfaces that row to a site-scoped collaborator. A policy that
// closed the write doors by hiding the org row would break the product, and
// would look like a security win while doing it.
func TestInheritingReadOfTheOrgConfigStillWorks(t *testing.T) {
	pool := startPostgres(t)
	f := seedEmailScopeFixture(t, pool)

	// A collaborator granted ONLY siteB, which has no config row of its own.
	asCollaborator(t, pool, f.tenant, []uuid.UUID{f.siteB}, func(tx pgx.Tx) {
		var host string
		err := tx.QueryRow(context.Background(),
			`SELECT config->>'host' FROM site_email_config WHERE id = $1`, f.orgConfig).Scan(&host)
		if err != nil {
			t.Fatalf("a site-scoped collaborator on an INHERITING site must still be able "+
				"to READ the org config their site actually sends with: %v", err)
		}
		if host != "smtp.org-relay.example" {
			t.Fatalf("org config host = %q, want the seeded org relay", host)
		}
	})
}

// TestInheritingReadOfOrgConnectionsStillWorks is the same guarantee for the
// connection registry, which listConnections surfaces for an inheriting site.
func TestInheritingReadOfOrgConnectionsStillWorks(t *testing.T) {
	pool := startPostgres(t)
	f := seedEmailScopeFixture(t, pool)

	asCollaborator(t, pool, f.tenant, []uuid.UUID{f.siteB}, func(tx pgx.Tx) {
		var n int
		if err := tx.QueryRow(context.Background(),
			`SELECT count(*) FROM site_email_connection WHERE id = $1`, f.orgConn).Scan(&n); err != nil {
			t.Fatalf("count org connection: %v", err)
		}
		if n != 1 {
			t.Fatal("a site-scoped collaborator must still be able to READ the org's " +
				"named connections, which is what their inheriting site sends with")
		}
	})
}

// TestFleetSuppressionIsStillReadable: the pre-send check matches site_id IS
// NULL OR site_id = @site_id, so fleet-wide entries must remain visible or a
// collaborator's site would start mailing addresses the organisation suppressed.
func TestFleetSuppressionIsStillReadable(t *testing.T) {
	pool := startPostgres(t)
	f := seedEmailScopeFixture(t, pool)

	asCollaborator(t, pool, f.tenant, []uuid.UUID{f.siteB}, func(tx pgx.Tx) {
		var n int
		if err := tx.QueryRow(context.Background(),
			`SELECT count(*) FROM email_suppression WHERE id = $1`, f.orgSuppress).Scan(&n); err != nil {
			t.Fatalf("count fleet suppression: %v", err)
		}
		if n != 1 {
			t.Fatal("fleet-wide suppression entries must stay readable to a site-scoped principal")
		}
	})
}

// TestCollaboratorCannotUpdateTheOrgConfig is THE attack. It is the terminal
// step of every one of the seven doors: whatever route was used to get here,
// the write itself must not land on the organisation's row.
func TestCollaboratorCannotUpdateTheOrgConfig(t *testing.T) {
	pool := startPostgres(t)
	f := seedEmailScopeFixture(t, pool)
	before := orgSecretCiphertext(t, pool, f.orgConfig)

	asCollaborator(t, pool, f.tenant, []uuid.UUID{f.siteB}, func(tx pgx.Tx) {
		tag, err := tx.Exec(context.Background(),
			`UPDATE site_email_config
			    SET config = '{"host":"smtp.attacker.example","username":"fleet"}'::jsonb
			  WHERE id = $1`, f.orgConfig)
		if err != nil {
			// A hard refusal is an acceptable outcome too; what must never
			// happen is a successful write.
			return
		}
		if tag.RowsAffected() != 0 {
			t.Fatalf("a site-scoped collaborator REPOINTED the organisation's mail server: "+
				"%d rows updated", tag.RowsAffected())
		}
	})

	// The org row must be untouched, credential included.
	admin := connectAdmin(t, pool)
	defer admin.Close()
	var host string
	if err := admin.QueryRow(context.Background(),
		`SELECT config->>'host' FROM site_email_config WHERE id = $1`, f.orgConfig).Scan(&host); err != nil {
		t.Fatalf("re-read org config: %v", err)
	}
	if host != "smtp.org-relay.example" {
		t.Fatalf("org config host is now %q: the organisation's mail server was repointed", host)
	}
	if after := orgSecretCiphertext(t, pool, f.orgConfig); string(after) != string(before) {
		t.Fatal("the organisation's stored credential changed under a site-scoped principal")
	}
}

// TestCollaboratorCannotUpsertOverTheOrgConfig exercises the shape the real
// write takes: UpsertOrgEmailConfig is INSERT ... ON CONFLICT (tenant_id) WHERE
// site_id IS NULL DO UPDATE. A plain UPDATE test would miss it, because the
// upsert reaches the row through the INSERT path first.
func TestCollaboratorCannotUpsertOverTheOrgConfig(t *testing.T) {
	pool := startPostgres(t)
	f := seedEmailScopeFixture(t, pool)
	before := orgSecretCiphertext(t, pool, f.orgConfig)

	err := pool.InScopedTenantTx(context.Background(), f.tenant, uuid.Nil, []uuid.UUID{f.siteB},
		func(tx pgx.Tx) error {
			_, execErr := tx.Exec(context.Background(),
				`INSERT INTO site_email_config (tenant_id, site_id, provider, from_address, config)
				 VALUES ($1, NULL, 'smtp', 'pwned@example.com', '{"host":"smtp.attacker.example"}'::jsonb)
				 ON CONFLICT (tenant_id) WHERE site_id IS NULL
				 DO UPDATE SET config = EXCLUDED.config, from_address = EXCLUDED.from_address`,
				f.tenant)
			return execErr
		})
	if err == nil {
		t.Fatal("a site-scoped collaborator's org-row upsert SUCCEEDED; the INSERT " +
			"WITH CHECK policy must refuse it")
	}
	// Specifically an RLS refusal. The org row is protected by a unique index
	// too, and a 23505 here would mean the test is passing on the index rather
	// than on the policy it claims to exercise.
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("the upsert was refused, but not by RLS (want SQLSTATE 42501); got %v", err)
	}

	admin := connectAdmin(t, pool)
	defer admin.Close()
	var from string
	if qerr := admin.QueryRow(context.Background(),
		`SELECT from_address FROM site_email_config WHERE id = $1`, f.orgConfig).Scan(&from); qerr != nil {
		t.Fatalf("re-read org config: %v", qerr)
	}
	if from != "fleet@example.com" {
		t.Fatalf("org from_address is now %q: the upsert landed on the organisation", from)
	}
	if after := orgSecretCiphertext(t, pool, f.orgConfig); string(after) != string(before) {
		t.Fatal("the organisation's stored credential changed under a site-scoped upsert")
	}
}

// TestCollaboratorCannotDeleteTheOrgConfig closes the crudest version: if you
// cannot repoint the org row, delete it and let your own row take over.
func TestCollaboratorCannotDeleteTheOrgConfig(t *testing.T) {
	pool := startPostgres(t)
	f := seedEmailScopeFixture(t, pool)

	asCollaborator(t, pool, f.tenant, []uuid.UUID{f.siteB}, func(tx pgx.Tx) {
		tag, err := tx.Exec(context.Background(),
			`DELETE FROM site_email_config WHERE id = $1`, f.orgConfig)
		if err != nil {
			return
		}
		if tag.RowsAffected() != 0 {
			t.Fatalf("a site-scoped collaborator DELETED the organisation's email config "+
				"(%d rows)", tag.RowsAffected())
		}
	})
}

// TestCollaboratorCannotPromoteTheirRowToTheOrgRow is the two-step route the
// UPDATE policy's WITH CHECK clause exists for: if you cannot write the org row
// directly, move your own row into it by nulling site_id.
//
// The tenant here deliberately has NO org row. With one present,
// site_email_config_org_default_idx (one org default per tenant) refuses the
// promotion on its own and the test would pass without any policy at all,
// proving nothing. Removing the org row removes that alibi, so the only thing
// left that can refuse the write is the WITH CHECK clause. The assertion is
// specific to it: a 42501 insufficient_privilege from Postgres, not merely
// "some error happened".
func TestCollaboratorCannotPromoteTheirRowToTheOrgRow(t *testing.T) {
	pool := startPostgres(t)
	f := seedEmailScopeFixture(t, pool)
	ctx := context.Background()

	admin := connectAdmin(t, pool)
	defer admin.Close()
	if _, err := admin.Exec(ctx,
		`DELETE FROM site_email_config WHERE id = $1`, f.orgConfig); err != nil {
		t.Fatalf("remove the org row so the unique index cannot mask the policy: %v", err)
	}

	err := pool.InScopedTenantTx(ctx, f.tenant, uuid.Nil, []uuid.UUID{f.siteA},
		func(tx pgx.Tx) error {
			_, execErr := tx.Exec(ctx,
				`UPDATE site_email_config SET site_id = NULL WHERE id = $1`, f.siteAConfig)
			return execErr
		})
	if err == nil {
		t.Fatal("a site-scoped collaborator turned their OWN row into an org row; " +
			"the UPDATE WITH CHECK policy must refuse it")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("the promotion was refused, but not by RLS (want SQLSTATE 42501 "+
			"insufficient_privilege, so we know the WITH CHECK clause is what stopped it); got %v", err)
	}

	var siteID *uuid.UUID
	if qerr := admin.QueryRow(ctx,
		`SELECT site_id FROM site_email_config WHERE id = $1`, f.siteAConfig).Scan(&siteID); qerr != nil {
		t.Fatalf("re-read siteA config: %v", qerr)
	}
	if siteID == nil {
		t.Fatal("siteA's config row now has site_id IS NULL: it became an org row")
	}
}

// TestCollaboratorCanStillWriteTheirOwnConfig proves the policies filter by
// site rather than simply blocking everything once app.site_scope is on. A
// policy that denied the collaborator their OWN row would pass every attack
// test in this file while breaking the feature entirely.
func TestCollaboratorCanStillWriteTheirOwnConfig(t *testing.T) {
	pool := startPostgres(t)
	f := seedEmailScopeFixture(t, pool)

	asCollaborator(t, pool, f.tenant, []uuid.UUID{f.siteA}, func(tx pgx.Tx) {
		tag, err := tx.Exec(context.Background(),
			`UPDATE site_email_config SET from_name = 'Site A Desk' WHERE id = $1`, f.siteAConfig)
		if err != nil {
			t.Fatalf("collaborator updating their OWN site config must work: %v", err)
		}
		if tag.RowsAffected() != 1 {
			t.Fatalf("own-site update affected %d rows, want 1", tag.RowsAffected())
		}
	})
}

// TestCollaboratorCannotWriteAnotherSitesConfig is the plain cross-site case
// inside one tenant, which tenant_isolation alone never covered.
func TestCollaboratorCannotWriteAnotherSitesConfig(t *testing.T) {
	pool := startPostgres(t)
	f := seedEmailScopeFixture(t, pool)

	// Granted siteB only; siteA's row is somebody else's.
	asCollaborator(t, pool, f.tenant, []uuid.UUID{f.siteB}, func(tx pgx.Tx) {
		tag, err := tx.Exec(context.Background(),
			`UPDATE site_email_config SET from_name = 'pwned' WHERE id = $1`, f.siteAConfig)
		if err != nil {
			return
		}
		if tag.RowsAffected() != 0 {
			t.Fatalf("collaborator wrote ANOTHER site's email config (%d rows)", tag.RowsAffected())
		}
	})
}

// TestCollaboratorCannotRebindTheOrgConnection is the connection-registry
// spelling of the same escalation, and the reason the child table needed its
// own policies: site_email_connection has no site_id column, so a predicate
// copied from the exemplar would not even have compiled against it. The row
// carries its own provider_secret_encrypted.
func TestCollaboratorCannotRebindTheOrgConnection(t *testing.T) {
	pool := startPostgres(t)
	f := seedEmailScopeFixture(t, pool)

	asCollaborator(t, pool, f.tenant, []uuid.UUID{f.siteB}, func(tx pgx.Tx) {
		tag, err := tx.Exec(context.Background(),
			`UPDATE site_email_connection
			    SET config = '{"host":"smtp.attacker.example"}'::jsonb
			  WHERE id = $1`, f.orgConn)
		if err != nil {
			return
		}
		if tag.RowsAffected() != 0 {
			t.Fatalf("a site-scoped collaborator repointed the ORG connection (%d rows)",
				tag.RowsAffected())
		}
	})

	admin := connectAdmin(t, pool)
	defer admin.Close()
	var host string
	if err := admin.QueryRow(context.Background(),
		`SELECT config->>'host' FROM site_email_connection WHERE id = $1`, f.orgConn).Scan(&host); err != nil {
		t.Fatalf("re-read org connection: %v", err)
	}
	if host != "smtp.org-relay.example" {
		t.Fatalf("org connection host is now %q", host)
	}
}

// TestCollaboratorCannotDeleteTheOrgConnection: deleting an org connection is
// an organisation-level act, and the delete cascades nothing back that would
// make it recoverable.
func TestCollaboratorCannotDeleteTheOrgConnection(t *testing.T) {
	pool := startPostgres(t)
	f := seedEmailScopeFixture(t, pool)

	asCollaborator(t, pool, f.tenant, []uuid.UUID{f.siteB}, func(tx pgx.Tx) {
		tag, err := tx.Exec(context.Background(),
			`DELETE FROM site_email_connection WHERE id = $1`, f.orgConn)
		if err != nil {
			return
		}
		if tag.RowsAffected() != 0 {
			t.Fatalf("a site-scoped collaborator deleted the ORG connection (%d rows)",
				tag.RowsAffected())
		}
	})
}

// TestCollaboratorCanStillWriteTheirOwnConnection: the filter must be by site,
// not a blanket block, on the child table too.
func TestCollaboratorCanStillWriteTheirOwnConnection(t *testing.T) {
	pool := startPostgres(t)
	f := seedEmailScopeFixture(t, pool)

	asCollaborator(t, pool, f.tenant, []uuid.UUID{f.siteA}, func(tx pgx.Tx) {
		tag, err := tx.Exec(context.Background(),
			`UPDATE site_email_connection SET from_name = 'A' WHERE id = $1`, f.siteAConn)
		if err != nil {
			t.Fatalf("collaborator updating their OWN connection must work: %v", err)
		}
		if tag.RowsAffected() != 1 {
			t.Fatalf("own connection update affected %d rows, want 1", tag.RowsAffected())
		}
	})
}

// TestCollaboratorCannotDeleteFleetSuppression: a fleet-wide suppression row is
// what stops the whole organisation mailing an address that complained.
// Removing it is an organisation-level act, and doing so quietly re-enables
// mail the organisation deliberately stopped.
func TestCollaboratorCannotDeleteFleetSuppression(t *testing.T) {
	pool := startPostgres(t)
	f := seedEmailScopeFixture(t, pool)

	asCollaborator(t, pool, f.tenant, []uuid.UUID{f.siteB}, func(tx pgx.Tx) {
		tag, err := tx.Exec(context.Background(),
			`DELETE FROM email_suppression WHERE id = $1`, f.orgSuppress)
		if err != nil {
			return
		}
		if tag.RowsAffected() != 0 {
			t.Fatalf("a site-scoped collaborator deleted a FLEET-WIDE suppression entry (%d rows)",
				tag.RowsAffected())
		}
	})

	admin := connectAdmin(t, pool)
	defer admin.Close()
	var n int
	if err := admin.QueryRow(context.Background(),
		`SELECT count(*) FROM email_suppression WHERE id = $1`, f.orgSuppress).Scan(&n); err != nil {
		t.Fatalf("re-read suppression: %v", err)
	}
	if n != 1 {
		t.Fatal("the fleet-wide suppression entry is gone")
	}
}

// TestCollaboratorCannotReadAnotherSitesEmailLog: the log carries subjects,
// recipients and optionally message bodies. site_id is NOT NULL there, so this
// is the straightforward per-site case.
func TestCollaboratorCannotReadAnotherSitesEmailLog(t *testing.T) {
	pool := startPostgres(t)
	f := seedEmailScopeFixture(t, pool)

	asCollaborator(t, pool, f.tenant, []uuid.UUID{f.siteB}, func(tx pgx.Tx) {
		var n int
		if err := tx.QueryRow(context.Background(),
			`SELECT count(*) FROM site_email_log WHERE site_id = $1`, f.siteA).Scan(&n); err != nil {
			t.Fatalf("count siteA log: %v", err)
		}
		if n != 0 {
			t.Fatalf("collaborator scoped to siteB read %d of siteA's email log rows", n)
		}
		// And their own site's log is still readable, so the policy filters
		// rather than blocks.
		if err := tx.QueryRow(context.Background(),
			`SELECT count(*) FROM site_email_log WHERE site_id = $1`, f.siteB).Scan(&n); err != nil {
			t.Fatalf("count siteB log: %v", err)
		}
		if n != 1 {
			t.Fatalf("collaborator cannot see their OWN site's email log (%d rows)", n)
		}
	})
}

// TestOrgMemberBehaviourIsUnchanged is the regression guard the migration
// comment promises. Every predicate opens with a
// coalesce(current_setting('app.site_scope', true), ”) <> 'on' tautology, so
// an org member's transaction (InTenantTx, which never sets that GUC) must be
// able to do everything it could before m112. If this test fails, m112 has
// broken the organisation-wide email settings page for everybody.
func TestOrgMemberBehaviourIsUnchanged(t *testing.T) {
	pool := startPostgres(t)
	f := seedEmailScopeFixture(t, pool)
	ctx := context.Background()

	err := pool.InTenantTx(ctx, f.tenant, func(tx pgx.Tx) error {
		tag, execErr := tx.Exec(ctx,
			`UPDATE site_email_config SET from_name = 'Fleet' WHERE id = $1`, f.orgConfig)
		if execErr != nil {
			t.Fatalf("org member updating the org config must work: %v", execErr)
		}
		if tag.RowsAffected() != 1 {
			t.Fatalf("org member org-config update affected %d rows, want 1", tag.RowsAffected())
		}
		tag, execErr = tx.Exec(ctx,
			`UPDATE site_email_connection SET from_name = 'Fleet' WHERE id = $1`, f.orgConn)
		if execErr != nil {
			t.Fatalf("org member updating the org connection must work: %v", execErr)
		}
		if tag.RowsAffected() != 1 {
			t.Fatalf("org member org-connection update affected %d rows, want 1", tag.RowsAffected())
		}
		tag, execErr = tx.Exec(ctx,
			`DELETE FROM email_suppression WHERE id = $1`, f.orgSuppress)
		if execErr != nil {
			t.Fatalf("org member deleting a fleet suppression entry must work: %v", execErr)
		}
		if tag.RowsAffected() != 1 {
			t.Fatalf("org member suppression delete affected %d rows, want 1", tag.RowsAffected())
		}
		// And the org member still sees BOTH sites' logs.
		var n int
		if qerr := tx.QueryRow(ctx, `SELECT count(*) FROM site_email_log`).Scan(&n); qerr != nil {
			t.Fatalf("org member log count: %v", qerr)
		}
		if n != 2 {
			t.Fatalf("org member sees %d log rows, want both sites' 2", n)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("org member tx: %v", err)
	}
}

// TestAgentPathIsUnchanged is the other promise the migration makes. The agent
// pushes email logs and reads config under InAgentTx (app.agent='on', no
// tenant GUC and no site scope). Breaking that would stop every site in the
// fleet reporting mail, which is a fleet-wide outage rather than a bug.
func TestAgentPathIsUnchanged(t *testing.T) {
	pool := startPostgres(t)
	f := seedEmailScopeFixture(t, pool)
	ctx := context.Background()

	err := pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		var n int
		if qerr := tx.QueryRow(ctx, `SELECT count(*) FROM site_email_config`).Scan(&n); qerr != nil {
			return qerr
		}
		if n != 2 {
			t.Fatalf("agent sees %d config rows, want the org row + siteA's", n)
		}
		if qerr := tx.QueryRow(ctx, `SELECT count(*) FROM site_email_connection`).Scan(&n); qerr != nil {
			return qerr
		}
		if n != 2 {
			t.Fatalf("agent sees %d connections, want 2", n)
		}
		// The agent's own write: a pushed log row.
		if _, execErr := tx.Exec(ctx,
			`INSERT INTO site_email_log (tenant_id, site_id, subject, status)
			 VALUES ($1, $2, 'agent push', 'sent')`, f.tenant, f.siteB); execErr != nil {
			t.Fatalf("the agent must still be able to push email log rows: %v", execErr)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("agent tx: %v", err)
	}
}

// TestTenantIsolationStillHolds confirms m112's RESTRICTIVE policies did not
// disturb the permissive tenant_isolation policies underneath them. RESTRICTIVE
// policies can only narrow, so this should be impossible to break here, which
// is exactly why it is cheap to assert.
func TestTenantIsolationStillHolds(t *testing.T) {
	pool := startPostgres(t)
	f := seedEmailScopeFixture(t, pool)
	other := seedTenant(t, pool, "email-scope-other")
	ctx := context.Background()

	err := pool.InTenantTx(ctx, other, func(tx pgx.Tx) error {
		var n int
		if qerr := tx.QueryRow(ctx,
			`SELECT count(*) FROM site_email_config WHERE id = $1`, f.orgConfig).Scan(&n); qerr != nil {
			return qerr
		}
		if n != 0 {
			t.Fatal("another tenant can see this tenant's org email config")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("other-tenant tx: %v", err)
	}
}

// TestEveryEmailTableCarriesTheSiteScopePolicies is the anti-drift check. The
// class of bug this migration closes came from a table that was created
// without the policies its siblings had, and nothing noticed for four
// releases. This asserts the policy set exists by NAME for all four tables, so
// a future ALTER or a squashed schema that drops one fails here rather than in
// a fifth review round.
func TestEveryEmailTableCarriesTheSiteScopePolicies(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	admin := connectAdmin(t, pool)
	defer admin.Close()

	tables := []string{
		"site_email_config",
		"site_email_connection",
		"site_email_log",
		"email_suppression",
	}
	// cmd is the pg_policies.cmd value each policy must carry. Getting these
	// wrong is the whole failure mode the split exists to avoid: a read policy
	// registered as ALL would silently narrow SELECT and break inheritance.
	wantCmd := map[string]string{
		"read":   "SELECT",
		"insert": "INSERT",
		"update": "UPDATE",
		"delete": "DELETE",
	}
	for _, table := range tables {
		for suffix, cmd := range wantCmd {
			name := table + "_site_scope_" + suffix
			var gotCmd string
			var permissive string
			err := admin.QueryRow(ctx,
				`SELECT cmd, permissive FROM pg_policies
				  WHERE schemaname = 'public' AND tablename = $1 AND policyname = $2`,
				table, name).Scan(&gotCmd, &permissive)
			if err != nil {
				t.Errorf("policy %s is missing: %v", name, err)
				continue
			}
			if gotCmd != cmd {
				t.Errorf("policy %s applies to %s, want %s", name, gotCmd, cmd)
			}
			if permissive != "RESTRICTIVE" {
				t.Errorf("policy %s is %s, want RESTRICTIVE (a PERMISSIVE policy would "+
					"WIDEN access instead of narrowing it)", name, permissive)
			}
		}
	}
}

// TestM112IsIdempotent is the boot-safety guard. internal/db/migrate.go applies
// migrations on startup and a failure there takes the control plane down;
// adversarial review has caught a boot-blocking migration in this repo before.
// The version ledger normally stops a migration running twice, but a ledger
// that has been repaired by hand, a squashed history, or a restored backup can
// all present m112 to a database that already has its policies. CREATE POLICY
// has no IF NOT EXISTS in Postgres 16, so every statement in the file is
// wrapped in a pg_policies existence check; this test executes the real file a
// SECOND time against a fully-migrated database and requires it to succeed
// silently.
func TestM112IsIdempotent(t *testing.T) {
	pool := startPostgres(t) // already applied every migration including m112
	ctx := context.Background()

	body, err := migrations.FS.ReadFile(m112Filename)
	if err != nil {
		t.Fatalf("read the embedded m112 file (renamed? the name is load-bearing "+
			"for lexical apply order): %v", err)
	}

	admin := connectAdmin(t, pool)
	defer admin.Close()
	if _, err := admin.Exec(ctx, string(body)); err != nil {
		t.Fatalf("m112 is NOT idempotent: re-applying it to an already-migrated "+
			"database failed, which is a control-plane outage on any boot that "+
			"replays it: %v", err)
	}

	// And the policies are still exactly one of each, not duplicated.
	var n int
	if err := admin.QueryRow(ctx,
		`SELECT count(*) FROM pg_policies
		  WHERE schemaname = 'public' AND policyname LIKE '%_site_scope_%'
		    AND tablename IN ('site_email_config', 'site_email_connection',
		                      'site_email_log', 'email_suppression')`).Scan(&n); err != nil {
		t.Fatalf("count policies: %v", err)
	}
	if n != 16 {
		t.Fatalf("want 16 email site-scope policies (4 tables x 4 operations), got %d", n)
	}
}

// m112Filename is the embedded migration this file proves. Spelled out rather
// than globbed so a rename shows up here as a failure rather than as a silently
// skipped test.
const m112Filename = "20260814000000_m112_email_site_scope_rls.sql"
