// G2a proof (slice BF-E): mcp.Repo.EligibleDraftIDs, the store behind
// p.allowed_draft_ids, read through the connection's site-scoped transaction
// on the wpmgr_app pool, over request rows seeded as wpmgr_app through the
// shipped insert, approve, outcome and undo statements (the m169 helpers in
// builder_page_edit_m169_integration_test.go).
package tests

import (
	"context"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
)

// TestRepoEligibleDraftIDsAsAppRole: EligibleDraftIDs names a post only when a
// done wpmgr/page-create on the asked site, in the principal's tenant, created
// it and its undo has neither trashed it nor is trashing it. Another site's
// draft, another tenant's draft, an undone, an undoing and a trashed creation
// are never listed, and a principal that is not site-constrained is refused.
func TestRepoEligibleDraftIDsAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	repo := mcp.NewRepo(pool)
	tenantA := seedTenant(t, pool, "g2a-a-"+uuid.NewString()[:8])
	tenantB := seedTenant(t, pool, "g2a-b-"+uuid.NewString()[:8])
	s1 := seedSite(t, pool, tenantA, "")
	s2 := seedSite(t, pool, tenantA, "")
	s3 := seedSite(t, pool, tenantB, "")

	m169Created(t, pool, tenantA, s1, 42, "g2a-s1", nil)
	m169Created(t, pool, tenantA, s2, 50, "g2a-s2", nil)
	m169Created(t, pool, tenantB, s3, 42, "g2a-s3", nil)
	undone := m169Created(t, pool, tenantA, s1, 43, "g2a-undone", nil)
	m169BeginUndo(t, pool, undone)
	m169FinishUndo(t, pool, undone, "undone")
	undoing := m169Created(t, pool, tenantA, s1, 44, "g2a-undoing", nil)
	m169BeginUndo(t, pool, undoing)
	tru := true
	m169Created(t, pool, tenantA, s1, 46, "g2a-trashed", func(oc *sqlc.RecordAbilityRequestOutcomeParams) { oc.Trashed = &tru })

	// The repo runs on this pool through RunTenantTx under the connection's
	// principal; the role is asserted on the same pool and principal.
	both := acprSitePrincipal(tenantA, s1, s2)
	if err := pool.RunTenantTx(ctx, both, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "RunTenantTx (g2a repo pool)")
		return nil
	}); err != nil {
		t.Fatalf("role check: %v", err)
	}

	ids := func(p domain.Principal, site uuid.UUID, post int64) []int64 {
		t.Helper()
		got, err := repo.EligibleDraftIDs(ctx, p, site, post)
		if err != nil {
			t.Fatalf("EligibleDraftIDs(tenant %s, site %s, post %d): %v", p.TenantID, site, post, err)
		}
		if got == nil {
			t.Fatalf("EligibleDraftIDs(tenant %s, site %s, post %d) returned nil; the list is always sent", p.TenantID, site, post)
		}
		return got
	}

	// Positive controls: each tenant's own live draft on its own site.
	for _, c := range []struct {
		p    domain.Principal
		site uuid.UUID
		post int64
	}{
		{both, s1, 42},
		{both, s2, 50},
		{acprSitePrincipal(tenantB, s3), s3, 42},
	} {
		if got := ids(c.p, c.site, c.post); !reflect.DeepEqual(got, []int64{c.post}) {
			t.Fatalf("positive control: (site %s, post %d) listed %v, want [%d]", c.site, c.post, got, c.post)
		}
	}

	for _, c := range []struct {
		what string
		p    domain.Principal
		site uuid.UUID
		post int64
	}{
		{"CROSS-SITE: s2's draft asked for on s1 under a principal for both sites", both, s1, 50},
		{"SITE-SCOPE LEAK: s2's draft under a principal for s1 only", acprSitePrincipal(tenantA, s1), s2, 50},
		{"TENANCY LEAK: tenant B's draft under tenant A's principal", both, s3, 42},
		{"UNDONE DRAFT LISTED: a creation its undo trashed", both, s1, 43},
		{"UNDOING DRAFT LISTED: a creation whose undo is in progress", both, s1, 44},
		{"TRASHED DRAFT LISTED: a creation recorded trashed", both, s1, 46},
		{"a post nothing created", both, s1, 48},
	} {
		if got := ids(c.p, c.site, c.post); len(got) != 0 {
			t.Fatalf("%s (site %s, post %d) listed %v, want []", c.what, c.site, c.post, got)
		}
	}

	// Only a connection's site-scoped transaction runs this read.
	if got, err := repo.EligibleDraftIDs(ctx, acprOrgPrincipal(tenantA), s1, 42); err == nil {
		t.Fatalf("an org-wide principal was answered %v; the read runs only under a connection's site scope", got)
	}
}
