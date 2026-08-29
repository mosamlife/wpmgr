// adr064_s6b2_mcp_transport_rls_test.go: the S6b-2 read tool and the connect
// record, executed against the REAL schema as wpmgr_app.
//
// WHY THIS FILE EXISTS. The S6b-2 unit proofs drive a fake store whose
// ListSitesForRead and RecordClientIdentity IGNORE the tenant argument
// entirely. Everything they establish about JSON shape, staleness, truncation
// and negotiation is real, and none of it touches the tenancy boundary: with
// that fake, "org A cannot read org B's sites" and "the identity write lands
// under FORCE ROW LEVEL SECURITY" rest on reading the policy files and
// believing them. This project has been bitten by exactly that -- m112's proofs
// opened their own connections, so every policy was inert and every test was
// green.
//
// TWO THINGS ARE ACTUALLY IN QUESTION HERE, and neither is decidable by
// reading:
//
//  1. ListSitesForRead runs InTenantTx and passes tenant_id explicitly. If the
//     sites policy or the helper were wrong, org A's read would return org B's
//     rows and list_sites would leak the fleet across the tenancy boundary --
//     the worst failure this surface can have.
//
//  2. RecordClientIdentity is an UPDATE issued under InTenantTx. m124's
//     obligation 1 is that a write issued in the TOKEN-LOOKUP transaction
//     instead would match ZERO ROWS AND RAISE NO ERROR under FORCE RLS. The
//     query is :one with RETURNING *, so zero rows surfaces as pgx.ErrNoRows
//     and initialize refuses -- but "the write actually lands when it is issued
//     correctly" is the half nobody had executed.
//
// HOW IT IS TESTED. Through mcp.Repo -- the same methods the handler calls, the
// same generated queries, the same tx helpers. GUCs are never hand-set and no
// connection is opened by this file.
//
// The role is load-bearing: wpmgr_app is NOSUPERUSER NOBYPASSRLS, and either
// privilege would make all of this pass vacuously. It is asserted, and printed.
package tests

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

// mcpSeedGrant mints a client and a grant for one tenant, returning the grant.
// It goes through mcp.Repo so the grant is created exactly as the consent
// endpoint creates it.
func mcpSeedGrant(t *testing.T, repo *mcp.Repo, tenantID uuid.UUID, mode string, siteIDs []uuid.UUID) sqlc.McpGrant {
	t.Helper()
	ctx := context.Background()

	clientID := "test-client-" + uuid.NewString()
	secretHash := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	affected, err := repo.RegisterClient(ctx, sqlc.RegisterMCPOAuthClientParams{
		ClientID:                clientID,
		ClientSecretHash:        &secretHash,
		TokenEndpointAuthMethod: "client_secret_basic",
		RedirectUris:            []string{"https://claude.ai/api/mcp/auth_callback"},
	})
	if err != nil || affected != 1 {
		t.Fatalf("seed client: affected=%d err=%v", affected, err)
	}

	// Both arrays are NOT NULL with a '{}' default, so nil is a 23502 rather
	// than an empty array. Normalising here keeps that the schema's business.
	if siteIDs == nil {
		siteIDs = []uuid.UUID{}
	}
	grant, _, err := repo.CreateGrantWithCode(ctx, sqlc.CreateMCPGrantParams{
		TenantID:      tenantID,
		Name:          "test grant",
		Status:        "active",
		SiteScopeMode: mode,
		ScopeTagIds:   []uuid.UUID{},
		ScopeSiteIds:  siteIDs,
		ClientID:      &clientID,
	}, func(grantID uuid.UUID) sqlc.CreateMCPAuthorizationCodeParams {
		return sqlc.CreateMCPAuthorizationCodeParams{
			TenantID:            tenantID,
			GrantID:             grantID,
			ClientID:            clientID,
			CodeHash:            "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
			CodeChallenge:       "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM",
			CodeChallengeMethod: "S256",
			RedirectUri:         "https://claude.ai/api/mcp/auth_callback",
			ExpiresAt:           time.Now().UTC().Add(5 * time.Minute),
		}
	})
	if err != nil {
		t.Fatalf("seed grant for tenant %s: %v", tenantID, err)
	}
	return grant
}

// TestMCPListSitesForReadIsTenantIsolatedAsAppRole is question 1: org A's read
// must return org A's sites and NOT org B's, executed as wpmgr_app.
func TestMCPListSitesForReadIsTenantIsolatedAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	mcpRepo := mcp.NewRepo(pool)
	siteRepo := site.NewRepo(pool)

	tenantA := seedTenant(t, pool, "mcp-s6b2-org-a-"+uuid.NewString()[:8])
	tenantB := seedTenant(t, pool, "mcp-s6b2-org-b-"+uuid.NewString()[:8])

	// Confirm the role inside the very transaction the read uses, not on some
	// other connection.
	if err := pool.InTenantTx(ctx, tenantA, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (ListSitesForRead path)")
		return nil
	}); err != nil {
		t.Fatalf("open tenant tx: %v", err)
	}

	siteA1, err := siteRepo.Create(ctx, site.CreateInput{
		TenantID: tenantA, URL: "https://a1.example.com", Name: "a1-alpha"})
	if err != nil {
		t.Fatalf("create site A1: %v", err)
	}
	siteA2, err := siteRepo.Create(ctx, site.CreateInput{
		TenantID: tenantA, URL: "https://a2.example.com", Name: "a2-beta"})
	if err != nil {
		t.Fatalf("create site A2: %v", err)
	}
	siteB1, err := siteRepo.Create(ctx, site.CreateInput{
		TenantID: tenantB, URL: "https://b1.example.com", Name: "b1-gamma"})
	if err != nil {
		t.Fatalf("create site B1: %v", err)
	}

	// THE READ, through the method list_sites calls.
	rows, more, err := mcpRepo.ListSitesForRead(ctx, tenantA, 500)
	if err != nil {
		t.Fatalf("ListSitesForRead as wpmgr_app: %v", err)
	}
	if more {
		t.Errorf("more=true with 2 sites and a bound of 500")
	}
	t.Logf("org A read %d rows as wpmgr_app", len(rows))

	seen := map[uuid.UUID]bool{}
	for _, r := range rows {
		seen[r.ID] = true
		// Belt and braces: no row may carry another tenant's id.
		if r.TenantID != tenantA {
			t.Errorf("org A's read returned a row owned by tenant %s", r.TenantID)
		}
	}
	if !seen[siteA1.ID] || !seen[siteA2.ID] {
		t.Errorf("org A cannot see its own sites: got %v", seen)
	}
	// THE ASSERTION THAT MATTERS.
	if seen[siteB1.ID] {
		t.Fatalf("CROSS-TENANT LEAK: org A's list_sites returned org B's site %s", siteB1.ID)
	}
	if len(rows) != 2 {
		t.Errorf("org A read %d rows, want exactly its own 2", len(rows))
	}

	// And the mirror, so this is not an artefact of ordering or of A simply
	// having been created first.
	rowsB, _, err := mcpRepo.ListSitesForRead(ctx, tenantB, 500)
	if err != nil {
		t.Fatalf("ListSitesForRead for org B: %v", err)
	}
	if len(rowsB) != 1 || rowsB[0].ID != siteB1.ID {
		t.Fatalf("org B read %d rows, want exactly its own 1", len(rowsB))
	}
	t.Logf("mirror ok: org B read %d row and it is its own", len(rowsB))
}

// TestMCPScopeResolutionDropsForeignSiteIDsAsAppRole is m124 obligation 2
// executed rather than read: scope_site_ids is a uuid[] with no foreign key, so
// the column ACCEPTS another org's site id. Resolving inside InTenantTx is the
// only thing that catches it.
func TestMCPScopeResolutionDropsForeignSiteIDsAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	mcpRepo := mcp.NewRepo(pool)
	siteRepo := site.NewRepo(pool)

	tenantA := seedTenant(t, pool, "mcp-s6b2-scope-a-"+uuid.NewString()[:8])
	tenantB := seedTenant(t, pool, "mcp-s6b2-scope-b-"+uuid.NewString()[:8])

	siteA1, err := siteRepo.Create(ctx, site.CreateInput{
		TenantID: tenantA, URL: "https://sa1.example.com", Name: "sa1"})
	if err != nil {
		t.Fatalf("create site A1: %v", err)
	}
	siteB1, err := siteRepo.Create(ctx, site.CreateInput{
		TenantID: tenantB, URL: "https://sb1.example.com", Name: "sb1"})
	if err != nil {
		t.Fatalf("create site B1: %v", err)
	}

	// A grant for org A whose scope list names org A's site AND org B's, plus
	// a uuid that never existed. The database cannot refuse any of them.
	neverExisted := uuid.New()
	resolved, err := mcpRepo.ResolveScopeSites(ctx, tenantA, "list", nil,
		[]uuid.UUID{siteA1.ID, siteB1.ID, neverExisted})
	if err != nil {
		t.Fatalf("ResolveScopeSites as wpmgr_app: %v", err)
	}
	t.Logf("scope of 3 ids (1 own, 1 foreign, 1 nonexistent) resolved to %d", len(resolved))

	got := map[uuid.UUID]bool{}
	for _, id := range resolved {
		got[id] = true
	}
	if !got[siteA1.ID] {
		t.Error("the tenant's own site was dropped from its scope")
	}
	if got[siteB1.ID] {
		t.Fatalf("CROSS-TENANT LEAK: org B's site %s survived resolution under org A", siteB1.ID)
	}
	if got[neverExisted] {
		t.Errorf("a uuid that names no site survived resolution")
	}
	if len(resolved) != 1 {
		t.Errorf("resolved %d ids, want exactly 1", len(resolved))
	}

	// MODE 'all' MUST STILL RESOLVE TO THIS TENANT'S SITES AND ONLY THOSE.
	// Asserted because 'all' is the mode that kept working while 'list' and
	// 'tags' were silently resolving to nothing, so it is the one that would
	// have masked the swap in any smoke test.
	all, err := mcpRepo.ResolveScopeSites(ctx, tenantA, "all", nil, nil)
	if err != nil {
		t.Fatalf("ResolveScopeSites mode all: %v", err)
	}
	if len(all) != 1 || all[0] != siteA1.ID {
		t.Errorf("mode 'all' resolved %v, want just org A's own site", all)
	}

	// AN UNRECOGNISED MODE RESOLVES TO NO SITES, never to every site. This is
	// the query's `ELSE false` and it is the fail-closed direction.
	none, err := mcpRepo.ResolveScopeSites(ctx, tenantA, "not-a-mode", nil,
		[]uuid.UUID{siteA1.ID})
	if err != nil {
		t.Fatalf("ResolveScopeSites unknown mode: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("an unrecognised scope mode resolved to %d sites, want 0", len(none))
	}
}

// TestMCPScopeResolutionListModeSelectsTheNamedSites is the regression proof
// for the swapped array arguments.
//
// Both scoped modes resolved to ZERO SITES because repo.ResolveScopeSites
// passed the tag array as $3 (which the query uses for site ids) and the site
// array as $4 (which it uses for tag ids). It failed closed, so nothing leaked
// -- but every grant that was not mode 'all' read nothing, and the transport
// reports that as "your scope resolves to no sites" for a valid grant.
//
// No unit test could see it: the fake store ignores both arrays entirely.
func TestMCPScopeResolutionListModeSelectsTheNamedSites(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	mcpRepo := mcp.NewRepo(pool)
	siteRepo := site.NewRepo(pool)

	tenantA := seedTenant(t, pool, "mcp-s6b2-list-"+uuid.NewString()[:8])

	one, err := siteRepo.Create(ctx, site.CreateInput{
		TenantID: tenantA, URL: "https://l1.example.com", Name: "l1"})
	if err != nil {
		t.Fatalf("create site 1: %v", err)
	}
	two, err := siteRepo.Create(ctx, site.CreateInput{
		TenantID: tenantA, URL: "https://l2.example.com", Name: "l2"})
	if err != nil {
		t.Fatalf("create site 2: %v", err)
	}
	three, err := siteRepo.Create(ctx, site.CreateInput{
		TenantID: tenantA, URL: "https://l3.example.com", Name: "l3"})
	if err != nil {
		t.Fatalf("create site 3: %v", err)
	}

	// A grant naming TWO of the three. The third must not appear.
	resolved, err := mcpRepo.ResolveScopeSites(ctx, tenantA, "list", nil,
		[]uuid.UUID{one.ID, two.ID})
	if err != nil {
		t.Fatalf("ResolveScopeSites mode list: %v", err)
	}
	t.Logf("mode 'list' naming 2 of 3 sites resolved to %d", len(resolved))

	if len(resolved) == 0 {
		t.Fatal("mode 'list' resolved to NO sites for a grant that names two of " +
			"its own: the site and tag arrays are being passed in the wrong positions")
	}
	got := map[uuid.UUID]bool{}
	for _, id := range resolved {
		got[id] = true
	}
	if !got[one.ID] || !got[two.ID] {
		t.Errorf("mode 'list' dropped a named site: %v", resolved)
	}
	if got[three.ID] {
		t.Errorf("mode 'list' returned site %s, which the grant does not name", three.ID)
	}
	if len(resolved) != 2 {
		t.Errorf("resolved %d, want exactly the 2 named", len(resolved))
	}
}

// TestMCPRecordClientIdentityLandsAsAppRole is question 2: the connect record
// must actually WRITE when issued from InTenantTx, and the absent-header case
// must persist as SQL NULL rather than as a string.
func TestMCPRecordClientIdentityLandsAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	repo := mcp.NewRepo(pool)

	tenantA := seedTenant(t, pool, "mcp-s6b2-id-a-"+uuid.NewString()[:8])
	grant := mcpSeedGrant(t, repo, tenantA, "all", nil)

	// Before: never connected. Both columns must be NULL, which is the state
	// the "has never connected" reading depends on.
	before := mcpReadGrant(t, pool, tenantA, grant.ID)
	if before.ClientIdentityRecordedAt.Valid {
		t.Fatalf("a fresh grant already has client_identity_recorded_at set")
	}

	// THE WRITE, with NO protocol header -- the case the design says to expect
	// most of.
	if err := repo.RecordClientIdentity(ctx, tenantA, grant.ID,
		"Claude Desktop", "1.2.3", nil); err != nil {
		t.Fatalf("RecordClientIdentity as wpmgr_app: %v\n"+
			"if this is ErrNoRows the UPDATE matched zero rows under FORCE RLS", err)
	}

	after := mcpReadGrant(t, pool, tenantA, grant.ID)

	// THE ASSERTION THAT MATTERS: the write LANDED. A zero-row UPDATE would
	// have surfaced as ErrNoRows above, but a row that came back unchanged
	// would not, so the stored values are checked rather than the call's error.
	if !after.ClientIdentityRecordedAt.Valid {
		t.Fatalf("the identity write did not land: client_identity_recorded_at is still NULL")
	}
	if after.ClientName == nil || *after.ClientName != "Claude Desktop" {
		t.Errorf("client_name = %v, want Claude Desktop", after.ClientName)
	}
	if after.ClientVersion == nil || *after.ClientVersion != "1.2.3" {
		t.Errorf("client_version = %v, want 1.2.3", after.ClientVersion)
	}
	// AND THE ABSENCE SURVIVED AS AN ABSENCE. recorded_at set with
	// protocol_version NULL is "connected and sent no header"; if the Go layer
	// had defaulted the header to the floor, this column would hold a string
	// the client never sent and the signal would be gone.
	if after.ProtocolVersion != nil {
		t.Errorf("an ABSENT protocol header was persisted as %q; it must be SQL NULL",
			*after.ProtocolVersion)
	}
	t.Logf("connect record landed: recorded_at set, protocol_version NULL (header absent)")

	// The present-header case must be distinguishable from the above.
	hdr := mcp.ProtocolTarget
	if err := repo.RecordClientIdentity(ctx, tenantA, grant.ID,
		"Claude Desktop", "1.2.3", &hdr); err != nil {
		t.Fatalf("RecordClientIdentity with a header: %v", err)
	}
	withHdr := mcpReadGrant(t, pool, tenantA, grant.ID)
	if withHdr.ProtocolVersion == nil || *withHdr.ProtocolVersion != mcp.ProtocolTarget {
		t.Fatalf("a PRESENT header stored as %v, want %q", withHdr.ProtocolVersion, mcp.ProtocolTarget)
	}
}

// TestMCPRecordClientIdentityCannotCrossTenants: the same write aimed at
// another org's grant must land nothing and say so, rather than succeeding
// quietly against zero rows.
func TestMCPRecordClientIdentityCannotCrossTenants(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	repo := mcp.NewRepo(pool)

	tenantA := seedTenant(t, pool, "mcp-s6b2-x-a-"+uuid.NewString()[:8])
	tenantB := seedTenant(t, pool, "mcp-s6b2-x-b-"+uuid.NewString()[:8])
	grantB := mcpSeedGrant(t, repo, tenantB, "all", nil)

	// Org A tries to stamp org B's grant. Under tenant isolation the UPDATE
	// matches zero rows, and because the query is :one that surfaces as
	// ErrNoRows rather than as a silent success.
	err := repo.RecordClientIdentity(ctx, tenantA, grantB.ID, "Evil", "9.9.9", nil)
	if err == nil {
		t.Fatal("org A stamped org B's grant and the call reported success")
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Logf("cross-tenant write refused with: %v", err)
	}

	// And org B's grant is untouched.
	got := mcpReadGrant(t, pool, tenantB, grantB.ID)
	if got.ClientIdentityRecordedAt.Valid {
		t.Fatalf("org B's grant was stamped by org A")
	}
	if got.ClientName != nil {
		t.Errorf("org B's grant carries client_name %q written by org A", *got.ClientName)
	}
	t.Logf("cross-tenant identity write landed nothing, as required")
}

// mcpReadGrant reads one grant through the generated query inside InTenantTx.
func mcpReadGrant(t *testing.T, pool *db.Pool, tenantID, grantID uuid.UUID) sqlc.McpGrant {
	t.Helper()
	var out sqlc.McpGrant
	err := pool.InTenantTx(context.Background(), tenantID, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).GetMCPGrant(context.Background(), sqlc.GetMCPGrantParams{
			TenantID: tenantID, ID: grantID,
		})
		if err != nil {
			return err
		}
		out = row
		return nil
	})
	if err != nil {
		t.Fatalf("read grant %s: %v", grantID, err)
	}
	return out
}
