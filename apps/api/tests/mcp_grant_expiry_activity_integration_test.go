// mcp_grant_expiry_activity_integration_test.go: m127's three grant columns
// and GH #605's activity stamp, executed against the REAL schema as wpmgr_app.
//
// WHY THIS FILE EXISTS. m127 wrote its own hazard down rather than leaving it
// to be discovered, and the hazard is not decidable by reading Go:
//
//	"if a non-NULL idle_expire_after_days is ever written while the stamp is
//	 still unwired, coalesce(last_used_at, created_at) collapses to created_at
//	 permanently, and EVERY AFFECTED CONNECTION DIES idle_expire_after_days
//	 AFTER IT WAS CREATED NO MATTER HOW ACTIVE IT IS."
//
// The Go change that closes it is one call to Repo.TouchActivity. Whether that
// call actually MOVES THE COLUMN THE PREDICATE READS is a property of the
// query, the tx helper, the RLS policies and the role together -- exactly the
// combination m112's proofs got wrong by opening their own connections, so
// every policy was inert and every test was green.
//
// FOUR THINGS ARE IN QUESTION HERE, and each is executed rather than argued:
//
//  1. Service.Approve supplies all three of m127's columns. capabilities and
//     expires_at are NOT NULL with no default, so an omission is 23502; and
//     idle_expire_after_days must come back NULL, because NULL is the answer
//     ("never idle-expire"), not a placeholder.
//
//  2. The stamp lands. last_used_at is NULL on a fresh grant and non-NULL after
//     one RecordActivity, read back through the same verdict query the request
//     path reads.
//
//  3. THE FLEET-OUTAGE CASE, BOTH DIRECTIONS. A grant older than its idle
//     window is refused when it has never been used, and the SAME grant is
//     authorized once the stamp has run. Without direction two this is just a
//     test that expiry works; without direction one it is a test that proves
//     nothing about expiry at all.
//
//  4. Absolute expiry still bites, and ACTIVITY CANNOT RESCUE IT. The two
//     expiries are independent facts (m127 DECISION 2) and a stamp that
//     extended the absolute one would silently make every connection immortal.
//
// HOW IT IS TESTED. Through mcp.Repo and mcp.Service -- the same methods the
// transport calls, the same generated queries, the same tx helpers. GUCs are
// never hand-set and no connection is opened by this file. The role is
// load-bearing and is asserted inside the transaction under test: wpmgr_app is
// NOSUPERUSER NOBYPASSRLS, and either privilege would make all of this pass
// vacuously.
package tests

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

// mcpAgeGrant rewrites created_at and idle_expire_after_days on one grant, so a
// test can construct "this connection is older than its idle window" without
// waiting days for it.
//
// IT GOES THROUGH pool.InTenantTx, NOT ITS OWN CONNECTION. A raw connection
// here would set no GUC, so the UPDATE would run with the policies inert -- and
// then the test would be constructing its fixture through a path the request
// path does not have, which is the failure mode this whole file exists to
// avoid. Under InTenantTx as wpmgr_app the write only lands if the tenant
// policy actually admits it.
func mcpAgeGrant(t *testing.T, pool interface {
	InTenantTx(context.Context, uuid.UUID, func(pgx.Tx) error) error
}, tenantID, grantID uuid.UUID, createdAgo time.Duration, idleDays *int32) {
	t.Helper()
	err := pool.InTenantTx(context.Background(), tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(context.Background(),
			`UPDATE mcp_grants
			    SET created_at = now() - $3::interval,
			        idle_expire_after_days = $4
			  WHERE tenant_id = $1 AND id = $2`,
			tenantID, grantID, createdAgo.String(), idleDays)
		if err != nil {
			return err
		}
		// A zero-row UPDATE that reports success is the exact silent failure
		// m124 obligation 1 names. If the fixture did not land, every
		// assertion below would be measuring an untouched grant.
		if tag.RowsAffected() != 1 {
			t.Fatalf("aging fixture updated %d rows, want 1 -- the fixture did not land, "+
				"so nothing below is testing what it claims", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("age grant %s: %v", grantID, err)
	}
}

// mcpReadGrantColumns reads the three m127 columns plus last_used_at straight
// off the row, under InTenantTx as wpmgr_app.
func mcpReadGrantColumns(t *testing.T, pool interface {
	InTenantTx(context.Context, uuid.UUID, func(pgx.Tx) error) error
}, tenantID, grantID uuid.UUID) (caps []string, expiresAt time.Time, idleDays *int32, lastUsed *time.Time) {
	t.Helper()
	err := pool.InTenantTx(context.Background(), tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT capabilities, expires_at, idle_expire_after_days, last_used_at
			   FROM mcp_grants WHERE tenant_id = $1 AND id = $2`,
			tenantID, grantID).Scan(&caps, &expiresAt, &idleDays, &lastUsed)
	})
	if err != nil {
		t.Fatalf("read grant columns for %s: %v", grantID, err)
	}
	return caps, expiresAt, idleDays, lastUsed
}

// TestMCPApproveSuppliesM127ColumnsAsAppRole is question 1: the consent flow's
// own minting path, end to end, against the real NOT NULL columns.
//
// THE 23502 IS THE DESIGNED FAILURE AND THIS TEST IS WHAT MAKES IT VISIBLE. A
// CreateMCPGrantParams literal that omits either NOT NULL field still COMPILES
// -- the field is simply left zero, which is nil/invalid, which is NULL -- so
// the build says nothing and only an executed INSERT can. That is why this
// proof drives Service.Approve rather than asserting on the struct.
func TestMCPApproveSuppliesM127ColumnsAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	repo := mcp.NewRepo(pool)
	svc := mcp.NewService(repo)

	tenant := seedTenant(t, pool, "mcp-m127-approve-"+uuid.NewString()[:8])

	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (Approve read-back path)")
		return nil
	}); err != nil {
		t.Fatalf("open tenant tx: %v", err)
	}

	clientID := "m127-client-" + uuid.NewString()
	secretHash := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if n, err := repo.RegisterClient(ctx, sqlc.RegisterMCPOAuthClientParams{
		ClientID:                clientID,
		ClientSecretHash:        &secretHash,
		TokenEndpointAuthMethod: "client_secret_basic",
		RedirectUris:            []string{"https://claude.ai/api/mcp/auth_callback"},
	}); err != nil || n != 1 {
		t.Fatalf("seed client: affected=%d err=%v", n, err)
	}

	before := time.Now().UTC()
	approval, err := svc.Approve(ctx, mcp.ApprovalRequest{
		Principal: domain.Principal{TenantID: tenant, Scope: domain.ScopeOrg},
		GrantName: "m127 column proof",
		SiteScope: mcp.SiteScopeRequest{Mode: mcp.SiteScopeModeAll},
		Consent: mcp.ConsentContext{
			ClientID:            clientID,
			RedirectURI:         "https://claude.ai/api/mcp/auth_callback",
			Scopes:              []mcp.Scope{mcp.ScopeRead},
			CodeChallenge:       "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM",
			CodeChallengeMethod: "S256",
		},
	})
	if err != nil {
		// A 23502 lands HERE, and it is the failure this whole change exists to
		// remove. Naming it in the message means the next reader does not have
		// to decode the SQLSTATE.
		t.Fatalf("Approve failed -- if this is SQLSTATE 23502 then a NOT NULL m127 "+
			"column is not being supplied at internal/mcp/service.go's "+
			"CreateMCPGrantParams literal: %v", err)
	}
	after := time.Now().UTC()

	caps, expiresAt, idleDays, lastUsed := mcpReadGrantColumns(t, pool, tenant, approval.GrantID)

	// capabilities: the stored set, not an empty array and not a guess.
	if len(caps) != 1 || caps[0] != string(mcp.CapSitesRead) {
		t.Fatalf("capabilities = %v, want exactly [%q]", caps, mcp.CapSitesRead)
	}

	// expires_at: NOT NULL, in the future, and roughly the 90-day term. The
	// window is deliberately loose on the low side and tight on the high side:
	// the assertion that matters is that SOMETHING chose a bounded term, not
	// that a clock agreed to the second.
	if !expiresAt.After(after) {
		t.Fatalf("expires_at = %s is not in the future (now ~%s)", expiresAt, after)
	}
	wantLo := before.Add(89 * 24 * time.Hour)
	wantHi := after.Add(91 * 24 * time.Hour)
	if expiresAt.Before(wantLo) || expiresAt.After(wantHi) {
		t.Fatalf("expires_at = %s, want between %s and %s (the 90-day term)",
			expiresAt, wantLo, wantHi)
	}

	// idle_expire_after_days: NULL, and NULL is the answer.
	//
	// A NON-NULL VALUE HERE WOULD BE THE FLEET OUTAGE, not a stricter default:
	// m127 DECISION 4 is that a window may only be persisted once the stamp is
	// wired AND an operator has chosen one. The stamp is wired now; no control
	// asks for a window yet, so writing one would be inventing a deadline
	// nobody chose.
	if idleDays != nil {
		t.Fatalf("idle_expire_after_days = %d, want NULL -- a window nobody chose "+
			"would expire this connection %d days after creation", *idleDays, *idleDays)
	}

	// last_used_at: NULL on a grant that has never served a request. This is
	// the baseline the stamp test moves.
	if lastUsed != nil {
		t.Fatalf("last_used_at = %s on a brand new grant, want NULL", *lastUsed)
	}

	t.Logf("Approve wrote capabilities=%v expires_at=%s idle_expire_after_days=NULL last_used_at=NULL",
		caps, expiresAt.Format(time.RFC3339))
}

// TestMCPActivityStampLandsAsAppRole is question 2, and it is GH #605 itself:
// mcp_grants.last_used_at was never written, so the connections list reported
// every connection as "Never used" including one actively reading the fleet.
//
// The read-back is through ReCheckAuthorization -- THE SAME QUERY THE REQUEST
// PATH READS THE VERDICT FROM -- rather than a bespoke SELECT, so this also
// establishes that the column the idle predicate consults is the column the
// stamp moved.
func TestMCPActivityStampLandsAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	repo := mcp.NewRepo(pool)
	siteRepo := site.NewRepo(pool)
	svc := mcp.NewService(repo)

	tenant := seedTenant(t, pool, "mcp-605-stamp-"+uuid.NewString()[:8])

	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (TouchActivity path)")
		return nil
	}); err != nil {
		t.Fatalf("open tenant tx: %v", err)
	}

	s1, err := siteRepo.Create(ctx, site.CreateInput{
		TenantID: tenant, URL: "https://stamp.example.com", Name: "stamp-alpha"})
	if err != nil {
		t.Fatalf("create site: %v", err)
	}

	grant, bearer := s7GrantWithBearer(t, repo, tenant, "list", []uuid.UUID{s1.ID})

	// BASELINE: never used. If this were already non-NULL the assertion below
	// would pass without the stamp doing anything.
	if _, _, _, lastUsed := mcpReadGrantColumns(t, pool, tenant, grant.ID); lastUsed != nil {
		t.Fatalf("last_used_at = %s before any request, want NULL", *lastUsed)
	}

	auth, err := svc.Authenticate(ctx, bearer)
	if err != nil {
		t.Fatalf("Authenticate with a live bearer: %v", err)
	}

	// THE STAMP, through the method the transport's tools/list and tools/call
	// arms call.
	if err := svc.RecordActivity(ctx, auth); err != nil {
		t.Fatalf("RecordActivity: %v", err)
	}

	// Read it back through the VERDICT QUERY, under the same tenant.
	chk, err := repo.ReCheckAuthorization(ctx, tenant, auth.TokenID)
	if err != nil {
		t.Fatalf("re-check after stamp: %v", err)
	}
	if !chk.GrantLastUsedAt.Valid {
		t.Fatal("last_used_at is still NULL after RecordActivity -- the stamp did not " +
			"land, and every connection will still read as \"Never used\" (GH #605)")
	}
	if chk.GrantLastUsedAt.Time.Before(grant.CreatedAt.Add(-time.Second)) {
		t.Fatalf("last_used_at = %s is before the grant's own created_at %s",
			chk.GrantLastUsedAt.Time, grant.CreatedAt)
	}
	t.Logf("grant.last_used_at stamped to %s and read back through the verdict query",
		chk.GrantLastUsedAt.Time.Format(time.RFC3339))

	// THE TOKEN HALF. The rotation UI tells two live tokens apart by
	// last_used_at, so a grant stamped without its token would make it claim a
	// token nobody has retired is dead.
	var tokenLastUsed *time.Time
	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT last_used_at FROM mcp_connection_tokens WHERE tenant_id = $1 AND id = $2`,
			tenant, auth.TokenID).Scan(&tokenLastUsed)
	}); err != nil {
		t.Fatalf("read token last_used_at: %v", err)
	}
	if tokenLastUsed == nil {
		t.Fatal("mcp_connection_tokens.last_used_at is still NULL after RecordActivity -- " +
			"TouchMCPConnectionTokenInTenantTx is still unwired")
	}
	t.Logf("token.last_used_at stamped to %s in the same transaction",
		tokenLastUsed.Format(time.RFC3339))
}

// TestMCPActiveConnectionDoesNotIdleExpireAsAppRole is question 3, and it is
// the fleet outage m127 DECISION 4 describes, constructed and executed in both
// directions on ONE grant.
//
// The grant is aged 10 days with a 1-day idle window. Direction one is the
// outage as it would have shipped: last_used_at NULL, so
// coalesce(last_used_at, created_at) is created_at, so the connection is
// refused despite nothing being wrong with it. Direction two is the fix: one
// RecordActivity, and the same grant with the same window is authorized.
//
// BOTH HALVES ARE REQUIRED. Direction one alone is a test that idle expiry
// works. Direction two alone is a test that could pass with idle expiry
// disabled entirely. Only together do they say "the deadline moves with use".
func TestMCPActiveConnectionDoesNotIdleExpireAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	repo := mcp.NewRepo(pool)
	siteRepo := site.NewRepo(pool)
	svc := mcp.NewService(repo)

	tenant := seedTenant(t, pool, "mcp-127-idle-"+uuid.NewString()[:8])

	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (idle expiry path)")
		return nil
	}); err != nil {
		t.Fatalf("open tenant tx: %v", err)
	}

	s1, err := siteRepo.Create(ctx, site.CreateInput{
		TenantID: tenant, URL: "https://idle.example.com", Name: "idle-alpha"})
	if err != nil {
		t.Fatalf("create site: %v", err)
	}

	grant, bearer := s7GrantWithBearer(t, repo, tenant, "list", []uuid.UUID{s1.ID})

	oneDay := int32(1)
	mcpAgeGrant(t, pool, tenant, grant.ID, 10*24*time.Hour, &oneDay)

	// ---- DIRECTION ONE: never used, and therefore idle-expired. ----
	chk, err := repo.ReCheckAuthorization(ctx, tenant, mcpTokenIDFor(t, repo, ctx, bearer))
	if err != nil {
		t.Fatalf("re-check before stamp: %v", err)
	}
	if chk.GrantLastUsedAt.Valid {
		t.Fatalf("fixture is wrong: last_used_at = %s, want NULL for the never-used case",
			chk.GrantLastUsedAt.Time)
	}
	if !chk.GrantIdleExpired {
		t.Fatal("a grant created 10 days ago with a 1-day idle window and no recorded " +
			"use is not reported idle-expired; the idle predicate is not binding")
	}
	if chk.Authorized {
		t.Fatal("an idle-expired grant came back authorized -- idle expiry is not part " +
			"of the verdict")
	}
	if _, err := svc.Authenticate(ctx, bearer); err == nil {
		t.Fatal("Authenticate admitted a grant past its idle window")
	}
	t.Log("direction one: never-used grant older than its idle window is REFUSED")

	// ---- DIRECTION TWO: the same grant, actively used. ----
	//
	// The stamp has to be issued outside Authenticate, because Authenticate now
	// refuses this grant -- which is exactly the state a real connection is
	// never in, since a real connection is stamped on every tool call from its
	// first one. Constructing the stamp directly is what lets ONE grant carry
	// both directions and makes the comparison exact.
	if err := repoTouch(ctx, repo, tenant, grant.ID, chk.TokenID); err != nil {
		t.Fatalf("stamp activity: %v", err)
	}

	chk2, err := repo.ReCheckAuthorization(ctx, tenant, chk.TokenID)
	if err != nil {
		t.Fatalf("re-check after stamp: %v", err)
	}
	if chk2.GrantIdleExpired {
		t.Fatal("THE FLEET OUTAGE: a connection used seconds ago is still reported " +
			"idle-expired. coalesce(last_used_at, created_at) is still collapsing to " +
			"created_at, so every connection dies N days after creation however " +
			"active it is (m127 DECISION 4)")
	}
	if !chk2.Authorized {
		t.Fatalf("an actively used grant is not authorized: grant_status=%q token_status=%q "+
			"absolute_expired=%v idle_expired=%v",
			chk2.GrantStatus, chk2.TokenStatus, chk2.GrantAbsoluteExpired, chk2.GrantIdleExpired)
	}
	auth, err := svc.Authenticate(ctx, bearer)
	if err != nil {
		t.Fatalf("Authenticate refused an actively used connection: %v", err)
	}
	if !auth.Capabilities.Allows(mcp.CapSitesRead) {
		t.Fatal("an actively used connection resolved to no capability")
	}
	t.Logf("direction two: the SAME grant, stamped, is AUTHORIZED with %d capability and %d site",
		auth.Capabilities.Len(), auth.Sites.Len())
}

// TestMCPAbsoluteExpiryRefusesAndActivityCannotRescueItAsAppRole is question 4.
//
// The two expiries are independent facts and neither can rescue the other
// (m127 DECISION 2). The direction that matters is this one: if the activity
// stamp extended the ABSOLUTE expiry as well as the idle one, every connection
// that is used at all would be immortal and the 90-day term would be
// decorative -- a silent extension of every credential, which is the failure
// direction m127 says the schema must never take.
func TestMCPAbsoluteExpiryRefusesAndActivityCannotRescueItAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	repo := mcp.NewRepo(pool)
	siteRepo := site.NewRepo(pool)
	svc := mcp.NewService(repo)

	tenant := seedTenant(t, pool, "mcp-127-abs-"+uuid.NewString()[:8])

	s1, err := siteRepo.Create(ctx, site.CreateInput{
		TenantID: tenant, URL: "https://abs.example.com", Name: "abs-alpha"})
	if err != nil {
		t.Fatalf("create site: %v", err)
	}

	grant, bearer := s7GrantWithBearer(t, repo, tenant, "list", []uuid.UUID{s1.ID})

	// It authenticates first, so the refusal below is attributable to the
	// expiry and not to a broken fixture.
	auth, err := svc.Authenticate(ctx, bearer)
	if err != nil {
		t.Fatalf("Authenticate before expiry: %v", err)
	}
	tokenID := auth.TokenID

	// Push expires_at into the past. created_at moves with it because
	// mcp_grants_expires_at_after_created_check requires expires_at >
	// created_at, and a fixture that violates a CHECK would fail for a reason
	// this test is not about.
	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE mcp_grants
			    SET created_at = now() - interval '100 days',
			        expires_at = now() - interval '1 hour'
			  WHERE tenant_id = $1 AND id = $2`,
			tenant, grant.ID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			t.Fatalf("expiry fixture updated %d rows, want 1", tag.RowsAffected())
		}
		return nil
	}); err != nil {
		t.Fatalf("expire grant: %v", err)
	}

	chk, err := repo.ReCheckAuthorization(ctx, tenant, tokenID)
	if err != nil {
		t.Fatalf("re-check after expiry: %v", err)
	}
	if !chk.GrantAbsoluteExpired {
		t.Fatal("a grant whose expires_at is in the past is not reported absolute-expired")
	}
	if chk.Authorized {
		t.Fatal("an expired grant came back authorized -- absolute expiry is not part of " +
			"the verdict")
	}
	if _, err := svc.Authenticate(ctx, bearer); err == nil {
		t.Fatal("Authenticate admitted a grant past its absolute expiry")
	}
	t.Log("a grant past expires_at is REFUSED")

	// THE RESCUE ATTEMPT. Stamp it, hard, and confirm the verdict does not
	// move.
	if err := repoTouch(ctx, repo, tenant, grant.ID, tokenID); err != nil {
		t.Fatalf("stamp activity on an expired grant: %v", err)
	}
	chk2, err := repo.ReCheckAuthorization(ctx, tenant, tokenID)
	if err != nil {
		t.Fatalf("re-check after stamping an expired grant: %v", err)
	}
	if !chk2.GrantLastUsedAt.Valid {
		t.Fatal("fixture is wrong: the stamp did not land, so the rescue is untested")
	}
	if chk2.Authorized || !chk2.GrantAbsoluteExpired {
		t.Fatal("USE EXTENDED AN ABSOLUTE EXPIRY: a stamped grant past expires_at came " +
			"back authorized. The two expiries are independent and activity must move " +
			"only the idle one")
	}
	t.Log("the same grant, freshly stamped, is STILL refused: activity moves the idle " +
		"deadline and never the absolute one")
}

// TestMCPStoredCapabilitiesAreTheAuthorityAsAppRole is the proof that
// Authenticate READS the capability column rather than recomputing it.
//
// IT IS THE ONLY CONSTRUCTION IN WHICH THE TWO ANSWERS DIFFER TODAY. The
// vocabulary holds exactly one name, so for any grant carrying
// [mcp.sites.read] the stored set and the set computed from grantScopes() are
// identical and no test can tell them apart. An EMPTY stored set is the
// discriminator: the column's shape CHECK admits '{}' (the restrictive value
// passes the vocabulary containment test), the computed answer is still
// [mcp.sites.read], and only a path that reads the row refuses.
//
// Deleting the verdict-row read and restoring OrgDefaultCapabilities(
// grantScopes()) makes this test, and only this test, go red.
func TestMCPStoredCapabilitiesAreTheAuthorityAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	repo := mcp.NewRepo(pool)
	siteRepo := site.NewRepo(pool)
	svc := mcp.NewService(repo)

	tenant := seedTenant(t, pool, "mcp-127-caps-"+uuid.NewString()[:8])

	s1, err := siteRepo.Create(ctx, site.CreateInput{
		TenantID: tenant, URL: "https://caps.example.com", Name: "caps-alpha"})
	if err != nil {
		t.Fatalf("create site: %v", err)
	}

	grant, bearer := s7GrantWithBearer(t, repo, tenant, "list", []uuid.UUID{s1.ID})

	// It authenticates with the seeded capability, so the refusal below is
	// attributable to the emptied column and not to a broken fixture.
	auth, err := svc.Authenticate(ctx, bearer)
	if err != nil {
		t.Fatalf("Authenticate before emptying capabilities: %v", err)
	}
	if !auth.Capabilities.Allows(mcp.CapSitesRead) {
		t.Fatal("the seeded grant did not resolve to its own capability")
	}

	// Empty the column. The grant stays 'active' and unexpired -- ONLY the
	// capability set changes, so nothing else can explain the refusal.
	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE mcp_grants SET capabilities = '{}'::text[]
			  WHERE tenant_id = $1 AND id = $2`, tenant, grant.ID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			t.Fatalf("capability fixture updated %d rows, want 1", tag.RowsAffected())
		}
		return nil
	}); err != nil {
		t.Fatalf("empty capabilities: %v", err)
	}

	// The verdict itself is unchanged: an empty capability set is not an
	// expiry and not a revocation, so `authorized` stays true. That is what
	// makes this a test of the Go read and not of the SQL predicate.
	chk, err := repo.ReCheckAuthorization(ctx, tenant, auth.TokenID)
	if err != nil {
		t.Fatalf("re-check after emptying capabilities: %v", err)
	}
	if !chk.Authorized {
		t.Fatalf("emptying capabilities changed the SQL verdict; this test can no longer "+
			"isolate the Go read (grant_status=%q)", chk.GrantStatus)
	}
	if len(chk.GrantCapabilities) != 0 {
		t.Fatalf("grant_capabilities = %v after emptying, want []", chk.GrantCapabilities)
	}

	if _, err := svc.Authenticate(ctx, bearer); err == nil {
		t.Fatal("Authenticate ADMITTED a grant whose stored capability set is empty. " +
			"The capability set is being computed from the scope registry instead of " +
			"read from the grant, so the column is decorative and a narrowed connection " +
			"would hold the org default regardless of what was stored.")
	}
	t.Log("a grant with capabilities = '{}' is REFUSED: the stored column is the authority")
}

// repoTouch issues the activity stamp through the Store interface the service
// uses, so this file never reimplements the UPDATE it is testing.
func repoTouch(ctx context.Context, repo *mcp.Repo, tenantID, grantID, tokenID uuid.UUID) error {
	_, err := repo.TouchActivity(ctx, tenantID, grantID, tokenID)
	return err
}

// mcpTokenIDFor resolves the token id for a bearer WITHOUT going through
// Authenticate, which refuses an expired grant and therefore cannot be used to
// learn the id of the token whose expiry is under test.
//
// It uses Repo.LookupConnectionToken -- step 1 of the real two-query
// authentication, under InMCPTokenLookupTx -- so even the fixture reaches the
// row the way production does.
func mcpTokenIDFor(t *testing.T, repo *mcp.Repo, ctx context.Context, bearer string) uuid.UUID {
	t.Helper()
	sum := sha256.Sum256([]byte(bearer))
	row, err := repo.LookupConnectionToken(ctx, hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatalf("resolve token id: %v", err)
	}
	return row.ID
}
