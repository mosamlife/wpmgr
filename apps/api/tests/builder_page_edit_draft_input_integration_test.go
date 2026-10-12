// GetEligibleCreatedDraftInput proofs: the query names the same page-create
// request GetEligibleCreatedDraft names, with its exact input_json, under
// FORCE row security, as wpmgr_app, with rows seeded through the shipped
// statements; nothing for a draft that is not eligible; and nothing across a
// tenant, whether row security or the query's own predicates are what
// separate the tenants.
package tests

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// draftInput runs GetEligibleCreatedDraftInput under principal p. found is
// false on pgx.ErrNoRows; any other error fails the test.
func draftInput(t *testing.T, pool *db.Pool, p domain.Principal, tenant, site uuid.UUID, post int64) (sqlc.GetEligibleCreatedDraftInputRow, bool) {
	t.Helper()
	var row sqlc.GetEligibleCreatedDraftInputRow
	found := true
	if err := pool.RunTenantTx(context.Background(), p, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "RunTenantTx (eligible draft input)")
		var err error
		row, err = sqlc.New(tx).GetEligibleCreatedDraftInput(context.Background(), sqlc.GetEligibleCreatedDraftInputParams{
			TenantID: tenant, SiteID: site, PostID: post,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			found = false
			return nil
		}
		return err
	}); err != nil {
		t.Fatalf("GetEligibleCreatedDraftInput(%s, %s, %d): %v", tenant, site, post, err)
	}
	return row, found
}

// TestGetEligibleCreatedDraftInputAsAppRole: the input of the eligible
// creation of that post on that site in that tenant, the same row
// GetEligibleCreatedDraft names, and nothing across a site, a tenant, or for
// a creation that is not eligible.
func TestGetEligibleCreatedDraftInputAsAppRole(t *testing.T) {
	pool := startPostgres(t)
	tenantA := seedTenant(t, pool, "dpi-a-"+uuid.NewString()[:8])
	tenantB := seedTenant(t, pool, "dpi-b-"+uuid.NewString()[:8])
	s1 := seedSite(t, pool, tenantA, "")
	s2 := seedSite(t, pool, tenantA, "")
	s3 := seedSite(t, pool, tenantB, "")

	// Post 42 created twice on s1 (the newer one is the draft), once on s2
	// and once on tenant B's s3; post 43 trashed by its undo.
	m169Created(t, pool, tenantA, s1, 42, "dpi-s1-old", nil)
	onS1 := m169Created(t, pool, tenantA, s1, 42, "dpi-s1-new", nil)
	onS2 := m169Created(t, pool, tenantA, s2, 42, "dpi-s2", nil)
	onS3 := m169Created(t, pool, tenantB, s3, 42, "dpi-s3", nil)
	undone := m169Created(t, pool, tenantA, s1, 43, "dpi-undone", nil)
	m169BeginUndo(t, pool, undone)
	m169FinishUndo(t, pool, undone, "undone")
	m169Applied(t, pool, tenantA, s1, 44, "dpi-edit")

	both := acprSitePrincipal(tenantA, s1, s2)
	r, ok := draftInput(t, pool, both, tenantA, s1, 42)
	if !ok || r.ID != onS1.ID || r.InputJson != onS1.InputJson {
		t.Fatalf("(A, s1, 42) returned %+v (found %t), want the newer creation %s with its input %q", r, ok, onS1.ID, onS1.InputJson)
	}
	if e, ok := m169Eligible(t, pool, both, tenantA, s1, 42); !ok || e.ID != r.ID {
		t.Fatalf("GetEligibleCreatedDraft named %+v (found %t), the input query %s; they must name one request", e, ok, r.ID)
	}
	if r, ok := draftInput(t, pool, both, tenantA, s2, 42); !ok || r.ID != onS2.ID || r.InputJson != onS2.InputJson {
		t.Fatalf("(A, s2, 42) returned %+v (found %t), want s2's creation %s", r, ok, onS2.ID)
	}
	if r, ok := draftInput(t, pool, acprSitePrincipal(tenantA, s1), tenantA, s2, 42); ok {
		t.Fatalf("SITE-SCOPE LEAK: a principal for s1 only read s2's draft input %s", r.ID)
	}
	if r, ok := draftInput(t, pool, both, tenantB, s3, 42); ok {
		t.Fatalf("TENANCY LEAK: tenant A read tenant B's draft input %s", r.ID)
	}
	if r, ok := draftInput(t, pool, acprSitePrincipal(tenantB, s3), tenantB, s3, 42); !ok || r.ID != onS3.ID || r.InputJson != onS3.InputJson {
		t.Fatalf("positive control: tenant B's own draft input returned %+v (found %t), want %s", r, ok, onS3.ID)
	}
	for _, c := range []struct {
		post int64
		what string
	}{
		{43, "a creation its undo trashed"},
		{44, "a page edit"},
		{45, "a post nothing created"},
	} {
		if r, ok := draftInput(t, pool, both, tenantA, s1, c.post); ok {
			t.Fatalf("%s (post %d) returned an input: %+v", c.what, c.post, r)
		}
	}
}

// draftInputAgent runs GetEligibleCreatedDraftInput in the agent dispatch,
// where the requests' agent policy shows every tenant's rows to SELECT, so
// the query's own predicates are all that bind it. found is false on
// pgx.ErrNoRows; any other error fails the test.
func draftInputAgent(t *testing.T, pool *db.Pool, tenant, site uuid.UUID, post int64) (sqlc.GetEligibleCreatedDraftInputRow, bool) {
	t.Helper()
	var row sqlc.GetEligibleCreatedDraftInputRow
	found := true
	if err := pool.InAgentTx(context.Background(), func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InAgentTx (eligible draft input)")
		var err error
		row, err = sqlc.New(tx).GetEligibleCreatedDraftInput(context.Background(), sqlc.GetEligibleCreatedDraftInputParams{
			TenantID: tenant, SiteID: site, PostID: post,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			found = false
			return nil
		}
		return err
	}); err != nil {
		t.Fatalf("GetEligibleCreatedDraftInput as the agent (%s, %s, %d): %v", tenant, site, post, err)
	}
	return row, found
}

// TestGetEligibleCreatedDraftInputCrossTenant: the query is bound to the
// tenant and the site it names. Row security refuses another tenant's draft
// input to that tenant's own org-wide principal and to a principal whose
// allowlist names the other tenant's site. Where row security shows every
// tenant's rows (the agent dispatch), the query returns nothing for a tenant
// and a site that do not belong together, or for a site with no draft.
//
// Mutation: dropping tenant_id = @tenant_id from the query returns tenant B's
// input for tenant A with B's site and fires "QUERY NOT BOUND TO TENANT".
func TestGetEligibleCreatedDraftInputCrossTenant(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	tenantA := seedTenant(t, pool, "dpx-a-"+uuid.NewString()[:8])
	tenantB := seedTenant(t, pool, "dpx-b-"+uuid.NewString()[:8])
	sA := seedSite(t, pool, tenantA, "")
	sA2 := seedSite(t, pool, tenantA, "")
	sB := seedSite(t, pool, tenantB, "")

	// Post 42 created once on A's sA and once on B's sB; A's sA2 has none.
	onA := m169Created(t, pool, tenantA, sA, 42, "dpx-a", nil)
	onB := m169Created(t, pool, tenantB, sB, 42, "dpx-b", nil)

	// Row security, through the production tenant dispatch.
	if r, ok := draftInput(t, pool, acprOrgPrincipal(tenantA), tenantA, sA, 42); !ok || r.ID != onA.ID || r.InputJson != onA.InputJson {
		t.Fatalf("positive control: A's org principal read %+v (found %t) for its own draft, want %s", r, ok, onA.ID)
	}
	if r, ok := draftInput(t, pool, acprOrgPrincipal(tenantB), tenantB, sB, 42); !ok || r.ID != onB.ID || r.InputJson != onB.InputJson {
		t.Fatalf("positive control: B's org principal read %+v (found %t) for its own draft, want %s", r, ok, onB.ID)
	}
	if r, ok := draftInput(t, pool, acprOrgPrincipal(tenantA), tenantB, sB, 42); ok {
		t.Fatalf("TENANCY LEAK: A's org principal read B's draft input %s", r.ID)
	}
	if r, ok := draftInput(t, pool, acprSitePrincipal(tenantA, sB), tenantB, sB, 42); ok {
		t.Fatalf("TENANCY LEAK: a principal of A whose allowlist names B's site read B's draft input %s", r.ID)
	}

	// The agent dispatch shows both requests, so what follows tests the
	// query's predicates and not row security.
	var seen int
	if err := pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InAgentTx (both requests visible)")
		return tx.QueryRow(ctx, `SELECT count(*) FROM assistant_ability_requests WHERE id = ANY($1)`,
			[]uuid.UUID{onA.ID, onB.ID}).Scan(&seen)
	}); err != nil {
		t.Fatalf("count requests as the agent: %v", err)
	}
	if seen != 2 {
		t.Fatalf("INDETERMINATE: the agent dispatch sees %d of the 2 requests; the query's own predicates are not what is under test", seen)
	}
	for _, c := range []struct {
		tenant, site uuid.UUID
		want         *sqlc.AssistantAbilityRequest
		what         string
	}{
		{tenantA, sA, &onA, "A's own draft"},
		{tenantB, sB, &onB, "B's own draft"},
		{tenantA, sB, nil, "QUERY NOT BOUND TO TENANT: tenant A with B's site"},
		{tenantB, sA, nil, "QUERY NOT BOUND TO TENANT: tenant B with A's site"},
		{tenantA, sA2, nil, "QUERY NOT BOUND TO SITE: A's site with no draft"},
	} {
		r, ok := draftInputAgent(t, pool, c.tenant, c.site, 42)
		switch {
		case c.want == nil && ok:
			t.Fatalf("%s returned %s", c.what, r.ID)
		case c.want != nil && (!ok || r.ID != c.want.ID || r.InputJson != c.want.InputJson):
			t.Fatalf("%s returned %+v (found %t), want %s", c.what, r, ok, c.want.ID)
		}
	}
}
