package admin

// billing_repo.go — M16 Phase C1 superadmin billing-admin panel data access.
// See db/query/admin_billing.sql for the query definitions and their
// RLS/transaction-context rationale (InAgentTx for a query spanning ALL
// tenants; InTenantTx(tenantID) for a query scoped to exactly one target
// tenant — the ordinary tenant_isolation policy already covers that case).

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// asTimePtr converts a computed-expression interface{} column (e.g. the
// GREATEST() result in AdminListAccounts) to a *time.Time. pgx decodes a
// non-NULL timestamptz scanned into interface{} as time.Time; NULL decodes
// to a nil interface. Mirrors this file's own asBool for computed booleans.
func asTimePtr(v interface{}) *time.Time {
	if v == nil {
		return nil
	}
	if t, ok := v.(time.Time); ok {
		return &t
	}
	return nil
}

func pgTimePtr(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	tm := t.Time
	return &tm
}

func strPtrOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// PlanStatusCount is one (plan, plan_status, count) row — the source data for
// both the accounts-page tiles and the revenue page's plan-distribution +
// MRR tiles.
type PlanStatusCount struct {
	Plan       string
	PlanStatus string
	Count      int64
}

// AccountsListFilter holds every optional narrowing criterion for
// ListAccounts. A zero-value Filter matches everything.
type AccountsListFilter struct {
	Search       string
	Statuses     []string
	Plans        []string
	PastDue      bool
	Comped       bool
	HasOverrides bool
	Idle90d      bool
}

// AccountRow is one raw tenant row from the accounts-list aggregate, before
// Go-side MRR/near-limit/sort computation.
type AccountRow struct {
	TenantID            uuid.UUID
	OrgName             string
	OrgSlug             string
	Plan                string
	PlanStatus          string
	PlanOverrides       []byte
	GraceUntil          *time.Time
	SuspendedAt         *time.Time
	CreatedAt           time.Time
	OwnerEmail          *string
	SitesUsed           int64
	ManagedStorageBytes int64
	LastActivity        *time.Time
}

// BillingRepo provides the superadmin billing-admin panel's data access. It
// mirrors Repo's shape exactly (see repo.go) — a plain *db.Pool, no RLS
// tenant scope of its own, since every query below explicitly chooses
// InAgentTx or InTenantTx(tenantID) per its own RLS needs.
type BillingRepo struct {
	pool *db.Pool
}

// NewBillingRepo builds a BillingRepo over the shared pgx pool.
func NewBillingRepo(pool *db.Pool) *BillingRepo {
	return &BillingRepo{pool: pool}
}

// PlanStatusCounts returns the instance-wide (plan, plan_status) census.
func (r *BillingRepo) PlanStatusCounts(ctx context.Context) ([]PlanStatusCount, error) {
	rows, err := sqlc.New(r.pool.Pool).AdminAccountPlanStatusCounts(ctx)
	if err != nil {
		return nil, domain.Internal("admin_billing_plan_counts_failed", "failed to load plan/status counts").WithCause(err)
	}
	out := make([]PlanStatusCount, 0, len(rows))
	for _, row := range rows {
		out = append(out, PlanStatusCount{Plan: row.Plan, PlanStatus: row.PlanStatus, Count: row.Cnt})
	}
	return out, nil
}

// ListAccounts returns every tenant matching f in ONE aggregate query (see
// AdminListAccounts's doc comment for the LATERAL/CTE shape). Runs under
// InAgentTx (sites_agent/memberships_agent/backup_chunks_agent/audit_log_agent).
func (r *BillingRepo) ListAccounts(ctx context.Context, f AccountsListFilter) ([]AccountRow, error) {
	params := sqlc.AdminListAccountsParams{
		SearchFilter:    f.Search != "",
		StatusFilter:    len(f.Statuses) > 0,
		Statuses:        f.Statuses,
		PlanFilter:      len(f.Plans) > 0,
		Plans:           f.Plans,
		PastDueFilter:   f.PastDue,
		CompedFilter:    f.Comped,
		OverridesFilter: f.HasOverrides,
		IdleFilter:      f.Idle90d,
	}
	if f.Search != "" {
		s := f.Search
		params.Search = &s
	}

	var out []AccountRow
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		rows, qerr := sqlc.New(tx).AdminListAccounts(ctx, params)
		if qerr != nil {
			return qerr
		}
		out = make([]AccountRow, 0, len(rows))
		for _, row := range rows {
			out = append(out, AccountRow{
				TenantID:            row.TenantID,
				OrgName:             row.OrgName,
				OrgSlug:             row.OrgSlug,
				Plan:                row.Plan,
				PlanStatus:          row.PlanStatus,
				PlanOverrides:       row.PlanOverrides,
				GraceUntil:          pgTimePtr(row.GraceUntil),
				SuspendedAt:         pgTimePtr(row.SuspendedAt),
				CreatedAt:           row.CreatedAt,
				OwnerEmail:          row.OwnerEmail,
				SitesUsed:           row.SitesUsed,
				ManagedStorageBytes: row.ManagedStorageBytes,
				LastActivity:        asTimePtr(row.LastActivity),
			})
		}
		return nil
	})
	if err != nil {
		return nil, domain.Internal("admin_billing_list_accounts_failed", "failed to list accounts").WithCause(err)
	}
	return out, nil
}

// AccountHeader is the raw account-detail header row.
type AccountHeader struct {
	TenantID               uuid.UUID
	Name                   string
	Slug                   string
	Plan                   string
	PlanStatus             string
	PlanOverrides          []byte
	GraceUntil             *time.Time
	BillingProvider        string
	ProviderCustomerID     string
	ProviderSubscriptionID string
	CurrentPeriodEnd       *time.Time
	CompReason             string
	SuspendedAt            *time.Time
	SuspendedReason        string
	CancelAtPeriodEnd      bool
	CreatedAt              time.Time
	OwnerEmail             string
}

// GetAccountHeader loads the account-detail header for one tenant. Runs under
// InTenantTx(tenantID). Returns domain.NotFound when the tenant does not exist.
func (r *BillingRepo) GetAccountHeader(ctx context.Context, tenantID uuid.UUID) (AccountHeader, error) {
	var out AccountHeader
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row, qerr := sqlc.New(tx).AdminGetAccountHeader(ctx, tenantID)
		if qerr != nil {
			return qerr
		}
		out = AccountHeader{
			TenantID:               row.ID,
			Name:                   row.Name,
			Slug:                   row.Slug,
			Plan:                   row.Plan,
			PlanStatus:             row.PlanStatus,
			PlanOverrides:          row.PlanOverrides,
			GraceUntil:             pgTimePtr(row.GraceUntil),
			BillingProvider:        strPtrOrEmpty(row.BillingProvider),
			ProviderCustomerID:     strPtrOrEmpty(row.ProviderCustomerID),
			ProviderSubscriptionID: strPtrOrEmpty(row.ProviderSubscriptionID),
			CurrentPeriodEnd:       pgTimePtr(row.CurrentPeriodEnd),
			CompReason:             strPtrOrEmpty(row.CompReason),
			SuspendedAt:            pgTimePtr(row.SuspendedAt),
			SuspendedReason:        strPtrOrEmpty(row.SuspendedReason),
			CancelAtPeriodEnd:      row.CancelAtPeriodEnd,
			CreatedAt:              row.CreatedAt,
			OwnerEmail:             strPtrOrEmpty(row.OwnerEmail),
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return AccountHeader{}, domain.NotFound("account_not_found", "account not found")
		}
		return AccountHeader{}, domain.Internal("admin_billing_get_header_failed", "failed to load account header").WithCause(err)
	}
	return out, nil
}

// AccountUsageRaw is the raw usage-meter row for one tenant.
type AccountUsageRaw struct {
	SitesUsed           int64
	ManagedStorageBytes int64
	SeatsUsed           int64
}

// GetAccountUsage loads sites/storage/seats usage for one tenant. Runs under
// InTenantTx(tenantID).
func (r *BillingRepo) GetAccountUsage(ctx context.Context, tenantID uuid.UUID) (AccountUsageRaw, error) {
	var out AccountUsageRaw
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row, qerr := sqlc.New(tx).AdminGetAccountUsage(ctx, tenantID)
		if qerr != nil {
			return qerr
		}
		out = AccountUsageRaw{SitesUsed: row.SitesUsed, ManagedStorageBytes: row.ManagedStorageBytes, SeatsUsed: row.SeatsUsed}
		return nil
	})
	if err != nil {
		return AccountUsageRaw{}, domain.Internal("admin_billing_get_usage_failed", "failed to load account usage").WithCause(err)
	}
	return out, nil
}

// GetRestoreVolumeApprox returns the approximate restore-egress volume (bytes)
// for tenantID since periodStart: SUM(backup_snapshots.total_size) for every
// restore_runs row in status='completed' whose created_at >= periodStart.
// restore_runs/backup_snapshots are NOT declared in db/schema.sql (a
// pre-existing gap unrelated to this feature — see internal/backup/repo.go's
// own HasActiveRestore, which hand-writes SQL against the same table for the
// same reason), so this is hand-written raw SQL rather than a sqlc query.
// Runs under InTenantTx(tenantID) (both tables carry their own
// tenant_isolation RLS policy).
func (r *BillingRepo) GetRestoreVolumeApprox(ctx context.Context, tenantID uuid.UUID, periodStart time.Time) (int64, error) {
	var total int64
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT COALESCE(SUM(bs.total_size), 0)
			FROM restore_runs rr
			JOIN backup_snapshots bs ON bs.id = rr.snapshot_id AND bs.tenant_id = rr.tenant_id
			WHERE rr.tenant_id = $1
			  AND rr.status = 'completed'
			  AND rr.created_at >= $2`,
			tenantID, periodStart,
		).Scan(&total)
	})
	if err != nil {
		return 0, domain.Internal("admin_billing_restore_volume_failed", "failed to compute restore volume").WithCause(err)
	}
	return total, nil
}

// MemberRow is one raw account-detail member row.
type MemberRow struct {
	ID            uuid.UUID
	Email         string
	Name          string
	Role          string
	Status        string
	EmailVerified bool
	LastLoginAt   *time.Time
	MemberSince   time.Time
}

// ListAccountMembers loads the member roster for one tenant. Runs under
// InTenantTx(tenantID).
func (r *BillingRepo) ListAccountMembers(ctx context.Context, tenantID uuid.UUID) ([]MemberRow, error) {
	var out []MemberRow
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, qerr := sqlc.New(tx).AdminListAccountMembers(ctx, tenantID)
		if qerr != nil {
			return qerr
		}
		out = make([]MemberRow, 0, len(rows))
		for _, row := range rows {
			out = append(out, MemberRow{
				ID:            row.ID,
				Email:         row.Email,
				Name:          row.Name,
				Role:          row.Role,
				Status:        row.Status,
				EmailVerified: asBool(row.EmailVerified),
				LastLoginAt:   pgTimePtr(row.LastLoginAt),
				MemberSince:   row.MemberSince,
			})
		}
		return nil
	})
	if err != nil {
		return nil, domain.Internal("admin_billing_list_members_failed", "failed to list account members").WithCause(err)
	}
	return out, nil
}

// SiteRow is one raw account-detail site row.
type SiteRow struct {
	ID              uuid.UUID
	URL             string
	ConnectionState string
	CreatedAt       time.Time
}

// ListAccountSites loads the compact site list for one tenant. Runs under
// InTenantTx(tenantID).
func (r *BillingRepo) ListAccountSites(ctx context.Context, tenantID uuid.UUID) ([]SiteRow, error) {
	var out []SiteRow
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, qerr := sqlc.New(tx).AdminListAccountSites(ctx, tenantID)
		if qerr != nil {
			return qerr
		}
		out = make([]SiteRow, 0, len(rows))
		for _, row := range rows {
			out = append(out, SiteRow{ID: row.ID, URL: row.Url, ConnectionState: row.ConnectionState, CreatedAt: row.CreatedAt})
		}
		return nil
	})
	if err != nil {
		return nil, domain.Internal("admin_billing_list_sites_failed", "failed to list account sites").WithCause(err)
	}
	return out, nil
}

// BillingEventRow is one raw billing_events timeline row.
type BillingEventRow struct {
	ID         uuid.UUID
	Provider   string
	Kind       string
	OccurredAt time.Time
	Payload    []byte
}

// ListAccountBillingEvents loads up to limit billing_events rows for one
// tenant, newest first. Runs under InTenantTx(tenantID).
func (r *BillingRepo) ListAccountBillingEvents(ctx context.Context, tenantID uuid.UUID, limit int32) ([]BillingEventRow, error) {
	var out []BillingEventRow
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, qerr := sqlc.New(tx).AdminListAccountBillingEvents(ctx, sqlc.AdminListAccountBillingEventsParams{
			TenantID: pgtype.UUID{Bytes: tenantID, Valid: true},
			RowLimit: limit,
		})
		if qerr != nil {
			return qerr
		}
		out = make([]BillingEventRow, 0, len(rows))
		for _, row := range rows {
			out = append(out, BillingEventRow{ID: row.ID, Provider: row.Provider, Kind: row.Kind, OccurredAt: row.OccurredAt, Payload: row.Payload})
		}
		return nil
	})
	if err != nil {
		return nil, domain.Internal("admin_billing_list_events_failed", "failed to list billing events").WithCause(err)
	}
	return out, nil
}

// AuditEventRow is one raw admin.billing.*/billing.* audit timeline row.
type AuditEventRow struct {
	ID        uuid.UUID
	ActorType string
	ActorID   string
	Action    string
	Metadata  []byte
	CreatedAt time.Time
}

// ListAccountAuditEvents loads up to limit admin.billing.*/billing.* audit_log
// rows for one tenant, newest first. Runs under InTenantTx(tenantID).
func (r *BillingRepo) ListAccountAuditEvents(ctx context.Context, tenantID uuid.UUID, limit int32) ([]AuditEventRow, error) {
	var out []AuditEventRow
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, qerr := sqlc.New(tx).AdminListAccountAuditEvents(ctx, sqlc.AdminListAccountAuditEventsParams{TenantID: tenantID, RowLimit: limit})
		if qerr != nil {
			return qerr
		}
		out = make([]AuditEventRow, 0, len(rows))
		for _, row := range rows {
			out = append(out, AuditEventRow{ID: row.ID, ActorType: row.ActorType, ActorID: row.ActorID, Action: row.Action, Metadata: row.Metadata, CreatedAt: row.CreatedAt})
		}
		return nil
	})
	if err != nil {
		return nil, domain.Internal("admin_billing_list_audit_failed", "failed to list account audit events").WithCause(err)
	}
	return out, nil
}

// GetLastBillingEventAt returns the newest billing_events.occurred_at for one
// tenant, or nil when the tenant has no billing_events rows yet. Runs under
// InTenantTx(tenantID).
func (r *BillingRepo) GetLastBillingEventAt(ctx context.Context, tenantID uuid.UUID) (*time.Time, error) {
	var out *time.Time
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		t, qerr := sqlc.New(tx).AdminGetLastBillingEventForTenant(ctx, pgtype.UUID{Bytes: tenantID, Valid: true})
		if qerr != nil {
			if errors.Is(qerr, pgx.ErrNoRows) {
				return nil
			}
			return qerr
		}
		out = &t
		return nil
	})
	if err != nil {
		return nil, domain.Internal("admin_billing_last_event_failed", "failed to load last billing event").WithCause(err)
	}
	return out, nil
}

// PastDueRevenueRowRaw is one raw past-due-list row.
type PastDueRevenueRowRaw struct {
	TenantID            uuid.UUID
	OrgName             string
	OrgSlug             string
	Plan                string
	GraceUntil          *time.Time
	OwnerEmail          *string
	LastPaymentFailedAt *time.Time
}

// ListPastDue loads the revenue page's past-due list. Runs under InAgentTx
// (memberships_agent + billing_events_system).
func (r *BillingRepo) ListPastDue(ctx context.Context) ([]PastDueRevenueRowRaw, error) {
	var out []PastDueRevenueRowRaw
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		rows, qerr := sqlc.New(tx).AdminRevenuePastDueList(ctx)
		if qerr != nil {
			return qerr
		}
		out = make([]PastDueRevenueRowRaw, 0, len(rows))
		for _, row := range rows {
			out = append(out, PastDueRevenueRowRaw{
				TenantID:            row.TenantID,
				OrgName:             row.OrgName,
				OrgSlug:             row.OrgSlug,
				Plan:                row.Plan,
				GraceUntil:          pgTimePtr(row.GraceUntil),
				OwnerEmail:          row.OwnerEmail,
				LastPaymentFailedAt: pgTimePtr(row.LastPaymentFailedAt),
			})
		}
		return nil
	})
	if err != nil {
		return nil, domain.Internal("admin_billing_past_due_failed", "failed to list past-due accounts").WithCause(err)
	}
	return out, nil
}

// ActivationCounts holds the revenue page's calendar-month tenant-activation
// counts.
type ActivationCounts struct {
	NewThisMonth      int64
	CanceledThisMonth int64
}

// GetActivationCountsThisMonth loads new/canceled-this-month tenant counts.
// Runs under InAgentTx (billing_events_system).
func (r *BillingRepo) GetActivationCountsThisMonth(ctx context.Context) (ActivationCounts, error) {
	var out ActivationCounts
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		row, qerr := sqlc.New(tx).AdminRevenueActivationCountsThisMonth(ctx)
		if qerr != nil {
			return qerr
		}
		out = ActivationCounts{NewThisMonth: row.NewThisMonth, CanceledThisMonth: row.CanceledThisMonth}
		return nil
	})
	if err != nil {
		return ActivationCounts{}, domain.Internal("admin_billing_activation_counts_failed", "failed to load activation counts").WithCause(err)
	}
	return out, nil
}

// RecentEventRow is one raw recent-billing-events-feed row.
type RecentEventRow struct {
	ID         uuid.UUID
	Provider   string
	Kind       string
	OccurredAt time.Time
	TenantID   *uuid.UUID
	OrgName    *string
	OrgSlug    *string
}

// ListRecentEvents loads the last 20 billing_events across every tenant. Runs
// under InAgentTx (billing_events_system).
func (r *BillingRepo) ListRecentEvents(ctx context.Context) ([]RecentEventRow, error) {
	var out []RecentEventRow
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		rows, qerr := sqlc.New(tx).AdminRevenueRecentEvents(ctx)
		if qerr != nil {
			return qerr
		}
		out = make([]RecentEventRow, 0, len(rows))
		for _, row := range rows {
			var tid *uuid.UUID
			if row.TenantID.Valid {
				id := uuid.UUID(row.TenantID.Bytes)
				tid = &id
			}
			out = append(out, RecentEventRow{
				ID: row.ID, Provider: row.Provider, Kind: row.Kind, OccurredAt: row.OccurredAt,
				TenantID: tid, OrgName: row.OrgName, OrgSlug: row.OrgSlug,
			})
		}
		return nil
	})
	if err != nil {
		return nil, domain.Internal("admin_billing_recent_events_failed", "failed to list recent billing events").WithCause(err)
	}
	return out, nil
}

// GetLastWebhookReceivedAt returns the newest created_at among real provider
// billing_events deliveries (provider <> 'admin'), or nil when there are
// none yet. Runs under InAgentTx (billing_events_system).
func (r *BillingRepo) GetLastWebhookReceivedAt(ctx context.Context) (*time.Time, error) {
	var out *time.Time
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		t, qerr := sqlc.New(tx).AdminRevenueLastWebhookReceivedAt(ctx)
		if qerr != nil {
			if errors.Is(qerr, pgx.ErrNoRows) {
				return nil
			}
			return qerr
		}
		out = &t
		return nil
	})
	if err != nil {
		return nil, domain.Internal("admin_billing_last_webhook_failed", "failed to load last webhook receipt").WithCause(err)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Mutations
// ---------------------------------------------------------------------------

// SetComp grants a manual comp: plan_status='comped', plan=tier, comp_reason
// set. Runs on the bare pool (tenants carries no RLS).
func (r *BillingRepo) SetComp(ctx context.Context, tenantID uuid.UUID, plan, reason string) error {
	err := sqlc.New(r.pool.Pool).AdminSetTenantComp(ctx, sqlc.AdminSetTenantCompParams{Plan: plan, CompReason: &reason, TenantID: tenantID})
	if err != nil {
		return domain.Internal("admin_billing_set_comp_failed", "failed to grant comp").WithCause(err)
	}
	return nil
}

// RevokeCompToFree reverts a comp to the free/none resting state (used when
// there is no live provider subscription to adopt instead).
func (r *BillingRepo) RevokeCompToFree(ctx context.Context, tenantID uuid.UUID) error {
	if err := sqlc.New(r.pool.Pool).AdminRevokeCompToFree(ctx, tenantID); err != nil {
		return domain.Internal("admin_billing_revoke_comp_failed", "failed to revoke comp").WithCause(err)
	}
	return nil
}

// ClearCompReason clears comp_reason only (used after billing.ReconcileOneNow
// has already written the adopted live-subscription plan/status).
func (r *BillingRepo) ClearCompReason(ctx context.Context, tenantID uuid.UUID) error {
	if err := sqlc.New(r.pool.Pool).AdminClearCompReason(ctx, tenantID); err != nil {
		return domain.Internal("admin_billing_clear_comp_reason_failed", "failed to clear comp reason").WithCause(err)
	}
	return nil
}

// SetOverrides persists the full resolved plan_overrides object (the caller
// has already merged the requested deltas).
func (r *BillingRepo) SetOverrides(ctx context.Context, tenantID uuid.UUID, overridesJSON []byte) error {
	err := sqlc.New(r.pool.Pool).AdminSetOverrides(ctx, sqlc.AdminSetOverridesParams{PlanOverrides: overridesJSON, TenantID: tenantID})
	if err != nil {
		return domain.Internal("admin_billing_set_overrides_failed", "failed to set overrides").WithCause(err)
	}
	return nil
}

// SetGrace extends grace_until.
func (r *BillingRepo) SetGrace(ctx context.Context, tenantID uuid.UUID, until time.Time) error {
	err := sqlc.New(r.pool.Pool).AdminSetGrace(ctx, sqlc.AdminSetGraceParams{
		GraceUntil: pgtype.Timestamptz{Time: until, Valid: true},
		TenantID:   tenantID,
	})
	if err != nil {
		return domain.Internal("admin_billing_set_grace_failed", "failed to extend grace").WithCause(err)
	}
	return nil
}

// SetSuspended sets suspended_at=now(), suspended_reason=reason.
func (r *BillingRepo) SetSuspended(ctx context.Context, tenantID uuid.UUID, reason string) error {
	err := sqlc.New(r.pool.Pool).AdminSetSuspended(ctx, sqlc.AdminSetSuspendedParams{SuspendedReason: &reason, TenantID: tenantID})
	if err != nil {
		return domain.Internal("admin_billing_suspend_failed", "failed to suspend account").WithCause(err)
	}
	return nil
}

// ClearSuspended clears suspended_at/suspended_reason.
func (r *BillingRepo) ClearSuspended(ctx context.Context, tenantID uuid.UUID) error {
	if err := sqlc.New(r.pool.Pool).AdminClearSuspended(ctx, tenantID); err != nil {
		return domain.Internal("admin_billing_restore_failed", "failed to restore account").WithCause(err)
	}
	return nil
}

// ForceState sets plan+plan_status directly (clearing grace_until).
func (r *BillingRepo) ForceState(ctx context.Context, tenantID uuid.UUID, plan, planStatus string) error {
	err := sqlc.New(r.pool.Pool).AdminForceState(ctx, sqlc.AdminForceStateParams{Plan: plan, PlanStatus: planStatus, TenantID: tenantID})
	if err != nil {
		return domain.Internal("admin_billing_force_state_failed", "failed to force billing state").WithCause(err)
	}
	return nil
}
