package razorpay

// webhook.go — Provider.VerifyWebhook: signature verification (raw body,
// BEFORE any JSON unmarshal) + normalization of Razorpay's webhook envelope
// to billing.Event. https://razorpay.com/docs/webhooks/subscriptions/ /
// https://razorpay.com/docs/webhooks/validate-test/
//
// Razorpay's webhook envelope shape:
//
//	{
//	  "entity": "event",
//	  "event": "subscription.charged",
//	  "contains": ["subscription", "payment"],
//	  "payload": {
//	    "subscription": {"entity": { ...subscription fields... }},
//	    "payment":      {"entity": { ...payment fields...      }}
//	  },
//	  "created_at": 1700000000
//	}
//
// Unlike Stripe, Razorpay does not carry a stable event id in the JSON body
// itself — the dedup key rides the X-Razorpay-Event-Id HEADER instead (see
// eventIDHeader below).

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/billing"
)

const (
	signatureHeader = "X-Razorpay-Signature"
	eventIDHeader   = "X-Razorpay-Event-Id"

	// tenantNotesKey is the Razorpay subscription/payment "notes" key this
	// adapter stamps with the tenant id at CreateCheckout time (Razorpay has
	// no client_reference_id/metadata equivalent — notes is the ONLY place
	// tenant attribution can ride).
	tenantNotesKey = "wpmgr_tenant_id"
)

// webhookEnvelope is Razorpay's top-level webhook body.
type webhookEnvelope struct {
	Entity    string          `json:"entity"`
	Event     string          `json:"event"`
	Contains  []string        `json:"contains"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt int64           `json:"created_at"`
}

// entityContainer is the {"entity": {...}} wrapper Razorpay puts around each
// named entity (payload.subscription, payload.payment, ...) inside payload.
type entityContainer struct {
	Entity json.RawMessage `json:"entity"`
}

// webhookPayload is the envelope's "payload" object, decoded just enough to
// reach the subscription/payment entities this adapter needs.
type webhookPayload struct {
	Subscription *entityContainer `json:"subscription"`
	Payment      *entityContainer `json:"payment"`
}

// paymentEntity is trimmed to the fields VerifyWebhook needs from a
// payment.* event when no sibling "subscription" entity is present in the
// same payload (Razorpay recurring-payment webhooks usually include both;
// this is the defensive fallback).
type paymentEntity struct {
	ID             string            `json:"id"`
	SubscriptionID string            `json:"subscription_id"`
	Notes          map[string]string `json:"notes"`
}

// VerifyWebhook implements billing.Provider: verifies the
// X-Razorpay-Signature header (HMAC-SHA256 of the EXACT raw body, keyed by
// the configured webhook secret) BEFORE touching the body's contents in any
// way, then normalizes the event. Returns an error on ANY signature failure —
// the caller (Service.ProcessWebhook) maps that to HTTP 401 without this
// function having touched the database.
func (p *Provider) VerifyWebhook(rawBody []byte, headers http.Header) (billing.Event, error) {
	sig := headers.Get(signatureHeader)
	if sig == "" {
		return billing.Event{}, errors.New("razorpay: missing " + signatureHeader + " header")
	}
	// Signature check happens BEFORE any json.Unmarshal of rawBody — the raw
	// bytes are what was signed; re-marshaling a decoded value first would
	// silently change key order/whitespace and break verification.
	if !verifyHMACSHA256Hex(rawBody, sig, p.webhookSecret) {
		return billing.Event{}, errors.New("razorpay: webhook signature verification failed")
	}

	eventID := headers.Get(eventIDHeader)
	if eventID == "" {
		// Real Razorpay deliveries always carry this header; its absence
		// means we cannot dedup this delivery safely against
		// billing_events.provider_event_id — fail loud rather than risk
		// treating two distinct events (both with an empty id) as the same
		// delivery.
		return billing.Event{}, errors.New("razorpay: missing " + eventIDHeader + " header")
	}

	var env webhookEnvelope
	if err := json.Unmarshal(rawBody, &env); err != nil {
		return billing.Event{}, errors.New("razorpay: malformed webhook body: " + err.Error())
	}

	out := billing.Event{
		ProviderEventID:   eventID,
		ProviderEventType: env.Event,
		OccurredAt:        time.Unix(env.CreatedAt, 0).UTC(),
		Raw:               rawBody,
	}

	// Handled/Kind: a best-effort ledger label. The ACTUAL state transition
	// never trusts this — Service.ProcessWebhook always re-fetches the
	// subscription's current truth via GetSubscription before mutating
	// anything (mirrors the Stripe adapter exactly).
	switch env.Event {
	case "subscription.activated":
		out.Handled = true
		out.Kind = billing.EventActivated
	case "subscription.charged":
		out.Handled = true
		out.Kind = billing.EventPaymentSucceeded
	case "subscription.pending":
		// Razorpay is mid-retry on a failed charge; do NOT revoke — mirrors
		// Stripe's past_due grace window.
		out.Handled = true
		out.Kind = billing.EventPastDue
	case "subscription.halted":
		// Razorpay's retry schedule is exhausted (mirrors Stripe's
		// "unpaid"). Still normalized to EventPastDue — there is no separate
		// "terminal past_due" EventKind; the ACTUAL grace-window/downgrade
		// decision is made by nextBillingState from the re-fetched Status,
		// not from this ledger label.
		out.Handled = true
		out.Kind = billing.EventPastDue
	case "subscription.cancelled", "subscription.completed":
		out.Handled = true
		out.Kind = billing.EventCanceled
	case "subscription.paused":
		out.Handled = true
		out.Kind = billing.EventPaused
	case "subscription.resumed":
		out.Handled = true
		out.Kind = billing.EventResumed
	case "payment.failed":
		out.Handled = true
		out.Kind = billing.EventPaymentFailed
	default:
		out.Handled = false
	}

	// Attribution: parsed best-effort. A malformed/absent payload sub-object
	// still leaves the event correctly ledgered (ProviderEventID/Type/Kind
	// above) — it just cannot be reconciled against a tenant, exactly like
	// the "unknown customer" path in Service.ProcessWebhook.
	var payload webhookPayload
	if len(env.Payload) > 0 {
		_ = json.Unmarshal(env.Payload, &payload)
	}

	if payload.Subscription != nil && len(payload.Subscription.Entity) > 0 {
		var sub subscriptionEntity
		if err := json.Unmarshal(payload.Subscription.Entity, &sub); err == nil {
			out.ProviderSubscriptionID = sub.ID
			out.ProviderCustomerID = sub.CustomerID
			out.TenantID = tenantIDFromNotes(sub.Notes)
		}
	} else if payload.Payment != nil && len(payload.Payment.Entity) > 0 {
		var pay paymentEntity
		if err := json.Unmarshal(payload.Payment.Entity, &pay); err == nil {
			out.ProviderSubscriptionID = pay.SubscriptionID
			out.TenantID = tenantIDFromNotes(pay.Notes)
		}
	}

	return out, nil
}

// tenantIDFromNotes parses billing.Event.TenantID from a Razorpay entity's
// "notes" map. Returns uuid.Nil when absent/unparseable — the caller then
// falls back to the (provider, provider_customer_id) lookup, same as Stripe.
func tenantIDFromNotes(notes map[string]string) uuid.UUID {
	if notes == nil {
		return uuid.Nil
	}
	if v, ok := notes[tenantNotesKey]; ok && v != "" {
		if parsed, err := uuid.Parse(v); err == nil {
			return parsed
		}
	}
	return uuid.Nil
}
