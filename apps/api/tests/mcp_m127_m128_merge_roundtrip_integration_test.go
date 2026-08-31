// m127 and m128 were built on separate branches and reconciled by merge. m128's
// author predicted the two files commute -- neither reads the other's columns,
// neither backfill touches the other's rows -- and the merge resolution kept
// both sides of the CreateMCPGrant column list on that basis.
//
// THAT PREDICTION IS AN ASSERTION ABOUT A DATABASE, AND UNTIL SOMETHING EXECUTES
// IT IT IS ONLY A CLAIM. These two tests execute it, as wpmgr_app, against the
// real merged schema.
//
// WHY A DEDICATED PROOF RATHER THAN LEANING ON THE m127 SUITE. Dropping
// setup_client from the merged INSERT compiles clean and leaves every existing
// test green, because no Go caller populates the field yet:
//
//	grep -rn "SetupClient" apps/api --include="*.go" | grep -v "internal/db/sqlc/"
//	  -> no matches
//
// So the compiler guards m127's half of the merge and nothing at all guards
// m128's. This file is that missing guard. It drives the sqlc query directly
// rather than Service.Approve precisely because the service layer does not yet
// carry the column -- the query is the surface the merge changed, so the query
// is the surface under test.
//
// THE NULLABILITY SPLIT IS THE POINT AND IT IS ASYMMETRIC ON PURPOSE.
// capabilities and expires_at are NOT NULL, so an omitting caller gets 23502
// rather than an unrestricted or never-expiring connection. setup_client is
// NULLABLE, so an omitting caller gets NULL and every non-wizard insert path
// keeps working. A merge that quietly flipped either direction would still
// compile and still generate; only an executed INSERT can tell them apart.
package tests

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// mcpMergeCreateGrant inserts through RunTenantTx -- the ONLY helper that sets
// app.site_scope, which the four RESTRICTIVE mcp_grants_site_scope_* policies
// read. A test that picked InTenantTx here would leave the GUC unset, the
// coalesced empty string would not equal 'on', the RESTRICTIVE check would pass
// and the policies would be inert while this test stayed green. m127 DECISION 5
// records a live privilege escalation of exactly that shape, fixed 2026-08-30.
//
// The role is asserted INSIDE this transaction, not around it, so the evidence
// belongs to the connection that actually ran the INSERT.
func mcpMergeCreateGrant(t *testing.T, pool interface {
	RunTenantTx(context.Context, db.ScopedPrincipal, func(pgx.Tx) error) error
}, tenantID uuid.UUID, arg sqlc.CreateMCPGrantParams, where string) sqlc.McpGrant {
	t.Helper()
	var out sqlc.McpGrant
	principal := domain.Principal{TenantID: tenantID, Scope: domain.ScopeOrg}
	err := pool.RunTenantTx(context.Background(), principal, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, where)
		gr, err := sqlc.New(tx).CreateMCPGrant(context.Background(), arg)
		if err != nil {
			return err
		}
		out = gr
		return nil
	})
	if err != nil {
		t.Fatalf("%s: CreateMCPGrant: %v", where, err)
	}
	return out
}

// TestMCPMergedGrantCarriesBothMigrationsColumnsAsAppRole supplies m127's three
// columns AND m128's setup_client on one INSERT and asserts every value comes
// back unchanged. This is the merge resolution's central claim: the two column
// sets are independent and both land.
func TestMCPMergedGrantCarriesBothMigrationsColumnsAsAppRole(t *testing.T) {
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "mcp-merge-both-"+uuid.NewString()[:8])

	idleDays := int32(45)
	setupClient := "claude-desktop"
	expires := time.Now().UTC().Add(30 * 24 * time.Hour).Truncate(time.Microsecond)

	created := mcpMergeCreateGrant(t, pool, tenant, sqlc.CreateMCPGrantParams{
		TenantID:      tenant,
		Name:          "m127 and m128 together",
		Status:        "active",
		SiteScopeMode: "all",
		// NOT NULL DEFAULT '{}' -- an empty slice, never nil. nil marshals to
		// NULL and would take 23502 on a column that has a default, which reads
		// as a merge defect and is not one.
		ScopeTagIds:  []uuid.UUID{},
		ScopeSiteIds: []uuid.UUID{},
		// m127.
		Capabilities:        []string{"mcp.sites.read"},
		ExpiresAt:           expires,
		IdleExpireAfterDays: &idleDays,
		// m128.
		SetupClient: &setupClient,
	}, "RunTenantTx (both column sets)")

	// m127: capabilities. The stored set is the authority; an empty array here
	// would mean the merge dropped the value and reached no tool, and a longer
	// one would mean it invented authority nobody granted.
	if len(created.Capabilities) != 1 || created.Capabilities[0] != "mcp.sites.read" {
		t.Fatalf("capabilities = %v, want exactly [mcp.sites.read]", created.Capabilities)
	}

	// m127: expires_at. Compared at microsecond resolution because that is what
	// timestamptz stores; a wider tolerance would hide a column that came back
	// from a different source than it went in on.
	if got := created.ExpiresAt.UTC().Truncate(time.Microsecond); !got.Equal(expires) {
		t.Fatalf("expires_at = %s, want %s", got, expires)
	}

	// m127: idle_expire_after_days, non-NULL on this row specifically so the
	// NULL in the second test is known to be a real answer and not the only
	// value this column can hold.
	if created.IdleExpireAfterDays == nil {
		t.Fatalf("idle_expire_after_days = NULL, want %d", idleDays)
	}
	if *created.IdleExpireAfterDays != idleDays {
		t.Fatalf("idle_expire_after_days = %d, want %d", *created.IdleExpireAfterDays, idleDays)
	}

	// m128: setup_client. THIS IS THE ASSERTION NOTHING ELSE IN THE TREE MAKES.
	if created.SetupClient == nil {
		t.Fatalf("setup_client = NULL, want %q -- the merge dropped m128's column "+
			"from the INSERT and no compiler or existing test would have said so",
			setupClient)
	}
	if *created.SetupClient != setupClient {
		t.Fatalf("setup_client = %q, want %q", *created.SetupClient, setupClient)
	}

	t.Logf("round-tripped capabilities=%v expires_at=%s idle_expire_after_days=%d setup_client=%q",
		created.Capabilities, created.ExpiresAt.UTC().Format(time.RFC3339),
		*created.IdleExpireAfterDays, *created.SetupClient)
}

// TestMCPMergedGrantOmittingSetupClientIsNullAsAppRole is the other half, and it
// is what keeps every non-wizard insert path alive. m128's column is NULLABLE
// WITH NO DEFAULT so a caller that never asked can pass NULL and mean "no
// operator choice was recorded".
//
// NULL IS NOT 'generic'. 'generic' means an operator saw the client cards and
// chose "Other MCP client", which is a different fact that S29 step 9
// distinguishes. A DEFAULT 'generic' -- or a merge that added one -- would make
// every row ever created assert a choice nobody made, so this test fails on any
// non-NULL value rather than only on the wrong string.
func TestMCPMergedGrantOmittingSetupClientIsNullAsAppRole(t *testing.T) {
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "mcp-merge-null-"+uuid.NewString()[:8])

	expires := time.Now().UTC().Add(90 * 24 * time.Hour).Truncate(time.Microsecond)

	created := mcpMergeCreateGrant(t, pool, tenant, sqlc.CreateMCPGrantParams{
		TenantID:      tenant,
		Name:          "m127 only, no operator choice",
		Status:        "active",
		SiteScopeMode: "all",
		ScopeTagIds:   []uuid.UUID{},
		ScopeSiteIds:  []uuid.UUID{},
		Capabilities:  []string{"mcp.sites.read"},
		ExpiresAt:     expires,
		// IdleExpireAfterDays and SetupClient both deliberately omitted. Both are
		// nullable; leaving the fields zero is exactly what a caller that does
		// not know about these columns does.
	}, "RunTenantTx (setup_client omitted)")

	if created.SetupClient != nil {
		t.Fatalf("setup_client = %q on a caller that never supplied it, want NULL -- "+
			"a non-NULL value here means a DEFAULT reached the column and every "+
			"grant now asserts an operator choice nobody made", *created.SetupClient)
	}
	if created.IdleExpireAfterDays != nil {
		t.Fatalf("idle_expire_after_days = %d, want NULL", *created.IdleExpireAfterDays)
	}

	// m127's NOT NULL columns still land on this path, which is what makes the
	// two migrations independent rather than merely co-resident.
	if len(created.Capabilities) != 1 || created.Capabilities[0] != "mcp.sites.read" {
		t.Fatalf("capabilities = %v, want exactly [mcp.sites.read]", created.Capabilities)
	}
	if got := created.ExpiresAt.UTC().Truncate(time.Microsecond); !got.Equal(expires) {
		t.Fatalf("expires_at = %s, want %s", got, expires)
	}

	t.Logf("omitted-path grant: setup_client=NULL idle_expire_after_days=NULL "+
		"capabilities=%v expires_at=%s", created.Capabilities,
		created.ExpiresAt.UTC().Format(time.RFC3339))
}
