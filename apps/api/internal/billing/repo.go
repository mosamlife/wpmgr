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
	out.CancelAtPeriodEnd = row.CancelAtPeriodEnd
	if row.CancelAt.Valid {
		t := row.CancelAt.Time
		out.CancelAt = &t
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

// applySubscriptionStateTx persists the state machine's resolved next
// tenantBillingProfile inside the caller's locked transaction.
func applySubscriptionStateTx(ctx context.Context, q *sqlc.Queries, tenantID uuid.UUID, next tenantBillingProfile) error {
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
	if err := q.ApplyBillingSubscriptionState(ctx, params); err != nil {
		return domain.Internal("billing_apply_state_failed", "failed to persist billing subscription state").WithCause(err)
	}
	return nil
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

// nonEmptyPtr returns nil for an empty string, else a pointer to s — used for
// nullable *string sqlc params (provider_subscription_id).
func nonEmptyPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
