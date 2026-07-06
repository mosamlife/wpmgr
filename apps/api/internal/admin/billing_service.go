package admin

// billing_service.go — M16 Phase C1 superadmin billing-admin panel business
// logic: accounts list (filter/sort/MRR), account detail, revenue, and the
// six manual-override mutations. Every mutation here follows the SAME
// pattern: validate -> persist (BillingRepo) -> invalidate the tenant's
// cached Entitlements (billing.Service) -> record an audit.Recorder entry
// under the TARGET tenant's own hash chain (ActorType=ActorUser + the
// superadmin's REAL user id — never a synthetic actor string, see the
// audit_log actor_id ::uuid-cast incident this rule guards against) -> record
// a billing_events row (source="admin", via billing.Service.
// RecordAdminBillingEvent). A reason is REQUIRED (non-empty) on every
// mutation and is stored verbatim in both the audit metadata and the
// billing_events payload.

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/billing"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// gbBytes is 1 GiB in bytes — used to convert the overrides endpoint's
// storage_gb delta to the plan_overrides.managed_storage_bytes key.
const gbBytes = int64(1) << 30

// nearLimitThreshold is the usage/cap ratio (>=) that marks an account
// "near-limit" for the accounts-list filter/badge.
const nearLimitThreshold = 0.8

// staleBillingEventWindow is how long a past_due tenant may go without a
// billing_events row before the account-detail subscription card flags the
// webhook pipe itself (not just the customer) as possibly stuck.
const staleBillingEventWindow = 25 * time.Hour

func requireReason(reason string) error {
	if reason == "" {
		return domain.Validation("reason_required", "a reason is required for this action")
	}
	return nil
}

// billingPanelReady reports whether SetBillingPanel has been called with a
// non-nil repo. Every method below returns a clean ServiceUnavailable rather
// than a nil-pointer panic when it has not.
func (s *Service) billingPanelReady() bool {
	return s.billingRepo != nil
}

func errBillingPanelNotWired() error {
	return domain.ServiceUnavailable("admin_billing_panel_not_wired", "the billing-admin panel is not configured on this instance")
}

// ---------------------------------------------------------------------------
// Accounts list
// ---------------------------------------------------------------------------

// AccountsListOptions holds GET /admin/accounts' query parameters.
type AccountsListOptions struct {
	Search       string
	Statuses     []string
	Plans        []string
	PastDue      bool
	NearLimit    bool
	HasOverrides bool
	Comped       bool
	Idle90d      bool
	Limit        int
	Offset       int
}

// accountComputed bundles the Go-side-computed fields for one AccountRow,
// used both for the response item and for the needs-attention sort.
type accountComputed struct {
	row          AccountRow
	mrrCents     int64
	sitesCap     int
	storageCap   int64
	hasOverrides bool
	nearLimit    bool
}

// ListAccounts is the superadmin accounts-list: header tiles (instance-wide,
// unaffected by the current filter) + a filtered/sorted/paginated item list.
func (s *Service) ListAccounts(ctx context.Context, opts AccountsListOptions) (AccountsResponse, error) {
	if !s.billingPanelReady() {
		return AccountsResponse{}, errBillingPanelNotWired()
	}
	limit := opts.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	offset := opts.Offset
	if offset < 0 {
		offset = 0
	}

	tiles, err := s.accountTiles(ctx)
	if err != nil {
		return AccountsResponse{}, err
	}

	rows, err := s.billingRepo.ListAccounts(ctx, AccountsListFilter{
		Search:       opts.Search,
		Statuses:     opts.Statuses,
		Plans:        opts.Plans,
		PastDue:      opts.PastDue,
		Comped:       opts.Comped,
		HasOverrides: opts.HasOverrides,
		Idle90d:      opts.Idle90d,
	})
	if err != nil {
		return AccountsResponse{}, err
	}

	now := time.Now().UTC()
	computed := make([]accountComputed, 0, len(rows))
	for _, row := range rows {
		c, cerr := computeAccountRow(row, now)
		if cerr != nil {
			// A single tenant's corrupt plan_overrides must never break the
			// whole list; skip it (mirrors resolve()'s error path elsewhere in
			// billing) rather than fail the entire request.
			continue
		}
		if opts.NearLimit && !c.nearLimit {
			continue
		}
		computed = append(computed, c)
	}

	sortAccountsNeedsAttention(computed)

	total := len(computed)
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	page := computed[offset:end]

	items := make([]AccountListItem, 0, len(page))
	for _, c := range page {
		items = append(items, toAccountListItem(c))
	}

	return AccountsResponse{Tiles: tiles, Items: items, Total: total, Limit: limit, Offset: offset}, nil
}

// computeAccountRow resolves the ladder-aware effective caps/MRR/near-limit
// for one raw AccountRow.
func computeAccountRow(row AccountRow, now time.Time) (accountComputed, error) {
	ent, err := billing.ResolveEntitlements(row.Plan, row.PlanStatus, row.PlanOverrides, row.GraceUntil, now)
	if err != nil {
		return accountComputed{}, err
	}
	mrr := int64(0)
	if row.PlanStatus == string(billing.StatusActive) || row.PlanStatus == string(billing.StatusPastDue) {
		mrr = int64(billing.MonthlyPriceCentsForTier(billing.Tier(row.Plan)))
	}
	hasOverrides := hasNonEmptyOverrides(row.PlanOverrides)
	near := false
	if ent.MaxSites > 0 && float64(row.SitesUsed)/float64(ent.MaxSites) >= nearLimitThreshold {
		near = true
	}
	if ent.ManagedStorageBytes > 0 && float64(row.ManagedStorageBytes)/float64(ent.ManagedStorageBytes) >= nearLimitThreshold {
		near = true
	}
	return accountComputed{
		row:          row,
		mrrCents:     mrr,
		sitesCap:     ent.MaxSites,
		storageCap:   ent.ManagedStorageBytes,
		hasOverrides: hasOverrides,
		nearLimit:    near,
	}, nil
}

func hasNonEmptyOverrides(raw []byte) bool {
	if len(raw) == 0 {
		return false
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return false
	}
	return len(m) > 0
}

// sortAccountsNeedsAttention implements the default "needs-attention" order:
// suspended first, then past_due (oldest grace-first — closest to running
// out first), then active (MRR desc), then everything else (created_at
// desc). Stable within each bucket by tenant id so the order is
// deterministic across repeated calls with tied sort keys.
func sortAccountsNeedsAttention(items []accountComputed) {
	bucket := func(c accountComputed) int {
		switch {
		case c.row.SuspendedAt != nil:
			return 0
		case c.row.PlanStatus == string(billing.StatusPastDue):
			return 1
		case c.row.PlanStatus == string(billing.StatusActive):
			return 2
		default:
			return 3
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		bi, bj := bucket(items[i]), bucket(items[j])
		if bi != bj {
			return bi < bj
		}
		switch bi {
		case 1: // past_due: oldest grace (soonest to expire) first
			gi, gj := items[i].row.GraceUntil, items[j].row.GraceUntil
			if gi == nil || gj == nil {
				return gi != nil // a missing grace sorts last within this bucket
			}
			if !gi.Equal(*gj) {
				return gi.Before(*gj)
			}
		case 2: // active: MRR desc
			if items[i].mrrCents != items[j].mrrCents {
				return items[i].mrrCents > items[j].mrrCents
			}
		default:
			if !items[i].row.CreatedAt.Equal(items[j].row.CreatedAt) {
				return items[i].row.CreatedAt.After(items[j].row.CreatedAt)
			}
		}
		return items[i].row.TenantID.String() < items[j].row.TenantID.String()
	})
}

func toAccountListItem(c accountComputed) AccountListItem {
	item := AccountListItem{
		TenantID:               c.row.TenantID,
		OrgName:                c.row.OrgName,
		OrgSlug:                c.row.OrgSlug,
		Plan:                   c.row.Plan,
		PlanStatus:             c.row.PlanStatus,
		SuspendedAt:            c.row.SuspendedAt,
		HasOverrides:           c.hasOverrides,
		MRRCents:               c.mrrCents,
		SitesUsed:              int(c.row.SitesUsed),
		SitesCap:               c.sitesCap,
		StorageUsedBytesApprox: c.row.ManagedStorageBytes,
		StorageCapBytes:        c.storageCap,
		NearLimit:              c.nearLimit,
		CreatedAt:              c.row.CreatedAt,
		LastActivity:           c.row.LastActivity,
	}
	if c.row.OwnerEmail != nil {
		item.OwnerEmail = *c.row.OwnerEmail
	}
	return item
}

// accountTiles computes the accounts-page header tiles from the instance-wide
// (plan, plan_status) census — always unfiltered, regardless of the current
// list filter.
func (s *Service) accountTiles(ctx context.Context) (AccountTiles, error) {
	counts, err := s.billingRepo.PlanStatusCounts(ctx)
	if err != nil {
		return AccountTiles{}, err
	}
	var t AccountTiles
	for _, c := range counts {
		t.AccountsTotal += c.Count
		switch c.PlanStatus {
		case string(billing.StatusActive):
			t.ActiveSubs += c.Count
			t.MRRCents += int64(c.Count) * int64(billing.MonthlyPriceCentsForTier(billing.Tier(c.Plan)))
		case string(billing.StatusPastDue):
			t.PastDueCount += c.Count
			t.MRRCents += int64(c.Count) * int64(billing.MonthlyPriceCentsForTier(billing.Tier(c.Plan)))
		}
	}
	return t, nil
}

// ---------------------------------------------------------------------------
// Account detail
// ---------------------------------------------------------------------------

// timelineLimit bounds how many rows each half of the merged timeline
// fetches before the merge+truncate.
const timelineLimit = 50

// GetAccountDetail resolves the full account-detail page for one tenant.
func (s *Service) GetAccountDetail(ctx context.Context, tenantID uuid.UUID) (AccountDetail, error) {
	if !s.billingPanelReady() {
		return AccountDetail{}, errBillingPanelNotWired()
	}
	header, err := s.billingRepo.GetAccountHeader(ctx, tenantID)
	if err != nil {
		return AccountDetail{}, err
	}
	now := time.Now().UTC()
	ent, eerr := billing.ResolveEntitlements(header.Plan, header.PlanStatus, header.PlanOverrides, header.GraceUntil, now)
	if eerr != nil {
		return AccountDetail{}, eerr
	}

	usageRaw, err := s.billingRepo.GetAccountUsage(ctx, tenantID)
	if err != nil {
		return AccountDetail{}, err
	}

	periodStart := now.AddDate(0, 0, -30)
	if header.CurrentPeriodEnd != nil {
		periodStart = header.CurrentPeriodEnd.AddDate(0, -1, 0)
	}
	restoreVolume, rerr := s.billingRepo.GetRestoreVolumeApprox(ctx, tenantID, periodStart)
	if rerr != nil {
		restoreVolume = 0 // approximation-only meter; never fail the whole page over it
	}

	members, err := s.billingRepo.ListAccountMembers(ctx, tenantID)
	if err != nil {
		return AccountDetail{}, err
	}
	sites, err := s.billingRepo.ListAccountSites(ctx, tenantID)
	if err != nil {
		return AccountDetail{}, err
	}
	billingEvents, err := s.billingRepo.ListAccountBillingEvents(ctx, tenantID, timelineLimit)
	if err != nil {
		return AccountDetail{}, err
	}
	auditEvents, err := s.billingRepo.ListAccountAuditEvents(ctx, tenantID, timelineLimit)
	if err != nil {
		return AccountDetail{}, err
	}
	lastBillingEventAt, err := s.billingRepo.GetLastBillingEventAt(ctx, tenantID)
	if err != nil {
		lastBillingEventAt = nil
	}

	mrr := int64(0)
	if header.PlanStatus == string(billing.StatusActive) || header.PlanStatus == string(billing.StatusPastDue) {
		mrr = int64(billing.MonthlyPriceCentsForTier(billing.Tier(header.Plan)))
	}

	stale := header.PlanStatus == string(billing.StatusPastDue) &&
		(lastBillingEventAt == nil || now.Sub(*lastBillingEventAt) > staleBillingEventWindow)

	dashboardURL := ""
	if header.BillingProvider == "stripe" && header.ProviderSubscriptionID != "" {
		base := "https://dashboard.stripe.com/"
		if s.stripeTestMode {
			base += "test/"
		}
		dashboardURL = base + "subscriptions/" + header.ProviderSubscriptionID
	}

	timeline := mergeTimeline(billingEvents, auditEvents)

	detail := AccountDetail{
		TenantID:        header.TenantID,
		OrgName:         header.Name,
		OrgSlug:         header.Slug,
		OwnerEmail:      header.OwnerEmail,
		Plan:            header.Plan,
		PlanStatus:      header.PlanStatus,
		MRRCents:        mrr,
		CreatedAt:       header.CreatedAt,
		SuspendedAt:     header.SuspendedAt,
		SuspendedReason: header.SuspendedReason,
		Usage: AccountUsage{
			Sites:                    AccountUsageMeter{Used: usageRaw.SitesUsed, Cap: int64(ent.MaxSites)},
			Storage:                  AccountUsageMeter{Used: usageRaw.ManagedStorageBytes, Cap: ent.ManagedStorageBytes},
			Seats:                    usageRaw.SeatsUsed,
			RestoreVolumeBytesApprox: restoreVolume,
			Entitlements: AccountEntitlementValues{
				ProbeIntervalFloorSec:     ent.ProbeIntervalFloorSec,
				BackupCadenceFloorSeconds: int(ent.BackupCadenceFloor.Seconds()),
				IncrementalBackups:        ent.IncrementalBackups,
				ClientPortal:              ent.ClientPortal,
			},
		},
		Subscription: AccountSubscription{
			Provider:               header.BillingProvider,
			ProviderCustomerID:     header.ProviderCustomerID,
			ProviderSubscriptionID: header.ProviderSubscriptionID,
			DashboardURL:           dashboardURL,
			CurrentPeriodEnd:       header.CurrentPeriodEnd,
			CancelAtPeriodEnd:      header.CancelAtPeriodEnd,
			GraceUntil:             header.GraceUntil,
			CompReason:             header.CompReason,
			LastBillingEventAt:     lastBillingEventAt,
			Stale:                  stale,
		},
		Timeline: timeline,
		Members:  toAccountMembers(members),
		Sites:    toAccountSites(sites),
	}
	return detail, nil
}

func mergeTimeline(billingEvents []BillingEventRow, auditEvents []AuditEventRow) []TimelineEntry {
	out := make([]TimelineEntry, 0, len(billingEvents)+len(auditEvents))
	for _, e := range billingEvents {
		var meta map[string]any
		_ = json.Unmarshal(e.Payload, &meta)
		out = append(out, TimelineEntry{Source: "billing_event", OccurredAt: e.OccurredAt, Kind: e.Kind, Metadata: meta})
	}
	for _, e := range auditEvents {
		var meta map[string]any
		_ = json.Unmarshal(e.Metadata, &meta)
		out = append(out, TimelineEntry{Source: "audit", OccurredAt: e.CreatedAt, Kind: e.Action, ActorType: e.ActorType, ActorID: e.ActorID, Metadata: meta})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].OccurredAt.After(out[j].OccurredAt) })
	if len(out) > timelineLimit {
		out = out[:timelineLimit]
	}
	return out
}

func toAccountMembers(rows []MemberRow) []AccountMember {
	out := make([]AccountMember, 0, len(rows))
	for _, r := range rows {
		out = append(out, AccountMember{
			ID: r.ID, Email: r.Email, Name: r.Name, Role: r.Role, Status: r.Status,
			EmailVerified: r.EmailVerified, LastLoginAt: r.LastLoginAt, MemberSince: r.MemberSince,
		})
	}
	return out
}

func toAccountSites(rows []SiteRow) []AccountSite {
	out := make([]AccountSite, 0, len(rows))
	for _, r := range rows {
		out = append(out, AccountSite{ID: r.ID, URL: r.URL, ConnectionState: r.ConnectionState, CreatedAt: r.CreatedAt})
	}
	return out
}

// ---------------------------------------------------------------------------
// Revenue
// ---------------------------------------------------------------------------

// GetRevenue resolves the full revenue page — LOCAL STATE ONLY (tenants +
// billing_events), zero provider API calls.
func (s *Service) GetRevenue(ctx context.Context) (RevenueResponse, error) {
	if !s.billingPanelReady() {
		return RevenueResponse{}, errBillingPanelNotWired()
	}
	counts, err := s.billingRepo.PlanStatusCounts(ctx)
	if err != nil {
		return RevenueResponse{}, err
	}

	var tiles RevenueTiles
	type planAgg struct {
		count    int64
		mrrCents int64
	}
	byPlan := map[string]*planAgg{}
	var compedCount int64
	var compedValueCents int64
	for _, c := range counts {
		if byPlan[c.Plan] == nil {
			byPlan[c.Plan] = &planAgg{}
		}
		byPlan[c.Plan].count += c.Count
		price := int64(billing.MonthlyPriceCentsForTier(billing.Tier(c.Plan)))
		switch c.PlanStatus {
		case string(billing.StatusActive):
			tiles.ActiveSubs += c.Count
			tiles.MRRCents += c.Count * price
			byPlan[c.Plan].mrrCents += c.Count * price
		case string(billing.StatusPastDue):
			tiles.PastDueCount += c.Count
			tiles.MRRPastDueCents += c.Count * price
			tiles.MRRCents += c.Count * price
			byPlan[c.Plan].mrrCents += c.Count * price
		case string(billing.StatusTrialing):
			tiles.TrialingSubs += c.Count
		case string(billing.StatusComped):
			compedCount += c.Count
			compedValueCents += c.Count * price
		}
	}
	tiles.PastDueAtRiskCents = tiles.MRRPastDueCents

	activation, aerr := s.billingRepo.GetActivationCountsThisMonth(ctx)
	if aerr != nil {
		return RevenueResponse{}, aerr
	}
	tiles.NewThisMonth = activation.NewThisMonth
	tiles.CanceledThisMonth = activation.CanceledThisMonth

	planDist := make([]PlanDistributionRow, 0, len(byPlan))
	for _, tier := range []billing.Tier{billing.TierFree, billing.TierStarter, billing.TierAgency, billing.TierScale} {
		agg, ok := byPlan[string(tier)]
		if !ok {
			continue
		}
		planDist = append(planDist, PlanDistributionRow{Plan: string(tier), Count: agg.count, MRRCents: agg.mrrCents})
	}

	pastDueRaw, perr := s.billingRepo.ListPastDue(ctx)
	if perr != nil {
		return RevenueResponse{}, perr
	}
	now := time.Now().UTC()
	pastDue := make([]PastDueRevenueRow, 0, len(pastDueRaw))
	for _, row := range pastDueRaw {
		r := PastDueRevenueRow{
			TenantID:            row.TenantID,
			OrgName:             row.OrgName,
			OrgSlug:             row.OrgSlug,
			AmountCents:         int64(billing.MonthlyPriceCentsForTier(billing.Tier(row.Plan))),
			GraceUntil:          row.GraceUntil,
			LastPaymentFailedAt: row.LastPaymentFailedAt,
		}
		if row.OwnerEmail != nil {
			r.OwnerEmail = *row.OwnerEmail
		}
		if row.GraceUntil != nil {
			enteredPastDueAt := row.GraceUntil.Add(-billing.PastDueGracePeriod())
			days := int(now.Sub(enteredPastDueAt).Hours() / 24)
			if days > 0 {
				r.DaysPastDue = days
			}
		}
		pastDue = append(pastDue, r)
	}

	recentRaw, rrerr := s.billingRepo.ListRecentEvents(ctx)
	if rrerr != nil {
		return RevenueResponse{}, rrerr
	}
	recent := make([]RecentBillingEvent, 0, len(recentRaw))
	for _, row := range recentRaw {
		e := RecentBillingEvent{ID: row.ID, OccurredAt: row.OccurredAt, Kind: row.Kind, Provider: row.Provider, TenantID: row.TenantID}
		if row.OrgName != nil {
			e.OrgName = *row.OrgName
		}
		if row.OrgSlug != nil {
			e.OrgSlug = *row.OrgSlug
		}
		recent = append(recent, e)
	}

	lastWebhook, lwerr := s.billingRepo.GetLastWebhookReceivedAt(ctx)
	if lwerr != nil {
		lastWebhook = nil
	}

	return RevenueResponse{
		Tiles:                 tiles,
		PlanDistribution:      planDist,
		Comped:                CompedRow{Count: compedCount, HypotheticalCents: compedValueCents},
		PastDue:               pastDue,
		RecentEvents:          recent,
		LastWebhookReceivedAt: lastWebhook,
	}, nil
}

// ---------------------------------------------------------------------------
// Mutations
// ---------------------------------------------------------------------------

// recordBillingMutation writes the standing audit+billing_events pair every
// mutation below shares: an audit.Recorder entry under the TARGET tenant's
// own hash chain (ActorType=ActorUser + actorUserID — the superadmin's REAL
// user id) and a billing_events row (source="admin", via
// billing.Service.RecordAdminBillingEvent). Both are best-effort logged
// (never fail the mutation that already succeeded) EXCEPT the audit write,
// which the caller surfaces — an unaudited superadmin billing mutation
// defeats the point of the feature (mirrors audit.Recorder.Rebaseline's own
// "mutate-then-audit" ordering/error-surfacing convention).
func (s *Service) recordBillingMutation(ctx context.Context, tenantID, actorUserID uuid.UUID, action, kind string, meta map[string]any) error {
	if s.billingAudit != nil {
		if _, err := s.billingAudit.Record(ctx, audit.Event{
			TenantID:   tenantID,
			ActorType:  audit.ActorUser,
			ActorID:    actorUserID.String(),
			Action:     action,
			TargetType: "tenant",
			TargetID:   tenantID.String(),
			Metadata:   meta,
		}); err != nil {
			return domain.Internal("admin_billing_audit_failed", "the action was applied but recording the audit entry failed").WithCause(err)
		}
	}
	if s.billingSvc != nil {
		if err := s.billingSvc.RecordAdminBillingEvent(ctx, tenantID, kind, meta); err != nil {
			// Best-effort: the ledger entry is a secondary record of an action
			// already fully applied and audited above.
			_ = err
		}
		s.billingSvc.InvalidateEntitlementsCache(ctx, tenantID)
	}
	return nil
}

// CompAccount grants a manual comp: plan_status=comped, plan=tier,
// comp_reason set. Webhook mutation of a comped tenant is already blocked by
// Phase B's comped-tenant immunity (see billing.Service.ProcessWebhook).
func (s *Service) CompAccount(ctx context.Context, actorUserID, tenantID uuid.UUID, tier billing.Tier, reason string) error {
	if !s.billingPanelReady() {
		return errBillingPanelNotWired()
	}
	if err := requireReason(reason); err != nil {
		return err
	}
	if !billing.ValidTier(tier) {
		return domain.Validation("invalid_tier", "tier must be one of: free, starter, agency, scale")
	}
	before, err := s.billingRepo.GetAccountHeader(ctx, tenantID)
	if err != nil {
		return err
	}
	if err := s.billingRepo.SetComp(ctx, tenantID, string(tier), reason); err != nil {
		return err
	}
	return s.recordBillingMutation(ctx, tenantID, actorUserID, audit.ActionAdminBillingCompGranted, "admin.comp.granted", map[string]any{
		"reason":          reason,
		"plan":            string(tier),
		"old_plan":        before.Plan,
		"old_plan_status": before.PlanStatus,
	})
}

// RevokeComp reverts a comp: adopts the live provider subscription when one
// exists (billing.Service.ReconcileOneNow), else falls back to free/none.
func (s *Service) RevokeComp(ctx context.Context, actorUserID, tenantID uuid.UUID, reason string) error {
	if !s.billingPanelReady() {
		return errBillingPanelNotWired()
	}
	if err := requireReason(reason); err != nil {
		return err
	}
	adopted := false
	if s.billingSvc != nil {
		_, hadSub, rerr := s.billingSvc.ReconcileOneNow(ctx, tenantID)
		if rerr != nil {
			return domain.Internal("admin_billing_revoke_comp_reconcile_failed", "failed to reconcile the live subscription").WithCause(rerr)
		}
		if hadSub {
			adopted = true
			if err := s.billingRepo.ClearCompReason(ctx, tenantID); err != nil {
				return err
			}
		}
	}
	if !adopted {
		if err := s.billingRepo.RevokeCompToFree(ctx, tenantID); err != nil {
			return err
		}
	}
	header, herr := s.billingRepo.GetAccountHeader(ctx, tenantID)
	newPlan, newStatus := "free", "none"
	if herr == nil {
		newPlan, newStatus = header.Plan, header.PlanStatus
	}
	return s.recordBillingMutation(ctx, tenantID, actorUserID, audit.ActionAdminBillingCompRevoked, "admin.comp.revoked", map[string]any{
		"reason":                    reason,
		"adopted_live_subscription": adopted,
		"new_plan":                  newPlan,
		"new_plan_status":           newStatus,
	})
}

// OverrideDeltas is the PUT /admin/accounts/:id/overrides request body: each
// field is a signed delta applied on top of the tenant's CURRENT plan's
// ladder base (NOT accumulated with any prior override for that key — a
// second PUT replaces the delta, matching PUT semantics). nil or 0 clears
// that key entirely (reverting to the pure ladder base for it).
type OverrideDeltas struct {
	SitesDelta     *int
	StorageGBDelta *int
	SeatsDelta     *int
}

// SetAccountOverrides applies deltas on top of the ladder base for the
// tenant's CURRENT plan, persisting the result as tenants.plan_overrides.
// Only the sites/storage_gb/seats keys are ever touched here — any OTHER key
// already present in plan_overrides (none exist today, but the shape allows
// for one later) is preserved untouched.
func (s *Service) SetAccountOverrides(ctx context.Context, actorUserID, tenantID uuid.UUID, deltas OverrideDeltas, reason string) error {
	if !s.billingPanelReady() {
		return errBillingPanelNotWired()
	}
	if err := requireReason(reason); err != nil {
		return err
	}
	header, err := s.billingRepo.GetAccountHeader(ctx, tenantID)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	// The pure ladder base for the tenant's current plan (status "active",
	// overrides nil) — deltas are computed against THIS, never against
	// whatever a prior override already set, so re-submitting the same delta
	// is idempotent.
	base, berr := billing.ResolveEntitlements(header.Plan, string(billing.StatusActive), nil, nil, now)
	if berr != nil {
		return berr
	}

	m := map[string]json.RawMessage{}
	if len(header.PlanOverrides) > 0 {
		_ = json.Unmarshal(header.PlanOverrides, &m)
	}
	clearedKeys := []string{}
	appliedDeltas := map[string]any{}

	setOrClear := func(key string, delta *int, baseVal int64, unit int64) {
		if delta == nil || *delta == 0 {
			if _, ok := m[key]; ok {
				delete(m, key)
				clearedKeys = append(clearedKeys, key)
			}
			return
		}
		newVal := baseVal + int64(*delta)*unit
		raw, _ := json.Marshal(newVal)
		m[key] = raw
		appliedDeltas[key] = *delta
	}
	setOrClear("max_sites", deltas.SitesDelta, int64(base.MaxSites), 1)
	setOrClear("managed_storage_bytes", deltas.StorageGBDelta, base.ManagedStorageBytes, gbBytes)
	setOrClear("seats_soft", deltas.SeatsDelta, int64(base.SeatsSoft), 1)

	resultJSON, merr := json.Marshal(m)
	if merr != nil {
		return domain.Internal("admin_billing_overrides_marshal_failed", "failed to encode overrides").WithCause(merr)
	}
	if err := s.billingRepo.SetOverrides(ctx, tenantID, resultJSON); err != nil {
		return err
	}

	action := audit.ActionAdminBillingOverrideSet
	if len(appliedDeltas) == 0 {
		action = audit.ActionAdminBillingOverrideCleared
	}
	return s.recordBillingMutation(ctx, tenantID, actorUserID, action, "admin.override.set", map[string]any{
		"reason":              reason,
		"deltas":              appliedDeltas,
		"cleared_keys":        clearedKeys,
		"resulting_overrides": json.RawMessage(resultJSON),
	})
}

// maxGraceExtension bounds a single grace extension to 90 days out from now
// (forward-only — see ExtendGrace).
const maxGraceExtension = 90 * 24 * time.Hour

// ExtendGrace sets tenants.grace_until, clamped to at most 90 days out and
// forward-only (never moves grace_until earlier than it already is).
func (s *Service) ExtendGrace(ctx context.Context, actorUserID, tenantID uuid.UUID, until time.Time, reason string) error {
	if !s.billingPanelReady() {
		return errBillingPanelNotWired()
	}
	if err := requireReason(reason); err != nil {
		return err
	}
	now := time.Now().UTC()
	maxUntil := now.Add(maxGraceExtension)
	if until.After(maxUntil) {
		until = maxUntil
	}
	header, err := s.billingRepo.GetAccountHeader(ctx, tenantID)
	if err != nil {
		return err
	}
	if header.GraceUntil != nil && !until.After(*header.GraceUntil) {
		return domain.Validation("grace_not_forward", "the new grace period must extend further out than the current one")
	}
	if err := s.billingRepo.SetGrace(ctx, tenantID, until); err != nil {
		return err
	}
	return s.recordBillingMutation(ctx, tenantID, actorUserID, audit.ActionAdminBillingGraceExtended, "admin.grace.extended", map[string]any{
		"reason":          reason,
		"old_grace_until": header.GraceUntil,
		"new_grace_until": until,
	})
}

// SuspendAccount sets tenants.suspended_at (a SEPARATE field from
// plan_status — see suspension.go). Data is never touched.
func (s *Service) SuspendAccount(ctx context.Context, actorUserID, tenantID uuid.UUID, reason string) error {
	if !s.billingPanelReady() {
		return errBillingPanelNotWired()
	}
	if err := requireReason(reason); err != nil {
		return err
	}
	if err := s.billingRepo.SetSuspended(ctx, tenantID, reason); err != nil {
		return err
	}
	return s.recordBillingMutation(ctx, tenantID, actorUserID, audit.ActionAdminBillingSuspended, "admin.suspended", map[string]any{"reason": reason})
}

// RestoreAccount clears tenants.suspended_at/suspended_reason, returning the
// tenant to whatever its underlying billing state (plan/plan_status) already
// was — a clean, lossless un-suspend.
func (s *Service) RestoreAccount(ctx context.Context, actorUserID, tenantID uuid.UUID, reason string) error {
	if !s.billingPanelReady() {
		return errBillingPanelNotWired()
	}
	if err := requireReason(reason); err != nil {
		return err
	}
	if err := s.billingRepo.ClearSuspended(ctx, tenantID); err != nil {
		return err
	}
	return s.recordBillingMutation(ctx, tenantID, actorUserID, audit.ActionAdminBillingRestored, "admin.restored", map[string]any{"reason": reason})
}

// ForceAccountState is the manual force-state escape hatch for webhook drift:
// sets plan+plan_status directly (clearing grace_until).
func (s *Service) ForceAccountState(ctx context.Context, actorUserID, tenantID uuid.UUID, plan billing.Tier, status billing.Status, reason string) error {
	if !s.billingPanelReady() {
		return errBillingPanelNotWired()
	}
	if err := requireReason(reason); err != nil {
		return err
	}
	if !billing.ValidTier(plan) {
		return domain.Validation("invalid_tier", "plan must be one of: free, starter, agency, scale")
	}
	if !validPlanStatus(status) {
		return domain.Validation("invalid_plan_status", "plan_status must be one of: none, trialing, active, past_due, canceled, paused, comped")
	}
	before, err := s.billingRepo.GetAccountHeader(ctx, tenantID)
	if err != nil {
		return err
	}
	if err := s.billingRepo.ForceState(ctx, tenantID, string(plan), string(status)); err != nil {
		return err
	}
	return s.recordBillingMutation(ctx, tenantID, actorUserID, audit.ActionAdminBillingStateForced, "admin.state.forced", map[string]any{
		"reason":          reason,
		"old_plan":        before.Plan,
		"old_plan_status": before.PlanStatus,
		"new_plan":        string(plan),
		"new_plan_status": string(status),
	})
}

func validPlanStatus(st billing.Status) bool {
	switch st {
	case billing.StatusNone, billing.StatusTrialing, billing.StatusActive,
		billing.StatusPastDue, billing.StatusCanceled, billing.StatusPaused, billing.StatusComped:
		return true
	}
	return false
}
