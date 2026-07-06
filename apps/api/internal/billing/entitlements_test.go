package billing

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/gomodule/redigo/redis"
	"github.com/google/uuid"
)

// ---- resolve(): ladder / override / status-gate layering -----------------

func TestResolveLadderBase(t *testing.T) {
	now := time.Now()
	tests := []struct {
		plan     string
		wantTier Tier
		wantMax  int
	}{
		{"free", TierFree, 3},
		{"starter", TierStarter, 10},
		{"agency", TierAgency, 50},
		{"scale", TierScale, 200},
	}
	for _, tt := range tests {
		t.Run(tt.plan, func(t *testing.T) {
			row := tenantBillingRow{Plan: tt.plan, PlanStatus: "active"}
			ent, err := resolve(row, now)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if ent.Plan != tt.wantTier {
				t.Fatalf("Plan = %q, want %q", ent.Plan, tt.wantTier)
			}
			if ent.MaxSites != tt.wantMax {
				t.Fatalf("MaxSites = %d, want %d", ent.MaxSites, tt.wantMax)
			}
		})
	}
}

func TestResolveUnrecognizedPlanDegradesToFree(t *testing.T) {
	row := tenantBillingRow{Plan: "not-a-real-tier", PlanStatus: "active"}
	ent, err := resolve(row, time.Now())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if ent.Plan != TierFree || ent.MaxSites != 3 {
		t.Fatalf("unrecognized plan should degrade to free/3, got %+v", ent)
	}
}

func TestResolveStatusGate(t *testing.T) {
	now := time.Now()
	future := now.Add(time.Hour)
	past := now.Add(-time.Hour)

	tests := []struct {
		name       string
		status     string
		graceUntil *time.Time
		wantTier   Tier
	}{
		{"active keeps paid plan", "active", nil, TierStarter},
		{"trialing keeps paid plan", "trialing", nil, TierStarter},
		{"comped keeps paid plan", "comped", nil, TierStarter},
		{"past_due within grace keeps paid plan", "past_due", &future, TierStarter},
		{"past_due past grace falls back to free", "past_due", &past, TierFree},
		{"past_due with no grace_until falls back to free", "past_due", nil, TierFree},
		{"canceled falls back to free", "canceled", nil, TierFree},
		{"paused falls back to free", "paused", nil, TierFree},
		{"none falls back to free", "none", nil, TierFree},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			row := tenantBillingRow{Plan: "starter", PlanStatus: tt.status, GraceUntil: tt.graceUntil}
			ent, err := resolve(row, now)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if ent.Plan != tt.wantTier {
				t.Fatalf("Plan = %q, want %q", ent.Plan, tt.wantTier)
			}
		})
	}
}

func TestResolveOverridesApplyOnTopOfLadder(t *testing.T) {
	row := tenantBillingRow{
		Plan:          "free",
		PlanStatus:    "none",
		PlanOverrides: []byte(`{"max_sites": 25}`),
	}
	ent, err := resolve(row, time.Now())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if ent.MaxSites != 25 {
		t.Fatalf("MaxSites = %d, want the override value 25", ent.MaxSites)
	}
	// Untouched ladder fields survive the override unchanged.
	if ent.RetentionDays != 7 {
		t.Fatalf("RetentionDays = %d, want the free-tier ladder value 7 (override must not clobber other fields)", ent.RetentionDays)
	}
}

func TestResolveOverridesOnPaidPlan(t *testing.T) {
	row := tenantBillingRow{
		Plan:          "starter",
		PlanStatus:    "active",
		PlanOverrides: []byte(`{"managed_storage_bytes": 999, "seats_soft": 7}`),
	}
	ent, err := resolve(row, time.Now())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if ent.ManagedStorageBytes != 999 {
		t.Fatalf("ManagedStorageBytes = %d, want 999", ent.ManagedStorageBytes)
	}
	if ent.SeatsSoft != 7 {
		t.Fatalf("SeatsSoft = %d, want 7", ent.SeatsSoft)
	}
	if ent.MaxSites != 10 {
		t.Fatalf("MaxSites = %d, want the untouched starter ladder value 10", ent.MaxSites)
	}
}

func TestResolveEmptyOverridesIsANoop(t *testing.T) {
	row := tenantBillingRow{Plan: "free", PlanStatus: "none", PlanOverrides: []byte(`{}`)}
	ent, err := resolve(row, time.Now())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if ent.MaxSites != 3 {
		t.Fatalf("MaxSites = %d, want the unmodified free-tier value 3", ent.MaxSites)
	}
}

func TestResolveInvalidOverridesJSONErrors(t *testing.T) {
	row := tenantBillingRow{Plan: "free", PlanStatus: "none", PlanOverrides: []byte(`not json`)}
	if _, err := resolve(row, time.Now()); err == nil {
		t.Fatal("expected an error for malformed plan_overrides JSON")
	}
}

// ---- Unlimited() ------------------------------------------------------

func TestUnlimitedHasNoEffectiveCap(t *testing.T) {
	u := Unlimited()
	if u.MaxSites <= planLadder[TierScale].MaxSites {
		t.Fatalf("Unlimited().MaxSites = %d, want it to exceed every real tier's cap", u.MaxSites)
	}
}

// ---- Entitlements()/CheckSiteCreate() no-op when hosted billing is off ---

func TestServiceDisabledIsANoOpWithoutAnyIO(t *testing.T) {
	// pool and redis are both nil: if Entitlements touched either, this would
	// panic. Proves the disabled path never dials the database or Redis.
	svc := New(nil, nil, false, fixedClock{}, slog.Default())

	ent, err := svc.Entitlements(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("Entitlements: %v", err)
	}
	if ent.MaxSites != Unlimited().MaxSites {
		t.Fatalf("disabled Service should resolve to Unlimited(), got %+v", ent)
	}

	// CheckSiteCreate must also no-op without touching the (nil) tx.
	if err := svc.CheckSiteCreate(context.Background(), nil, uuid.New()); err != nil {
		t.Fatalf("CheckSiteCreate on a disabled Service should always return nil, got %v", err)
	}
}

// ---- Redis cache: fail-open on any Redis error ---------------------------

// alwaysFailDial simulates "Redis is unreachable": every Dial attempt errors,
// so any pool.GetContext call fails immediately without a network timeout.
func alwaysFailDial() (redis.Conn, error) {
	return nil, errors.New("simulated redis outage")
}

func TestGetCachedFailsOpenWhenRedisDown(t *testing.T) {
	pool := &redis.Pool{Dial: alwaysFailDial, MaxIdle: 1}
	defer pool.Close()

	svc := &Service{redis: pool, logger: slog.Default()}
	ent, ok := svc.getCached(context.Background(), uuid.New())
	if ok {
		t.Fatal("getCached should report a miss when redis is down, not panic or error out")
	}
	if ent != (Entitlements{}) {
		t.Fatalf("getCached should return the zero value on a miss, got %+v", ent)
	}
}

func TestSetCachedFailsOpenWhenRedisDown(t *testing.T) {
	pool := &redis.Pool{Dial: alwaysFailDial, MaxIdle: 1}
	defer pool.Close()

	svc := &Service{redis: pool, logger: slog.Default()}
	// Must not panic and must return (setCached has no return value — this
	// test's only assertion is that it completes without panicking).
	svc.setCached(context.Background(), uuid.New(), planLadder[TierFree])
}

func TestGetCachedNilRedisIsAMiss(t *testing.T) {
	svc := &Service{redis: nil, logger: slog.Default()}
	if _, ok := svc.getCached(context.Background(), uuid.New()); ok {
		t.Fatal("getCached with a nil redis pool must always report a miss")
	}
}

// fixedClock is a minimal domain.Clock for tests that do not care about the
// exact instant.
type fixedClock struct{}

func (fixedClock) Now() time.Time { return time.Now() }
