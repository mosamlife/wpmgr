package admin

// billing_dto.go — M16 Phase C1 superadmin billing-admin panel: the wire
// response shapes for GET /admin/accounts, GET /admin/accounts/:tenantId, and
// GET /admin/revenue. Hand-rolled JSON (this whole admin area is hand-rolled
// Gin, not ogen — see handler.go), documented here for the frontend.

import (
	"time"

	"github.com/google/uuid"
)

// AccountTiles are the accounts-page header tiles.
type AccountTiles struct {
	MRRCents      int64 `json:"mrr_cents"`
	ActiveSubs    int64 `json:"active_subs"`
	PastDueCount  int64 `json:"past_due_count"`
	AccountsTotal int64 `json:"accounts_total"`
}

// AccountListItem is one row of GET /admin/accounts.
type AccountListItem struct {
	TenantID     uuid.UUID  `json:"tenant_id"`
	OrgName      string     `json:"org_name"`
	OrgSlug      string     `json:"org_slug"`
	OwnerEmail   string     `json:"owner_email,omitempty"`
	Plan         string     `json:"plan"`
	PlanStatus   string     `json:"plan_status"`
	SuspendedAt  *time.Time `json:"suspended_at,omitempty"`
	HasOverrides bool       `json:"has_overrides"`
	MRRCents     int64      `json:"mrr_cents"`
	SitesUsed    int        `json:"sites_used"`
	SitesCap     int        `json:"sites_cap"`
	// StorageUsedBytes is a v1 APPROXIMATION: SUM(backup_chunks.size) for the
	// tenant, which does not yet distinguish CP-managed from BYO-storage
	// destinations (true attribution is a future pass — see the "approximate"
	// label). StorageCapBytes is 0 for the free tier (BYO-storage only; no
	// CP-managed cap to approach).
	StorageUsedBytesApprox int64      `json:"storage_used_bytes_approx"`
	StorageCapBytes        int64      `json:"storage_cap_bytes"`
	NearLimit              bool       `json:"near_limit"`
	CreatedAt              time.Time  `json:"created_at"`
	LastActivity           *time.Time `json:"last_activity,omitempty"`
}

// AccountsResponse is the full GET /admin/accounts body.
type AccountsResponse struct {
	Tiles  AccountTiles      `json:"tiles"`
	Items  []AccountListItem `json:"items"`
	Total  int               `json:"total"` // count AFTER filtering (before limit/offset)
	Limit  int               `json:"limit"`
	Offset int               `json:"offset"`
}

// AccountUsageMeter is one used/cap pair.
type AccountUsageMeter struct {
	Used int64 `json:"used"`
	Cap  int64 `json:"cap"`
}

// AccountEntitlementValues are the ladder's non-numeric-cap reference values
// for the account-detail page's "what this plan includes" rows. Not enforced
// anywhere (see billing.Entitlements' IncrementalBackups/ClientPortal doc
// comment) — display data only.
type AccountEntitlementValues struct {
	ProbeIntervalFloorSec     int  `json:"probe_interval_floor_sec"`
	BackupCadenceFloorSeconds int  `json:"backup_cadence_floor_seconds"`
	IncrementalBackups        bool `json:"incremental_backups"`
	ClientPortal              bool `json:"client_portal"`
}

// AccountUsage is the account-detail usage-vs-limits block.
type AccountUsage struct {
	Sites   AccountUsageMeter `json:"sites"`
	Storage AccountUsageMeter `json:"storage_bytes_approx"`
	Seats   int64             `json:"seats_used"`
	// RestoreVolumeBytesApprox is an APPROXIMATION: SUM of restore_runs'
	// backup_snapshots.total_size for this tenant since the current billing
	// period started (or the last 30 days, when no period is on record yet).
	RestoreVolumeBytesApprox int64                    `json:"restore_volume_bytes_approx"`
	Entitlements             AccountEntitlementValues `json:"entitlements"`
}

// AccountSubscription is the account-detail subscription card.
type AccountSubscription struct {
	Provider               string `json:"provider,omitempty"`
	ProviderCustomerID     string `json:"provider_customer_id,omitempty"`
	ProviderSubscriptionID string `json:"provider_subscription_id,omitempty"`
	// DashboardURL is a Stripe-dashboard deep link built from the configured
	// key's mode (test vs live) — see Service.stripeTestMode. Empty when the
	// provider is not Stripe, or there is no subscription id yet.
	DashboardURL       string     `json:"dashboard_url,omitempty"`
	CurrentPeriodEnd   *time.Time `json:"current_period_end,omitempty"`
	CancelAtPeriodEnd  bool       `json:"cancel_at_period_end"`
	GraceUntil         *time.Time `json:"grace_until,omitempty"`
	CompReason         string     `json:"comp_reason,omitempty"`
	LastBillingEventAt *time.Time `json:"last_billing_event_at,omitempty"`
	// Stale is true when plan_status=="past_due" and LastBillingEventAt is
	// either unset or more than 25 hours old — a signal the webhook pipe may
	// itself be stuck rather than the customer's card.
	Stale bool `json:"stale"`
}

// TimelineEntry is one merged row in the account-detail timeline
// (billing_events + audit_log admin.billing.*/billing.* actions), newest
// first.
type TimelineEntry struct {
	Source     string         `json:"source"` // "billing_event" | "audit"
	OccurredAt time.Time      `json:"occurred_at"`
	Kind       string         `json:"kind"` // billing_events.kind, or audit_log.action
	ActorType  string         `json:"actor_type,omitempty"`
	ActorID    string         `json:"actor_id,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
}

// AccountMember is one row in the account-detail member roster.
type AccountMember struct {
	ID            uuid.UUID  `json:"id"`
	Email         string     `json:"email"`
	Name          string     `json:"name"`
	Role          string     `json:"role"`
	Status        string     `json:"status"`
	EmailVerified bool       `json:"email_verified"`
	LastLoginAt   *time.Time `json:"last_login_at,omitempty"`
	MemberSince   time.Time  `json:"member_since"`
}

// AccountSite is one compact row in the account-detail site list.
type AccountSite struct {
	ID              uuid.UUID `json:"id"`
	URL             string    `json:"url"`
	ConnectionState string    `json:"connection_state"`
	CreatedAt       time.Time `json:"created_at"`
}

// AccountDetail is the full GET /admin/accounts/:tenantId body.
type AccountDetail struct {
	TenantID        uuid.UUID           `json:"tenant_id"`
	OrgName         string              `json:"org_name"`
	OrgSlug         string              `json:"org_slug"`
	OwnerEmail      string              `json:"owner_email,omitempty"`
	Plan            string              `json:"plan"`
	PlanStatus      string              `json:"plan_status"`
	MRRCents        int64               `json:"mrr_cents"`
	CreatedAt       time.Time           `json:"created_at"`
	SuspendedAt     *time.Time          `json:"suspended_at,omitempty"`
	SuspendedReason string              `json:"suspended_reason,omitempty"`
	Usage           AccountUsage        `json:"usage"`
	Subscription    AccountSubscription `json:"subscription"`
	Timeline        []TimelineEntry     `json:"timeline"`
	Members         []AccountMember     `json:"members"`
	Sites           []AccountSite       `json:"sites"`
}

// RevenueTiles are the revenue-page header tiles.
type RevenueTiles struct {
	MRRCents           int64 `json:"mrr_cents"`
	MRRPastDueCents    int64 `json:"mrr_past_due_cents"`
	ActiveSubs         int64 `json:"active_subs"`
	TrialingSubs       int64 `json:"trialing_subs"`
	PastDueCount       int64 `json:"past_due_count"`
	PastDueAtRiskCents int64 `json:"past_due_at_risk_cents"`
	NewThisMonth       int64 `json:"new_this_month"`
	CanceledThisMonth  int64 `json:"canceled_this_month"`
}

// PlanDistributionRow is one row of the revenue page's plan-distribution
// table.
type PlanDistributionRow struct {
	Plan     string `json:"plan"`
	Count    int64  `json:"count"`
	MRRCents int64  `json:"mrr_cents"`
}

// CompedRow is the revenue page's "comped value" summary row — a
// hypothetical figure (what these tenants WOULD pay), never real revenue.
type CompedRow struct {
	Count             int64 `json:"count"`
	HypotheticalCents int64 `json:"hypothetical_value_cents"`
}

// PastDueRevenueRow is one row of the revenue page's past-due list.
type PastDueRevenueRow struct {
	TenantID            uuid.UUID  `json:"tenant_id"`
	OrgName             string     `json:"org_name"`
	OrgSlug             string     `json:"org_slug"`
	OwnerEmail          string     `json:"owner_email,omitempty"`
	AmountCents         int64      `json:"amount_cents"`
	DaysPastDue         int        `json:"days_past_due"`
	GraceUntil          *time.Time `json:"grace_until,omitempty"`
	LastPaymentFailedAt *time.Time `json:"last_payment_failed_at,omitempty"`
}

// RecentBillingEvent is one row of the revenue page's recent-activity feed.
type RecentBillingEvent struct {
	ID         uuid.UUID  `json:"id"`
	OccurredAt time.Time  `json:"occurred_at"`
	OrgName    string     `json:"org_name,omitempty"`
	OrgSlug    string     `json:"org_slug,omitempty"`
	TenantID   *uuid.UUID `json:"tenant_id,omitempty"`
	Kind       string     `json:"kind"`
	Provider   string     `json:"provider"`
}

// RevenueResponse is the full GET /admin/revenue body.
type RevenueResponse struct {
	Tiles                 RevenueTiles          `json:"tiles"`
	PlanDistribution      []PlanDistributionRow `json:"plan_distribution"`
	Comped                CompedRow             `json:"comped"`
	PastDue               []PastDueRevenueRow   `json:"past_due"`
	RecentEvents          []RecentBillingEvent  `json:"recent_events"`
	LastWebhookReceivedAt *time.Time            `json:"last_webhook_received_at,omitempty"`
}
