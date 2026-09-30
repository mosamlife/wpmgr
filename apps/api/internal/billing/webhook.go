package billing

// webhook.go — payment-provider webhook intake. Intake acknowledges fast: it
// verifies, classifies ownership, records the event and enqueues its
// billing_apply job in one transaction, and returns. It never calls the
// provider and never changes a tenant's billing state; the apply worker
// (apply.go) does that under the per-tenant lock ("push is a hint, pull is
// the truth").

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// stripeCheckoutCompleted is the one event type whose ledger payload carries
// the session's tax-ID types and billing country.
const stripeCheckoutCompleted = "checkout.session.completed"

// ProcessWebhook is webhook intake:
//
//  1. Resolve the named provider (404 when unknown) and verify the request
//     (401 on any verification failure; nothing before this is trusted).
//  2. Classify ownership. A foreign event returns nil (HTTP 200) with no
//     ledger row, no job and no alert. A by-customer event is owned only
//     when a tenant stores its customer for this provider.
//  3. In one transaction: record the event, and either enqueue billing_apply
//     (a handled type) or mark it processed (an unhandled type). A claim
//     naming a tenant that no longer exists is recorded with a NULL tenant.
//  4. A duplicate delivery whose event is not yet processed re-enqueues its
//     job; one already processed is a no-op.
//
// Any database or queue error returns 500 so the provider retries.
func (s *Service) ProcessWebhook(ctx context.Context, providerName string, rawBody []byte, headers http.Header) error {
	if !s.enabled {
		return domain.Unavailable("billing_disabled", "hosted billing is not enabled on this instance")
	}
	if s.registry == nil {
		return domain.NotFound("billing_provider_unknown", "no payment provider is configured")
	}
	provider, ok := s.registry.Provider(providerName)
	if !ok {
		return domain.NotFound("billing_provider_unknown", "unrecognized payment provider")
	}

	ev, err := provider.VerifyWebhook(rawBody, headers)
	if err != nil {
		return domain.Unauthorized("billing_webhook_signature_invalid", "webhook signature verification failed").WithCause(err)
	}

	switch ev.Ownership {
	case OwnershipOwned:
	case OwnershipByCustomer:
		candidates, err := s.tenantsByCustomer(ctx, providerName, ev.ProviderCustomerID)
		if err != nil {
			return err
		}
		if len(candidates) == 0 {
			s.logger.Debug("billing: webhook event for a customer no tenant stores; not recorded",
				slog.String("provider", providerName), slog.String("event_type", ev.ProviderEventType))
			return nil
		}
	default:
		s.logger.Debug("billing: webhook event belongs to another product; not recorded",
			slog.String("provider", providerName), slog.String("event_type", ev.ProviderEventType))
		return nil
	}

	if s.river == nil {
		return errQueueNotWired
	}
	return s.recordAndEnqueue(ctx, providerName, ev)
}

// intakePayload builds the ledger payload for an owned event.
func intakePayload(providerName string, ev Event) map[string]any {
	claim := ""
	if ev.TenantID != uuid.Nil {
		claim = ev.TenantID.String()
	}
	payload := map[string]any{
		"normalized_kind":          string(ev.Kind),
		"provider_customer_id":     ev.ProviderCustomerID,
		"provider_subscription_id": ev.ProviderSubscriptionID,
		"claimed_tenant_id":        claim,
	}
	if providerName == providerStripe && ev.ProviderEventType == stripeCheckoutCompleted {
		types := ev.TaxIDTypes
		if types == nil {
			types = []string{}
		}
		payload["session_tax_id_types"] = types
		payload["billing_country"] = ev.BillingCountry
	}
	if ev.Kind == EventTaxIDUpdated {
		payload["tax_id_type"] = ev.TaxIDType
		payload["verification_status"] = ev.TaxIDVerificationStatus
	}
	return payload
}

// recordAndEnqueue is intake's one transaction (step 3 and 4). A foreign-key
// violation from a tenant hard-deleted between the claim check and the
// insert is retried once with no claim.
func (s *Service) recordAndEnqueue(ctx context.Context, providerName string, ev Event) error {
	payloadJSON, err := json.Marshal(intakePayload(providerName, ev))
	if err != nil {
		return domain.Internal("billing_event_marshal_failed", "failed to encode billing event payload").WithCause(err)
	}

	claim := ev.TenantID
	for attempt := 0; ; attempt++ {
		err := s.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
			return s.recordAndEnqueueTx(ctx, tx, providerName, ev, claim, payloadJSON)
		})
		if err == nil {
			return nil
		}
		if attempt == 0 && claim != uuid.Nil && isForeignKeyViolation(err) {
			claim = uuid.Nil
			continue
		}
		return domain.Internal("billing_event_insert_failed", "failed to record billing event").WithCause(err)
	}
}

func (s *Service) recordAndEnqueueTx(ctx context.Context, tx pgx.Tx, providerName string, ev Event, claim uuid.UUID, payloadJSON []byte) error {
	q := sqlc.New(tx)
	var tenantPG pgtype.UUID
	if claim != uuid.Nil {
		tenantPG = pgtype.UUID{Bytes: claim, Valid: true}
	}
	id, err := q.InsertBillingEvent(ctx, sqlc.InsertBillingEventParams{
		Provider:        providerName,
		ProviderEventID: ev.ProviderEventID,
		Kind:            ev.ProviderEventType,
		TenantID:        tenantPG,
		Payload:         payloadJSON,
		OccurredAt:      ev.OccurredAt,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// Duplicate delivery: re-enqueue only an event whose processing never
		// completed.
		row, gerr := q.GetBillingEventByProviderEventID(ctx, sqlc.GetBillingEventByProviderEventIDParams{
			Provider: providerName, ProviderEventID: ev.ProviderEventID,
		})
		if gerr != nil {
			return gerr
		}
		if row.ProcessedAt.Valid {
			return nil
		}
		_, ierr := s.river.InsertTx(ctx, tx, BillingApplyArgs{
			EventID: row.ID, Provider: providerName, ProviderEventID: ev.ProviderEventID,
		}, nil)
		return ierr
	}
	if err != nil {
		return err
	}
	if !ev.Handled {
		return q.MarkBillingEventProcessed(ctx, id)
	}
	_, err = s.river.InsertTx(ctx, tx, BillingApplyArgs{
		EventID: id, Provider: providerName, ProviderEventID: ev.ProviderEventID,
	}, nil)
	return err
}

// isForeignKeyViolation reports whether err is Postgres error 23503.
func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}
