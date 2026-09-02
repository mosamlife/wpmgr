// mcp_layer3_hiding_integration_test.go: LAYER 3 HIDES, and it hides through
// the mounted transport, as wpmgr_app, against the real schema.
//
// WHY THIS FILE EXISTS SEPARATELY FROM THE UNIT PROOFS. The mcp package's own
// tests drive a fake store and construct AuthorizedRequest by hand, so they can
// establish the ENVELOPE'S ARITHMETIC but not the thing that matters here: that
// a site outside a connection's resolved scope is absent from what a real
// client receives over HTTP. With a fake store, "site B is invisible" holds
// because the fake never returned it, which proves nothing about the code.
//
// THE PROPERTY UNDER TEST IS NOT "B IS FILTERED". It is stronger, and the
// difference is the whole reason layer 3 is different from the other six:
//
//	A site outside scope is not greyed with a reason and not counted. It is
//	ABSENT -- from every field, and from every arithmetic relationship BETWEEN
//	the fields. "You cannot touch clientname.com" already tells the asker that
//	clientname.com exists and is one of ours. Presence-only exists so that a
//	capability gap is legible; a tenancy boundary must leak nothing, including
//	the existence of a site.
//
// So this file asserts on EXACT NUMBERS rather than on shapes. A test that
// only checked "B's id is not in the body" would pass against an
// implementation that reported asked=2 -- and a caller who can read asked=2
// while seeing one site has been told a second site exists.
//
// THE ROLE IS LOAD-BEARING. wpmgr_app is NOSUPERUSER NOBYPASSRLS; either
// privilege would make the scope resolution pass vacuously. It is asserted and
// printed from inside an InTenantTx on the SAME POOL the read uses -- not from
// inside the read's own transaction, which this comment previously claimed. The
// two are equivalent in effect because the pool and the helper are the same,
// but the weaker claim is the true one and the stronger one invited nobody to
// check.
package tests

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/govcontext"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

// layer3Envelope is the envelope as a CLIENT sees it -- decoded from the wire,
// not borrowed from the package's own type. Decoding into a local struct is
// deliberate: mcp.Envelope's UnmarshalText would REFUSE an unknown refusal
// code, so decoding through it could turn a leak that names an unexpected code
// into a decode error, and the test would fail for the wrong reason and be
// "fixed" by relaxing the decoder. Plain strings here see whatever arrived.
type layer3Envelope struct {
	Asked    int `json:"asked"`
	OK       int `json:"ok"`
	Refused  int `json:"refused"`
	Refusals []struct {
		SiteID string `json:"site_id"`
		Code   string `json:"code"`
		// Detail is decoded because it is CUSTOMER-FACING TEXT that a model
		// acts on. A refusal whose advice cannot work ("retry" against a
		// deterministic page) is a defect in the same way a wrong number is,
		// and it is only testable if the wire text is read back.
		Detail string `json:"detail"`
	} `json:"refusals"`
}

type layer3Payload struct {
	Sites []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		URL  string `json:"url"`
	} `json:"sites"`
	Envelope   layer3Envelope `json:"envelope"`
	Truncation struct {
		Truncated bool `json:"truncated"`
		Returned  int  `json:"returned"`
		Available *int `json:"available"`
	} `json:"truncation"`
}

// EVERY ABSENCE ASSERTION BELOW IS MADE AGAINST THE RAW RESPONSE BODY, not
// against the decoded struct. A decoded struct can only be searched for fields
// the test thought to declare, so a leak in a field nobody anticipated would
// decode away silently. The raw body cannot hide one.

// TestMCPLayer3OutOfScopeSiteIsAbsentAndNotInferableAsAppRole is the A/B proof.
//
// One tenant, two sites. The connection is scoped to A only. B must be absent
// from every field, and no combination of the returned numbers may reveal that
// B exists.
//
// BOTH SITES ARE IN THE SAME TENANT, and that is the point rather than an
// oversight. Cross-TENANT isolation is already proved by RLS in
// adr064_s6b2_mcp_transport_rls_test.go. Layer 3 is the WITHIN-tenant boundary:
// RLS returns both rows to this transaction quite correctly, and the only thing
// standing between site B and the wire is the scope filter under test. A
// two-tenant fixture here would have RLS silently doing the work and would pass
// with the filter deleted.
func TestMCPLayer3OutOfScopeSiteIsAbsentAndNotInferableAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	mcpRepo := mcp.NewRepo(pool)
	siteRepo := site.NewRepo(pool)
	// THE CONTEXT RESOLVER IS WIRED BECAUSE PRODUCTION WIRES IT: both non-test
	// call sites of mcp.NewService (cmd/wpmgr/main.go, cmd/dump-routes/routes.go)
	// chain WithContextResolver, and a Service without one refuses every fleet
	// tool call rather than serving with the operator's governance silently
	// absent. Omitting it here would make the tools/call below refuse and this
	// test would prove nothing about what it is named for. It is the REAL
	// govcontext.Repo over the same pool, so the organisation-context read runs
	// through InTenantTx as wpmgr_app under the same RLS policies as every other
	// read in this file.
	svc := mcp.NewService(mcpRepo).WithAudit(audit.NewRecorder(pool, domain.SystemClock{})).
		WithContextResolver(&govcontext.Resolver{Store: govcontext.NewRepo(pool)})

	tenant := seedTenant(t, pool, "mcp-l3-"+uuid.NewString()[:8])

	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (fleet_sites_list read path)")
		return nil
	}); err != nil {
		t.Fatalf("open tenant tx: %v", err)
	}

	siteA, err := siteRepo.Create(ctx, site.CreateInput{
		TenantID: tenant, URL: "https://in-scope.example.com", Name: "aaa-in-scope"})
	if err != nil {
		t.Fatalf("create site A: %v", err)
	}
	// The out-of-scope site is given a DISTINCTIVE name and host so that a
	// substring search over the whole response body is a meaningful test. A
	// generic fixture name risks colliding with instruction text and turning a
	// real leak into a passing assertion.
	siteB, err := siteRepo.Create(ctx, site.CreateInput{
		TenantID: tenant, URL: "https://zzz-secret-client.example.com", Name: "zzz-secret-client"})
	if err != nil {
		t.Fatalf("create site B: %v", err)
	}

	// Scoped to A ONLY. B is in the same tenant and RLS will happily return it.
	_, bearer := s7GrantWithBearer(t, mcpRepo, tenant, "list", []uuid.UUID{siteA.ID})

	auth, err := svc.Authenticate(ctx, bearer)
	if err != nil {
		t.Fatalf("Authenticate with a live bearer: %v", err)
	}
	if auth.Sites.Len() != 1 || !auth.Sites.Allows(siteA.ID) || auth.Sites.Allows(siteB.ID) {
		t.Fatalf("scope resolved to %d sites, Allows(A)=%v Allows(B)=%v; want exactly A",
			auth.Sites.Len(), auth.Sites.Allows(siteA.ID), auth.Sites.Allows(siteB.ID))
	}

	// A TENANT TRANSACTION GENUINELY SEES BOTH ROWS. Asserting it here is what
	// makes the rest of the test meaningful: it establishes that hiding B is
	// work the scope layers must do, not something tenant RLS already did.
	//
	// This is read through pool.InTenantTx rather than through the repo BECAUSE
	// the repo method is no longer capable of a tenant-wide read -- which is
	// itself the change under test. It is a fixture assertion about what the
	// tenant holds, never a substitute for exercising the production path.
	var tenantSiteCount int
	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (fixture: what the tenant holds)")
		return tx.QueryRow(ctx,
			"SELECT count(*) FROM sites WHERE tenant_id = $1", tenant).Scan(&tenantSiteCount)
	}); err != nil {
		t.Fatalf("count the tenant's sites: %v", err)
	}
	if tenantSiteCount != 2 {
		t.Fatalf("the tenant holds %d sites; this proof needs both visible to a tenant "+
			"transaction, otherwise hiding B is not work anything had to do", tenantSiteCount)
	}

	// AND THE SCOPED READ RETURNS ONLY A. This is the database doing the
	// hiding, before the Go filter below ever runs: the query's site_ids
	// predicate and the RESTRICTIVE sites_site_scope policy both drop B.
	rows, _, err := mcpRepo.ListSitesForRead(ctx,
		domain.Principal{TenantID: tenant, Scope: domain.ScopeSite,
			AllowedSiteIDs: []uuid.UUID{siteA.ID}}, 500)
	if err != nil {
		t.Fatalf("ListSitesForRead: %v", err)
	}
	for _, r := range rows {
		if r.ID == siteB.ID {
			t.Fatalf("SCOPE LEAK AT THE DATABASE: the scoped read returned out-of-scope site %s",
				siteB.ID)
		}
	}
	if len(rows) != 1 {
		t.Fatalf("the scoped read returned %d rows, want exactly the 1 site in scope", len(rows))
	}
	t.Logf("the tenant holds %d sites; the scoped read returned %d", tenantSiteCount, len(rows))

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

	// ---- ABSENCE, ASSERTED OVER THE WHOLE RESPONSE BODY ----
	//
	// This is the assertion that would catch a leak in a field this test does
	// not know about, including one added later.
	if strings.Contains(res.body, siteB.ID.String()) {
		t.Fatalf("LAYER 3 LEAK: out-of-scope site id %s appears in the response body:\n%s",
			siteB.ID, res.body)
	}
	if strings.Contains(res.body, "zzz-secret-client") {
		t.Fatalf("LAYER 3 LEAK: the out-of-scope site's name or host appears in the response body:\n%s",
			res.body)
	}

	// ---- THE NUMBERS, ASSERTED EXACTLY ----
	text := extractToolText(t, res.body)
	payload := decodeSitesPayload(t, text)

	if payload.Envelope.Asked != 1 {
		t.Fatalf("INFERENCE LEAK: envelope.asked = %d, want exactly 1. asked must count the "+
			"CALLER'S OWN resolved scope; any larger number tells the caller that sites exist "+
			"which it may not see, which is the same disclosure as naming them",
			payload.Envelope.Asked)
	}
	if payload.Envelope.OK != 1 {
		t.Fatalf("envelope.ok = %d, want exactly 1", payload.Envelope.OK)
	}
	if payload.Envelope.Refused != 0 || len(payload.Envelope.Refusals) != 0 {
		t.Fatalf("INFERENCE LEAK: envelope.refused = %d with %d refusals, want 0. An "+
			"out-of-scope site must not be reported as a refusal: a refusal is a disclosure "+
			"that the site exists. Refusals: %+v",
			payload.Envelope.Refused, len(payload.Envelope.Refusals), payload.Envelope.Refusals)
	}
	if payload.Envelope.OK+payload.Envelope.Refused != payload.Envelope.Asked {
		t.Fatalf("envelope does not balance: %d + %d != %d; a residual is a site the caller "+
			"can subtract for", payload.Envelope.OK, payload.Envelope.Refused, payload.Envelope.Asked)
	}

	// The site list itself, and the truncation block, must agree with asked.
	if len(payload.Sites) != 1 || payload.Sites[0].ID != siteA.ID.String() {
		t.Fatalf("sites = %+v, want exactly site A (%s)", payload.Sites, siteA.ID)
	}
	if payload.Truncation.Returned != 1 {
		t.Fatalf("truncation.returned = %d, want 1", payload.Truncation.Returned)
	}
	if payload.Truncation.Available == nil || *payload.Truncation.Available != 1 {
		t.Fatalf("INFERENCE LEAK: truncation.available = %v, want 1. available counted over the "+
			"TENANT rather than over the caller's scope discloses the tenant's size",
			payload.Truncation.Available)
	}

	// NO COUNT FIELD ANYWHERE IN THE PAYLOAD MAY EQUAL THE TENANT'S SITE
	// COUNT. This is the guard against a FOURTH count field arriving later
	// and quietly carrying tenant cardinality; the three above are only the
	// ones that exist today.
	//
	// IT WALKS DECODED JSON NUMBERS, NOT THE TEXT. A substring search for the
	// tenant count was tried first and is wrong: small integers occur inside
	// uuids and inside byte_cap (30720 contains "2"), so it reddened a
	// correct result on its first run. A guard that fails on correct work
	// gets switched off, and then it guards nothing.
	//
	// byte_cap is excluded by name because it is a compile-time constant that
	// has nothing to do with the fleet, and would otherwise collide by
	// coincidence for any tenant holding 30720 sites.
	//
	// IT COMPARES AGAINST tenantSiteCount AND NOT len(rows), AND THE
	// DIFFERENCE IS WHETHER THIS GUARD CAN FIRE AT ALL. It read `len(rows)`
	// while `rows` was the TENANT-WIDE read; scoping that read (#671) made
	// len(rows) the CALLER'S count, which is payload.Envelope.Asked. The two
	// conditions then became mutually exclusive -- `n == len(rows) && n !=
	// Asked` is unsatisfiable when len(rows) == Asked -- and the guard could
	// not fire on any input. It was not written wrong; a correct change
	// elsewhere silently emptied it, which is why it is now proven to fire by
	// planting a field rather than by being read.
	//
	// tenantSiteCount is counted at the top of this test through InTenantTx and
	// is the value the guard was always about: what the TENANT holds, which
	// this caller must not be able to learn.
	// THE GUARD REFUSES TO BE DEAD. Its two conditions are `n == tenantSiteCount`
	// and `n != Asked`, which are mutually exclusive -- and the loop therefore
	// unsatisfiable on every input -- whenever the fixture makes those two
	// numbers equal. That is precisely how this guard spent a period being
	// green on everything, so the precondition is asserted rather than assumed.
	// A fixture that drifts into tenantSiteCount == Asked now fails loudly
	// instead of quietly guarding nothing.
	if tenantSiteCount == payload.Envelope.Asked {
		t.Fatalf("this guard cannot fire: the tenant holds %d sites and the caller's scope is "+
			"also %d, so `n == tenantSiteCount && n != Asked` is unsatisfiable. The fixture must "+
			"keep the tenant strictly larger than the caller's scope for the sweep below to "+
			"distinguish anything", tenantSiteCount, payload.Envelope.Asked)
	}
	for field, n := range numericFields(t, text) {
		if field == "byte_cap" {
			continue
		}
		if n == tenantSiteCount && n != payload.Envelope.Asked {
			t.Errorf("INFERENCE LEAK: field %q = %d, which is the TENANT's site count while the "+
				"caller's scope is %d. A count field carrying tenant cardinality discloses that "+
				"sites exist which the caller may not see:\n%s",
				field, n, payload.Envelope.Asked, text)
		}
	}

	t.Logf("layer 3 holds: asked=%d ok=%d refused=%d, and site B is absent from %d bytes of body",
		payload.Envelope.Asked, payload.Envelope.OK, payload.Envelope.Refused, len(res.body))
}

// TestMCPPageBoundDoesNotDiscloseTenantSizeAsAppRole is the regression test for
// the disclosure fixed in this branch. IT SHIPPED, so it must not ship twice.
//
// THE DEFECT. ListSitesForModel read a TENANT-WIDE bounded page and passed the
// page-bound flag ("this tenant has rows beyond the bound") straight into the
// rendered result, AFTER the grant's site scope had been applied. A connection
// scoped to a couple of sites in a large tenant therefore received every site
// it may read AND a notice that further sites had been withheld -- false for
// that caller, and enough to infer that the tenant holds more sites than the
// page bound.
//
// THE BOUND IS INJECTED INTO THE SERVICE, NOT JUST INTO A PRE-CHECK, AND THE
// DIFFERENCE IS THE WHOLE VALUE OF THIS TEST.
//
// The first version of this proof seeded six sites, called
// mcpRepo.ListSitesForRead(ctx, tenant, 3) to "establish the bound is
// exceeded", and then invoked the tool. That standalone call fed nothing: the
// tool used the hard-coded sitesPageBound of 500, six sites never came near
// it, and every assertion below passed identically with the disclosure fully
// restored in production code. It read as protection and was vacuous.
//
// svc.WithPageBound(3) puts the small bound on the path under test, so the
// production read genuinely runs against a tenant that exceeds it. Seeding 501
// sites would reach the same state and make the test minutes long, which is how
// a slow proof gets skipped and then deleted.
//
// WHAT CHANGED UNDER THIS TEST, AND WHY IT IS KEPT ANYWAY. The read is now
// bounded over the CALLER'S OWN SCOPE (ListSitesForMCPScope), so `more` inside
// ListSitesForModel is false here and the tenant's row count is no longer
// measured against the page bound at all. The disclosure this test names is
// therefore structurally out of reach rather than merely discarded in Go. The
// test stays because "out of reach" is a property of the current query, and one
// signature change -- a caller passing the tenant's sites as the allowlist, a
// helper reverting to ListSites -- puts it back within reach with every other
// proof in this package still green. The scoped assertion added below is what
// pins the structural half.
func TestMCPPageBoundDoesNotDiscloseTenantSizeAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	mcpRepo := mcp.NewRepo(pool)
	siteRepo := site.NewRepo(pool)
	// WithPageBound(3) is what makes this proof non-vacuous. See the note above.
	// THE CONTEXT RESOLVER IS WIRED BECAUSE PRODUCTION WIRES IT: both non-test
	// call sites of mcp.NewService (cmd/wpmgr/main.go, cmd/dump-routes/routes.go)
	// chain WithContextResolver, and a Service without one refuses every fleet
	// tool call rather than serving with the operator's governance silently
	// absent. Omitting it here would make the tools/call below refuse and this
	// test would prove nothing about what it is named for. It is the REAL
	// govcontext.Repo over the same pool, so the organisation-context read runs
	// through InTenantTx as wpmgr_app under the same RLS policies as every other
	// read in this file.
	svc := mcp.NewService(mcpRepo).
		WithAudit(audit.NewRecorder(pool, domain.SystemClock{})).
		WithPageBound(3).
		WithContextResolver(&govcontext.Resolver{Store: govcontext.NewRepo(pool)})

	tenant := seedTenant(t, pool, "mcp-pb-"+uuid.NewString()[:8])

	// A tenant big enough that a small bound is genuinely exceeded.
	var ids []uuid.UUID
	for i := 0; i < 6; i++ {
		s, err := siteRepo.Create(ctx, site.CreateInput{
			TenantID: tenant,
			URL:      "https://pb" + strconv.Itoa(i) + ".example.com",
			Name:     "pb-" + strconv.Itoa(i),
		})
		if err != nil {
			t.Fatalf("create site %d: %v", i, err)
		}
		ids = append(ids, s.ID)
	}

	// Confirm the TENANT would exceed the bound the service was given, at the
	// SAME value. This is a corroboration of the injected bound, not a
	// substitute for injecting it: on its own it proves nothing about the tool,
	// which is exactly how the first version of this test came to be vacuous.
	//
	// It is counted through InTenantTx rather than through the repo because the
	// repo can no longer take a tenant-wide read at all -- see below.
	var tenantSiteCount int
	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			"SELECT count(*) FROM sites WHERE tenant_id = $1", tenant).Scan(&tenantSiteCount)
	}); err != nil {
		t.Fatalf("count the tenant's sites: %v", err)
	}
	if tenantSiteCount <= 3 {
		t.Fatalf("the tenant holds %d sites; this proof needs it to exceed the injected bound of 3",
			tenantSiteCount)
	}

	// The grant is scoped to ONE site.
	_, bearer := s7GrantWithBearer(t, mcpRepo, tenant, "list", []uuid.UUID{ids[0]})

	// AND THE CALLER'S OWN PAGE IS NOT BOUNDED, which is the structural half of
	// the fix rather than a restatement of the assertions below. The tenant
	// exceeds the bound; this caller's scope does not, so `more` is false and
	// there is no page-bound fact about this call in the first place. While the
	// bound was applied over the tenant, `more` was true here and the only
	// thing standing between it and the caller was Go code choosing to discard
	// it.
	scoped, more, err := mcpRepo.ListSitesForRead(ctx,
		domain.Principal{TenantID: tenant, Scope: domain.ScopeSite,
			AllowedSiteIDs: []uuid.UUID{ids[0]}}, 3)
	if err != nil {
		t.Fatalf("ListSitesForRead scoped: %v", err)
	}
	if more {
		t.Fatalf("DISCLOSURE REGRESSION: more=true for a caller scoped to 1 site under a bound "+
			"of 3 in a %d-site tenant. `more` must be a fact about the CALLER'S OWN scope; a "+
			"true here is the tenant's row count reaching a page bound it should never have "+
			"been measured against", tenantSiteCount)
	}
	if len(scoped) != 1 {
		t.Fatalf("the scoped read returned %d rows, want exactly the 1 site in scope", len(scoped))
	}
	t.Logf("the tenant holds %d sites (bound 3, exceeded); the caller's scoped page holds %d "+
		"with more=%v", tenantSiteCount, len(scoped), more)

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

	// The caller got the one site it may read, and was told nothing about the
	// other five.
	if payload.Envelope.Asked != 1 || payload.Envelope.OK != 1 || payload.Envelope.Refused != 0 {
		t.Fatalf("envelope asked/ok/refused = %d/%d/%d, want 1/1/0",
			payload.Envelope.Asked, payload.Envelope.OK, payload.Envelope.Refused)
	}

	// THE REGRESSION ASSERTION. The old code emitted truncated=true and
	// available=null here, and a banner saying further sites were withheld.
	if payload.Truncation.Truncated {
		t.Fatalf("DISCLOSURE REGRESSION: truncation.truncated is true for a caller that received "+
			"every site it may read. This is the tenant's page bound leaking through the scope "+
			"filter, which is the defect this test exists for:\n%s", text)
	}
	if payload.Truncation.Available == nil {
		t.Fatalf("DISCLOSURE REGRESSION: truncation.available is null, which the old code emitted "+
			"to signal 'the tenant has more rows than this page'. For a scoped caller the "+
			"available count is knowable and is 1:\n%s", text)
	}
	if *payload.Truncation.Available != 1 {
		t.Fatalf("DISCLOSURE REGRESSION: truncation.available = %d, want 1 (the caller's own "+
			"scope). Any other number counts sites the caller may not see",
			*payload.Truncation.Available)
	}
	if strings.Contains(text, "INCOMPLETE RESULT") || strings.Contains(text, "PARTIAL RESULT") {
		t.Fatalf("DISCLOSURE REGRESSION: a complete-for-this-caller result carries an "+
			"incompleteness banner, which tells the caller sites were withheld:\n%s", text)
	}

	// None of the five out-of-scope sites may appear anywhere.
	for _, id := range ids[1:] {
		if strings.Contains(res.body, id.String()) {
			t.Fatalf("LAYER 3 LEAK: out-of-scope site %s appears in the response body", id)
		}
	}
	t.Logf("page bound did not disclose tenant size: asked=%d truncated=%v available=%d",
		payload.Envelope.Asked, payload.Truncation.Truncated, *payload.Truncation.Available)
}

// ---------------------------------------------------------------------------
// Decoding helpers
// ---------------------------------------------------------------------------

// extractToolText pulls the tool result text out of a JSON-RPC response.
//
// It FAILS rather than returning "" when the shape is not what it expects. A
// helper that returns an empty string on a surprise turns every absence
// assertion above into a vacuous pass -- the exact "a guard that finds nothing
// must go red, not green" failure this project is governed by.
func extractToolText(t *testing.T, body string) string {
	t.Helper()
	var env struct {
		Result *struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("response is not JSON-RPC: %v\n%s", err, body)
	}
	if env.Error != nil {
		t.Fatalf("tools/call returned a JSON-RPC error %d: %s", env.Error.Code, env.Error.Message)
	}
	if env.Result == nil || len(env.Result.Content) == 0 {
		t.Fatalf("tools/call returned no content: %s", body)
	}
	if env.Result.IsError {
		t.Fatalf("tools/call returned isError=true: %s", body)
	}
	return env.Result.Content[0].Text
}

// numericFields walks the decoded payload and returns every integer-valued
// field by name, at any depth. It exists so the tenant-cardinality guard can
// be structural rather than a substring search over the rendered text.
func numericFields(t *testing.T, text string) map[string]int {
	t.Helper()
	i := strings.Index(text, `{"sites"`)
	if i < 0 {
		t.Fatalf("tool text carries no sites payload:\n%s", text)
	}
	var raw any
	if err := json.Unmarshal([]byte(text[i:]), &raw); err != nil {
		t.Fatalf("sites payload is not JSON: %v", err)
	}
	out := map[string]int{}
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		switch tv := v.(type) {
		case map[string]any:
			for k, child := range tv {
				walk(k, child)
			}
		case []any:
			for _, child := range tv {
				walk(prefix, child)
			}
		case float64:
			// Only whole numbers are counts. A fractional value is a
			// measurement (an age, a ratio) and cannot be a site cardinality.
			if tv == float64(int(tv)) {
				out[prefix] = int(tv)
			}
		}
	}
	walk("", raw)
	return out
}

// decodeSitesPayload finds the JSON object in the tool text, which is preceded
// by prepended instruction and banner text.
func decodeSitesPayload(t *testing.T, text string) layer3Payload {
	t.Helper()
	i := strings.Index(text, `{"sites"`)
	if i < 0 {
		t.Fatalf("tool text carries no sites payload:\n%s", text)
	}
	var p layer3Payload
	if err := json.Unmarshal([]byte(text[i:]), &p); err != nil {
		t.Fatalf("sites payload is not JSON: %v\n%s", err, text[i:])
	}
	return p
}
