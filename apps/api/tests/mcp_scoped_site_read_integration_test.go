// mcp_scoped_site_read_integration_test.go: the assistant's site read is
// bounded over THE CALLER'S OWN SCOPE, and the m19 site-scope policy is a live
// second layer under it. Executed against the real schema as wpmgr_app.
//
// TWO SEPARATE CLAIMS LIVE HERE AND THEY FAIL INDEPENDENTLY.
//
//  1. THE BOUND. ListSitesForRead read ListSites over the whole tenant with
//     Limit = bound+1 and Sort = 'name', and ListSitesForModel applied the
//     connection's site scope in Go afterwards. An in-scope site whose name
//     sorted past the tenant's first page was never among the rows the filter
//     ran over, so a perfectly healthy site came back as a site_unread refusal
//     advising a retry -- advice that could never work, because the order is
//     deterministic and every call returns the same page. The first test seeds
//     the exact shape (600 sites, scope = the one that sorts LAST) and runs the
//     OLD read alongside the new one, so the difference is executed rather than
//     asserted from the commit message.
//
//  2. THE POLICY. That read opened with InTenantTx, which never sets
//     app.site_scope. The RESTRICTIVE sites_site_scope policy (m19,
//     20260531050000_m19_orgs_sharing.sql) is `app.site_scope <> 'on' OR
//     id = ANY(app.allowed_site_ids)`, so with the GUC unset its first disjunct
//     is true for every row and the policy does nothing. The Go filter in
//     ListSitesForModel was the only gate on the site axis. The second test
//     runs ONE predicate-free statement under both helpers and shows the row
//     count differ -- the policy alone, with the query's own site_ids predicate
//     deliberately out of the way, because a proof that cannot separate the two
//     cannot tell a live policy from a well-written WHERE clause.
//
// The role is load-bearing: wpmgr_app is NOSUPERUSER NOBYPASSRLS, and either
// privilege makes every assertion here vacuous. It is asserted and printed from
// INSIDE the transaction under test.
package tests

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
)

// scopedSiteReadFleetSize is the tenant size the first proof needs. It must
// exceed the service's own page bound (mcp.sitesPageBound, 500) or the defect
// cannot arise at all: the shortfall only appears once the TENANT holds more
// rows than one page.
const scopedSiteReadFleetSize = 600

// seedSitesBulk inserts n sites in one statement and returns the id of the one
// named lastName, which is seeded to sort LAST under `lower(name) ASC`.
//
// IT IS A FIXTURE AND IT IS DELIBERATELY NOT site.Repo.Create. Six hundred
// round trips would make this proof slow enough to be skipped and then deleted,
// which is how the page-bound test in this package became vacuous once already.
// The thing under test is the READ, and the read is reached through the
// production repo below; nothing about how the rows arrived is in question.
func seedSitesBulk(t *testing.T, pool *db.Pool, tenantID uuid.UUID, n int, lastName string) uuid.UUID {
	t.Helper()
	ctx := context.Background()

	var lastID uuid.UUID
	if err := pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		// n-1 fillers, all sorting before lastName.
		if _, err := tx.Exec(ctx, `
			INSERT INTO sites (tenant_id, url, name)
			SELECT $1,
			       'https://filler-' || lpad(i::text, 4, '0') || '.example.com',
			       'aaa-filler-' || lpad(i::text, 4, '0')
			FROM generate_series(1, $2) AS i`, tenantID, n-1); err != nil {
			return fmt.Errorf("insert fillers: %w", err)
		}
		return tx.QueryRow(ctx, `
			INSERT INTO sites (tenant_id, url, name)
			VALUES ($1, 'https://last.example.com', $2)
			RETURNING id`, tenantID, lastName).Scan(&lastID)
	}); err != nil {
		t.Fatalf("seed %d sites: %v", n, err)
	}
	return lastID
}

// TestMCPSiteReadReturnsTheLastSortingInScopeSiteAsAppRole is the case that was
// impossible before this change.
//
// 600 sites in one tenant; the connection is scoped to the SINGLE site that
// sorts last by name. Under the old tenant-wide read with a bound of 500 that
// site was not in the page, so the caller received zero sites and one
// site_unread refusal. The old read is executed here, in this test, so the
// number it produces is measured rather than quoted.
func TestMCPSiteReadReturnsTheLastSortingInScopeSiteAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	mcpRepo := mcp.NewRepo(pool)
	svc := mcp.NewService(mcpRepo).WithAudit(audit.NewRecorder(pool, domain.SystemClock{}))

	tenant := seedTenant(t, pool, "mcp-ssr-"+uuid.NewString()[:8])
	const lastName = "zzz-sorts-last"
	lastID := seedSitesBulk(t, pool, tenant, scopedSiteReadFleetSize, lastName)

	principal := domain.Principal{
		TenantID:       tenant,
		Scope:          domain.ScopeSite,
		AllowedSiteIDs: []uuid.UUID{lastID},
	}

	// ---- THE CONTROL: what the OLD read returned, executed ----
	//
	// ListSites over the whole tenant, Sort 'name', Limit bound+1, then the
	// scope filter in Go. This is the exact shape ListSitesForRead had, and it
	// is run here so "the old shape returned 0" is a number this test produced
	// rather than a claim inherited from a commit message.
	var oldShapeInScope int
	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (the OLD tenant-wide read)")
		rows, err := sqlc.New(tx).ListSites(ctx, sqlc.ListSitesParams{
			TenantID: tenant, Limit: 501, Offset: 0, Sort: "name"})
		if err != nil {
			return err
		}
		for _, r := range rows {
			if r.ID == lastID {
				oldShapeInScope++
			}
		}
		t.Logf("the OLD tenant-wide read returned %d rows and %d of them were in scope",
			len(rows), oldShapeInScope)
		return nil
	}); err != nil {
		t.Fatalf("run the old read shape: %v", err)
	}
	if oldShapeInScope != 0 {
		t.Fatalf("the old tenant-wide read found the in-scope site (%d rows). This proof needs "+
			"the defect to be reproducible in this fixture; if it is not, the fixture no "+
			"longer reproduces the bug and the assertions below prove nothing", oldShapeInScope)
	}

	// ---- THE READ UNDER TEST ----
	rows, more, err := mcpRepo.ListSitesForRead(ctx, principal, 500)
	if err != nil {
		t.Fatalf("ListSitesForRead as wpmgr_app: %v", err)
	}

	// THE ASSERTION THAT NAMES THE DEFECT, FIRST. A zero here is the shipped
	// bug: the caller's own site, unreachable at any number of retries.
	if len(rows) != 1 || rows[0].ID != lastID {
		t.Fatalf("SCOPE-BOUND DEFECT: the read returned %d rows for a connection scoped to the "+
			"single site that sorts LAST in a %d-site tenant, want exactly that site (%s). The "+
			"bound must be applied AFTER the scope predicate, not over the tenant; the old "+
			"shape returned %d in-scope rows for this same fixture",
			len(rows), scopedSiteReadFleetSize, lastID, oldShapeInScope)
	}
	if more {
		t.Fatalf("more=true for a caller scoped to 1 site under a bound of 500; `more` must be "+
			"a fact about the caller's own scope and never about the tenant")
	}
	if rows[0].Name != lastName {
		t.Fatalf("the returned site is named %q, want %q", rows[0].Name, lastName)
	}
	t.Logf("old shape: %d in-scope rows; new shape: %d rows, more=%v",
		oldShapeInScope, len(rows), more)

	// ---- AND THROUGH THE TOOL, WHERE site_unread WOULD HAVE APPEARED ----
	_, bearer := s7GrantWithBearer(t, mcpRepo, tenant, "list", []uuid.UUID{lastID})
	eng := mountLikeProduction(t, svc, domain.Principal{TenantID: tenant, Scope: domain.ScopeOrg})
	res := mcpRPC(t, eng, bearer, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params":  map[string]any{"name": mcp.ToolFleetSitesList, "arguments": map[string]any{}},
	})
	if res.status != 200 {
		t.Fatalf("tools/call status %d, body %s", res.status, res.body)
	}
	text := extractToolText(t, res.body)
	payload := decodeSitesPayload(t, text)

	// site_unread IS ASSERTED UNREACHABLE HERE, NOT ASSUMED. This is the exact
	// state that produced it: an in-scope site outside the tenant's first page.
	for _, r := range payload.Envelope.Refusals {
		if r.Code == "site_unread" {
			t.Fatalf("site_unread is still reachable for an in-scope site that sorts past the "+
				"TENANT's first page. The bound is meant to be taken over the caller's own "+
				"scope, which removes this cause entirely. Refusal: %+v", r)
		}
	}
	if payload.Envelope.Asked != 1 || payload.Envelope.OK != 1 || payload.Envelope.Refused != 0 {
		t.Fatalf("envelope asked/ok/refused = %d/%d/%d, want 1/1/0",
			payload.Envelope.Asked, payload.Envelope.OK, payload.Envelope.Refused)
	}
	if len(payload.Sites) != 1 || payload.Sites[0].ID != lastID.String() {
		t.Fatalf("the tool returned %d sites, want exactly %s", len(payload.Sites), lastID)
	}
	if payload.Truncation.Truncated {
		t.Fatalf("truncation.truncated is true for a caller that received every site it may "+
			"read:\n%s", text)
	}
	t.Logf("the tool returned the last-sorting in-scope site with envelope %d/%d/%d and no "+
		"site_unread", payload.Envelope.Asked, payload.Envelope.OK, payload.Envelope.Refused)
}

// TestMCPSitesSiteScopePolicyIsLiveNotJustTheGoFilterAsAppRole is part 2, and
// it is the test that would have caught the original defect.
//
// ONE STATEMENT, TWO HELPERS, AND THE STATEMENT CARRIES NO SCOPE PREDICATE OF
// ITS OWN. `SELECT count(*) FROM sites WHERE tenant_id = $1` is exactly the
// query a future caller writes when it forgets the site_ids parameter, and it
// is the only shape that can tell a LIVE POLICY apart from a well-written WHERE
// clause. Run under InTenantTx it sees the tenant; run under the dispatch a
// site-constrained principal takes, the policy must cut it to the allowlist.
//
// If these two numbers are ever equal, the m19 policy is inert on this path and
// the Go filter in ListSitesForModel is the only thing standing between a
// scoped connection and the whole fleet -- which is the state this change ends.
func TestMCPSitesSiteScopePolicyIsLiveNotJustTheGoFilterAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)

	tenant := seedTenant(t, pool, "mcp-pol-"+uuid.NewString()[:8])
	const fleet = 12
	inScope := seedSitesBulk(t, pool, tenant, fleet, "zzz-in-scope")

	// The SAME statement both times. No site_ids, no id filter, nothing but the
	// tenant -- so every difference below is the policy's doing.
	const predicateFree = "SELECT count(*) FROM sites WHERE tenant_id = $1"

	// ---- GUC UNSET: the policy's first disjunct is true for every row ----
	var unscopedCount int
	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (app.site_scope UNSET)")
		return tx.QueryRow(ctx, predicateFree, tenant).Scan(&unscopedCount)
	}); err != nil {
		t.Fatalf("predicate-free read under InTenantTx: %v", err)
	}
	if unscopedCount != fleet {
		t.Fatalf("the unscoped read saw %d sites, want the tenant's %d; without this the "+
			"comparison below has no baseline", unscopedCount, fleet)
	}

	// ---- GUC SET: through db.Pool.RunTenantTx with the principal
	// mcp.connectionScopedPrincipal builds, which is the identical dispatch
	// Repo.ListSitesForRead takes. ----
	principal := domain.Principal{
		TenantID:       tenant,
		Scope:          domain.ScopeSite,
		AllowedSiteIDs: []uuid.UUID{inScope},
	}
	var scopedCount int
	var guc string
	if err := pool.RunTenantTx(ctx, principal, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "RunTenantTx (app.site_scope ON)")
		if err := tx.QueryRow(ctx,
			"SELECT current_setting('app.site_scope', true)").Scan(&guc); err != nil {
			return err
		}
		return tx.QueryRow(ctx, predicateFree, tenant).Scan(&scopedCount)
	}); err != nil {
		t.Fatalf("predicate-free read under RunTenantTx: %v", err)
	}

	// THE ASSERTION THAT NAMES THE LEAK, FIRST -- ahead of the GUC check and
	// ahead of the exact-count check. Behind either of them it would not run on
	// the failure that matters: a transaction that set no GUC still returns the
	// whole tenant here, and the GUC check would have failed the test with the
	// leak assertion never reached.
	if scopedCount == unscopedCount {
		t.Fatalf("POLICY INERT: a predicate-free read saw all %d of the tenant's sites inside "+
			"the SITE-SCOPED transaction, the same number an unscoped transaction saw. "+
			"sites_site_scope (m19) is RESTRICTIVE on `app.site_scope <> 'on' OR id = ANY(...)`, "+
			"so an equal count means the GUC was never set (app.site_scope=%q) and the database "+
			"is enforcing nothing on the site axis -- the Go filter in ListSitesForModel is the "+
			"only gate", unscopedCount, guc)
	}
	if guc != "on" {
		t.Fatalf("app.site_scope = %q inside the scoped transaction, want \"on\"", guc)
	}
	if scopedCount != 1 {
		t.Fatalf("the scoped predicate-free read saw %d sites, want exactly the 1 in the "+
			"allowlist", scopedCount)
	}
	t.Logf("BEHAVIOURAL DIFFERENCE: the identical statement saw %d sites under InTenantTx and "+
		"%d under the scoped dispatch (app.site_scope=%q)", unscopedCount, scopedCount, guc)
}

// TestMCPSiteReadArchivedInScopeSiteIsAccountedAsAppRole records the ONE
// residual cause of site_unread on this path, because "unreachable" was the
// expectation and it is not quite true.
//
// ResolveMCPGrantScopeSitesInTenantTx has no archived predicate, so an ARCHIVED
// site named in a grant's scope resolves into auth.Sites. ListSitesForMCPScope
// excludes archived rows, matching the dashboard's default (ADR-041) and
// matching what ListSites gave this surface before. The site is therefore in
// scope and unread, which is exactly what site_unread is for.
//
// WHAT THIS TEST PINS IS THE HONESTY OF THE MESSAGE. The old detail told the
// caller to "retry", and no retry can ever restore an archived site. The
// shortfall must be accounted (ok+refused == asked, never a short list reading
// as a complete fleet) and the advice must not be a retry.
func TestMCPSiteReadArchivedInScopeSiteIsAccountedAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	mcpRepo := mcp.NewRepo(pool)
	svc := mcp.NewService(mcpRepo).WithAudit(audit.NewRecorder(pool, domain.SystemClock{}))

	tenant := seedTenant(t, pool, "mcp-arch-"+uuid.NewString()[:8])
	liveID := seedSitesBulk(t, pool, tenant, 2, "aaa-live")
	var archivedID uuid.UUID
	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO sites (tenant_id, url, name, connection_state)
			VALUES ($1, 'https://archived.example.com', 'bbb-archived', 'archived')
			RETURNING id`, tenant).Scan(&archivedID)
	}); err != nil {
		t.Fatalf("seed the archived site: %v", err)
	}

	_, bearer := s7GrantWithBearer(t, mcpRepo, tenant, "list", []uuid.UUID{liveID, archivedID})
	eng := mountLikeProduction(t, svc, domain.Principal{TenantID: tenant, Scope: domain.ScopeOrg})
	res := mcpRPC(t, eng, bearer, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params":  map[string]any{"name": mcp.ToolFleetSitesList, "arguments": map[string]any{}},
	})
	if res.status != 200 {
		t.Fatalf("tools/call status %d, body %s", res.status, res.body)
	}
	payload := decodeSitesPayload(t, extractToolText(t, res.body))

	// ACCOUNTED, NOT DROPPED. A short list that reads as a complete fleet is
	// the failure this envelope exists to prevent.
	if payload.Envelope.OK+payload.Envelope.Refused != payload.Envelope.Asked {
		t.Fatalf("envelope does not balance: %d + %d != %d; an in-scope site went missing "+
			"without being accounted for",
			payload.Envelope.OK, payload.Envelope.Refused, payload.Envelope.Asked)
	}
	if payload.Envelope.Asked != 2 || payload.Envelope.OK != 1 || payload.Envelope.Refused != 1 {
		t.Fatalf("envelope asked/ok/refused = %d/%d/%d, want 2/1/1",
			payload.Envelope.Asked, payload.Envelope.OK, payload.Envelope.Refused)
	}
	if len(payload.Envelope.Refusals) != 1 ||
		payload.Envelope.Refusals[0].SiteID != archivedID.String() ||
		payload.Envelope.Refusals[0].Code != "site_unread" {
		t.Fatalf("refusals = %+v, want one site_unread naming %s",
			payload.Envelope.Refusals, archivedID)
	}

	// THE ADVICE MUST NOT BE A RETRY. The shortfall is deterministic: the same
	// page comes back every time, so "retry" sends the caller round a loop that
	// cannot terminate.
	detail := payload.Envelope.Refusals[0].Detail
	if strings.Contains(strings.ToLower(detail), "retry, and report it") {
		t.Fatalf("MISLEADING REFUSAL: site_unread still advises a bare retry. The page is "+
			"bounded over the caller's own scope in a fixed order, so retrying returns the "+
			"same page. Detail: %q", detail)
	}
	if !strings.Contains(strings.ToLower(detail), "archived") {
		t.Fatalf("site_unread does not name archiving as a cause, so an operator reading it "+
			"has nothing to act on. Detail: %q", detail)
	}
	t.Logf("archived in-scope site accounted as site_unread with actionable detail: %q", detail)
}
