// email_site_config_delete_integration_test.go: GH #968. Removing a site's own
// email provider deletes that site's site_email_config row with one statement,
// DeleteSiteEmailConfig. These tests pin what that statement can reach, against
// the real schema (migrations plus RLS), as wpmgr_app.
//
// Every delete runs through the generated query inside the transaction helpers
// the email repo's scopedTenantTx dispatches to: db.Pool.InTenantTx for an
// organisation member, db.Pool.InScopedTenantTx for a collaborator granted only
// some sites. requireAppRoleInTx checks, inside each of those transactions, that
// the role is wpmgr_app with neither SUPERUSER nor BYPASSRLS, so the policies
// are live. What survived is read back on the superuser pool, which RLS does not
// filter, so a row a policy merely hides cannot pass for a row that is still
// there.
package tests

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

// siteEmailDeleteFixture is one organisation with its own default config
// (site_id IS NULL), two sites that each have a config of their own, and a
// second organisation with a site config. Every config carries a named
// connection, and site A carries two, so the cascade is seen to take more than
// one row.
type siteEmailDeleteFixture struct {
	tenant, otherTenant     uuid.UUID
	siteA, siteB, otherSite uuid.UUID

	orgConfig, siteAConfig, siteBConfig, otherConfig uuid.UUID
}

func siteEmailDeleteSiteID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}

// seedSiteEmailDeleteFixture seeds on the superuser pool. Seeding is setup;
// every delete below runs as the app role.
func seedSiteEmailDeleteFixture(t *testing.T, app *db.Pool) siteEmailDeleteFixture {
	t.Helper()
	ctx := context.Background()
	admin := connectAdmin(t, app)
	defer admin.Close()

	f := siteEmailDeleteFixture{siteA: uuid.New(), siteB: uuid.New(), otherSite: uuid.New()}
	for _, tn := range []struct {
		id   *uuid.UUID
		slug string
	}{{&f.tenant, "email-delete"}, {&f.otherTenant, "email-delete-other"}} {
		if err := admin.QueryRow(ctx,
			"INSERT INTO tenants (name, slug) VALUES ($1, $1) RETURNING id", tn.slug).Scan(tn.id); err != nil {
			t.Fatalf("seed tenant %s: %v", tn.slug, err)
		}
	}
	for _, s := range []struct {
		id, tenant uuid.UUID
		url        string
	}{
		{f.siteA, f.tenant, "https://a.email-delete.example"},
		{f.siteB, f.tenant, "https://b.email-delete.example"},
		{f.otherSite, f.otherTenant, "https://other.email-delete.example"},
	} {
		if _, err := admin.Exec(ctx,
			"INSERT INTO sites (id, tenant_id, url, name) VALUES ($1, $2, $3, $3)", s.id, s.tenant, s.url); err != nil {
			t.Fatalf("seed site %s: %v", s.url, err)
		}
	}

	seedConfig := func(tenant uuid.UUID, site pgtype.UUID, from string) uuid.UUID {
		var id uuid.UUID
		if err := admin.QueryRow(ctx,
			`INSERT INTO site_email_config (tenant_id, site_id, provider, from_address, config, provider_secret_encrypted)
			 VALUES ($1, $2, 'smtp', $3, '{"host":"smtp.email-delete.example"}'::jsonb, '\x534543524554')
			 RETURNING id`, tenant, site, from).Scan(&id); err != nil {
			t.Fatalf("seed config %s: %v", from, err)
		}
		return id
	}
	f.orgConfig = seedConfig(f.tenant, pgtype.UUID{}, "org@email-delete.example")
	f.siteAConfig = seedConfig(f.tenant, siteEmailDeleteSiteID(f.siteA), "a@email-delete.example")
	f.siteBConfig = seedConfig(f.tenant, siteEmailDeleteSiteID(f.siteB), "b@email-delete.example")
	f.otherConfig = seedConfig(f.otherTenant, siteEmailDeleteSiteID(f.otherSite), "other@email-delete.example")

	for _, c := range []struct {
		tenant, config uuid.UUID
		key            string
	}{
		{f.tenant, f.siteAConfig, "billing"},
		{f.tenant, f.siteAConfig, "alerts"},
		{f.tenant, f.siteBConfig, "billing"},
		{f.tenant, f.orgConfig, "billing"},
		{f.otherTenant, f.otherConfig, "billing"},
	} {
		if _, err := admin.Exec(ctx,
			`INSERT INTO site_email_connection (tenant_id, config_id, connection_key, provider, config, provider_secret_encrypted)
			 VALUES ($1, $2, $3, 'smtp', '{"host":"smtp.email-delete.example"}'::jsonb, '\x434f4e4e')`,
			c.tenant, c.config, c.key); err != nil {
			t.Fatalf("seed connection %s: %v", c.key, err)
		}
	}

	// Site A's email log: the record of what the site sent.
	if _, err := admin.Exec(ctx,
		`INSERT INTO site_email_log (tenant_id, site_id, subject, status) VALUES ($1, $2, 'hello', 'sent')`,
		f.tenant, f.siteA); err != nil {
		t.Fatalf("seed site A log: %v", err)
	}
	return f
}

// siteEmailRowCounter counts rows on the superuser pool, so a count is what
// the table holds rather than what some principal can see.
func siteEmailRowCounter(t *testing.T, app *db.Pool) func(query string, args ...any) int {
	t.Helper()
	admin := connectAdmin(t, app)
	t.Cleanup(admin.Close)
	return func(query string, args ...any) int {
		t.Helper()
		var n int
		if err := admin.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
			t.Fatalf("count %q: %v", query, err)
		}
		return n
	}
}

const (
	countSiteEmailConfigByID        = `SELECT count(*) FROM site_email_config WHERE id = $1`
	countSiteEmailConnectionsByConf = `SELECT count(*) FROM site_email_connection WHERE config_id = $1`
)

// logSiteEmailDelete records, from inside the transaction, the role and the
// RLS settings the delete ran under, so the -v output carries the evidence and
// not only the verdict.
func logSiteEmailDelete(t *testing.T, ctx context.Context, tx pgx.Tx, label string, n int64) {
	t.Helper()
	var role, tenant, scope, allowed string
	if err := tx.QueryRow(ctx, `SELECT current_user,
		       coalesce(current_setting('app.tenant_id', true), ''),
		       coalesce(current_setting('app.site_scope', true), ''),
		       coalesce(current_setting('app.allowed_site_ids', true), '')`).
		Scan(&role, &tenant, &scope, &allowed); err != nil {
		t.Fatalf("read the RLS settings: %v", err)
	}
	t.Logf("%s: role=%s app.tenant_id=%s app.site_scope=%q app.allowed_site_ids=%q -> %d rows",
		label, role, tenant, scope, allowed, n)
}

// deleteSiteEmailConfigAsMember runs DeleteSiteEmailConfig the way an
// organisation member's request does: in db.Pool.InTenantTx for actingTenant,
// with whatever ids the request names.
func deleteSiteEmailConfigAsMember(t *testing.T, pool *db.Pool, label string, actingTenant, tenant, site uuid.UUID) int64 {
	t.Helper()
	ctx := context.Background()
	var n int64
	if err := pool.InTenantTx(ctx, actingTenant, func(tx pgx.Tx) error {
		requireAppRoleInTx(t, ctx, tx)
		var qerr error
		n, qerr = sqlc.New(tx).DeleteSiteEmailConfig(ctx, sqlc.DeleteSiteEmailConfigParams{
			TenantID: tenant,
			SiteID:   siteEmailDeleteSiteID(site),
		})
		if qerr != nil {
			return qerr
		}
		logSiteEmailDelete(t, ctx, tx, label, n)
		return nil
	}); err != nil {
		t.Fatalf("DeleteSiteEmailConfig as a member of %s: %v", actingTenant, err)
	}
	return n
}

// deleteSiteEmailConfigAsCollaborator runs it the way a site-scoped
// collaborator's request does: in db.Pool.InScopedTenantTx with only the
// granted sites in app.allowed_site_ids.
func deleteSiteEmailConfigAsCollaborator(t *testing.T, pool *db.Pool, label string, tenant uuid.UUID, granted []uuid.UUID, site uuid.UUID) int64 {
	t.Helper()
	ctx := context.Background()
	var n int64
	if err := pool.InScopedTenantTx(ctx, tenant, uuid.Nil, granted, func(tx pgx.Tx) error {
		requireAppRoleInTx(t, ctx, tx)
		var qerr error
		n, qerr = sqlc.New(tx).DeleteSiteEmailConfig(ctx, sqlc.DeleteSiteEmailConfigParams{
			TenantID: tenant,
			SiteID:   siteEmailDeleteSiteID(site),
		})
		if qerr != nil {
			return qerr
		}
		logSiteEmailDelete(t, ctx, tx, label, n)
		return nil
	}); err != nil {
		t.Fatalf("DeleteSiteEmailConfig as a collaborator granted %v: %v", granted, err)
	}
	return n
}

// TestDeleteSiteEmailConfigRemovesOnlyThatSitesOwnRow is the feature: an
// organisation member removes site A's provider. Exactly site A's row goes,
// and its named connections with it; site B's row, the organisation default
// that site A now falls back to, and site A's email log all stay.
func TestDeleteSiteEmailConfigRemovesOnlyThatSitesOwnRow(t *testing.T) {
	pool := startPostgres(t)
	f := seedSiteEmailDeleteFixture(t, pool)
	count := siteEmailRowCounter(t, pool)

	// Positive control: the fixture holds what the assertions below count.
	if got := count(countSiteEmailConnectionsByConf, f.siteAConfig); got != 2 {
		t.Fatalf("fixture: site A has %d connections, want 2", got)
	}

	if n := deleteSiteEmailConfigAsMember(t, pool, "member, site A, first delete", f.tenant, f.tenant, f.siteA); n != 1 {
		t.Fatalf("first delete of site A's config affected %d rows, want 1", n)
	}
	if n := deleteSiteEmailConfigAsMember(t, pool, "member, site A, repeat delete", f.tenant, f.tenant, f.siteA); n != 0 {
		t.Fatalf("repeat delete of site A's config affected %d rows, want 0", n)
	}

	for _, c := range []struct {
		what        string
		config      uuid.UUID
		rows, conns int
	}{
		{"site A's own config", f.siteAConfig, 0, 0},
		{"site B's config", f.siteBConfig, 1, 1},
		{"the organisation default", f.orgConfig, 1, 1},
		{"the other organisation's config", f.otherConfig, 1, 1},
	} {
		if got := count(countSiteEmailConfigByID, c.config); got != c.rows {
			t.Errorf("%s: %d config rows, want %d", c.what, got, c.rows)
		}
		if got := count(countSiteEmailConnectionsByConf, c.config); got != c.conns {
			t.Errorf("%s: %d connection rows, want %d", c.what, got, c.conns)
		}
	}
	if got := count(`SELECT count(*) FROM site_email_log WHERE tenant_id = $1 AND site_id = $2`,
		f.tenant, f.siteA); got != 1 {
		t.Errorf("site A's email log: %d rows, want 1; removing the provider must not remove the record of what was sent", got)
	}

	// What the service reads next for site A: no row of its own, so it
	// resolves to the organisation default.
	ctx := context.Background()
	if err := pool.InTenantTx(ctx, f.tenant, func(tx pgx.Tx) error {
		requireAppRoleInTx(t, ctx, tx)
		q := sqlc.New(tx)
		_, err := q.GetSiteEmailConfig(ctx, sqlc.GetSiteEmailConfigParams{
			TenantID: f.tenant,
			SiteID:   siteEmailDeleteSiteID(f.siteA),
		})
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("GetSiteEmailConfig for site A after the delete: err = %v, want pgx.ErrNoRows", err)
		}
		org, err := q.GetOrgEmailConfig(ctx, f.tenant)
		if err != nil {
			t.Errorf("GetOrgEmailConfig after the delete: %v", err)
		} else if org.ID != f.orgConfig {
			t.Errorf("GetOrgEmailConfig after the delete returned %s, want the organisation default %s", org.ID, f.orgConfig)
		}
		return nil
	}); err != nil {
		t.Fatalf("read back as a member: %v", err)
	}
}

// TestDeleteSiteEmailConfigCannotReachAnotherTenant: a member of one
// organisation who names another organisation's ids deletes nothing. The first
// call puts the other tenant in the WHERE clause, so tenant isolation is the
// only thing between it and the row; the superuser control at the end shows the
// same statement does match that row when RLS is not applied.
func TestDeleteSiteEmailConfigCannotReachAnotherTenant(t *testing.T) {
	pool := startPostgres(t)
	f := seedSiteEmailDeleteFixture(t, pool)
	count := siteEmailRowCounter(t, pool)

	if n := deleteSiteEmailConfigAsMember(t, pool, "member, other organisation's tenant and site",
		f.tenant, f.otherTenant, f.otherSite); n != 0 {
		t.Fatalf("a member naming another organisation's tenant and site deleted %d rows, want 0", n)
	}
	if n := deleteSiteEmailConfigAsMember(t, pool, "member, other organisation's site under own tenant",
		f.tenant, f.tenant, f.otherSite); n != 0 {
		t.Fatalf("a member naming another organisation's site under their own tenant deleted %d rows, want 0", n)
	}
	if got := count(countSiteEmailConfigByID, f.otherConfig); got != 1 {
		t.Fatalf("the other organisation's config: %d rows, want 1", got)
	}
	if got := count(countSiteEmailConnectionsByConf, f.otherConfig); got != 1 {
		t.Fatalf("the other organisation's connections: %d rows, want 1", got)
	}
	if got := count(`SELECT count(*) FROM site_email_config WHERE tenant_id = $1`, f.tenant); got != 3 {
		t.Fatalf("the acting organisation's own configs: %d rows, want its default, site A and site B", got)
	}

	// Control: the zero above comes from RLS, not from ids that match nothing.
	ctx := context.Background()
	admin := connectAdmin(t, pool)
	defer admin.Close()
	tx, err := admin.Begin(ctx)
	if err != nil {
		t.Fatalf("control: begin on the superuser pool: %v", err)
	}
	n, err := sqlc.New(tx).DeleteSiteEmailConfig(ctx, sqlc.DeleteSiteEmailConfigParams{
		TenantID: f.otherTenant,
		SiteID:   siteEmailDeleteSiteID(f.otherSite),
	})
	if rbErr := tx.Rollback(ctx); rbErr != nil {
		t.Fatalf("control: rollback: %v", rbErr)
	}
	if err != nil {
		t.Fatalf("control: delete on the superuser pool: %v", err)
	}
	t.Logf("control, superuser pool, no RLS, other organisation's tenant and site: -> %d rows (rolled back)", n)
	if n != 1 {
		t.Fatalf("control: without RLS the statement matched %d rows, want 1, so the zero above proves nothing", n)
	}
	if got := count(countSiteEmailConfigByID, f.otherConfig); got != 1 {
		t.Fatalf("control was not rolled back: the other organisation's config has %d rows", got)
	}
}

// TestDeleteSiteEmailConfigRespectsSiteScope: a collaborator granted only site
// B cannot remove site A's provider, and one granted site A can. The refusal is
// the RESTRICTIVE site_email_config_site_scope_delete policy (m112), which
// Postgres reports as zero rows affected rather than as an error.
func TestDeleteSiteEmailConfigRespectsSiteScope(t *testing.T) {
	pool := startPostgres(t)
	f := seedSiteEmailDeleteFixture(t, pool)
	count := siteEmailRowCounter(t, pool)

	if n := deleteSiteEmailConfigAsCollaborator(t, pool, "collaborator granted site B, site A",
		f.tenant, []uuid.UUID{f.siteB}, f.siteA); n != 0 {
		t.Fatalf("a collaborator granted only site B deleted %d of site A's config rows", n)
	}
	if got := count(countSiteEmailConfigByID, f.siteAConfig); got != 1 {
		t.Fatalf("site A's config after a refused delete: %d rows, want 1", got)
	}
	if got := count(countSiteEmailConnectionsByConf, f.siteAConfig); got != 2 {
		t.Fatalf("site A's connections after a refused delete: %d rows, want 2", got)
	}

	if n := deleteSiteEmailConfigAsCollaborator(t, pool, "collaborator granted site A, site A",
		f.tenant, []uuid.UUID{f.siteA}, f.siteA); n != 1 {
		t.Fatalf("a collaborator granted site A deleted %d of its config rows, want 1", n)
	}
	for _, c := range []struct {
		what        string
		config      uuid.UUID
		rows, conns int
	}{
		{"site A's own config", f.siteAConfig, 0, 0},
		{"site B's config", f.siteBConfig, 1, 1},
		{"the organisation default", f.orgConfig, 1, 1},
	} {
		if got := count(countSiteEmailConfigByID, c.config); got != c.rows {
			t.Errorf("%s: %d config rows, want %d", c.what, got, c.rows)
		}
		if got := count(countSiteEmailConnectionsByConf, c.config); got != c.conns {
			t.Errorf("%s: %d connection rows, want %d", c.what, got, c.conns)
		}
	}
}
