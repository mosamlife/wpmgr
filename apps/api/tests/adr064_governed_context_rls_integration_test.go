// adr064_governed_context_rls_integration_test.go: the tenancy, site-scope and
// append-only properties of m122's two context tables, asserted against the
// REAL schema (migrations + RLS) as a NON-SUPERUSER role.
//
// WHAT IS BEING PROTECTED. org_context_versions and site_context_versions hold
// the human-authored instructions ADR-064 hands to a model-facing surface:
// what it may do, what it may never do, and how it should speak for a client.
// Two distinct escalations are possible if the database has no opinion, and
// they are NOT the same escalation:
//
//   READ   a collaborator invited to exactly one site reading another site's
//          context learns that site's brand rules, its terminology and, more
//          to the point, the named boundaries its operator set.
//   WRITE  a collaborator AUTHORING context for a site or an organisation they
//          do not hold is the serious one. ADR-064 Decision 1's whole design
//          rests on "a lower layer can never widen a higher one"; a
//          site-scoped principal that can write the ORGANISATION's layer-2 row
//          does not need to widen anything, because it simply becomes the
//          higher layer. That is m112's defect class exactly -- a per-site
//          principal reaching the organisation row -- which took three review
//          rounds and seven handler fixes to close for the email domain before
//          the fourth round worked out that the handlers were not the problem.
//
// So the write assertions matter at least as much as the read ones, and the
// org-table assertions matter more than the site-table ones.
//
// HOW IT IS TESTED. Every read and every write goes through the real
// transaction wrappers production uses -- db.Pool.InTenantTx for an ordinary
// organisation member and db.Pool.InScopedTenantTx for a site-scoped
// collaborator, which is what db.Pool.RunTenantTx dispatches to for
// Scope == "site". GUCs are never hand-set. A test that opened its own
// connection, or set app.site_scope itself, would leave every policy in this
// file inert and pass while testing nothing; that has happened in this
// codebase before.
//
// The rows under attack are seeded through InTenantTx as wpmgr_app rather than
// inserted by the superuser pool, so the fixture itself exercises the policies'
// WITH CHECK on the way in. There is no repository layer to seed through yet:
// ADR-064's S4 (the resolution function, the widen-check, the secret scan and
// the fail-closed audit append) is backend-architect's and lands on top of
// this migration. When it exists, the seeds below should move onto it.
//
// The non-superuser role is load-bearing and not incidental: a superuser, or
// any role with BYPASSRLS, ignores every policy in this file and all of it
// would pass vacuously. startPostgres provisions the plain wpmgr_app role
// (NOSUPERUSER NOBYPASSRLS, created in the m1 auth migration).
package tests

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

type adr064Fixture struct {
	tenant       uuid.UUID
	siteA, siteB uuid.UUID
}

func adr064SeedSite(t *testing.T, admin *db.Pool, tenant uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := admin.QueryRow(context.Background(),
		`INSERT INTO sites (tenant_id, url, name) VALUES ($1, $2, 'adr064') RETURNING id`,
		tenant, "https://"+uuid.NewString()+".example.com").Scan(&id); err != nil {
		t.Fatalf("seed site: %v", err)
	}
	return id
}

// adr064SeedFixture builds one organisation with two sites, and gives the
// organisation and both sites a version-1 context row.
//
// The seeds run through InTenantTx as wpmgr_app -- the wrapper an ordinary
// organisation member gets -- so the rows arrive the way production would write
// them, past the same tenant_isolation WITH CHECK.
func adr064SeedFixture(t *testing.T, pool *db.Pool, admin *db.Pool) adr064Fixture {
	t.Helper()
	ctx := context.Background()

	f := adr064Fixture{tenant: seedTenant(t, pool, "adr064-"+uuid.NewString()[:8])}
	f.siteA = adr064SeedSite(t, admin, f.tenant)
	f.siteB = adr064SeedSite(t, admin, f.tenant)

	if err := pool.InTenantTx(ctx, f.tenant, func(tx pgx.Tx) error {
		// The fixture must be built by the APPLICATION role, not the superuser
		// pool. This is the sharper half of the CodeRabbit finding: rows
		// inserted by an owner or a BYPASSRLS role never run the policies'
		// WITH CHECK on the way in, so the fixture would prove nothing about
		// insert-time enforcement and a broken WITH CHECK could ship unnoticed.
		// Seeding through InTenantTx as wpmgr_app means these very INSERTs are
		// themselves an assertion that the write path works for a legitimate
		// principal.
		adr064AssertUnprivilegedRole(t, tx)
		if _, e := tx.Exec(ctx,
			`INSERT INTO org_context_versions
			   (tenant_id, version, restrictions, guidance, author_type, author_id, provenance)
			 VALUES ($1, 1, $2, $3, 'system', NULL, 'manual')`,
			f.tenant,
			`{"never_publish": true}`,
			`{"brand_voice": "org-level house style"}`); e != nil {
			return e
		}
		for _, s := range []uuid.UUID{f.siteA, f.siteB} {
			if _, e := tx.Exec(ctx,
				`INSERT INTO site_context_versions
				   (tenant_id, site_id, version, restrictions, guidance, author_type, author_id, provenance)
				 VALUES ($1, $2, 1, $3, $4, 'system', NULL, 'manual')`,
				f.tenant, s,
				`{"never_touch_theme": true}`,
				`{"brand_voice": "site-level house style"}`); e != nil {
				return e
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed context rows: %v", err)
	}
	return f
}

// adr064AsCollaborator runs fn under the exact GUCs a site-scoped collaborator
// granted only `sites` gets in production.
//
// THIS IS THE MUTATION POINT for this file. Swapping InScopedTenantTx for the
// unscoped pool.InTenantTx leaves app.site_scope unset, which turns every
// RESTRICTIVE policy's first branch into a tautology and hands the caller the
// whole organisation. Every assertion below must go red when that is done; any
// that still passes is not testing the policy.
func adr064AsCollaborator(t *testing.T, pool *db.Pool, tenant uuid.UUID, sites []uuid.UUID, fn func(tx pgx.Tx) error) error {
	t.Helper()
	return pool.InScopedTenantTx(context.Background(), tenant, uuid.Nil, sites, func(tx pgx.Tx) error {
		// PRECONDITIONS, CHECKED INSIDE THE TRANSACTION THAT MAKES THE CLAIM.
		// Raised by CodeRabbit on PR #562, and the point is sound even though
		// both conditions already held: without these, every assertion in this
		// file is conditional on setup it inherits and never verifies. A harness
		// change that connected as the owner, or a helper that stopped setting
		// app.site_scope, would leave the policies inert and the whole file
		// green -- which is the exact failure this codebase has shipped before.
		adr064AssertUnprivilegedRole(t, tx)
		adr064AssertSiteScopeActive(t, tx, sites)
		return fn(tx)
	})
}

// adr064AssertUnprivilegedRole fails the test unless the transaction is running
// as the real application role.
//
// A superuser, or any role holding BYPASSRLS, ignores every policy in this
// file. Under that role each proof passes without exercising anything, and
// passes LOUDLY -- there is no symptom to notice. The role is therefore checked
// from inside the transaction doing the work rather than assumed from how the
// pool was built.
func adr064AssertUnprivilegedRole(t *testing.T, tx pgx.Tx) {
	t.Helper()
	var role string
	var super, bypass bool
	if err := tx.QueryRow(context.Background(),
		`SELECT current_user, rolsuper, rolbypassrls
		   FROM pg_roles WHERE rolname = current_user`).Scan(&role, &super, &bypass); err != nil {
		t.Fatalf("read the connection's own role: %v", err)
	}
	if super || bypass {
		t.Fatalf("this proof is running as %q with rolsuper=%v rolbypassrls=%v. "+
			"Either one bypasses every RLS policy on these tables, so every assertion "+
			"in this file would pass without testing anything", role, super, bypass)
	}
	if role != "wpmgr_app" {
		t.Fatalf("this proof is running as %q, not wpmgr_app. wpmgr_app is the role every "+
			"real install connects as, and the only one these proofs describe", role)
	}
}

// adr064AssertSiteScopeActive fails the test unless the site-scope GUCs are
// actually in force, with the allowlist the caller asked for.
//
// InScopedTenantTx sets them with set_config(..., true) -- transaction-local,
// so they cannot leak between tests on a pooled connection. That is the
// property being pinned: if the GUC were ever set session-wide, or the helper
// stopped setting it, the RESTRICTIVE policies' first branch becomes a
// tautology and every site-scope assertion here silently stops testing a
// boundary.
func adr064AssertSiteScopeActive(t *testing.T, tx pgx.Tx, want []uuid.UUID) {
	t.Helper()
	var scope, allowed string
	if err := tx.QueryRow(context.Background(),
		`SELECT coalesce(current_setting('app.site_scope', true), ''),
		        coalesce(current_setting('app.allowed_site_ids', true), '')`).
		Scan(&scope, &allowed); err != nil {
		t.Fatalf("read the site-scope GUCs: %v", err)
	}
	if scope != "on" {
		t.Fatalf("app.site_scope is %q, want \"on\". The RESTRICTIVE policies short-circuit to a "+
			"tautology when it is anything else, so this proof would assert nothing", scope)
	}
	ids := make([]string, len(want))
	for i, id := range want {
		ids[i] = id.String()
	}
	if got := strings.Join(ids, ","); allowed != got {
		t.Fatalf("app.allowed_site_ids is %q, want %q; the collaborator is not scoped to the "+
			"sites this test believes it granted", allowed, got)
	}
}

// These two tables are protected by TWO INDEPENDENT LAYERS, and the whole
// point of the pair is that neither is load-bearing alone. Postgres reports
// both as SQLSTATE 42501 insufficient_privilege, so the code is NOT enough to
// tell them apart and a helper that checks only the code proves whichever one
// happens to fire.
//
// That is not hypothetical. The org table shipped with an RLS gate on INSERT
// alone; UPDATE and DELETE were held off only by the REVOKE, and a test that
// accepted any 42501 passed on the privilege layer while there was no policy
// layer behind it at all. The messages differ, and were read off
// postgres:16-alpine rather than assumed:
//
//	policy    ERROR: new row violates row-level security policy "x" for table "t"
//	privilege ERROR: permission denied for table t
//
// A third shape has no error at all: a RESTRICTIVE policy's USING clause on
// UPDATE or DELETE FILTERS rows rather than raising, so the refusal is
// "0 rows affected" and a successful-looking command tag. Proofs of a USING
// gate must therefore assert RowsAffected, never an error -- see
// adr064AssertNoRowsTouched.

// adr064IsRowSecurityRefusal reports whether err is a POLICY refusing the row.
// This is the assertion to make when the privilege is genuinely held, so that
// the only thing left to refuse is row security.
func adr064IsRowSecurityRefusal(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		return false
	}
	return strings.Contains(pgErr.Message, "row-level security policy")
}

// adr064IsPermissionDenied reports whether err is the GRANT layer refusing --
// the append-only REVOKE, not a policy.
func adr064IsPermissionDenied(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		return false
	}
	return strings.Contains(pgErr.Message, "permission denied")
}

// adr064CountVisible counts rows the given transaction can actually see.
func adr064CountVisible(t *testing.T, tx pgx.Tx, query string, args ...any) int {
	t.Helper()
	var n int
	if err := tx.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count query failed rather than returning a count: %v", err)
	}
	return n
}

// ---------------------------------------------------------------------------
// 1. Tenant isolation, both tables
// ---------------------------------------------------------------------------

func TestADR064_TenantIsolation_AnotherOrgSeesNoContext(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()

	f := adr064SeedFixture(t, pool, admin)
	other := seedTenant(t, pool, "adr064-other-"+uuid.NewString()[:8])

	if err := pool.InTenantTx(ctx, other, func(tx pgx.Tx) error {
		if n := adr064CountVisible(t, tx,
			`SELECT count(*) FROM org_context_versions WHERE tenant_id = $1`, f.tenant); n != 0 {
			t.Errorf("another organisation reads %d of this organisation's context versions, want 0", n)
		}
		if n := adr064CountVisible(t, tx,
			`SELECT count(*) FROM site_context_versions WHERE tenant_id = $1`, f.tenant); n != 0 {
			t.Errorf("another organisation reads %d of this organisation's site context versions, want 0", n)
		}
		return nil
	}); err != nil {
		t.Fatalf("other-tenant tx: %v", err)
	}
}

// A neighbouring organisation must not be able to AUTHOR context for someone
// else's site either. tenant_isolation's WITH CHECK is what refuses this.
func TestADR064_TenantIsolation_AnotherOrgCannotAuthorContext(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()

	f := adr064SeedFixture(t, pool, admin)
	other := seedTenant(t, pool, "adr064-other-"+uuid.NewString()[:8])

	var insertErr error
	_ = pool.InTenantTx(ctx, other, func(tx pgx.Tx) error {
		_, insertErr = tx.Exec(ctx,
			`INSERT INTO site_context_versions
			   (tenant_id, site_id, version, author_type, author_id, provenance)
			 VALUES ($1, $2, 99, 'system', NULL, 'manual')`,
			f.tenant, f.siteA)
		return insertErr
	})
	if insertErr == nil {
		t.Error("a neighbouring organisation authored a context version for another " +
			"organisation's site; tenant_isolation's WITH CHECK is missing or permissive")
	}

	var n int
	if err := admin.QueryRow(ctx,
		`SELECT count(*) FROM site_context_versions WHERE site_id = $1 AND version = 99`,
		f.siteA).Scan(&n); err != nil {
		t.Fatalf("read back forged rows: %v", err)
	}
	if n != 0 {
		t.Errorf("%d forged context version(s) exist for another organisation's site", n)
	}
}

// ---------------------------------------------------------------------------
// 2. Site scope on site_context_versions -- the m19/m113 shape
// ---------------------------------------------------------------------------

func TestADR064_SiteScope_ReadIsConfinedToTheGrantedSite(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()

	f := adr064SeedFixture(t, pool, admin)

	if err := adr064AsCollaborator(t, pool, f.tenant, []uuid.UUID{f.siteA}, func(tx pgx.Tx) error {
		if n := adr064CountVisible(t, tx, `SELECT count(*) FROM site_context_versions`); n != 1 {
			t.Errorf("a collaborator invited to ONE site sees %d site context versions, want 1", n)
		}
		if n := adr064CountVisible(t, tx,
			`SELECT count(*) FROM site_context_versions WHERE site_id = $1`, f.siteB); n != 0 {
			t.Errorf("a collaborator invited to siteA read %d of siteB's context versions. "+
				"site_context_versions is site-keyed, so tenant isolation alone is not the boundary", n)
		}
		// Their own site is still readable, so the policy filters rather than blocks.
		if n := adr064CountVisible(t, tx,
			`SELECT count(*) FROM site_context_versions WHERE site_id = $1`, f.siteA); n != 1 {
			t.Errorf("a collaborator cannot read their OWN site's context (%d rows, want 1)", n)
		}
		return nil
	}); err != nil {
		t.Fatalf("collaborator read: %v", err)
	}
}

func TestADR064_SiteScope_CannotAuthorContextForAnotherSite(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()

	f := adr064SeedFixture(t, pool, admin)

	var insertErr error
	_ = adr064AsCollaborator(t, pool, f.tenant, []uuid.UUID{f.siteA}, func(tx pgx.Tx) error {
		_, insertErr = tx.Exec(ctx,
			`INSERT INTO site_context_versions
			   (tenant_id, site_id, version, restrictions, author_type, author_id, provenance)
			 VALUES ($1, $2, 2, $3, 'system', NULL, 'manual')`,
			f.tenant, f.siteB, `{"never_touch_theme": false}`)
		return insertErr
	})
	if insertErr == nil {
		t.Error("a collaborator invited to siteA authored a context version for siteB. " +
			"The RESTRICTIVE policy's WITH CHECK is missing or permissive, and a lower-privileged " +
			"principal can now rewrite the rules another site's model-facing surface runs under")
	} else if !adr064IsRowSecurityRefusal(insertErr) {
		t.Fatalf("the INSERT was not refused by ROW SECURITY, it failed for another reason: %v.\n"+
			"wpmgr_app holds INSERT on this table, so a permission-denied here would mean the "+
			"grant refused it and the policy was never reached -- which would let this assertion "+
			"pass without testing the policy at all", insertErr)
	}

	var n int
	if err := admin.QueryRow(ctx,
		`SELECT count(*) FROM site_context_versions WHERE site_id = $1`, f.siteB).Scan(&n); err != nil {
		t.Fatalf("read back siteB: %v", err)
	}
	if n != 1 {
		t.Errorf("siteB has %d context versions after the attack, want the 1 it started with", n)
	}
}

// ---------------------------------------------------------------------------
// 3. The org table: READ permitted, WRITE refused
//
// This pair is the reason org_context_versions_site_scope_insert exists, and
// the two halves must be asserted together. A policy that refused the write by
// also refusing the read would pass the second test and silently break the
// layer-2 read ADR-064 Decision 6 and Decision 8 both require.
// ---------------------------------------------------------------------------

func TestADR064_OrgContext_SiteScopedCollaboratorMayRead(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()

	f := adr064SeedFixture(t, pool, admin)

	if err := adr064AsCollaborator(t, pool, f.tenant, []uuid.UUID{f.siteA}, func(tx pgx.Tx) error {
		n := adr064CountVisible(t, tx, `SELECT count(*) FROM org_context_versions`)
		if n != 1 {
			t.Errorf("a site-scoped collaborator sees %d organisation context versions, want 1. "+
				"ADR-064 Decision 6 gives read access at the organisation AND site scope covering "+
				"their site, and Decision 8's effective-context preview renders layer 2; a "+
				"restrictive SELECT gate on this table would break both", n)
		}
		return nil
	}); err != nil {
		t.Fatalf("collaborator org read: %v", err)
	}
}

func TestADR064_OrgContext_SiteScopedCollaboratorCannotAuthor(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()

	f := adr064SeedFixture(t, pool, admin)

	var insertErr error
	_ = adr064AsCollaborator(t, pool, f.tenant, []uuid.UUID{f.siteA}, func(tx pgx.Tx) error {
		_, insertErr = tx.Exec(ctx,
			`INSERT INTO org_context_versions
			   (tenant_id, version, restrictions, author_type, author_id, provenance)
			 VALUES ($1, 2, $2, 'system', NULL, 'manual')`,
			f.tenant, `{"never_publish": false}`)
		return insertErr
	})
	if insertErr == nil {
		t.Fatal("a site-scoped collaborator authored an ORGANISATION context version. " +
			"This is m112's defect class: a per-site principal reaching the organisation row. " +
			"ADR-064 Decision 1 rests on a lower layer never widening a higher one, and a " +
			"principal that can write layer 2 does not need to widen anything -- it becomes the " +
			"higher layer for every site in the organisation")
	}
	if !adr064IsRowSecurityRefusal(insertErr) {
		t.Fatalf("the INSERT was not refused by ROW SECURITY, it failed for another reason: %v.\n"+
			"wpmgr_app holds INSERT on this table, so this must be the policy refusing", insertErr)
	}

	var n int
	if err := admin.QueryRow(ctx,
		`SELECT count(*) FROM org_context_versions WHERE tenant_id = $1`, f.tenant).Scan(&n); err != nil {
		t.Fatalf("read back org versions: %v", err)
	}
	if n != 1 {
		t.Errorf("the organisation has %d context versions after the attack, want the 1 it started with", n)
	}
}

// ---------------------------------------------------------------------------
// 4. The restrictive policies must only ever SUBTRACT
//
// A policy that reddens correct work gets switched off, and then it guards
// nothing. This is the over-fire half.
// ---------------------------------------------------------------------------

func TestADR064_OrdinaryOrgMemberIsUnaffected(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()

	f := adr064SeedFixture(t, pool, admin)

	if err := pool.InTenantTx(ctx, f.tenant, func(tx pgx.Tx) error {
		if n := adr064CountVisible(t, tx, `SELECT count(*) FROM site_context_versions`); n != 2 {
			t.Errorf("an ordinary organisation member sees %d site context versions, want 2; "+
				"the restrictive policy is subtracting from a principal it must not touch", n)
		}
		if n := adr064CountVisible(t, tx, `SELECT count(*) FROM org_context_versions`); n != 1 {
			t.Errorf("an ordinary organisation member sees %d organisation context versions, want 1", n)
		}
		// And they can still author both layers -- the seeds already proved the
		// site table, so prove the org table's INSERT is not blocked for them.
		_, e := tx.Exec(ctx,
			`INSERT INTO org_context_versions
			   (tenant_id, version, author_type, author_id, provenance)
			 VALUES ($1, 2, 'system', NULL, 'manual')`, f.tenant)
		return e
	}); err != nil {
		t.Fatalf("an ordinary organisation member could not author organisation context: %v.\n"+
			"org_context_versions_site_scope_insert is RESTRICTIVE and must be a tautology when "+
			"app.site_scope is unset; if it is refusing here it is refusing every real write", err)
	}
}

// ---------------------------------------------------------------------------
// 5. Append-only, enforced by PRIVILEGE
//
// ADR-064 requires UPDATE/DELETE revoked from the application role "the same
// way the audit log already enforces append-only". A policy would not be
// enough: policies filter rows, and a caller holding UPDATE could still
// rewrite the rows it CAN see -- which for an ordinary organisation member is
// its own entire history.
// ---------------------------------------------------------------------------

func TestADR064_HistoryIsAppendOnlyForTheApplicationRole(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()

	f := adr064SeedFixture(t, pool, admin)

	cases := []struct {
		name string
		sql  string
	}{
		{"update org context", `UPDATE org_context_versions SET guidance = '{}'::jsonb`},
		{"delete org context", `DELETE FROM org_context_versions`},
		{"update site context", `UPDATE site_context_versions SET guidance = '{}'::jsonb`},
		{"delete site context", `DELETE FROM site_context_versions`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var execErr error
			_ = pool.InTenantTx(ctx, f.tenant, func(tx pgx.Tx) error {
				_, execErr = tx.Exec(ctx, tc.sql)
				return execErr
			})
			if execErr == nil {
				t.Fatalf("%q succeeded as wpmgr_app. Context history is append-only by ADR-064's "+
					"own requirement, and an editable history makes Decision 7's claim -- that an "+
					"auditor can prove what instruction set a model was given at a point in time -- "+
					"unprovable", tc.sql)
			}
			// This test is specifically about the PRIVILEGE layer, so it asserts
			// permission-denied rather than any 42501. The policy layer behind it
			// is proved separately, with the privileges granted back, by
			// TestADR064_OrgContext_WriteGatesSurviveARestoredGrant.
			if !adr064IsPermissionDenied(execErr) {
				t.Fatalf("%q did not fail with permission denied but with: %v.\n"+
					"Some other failure would let this assertion pass while the REVOKE was absent",
					tc.sql, execErr)
			}
		})
	}

	// The rows are genuinely still there.
	var n int
	if err := admin.QueryRow(ctx,
		`SELECT count(*) FROM site_context_versions WHERE tenant_id = $1`, f.tenant).Scan(&n); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if n != 2 {
		t.Errorf("%d site context versions survive the mutation attempts, want 2", n)
	}
}

// ---------------------------------------------------------------------------
// 5b. The org table's write gates must survive the privileges coming back
//
// FOUND BY SECURITY REVIEW ON PR #562, and this test is the reason the finding
// cannot return.
//
// org_context_versions originally carried a RESTRICTIVE gate on INSERT alone,
// on the reasoning that the table is append-only so INSERT is the entire write
// surface. That reasoning was wrong in a specific way: it is the REVOKE that
// makes INSERT the whole write surface, so the argument quietly made the REVOKE
// the ONLY layer. The site table never depended on that luck -- its policy is
// FOR ALL.
//
// The review granted UPDATE and DELETE back and found:
//
//	ORG  UPDATE by site-scoped principal -> SUCCEEDED, 1 row(s)
//	ORG  DELETE by site-scoped principal -> SUCCEEDED, 1 row(s)
//	SITE UPDATE of a sibling site        -> refused, 0 row(s)
//
// A blanket "GRANT ... ON ALL TABLES IN SCHEMA public TO wpmgr_app" is not an
// exotic hypothetical: m1:120 already contains that exact statement, and the
// harness in rls_integration_test.go runs it on every startPostgres. Any future
// migration or operator runbook repeating it silently re-arms the gap.
//
// So this test does what the reviewer did. It grants the privileges back and
// then asserts the POLICY refuses, which is the only assertion that can tell a
// real gate from an absent one.
//
// WHY THE ASSERTION IS ROWS-AFFECTED AND NOT AN ERROR. A RESTRICTIVE policy's
// USING clause filters rows rather than raising: with the privilege held and
// the policy in place, the UPDATE succeeds and reports 0 rows. That is the
// #470 silence shape appearing as the CORRECT outcome, so the test reads the
// command tag and then re-reads the row to prove it is genuinely untouched.
// ---------------------------------------------------------------------------

// adr064GrantWriteBack restores UPDATE and DELETE on the two context tables,
// simulating a future blanket GRANT, and revokes them again on cleanup.
//
// startPostgres gives every test its own container (tcpostgres.Run, terminated
// by t.Cleanup), so this cannot leak into another test even without the
// restore. The restore is here anyway, because a grant that outlived its test
// would make the append-only proofs above pass or fail for reasons belonging to
// this one.
func adr064GrantWriteBack(t *testing.T, admin *db.Pool) {
	t.Helper()
	ctx := context.Background()
	for _, tbl := range []string{"org_context_versions", "site_context_versions"} {
		if _, err := admin.Exec(ctx,
			"GRANT UPDATE, DELETE ON "+tbl+" TO wpmgr_app"); err != nil {
			t.Fatalf("grant write back on %s: %v", tbl, err)
		}
		t.Cleanup(func() {
			_, _ = admin.Exec(context.Background(),
				"REVOKE UPDATE, DELETE ON "+tbl+" FROM wpmgr_app")
		})
	}

	// The grant must actually have landed, or every assertion below passes on
	// permission-denied and proves nothing -- which is the exact failure this
	// whole test exists to rule out.
	var canUpdate bool
	if err := admin.QueryRow(ctx,
		`SELECT has_table_privilege('wpmgr_app', 'org_context_versions', 'UPDATE')`).
		Scan(&canUpdate); err != nil {
		t.Fatalf("check restored privilege: %v", err)
	}
	if !canUpdate {
		t.Fatal("wpmgr_app still lacks UPDATE after the GRANT; this test would pass on " +
			"permission-denied without ever reaching a policy")
	}
}

func TestADR064_OrgContext_WriteGatesSurviveARestoredGrant(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()

	f := adr064SeedFixture(t, pool, admin)
	adr064GrantWriteBack(t, admin)

	cases := []struct {
		name string
		sql  string
	}{
		{"update", `UPDATE org_context_versions SET guidance = '{"brand_voice":"seized"}'::jsonb`},
		{"delete", `DELETE FROM org_context_versions`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var affected int64
			err := adr064AsCollaborator(t, pool, f.tenant, []uuid.UUID{f.siteA}, func(tx pgx.Tx) error {
				tag, e := tx.Exec(ctx, tc.sql)
				if e != nil {
					return e
				}
				affected = tag.RowsAffected()
				return nil
			})
			if err != nil {
				// An error is acceptable ONLY if it is row security. It must not
				// be permission-denied: the grant is back, so that would mean the
				// test never reached a policy.
				if adr064IsPermissionDenied(err) {
					t.Fatalf("%q was refused by the GRANT layer, not by a policy: %v.\n"+
						"adr064GrantWriteBack was supposed to restore the privilege; with it "+
						"absent this assertion proves nothing about row security", tc.sql, err)
				}
				if !adr064IsRowSecurityRefusal(err) {
					t.Fatalf("%q failed for an unrelated reason: %v", tc.sql, err)
				}
				return
			}
			if affected != 0 {
				t.Fatalf("%q by a SITE-SCOPED principal affected %d organisation context row(s), "+
					"want 0.\nThe REVOKE is not a substitute for a policy: a blanket "+
					"GRANT ... ON ALL TABLES (m1:120 contains that statement, and the test harness "+
					"runs it) hands the privilege straight back, and with no RESTRICTIVE gate on "+
					"this command a per-site collaborator can rewrite or destroy the organisation's "+
					"governing context for every site in it", tc.sql, affected)
			}
		})
	}

	// The rows are genuinely untouched -- not merely unreported.
	var n int
	var guidance string
	if err := admin.QueryRow(ctx,
		`SELECT count(*), coalesce(max(guidance ->> 'brand_voice'), '')
		   FROM org_context_versions WHERE tenant_id = $1`, f.tenant).Scan(&n, &guidance); err != nil {
		t.Fatalf("read back org context: %v", err)
	}
	if n != 1 {
		t.Errorf("the organisation has %d context versions after the attack, want the 1 it started with", n)
	}
	if guidance != "org-level house style" {
		t.Errorf("organisation guidance is now %q, want it unchanged at %q",
			guidance, "org-level house style")
	}
}

// The site table's FOR ALL policy must hold under the same restored grant. This
// is the control: it passed in the review while the org table failed, and if it
// ever stops passing the two tables have diverged again.
func TestADR064_SiteContext_WriteGatesSurviveARestoredGrant(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()

	f := adr064SeedFixture(t, pool, admin)
	adr064GrantWriteBack(t, admin)

	var affected int64
	if err := adr064AsCollaborator(t, pool, f.tenant, []uuid.UUID{f.siteA}, func(tx pgx.Tx) error {
		tag, e := tx.Exec(ctx,
			`UPDATE site_context_versions SET guidance = '{"brand_voice":"seized"}'::jsonb
			  WHERE site_id = $1`, f.siteB)
		if e != nil {
			return e
		}
		affected = tag.RowsAffected()
		return nil
	}); err != nil {
		if adr064IsPermissionDenied(err) {
			t.Fatalf("refused by the GRANT layer, not a policy: %v", err)
		}
		if !adr064IsRowSecurityRefusal(err) {
			t.Fatalf("failed for an unrelated reason: %v", err)
		}
		return
	}
	if affected != 0 {
		t.Fatalf("a collaborator invited to siteA rewrote %d of siteB's context rows, want 0", affected)
	}
}

// ---------------------------------------------------------------------------
// 6. The tenant cascade frees both tables -- including on the Lane A path,
//    which blanks app.tenant_id first
//
// ADR-064 Decision 12 says context rides "the same tenant-purge path every
// other tenant-scoped table rides". Both purge functions are cascade-driven and
// name no tables, so this is the only thing standing behind that sentence.
//
// The blank-GUC half is the one worth having. admin_delete_empty_tenant blanks
// app.tenant_id to '' BEFORE deleting the tenants row (m116), and under FORCE
// ROW LEVEL SECURITY these tables' tenant_isolation policy matches nothing when
// that GUC is blank. If referential actions did NOT bypass row security, the
// cascade would be refused and an organisation with context would become
// undeletable. They do bypass it -- and this test is what keeps that true
// rather than assumed, on whatever PostgreSQL the fleet actually runs.
// ---------------------------------------------------------------------------

func TestADR064_TenantDeleteCascadesBothContextTables(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()

	f := adr064SeedFixture(t, pool, admin)

	var before int
	if err := admin.QueryRow(ctx,
		`SELECT (SELECT count(*) FROM org_context_versions  WHERE tenant_id = $1)
		      + (SELECT count(*) FROM site_context_versions WHERE tenant_id = $1)`,
		f.tenant).Scan(&before); err != nil {
		t.Fatalf("count before: %v", err)
	}
	if before != 3 {
		t.Fatalf("fixture has %d context rows, want 3; this test would prove nothing", before)
	}

	// Delete the tenant with app.tenant_id BLANK, which is what Lane A does.
	if _, err := admin.Exec(ctx, `SELECT set_config('app.tenant_id', '', false)`); err != nil {
		t.Fatalf("blank the guc: %v", err)
	}
	if _, err := admin.Exec(ctx, `DELETE FROM tenants WHERE id = $1`, f.tenant); err != nil {
		t.Fatalf("deleting a tenant that owns context rows failed: %v.\n"+
			"If this is a foreign-key violation, the cascade onto the context tables was refused "+
			"by their own RLS and an organisation with context cannot be deleted at all", err)
	}

	var after int
	if err := admin.QueryRow(ctx,
		`SELECT (SELECT count(*) FROM org_context_versions  WHERE tenant_id = $1)
		      + (SELECT count(*) FROM site_context_versions WHERE tenant_id = $1)`,
		f.tenant).Scan(&after); err != nil {
		t.Fatalf("count after: %v", err)
	}
	if after != 0 {
		t.Errorf("%d context rows survive the tenant delete, want 0. ADR-064 Decision 12 frees "+
			"context with the organisation; surviving rows are an organisation's governing "+
			"instructions outliving the organisation itself", after)
	}
}

// The site-level cascade, separately: deleting one site must free that site's
// context and leave its sibling's alone.
func TestADR064_SiteDeleteCascadesOnlyThatSitesContext(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()

	f := adr064SeedFixture(t, pool, admin)

	if _, err := admin.Exec(ctx, `DELETE FROM sites WHERE id = $1`, f.siteA); err != nil {
		t.Fatalf("delete siteA: %v", err)
	}

	var a, b int
	if err := admin.QueryRow(ctx,
		`SELECT (SELECT count(*) FROM site_context_versions WHERE site_id = $1),
		        (SELECT count(*) FROM site_context_versions WHERE site_id = $2)`,
		f.siteA, f.siteB).Scan(&a, &b); err != nil {
		t.Fatalf("count after site delete: %v", err)
	}
	if a != 0 {
		t.Errorf("%d context rows survive their site's deletion, want 0", a)
	}
	if b != 1 {
		t.Errorf("deleting siteA took %d of siteB's context rows with it; want siteB left at 1", 1-b)
	}
}

// ---------------------------------------------------------------------------
// 7. The policies are actually the shape m122 says they are
//
// An anti-drift assertion read off pg_policies rather than off the migration
// text, so a later migration that DROPs or widens one of these is caught here
// and not in a review. The org table's gate being FOR INSERT rather than FOR
// ALL is the load-bearing detail: FOR ALL would AND itself onto SELECT and
// break the layer-2 read.
// ---------------------------------------------------------------------------

func TestADR064_ContextTablesCarryTheExpectedPolicies(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()

	want := []struct {
		table, policy, permissive, cmd string
	}{
		// Three command-specific gates on the org table, never one FOR ALL:
		// FOR ALL would be AND-combined onto SELECT and break the layer-2 read
		// ADR-064 Decision 6 and Decision 8 both require. m122 shipped only the
		// INSERT one; m123 added UPDATE and DELETE after security review found
		// the REVOKE was the sole layer behind them.
		{"org_context_versions", "org_context_versions_site_scope_insert", "RESTRICTIVE", "INSERT"},
		{"org_context_versions", "org_context_versions_site_scope_update", "RESTRICTIVE", "UPDATE"},
		{"org_context_versions", "org_context_versions_site_scope_delete", "RESTRICTIVE", "DELETE"},
		{"org_context_versions", "org_context_versions_tenant_isolation", "PERMISSIVE", "ALL"},
		{"site_context_versions", "site_context_versions_site_scope", "RESTRICTIVE", "ALL"},
		{"site_context_versions", "site_context_versions_tenant_isolation", "PERMISSIVE", "ALL"},
	}

	for _, w := range want {
		var permissive, cmd string
		err := admin.QueryRow(ctx,
			`SELECT permissive, cmd FROM pg_policies
			  WHERE schemaname = 'public' AND tablename = $1 AND policyname = $2`,
			w.table, w.policy).Scan(&permissive, &cmd)
		if errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("policy %s on %s does not exist. A missing policy is not the same fact as "+
				"a renamed one, and neither reads as 'this table needs no policy'", w.policy, w.table)
			continue
		}
		if err != nil {
			t.Fatalf("read pg_policies for %s: %v", w.policy, err)
		}
		if permissive != w.permissive || cmd != w.cmd {
			t.Errorf("policy %s on %s is %s/%s, want %s/%s",
				w.policy, w.table, permissive, cmd, w.permissive, w.cmd)
		}
	}

	// The ABSENCE of a restrictive SELECT gate on the org table is as
	// load-bearing as any policy present, so it is asserted rather than left to
	// a comment somebody will helpfully "complete" one day. A RESTRICTIVE
	// SELECT policy here -- or a FOR ALL one, which is AND-combined onto SELECT
	// and amounts to the same thing -- would break ADR-064 Decision 6's
	// organisation-scope read and Decision 8's effective-context preview for
	// every site-scoped collaborator, and it would do it silently, because a
	// USING gate filters rows rather than raising.
	var restrictiveReadGates int
	if err := admin.QueryRow(ctx,
		`SELECT count(*) FROM pg_policies
		  WHERE schemaname = 'public' AND tablename = 'org_context_versions'
		    AND permissive = 'RESTRICTIVE' AND cmd IN ('SELECT', 'ALL')`).
		Scan(&restrictiveReadGates); err != nil {
		t.Fatalf("count restrictive read gates: %v", err)
	}
	if restrictiveReadGates != 0 {
		t.Errorf("org_context_versions carries %d RESTRICTIVE policy/policies covering SELECT, want 0. "+
			"A site-scoped collaborator must be able to READ the organisation context governing "+
			"their own site; gating that read breaks ADR-064 Decision 6 and Decision 8 at once",
			restrictiveReadGates)
	}

	// FORCE matters as much as ENABLE: without it the table owner -- which is
	// the role the application connects as in some deployments -- skips every
	// policy above.
	for _, tbl := range []string{"org_context_versions", "site_context_versions"} {
		var enabled, forced bool
		if err := admin.QueryRow(ctx,
			`SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = $1`,
			tbl).Scan(&enabled, &forced); err != nil {
			t.Fatalf("read pg_class for %s: %v", tbl, err)
		}
		if !enabled || !forced {
			t.Errorf("%s has RLS enabled=%v forced=%v, want both true", tbl, enabled, forced)
		}
	}
}
