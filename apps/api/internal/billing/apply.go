package billing

// apply.go — the one locked function that changes a tenant's billing state
// from a payment provider. It runs only inside the billing queue's workers
// (billing_apply for a recorded webhook event, billing_refresh for a
// reconcile, a confirm or an operator action). Request paths and webhook
// intake never call a provider or change billing state themselves: they
// enqueue, and this code applies.
//
// Contract, in order:
//
//  1. The tenant is resolved from the recorded event (claim plus stored
//     customer). An owned event that resolves to no tenant is a mismatch: it
//     is alerted and marked processed.
//  2. Under the per-tenant billing lock, the tenant row is read.
//  3. A comped tenant is recorded only.
//  4. The subscription is fetched from the provider (15 s deadline).
//  5. The pinned provider, the stored customer, the price and the stored
//     subscription must all agree with the fetched subscription, whatever its
//     status. Any disagreement is alerted, recorded and changes nothing.
//  6. The state machine's result is written, the event is marked processed,
//     and an audit job is enqueued in the same transaction.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

const (
	// providerRazorpay is the one provider whose events are attributed by the
	// server-set notes they carry rather than by the stored customer.
	providerRazorpay = "razorpay"
	// providerStripe names the Stripe adapter in the ledger and the registry.
	providerStripe = "stripe"

	// providerCallTimeout bounds one provider call made by a billing worker.
	providerCallTimeout = 15 * time.Second
)

// Refresh sources recorded on billing_refresh jobs and in audit metadata.
const (
	RefreshSourceReconcile  = "reconcile"
	RefreshSourceRevokeComp = "revoke_comp"
	RefreshSourceConfirm    = "confirm"
	RefreshSourceCheckout   = "checkout"
	refreshSourceWebhook    = "webhook"
)

// Alert names. Each is logged at error level (warn for alertTaxIDUnverified)
// with the attribute alert=<name>, which is what log-based alerting keys on.
const (
	alertEventMissing       = "billing_event_missing"
	alertTenantMismatch     = "billing_tenant_mismatch"
	alertTenantGone         = "billing_tenant_gone"
	alertProviderMismatch   = "billing_provider_mismatch"
	alertCustomerMismatch   = "billing_customer_mismatch"
	alertUnknownPrice       = "billing_unknown_price"
	alertDoubleSubscription = "billing_double_subscription"
	alertDeletedOrgPayment  = "billing_deleted_org_payment"
	alertDeletedOrgLive     = "billing_deleted_org_live_subscription"
	alertProviderMissing    = "billing_provider_unregistered"
	alertTaxIDUnverified    = "billing_tax_id_unverified"
	alertSessionExpiry      = "billing_session_expiry_failed"
)

// alert logs an operator alert. level is slog.LevelError unless stated.
func (s *Service) alert(ctx context.Context, level slog.Level, name string, attrs ...slog.Attr) {
	all := append([]slog.Attr{slog.String("alert", name)}, attrs...)
	s.logger.LogAttrs(ctx, level, "billing alert: "+name, all...)
}

// LockTenantBilling takes the per-tenant billing advisory lock inside tx. It
// is held until tx ends. Every write to a tenant's billing columns (the apply
// worker, the operator's billing actions, checkout binding, the cancel
// markers) and the org-delete billing check take it first, so they serialize
// per tenant. Callers make no provider call while holding it, except the
// billing workers (bounded by providerCallTimeout).
//
// The key is defined once, in the LockTenantBilling query; no Go code spells
// it, so no caller can drift onto a different key.
func LockTenantBilling(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) error {
	if err := sqlc.New(tx).LockTenantBilling(ctx, tenantID); err != nil {
		return domain.Internal("billing_lock_failed", "failed to acquire the tenant billing lock").WithCause(err)
	}
	return nil
}

// eventPayload is the part of a billing_events payload the apply worker reads.
type eventPayload struct {
	NormalizedKind         string `json:"normalized_kind"`
	ProviderCustomerID     string `json:"provider_customer_id"`
	ProviderSubscriptionID string `json:"provider_subscription_id"`
	ClaimedTenantID        string `json:"claimed_tenant_id"`
	TaxIDType              string `json:"tax_id_type"`
	VerificationStatus     string `json:"verification_status"`
}

func (p eventPayload) claim() uuid.UUID {
	if p.ClaimedTenantID == "" {
		return uuid.Nil
	}
	id, err := uuid.Parse(p.ClaimedTenantID)
	if err != nil {
		return uuid.Nil
	}
	return id
}

// applyInput is one locked apply.
type applyInput struct {
	tenantID uuid.UUID
	// providerName is the event's provider; empty for a refresh, which uses
	// the tenant's pinned provider.
	providerName string
	// eventRowID is the billing_events row to mark processed; uuid.Nil for a
	// refresh.
	eventRowID uuid.UUID
	// subscriptionID is the subscription to fetch; empty means the stored one
	// (refresh only; an event with none is recorded only).
	subscriptionID string
	kind           EventKind
	eventType      string
	source         string
	auditKey       string
}

// applyEvent is billing_apply's work: load the recorded event, resolve its
// tenant, and run the locked apply. A nil return completes the job.
func (s *Service) applyEvent(ctx context.Context, args BillingApplyArgs) error {
	var row sqlc.BillingEvent
	err := s.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		var qerr error
		row, qerr = sqlc.New(tx).GetBillingEventByProviderEventID(ctx, sqlc.GetBillingEventByProviderEventIDParams{
			Provider: args.Provider, ProviderEventID: args.ProviderEventID,
		})
		return qerr
	})
	if errors.Is(err, pgx.ErrNoRows) {
		s.alert(ctx, slog.LevelError, alertEventMissing,
			slog.String("provider", args.Provider), slog.String("provider_event_id", args.ProviderEventID))
		return nil
	}
	if err != nil {
		return fmt.Errorf("billing apply: load event: %w", err)
	}
	if row.ProcessedAt.Valid {
		return nil
	}

	var pl eventPayload
	if len(row.Payload) > 0 {
		if err := json.Unmarshal(row.Payload, &pl); err != nil {
			return fmt.Errorf("billing apply: decode payload: %w", err)
		}
	}

	if _, ok := s.registry.Provider(args.Provider); !ok {
		s.alert(ctx, slog.LevelError, alertProviderMissing,
			slog.String("provider", args.Provider), slog.String("provider_event_id", args.ProviderEventID))
		return fmt.Errorf("billing apply: provider %q is not registered", args.Provider)
	}

	tenantID, reason, err := s.resolveEventTenant(ctx, args.Provider, pl.claim(), pl.ProviderCustomerID)
	if err != nil {
		return err
	}
	if tenantID == uuid.Nil {
		s.alert(ctx, slog.LevelError, alertTenantMismatch,
			slog.String("provider", args.Provider), slog.String("provider_event_id", args.ProviderEventID),
			slog.String("reason", reason), slog.String("claimed_tenant_id", pl.ClaimedTenantID))
		return s.markProcessed(ctx, row.ID)
	}

	kind := EventKind(pl.NormalizedKind)
	if kind == EventTaxIDUpdated {
		return s.applyTaxIDUpdated(ctx, row.ID, tenantID, args, pl)
	}

	return s.applyLocked(ctx, applyInput{
		tenantID:       tenantID,
		providerName:   args.Provider,
		eventRowID:     row.ID,
		subscriptionID: pl.ProviderSubscriptionID,
		kind:           kind,
		eventType:      row.Kind,
		source:         refreshSourceWebhook,
		auditKey:       "billing_event:" + args.Provider + ":" + args.ProviderEventID,
	})
}

// applyTaxIDUpdated records a tax-ID change: no billing state changes, an
// unverified ID raises a warning, and the event is marked processed.
func (s *Service) applyTaxIDUpdated(ctx context.Context, eventRowID, tenantID uuid.UUID, args BillingApplyArgs, pl eventPayload) error {
	if pl.VerificationStatus == "unverified" {
		s.alert(ctx, slog.LevelWarn, alertTaxIDUnverified,
			slog.String("tenant_id", tenantID.String()), slog.String("provider", args.Provider),
			slog.String("provider_event_id", args.ProviderEventID), slog.String("tax_id_type", pl.TaxIDType))
	}
	return s.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		if err := q.SetBillingEventTenant(ctx, sqlc.SetBillingEventTenantParams{
			TenantID: pgtype.UUID{Bytes: tenantID, Valid: true}, ID: eventRowID,
		}); err != nil {
			return err
		}
		return q.MarkBillingEventProcessed(ctx, eventRowID)
	})
}

// resolveEventTenant decides which tenant an owned event concerns. It
// returns uuid.Nil and a reason when the event is a mismatch.
//
//   - Razorpay: the server-set claim decides, and that tenant must exist.
//     Without a claim, exactly one tenant must store the customer.
//   - Every other provider: the tenants storing (provider, customer) are the
//     candidates. A claim must be one of them; without a claim there must be
//     exactly one. No candidate is a mismatch.
func (s *Service) resolveEventTenant(ctx context.Context, providerName string, claim uuid.UUID, customerID string) (uuid.UUID, string, error) {
	candidates, err := s.tenantsByCustomer(ctx, providerName, customerID)
	if err != nil {
		return uuid.Nil, "", err
	}
	if providerName == providerRazorpay {
		if claim != uuid.Nil {
			exists, err := s.tenantExists(ctx, claim)
			if err != nil {
				return uuid.Nil, "", err
			}
			if !exists {
				return uuid.Nil, "claimed_tenant_not_found", nil
			}
			return claim, "", nil
		}
		if len(candidates) == 1 {
			return candidates[0], "", nil
		}
		return uuid.Nil, "customer_not_unique", nil
	}

	if len(candidates) == 0 {
		return uuid.Nil, "no_tenant_stores_customer", nil
	}
	if claim != uuid.Nil {
		for _, c := range candidates {
			if c == claim {
				return claim, "", nil
			}
		}
		return uuid.Nil, "claim_not_matched_by_customer", nil
	}
	if len(candidates) == 1 {
		return candidates[0], "", nil
	}
	return uuid.Nil, "customer_not_unique", nil
}

// tenantsByCustomer lists every tenant storing (provider, customer), so the
// caller can tell none, exactly one and several apart. An empty customer
// matches nothing. tenants carries no RLS, so this reads the pool.
func (s *Service) tenantsByCustomer(ctx context.Context, providerName, customerID string) ([]uuid.UUID, error) {
	if customerID == "" {
		return nil, nil
	}
	ids, err := sqlc.New(s.pool.Pool).FindTenantsByProviderCustomer(ctx, sqlc.FindTenantsByProviderCustomerParams{
		BillingProvider:    providerName,
		ProviderCustomerID: customerID,
	})
	if err != nil {
		return nil, domain.Internal("billing_customer_lookup_failed", "failed to resolve tenant by billing customer").WithCause(err)
	}
	return ids, nil
}

// tenantExists reports whether a tenants row with id exists.
func (s *Service) tenantExists(ctx context.Context, id uuid.UUID) (bool, error) {
	_, err := sqlc.New(s.pool.Pool).GetTenantBillingProfile(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, domain.Internal("billing_profile_lookup_failed", "failed to load tenant billing profile").WithCause(err)
	}
	return true, nil
}

// markProcessed stamps processed_at on a billing_events row in its own
// transaction.
func (s *Service) markProcessed(ctx context.Context, eventRowID uuid.UUID) error {
	return s.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		return sqlc.New(tx).MarkBillingEventProcessed(ctx, eventRowID)
	})
}

// isLiveStatus reports whether a status is one where the stored subscription
// is still the tenant's current one, so a different subscription arriving is
// a second subscription rather than a replacement.
func isLiveStatus(st Status) bool {
	return st != StatusCanceled
}

func isActiveStatus(st Status) bool {
	return st == StatusActive || st == StatusTrialing
}

// ownershipMismatch returns the alert name for a fetched subscription that
// does not belong to this tenant's binding, or "" when it does. It is checked
// for every status.
func ownershipMismatch(providerName string, profile tenantBillingProfile, sub Subscription) string {
	if profile.BillingProvider != providerName {
		return alertProviderMismatch
	}
	if providerName == providerRazorpay {
		if profile.ProviderCustomerID != "" && sub.CustomerID != profile.ProviderCustomerID {
			return alertCustomerMismatch
		}
	} else if profile.ProviderCustomerID == "" || sub.CustomerID != profile.ProviderCustomerID {
		return alertCustomerMismatch
	}
	if !sub.PlanResolved {
		return alertUnknownPrice
	}
	if profile.ProviderSubscriptionID != "" && sub.ID != profile.ProviderSubscriptionID && isLiveStatus(profile.Status) {
		return alertDoubleSubscription
	}
	return ""
}

// postApply is what applyLocked does after its transaction commits.
type postApply struct {
	invalidate     bool
	expireSessions bool
	customerID     string
	provider       Provider
}

// applyLocked is the one locked apply. See the file comment for its
// contract. A returned error rolls the transaction back and retries the job.
func (s *Service) applyLocked(ctx context.Context, in applyInput) error {
	var post postApply
	err := s.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		if err := LockTenantBilling(ctx, tx, in.tenantID); err != nil {
			return err
		}
		q := sqlc.New(tx)
		markDone := func() error {
			if in.eventRowID == uuid.Nil {
				return nil
			}
			return q.MarkBillingEventProcessed(ctx, in.eventRowID)
		}

		prow, err := q.GetTenantBillingProfile(ctx, in.tenantID)
		if errors.Is(err, pgx.ErrNoRows) {
			s.alert(ctx, slog.LevelError, alertTenantGone,
				slog.String("tenant_id", in.tenantID.String()), slog.String("source", in.source))
			return markDone()
		}
		if err != nil {
			return domain.Internal("billing_profile_lookup_failed", "failed to load tenant billing profile").WithCause(err)
		}
		profile := toBillingProfile(prow)

		if in.eventRowID != uuid.Nil {
			if err := q.SetBillingEventTenant(ctx, sqlc.SetBillingEventTenantParams{
				TenantID: pgtype.UUID{Bytes: in.tenantID, Valid: true}, ID: in.eventRowID,
			}); err != nil {
				return err
			}
		}

		if in.kind == EventPaymentSucceeded && prow.DeletedAt.Valid {
			s.alert(ctx, slog.LevelError, alertDeletedOrgPayment,
				slog.String("tenant_id", in.tenantID.String()), slog.String("event_type", in.eventType))
		}

		if profile.Status == StatusComped {
			return markDone()
		}

		providerName := in.providerName
		if providerName == "" {
			providerName = profile.BillingProvider
		}
		if providerName == "" {
			return markDone()
		}
		provider, ok := s.registry.Provider(providerName)
		if !ok {
			s.alert(ctx, slog.LevelError, alertProviderMissing,
				slog.String("tenant_id", in.tenantID.String()), slog.String("provider", providerName))
			return markDone()
		}

		subID := in.subscriptionID
		if subID == "" && in.eventRowID == uuid.Nil {
			subID = profile.ProviderSubscriptionID
		}
		if subID == "" {
			return markDone()
		}

		fetchCtx, cancel := context.WithTimeout(ctx, providerCallTimeout)
		sub, err := provider.GetSubscription(fetchCtx, subID)
		cancel()
		if err != nil {
			return domain.Internal("billing_subscription_fetch_failed", "failed to fetch current subscription state from the payment provider").WithCause(err)
		}

		if reason := ownershipMismatch(providerName, profile, sub); reason != "" {
			s.alert(ctx, slog.LevelError, reason,
				slog.String("tenant_id", in.tenantID.String()), slog.String("provider", providerName),
				slog.String("provider_subscription_id", sub.ID), slog.String("status", string(sub.Status)),
				slog.String("source", in.source))
			return markDone()
		}

		next := nextBillingState(profile, sub, s.clock.Now())
		if prow.DeletedAt.Valid && orgDeleteBlock(next, s.clock.Now()) != nil {
			s.alert(ctx, slog.LevelError, alertDeletedOrgLive,
				slog.String("tenant_id", in.tenantID.String()), slog.String("status", string(next.Status)))
		}

		if err := applySubscriptionStateTx(ctx, q, in.tenantID, providerName, next); err != nil {
			return err
		}
		if err := markDone(); err != nil {
			return err
		}

		if profile.Plan != next.Plan || profile.Status != next.Status {
			if s.river == nil {
				return errQueueNotWired
			}
			if _, err := s.river.InsertTx(ctx, tx, BillingAuditArgs{
				TenantID: in.tenantID,
				ActorID:  providerName,
				Action:   "billing.subscription.changed",
				AuditKey: in.auditKey,
				Metadata: map[string]any{
					"old_plan":   string(profile.Plan),
					"new_plan":   string(next.Plan),
					"old_status": string(profile.Status),
					"new_status": string(next.Status),
					"source":     in.source,
					"event_type": in.eventType,
				},
			}, nil); err != nil {
				return fmt.Errorf("enqueue billing_audit: %w", err)
			}
		}

		post.invalidate = true
		activated := isActiveStatus(next.Status) &&
			(!isActiveStatus(profile.Status) || in.kind == EventActivated || in.kind == EventTrialStarted)
		if activated && (in.source == refreshSourceWebhook || in.source == RefreshSourceConfirm) {
			post.expireSessions = true
			post.customerID = next.ProviderCustomerID
			post.provider = provider
		}
		return nil
	})
	if err != nil {
		return err
	}
	if post.invalidate {
		s.invalidateCache(ctx, in.tenantID)
	}
	if post.expireSessions {
		s.expireOpenSessions(ctx, in.tenantID, post.provider, post.customerID)
	}
	return nil
}

// expireOpenSessions expires the customer's remaining open checkout sessions
// after an activation has committed, so no second payable checkout stays
// open. It runs outside any transaction, under providerCallTimeout. A failure
// is alerted and does not fail the job: the activation already committed.
func (s *Service) expireOpenSessions(ctx context.Context, tenantID uuid.UUID, provider Provider, customerID string) {
	exp, ok := provider.(CheckoutSessionExpirer)
	if !ok || customerID == "" {
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, providerCallTimeout)
	defer cancel()
	n, err := exp.ExpireOpenCheckoutSessions(callCtx, customerID)
	if err != nil {
		s.alert(ctx, slog.LevelWarn, alertSessionExpiry,
			slog.String("tenant_id", tenantID.String()), slog.Any("error", err))
		return
	}
	if n > 0 {
		s.logger.Info("billing: expired open checkout sessions after activation",
			slog.String("tenant_id", tenantID.String()), slog.Int("expired", n))
	}
}

// applyRefresh is billing_refresh's work.
func (s *Service) applyRefresh(ctx context.Context, jobID int64, args BillingRefreshArgs) error {
	return s.applyLocked(ctx, applyInput{
		tenantID:       args.TenantID,
		subscriptionID: args.SubscriptionID,
		source:         args.Source,
		auditKey:       fmt.Sprintf("billing_refresh:%d", jobID),
	})
}

// errQueueNotWired is returned when a billing path that must enqueue runs
// without a River client. It fails loudly rather than dropping the work.
var errQueueNotWired = domain.Internal("billing_queue_unavailable", "the billing job queue is not wired")

// RevokeCompResult reports what RevokeComp did.
type RevokeCompResult struct {
	// RefreshEnqueued is true when a subscription id was stored, so a
	// billing_refresh was enqueued to adopt the live subscription's state.
	RefreshEnqueued bool
}

// RevokeComp lifts an operator comp. Under the per-tenant billing lock it
// re-checks that the tenant is still comped, sets it to free/none, and, when
// a subscription id is stored, enqueues billing_refresh{source: revoke_comp}
// in the same transaction so the refresh worker adopts the provider's state.
// It makes no provider call. The caller records the operator audit entry.
func (s *Service) RevokeComp(ctx context.Context, tenantID uuid.UUID) (RevokeCompResult, error) {
	var out RevokeCompResult
	err := pgx.BeginFunc(ctx, s.pool.Pool, func(tx pgx.Tx) error {
		if err := LockTenantBilling(ctx, tx, tenantID); err != nil {
			return err
		}
		q := sqlc.New(tx)
		prow, err := q.GetTenantBillingProfile(ctx, tenantID)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound("tenant_not_found", "no such account")
		}
		if err != nil {
			return domain.Internal("billing_profile_lookup_failed", "failed to load tenant billing profile").WithCause(err)
		}
		profile := toBillingProfile(prow)
		if profile.Status != StatusComped {
			return domain.Conflict("billing_not_comped", "this account is not comped")
		}
		if err := q.AdminRevokeCompToFree(ctx, tenantID); err != nil {
			return domain.Internal("admin_billing_revoke_comp_failed", "failed to revoke comp").WithCause(err)
		}
		if profile.ProviderSubscriptionID == "" {
			return nil
		}
		if s.river == nil {
			return errQueueNotWired
		}
		if _, err := s.river.InsertTx(ctx, tx, BillingRefreshArgs{
			TenantID:       tenantID,
			SubscriptionID: profile.ProviderSubscriptionID,
			Source:         RefreshSourceRevokeComp,
		}, nil); err != nil {
			return fmt.Errorf("enqueue billing_refresh: %w", err)
		}
		out.RefreshEnqueued = true
		return nil
	})
	if err != nil {
		if _, ok := domain.AsDomain(err); ok {
			return RevokeCompResult{}, err
		}
		return RevokeCompResult{}, domain.Internal("admin_billing_revoke_comp_failed", "failed to revoke comp").WithCause(err)
	}
	s.invalidateCache(ctx, tenantID)
	return out, nil
}
