package billing

// repo.go — M16 Phase B data access: the tenant billing profile (provider
// identity + external ids, layered on top of Phase A's tenants columns) and
// the billing_events ledger. See db/query/billing.sql for the query
// definitions and their RLS/transaction-context rationale.

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// toBillingProfile maps the sqlc row to the DB-free tenantBillingProfile
// shape state_machine.go operates on.
func toBillingProfile(row sqlc.GetTenantBillingProfileRow) tenantBillingProfile {
	out := tenantBillingProfile{
		Plan:   Tier(row.Plan),
		Status: Status(row.PlanStatus),
	}
	if row.GraceUntil.Valid {
		t := row.GraceUntil.Time
		out.GraceUntil = &t
	}
	if row.CurrentPeriodEnd.Valid {
		t := row.CurrentPeriodEnd.Time
		out.CurrentPeriodEnd = &t
	}
	if row.BillingProvider != nil {
		out.BillingProvider = *row.BillingProvider
	}
	if row.ProviderCustomerID != nil {
		out.ProviderCustomerID = *row.ProviderCustomerID
	}
	if row.ProviderSubscriptionID != nil {
		out.ProviderSubscriptionID = *row.ProviderSubscriptionID
	}
	return out
}

// getBillingProfile loads a tenant's Phase-B billing profile. tenants carries
// no RLS (see schema.sql), so this reads through the plain pool — mirroring
// internal/tenant's pgRepo, which does the same for its own tenant-row reads.
func (s *Service) getBillingProfile(ctx context.Context, tenantID uuid.UUID) (tenantBillingProfile, error) {
	row, err := sqlc.New(s.pool.Pool).GetTenantBillingProfile(ctx, tenantID)
	if err != nil {
		return tenantBillingProfile{}, domain.Internal("billing_profile_lookup_failed", "failed to load tenant billing profile").WithCause(err)
	}
	return toBillingProfile(row), nil
}

// setBillingProviderIfUnset writes tenants.billing_provider the FIRST time a
// tenant starts a checkout ("one tenant = one provider at a time"). A no-op
// when already set (see SetTenantBillingProviderIfUnset's WHERE guard).
func (s *Service) setBillingProviderIfUnset(ctx context.Context, tenantID uuid.UUID, providerName string) error {
	_, err := sqlc.New(s.pool.Pool).SetTenantBillingProviderIfUnset(ctx, sqlc.SetTenantBillingProviderIfUnsetParams{
		TenantID:        tenantID,
		BillingProvider: &providerName,
	})
	if err != nil {
		return domain.Internal("billing_set_provider_failed", "failed to record billing provider").WithCause(err)
	}
	return nil
}

// applySubscriptionState persists the state machine's resolved next
// tenantBillingProfile.
func (s *Service) applySubscriptionState(ctx context.Context, tenantID uuid.UUID, next tenantBillingProfile) error {
	params := sqlc.ApplyBillingSubscriptionStateParams{
		Plan:                   string(next.Plan),
		PlanStatus:             string(next.Status),
		ProviderSubscriptionID: nonEmptyPtr(next.ProviderSubscriptionID),
		ProviderCustomerID:     next.ProviderCustomerID,
		TenantID:               tenantID,
	}
	if next.GraceUntil != nil {
		params.GraceUntil = pgtype.Timestamptz{Time: *next.GraceUntil, Valid: true}
	}
	if next.CurrentPeriodEnd != nil {
		params.CurrentPeriodEnd = pgtype.Timestamptz{Time: *next.CurrentPeriodEnd, Valid: true}
	}
	if err := sqlc.New(s.pool.Pool).ApplyBillingSubscriptionState(ctx, params); err != nil {
		return domain.Internal("billing_apply_state_failed", "failed to persist billing subscription state").WithCause(err)
	}
	return nil
}

// findTenantByProviderCustomer resolves a tenant from (provider,
// provider_customer_id) — the fallback attribution path for a webhook event
// whose payload carries no tenant metadata. ok=false (not an error) when no
// tenant matches.
func (s *Service) findTenantByProviderCustomer(ctx context.Context, providerName, providerCustomerID string) (uuid.UUID, bool, error) {
	id, err := sqlc.New(s.pool.Pool).FindTenantByProviderCustomer(ctx, sqlc.FindTenantByProviderCustomerParams{
		BillingProvider:    &providerName,
		ProviderCustomerID: &providerCustomerID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, false, nil
		}
		return uuid.Nil, false, domain.Internal("billing_customer_lookup_failed", "failed to resolve tenant by billing customer").WithCause(err)
	}
	return id, true, nil
}

// billingEventInsert is the input to insertBillingEvent.
type billingEventInsert struct {
	Provider        string
	ProviderEventID string
	// Kind is the provider-native event-type string (billing_events.kind),
	// e.g. "invoice.payment_failed" — see Event.ProviderEventType.
	Kind       string
	TenantID   uuid.UUID // uuid.Nil when attribution is not yet known
	Payload    map[string]any
	OccurredAt time.Time
}

// insertBillingEvent appends the webhook to the billing_events ledger.
// inserted=false means the (provider, provider_event_id) pair was already
// present — a duplicate delivery, which the caller treats as an idempotent
// no-op (still returns 200 to the provider). Runs under InAgentTx: this is a
// cross-tenant system write (see the m91/billing_events_system rationale).
func (s *Service) insertBillingEvent(ctx context.Context, in billingEventInsert) (id uuid.UUID, inserted bool, err error) {
	payload := in.Payload
	if payload == nil {
		payload = map[string]any{}
	}
	payloadJSON, merr := json.Marshal(payload)
	if merr != nil {
		return uuid.Nil, false, domain.Internal("billing_event_marshal_failed", "failed to encode billing event payload").WithCause(merr)
	}

	var tenantPG pgtype.UUID
	if in.TenantID != uuid.Nil {
		tenantPG = pgtype.UUID{Bytes: in.TenantID, Valid: true}
	}

	txErr := s.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		newID, qerr := sqlc.New(tx).InsertBillingEvent(ctx, sqlc.InsertBillingEventParams{
			Provider:        in.Provider,
			ProviderEventID: in.ProviderEventID,
			Kind:            in.Kind,
			TenantID:        tenantPG,
			Payload:         payloadJSON,
			OccurredAt:      in.OccurredAt,
		})
		if qerr != nil {
			if errors.Is(qerr, pgx.ErrNoRows) {
				// ON CONFLICT DO NOTHING -> duplicate delivery.
				inserted = false
				return nil
			}
			return qerr
		}
		id = newID
		inserted = true
		return nil
	})
	if txErr != nil {
		return uuid.Nil, false, domain.Internal("billing_event_insert_failed", "failed to record billing event").WithCause(txErr)
	}
	return id, inserted, nil
}

// setBillingEventTenant best-effort backfills tenant_id on a billing_events
// row once attribution is resolved after the initial insert.
func (s *Service) setBillingEventTenant(ctx context.Context, eventID, tenantID uuid.UUID) error {
	return s.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		return sqlc.New(tx).SetBillingEventTenant(ctx, sqlc.SetBillingEventTenantParams{
			TenantID: pgtype.UUID{Bytes: tenantID, Valid: true},
			ID:       eventID,
		})
	})
}

// markBillingEventProcessed stamps processed_at once a billing_events row has
// been fully handled (applied, or deliberately no-op'd — comped tenant,
// unknown price, out-of-order).
func (s *Service) markBillingEventProcessed(ctx context.Context, eventID uuid.UUID) error {
	return s.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		return sqlc.New(tx).MarkBillingEventProcessed(ctx, eventID)
	})
}

// isOutOfOrder reports whether occurredAt is OLDER than the newest
// occurred_at among this tenant's already-processed billing_events rows
// (excluding excludeEventID, the row currently being processed). See
// LastProcessedBillingEventOccurredAtForTenant's doc comment for why this is
// NOT a bare MAX() aggregate query.
func (s *Service) isOutOfOrder(ctx context.Context, tenantID, excludeEventID uuid.UUID, occurredAt time.Time) (bool, error) {
	var last time.Time
	var found bool
	err := s.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		row, qerr := sqlc.New(tx).LastProcessedBillingEventOccurredAtForTenant(ctx, sqlc.LastProcessedBillingEventOccurredAtForTenantParams{
			TenantID:  pgtype.UUID{Bytes: tenantID, Valid: true},
			ExcludeID: excludeEventID,
		})
		if qerr != nil {
			if errors.Is(qerr, pgx.ErrNoRows) {
				return nil
			}
			return qerr
		}
		last = row
		found = true
		return nil
	})
	if err != nil {
		return false, domain.Internal("billing_order_check_failed", "failed to check billing event ordering").WithCause(err)
	}
	if !found {
		return false, nil
	}
	return occurredAt.Before(last), nil
}

// nonEmptyPtr returns nil for an empty string, else a pointer to s — used for
// nullable *string sqlc params (provider_subscription_id).
func nonEmptyPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
