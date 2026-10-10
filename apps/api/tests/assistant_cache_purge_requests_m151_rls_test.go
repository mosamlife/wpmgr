// m151 proofs: assistant_cache_purge_requests is invisible outside its tenant
// and its site scope, its facts cannot move, its rows cannot be deleted, its
// CHECKs refuse every dishonest row the design names, and the statements the
// Go layer will run (db/query/assistant_requests.sql) do what their comments
// say under the principals they are written for.
//
// Built the way assistant_update_proposals_m133_rls_test.go is, for the same
// three reasons: every read and write goes through the production dispatch
// (RunTenantTx / InTenantTx / InScopedTenantTx / InAgentTx); every transaction
// under test asserts, from inside, that it is wpmgr_app with neither SUPERUSER
// nor BYPASSRLS; and in each proof the leak assertion comes first.
//
// Where a statement exists in db/query/assistant_requests.sql, the proof calls
// the generated sqlc method rather than a copy of its SQL, so the statement
// proven is the statement shipped. Raw SQL appears only for rows no shipped
// statement writes -- the dishonest rows a CHECK must refuse.
package tests

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/org"
)

const acprTable = "assistant_cache_purge_requests"

// acprHex builds a 64-char lowercase hex value from a seed, for
// presented_digest and digest_nonce. The real values are server-computed;
// these proofs are about the table, not the digest.
func acprHex(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])
}

func acprStr(s string) *string { return &s }

// acprSitePrincipal is the principal shape connectionScopedPrincipal and
// SingleSitePrincipal build in internal/mcp: site scope, an explicit allowed
// set. Test code may build it; the call-fence tests forbid it in non-test Go.
func acprSitePrincipal(tenant uuid.UUID, sites ...uuid.UUID) domain.Principal {
	return domain.Principal{TenantID: tenant, Scope: domain.ScopeSite, AllowedSiteIDs: sites}
}

func acprOrgPrincipal(tenant uuid.UUID) domain.Principal {
	return domain.Principal{TenantID: tenant, Scope: domain.ScopeOrg}
}

// acprDeciderPrincipal is the approving person's own org-wide principal: a
// person's approval is made in a transaction that carries that person's user
// id, which m174's backstop requires of every approval naming a decider.
func acprDeciderPrincipal(tenant, decider uuid.UUID) domain.Principal {
	return domain.Principal{Type: domain.PrincipalUser, UserID: decider, TenantID: tenant, Scope: domain.ScopeOrg}
}

// acprParams is an honest pending request for one site, as the creation path
// would build it. Callers override fields to plant a single fault.
func acprParams(tenant, site, grant uuid.UUID, seed string) sqlc.InsertAssistantCachePurgeRequestParams {
	return sqlc.InsertAssistantCachePurgeRequestParams{
		TenantID:          tenant,
		SiteID:            site,
		ProposedByGrantID: grant,
		Scope:             "all",
		Url:               nil,
		SiteLabel:         "Shop",
		SiteHost:          "shop.example.com",
		GrantLabel:        "Laptop",
		GrantVia:          "token",
		SetupClient:       acprStr("claude-code"),
		DigestNonce:       acprHex("nonce-" + seed),
		PresentedDigest:   acprHex("digest-" + seed),
		ExpiresAt:         time.Now().UTC().Add(24 * time.Hour),
	}
}

// acprInsert writes one request through the shipped insert statement under
// the given principal and returns it. It fails the test on any error, and on
// the ON CONFLICT no-row answer.
func acprInsert(t *testing.T, pool *db.Pool, p domain.Principal, arg sqlc.InsertAssistantCachePurgeRequestParams) sqlc.AssistantCachePurgeRequest {
	t.Helper()
	var row sqlc.AssistantCachePurgeRequest
	if err := pool.RunTenantTx(context.Background(), p, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "RunTenantTx (m151 insert)")
		var err error
		row, err = sqlc.New(tx).InsertAssistantCachePurgeRequest(context.Background(), arg)
		return err
	}); err != nil {
		t.Fatalf("insert request for site %s: %v", arg.SiteID, err)
	}
	return row
}

// acprApprove approves a pending row through the shipped statement under the
// decider's own principal and returns it.
func acprApprove(t *testing.T, pool *db.Pool, row sqlc.AssistantCachePurgeRequest, decider uuid.UUID) sqlc.AssistantCachePurgeRequest {
	t.Helper()
	var out sqlc.AssistantCachePurgeRequest
	if err := pool.RunTenantTx(context.Background(), acprDeciderPrincipal(row.TenantID, decider), func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "RunTenantTx (m151 approve)")
		var err error
		out, err = sqlc.New(tx).ApproveAssistantCachePurgeRequest(context.Background(),
			sqlc.ApproveAssistantCachePurgeRequestParams{
				DecidedByUserID: decider,
				TenantID:        row.TenantID,
				ID:              row.ID,
				SiteID:          row.SiteID,
				PresentedDigest: row.PresentedDigest,
			})
		return err
	}); err != nil {
		t.Fatalf("approve request %s: %v", row.ID, err)
	}
	return out
}

// acprExpectConstraint runs sql inside a savepoint and requires it to fail
// with SQLSTATE code at the named constraint. The constraint name matters: a
// refusal from a different CHECK would leave the named one unproven.
func acprExpectConstraint(t *testing.T, tx pgx.Tx, what, code, constraint, sql string, args ...any) {
	t.Helper()
	err := m133ExpectRefused(t, tx, sql, args...)
	if err == nil {
		t.Fatalf("%s: the statement SUCCEEDED; %s (%s) should have refused it", what, constraint, code)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("%s: refused, but not by PostgreSQL: %v", what, err)
	}
	if pgErr.Code != code || pgErr.ConstraintName != constraint {
		t.Fatalf("%s: refused with %s at %q, want %s at %q: %v",
			what, pgErr.Code, pgErr.ConstraintName, code, constraint, err)
	}
	t.Logf("%s: refused %s at %s", what, code, constraint)
}

func acprCountVisible(t *testing.T, tx pgx.Tx, id uuid.UUID) int {
	t.Helper()
	var n int
	if err := tx.QueryRow(context.Background(),
		`SELECT count(*) FROM assistant_cache_purge_requests WHERE id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("count visible requests: %v", err)
	}
	return n
}

// ---------------------------------------------------------------------------
// TENANT ISOLATION AND SITE SCOPE
// ---------------------------------------------------------------------------

// TestAssistantCachePurgeRequestIsolationAsAppRole proves the permissive
// tenant policy and the RESTRICTIVE site-scope policy, both directions of the
// latter.
//
// Mutations, planted and watched:
//   - ALTER POLICY assistant_cache_purge_requests_tenant_isolation ... USING
//     (true) WITH CHECK (true) fires "TENANCY LEAK";
//   - DROP POLICY assistant_cache_purge_requests_site_scope fires "SITE-SCOPE
//     LEAK" and the write arm.
func TestAssistantCachePurgeRequestIsolationAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)

	tenantA := seedTenant(t, pool, "m151-a-"+uuid.NewString()[:8])
	tenantB := seedTenant(t, pool, "m151-b-"+uuid.NewString()[:8])
	s1 := seedSite(t, pool, tenantA, "")
	s2 := seedSite(t, pool, tenantA, "")
	grant := uuid.New()

	r1 := acprInsert(t, pool, acprSitePrincipal(tenantA, s1, s2), acprParams(tenantA, s1, grant, "iso-1"))
	r2 := acprInsert(t, pool, acprSitePrincipal(tenantA, s1, s2), acprParams(tenantA, s2, grant, "iso-2"))

	if err := pool.InTenantTx(ctx, tenantB, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (foreign tenant)")
		if n := acprCountVisible(t, tx, r1.ID); n != 0 {
			t.Fatalf("TENANCY LEAK: tenant B sees tenant A's request %s (%d rows); "+
				"assistant_cache_purge_requests_tenant_isolation is missing or not enforced", r1.ID, n)
		}
		var total int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM assistant_cache_purge_requests`).Scan(&total); err != nil {
			return err
		}
		if total != 0 {
			t.Fatalf("tenant B sees %d requests in a table where it has written none", total)
		}
		return nil
	}); err != nil {
		t.Fatalf("foreign tenant read: %v", err)
	}

	if err := pool.RunTenantTx(ctx, acprSitePrincipal(tenantA, s1), func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "RunTenantTx (single site S1)")
		if n := acprCountVisible(t, tx, r2.ID); n != 0 {
			t.Fatalf("SITE-SCOPE LEAK: a principal scoped to site %s sees request %s on site %s (%d rows); "+
				"assistant_cache_purge_requests_site_scope is missing or not RESTRICTIVE", s1, r2.ID, s2, n)
		}
		if n := acprCountVisible(t, tx, r1.ID); n != 1 {
			t.Fatalf("OVER-FIRING: a principal scoped to site %s cannot see its own site's request %s (%d rows)", s1, r1.ID, n)
		}
		// The shipped insert statement, inside a savepoint so the refusal does
		// not abort the transaction under test.
		sp, err := tx.Begin(ctx)
		if err != nil {
			return err
		}
		_, err = sqlc.New(sp).InsertAssistantCachePurgeRequest(ctx, acprParams(tenantA, s2, uuid.New(), "iso-smuggle"))
		_ = sp.Rollback(ctx)
		if err == nil {
			t.Fatalf("SITE-SCOPE WRITE LEAK: a principal scoped to site %s inserted a request for site %s", s1, s2)
		}
		if !strings.Contains(strings.ToLower(err.Error()), "row-level security") {
			t.Fatalf("the out-of-scope INSERT was refused, but not by row security: %v", err)
		}
		t.Logf("out-of-scope insert refused by row security: %v", err)
		return nil
	}); err != nil {
		t.Fatalf("single-site read and write: %v", err)
	}
}

// TestAssistantCachePurgeRequestSiteMustBelongToItsTenantAsAppRole proves the
// composite foreign key: a request cannot name another tenant's site.
func TestAssistantCachePurgeRequestSiteMustBelongToItsTenantAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	tenantA := seedTenant(t, pool, "m151-fk-a-"+uuid.NewString()[:8])
	tenantB := seedTenant(t, pool, "m151-fk-b-"+uuid.NewString()[:8])
	siteB := seedSite(t, pool, tenantB, "")

	if err := pool.InTenantTx(ctx, tenantA, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (foreign site)")
		a := acprParams(tenantA, siteB, uuid.New(), "fk")
		acprExpectConstraint(t, tx, "tenant A's request naming tenant B's site", "23503",
			"assistant_cache_purge_requests_site_within_tenant_fkey", `
			INSERT INTO assistant_cache_purge_requests
			    (tenant_id, site_id, proposed_by_grant_id, scope, site_label, site_host,
			     grant_label, grant_via, digest_nonce, presented_digest, state, expires_at)
			VALUES ($1, $2, $3, 'all', 'x', 'x.test', 'g', 'token', $4, $5, 'pending', now() + interval '1 hour')`,
			a.TenantID, a.SiteID, a.ProposedByGrantID, a.DigestNonce, a.PresentedDigest)
		return nil
	}); err != nil {
		t.Fatalf("foreign-site insert: %v", err)
	}
}

// ---------------------------------------------------------------------------
// PRIVILEGES
// ---------------------------------------------------------------------------

// TestAssistantCachePurgeRequestFactsImmutableAndRowUndeletableAsAppRole proves
// the column-privilege immutability and the DELETE/TRUNCATE revoke.
//
// Mutation, planted and watched: removing the harness re-revokes for this
// table from rls_integration_test.go makes every refusal below succeed.
func TestAssistantCachePurgeRequestFactsImmutableAndRowUndeletableAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "m151-priv-"+uuid.NewString()[:8])
	site := seedSite(t, pool, tenant, "")
	arg := acprParams(tenant, site, uuid.New(), "priv")
	arg.Scope = "url"
	arg.Url = acprStr("https://shop.example.com/a/")
	row := acprInsert(t, pool, acprSitePrincipal(tenant, site), arg)

	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (privileges)")
		for _, col := range []struct{ name, value string }{
			{"url", "'https://shop.example.com/b/'"},
			{"site_host", "'other.example.com'"},
			{"digest_nonce", "'" + acprHex("other-nonce") + "'"},
			{"grant_label", "'Other'"},
			{"presented_digest", "'" + acprHex("other-digest") + "'"},
			{"expires_at", "now() + interval '30 days'"},
			{"site_id", "site_id"},
			{"proposed_by_grant_id", "proposed_by_grant_id"},
		} {
			err := m133ExpectRefused(t, tx,
				`UPDATE assistant_cache_purge_requests SET `+col.name+` = `+col.value+` WHERE id = $1`, row.ID)
			if !m133IsPermissionDenied(err) {
				t.Fatalf("UPDATE of immutable column %s: want 42501, got %v", col.name, err)
			}
			t.Logf("UPDATE %s refused 42501", col.name)
		}
		if err := m133ExpectRefused(t, tx, `DELETE FROM assistant_cache_purge_requests WHERE id = $1`, row.ID); !m133IsPermissionDenied(err) {
			t.Fatalf("DELETE: want 42501, got %v", err)
		}
		if err := m133ExpectRefused(t, tx, `TRUNCATE assistant_cache_purge_requests`); !m133IsPermissionDenied(err) {
			t.Fatalf("TRUNCATE: want 42501, got %v", err)
		}
		// POSITIVE CONTROL: a workflow column still moves.
		n, err := m133ExpectNoRows(t, tx, `UPDATE assistant_cache_purge_requests
			SET last_attempt_code = 'site_unreachable', last_attempt_at = now(),
			    dispatch_attempts = dispatch_attempts + 1 WHERE id = $1`, row.ID)
		if err != nil || n != 1 {
			t.Fatalf("OVER-FIRING: a granted workflow column did not move (rows=%d err=%v)", n, err)
		}
		return nil
	}); err != nil {
		t.Fatalf("privileges: %v", err)
	}
}

// ---------------------------------------------------------------------------
// THE CHECKS
// ---------------------------------------------------------------------------

// TestAssistantCachePurgeRequestChecksAsAppRole proves each CHECK refuses the
// dishonest row the design names, at its own constraint, and admits the
// honest neighbour. B1 (withdrawn, N6), F1 (url and site_host backstops) and
// G2 (a whole-site row with a Punycode host).
//
// Mutations, planted and watched: replacing not_sent_has_reason with the
// `(outcome = 'not_sent') = (not_sent_reason IS NOT NULL)` form admits
// "outcome NULL with a reason"; dropping site_host_ascii admits 'bücher.de'.
func TestAssistantCachePurgeRequestChecksAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "m151-chk-"+uuid.NewString()[:8])
	site := seedSite(t, pool, tenant, "")
	p := acprSitePrincipal(tenant, site)
	decider := uuid.New()

	// Honest rows the checks must admit: F1 and G2.
	urlArg := acprParams(tenant, site, uuid.New(), "chk-url")
	urlArg.Scope = "url"
	urlArg.Url = acprStr("https://xn--bcher-kva.de/shop/sale/")
	urlArg.SiteHost = "xn--bcher-kva.de"
	urlRow := acprInsert(t, pool, p, urlArg)

	allArg := acprParams(tenant, site, uuid.New(), "chk-all")
	allArg.SiteHost = "xn--bcher-kva.de"
	allRow := acprInsert(t, pool, p, allArg)
	if allRow.Url != nil || allRow.SiteHost != "xn--bcher-kva.de" {
		t.Fatalf("G2: whole-site row stored url=%v site_host=%q", allRow.Url, allRow.SiteHost)
	}
	t.Logf("G2: scope=all, url NULL, site_host=%s inserted", allRow.SiteHost)

	host255 := strings.Repeat("a", 255)
	h255 := acprParams(tenant, site, uuid.New(), "chk-255")
	h255.SiteHost = host255
	acprInsert(t, pool, p, h255)

	approved := acprApprove(t, pool, acprInsert(t, pool, p, acprParams(tenant, site, uuid.New(), "chk-approved")), decider)

	insertSQL := `INSERT INTO assistant_cache_purge_requests
		(tenant_id, site_id, proposed_by_grant_id, scope, url, site_label, site_host,
		 grant_label, grant_via, digest_nonce, presented_digest, state, expires_at)
		VALUES ($1, $2, gen_random_uuid(), $3, $4, 'Shop', $5, 'Laptop', 'token', $6, $7, 'pending',
		        now() + interval '1 hour')`
	ins := func(tx pgx.Tx, what, constraint, scope string, url *string, host string) {
		acprExpectConstraint(t, tx, what, "23514", constraint, insertSQL,
			tenant, site, scope, url, host, acprHex(what+"n"), acprHex(what+"d"))
	}

	if err := pool.RunTenantTx(ctx, p, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "RunTenantTx (checks)")

		// url_backstop.
		const backstop = "assistant_cache_purge_requests_url_backstop_check"
		for _, bad := range []string{
			"https://shop.example.com/a b/",
			"https://shop.example.com/a‮/",
			"https://shop.example.com/a?x=1",
			"https://shop.example.com/a#frag",
			"https://bücher.de/shop/sale/",
			"ftp://shop.example.com/a/",
			"https://shop.example.com/" + strings.Repeat("a", 2048),
		} {
			label := bad
			if len(label) > 40 {
				label = strings.ToValidUTF8(label[:40], "?") + "..."
			}
			ins(tx, "url "+label, backstop, "url", acprStr(bad), "shop.example.com")
		}

		// url present exactly when scope is 'url'.
		const matches = "assistant_cache_purge_requests_url_matches_scope_check"
		ins(tx, "scope url with no url", matches, "url", nil, "shop.example.com")
		ins(tx, "scope all with a url", matches, "all", acprStr("https://shop.example.com/a/"), "shop.example.com")

		// site_host_ascii (F1).
		const ascii = "assistant_cache_purge_requests_site_host_ascii_check"
		ins(tx, "site_host bücher.de", ascii, "all", nil, "bücher.de")
		ins(tx, "site_host empty", ascii, "all", nil, "")
		ins(tx, "site_host 256 bytes", ascii, "all", nil, strings.Repeat("a", 256))
		ins(tx, "site_host with a space", ascii, "all", nil, "shop example.com")

		// Decisions, on the pending url row.
		acprExpectConstraint(t, tx, "approval without a decider", "23514",
			"assistant_cache_purge_requests_approval_names_a_human_check",
			`UPDATE assistant_cache_purge_requests SET state = 'approved_undispatched', decided_at = now() WHERE id = $1`, urlRow.ID)
		acprExpectConstraint(t, tx, "rejection without a decider", "23514",
			"assistant_cache_purge_requests_rejection_names_a_human_check",
			`UPDATE assistant_cache_purge_requests SET state = 'rejected', decided_at = now() WHERE id = $1`, urlRow.ID)

		// Withdrawal (B1).
		acprExpectConstraint(t, tx, "withdrawn naming a decider", "23514",
			"assistant_cache_purge_requests_withdrawal_names_nobody_check",
			`UPDATE assistant_cache_purge_requests SET state = 'withdrawn', withdrawn_at = now(), decided_by_user_id = $2 WHERE id = $1`,
			urlRow.ID, decider)
		acprExpectConstraint(t, tx, "withdrawn without withdrawn_at", "23514",
			"assistant_cache_purge_requests_withdrawn_has_time_check",
			`UPDATE assistant_cache_purge_requests SET state = 'withdrawn' WHERE id = $1`, urlRow.ID)
		acprExpectConstraint(t, tx, "withdrawn with decided_at set", "23514",
			"assistant_cache_purge_requests_decided_states_have_time_check",
			`UPDATE assistant_cache_purge_requests SET state = 'withdrawn', withdrawn_at = now(), decided_at = now() WHERE id = $1`, urlRow.ID)
		acprExpectConstraint(t, tx, "withdrawn_at on a pending row", "23514",
			"assistant_cache_purge_requests_withdrawn_has_time_check",
			`UPDATE assistant_cache_purge_requests SET withdrawn_at = now() WHERE id = $1`, urlRow.ID)

		// N6: a reason with no outcome.
		acprExpectConstraint(t, tx, "not_sent_reason with outcome NULL", "23514",
			"assistant_cache_purge_requests_not_sent_has_reason_check",
			`UPDATE assistant_cache_purge_requests SET not_sent_reason = 'site_absent' WHERE id = $1`, urlRow.ID)

		// Outcomes, on the approved row.
		acprExpectConstraint(t, tx, "purged with no cache_purge_audit_id", "23514",
			"assistant_cache_purge_requests_sent_names_audit_row_check",
			`UPDATE assistant_cache_purge_requests SET state = 'dispatched', claimed_at = now(),
			     outcome = 'purged', outcome_at = now() WHERE id = $1`, approved.ID)
		acprExpectConstraint(t, tx, "not_sent with no reason", "23514",
			"assistant_cache_purge_requests_not_sent_has_reason_check",
			`UPDATE assistant_cache_purge_requests SET state = 'dispatched', claimed_at = now(),
			     outcome = 'not_sent', outcome_at = now() WHERE id = $1`, approved.ID)
		acprExpectConstraint(t, tx, "dispatched with no claimed_at", "23514",
			"assistant_cache_purge_requests_claimed_iff_dispatched_check",
			`UPDATE assistant_cache_purge_requests SET state = 'dispatched' WHERE id = $1`, approved.ID)
		acprExpectConstraint(t, tx, "an outcome on an undispatched row", "23514",
			"assistant_cache_purge_requests_outcome_when_dispatched_check",
			`UPDATE assistant_cache_purge_requests SET outcome = 'not_sent', not_sent_reason = 'site_absent',
			     outcome_at = now() WHERE id = $1`, approved.ID)
		acprExpectConstraint(t, tx, "an unknown hosting cache", "23514",
			"assistant_cache_purge_requests_hosting_cleared_check",
			`UPDATE assistant_cache_purge_requests SET hosting_caches_cleared = ARRAY['somehost'] WHERE id = $1`, approved.ID)

		// POSITIVE CONTROL for the whole block: the honest terminal close
		// through the shipped statement is admitted.
		n, err := sqlc.New(tx).CloseApprovedAssistantCachePurgeRequestNotSent(ctx,
			sqlc.CloseApprovedAssistantCachePurgeRequestNotSentParams{
				NotSentReason: "agent_outdated", TenantID: tenant, ID: approved.ID})
		if err != nil || n != 1 {
			t.Fatalf("OVER-FIRING: the honest not-sent close was refused (rows=%d err=%v)", n, err)
		}
		return nil
	}); err != nil {
		t.Fatalf("checks: %v", err)
	}

	// Approval after the window, through the shipped statement AND raw. A
	// lapsed row is planted with a backdated created_at (INSERT may set it).
	// The transaction carries the decider's own user id, as a person's
	// approval does, so the window CHECK is what refuses it.
	var lapsed uuid.UUID
	if err := pool.RunTenantTx(ctx, acprDeciderPrincipal(tenant, decider), func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO assistant_cache_purge_requests
			(tenant_id, site_id, proposed_by_grant_id, scope, site_label, site_host, grant_label,
			 grant_via, digest_nonce, presented_digest, state, created_at, expires_at)
			VALUES ($1, $2, gen_random_uuid(), 'all', 'Shop', 'shop.example.com', 'Laptop', 'token',
			        $3, $4, 'pending', now() - interval '2 hours', now() - interval '1 hour')
			RETURNING id`, tenant, site, acprHex("lapsed-n"), acprHex("lapsed-d")).Scan(&lapsed); err != nil {
			return err
		}
		mcpAssertAndReportRole(t, tx, "RunTenantTx (lapsed approval)")
		_, err := sqlc.New(tx).ApproveAssistantCachePurgeRequest(ctx, sqlc.ApproveAssistantCachePurgeRequestParams{
			DecidedByUserID: decider, TenantID: tenant, ID: lapsed, SiteID: site,
			PresentedDigest: acprHex("lapsed-d")})
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("approving a lapsed row through the shipped statement: want no row, got %v", err)
		}
		acprExpectConstraint(t, tx, "approval after expires_at", "23514",
			"assistant_cache_purge_requests_consent_within_window_check",
			`UPDATE assistant_cache_purge_requests SET state = 'approved_undispatched', decided_at = now(),
			     decided_by_user_id = $2 WHERE id = $1`, lapsed, decider)
		return nil
	}); err != nil {
		t.Fatalf("lapsed approval: %v", err)
	}

	// The honest withdrawal, through the shipped cascade statement.
	if err := pool.RunTenantTx(ctx, acprOrgPrincipal(tenant), func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "RunTenantTx (honest withdraw)")
		ids, err := sqlc.New(tx).WithdrawPendingAssistantCachePurgeRequestsForGrant(ctx,
			sqlc.WithdrawPendingAssistantCachePurgeRequestsForGrantParams{TenantID: tenant, ProposedByGrantID: urlRow.ProposedByGrantID})
		if err != nil {
			return err
		}
		if len(ids) != 1 || ids[0] != urlRow.ID {
			t.Fatalf("honest withdraw: got %v, want [%s]", ids, urlRow.ID)
		}
		return nil
	}); err != nil {
		t.Fatalf("honest withdraw: %v", err)
	}
}

// ---------------------------------------------------------------------------
// ONE WAITING REQUEST PER CONNECTION PER SITE
// ---------------------------------------------------------------------------

func TestAssistantCachePurgeRequestOnePendingPerGrantSiteAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "m151-uniq-"+uuid.NewString()[:8])
	site := seedSite(t, pool, tenant, "")
	p := acprSitePrincipal(tenant, site)
	grantX, grantY := uuid.New(), uuid.New()
	first := acprInsert(t, pool, p, acprParams(tenant, site, grantX, "uniq-1"))

	if err := pool.RunTenantTx(ctx, p, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "RunTenantTx (dedupe)")
		acprExpectConstraint(t, tx, "a second pending row for the same (site, grant)", "23505",
			"assistant_cache_purge_requests_one_pending_idx", `
			INSERT INTO assistant_cache_purge_requests
			    (tenant_id, site_id, proposed_by_grant_id, scope, site_label, site_host,
			     grant_label, grant_via, digest_nonce, presented_digest, state, expires_at)
			VALUES ($1, $2, $3, 'all', 'x', 'x.test', 'g', 'token', $4, $5, 'pending', now() + interval '1 hour')`,
			tenant, site, grantX, acprHex("u2n"), acprHex("u2d"))

		q := sqlc.New(tx)
		_, err := q.InsertAssistantCachePurgeRequest(ctx, acprParams(tenant, site, grantX, "uniq-2"))
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("shipped insert on conflict: want no row, got %v", err)
		}
		got, err := q.GetPendingAssistantCachePurgeRequestForGrantSite(ctx,
			sqlc.GetPendingAssistantCachePurgeRequestForGrantSiteParams{TenantID: tenant, SiteID: site, ProposedByGrantID: grantX})
		if err != nil || got.ID != first.ID {
			t.Fatalf("dedupe read: got %v err=%v, want %s", got.ID, err, first.ID)
		}
		other, err := q.InsertAssistantCachePurgeRequest(ctx, acprParams(tenant, site, grantY, "uniq-3"))
		if err != nil {
			t.Fatalf("OVER-FIRING: another connection's request for the same site was refused: %v", err)
		}
		t.Logf("same site, other grant: inserted %s", other.ID)
		return nil
	}); err != nil {
		t.Fatalf("dedupe: %v", err)
	}
}

// ---------------------------------------------------------------------------
// N1: THE CAP COUNT FOLLOWS THE PRINCIPAL
// ---------------------------------------------------------------------------

// TestAssistantCachePurgeGrantCapCountsEverySiteAsAppRole proves why the
// per-connection caps must run connection-scoped: the same statement under a
// single-site principal undercounts.
func TestAssistantCachePurgeGrantCapCountsEverySiteAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "m151-n1-"+uuid.NewString()[:8])
	s1 := seedSite(t, pool, tenant, "")
	s2 := seedSite(t, pool, tenant, "")
	grant := uuid.New()
	conn := acprSitePrincipal(tenant, s1, s2)
	acprInsert(t, pool, conn, acprParams(tenant, s1, grant, "n1-1"))
	acprInsert(t, pool, conn, acprParams(tenant, s2, grant, "n1-2"))

	count := func(p domain.Principal) (pending, daily int64) {
		if err := pool.RunTenantTx(ctx, p, func(tx pgx.Tx) error {
			mcpAssertAndReportRole(t, tx, "RunTenantTx (cap count)")
			q := sqlc.New(tx)
			var err error
			if pending, err = q.CountLivePendingAssistantCachePurgeRequestsForGrant(ctx,
				sqlc.CountLivePendingAssistantCachePurgeRequestsForGrantParams{TenantID: tenant, ProposedByGrantID: grant}); err != nil {
				return err
			}
			daily, err = q.CountAssistantCachePurgeRequestsForGrantSince(ctx,
				sqlc.CountAssistantCachePurgeRequestsForGrantSinceParams{TenantID: tenant, ProposedByGrantID: grant, WindowSeconds: 86400})
			return err
		}); err != nil {
			t.Fatalf("cap count: %v", err)
		}
		return pending, daily
	}
	if pending, daily := count(conn); pending != 2 || daily != 2 {
		t.Fatalf("connection-scoped: pending=%d daily=%d, want 2 and 2", pending, daily)
	}
	pending, daily := count(acprSitePrincipal(tenant, s1))
	if pending != 1 || daily != 1 {
		t.Fatalf("single-site: pending=%d daily=%d, want 1 and 1", pending, daily)
	}
	t.Logf("N1: connection-scoped counts 2, single-site counts %d: the caps must run connection-scoped", pending)
}

// ---------------------------------------------------------------------------
// EXCEPTION 11 AND THE REVOKE CASCADE
// ---------------------------------------------------------------------------

// TestAssistantCachePurgeCloseWithoutSiteStatementAsAppRole proves the
// exception 11 statement: under a site principal for another site it changes
// nothing; under the tenant transaction it closes the approved row once,
// keeping its approver; a repeat and another tenant change nothing.
func TestAssistantCachePurgeCloseWithoutSiteStatementAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "m151-x11-"+uuid.NewString()[:8])
	other := seedTenant(t, pool, "m151-x11o-"+uuid.NewString()[:8])
	s1 := seedSite(t, pool, tenant, "")
	s2 := seedSite(t, pool, tenant, "")
	decider := uuid.New()
	row := acprApprove(t, pool, acprInsert(t, pool, acprSitePrincipal(tenant, s1), acprParams(tenant, s1, uuid.New(), "x11")), decider)

	closeIt := func(where string, run func(fn func(tx pgx.Tx) error) error) int64 {
		var n int64
		if err := run(func(tx pgx.Tx) error {
			mcpAssertAndReportRole(t, tx, where)
			var err error
			n, err = sqlc.New(tx).CloseApprovedAssistantCachePurgeRequestNotSent(ctx,
				sqlc.CloseApprovedAssistantCachePurgeRequestNotSentParams{NotSentReason: "site_absent", TenantID: tenant, ID: row.ID})
			return err
		}); err != nil {
			t.Fatalf("%s: %v", where, err)
		}
		return n
	}
	if n := closeIt("site principal for S2", func(fn func(tx pgx.Tx) error) error {
		return pool.RunTenantTx(ctx, acprSitePrincipal(tenant, s2), fn)
	}); n != 0 {
		t.Fatalf("under a site principal for another site the close changed %d rows, want 0", n)
	}
	if n := closeIt("InTenantTx (other tenant)", func(fn func(tx pgx.Tx) error) error {
		return pool.InTenantTx(ctx, other, fn)
	}); n != 0 {
		t.Fatalf("under another tenant the close changed %d rows, want 0", n)
	}
	if n := closeIt("InTenantTx (exception 11)", func(fn func(tx pgx.Tx) error) error {
		return pool.InTenantTx(ctx, tenant, fn)
	}); n != 1 {
		t.Fatalf("under the tenant transaction the close changed %d rows, want 1", n)
	}
	if n := closeIt("InTenantTx (repeat)", func(fn func(tx pgx.Tx) error) error {
		return pool.InTenantTx(ctx, tenant, fn)
	}); n != 0 {
		t.Fatalf("a repeated close changed %d rows, want 0", n)
	}

	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		var state, outcome, reason string
		var by uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT state, outcome, not_sent_reason, decided_by_user_id
			FROM assistant_cache_purge_requests WHERE id = $1`, row.ID).Scan(&state, &outcome, &reason, &by); err != nil {
			return err
		}
		if state != "dispatched" || outcome != "not_sent" || reason != "site_absent" || by != decider {
			t.Fatalf("closed row: state=%s outcome=%s reason=%s decider=%s; want dispatched/not_sent/site_absent/%s",
				state, outcome, reason, by, decider)
		}
		return nil
	}); err != nil {
		t.Fatalf("read closed row: %v", err)
	}
}

// TestAssistantCachePurgeRevokeCascadeStatementsAsAppRole proves the two
// cascade statements under an org-scoped revoker: waiting rows become
// withdrawn naming nobody, approved rows close as not sent keeping their
// approver, a started clear and another grant's rows are untouched.
func TestAssistantCachePurgeRevokeCascadeStatementsAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "m151-rev-"+uuid.NewString()[:8])
	s1 := seedSite(t, pool, tenant, "")
	s2 := seedSite(t, pool, tenant, "")
	s3 := seedSite(t, pool, tenant, "")
	conn := acprSitePrincipal(tenant, s1, s2, s3)
	grant, otherGrant, decider := uuid.New(), uuid.New(), uuid.New()

	p1 := acprInsert(t, pool, conn, acprParams(tenant, s1, grant, "rev-1"))
	p2 := acprInsert(t, pool, conn, acprParams(tenant, s2, grant, "rev-2"))
	ap := acprApprove(t, pool, acprInsert(t, pool, conn, acprParams(tenant, s3, grant, "rev-3")), decider)
	untouched := acprInsert(t, pool, conn, acprParams(tenant, s1, otherGrant, "rev-other"))

	if err := pool.RunTenantTx(ctx, acprOrgPrincipal(tenant), func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "RunTenantTx (revoker)")
		q := sqlc.New(tx)
		withdrawn, err := q.WithdrawPendingAssistantCachePurgeRequestsForGrant(ctx,
			sqlc.WithdrawPendingAssistantCachePurgeRequestsForGrantParams{TenantID: tenant, ProposedByGrantID: grant})
		if err != nil {
			return err
		}
		notSent, err := q.CloseApprovedAssistantCachePurgeRequestsForGrant(ctx,
			sqlc.CloseApprovedAssistantCachePurgeRequestsForGrantParams{TenantID: tenant, ProposedByGrantID: grant})
		if err != nil {
			return err
		}
		if len(withdrawn) != 2 || len(notSent) != 1 || notSent[0] != ap.ID {
			t.Fatalf("cascade: withdrew %v and closed %v; want %s,%s and %s", withdrawn, notSent, p1.ID, p2.ID, ap.ID)
		}
		var nobody int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM assistant_cache_purge_requests
			WHERE id = ANY($1) AND state = 'withdrawn' AND decided_by_user_id IS NULL
			  AND decided_at IS NULL AND withdrawn_at IS NOT NULL`, []uuid.UUID{p1.ID, p2.ID}).Scan(&nobody); err != nil {
			return err
		}
		if nobody != 2 {
			t.Fatalf("withdrawn rows naming nobody: %d, want 2", nobody)
		}
		var by uuid.UUID
		var reason string
		if err := tx.QueryRow(ctx, `SELECT decided_by_user_id, not_sent_reason FROM assistant_cache_purge_requests
			WHERE id = $1 AND state = 'dispatched' AND outcome = 'not_sent'`, ap.ID).Scan(&by, &reason); err != nil {
			return err
		}
		if by != decider || reason != "grant_inactive" {
			t.Fatalf("closed approved row: decider=%s reason=%s, want %s grant_inactive", by, reason, decider)
		}
		var state string
		if err := tx.QueryRow(ctx, `SELECT state FROM assistant_cache_purge_requests WHERE id = $1`, untouched.ID).Scan(&state); err != nil {
			return err
		}
		if state != "pending" {
			t.Fatalf("another grant's row moved to %s", state)
		}
		// Idempotent: a second cascade finds nothing.
		again, err := q.WithdrawPendingAssistantCachePurgeRequestsForGrant(ctx,
			sqlc.WithdrawPendingAssistantCachePurgeRequestsForGrantParams{TenantID: tenant, ProposedByGrantID: grant})
		if err != nil || len(again) != 0 {
			t.Fatalf("repeat cascade withdrew %v (err=%v), want none", again, err)
		}
		return nil
	}); err != nil {
		t.Fatalf("cascade: %v", err)
	}
}

// ---------------------------------------------------------------------------
// THE AGENT CONTEXT SCANS AND CANNOT DECIDE
// ---------------------------------------------------------------------------

// TestAssistantCachePurgeAgentContextCanScanButCannotDecideAsAppRole is m133's
// twin. The read is asserted FIRST: a FOR SELECT policy and a missing policy
// both refuse the write, and only the read tells them apart.
func TestAssistantCachePurgeAgentContextCanScanButCannotDecideAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "m151-agent-"+uuid.NewString()[:8])
	site := seedSite(t, pool, tenant, "")
	decider := uuid.New()
	pending := acprInsert(t, pool, acprSitePrincipal(tenant, site), acprParams(tenant, site, uuid.New(), "agent-p"))
	approved := acprApprove(t, pool, acprInsert(t, pool, acprSitePrincipal(tenant, site),
		acprParams(tenant, site, uuid.New(), "agent-a")), decider)

	if err := pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InAgentTx (scan)")
		rows, err := sqlc.New(tx).ScanDueApprovedAssistantCachePurgeRequests(ctx,
			sqlc.ScanDueApprovedAssistantCachePurgeRequestsParams{BackoffCapSeconds: 300, BackoffBaseSeconds: 30, RowLimit: 100})
		if err != nil {
			return err
		}
		found := false
		for _, r := range rows {
			if r.ID == approved.ID && r.TenantID == tenant && r.SiteID == site {
				found = true
			}
		}
		if !found {
			t.Fatalf("the agent scan did not return approved row %s (%d rows); the FOR SELECT policy is missing", approved.ID, len(rows))
		}
		n, err := m133ExpectNoRows(t, tx, `UPDATE assistant_cache_purge_requests
			SET state = 'rejected', decided_at = now(), decided_by_user_id = $2 WHERE id = $1`, pending.ID, decider)
		if err != nil || n != 0 {
			t.Fatalf("DECISION LEAK: the agent context decided a request (rows=%d err=%v); the agent policy is not FOR SELECT", n, err)
		}
		n, err = m133ExpectNoRows(t, tx, `UPDATE assistant_cache_purge_requests
			SET state = 'dispatched', claimed_at = now(), outcome = 'not_sent',
			    not_sent_reason = 'site_absent', outcome_at = now() WHERE id = $1`, approved.ID)
		if err != nil || n != 0 {
			t.Fatalf("CLAIM LEAK: the agent context claimed an approved request (rows=%d err=%v)", n, err)
		}
		return nil
	}); err != nil {
		t.Fatalf("agent context: %v", err)
	}
}

// ---------------------------------------------------------------------------
// m1: THE LOCK STATEMENTS, AND THE RESERVATION STATEMENTS
// ---------------------------------------------------------------------------

// TestAssistantCachePurgeLockStatementsAsAppRole proves every advisory-lock
// statement runs as the app role with text arguments, the lifecycle try-lock
// answers false while a session-scoped holder has the key, and the uuid-bound
// form it replaced raises 42883 rather than taking any lock.
func TestAssistantCachePurgeLockStatementsAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "m151-lock-"+uuid.NewString()[:8])
	site := seedSite(t, pool, tenant, "")
	grant := uuid.New()

	if err := pool.RunTenantTx(ctx, acprSitePrincipal(tenant, site), func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "RunTenantTx (locks)")
		q := sqlc.New(tx)
		if err := q.TakeAssistantRequestXactLock(ctx, sqlc.TakeAssistantRequestXactLockParams{
			LockKey: "assistant_request_grant", LockID: grant.String()}); err != nil {
			t.Fatalf("grant lock: %v", err)
		}
		if err := q.TakeAssistantRequestXactLock(ctx, sqlc.TakeAssistantRequestXactLockParams{
			LockKey: "assistant_site_dispatch", LockID: site.String()}); err != nil {
			t.Fatalf("site dispatch lock: %v", err)
		}
		got, err := q.TryAssistantRequestXactLock(ctx, sqlc.TryAssistantRequestXactLockParams{
			LockKey: org.LifecycleLockKey, LockID: tenant.String()})
		if err != nil || !got {
			t.Fatalf("lifecycle try-lock with no holder: acquired=%t err=%v, want true", got, err)
		}
		err = m133ExpectRefused(t, tx, `SELECT pg_advisory_xact_lock(hashtext($1::uuid), hashtext($2::uuid))`, grant, site)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42883" {
			t.Fatalf("the uuid-bound lock form: want 42883 undefined_function, got %v", err)
		}
		t.Logf("uuid-bound hashtext refused 42883: %s", pgErr.Message)
		return nil
	}); err != nil {
		t.Fatalf("locks: %v", err)
	}

	// A session-scoped holder of the lifecycle key, as org.PurgeWorker holds it.
	holder, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire holder connection: %v", err)
	}
	defer holder.Release()
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_lock(hashtext($1), hashtext($2))`, org.LifecycleLockKey, tenant.String()); err != nil {
		t.Fatalf("session holder: %v", err)
	}
	if err := pool.RunTenantTx(ctx, acprSitePrincipal(tenant, site), func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "RunTenantTx (try-lock while held)")
		got, err := sqlc.New(tx).TryAssistantRequestXactLock(ctx, sqlc.TryAssistantRequestXactLockParams{
			LockKey: org.LifecycleLockKey, LockID: tenant.String()})
		if err != nil {
			return err
		}
		if got {
			t.Fatal("the lifecycle try-lock was acquired while a session-scoped holder had the key; the two do not share a lock")
		}
		return nil
	}); err != nil {
		t.Fatalf("try-lock while held: %v", err)
	}
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_unlock(hashtext($1), hashtext($2))`, org.LifecycleLockKey, tenant.String()); err != nil {
		t.Fatalf("release holder: %v", err)
	}
}

// TestAssistantCachePurgeReservationStatementsAsAppRole walks one approved
// request through the reservation and outcome statements under a single-site
// principal, and proves the organisation re-read, the busy check, the
// deadline predicate and the outcome compare-and-set.
func TestAssistantCachePurgeReservationStatementsAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "m151-res-"+uuid.NewString()[:8])
	site := seedSite(t, pool, tenant, "")
	approver := seedUser(t, pool, "m151-approver-"+uuid.NewString()[:8]+"@example.com", "Approver", true)
	grant := uuid.New()
	sp := acprSitePrincipal(tenant, site)
	row := acprApprove(t, pool, acprInsert(t, pool, sp, acprParams(tenant, site, grant, "res")), approver)

	if err := pool.RunTenantTx(ctx, sp, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "RunTenantTx (reservation)")
		q := sqlc.New(tx)
		life, err := q.GetTenantAssistantLifecycleForShare(ctx, tenant)
		if err != nil || life.AssistantPaused || life.Deleted {
			t.Fatalf("organisation re-read: %+v err=%v, want neither paused nor deleted", life, err)
		}
		busy, err := q.AssistantCachePurgeInFlightOnSite(ctx, sqlc.AssistantCachePurgeInFlightOnSiteParams{TenantID: tenant, SiteID: site})
		if err != nil || busy {
			t.Fatalf("busy before any reservation: %t err=%v", busy, err)
		}
		d, err := q.GetApprovedAssistantCachePurgeRequestForDispatch(ctx, sqlc.GetApprovedAssistantCachePurgeRequestForDispatchParams{
			DeadlineSeconds: 3600, TenantID: tenant, ID: row.ID})
		if err != nil || d.PastDeadline || d.AssistantCachePurgeRequest.ID != row.ID {
			t.Fatalf("dispatch read: %+v err=%v", d, err)
		}
		audit, err := q.InsertAssistantCachePurgeAudit(ctx, sqlc.InsertAssistantCachePurgeAuditParams{
			TenantID: tenant, SiteID: site, Kind: "all", ApproverUserID: approver,
			InitiatorGrantID: grant, TargetUrls: []string{}, UrlsCount: 0})
		if err != nil {
			return err
		}
		if !audit.InitiatorUserID.Valid || uuid.UUID(audit.InitiatorUserID.Bytes) != approver ||
			!audit.InitiatorGrantID.Valid || uuid.UUID(audit.InitiatorGrantID.Bytes) != grant {
			t.Fatalf("audit row initiators: user=%v grant=%v", audit.InitiatorUserID, audit.InitiatorGrantID)
		}
		// Deadline predicate: a zero-second deadline has already passed.
		if n, err := q.ReserveAssistantCachePurgeRequest(ctx, sqlc.ReserveAssistantCachePurgeRequestParams{
			CachePurgeAuditID: audit.ID, TenantID: tenant, ID: row.ID, DeadlineSeconds: 0}); err != nil || n != 0 {
			t.Fatalf("reservation past its deadline changed %d rows (err=%v), want 0", n, err)
		}
		if n, err := q.ReserveAssistantCachePurgeRequest(ctx, sqlc.ReserveAssistantCachePurgeRequestParams{
			CachePurgeAuditID: audit.ID, TenantID: tenant, ID: row.ID, DeadlineSeconds: 3600}); err != nil || n != 1 {
			t.Fatalf("reservation changed %d rows (err=%v), want 1", n, err)
		}
		if busy, err := q.AssistantCachePurgeInFlightOnSite(ctx, sqlc.AssistantCachePurgeInFlightOnSiteParams{TenantID: tenant, SiteID: site}); err != nil || !busy {
			t.Fatalf("busy after the reservation: %t err=%v, want true", busy, err)
		}
		hits, err := q.CountAssistantCachePurgesOnSiteSince(ctx, sqlc.CountAssistantCachePurgesOnSiteSinceParams{TenantID: tenant, SiteID: site, WindowSeconds: 3600})
		if err != nil || hits != 1 {
			t.Fatalf("AI clears on site in the hour: %d err=%v, want 1", hits, err)
		}
		if n, err := q.RecordAssistantCachePurgeOutcome(ctx, sqlc.RecordAssistantCachePurgeOutcomeParams{
			Outcome: "purged", HostingCachesCleared: []string{}, HostingCachesSkipped: []string{"kinsta"},
			OriginOnlyConfirmed: func() *bool { b := true; return &b }(), WpmgrCdn: acprStr("not_attempted"),
			TenantID: tenant, ID: row.ID}); err != nil || n != 1 {
			t.Fatalf("outcome changed %d rows (err=%v), want 1", n, err)
		}
		if n, err := q.RecordAssistantCachePurgeOutcome(ctx, sqlc.RecordAssistantCachePurgeOutcomeParams{
			Outcome: "outcome_unknown", TenantID: tenant, ID: row.ID}); err != nil || n != 0 {
			t.Fatalf("a second outcome changed %d rows (err=%v), want 0", n, err)
		}
		if busy, err := q.AssistantCachePurgeInFlightOnSite(ctx, sqlc.AssistantCachePurgeInFlightOnSiteParams{TenantID: tenant, SiteID: site}); err != nil || busy {
			t.Fatalf("busy after the outcome: %t err=%v, want false", busy, err)
		}
		// A deleted (or never-existing) approver is recorded as NULL.
		gone, err := q.InsertAssistantCachePurgeAudit(ctx, sqlc.InsertAssistantCachePurgeAuditParams{
			TenantID: tenant, SiteID: site, Kind: "url", ApproverUserID: uuid.New(),
			InitiatorGrantID: grant, TargetUrls: []string{"https://x.test/a"}, UrlsCount: 1})
		if err != nil || gone.InitiatorUserID.Valid {
			t.Fatalf("absent approver: initiator_user_id=%v err=%v, want NULL", gone.InitiatorUserID, err)
		}
		return nil
	}); err != nil {
		t.Fatalf("reservation: %v", err)
	}

	// Pause and soft-delete are seen by the organisation re-read.
	if _, err := pool.Exec(ctx, `UPDATE tenants SET assistant_paused_at = now(), assistant_paused_reason = 'test' WHERE id = $1`, tenant); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tenants SET deleted_at = now() WHERE id = $1`, tenant); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	if err := pool.RunTenantTx(ctx, sp, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "RunTenantTx (re-read after pause and delete)")
		life, err := sqlc.New(tx).GetTenantAssistantLifecycleForShare(ctx, tenant)
		if err != nil || !life.AssistantPaused || !life.Deleted {
			t.Fatalf("organisation re-read after pause and delete: %+v err=%v", life, err)
		}
		return nil
	}); err != nil {
		t.Fatalf("re-read: %v", err)
	}
}
