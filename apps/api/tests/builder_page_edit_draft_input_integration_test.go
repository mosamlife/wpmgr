// GetEligibleCreatedDraftInput proofs: the query names the same page-create
// request GetEligibleCreatedDraft names, with its exact input_json, under
// FORCE row security, as wpmgr_app, with rows seeded through the shipped
// statements; and nothing for a draft that is not eligible.
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
