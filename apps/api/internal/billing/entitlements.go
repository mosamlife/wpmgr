// Package billing is the ONLY place in the control plane that knows the
// hosted-SaaS plan ladder (M16 Phase A). Every other domain asks it a
// question ("can this tenant create one more site?", "what is this tenant
// entitled to?") instead of comparing a plan name itself — see
// grep_guard_test.go, which enforces that structurally.
//
// Everything here is a no-op when WPMGR_HOSTED is not enabled: self-host and
// current (pre-Phase-A) prod deployments see zero behavior change. This phase
// ships the entitlement substrate and site-cap enforcement only; there is no
// payment-provider integration yet (Phase B).
package billing

import (
	"encoding/json"
	"math"
	"time"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// Tier is a subscription plan tier. The four values below are the ONLY
// values the tenants.plan CHECK constraint accepts.
type Tier string

const (
	TierFree    Tier = "free"
	TierStarter Tier = "starter"
	TierAgency  Tier = "agency"
	TierScale   Tier = "scale"
)

// Status is a tenant's billing-provider subscription status. Mirrors the
// tenants.plan_status CHECK constraint exactly.
type Status string

const (
	StatusNone     Status = "none"
	StatusTrialing Status = "trialing"
	StatusActive   Status = "active"
	StatusPastDue  Status = "past_due"
	StatusCanceled Status = "canceled"
	StatusPaused   Status = "paused"
	StatusComped   Status = "comped"
)

// Entitlements is the fully-resolved set of limits/floors a tenant is
// entitled to right now (ladder base + plan_overrides delta + status gate).
//
// Only MaxSites is enforced in Phase A (CheckSiteCreate). The remaining
// fields are declared now — ahead of enforcement — so the vocabulary is
// locked and a future pass (storage billing, backup-cadence enforcement, a
// ClickHouse-backed RUM/probe retention move) changes only where these are
// READ, never the shape of the plan model itself.
type Entitlements struct {
	// Plan is the EFFECTIVE tier these limits came from — i.e. after the
	// plan_status gate (a past_due tenant past its grace window resolves to
	// TierFree here even though tenants.plan still says e.g. "starter").
	Plan Tier

	// MaxSites is the cap on non-archived sites (mirrors the sites-list
	// default filter: connection_state <> 'archived'). Enforced by
	// CheckSiteCreate.
	MaxSites int

	// ManagedStorageBytes is the CP-managed backup/media storage allowance.
	// Zero on the free tier: free is BYO-storage only (not yet enforced).
	ManagedStorageBytes int64

	// BackupCadenceFloor is the shortest backup-schedule interval the plan
	// permits (a "floor" on how often, i.e. the plan may not schedule MORE
	// frequently than this duration allows). Not yet enforced.
	BackupCadenceFloor time.Duration

	// ProbeIntervalFloorSec is the shortest uptime-probe interval, in
	// seconds, the plan permits. Not yet enforced.
	ProbeIntervalFloorSec int

	// RetentionDays is the backup retention window. Not yet enforced.
	RetentionDays int

	// SeatsSoft is a soft (advisory, not yet enforced) team-seat count.
	SeatsSoft int

	// RestoreEgressAllowanceBytes is the monthly restore-egress allowance
	// before overage applies. Not yet enforced.
	RestoreEgressAllowanceBytes int64

	// RUMEventsPerMonth is the forward-looking (ClickHouse-era) real-user-
	// monitoring event budget. Not yet enforced.
	RUMEventsPerMonth int64

	// ProbeRetentionDays is the forward-looking uptime-probe history
	// retention window. Not yet enforced.
	ProbeRetentionDays int
}

const (
	kb = 1 << 10
	mb = 1 << 20
	gb = 1 << 30
	tb = 1 << 40
)

// planLadder is the single source of truth for what each tier includes. The
// MaxSites values are LOCKED: free=3, starter=10, agency=50, scale=200.
var planLadder = map[Tier]Entitlements{
	TierFree: {
		Plan:                        TierFree,
		MaxSites:                    3,
		ManagedStorageBytes:         0, // BYO-storage only
		BackupCadenceFloor:          24 * time.Hour,
		ProbeIntervalFloorSec:       300,
		RetentionDays:               7,
		SeatsSoft:                   1,
		RestoreEgressAllowanceBytes: 0,
		RUMEventsPerMonth:           10_000,
		ProbeRetentionDays:          7,
	},
	TierStarter: {
		Plan:                        TierStarter,
		MaxSites:                    10,
		ManagedStorageBytes:         10 * gb,
		BackupCadenceFloor:          24 * time.Hour,
		ProbeIntervalFloorSec:       60,
		RetentionDays:               14,
		SeatsSoft:                   3,
		RestoreEgressAllowanceBytes: 25 * gb,
		RUMEventsPerMonth:           100_000,
		ProbeRetentionDays:          30,
	},
	TierAgency: {
		Plan:                        TierAgency,
		MaxSites:                    50,
		ManagedStorageBytes:         100 * gb,
		BackupCadenceFloor:          time.Hour,
		ProbeIntervalFloorSec:       60,
		RetentionDays:               30,
		SeatsSoft:                   10,
		RestoreEgressAllowanceBytes: 250 * gb,
		RUMEventsPerMonth:           1_000_000,
		ProbeRetentionDays:          90,
	},
	TierScale: {
		Plan:                        TierScale,
		MaxSites:                    200,
		ManagedStorageBytes:         500 * gb,
		BackupCadenceFloor:          time.Hour,
		ProbeIntervalFloorSec:       60,
		RetentionDays:               90,
		SeatsSoft:                   50,
		RestoreEgressAllowanceBytes: 1 * tb,
		RUMEventsPerMonth:           10_000_000,
		ProbeRetentionDays:          180,
	},
}

// Unlimited returns an Entitlements with every limit effectively infinite. It
// is what every check resolves to when WPMGR_HOSTED is disabled, so call
// sites never need their own "is hosted billing on?" branch.
func Unlimited() Entitlements {
	return Entitlements{
		Plan:                        "",
		MaxSites:                    math.MaxInt32,
		ManagedStorageBytes:         math.MaxInt64,
		BackupCadenceFloor:          0,
		ProbeIntervalFloorSec:       0,
		RetentionDays:               math.MaxInt32,
		SeatsSoft:                   math.MaxInt32,
		RestoreEgressAllowanceBytes: math.MaxInt64,
		RUMEventsPerMonth:           math.MaxInt64,
		ProbeRetentionDays:          math.MaxInt32,
	}
}

// tenantBillingRow is the minimal plan-resolution input, decoupled from the
// sqlc-generated row shape so resolve() stays a pure, easily-testable function.
type tenantBillingRow struct {
	Plan          string
	PlanStatus    string
	PlanOverrides []byte
	GraceUntil    *time.Time
}

// overrides is the plan_overrides jsonb shape: a sparse per-key delta applied
// on top of the ladder base. Keys mirror Entitlements' fields in snake_case.
type overrides struct {
	MaxSites                    *int   `json:"max_sites"`
	ManagedStorageBytes         *int64 `json:"managed_storage_bytes"`
	BackupCadenceFloorSeconds   *int64 `json:"backup_cadence_floor_seconds"`
	ProbeIntervalFloorSec       *int   `json:"probe_interval_floor_sec"`
	RetentionDays               *int   `json:"retention_days"`
	SeatsSoft                   *int   `json:"seats_soft"`
	RestoreEgressAllowanceBytes *int64 `json:"restore_egress_allowance_bytes"`
	RUMEventsPerMonth           *int64 `json:"rum_events_per_month"`
	ProbeRetentionDays          *int   `json:"probe_retention_days"`
}

func (o overrides) apply(e *Entitlements) {
	if o.MaxSites != nil {
		e.MaxSites = *o.MaxSites
	}
	if o.ManagedStorageBytes != nil {
		e.ManagedStorageBytes = *o.ManagedStorageBytes
	}
	if o.BackupCadenceFloorSeconds != nil {
		e.BackupCadenceFloor = time.Duration(*o.BackupCadenceFloorSeconds) * time.Second
	}
	if o.ProbeIntervalFloorSec != nil {
		e.ProbeIntervalFloorSec = *o.ProbeIntervalFloorSec
	}
	if o.RetentionDays != nil {
		e.RetentionDays = *o.RetentionDays
	}
	if o.SeatsSoft != nil {
		e.SeatsSoft = *o.SeatsSoft
	}
	if o.RestoreEgressAllowanceBytes != nil {
		e.RestoreEgressAllowanceBytes = *o.RestoreEgressAllowanceBytes
	}
	if o.RUMEventsPerMonth != nil {
		e.RUMEventsPerMonth = *o.RUMEventsPerMonth
	}
	if o.ProbeRetentionDays != nil {
		e.ProbeRetentionDays = *o.ProbeRetentionDays
	}
}

// effectiveTier applies the plan_status gate (locked resolution rule): an
// active/trialing/comped tenant gets its subscribed plan; a past_due tenant
// keeps its subscribed plan until grace_until elapses; every other status
// (none, canceled, paused, or anything unrecognized) resolves to free.
func effectiveTier(plan Tier, status Status, graceUntil *time.Time, now time.Time) Tier {
	switch status {
	case StatusActive, StatusTrialing, StatusComped:
		return plan
	case StatusPastDue:
		if graceUntil != nil && now.Before(*graceUntil) {
			return plan
		}
		return TierFree
	default: // none, canceled, paused, or an unrecognized status
		return TierFree
	}
}

// resolve is the pure, DB-free plan resolution function: ladder[plan] ->
// plan_overrides delta -> plan_status/grace_until gate. Kept free of sqlc/DB
// types so it is trivially unit-testable (see entitlements_test.go).
func resolve(row tenantBillingRow, now time.Time) (Entitlements, error) {
	subscribed := Tier(row.Plan)
	if _, ok := planLadder[subscribed]; !ok {
		// An unrecognized plan value (should be impossible given the CHECK
		// constraint) degrades to free rather than erroring — never block an
		// enroll over a data problem.
		subscribed = TierFree
	}

	effective := effectiveTier(subscribed, Status(row.PlanStatus), row.GraceUntil, now)
	ent := planLadder[effective]

	if len(row.PlanOverrides) > 0 {
		var ov overrides
		if err := json.Unmarshal(row.PlanOverrides, &ov); err != nil {
			return Entitlements{}, domain.Internal("billing_overrides_invalid", "failed to parse plan overrides").WithCause(err)
		}
		ov.apply(&ent)
	}
	return ent, nil
}
