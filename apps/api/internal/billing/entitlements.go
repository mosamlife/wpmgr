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

	// MonthlyPriceCents is this tier's list price in USD cents (0 for free).
	// This is the CONTRACTED price the superadmin billing panel's MRR figures
	// (M16 Phase C1, internal/admin) are computed from — a single source of
	// truth, never invented ad hoc at a call site. plan_overrides never
	// touches price (overrides only ever adjust resource limits).
	MonthlyPriceCents int

	// IncrementalBackups and ClientPortal are display-only reference flags
	// for the superadmin billing panel's "what this plan includes" rows
	// (M16 Phase C1) — NOT enforced by any gate anywhere in the codebase
	// today (unlike MaxSites/CheckSiteCreate). Adjusting these booleans later
	// carries no functional risk until/unless an enforcement point is added.
	IncrementalBackups bool
	ClientPortal       bool

	// ManagedBackupStorage is ENFORCED (M16 Phase B, CheckManagedBackupStorage):
	// false on the free tier means a tenant may not route a backup RUN to the
	// CP-managed bucket — it must configure a local-folder or S3-compatible
	// destination (internal/sitedestination) instead. Restore/download of a
	// snapshot that already lives in managed storage is NEVER gated by this
	// flag (a customer must always be able to get existing data back, even
	// after losing the entitlement) — only the CREATE-time destination choice
	// is enforced. true on every paid tier and on self-host/hosted-disabled
	// (Unlimited()).
	ManagedBackupStorage bool
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
		MonthlyPriceCents:           0,
		IncrementalBackups:          false,
		ClientPortal:                false,
		ManagedBackupStorage:        false, // BYO-storage only
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
		MonthlyPriceCents:           1500,
		IncrementalBackups:          true,
		ClientPortal:                false,
		ManagedBackupStorage:        true,
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
		MonthlyPriceCents:           5900,
		IncrementalBackups:          true,
		ClientPortal:                true,
		ManagedBackupStorage:        true,
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
		MonthlyPriceCents:           16900,
		IncrementalBackups:          true,
		ClientPortal:                true,
		ManagedBackupStorage:        true,
	},
}

// ValidTier reports whether t is one of the four tiers in the plan ladder
// (free/starter/agency/scale). Used by the superadmin billing panel
// (internal/admin, M16 Phase C1) to validate a manual comp/force-state's
// requested tier without that package ever needing to spell out a tier name
// itself — see the package doc comment + grep_guard_test.go for why only this
// package may know the paid-tier vocabulary.
func ValidTier(t Tier) bool {
	_, ok := planLadder[t]
	return ok
}

// ValidPaidTier reports whether t is one of the three PAID tiers in the plan
// ladder — TierFree is deliberately excluded, unlike ValidTier. Backs
// Service.ValidPaidTier (M16 "sign up into a plan" Phase 0), which lets
// internal/auth validate a self-serve registration's plan hint against the
// real ladder without spelling out a tier name itself.
func ValidPaidTier(t Tier) bool {
	return t != TierFree && ValidTier(t)
}

// MonthlyPriceCentsForTier returns the ladder's list price (USD cents) for
// t, ignoring plan_overrides (overrides never touch price). Returns 0 for an
// unrecognized tier — indistinguishable from free's legitimate 0, which is
// intentional: an invalid tier should never inflate an MRR figure.
func MonthlyPriceCentsForTier(t Tier) int {
	return planLadder[t].MonthlyPriceCents
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
		// ManagedBackupStorage is ENFORCED (unlike the display-only booleans
		// above, which the omit-pattern here leaves at their zero value):
		// Unlimited() is what a WPMGR_HOSTED-disabled Service resolves to, so
		// self-host must NOT come back false here or it would wrongly deny
		// managed backup storage on every self-hosted install. See
		// TestUnlimitedAllowsManagedBackupStorage.
		ManagedBackupStorage: true,
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

	// ManagedBackupStorage is the M16 Phase B grandfather override: a pointer
	// (not a plain bool) so "absent" means "use the ladder base" rather than
	// "force false" — a plain bool would be indistinguishable from an
	// explicit false and could not express "no override" for an ENFORCED
	// field. Written by the m95 grandfather migration for every tenant that
	// existed before this gate shipped (see that migration's comment), and by
	// the superadmin billing panel for a manual comp. Applied AFTER the
	// ladder base in resolve(), so an explicit override always wins.
	ManagedBackupStorage *bool `json:"managed_backup_storage"`
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
	if o.ManagedBackupStorage != nil {
		e.ManagedBackupStorage = *o.ManagedBackupStorage
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

// ResolveEntitlements is the exported form of resolve(), for a caller (the
// superadmin billing panel, internal/admin, M16 Phase C1) that already has a
// tenant's raw plan/plan_status/plan_overrides/grace_until in hand from its
// OWN query (e.g. the accounts-list aggregate) and must not reach back into
// Postgres per-tenant to compute effective caps — that would reintroduce the
// exact N+1 pattern the accounts-list query is designed to avoid. Mirrors
// resolve() exactly; planOverridesJSON may be nil/empty (no overrides).
func ResolveEntitlements(plan, planStatus string, planOverridesJSON []byte, graceUntil *time.Time, now time.Time) (Entitlements, error) {
	return resolve(tenantBillingRow{
		Plan:          plan,
		PlanStatus:    planStatus,
		PlanOverrides: planOverridesJSON,
		GraceUntil:    graceUntil,
	}, now)
}
