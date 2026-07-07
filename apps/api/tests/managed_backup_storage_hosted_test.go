// managed_backup_storage_hosted_test.go — M16 Phase B integration tests:
// CheckManagedBackupStorage against a real Postgres schema (migrations + RLS),
// complementing internal/backup/billing_gate_test.go's fake-based unit tests
// with end-to-end proof that entitlement resolution actually reads the
// seeded tenants row correctly. Requires Docker; skips if unavailable (see
// startPostgres).
package tests

import (
	"context"
	"testing"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// TestManagedBackupStorage_FreeTenantDenied proves CheckManagedBackupStorage
// returns the 402 byo_destination_required shape for a fresh (i.e. NOT
// grandfathered by m95 — it is created after startPostgres's boot-time
// Migrate call) free-plan tenant under WPMGR_HOSTED.
func TestManagedBackupStorage_FreeTenantDenied(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "mbs-free")

	svc := newHostedBillingService(pool, true)
	err := svc.CheckManagedBackupStorage(ctx, tenant)
	de, ok := domain.AsDomain(err)
	if !ok || de.Kind != domain.KindPaymentRequired {
		t.Fatalf("want a 402 byo_destination_required error, got: %v", err)
	}
	if de.Code != "byo_destination_required" {
		t.Fatalf("Code = %q, want byo_destination_required", de.Code)
	}
	if de.Details["plan"] != "free" {
		t.Fatalf("Details[plan] = %v, want free", de.Details["plan"])
	}
}

// TestManagedBackupStorage_PaidTenantAllowed proves a starter-plan (or
// higher) tenant is never gated.
func TestManagedBackupStorage_PaidTenantAllowed(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "mbs-paid")

	if _, err := pool.Exec(ctx, `UPDATE tenants SET plan = 'starter', plan_status = 'active' WHERE id = $1`, tenant); err != nil {
		t.Fatalf("upgrade tenant: %v", err)
	}

	svc := newHostedBillingService(pool, true)
	if err := svc.CheckManagedBackupStorage(ctx, tenant); err != nil {
		t.Fatalf("expected a paid tenant to be allowed managed storage, got %v", err)
	}
}

// TestManagedBackupStorage_GrandfatheredOverrideAllowed proves an explicit
// plan_overrides.managed_backup_storage=true override (the exact shape m95
// writes) allows an otherwise-free tenant.
func TestManagedBackupStorage_GrandfatheredOverrideAllowed(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "mbs-grandfathered")

	if _, err := pool.Exec(ctx,
		`UPDATE tenants SET plan_overrides = jsonb_set(plan_overrides, '{managed_backup_storage}', 'true'::jsonb, true) WHERE id = $1`,
		tenant,
	); err != nil {
		t.Fatalf("apply override: %v", err)
	}

	svc := newHostedBillingService(pool, true)
	if err := svc.CheckManagedBackupStorage(ctx, tenant); err != nil {
		t.Fatalf("expected a grandfathered free tenant to be allowed, got %v", err)
	}
}

// TestManagedBackupStorage_HostedDisabled_Uncapped proves the whole gate is a
// no-op when hosted billing is off — this is what makes M16 Phase B ship
// dark for every current self-hosted/pre-Phase-B deployment.
func TestManagedBackupStorage_HostedDisabled_Uncapped(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "mbs-disabled")

	svc := newHostedBillingService(pool, false)
	if err := svc.CheckManagedBackupStorage(ctx, tenant); err != nil {
		t.Fatalf("expected no error when hosted billing is disabled, got %v", err)
	}
}
