package billing

// managed_backup_storage_test.go — M16 Phase B gate tests: the DB-free
// decision function (mirrors resolve()'s testability), CheckManagedBackupStorage's
// disabled-is-a-no-op fast path (mirrors TestServiceDisabledIsANoOpWithoutAnyIO
// for CheckSiteCreate), and ManagedStorageAllowed's disabled/enabled behaviour.
//
// White-box, in-memory; no database (CheckManagedBackupStorage's DB-touching
// branch, like CheckSiteCreate's, is exercised in production via
// internal/backup's wiring tests with a fake BillingGate — see
// internal/backup/billing_gate_test.go).

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// ---- managedBackupStorageDecision (DB-free) -------------------------------

func TestManagedBackupStorageDecision_FreeTenantDenied(t *testing.T) {
	err := managedBackupStorageDecision(Entitlements{Plan: TierFree, ManagedBackupStorage: false})
	if err == nil {
		t.Fatal("expected a 402 error for a free-tier entitlement with ManagedBackupStorage=false")
	}
	de, ok := domain.AsDomain(err)
	if !ok {
		t.Fatalf("expected a *domain.Error, got %T", err)
	}
	if de.Kind != domain.KindPaymentRequired {
		t.Fatalf("Kind = %v, want KindPaymentRequired", de.Kind)
	}
	if de.Code != "byo_destination_required" {
		t.Fatalf("Code = %q, want byo_destination_required", de.Code)
	}
	if de.Details["plan"] != "free" {
		t.Fatalf("Details[plan] = %v, want %q", de.Details["plan"], "free")
	}
	if de.Details["has_byo_destination"] != false {
		t.Fatalf("Details[has_byo_destination] = %v, want false", de.Details["has_byo_destination"])
	}
}

func TestManagedBackupStorageDecision_PaidTenantAllowed(t *testing.T) {
	for _, tier := range []Tier{TierStarter, TierAgency, TierScale} {
		t.Run(string(tier), func(t *testing.T) {
			err := managedBackupStorageDecision(Entitlements{Plan: tier, ManagedBackupStorage: true})
			if err != nil {
				t.Fatalf("expected nil for a paid tier with ManagedBackupStorage=true, got %v", err)
			}
		})
	}
}

// TestManagedBackupStorageDecision_GrandfatheredFreeTenantAllowed ties the
// m95 grandfather override directly to the gate's allow/deny outcome: a
// free-plan tenant with the migration's plan_overrides.managed_backup_storage
// override resolves to an ALLOWED decision, exactly as if it were on a paid
// plan.
func TestManagedBackupStorageDecision_GrandfatheredFreeTenantAllowed(t *testing.T) {
	row := tenantBillingRow{
		Plan:          "free",
		PlanStatus:    "none",
		PlanOverrides: []byte(`{"managed_backup_storage": true}`),
	}
	ent, err := resolve(row, time.Now())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if err := managedBackupStorageDecision(ent); err != nil {
		t.Fatalf("expected a grandfathered free tenant to be allowed managed storage, got %v", err)
	}
}

// ---- CheckManagedBackupStorage: disabled is a no-op without any I/O -------

// TestCheckManagedBackupStorageDisabledIsANoOp mirrors
// TestServiceDisabledIsANoOpWithoutAnyIO's CheckSiteCreate assertion: a nil
// pool proves the disabled path never dials the database.
func TestCheckManagedBackupStorageDisabledIsANoOp(t *testing.T) {
	svc := New(nil, nil, false, fixedClock{}, slog.Default())
	if err := svc.CheckManagedBackupStorage(context.Background(), uuid.New()); err != nil {
		t.Fatalf("CheckManagedBackupStorage on a disabled Service should always return nil, got %v", err)
	}
}

// ---- ManagedStorageAllowed -------------------------------------------------

// TestManagedStorageAllowedDisabledIsTrueWithoutAnyIO proves the Me-response
// accessor matches Unlimited().ManagedBackupStorage exactly when hosted
// billing is off, without touching Postgres or Redis (both nil here).
func TestManagedStorageAllowedDisabledIsTrueWithoutAnyIO(t *testing.T) {
	svc := New(nil, nil, false, fixedClock{}, slog.Default())
	allowed, err := svc.ManagedStorageAllowed(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("ManagedStorageAllowed: %v", err)
	}
	if !allowed {
		t.Fatal("ManagedStorageAllowed must be true when hosted billing is disabled")
	}
}
