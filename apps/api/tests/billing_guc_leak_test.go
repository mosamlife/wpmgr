package tests

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

// TestBillingCountActiveSites_DoesNotLeakAgentGUC is the regression test for
// security review Finding A: billing_count_active_sites is SECURITY DEFINER
// and sets app.agent='on' in its own body (via set_config(...,true)) so its
// internal SELECT sees every tenant's sites under FORCE ROW LEVEL SECURITY.
// Postgres does NOT roll back an in-body set_config(..., true) at function
// exit — the "true" (is_local) argument scopes the change to the CALLER's
// transaction, not to the function invocation — so before the fix, 'on'
// persisted for the rest of whatever transaction called the function (e.g.
// the CreatePending/Transition site-birth tx that calls CheckSiteCreate),
// silently disabling that transaction's tenant-isolation RLS for every
// statement that ran afterward.
//
// This test proves current_setting('app.agent', true) is unchanged in the
// CALLER's transaction immediately after calling billing_count_active_sites.
// Before the fix this assertion fails: `after` comes back "on" even though
// `before` was "" (unset).
func TestBillingCountActiveSites_DoesNotLeakAgentGUC(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "guc-leak-billing-count")

	var before, after string
	err := pgx.BeginFunc(ctx, pool.Pool, func(tx pgx.Tx) error {
		// Baseline: a fresh transaction has no app.agent set, mirroring a normal
		// operator-scoped InTenantTx (the site-birth tx CheckSiteCreate runs
		// inside).
		if err := tx.QueryRow(ctx, "SELECT coalesce(current_setting('app.agent', true), '')").Scan(&before); err != nil {
			return err
		}

		var count int64
		if err := tx.QueryRow(ctx, "SELECT billing_count_active_sites($1)", tenant).Scan(&count); err != nil {
			return err
		}

		// Re-read app.agent in the SAME transaction right after the call.
		return tx.QueryRow(ctx, "SELECT coalesce(current_setting('app.agent', true), '')").Scan(&after)
	})
	if err != nil {
		t.Fatalf("tx: %v", err)
	}

	if before != "" {
		t.Fatalf("test setup invariant broken: app.agent was already %q before calling billing_count_active_sites", before)
	}
	if after != before {
		t.Fatalf("billing_count_active_sites leaked app.agent into the caller's transaction: before=%q after=%q "+
			"(pre-fix this returns \"on\", which disables this transaction's tenant-isolation RLS for every "+
			"statement that runs afterward — e.g. the rest of a site-birth CreatePending/Transition tx)", before, after)
	}
}

// TestAdminDeleteEmptyTenant_DoesNotLeakAgentGUC is the same regression for
// admin_delete_empty_tenant (m35), which has the identical unrestored-GUC
// pattern, corrected via CREATE OR REPLACE in m91 (see that migration's
// comment: m35 already ran in prod, so editing the m35 file would not
// re-apply there). Covers BOTH return paths (tenant not empty -> false, and
// tenant deleted -> true) since the leak existed on both.
func TestAdminDeleteEmptyTenant_DoesNotLeakAgentGUC(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()

	t.Run("tenant_not_empty", func(t *testing.T) {
		tenant := seedTenant(t, pool, "guc-leak-admin-delete-nonempty")
		repo := site.NewRepo(pool)
		if _, err := repo.Create(ctx, site.CreateInput{TenantID: tenant, URL: "https://guc-leak-nonempty.example.com", Name: "s"}); err != nil {
			t.Fatalf("seed site: %v", err)
		}

		var before, after string
		var deleted bool
		err := pgx.BeginFunc(ctx, pool.Pool, func(tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, "SELECT coalesce(current_setting('app.agent', true), '')").Scan(&before); err != nil {
				return err
			}
			if err := tx.QueryRow(ctx, "SELECT admin_delete_empty_tenant($1)", tenant).Scan(&deleted); err != nil {
				return err
			}
			return tx.QueryRow(ctx, "SELECT coalesce(current_setting('app.agent', true), '')").Scan(&after)
		})
		if err != nil {
			t.Fatalf("tx: %v", err)
		}
		if deleted {
			t.Fatal("admin_delete_empty_tenant should not delete a tenant with sites")
		}
		if before != "" {
			t.Fatalf("test setup invariant broken: app.agent was already %q", before)
		}
		if after != before {
			t.Fatalf("admin_delete_empty_tenant leaked app.agent on the not-empty path: before=%q after=%q", before, after)
		}
	})

	t.Run("tenant_empty", func(t *testing.T) {
		tenant := seedTenant(t, pool, "guc-leak-admin-delete-empty")

		var before, after string
		var deleted bool
		err := pgx.BeginFunc(ctx, pool.Pool, func(tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, "SELECT coalesce(current_setting('app.agent', true), '')").Scan(&before); err != nil {
				return err
			}
			if err := tx.QueryRow(ctx, "SELECT admin_delete_empty_tenant($1)", tenant).Scan(&deleted); err != nil {
				return err
			}
			return tx.QueryRow(ctx, "SELECT coalesce(current_setting('app.agent', true), '')").Scan(&after)
		})
		if err != nil {
			t.Fatalf("tx: %v", err)
		}
		if !deleted {
			t.Fatal("admin_delete_empty_tenant should delete an empty tenant")
		}
		if before != "" {
			t.Fatalf("test setup invariant broken: app.agent was already %q", before)
		}
		if after != before {
			t.Fatalf("admin_delete_empty_tenant leaked app.agent on the empty/deleted path: before=%q after=%q", before, after)
		}
	})
}
