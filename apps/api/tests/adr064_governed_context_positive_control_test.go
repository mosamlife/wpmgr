package tests

// ADR-064 governed context — the POSITIVE control for the site-scope RLS policy.
//
// adr064_governed_context_rls_integration_test.go proves every negative: a
// site-scoped collaborator cannot author org context, cannot author another
// site's context, cannot UPDATE, cannot DELETE. Nothing there proves the
// positive, so a policy that refused EVERYONE would pass that whole suite
// green. This file is the missing half: a collaborator granted siteA CAN
// author siteA's own context.
//
// The two halves must stay together. canWriteSiteContext in apps/web enables
// the context editor for exactly this principal, so an over-firing policy
// would ship a button that always errors, and a suite of negatives would never
// notice.
//
// The test runs as wpmgr_app through InScopedTenantTx — the same dispatch the
// request path uses — because a test that opens its own connection is a
// superuser test and leaves the policy inert. It asserts the positive AND a
// negative control IN ONE RUN, so a green cannot mean "RLS was switched off".

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestADR064_SiteScopedCollaboratorMayAuthorTheirOwnSitesContext(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()

	f := adr064SeedFixture(t, pool, admin)

	err := adr064AsCollaborator(t, pool, f.tenant, []uuid.UUID{f.siteA}, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx,
			`INSERT INTO site_context_versions
			   (tenant_id, site_id, version, restrictions, guidance, author_type, author_id, provenance)
			 VALUES ($1, $2, 2, $3, $4, 'system', NULL, 'manual')`,
			f.tenant, f.siteA,
			`{"never_touch_theme": true}`,
			`{"brand_voice": "written by the collaborator"}`)
		return e
	})
	if err != nil {
		t.Fatalf("a collaborator granted siteA could NOT author siteA's own context: %v.\n"+
			"canWriteSiteContext in apps/web enables the editor for exactly this principal; "+
			"if the database refuses it, the fix ships a button that always errors", err)
	}

	var n int
	if err := admin.QueryRow(ctx,
		`SELECT count(*) FROM site_context_versions WHERE site_id = $1`, f.siteA).Scan(&n); err != nil {
		t.Fatalf("read back siteA: %v", err)
	}
	if n != 2 {
		t.Fatalf("siteA has %d context versions, want 2 (seed + collaborator write)", n)
	}

	// Negative control in the same run: the identical statement against siteB
	// must be refused, so a green above cannot mean "the policy is inert".
	var insertErr error
	_ = adr064AsCollaborator(t, pool, f.tenant, []uuid.UUID{f.siteA}, func(tx pgx.Tx) error {
		_, insertErr = tx.Exec(ctx,
			`INSERT INTO site_context_versions
			   (tenant_id, site_id, version, restrictions, guidance, author_type, author_id, provenance)
			 VALUES ($1, $2, 2, $3, $4, 'system', NULL, 'manual')`,
			f.tenant, f.siteB,
			`{"never_touch_theme": true}`,
			`{"brand_voice": "written by the collaborator"}`)
		return insertErr
	})
	if insertErr == nil {
		t.Fatal("NEGATIVE CONTROL FAILED: the same collaborator also wrote siteB's context, " +
			"so the positive result above proves nothing about the policy")
	}
	if !adr064IsRowSecurityRefusal(insertErr) {
		t.Fatalf("siteB refusal was not a row-security refusal: %v", insertErr)
	}
	t.Logf("negative control refused as expected: %v", insertErr)
}
