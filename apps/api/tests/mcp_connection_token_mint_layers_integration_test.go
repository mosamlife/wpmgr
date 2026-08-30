package tests

// Integration proofs for the connection-token mint's LAYERS, alongside the
// end-to-end proofs in mcp_connection_token_mint_integration_test.go.
//
// WHY A SECOND FILE. The end-to-end proofs drive the mounted route, so every
// layer is stacked and a passing test cannot say WHICH layer refused. The
// proofs here each remove the layers above the one under test:
//
//   - the RESTRICTIVE mcp_grants_site_scope_insert policy, reached with BOTH
//     app-layer guards bypassed, because a policy that is only ever exercised
//     behind two guards that already refuse is a policy nothing would notice
//     the loss of;
//   - the transaction boundary, by making the second insert and then the audit
//     append fail on purpose;
//   - the ACTOR on the audit row, for the API-key caller that is this headless
//     endpoint's natural one.
//
// EVERYTHING HERE RUNS AS wpmgr_app THROUGH db.Pool, never a raw connection: a
// test that opens its own connection leaves every policy inert and still
// passes, which is exactly how m112's proofs stayed green over a
// cross-site-readable domain.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
	"github.com/mosamlife/wpmgr/apps/api/internal/sitetag"
)

// ---------------------------------------------------------------------------
// LAYER 3, ON ITS OWN: THE RESTRICTIVE INSERT POLICY.
//
// The mint has three gates against a site-scoped collaborator minting an
// organisation-wide credential: authz.RequirePermission(PermAPIKeyManage) on
// the route, requireOrgScopedPrincipal in the service, and
// mcp_grants_site_scope_insert in the database. Driven through the mounted
// route, the FIRST one answers and the other two are never consulted -- so the
// route-level proof stays green with requireOrgScopedPrincipal deleted, and
// green with the POLICY DROPPED. Nothing would report the loss.
//
// This reaches Repo.CreateGrantWithToken directly, which skips both app-layer
// gates and leaves the policy as the only thing standing. It is the only shape
// that can tell whether layer 3 is real or merely shadowed.
// ---------------------------------------------------------------------------

func TestMCPMintRLSInsertPolicyRefusesASiteScopedPrincipalAloneAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)

	// The role is load-bearing for every assertion in this file: as a
	// superuser or a BYPASSRLS role the policy below is inert and this test
	// would pass while proving nothing.
	if err := pool.InTenantTx(ctx, uuid.New(), func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx")
		return nil
	}); err != nil {
		t.Fatalf("open tenant tx: %v", err)
	}

	suffix := uuid.NewString()[:8]
	tenantID := seedTenant(t, pool, "mcp-mint-rls-"+suffix)
	userID := seedUserRow(t, pool, "mint-rls-"+suffix+"@example.test")
	siteID := seedSite(t, pool, tenantID, "https://mintrls-"+suffix+".example.test")

	repo := mcp.NewRepo(pool)

	// An outside collaborator shared onto ONE site. RunTenantTx dispatches this
	// principal to InScopedTenantTx, which is the only helper that sets
	// app.site_scope -- the GUC the RESTRICTIVE policy keys on.
	collaborator := domain.Principal{
		Type: domain.PrincipalUser, UserID: userID, TenantID: tenantID,
		Role: "admin", Scope: domain.ScopeSite, AllowedSiteIDs: []uuid.UUID{siteID},
	}

	before := countAllGrants(t, pool, tenantID)
	_, _, err := repo.CreateGrantWithToken(ctx, collaborator, mintProbeGrant(tenantID, "rls layer probe"),
		mintProbeToken(tenantID, "rls_probe_", strings.Repeat("a", 64)), nil)
	if err == nil {
		t.Fatal("THE RESTRICTIVE INSERT POLICY DID NOT FIRE: a site-scoped principal " +
			"inserted an mcp_grants row with both app-layer guards bypassed")
	}

	// THE REFUSAL MUST NAME THIS POLICY, and "some error happened" is not good
	// enough. mcp_grants carries a SECOND restrictive site-scope policy whose
	// USING clause Postgres also applies here, so with
	// mcp_grants_site_scope_insert DROPPED the insert is still refused -- by
	// the other one. An err != nil assertion therefore stays GREEN through the
	// deletion of the policy it claims to prove, which is the same shadowing
	// defect this whole file exists to remove one layer up.
	const policy = "mcp_grants_site_scope_insert"
	if !strings.Contains(err.Error(), policy) {
		t.Fatalf("the insert was refused, but NOT by %s -- so this proof would "+
			"survive that policy being dropped: %v", policy, err)
	}
	if !strings.Contains(err.Error(), "42501") {
		t.Fatalf("want an RLS violation (SQLSTATE 42501), got: %v", err)
	}
	t.Logf("layer 3 refused on its own: %v", err)
	if after := countAllGrants(t, pool, tenantID); after != before {
		t.Fatalf("the refused insert still wrote: grants %d -> %d", before, after)
	}

	// AND IT DOES NOT OVER-FIRE. Without this the test above would also pass if
	// the insert were broken for everybody, which would make it a proof of
	// nothing. The same call by an ORG-scoped principal must succeed.
	admin := domain.Principal{
		Type: domain.PrincipalUser, UserID: userID, TenantID: tenantID,
		Role: "admin", Scope: domain.ScopeOrg,
	}
	if _, _, err := repo.CreateGrantWithToken(ctx, admin, mintProbeGrant(tenantID, "org control"),
		mintProbeToken(tenantID, "org_probe_", strings.Repeat("d", 64)), nil); err != nil {
		t.Fatalf("an ORG-scoped principal was also refused, so the refusal above "+
			"proves nothing about site scope: %v", err)
	}
}

// ---------------------------------------------------------------------------
// THE TRANSACTION BOUNDARY. Two inserts and an audit append are one unit, and
// there is deliberately no way to express half of it.
// ---------------------------------------------------------------------------

// TestMCPMintRollsBackTheGrantWhenTheTokenInsertFailsAsAppRole makes the SECOND
// insert fail (duplicate token_hash) and proves the first did not survive. A
// grant with no token is a connection the operator sees in the list and can
// never use.
func TestMCPMintRollsBackTheGrantWhenTheTokenInsertFailsAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)

	suffix := uuid.NewString()[:8]
	tenantID := seedTenant(t, pool, "mcp-mint-2ins-"+suffix)
	userID := seedUserRow(t, pool, "mint-2ins-"+suffix+"@example.test")
	seedSite(t, pool, tenantID, "https://mint2ins-"+suffix+".example.test")

	repo := mcp.NewRepo(pool)
	admin := domain.Principal{
		Type: domain.PrincipalUser, UserID: userID, TenantID: tenantID,
		Role: "admin", Scope: domain.ScopeOrg,
	}

	// The FIRST mint must succeed, or the collision below never happens and the
	// whole probe is vacuous.
	sharedHash := strings.Repeat("b", 64)
	mkTok := mintProbeToken(tenantID, "dup_probe_", sharedHash)
	if _, _, err := repo.CreateGrantWithToken(ctx, admin,
		mintProbeGrant(tenantID, "first"), mkTok, nil); err != nil {
		t.Fatalf("the first mint must succeed for this probe to mean anything: %v", err)
	}

	before := countAllGrants(t, pool, tenantID)
	_, _, err := repo.CreateGrantWithToken(ctx, admin,
		mintProbeGrant(tenantID, "second, duplicate hash"), mkTok, nil)
	if err == nil {
		t.Fatal("a duplicate token_hash was accepted; this probe proves nothing")
	}
	t.Logf("token insert failed as designed: %v", err)
	if after := countAllGrants(t, pool, tenantID); after != before {
		t.Fatalf("A GRANT SURVIVED A FAILED TOKEN INSERT: grants %d -> %d", before, after)
	}
	assertNoOrphanMCPGrant(t, pool, tenantID)
}

// TestMCPMintRollsBackTheCredentialWhenTheAuditAppendFailsAsAppRole is the
// invariant mint.go is written around: "a live credential that no audit row
// explains" must be unrepresentable. onCreated runs INSIDE the transaction, so
// an error from it has to destroy both inserts.
func TestMCPMintRollsBackTheCredentialWhenTheAuditAppendFailsAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)

	suffix := uuid.NewString()[:8]
	tenantID := seedTenant(t, pool, "mcp-mint-aud-"+suffix)
	userID := seedUserRow(t, pool, "mint-aud-"+suffix+"@example.test")
	seedSite(t, pool, tenantID, "https://mintaud-"+suffix+".example.test")

	repo := mcp.NewRepo(pool)
	admin := domain.Principal{
		Type: domain.PrincipalUser, UserID: userID, TenantID: tenantID,
		Role: "admin", Scope: domain.ScopeOrg,
	}

	beforeGrants := countAllGrants(t, pool, tenantID)
	beforeTokens := countAllMCPTokens(t, pool, tenantID)
	_, _, err := repo.CreateGrantWithToken(ctx, admin,
		mintProbeGrant(tenantID, "audit fails"),
		mintProbeToken(tenantID, "aud_probe_", strings.Repeat("c", 64)),
		func(tx pgx.Tx, gr sqlc.McpGrant) error { return errMintAuditProbe })
	if err == nil {
		t.Fatal("an onCreated error did not abort the mint")
	}

	// THE ERROR IS PINNED TO THE PROBE, not merely observed to be non-nil.
	// The mint can fail BEFORE onCreated ever runs -- a 23502 from a NOT NULL
	// grant column, or a CHECK violation on expires_at or capabilities. In
	// that case nothing was ever inserted, both count assertions below still
	// pass, and this test stays green while proving nothing about the
	// transaction boundary it is named for.
	//
	// mintProbeGrant supplies Capabilities and ExpiresAt today, so the probe
	// does reach onCreated right now. The point is that WITHOUT THIS CHECK
	// this test cannot report the day that stops being true. Same approach as
	// the sibling probe,
	// TestMCPAuditEvents_RolledBackGrantCreationLeavesNoAuditRow_AsAppRole.
	if !errors.Is(err, errMintAuditProbe) {
		t.Fatalf("the mint failed with %v, which is NOT the injected onCreated failure. "+
			"The insert aborted before onCreated ran, so the rollback assertions below "+
			"are vacuous and this probe proves nothing about the transaction boundary.", err)
	}

	if after := countAllGrants(t, pool, tenantID); after != beforeGrants {
		t.Fatalf("A CREDENTIAL SURVIVED A FAILED AUDIT APPEND: grants %d -> %d",
			beforeGrants, after)
	}
	if after := countAllMCPTokens(t, pool, tenantID); after != beforeTokens {
		t.Fatalf("A TOKEN SURVIVED A FAILED AUDIT APPEND: tokens %d -> %d",
			beforeTokens, after)
	}
	t.Logf("audit failure rolled the whole mint back: %v", err)
}

// ---------------------------------------------------------------------------
// THE ACTOR ON THE AUDIT ROW.
//
// This is the HEADLESS path, so an API key is its natural caller: the route is
// mounted under Auth.Authenticate(), whose Bearer branch returns
// apikey.PrincipalFor's principal, and that principal NEVER carries a UserID.
// Recording ActorUser over Principal.UserID therefore attributed the one
// credential-issuance event this surface exists to explain to a user that does
// not exist, over a zero UUID, with created_by_user_id NULL -- a trail naming
// neither the key nor a person.
// ---------------------------------------------------------------------------

func TestMCPMintByAnAPIKeyNamesTheKeyInTheAuditRowAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)

	if err := pool.InTenantTx(ctx, uuid.New(), func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (the dispatch the audit read uses)")
		return nil
	}); err != nil {
		t.Fatalf("open tenant tx: %v", err)
	}

	suffix := uuid.NewString()[:8]
	tenantID := seedTenant(t, pool, "mcp-mint-key-"+suffix)
	seedSite(t, pool, tenantID, "https://mintkey-"+suffix+".example.test")

	svc := mcp.NewService(mcp.NewRepo(pool)).
		WithAudit(audit.NewRecorder(pool, domain.SystemClock{}))

	// EXACTLY what apikey.PrincipalFor builds for an org-scoped owner key.
	keyID := uuid.New()
	keyPrincipal := domain.Principal{
		Type: domain.PrincipalAPIKey, APIKeyID: keyID, TenantID: tenantID,
		Role: "owner", Scope: domain.ScopeOrg, AuthModel: domain.AuthModelRole,
	}
	if keyPrincipal.UserID != uuid.Nil {
		t.Fatal("this proof assumes an API-key principal carries no UserID")
	}
	eng := mountConnectionsLikeProduction(t, svc, keyPrincipal)

	var minted mintResponse
	code := mcpDoJSON(t, eng, http.MethodPost, mintPath, map[string]any{
		"name":            "ci runner via api key",
		"site_scope_mode": string(mcp.SiteScopeModeAll),
	}, nil, &minted)
	if code != http.StatusCreated {
		t.Fatalf("an API-key principal could not mint: %d", code)
	}

	// The audit row, read back through the same tx helper Recorder.List uses.
	var found bool
	for _, r := range queryMCPAuditRowsAsAppRole(t, pool, tenantID, "mcp.grant.created") {
		if r.targetID != minted.GrantID {
			continue
		}
		found = true
		if r.actorType != audit.ActorAPIKey {
			t.Fatalf("actor_type = %q, want %q: the mint attributed a machine's act "+
				"to a kind of actor it was not", r.actorType, audit.ActorAPIKey)
		}
		if r.actorID != keyID.String() {
			t.Fatalf("actor_id = %q, want the API key's id %s: the trail names "+
				"neither the key nor a person", r.actorID, keyID)
		}
		if r.actorID == uuid.Nil.String() {
			t.Fatal("actor_id is the zero UUID: the row explains nothing")
		}
		if r.metadata["issuance"] != "connection_token" {
			t.Fatalf("issuance = %v, want connection_token", r.metadata["issuance"])
		}
	}
	if !found {
		t.Fatalf("NO mcp.grant.created ROW FOR GRANT %s: a live credential that no "+
			"audit row explains is the state mint.go exists to make unrepresentable",
			minted.GrantID)
	}

	// created_by_user_id stays NULL, and that is the HONEST answer rather than
	// a gap: an API-key mint genuinely has no human author, and a zero UUID
	// here would look like a real one.
	var createdBy *uuid.UUID
	grantID, err := uuid.Parse(minted.GrantID)
	if err != nil {
		t.Fatalf("parse grant id: %v", err)
	}
	if err := pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT created_by_user_id FROM mcp_grants WHERE tenant_id=$1 AND id=$2`,
			tenantID, grantID).Scan(&createdBy)
	}); err != nil {
		t.Fatalf("read created_by_user_id: %v", err)
	}
	if createdBy != nil {
		t.Fatalf("created_by_user_id = %v for an API-key mint, want NULL", *createdBy)
	}

	// AND IT DOES NOT OVER-FIRE: a session user on the same route is still
	// recorded as a user, with their own id. A resolver that answered
	// "api_key" for everybody would pass the assertions above.
	userID := seedUserRow(t, pool, "mint-key-human-"+suffix+"@example.test")
	humanEng := mountConnectionsLikeProduction(t, svc, domain.Principal{
		Type: domain.PrincipalUser, UserID: userID, TenantID: tenantID,
		Role: "admin", Scope: domain.ScopeOrg,
	})
	var humanMint mintResponse
	if code := mcpDoJSON(t, humanEng, http.MethodPost, mintPath, map[string]any{
		"name":            "human minted",
		"site_scope_mode": string(mcp.SiteScopeModeAll),
	}, nil, &humanMint); code != http.StatusCreated {
		t.Fatalf("a session user could not mint: %d", code)
	}
	var sawHuman bool
	for _, r := range queryMCPAuditRowsAsAppRole(t, pool, tenantID, "mcp.grant.created") {
		if r.targetID != humanMint.GrantID {
			continue
		}
		sawHuman = true
		if r.actorType != audit.ActorUser || r.actorID != userID.String() {
			t.Fatalf("a session user's mint recorded actor_type=%q actor_id=%q, "+
				"want %q and %s", r.actorType, r.actorID, audit.ActorUser, userID)
		}
	}
	if !sawHuman {
		t.Fatalf("no mcp.grant.created row for the human's grant %s", humanMint.GrantID)
	}
}

// ---------------------------------------------------------------------------
// CROSS-TENANT SCOPE REFERENTS.
//
// The committed suite proves a GHOST id (naming nothing anywhere) is refused.
// These prove the harder case: a REAL row that belongs to another organisation.
// scope_tag_ids and scope_site_ids are uuid[] columns with no foreign key, so
// the database itself would store either happily; the refusal has to come from
// resolving the id under this tenant's RLS, and only a real foreign row can
// tell a working tenant scope from a lucky one.
// ---------------------------------------------------------------------------

func TestMCPMintRefusesAnotherTenantsTagIDAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)

	if err := pool.InTenantTx(ctx, uuid.New(), func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (the dispatch ListTagIDs uses)")
		return nil
	}); err != nil {
		t.Fatalf("open tenant tx: %v", err)
	}

	suffix := uuid.NewString()[:8]
	tenantA := seedTenant(t, pool, "mcp-mint-xt-a-"+suffix)
	tenantB := seedTenant(t, pool, "mcp-mint-xt-b-"+suffix)
	userA := seedUserRow(t, pool, "mint-xt-a-"+suffix+"@example.test")
	seedSite(t, pool, tenantA, "https://mintxta-"+suffix+".example.test")
	seedSite(t, pool, tenantB, "https://mintxtb-"+suffix+".example.test")

	_, tagSvc := newSiteTagServices(pool)
	tagB, err := tagSvc.Create(ctx, sitetag.CreateInput{
		TenantID: tenantB, Name: "foreign-" + suffix, Color: "#445566",
	})
	if err != nil {
		t.Fatalf("create tenant B tag: %v", err)
	}

	// The registry read the refusal rests on is tenant-scoped, BOTH WAYS: B's
	// tag is absent from A, and present under B. Without the second half the
	// first is vacuous -- an always-empty ListTagIDs would satisfy it.
	repo := mcp.NewRepo(pool)
	idsA, err := repo.ListTagIDs(ctx, tenantA)
	if err != nil {
		t.Fatalf("ListTagIDs(A): %v", err)
	}
	for _, id := range idsA {
		if id == tagB.ID {
			t.Fatalf("TENANCY LEAK: ListTagIDs(tenantA) returned tenant B's tag %s", tagB.ID)
		}
	}
	idsB, err := repo.ListTagIDs(ctx, tenantB)
	if err != nil {
		t.Fatalf("ListTagIDs(B): %v", err)
	}
	var seen bool
	for _, id := range idsB {
		seen = seen || id == tagB.ID
	}
	if !seen {
		t.Fatalf("ListTagIDs(tenantB) did not return tenant B's own tag %s; the "+
			"negative above would then be vacuous", tagB.ID)
	}

	svc := mcp.NewService(repo)
	eng := mountConnectionsLikeProduction(t, svc, domain.Principal{
		Type: domain.PrincipalUser, UserID: userA, TenantID: tenantA,
		Role: "admin", Scope: domain.ScopeOrg,
	})

	before := countAllGrants(t, pool, tenantA)
	var body map[string]any
	code := mcpDoJSON(t, eng, http.MethodPost, mintPath, map[string]any{
		"name":            "foreign tag scope",
		"site_scope_mode": string(mcp.SiteScopeModeTags),
		"scope_tag_ids":   []string{tagB.ID.String()},
	}, nil, &body)
	raw, mErr := json.Marshal(body)
	if mErr != nil {
		t.Fatalf("marshal refusal body: %v", mErr)
	}
	if code == http.StatusCreated {
		t.Fatalf("ANOTHER TENANT'S TAG ID WAS ACCEPTED (201): %s", raw)
	}
	if code != http.StatusUnprocessableEntity && code != http.StatusBadRequest {
		t.Fatalf("foreign tag answered %d, want a 4xx validation refusal; body=%s", code, raw)
	}
	if !strings.Contains(string(raw), tagB.ID.String()) {
		t.Fatalf("the refusal does not name the offending id, so it cannot be acted on: %s", raw)
	}
	if after := countAllGrants(t, pool, tenantA); after != before {
		t.Fatalf("the refused mint still wrote: grants %d -> %d", before, after)
	}
	t.Logf("foreign tag refused with %d: %s", code, raw)
}

func TestMCPMintRefusesAnotherTenantsSiteIDAsAppRole(t *testing.T) {
	pool := startPostgres(t)

	suffix := uuid.NewString()[:8]
	tenantA := seedTenant(t, pool, "mcp-mint-xs-a-"+suffix)
	tenantB := seedTenant(t, pool, "mcp-mint-xs-b-"+suffix)
	userA := seedUserRow(t, pool, "mint-xs-a-"+suffix+"@example.test")
	siteA := seedSite(t, pool, tenantA, "https://mintxsa-"+suffix+".example.test")
	siteB := seedSite(t, pool, tenantB, "https://mintxsb-"+suffix+".example.test")

	svc := mcp.NewService(mcp.NewRepo(pool))
	eng := mountConnectionsLikeProduction(t, svc, domain.Principal{
		Type: domain.PrincipalUser, UserID: userA, TenantID: tenantA,
		Role: "admin", Scope: domain.ScopeOrg,
	})

	before := countAllGrants(t, pool, tenantA)
	var body map[string]any
	code := mcpDoJSON(t, eng, http.MethodPost, mintPath, map[string]any{
		"name":            "foreign site scope",
		"site_scope_mode": string(mcp.SiteScopeModeList),
		"scope_site_ids":  []string{siteB.String()},
	}, nil, &body)
	raw, mErr := json.Marshal(body)
	if mErr != nil {
		t.Fatalf("marshal refusal body: %v", mErr)
	}
	if code == http.StatusCreated {
		t.Fatalf("ANOTHER TENANT'S SITE ID WAS ACCEPTED (201): %s", raw)
	}
	if code != http.StatusUnprocessableEntity && code != http.StatusBadRequest {
		t.Fatalf("foreign site answered %d, want a 4xx validation refusal; body=%s", code, raw)
	}
	if !strings.Contains(string(raw), siteB.String()) {
		t.Fatalf("the refusal does not name the offending site id: %s", raw)
	}
	if after := countAllGrants(t, pool, tenantA); after != before {
		t.Fatalf("the refused mint still wrote: grants %d -> %d", before, after)
	}

	// AND IT DOES NOT OVER-FIRE: tenant A's OWN site is accepted on the same
	// route. A resolver that dropped every id would satisfy the refusal above.
	var minted mintResponse
	if code := mcpDoJSON(t, eng, http.MethodPost, mintPath, map[string]any{
		"name":            "own site scope",
		"site_scope_mode": string(mcp.SiteScopeModeList),
		"scope_site_ids":  []string{siteA.String()},
	}, nil, &minted); code != http.StatusCreated {
		t.Fatalf("tenant A's OWN site was refused (%d), so the refusal above proves "+
			"nothing about tenancy", code)
	}
	t.Logf("foreign site refused with %d: %s", code, raw)
}

// ---------------------------------------------------------------------------
// Helpers. Every read goes THROUGH THE POOL as wpmgr_app, inside the tenant
// transaction the request path uses.
// ---------------------------------------------------------------------------

// mintProbeGrant is the minimal valid CreateMCPGrantParams for a repo-level
// probe: org-wide scope, one capability, and an absolute expiry, so the only
// thing that can refuse it is the layer under test.
func mintProbeGrant(tenantID uuid.UUID, name string) sqlc.CreateMCPGrantParams {
	return sqlc.CreateMCPGrantParams{
		TenantID:      tenantID,
		Name:          name,
		Status:        "active",
		SiteScopeMode: "all",
		ScopeTagIds:   []uuid.UUID{},
		ScopeSiteIds:  []uuid.UUID{},
		ClientID:      nil,
		Capabilities:  []string{"mcp.sites.read"},
		ExpiresAt:     time.Now().UTC().Add(24 * time.Hour),
	}
}

// mintProbeToken builds the companion token params. The hash is a caller-chosen
// constant so a test can force the UNIQUE collision deliberately.
func mintProbeToken(tenantID uuid.UUID, prefix, hash string) func(uuid.UUID) sqlc.CreateMCPConnectionTokenParams {
	return func(grantID uuid.UUID) sqlc.CreateMCPConnectionTokenParams {
		return sqlc.CreateMCPConnectionTokenParams{
			TenantID:    tenantID,
			GrantID:     grantID,
			TokenPrefix: prefix,
			TokenHash:   hash,
			Status:      "active",
			ExpiresAt:   pgtype.Timestamptz{Time: time.Now().UTC().Add(time.Hour), Valid: true},
		}
	}
}

// errMintAuditProbe is the deliberate onCreated failure. A named value rather
// than errors.New at the call site so a rollback assertion cannot accidentally
// match some real error.
var errMintAuditProbe = &mintProbeError{}

type mintProbeError struct{}

func (*mintProbeError) Error() string { return "mint probe: audit append failed" }

// countAllMCPTokens counts a tenant's connection tokens through InTenantTx --
// the same tx shape the request path uses. A raw connection would read past RLS
// and could report rows the application can never see.
func countAllMCPTokens(t *testing.T, pool *db.Pool, tenantID uuid.UUID) int64 {
	t.Helper()
	var n int64
	if err := pool.InTenantTx(context.Background(), tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT count(*) FROM mcp_connection_tokens WHERE tenant_id = $1`,
			tenantID).Scan(&n)
	}); err != nil {
		t.Fatalf("count tokens: %v", err)
	}
	return n
}

// assertNoOrphanMCPGrant fails if any grant exists with neither a token nor an
// authorization code. That is the half-built connection an operator sees in the
// list and can never use, and it is what a two-commit mint leaves behind.
func assertNoOrphanMCPGrant(t *testing.T, pool *db.Pool, tenantID uuid.UUID) {
	t.Helper()
	var n int64
	if err := pool.InTenantTx(context.Background(), tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT count(*) FROM mcp_grants g
			  WHERE g.tenant_id = $1
			    AND NOT EXISTS (SELECT 1 FROM mcp_connection_tokens t
			                     WHERE t.grant_id = g.id)
			    AND NOT EXISTS (SELECT 1 FROM mcp_authorization_codes c
			                     WHERE c.grant_id = g.id)`, tenantID).Scan(&n)
	}); err != nil {
		t.Fatalf("count orphan grants: %v", err)
	}
	if n != 0 {
		t.Fatalf("%d grant(s) exist with neither a token nor a code", n)
	}
}
