// m131 widened mcp_grants_capabilities_vocabulary_check from one member to
// eight; the commit that widened capabilityVocabulary in internal/mcp/policy.go
// to match is the one this file proves.
//
// THE VOCABULARY IS NOW CLOSED IN TWO PLACES AND THAT IS WORSE THAN ONE OPEN
// SET UNLESS SOMETHING EXECUTES THE COMPARISON. m131 DECISION 5 states the
// hazard: a capability the database accepts and Go does not is refused at a
// different layer with a different error, and a capability Go accepts and the
// database does not is a 23514 at INSERT on a path an operator reached through
// a wizard. Neither is visible to the compiler, to `go vet`, or to any unit
// test -- the unit test in internal/mcp pins the Go set by value and has no
// database in front of it.
//
// THE EXTRACTION IS m131 DECISION 5's, VERBATIM, AND NOT A REWRITE OF IT.
// pg_get_constraintdef renders the STORED expression tree, so this reads what
// the database actually enforces and cannot be fooled by editing the migration
// file's text or its comments.
//
// THE THIRD FAILURE MODE IS THE ONE THAT LOOKS LIKE A PASS. Rename the
// constraint and the extraction matches nothing: both difference arms come back
// empty, and "no differences" reads as "the sets agree" while the check has
// verified nothing at all. This file returns an explicit INDETERMINATE for that
// case, before either difference is computed, exactly as the database half
// does. A parity check that cannot find its subject must fail loudly.
//
// NOT BUILT AS A `1/0`-INSIDE-`CASE` GUARD. Postgres constant-folds the
// division at plan time, so that shape raises division_by_zero on every run
// including the passing ones. The guard here is a row count in Go.
package tests

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
)

// m131VocabularyConstraint is the name the extraction looks up and the name the
// INDETERMINATE message prints. ONE constant, bound as $1 rather than
// interpolated into the SQL, so the two can never disagree: a failure message
// naming a constraint the query did not actually look for is a message that
// sends the reader to the wrong place, and the whole point of the INDETERMINATE
// arm is that the reader believes it.
const m131VocabularyConstraint = "mcp_grants_capabilities_vocabulary_check"

// m131VocabularyExtraction is m131 DECISION 5's statement, used rather than
// rewritten so the two halves of the proof read the constraint identically.
const m131VocabularyExtraction = `
	SELECT m[1] AS capability
	  FROM pg_constraint c
	  CROSS JOIN LATERAL regexp_matches(
	           pg_get_constraintdef(c.oid), '''([^'']*)''::text', 'g') AS m
	 WHERE c.conrelid = 'public.mcp_grants'::regclass
	   AND c.conname  = $1`

// TestCapabilityVocabularyMatchesTheDatabaseCheckAsAppRole is the parity proof.
//
// ON THE ROLE, PLAINLY, BECAUSE THE BRIEF ASKS AND BECAUSE OVERCLAIMING HERE
// WOULD BE WORSE THAN SAYING NOTHING: the role does NOT change this particular
// answer. pg_constraint and pg_get_constraintdef are readable by every role,
// carry no RLS, and a CHECK constraint is enforced identically against a
// superuser and against wpmgr_app -- so this read would return the same eight
// strings connected as anyone. The role is asserted and printed anyway for two
// reasons that are real. First, it proves the constraint is visible in the
// database the application actually connects to, rather than in some other one
// a privileged fixture reached. Second, the sibling tests in this file INSERT
// and AUTHENTICATE, where the role is entirely load-bearing -- m127 DECISION 5
// records a live privilege escalation of exactly the shape a superuser fixture
// hides -- and a file whose evidence is uniform is a file where nobody has to
// work out which transaction was the honest one.
func TestCapabilityVocabularyMatchesTheDatabaseCheckAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "m131-parity-"+uuid.NewString()[:8])

	var inDB []string
	// Through InTenantTx -- the production dispatch -- and not a connection this
	// test opened. A test that dials its own connection proves something about a
	// database, not about the one the request path reaches.
	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (vocabulary parity)")
		rows, err := tx.Query(ctx, m131VocabularyExtraction, m131VocabularyConstraint)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c string
			if err := rows.Scan(&c); err != nil {
				return err
			}
			inDB = append(inDB, c)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("read mcp_grants_capabilities_vocabulary_check: %v", err)
	}

	// FAILURE MODE 3, CHECKED FIRST AND BEFORE EITHER DIFFERENCE.
	//
	// Zero rows means the constraint was not found or its definition did not
	// render as the expected literal list -- a rename, a drop, a schema change,
	// or an extraction that no longer matches. Every arm below would then be
	// empty and this test would report a pass having compared nothing against
	// nothing. It is INDETERMINATE, not agreement, and it fails.
	if len(inDB) == 0 {
		t.Fatalf("INDETERMINATE: the extraction returned no rows, so this test "+
			"compared nothing.\n"+
			"Looked for constraint %q on public.mcp_grants.\n"+
			"This is NOT 'the sets agree' -- both difference arms are empty because "+
			"there is nothing to difference against. Either the constraint was "+
			"renamed or dropped, or pg_get_constraintdef no longer renders the "+
			"vocabulary as quoted ::text literals. Fix the extraction or the "+
			"constraint; do not read this as a pass.",
			m131VocabularyConstraint)
	}

	sort.Strings(inDB)
	inGo := make([]string, 0, len(mcp.AllCapabilities()))
	for _, c := range mcp.AllCapabilities() {
		inGo = append(inGo, string(c))
	}
	sort.Strings(inGo)

	// A second empty-set guard, on the other side. AllCapabilities() ranges over
	// capabilityVocabulary, so an emptied map would make the "in Go but not in
	// the database" arm vacuously empty in the same way.
	if len(inGo) == 0 {
		t.Fatal("INDETERMINATE: mcp.AllCapabilities() is empty, so the Go half of " +
			"this comparison contributed nothing")
	}

	dbSet := map[string]struct{}{}
	for _, c := range inDB {
		dbSet[c] = struct{}{}
	}
	goSet := map[string]struct{}{}
	for _, c := range inGo {
		goSet[c] = struct{}{}
	}

	// FAILURE MODE 1: Go holds a member the CHECK does not. This is the
	// direction that produces a 23514 at INSERT, on a path an operator reached
	// through a wizard, after the wizard already told them it would work.
	var goOnly []string
	for _, c := range inGo {
		if _, ok := dbSet[c]; !ok {
			goOnly = append(goOnly, c)
		}
	}

	// FAILURE MODE 2: the CHECK holds a member Go does not. This one does not
	// error at INSERT; it produces a name the database will happily store and
	// that NewCapabilitySet then DROPS as unknown, so the grant authenticates
	// holding less than its row says.
	var dbOnly []string
	for _, c := range inDB {
		if _, ok := goSet[c]; !ok {
			dbOnly = append(dbOnly, c)
		}
	}

	if len(goOnly) > 0 || len(dbOnly) > 0 {
		t.Fatalf("the two closed vocabularies disagree.\n"+
			"  in Go but NOT in the CHECK: %v\n"+
			"      -> a grant naming one of these takes 23514 at INSERT, on the "+
			"wizard path, after the operator was shown it as available\n"+
			"  in the CHECK but NOT in Go:  %v\n"+
			"      -> the database stores it and NewCapabilitySet drops it as "+
			"unknown, so the grant authenticates holding less than its row says\n"+
			"  Go:       %v\n"+
			"  database: %v\n"+
			"Widening either set is a change to the other. The database half is a "+
			"migration (database-engineer); the Go half is capabilityVocabulary in "+
			"apps/api/internal/mcp/policy.go.",
			goOnly, dbOnly, inGo, inDB)
	}

	t.Logf("vocabularies agree on %d capabilities, read from pg_get_constraintdef "+
		"inside InTenantTx: %s", len(inDB), strings.Join(inDB, " "))
}

// m131GrantWithBearer mints a grant carrying an ARBITRARY capability list and a
// live connection token for it, returning the plaintext bearer.
//
// It is s7GrantWithBearer's shape with the one hard-coded value lifted into a
// parameter: that helper pins Capabilities to {mcp.sites.read}, which is
// exactly the value the tests below have to vary. Everything still goes through
// mcp.Repo -- CreateGrantWithCode is what the consent endpoint calls,
// RedeemAuthorizationCode is what the token endpoint calls -- so no row here is
// inserted by a statement this file wrote and no connection is opened by it.
func m131GrantWithBearer(
	t *testing.T, repo *mcp.Repo, tenantID uuid.UUID, caps []string,
) string {
	t.Helper()
	ctx := context.Background()

	clientID := "m131-client-" + uuid.NewString()
	secretHash := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	affected, err := repo.RegisterClient(ctx, sqlc.RegisterMCPOAuthClientParams{
		ClientID:                clientID,
		ClientSecretHash:        &secretHash,
		TokenEndpointAuthMethod: "client_secret_basic",
		RedirectUris:            []string{"https://claude.ai/api/mcp/auth_callback"},
	})
	if err != nil || affected != 1 {
		t.Fatalf("register client: affected=%d err=%v", affected, err)
	}

	codePlain := "m131-code-" + uuid.NewString()
	codeSum := sha256.Sum256([]byte(codePlain))
	approver := domain.Principal{TenantID: tenantID, Scope: domain.ScopeOrg}

	grant, code, err := repo.CreateGrantWithCode(ctx, approver, sqlc.CreateMCPGrantParams{
		TenantID:            tenantID,
		Name:                "m131 grant",
		Status:              "active",
		SiteScopeMode:       "all",
		ScopeTagIds:         []uuid.UUID{},
		ScopeSiteIds:        []uuid.UUID{},
		ClientID:            &clientID,
		Capabilities:        caps,
		ExpiresAt:           time.Now().UTC().Add(90 * 24 * time.Hour),
		IdleExpireAfterDays: nil,
	}, func(grantID uuid.UUID) sqlc.CreateMCPAuthorizationCodeParams {
		return sqlc.CreateMCPAuthorizationCodeParams{
			TenantID:            tenantID,
			GrantID:             grantID,
			ClientID:            clientID,
			CodeHash:            hex.EncodeToString(codeSum[:]),
			CodeChallenge:       "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM",
			CodeChallengeMethod: "S256",
			RedirectUri:         "https://claude.ai/api/mcp/auth_callback",
			ExpiresAt:           time.Now().UTC().Add(5 * time.Minute),
		}
	}, nil)
	if err != nil {
		t.Fatalf("create grant holding %v: %v -- if this is a 23514 the Go "+
			"vocabulary is wider than the CHECK", caps, err)
	}
	if len(grant.Capabilities) != len(caps) {
		t.Fatalf("stored capabilities = %v, want %v", grant.Capabilities, caps)
	}

	bearer := "m131-bearer-" + uuid.NewString()
	bearerSum := sha256.Sum256([]byte(bearer))
	if _, err := repo.RedeemAuthorizationCode(ctx, tenantID, code.ID,
		sqlc.CreateMCPConnectionTokenParams{
			TenantID:    tenantID,
			GrantID:     grant.ID,
			TokenPrefix: "m131tk",
			TokenHash:   hex.EncodeToString(bearerSum[:]),
			Status:      "active",
		}); err != nil {
		t.Fatalf("mint connection token: %v", err)
	}
	return bearer
}

// m131AssertRoleOnBothDispatches asserts, INSIDE the transactions the request
// path actually opens, that this connection is wpmgr_app with neither SUPERUSER
// nor BYPASSRLS -- and prints what it found, so the proof carries its own
// evidence rather than asking the reader to trust it.
//
// TWO HELPERS BECAUSE THE THREE TESTS BELOW USE TWO DISPATCHES. The grant and
// its token are inserted through RunTenantTx (repo.CreateGrantWithCode), and
// Authenticate resolves the bearer through InMCPTokenLookupTx. Asserting only
// one would leave the other unevidenced, and they are different helpers setting
// different GUCs.
//
// WHY THIS IS NOT A FRESH CONNECTION. Both calls go through the SAME db.Pool
// the test hands to mcp.NewRepo, using the SAME dispatch functions the path
// under test uses, so current_user is read on a connection from the pool that
// runs the INSERT and the lookup. Opening a connection of its own would prove
// something about a database rather than about the transactions these tests
// run in -- which is the mistake that left the m112 RLS policies inert while
// every proof passed.
func m131AssertRoleOnBothDispatches(t *testing.T, pool *db.Pool, tenantID uuid.UUID) {
	t.Helper()
	ctx := context.Background()

	principal := domain.Principal{TenantID: tenantID, Scope: domain.ScopeOrg}
	if err := pool.RunTenantTx(ctx, principal, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "RunTenantTx (the grant insert dispatch)")
		return nil
	}); err != nil {
		t.Fatalf("open the grant-insert dispatch: %v", err)
	}

	if err := pool.InMCPTokenLookupTx(ctx, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InMCPTokenLookupTx (the Authenticate dispatch)")
		return nil
	}); err != nil {
		t.Fatalf("open the token-lookup dispatch: %v", err)
	}
}

// TestExistingSitesReadGrantStillAuthenticatesAsAppRole is the no-regression
// half, and it is the one that matters most: every grant this surface has ever
// minted holds exactly {mcp.sites.read}. Widening a containment CHECK is
// monotone so the row still passes, but the GO side changed too -- the ceiling
// grew from one member to seven and Authenticate narrows the stored column
// against it -- and nothing about that is monotone by inspection.
func TestExistingSitesReadGrantStillAuthenticatesAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "m131-legacy-"+uuid.NewString()[:8])
	repo := mcp.NewRepo(pool)
	svc := mcp.NewService(repo)

	// The role is load-bearing HERE: this test INSERTS a grant and
	// AUTHENTICATES a bearer, and either privilege makes both vacuous.
	m131AssertRoleOnBothDispatches(t, pool, tenant)

	bearer := m131GrantWithBearer(t, repo, tenant, []string{"mcp.sites.read"})

	auth, err := svc.Authenticate(ctx, bearer)
	if err != nil {
		t.Fatalf("a pre-m131 grant holding {mcp.sites.read} no longer "+
			"authenticates: %v", err)
	}
	if !auth.Capabilities.Allows(mcp.CapSitesRead) {
		t.Fatalf("resolved capabilities = %v, want to hold %q",
			auth.Capabilities.Sorted(), mcp.CapSitesRead)
	}
	// EXACTLY ONE. A wider resolved set would mean the widening reached an
	// existing credential, which is the failure m131 DECISION 4 refuses a
	// backfill to avoid and which this commit must not reintroduce in Go.
	if auth.Capabilities.Len() != 1 {
		t.Fatalf("a grant whose row holds exactly {mcp.sites.read} resolved to %v.\n"+
			"The stored column is the authority and the ceiling only ever narrows "+
			"it; a resolved set wider than the row is the widening this commit "+
			"exists to prevent, arriving at read time instead of at write time.",
			auth.Capabilities.Sorted())
	}
	t.Logf("legacy grant authenticated holding %v", auth.Capabilities.Sorted())
}

// TestNewlySeatedCapabilityInsertsAndAuthenticatesAsAppRole is the other half:
// m131's point was that the tool registry can now widen with no further schema
// work, and that claim is only true if a grant holding one of the seven newly
// seated names both INSERTS (the CHECK accepts it) and AUTHENTICATES (the Go
// ceiling confers it). Either one failing makes the migration a no-op.
func TestNewlySeatedCapabilityInsertsAndAuthenticatesAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "m131-new-"+uuid.NewString()[:8])
	repo := mcp.NewRepo(pool)
	svc := mcp.NewService(repo)

	// The role is load-bearing HERE: this test INSERTS a grant and
	// AUTHENTICATES a bearer, and either privilege makes both vacuous.
	m131AssertRoleOnBothDispatches(t, pool, tenant)

	want := []string{"mcp.sites.read", "mcp.uptime.read"}
	bearer := m131GrantWithBearer(t, repo, tenant, want)

	auth, err := svc.Authenticate(ctx, bearer)
	if err != nil {
		t.Fatalf("a grant holding a newly seated capability does not "+
			"authenticate: %v -- the row inserted, so this is the Go ceiling "+
			"refusing what the database accepted", err)
	}
	if !auth.Capabilities.Allows(mcp.CapUptimeRead) {
		t.Fatalf("resolved capabilities = %v, want to hold %q",
			auth.Capabilities.Sorted(), mcp.CapUptimeRead)
	}
	if auth.Capabilities.Len() != len(want) {
		t.Fatalf("resolved %v, want exactly %v", auth.Capabilities.Sorted(), want)
	}
	t.Logf("newly seated capability round-tripped: stored %v, resolved %v",
		want, auth.Capabilities.Sorted())
}

// TestContentReadInsertsButDoesNotAuthenticateAsAppRole executes the one
// deliberate asymmetry between the two closed sets, so that it is a proven
// stance rather than a comment.
//
// m131 DECISION 3 seats mcp.content.read in the CHECK and calls it unreachable.
// The database's version of "unreachable" is that no INSERT names it -- the
// CHECK is a ceiling and a member no code writes affects no row -- so the
// database will store it if asked. Go's version is stronger and is where the
// stance is actually held: no scope confers it, so Authenticate's ceiling
// refuses the whole stored set rather than trimming it.
//
// The refusal being WHOLE rather than a trim is the property under test. A trim
// would authenticate the connection holding {mcp.sites.read} and tell nobody
// that the row and the resolved set disagree, which is the silent-narrowing
// failure NarrowTo is written to refuse.
func TestContentReadInsertsButDoesNotAuthenticateAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "m131-content-"+uuid.NewString()[:8])
	repo := mcp.NewRepo(pool)
	svc := mcp.NewService(repo)

	// The role is load-bearing HERE: this test INSERTS a grant and
	// AUTHENTICATES a bearer, and either privilege makes both vacuous.
	m131AssertRoleOnBothDispatches(t, pool, tenant)

	// It INSERTS: the CHECK holds it, which is what m131 seated.
	bearer := m131GrantWithBearer(t, repo, tenant,
		[]string{"mcp.content.read", "mcp.sites.read"})

	// It does NOT authenticate: no scope confers it, so the ceiling refuses.
	auth, err := svc.Authenticate(ctx, bearer)
	if err == nil {
		t.Fatalf("a grant holding %q authenticated, resolving to %v.\n"+
			"Nothing serves that capability -- no post or page table, no agent "+
			"command returning post content, ADR-062 behind ship blockers -- and a "+
			"connection that holds it today would gain real reach the day those "+
			"tools land, with no second consent. Confer it in the diff that ships "+
			"them.", "mcp.content.read", auth.Capabilities.Sorted())
	}
	t.Logf("a grant holding mcp.content.read inserted and was refused at "+
		"Authenticate: %v", err)
}
