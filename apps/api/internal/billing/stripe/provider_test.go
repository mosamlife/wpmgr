package stripe

// provider_test.go — fast, pure unit tests. No live Stripe API calls: webhook
// signature tests use stripe-go's own testhelpers (webhook.GenerateTestSignedPayload,
// the exact mechanism the stripe-go test suite itself uses), and subscription
// mapping tests construct *stripesdk.Subscription values directly in Go.

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	stripesdk "github.com/stripe/stripe-go/v86"
	"github.com/stripe/stripe-go/v86/webhook"

	"github.com/mosamlife/wpmgr/apps/api/internal/billing"
)

func testConfig() Config {
	return Config{
		SecretKey:     "sk_test_123",
		WebhookSecret: "whsec_test_secret",
		PriceStarter:  "price_starter",
		PriceAgency:   "price_agency",
		PriceScale:    "price_scale",
	}
}

func sign(t *testing.T, body []byte, secret string) http.Header {
	t.Helper()
	sp := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{Payload: body, Secret: secret})
	h := http.Header{}
	h.Set("Stripe-Signature", sp.Header)
	return h
}

// buildEvent wraps objectJSON in a full Stripe Event envelope, stamping the
// EXACT api_version this stripe-go build expects (stripesdk.APIVersion).
// VerifyWebhook (via webhook.ConstructEvent) refuses an event whose
// api_version does not match — deliberately NOT suppressed with
// IgnoreAPIVersionMismatch in production code, because a mismatched version
// can mean Stripe shaped the object differently (e.g. current_period_end
// lived on the subscription itself before it moved to the line item) and
// silently misparsing is worse than a loud, clear failure. Operationally,
// this means a deployed Stripe webhook endpoint MUST be configured (Stripe
// lets you pin this per-endpoint) to send this same API version — see the
// deploy-time env doc.
func buildEvent(id, evType string, created int64, objectJSON string) []byte {
	return []byte(fmt.Sprintf(
		`{"id":%q,"object":"event","api_version":%q,"type":%q,"created":%d,"data":{"object":%s}}`,
		id, stripesdk.APIVersion, evType, created, objectJSON,
	))
}

// ---------------------------------------------------------------------------
// Signature verification: valid / forged / garbled.
// ---------------------------------------------------------------------------

func TestVerifyWebhook_ValidSignature(t *testing.T) {
	p := New(testConfig())
	body := buildEvent("evt_valid", "customer.created", 1700000000, `{}`)
	headers := sign(t, body, testConfig().WebhookSecret)

	ev, err := p.VerifyWebhook(body, headers)
	if err != nil {
		t.Fatalf("VerifyWebhook: %v", err)
	}
	if ev.ProviderEventID != "evt_valid" {
		t.Fatalf("ProviderEventID = %q, want evt_valid", ev.ProviderEventID)
	}
	if ev.ProviderEventType != "customer.created" {
		t.Fatalf("ProviderEventType = %q", ev.ProviderEventType)
	}
	if ev.Handled {
		t.Fatal("customer.created is not one of the 7 acted-on event types; Handled should be false")
	}
}

func TestVerifyWebhook_ForgedSignature(t *testing.T) {
	p := New(testConfig())
	body := buildEvent("evt_forged", "customer.created", 1700000000, `{}`)
	// Signed with the WRONG secret — a forged webhook attempt.
	headers := sign(t, body, "whsec_attacker_secret")

	if _, err := p.VerifyWebhook(body, headers); err == nil {
		t.Fatal("expected an error for a signature computed with the wrong secret")
	}
}

func TestVerifyWebhook_GarbledBody(t *testing.T) {
	p := New(testConfig())
	body := buildEvent("evt_garbled", "customer.created", 1700000000, `{}`)
	headers := sign(t, body, testConfig().WebhookSecret)

	// Tamper with the body AFTER signing — the signature no longer matches.
	tampered := append(append([]byte{}, body...), '{')
	if _, err := p.VerifyWebhook(tampered, headers); err == nil {
		t.Fatal("expected an error for a body that does not match its signature")
	}
}

func TestVerifyWebhook_MissingSignatureHeader(t *testing.T) {
	p := New(testConfig())
	body := buildEvent("evt_nosig", "customer.created", 1700000000, `{}`)

	if _, err := p.VerifyWebhook(body, http.Header{}); err == nil {
		t.Fatal("expected an error when the Stripe-Signature header is absent")
	}
}

// ---------------------------------------------------------------------------
// Event normalization + tenant attribution.
// ---------------------------------------------------------------------------

func TestVerifyWebhook_CheckoutSessionCompleted_AttributesTenant(t *testing.T) {
	p := New(testConfig())
	tenantID := "b6f1e2b0-5b2a-4b7d-9b1a-000000000001"
	body := buildEvent("evt_checkout", "checkout.session.completed", 1700000000, `{
		"id": "cs_test_1",
		"object": "checkout.session",
		"mode": "subscription",
		"customer": "cus_123",
		"subscription": "sub_123",
		"client_reference_id": "`+tenantID+`",
		"metadata": {"wpmgr_tenant_id": "`+tenantID+`"}
	}`)
	headers := sign(t, body, testConfig().WebhookSecret)

	ev, err := p.VerifyWebhook(body, headers)
	if err != nil {
		t.Fatalf("VerifyWebhook: %v", err)
	}
	if !ev.Handled {
		t.Fatal("checkout.session.completed should be Handled")
	}
	if ev.Kind != billing.EventActivated {
		t.Fatalf("Kind = %q, want activated", ev.Kind)
	}
	if ev.TenantID.String() != tenantID {
		t.Fatalf("TenantID = %s, want %s", ev.TenantID, tenantID)
	}
	if ev.ProviderCustomerID != "cus_123" {
		t.Fatalf("ProviderCustomerID = %q", ev.ProviderCustomerID)
	}
	if ev.ProviderSubscriptionID != "sub_123" {
		t.Fatalf("ProviderSubscriptionID = %q", ev.ProviderSubscriptionID)
	}
}

func TestVerifyWebhook_CheckoutSessionCompleted_NonSubscriptionModeIgnored(t *testing.T) {
	p := New(testConfig())
	body := buildEvent("evt_checkout_payment", "checkout.session.completed", 1700000000,
		`{"id": "cs_test_2", "object": "checkout.session", "mode": "payment"}`)
	headers := sign(t, body, testConfig().WebhookSecret)

	ev, err := p.VerifyWebhook(body, headers)
	if err != nil {
		t.Fatalf("VerifyWebhook: %v", err)
	}
	if ev.Handled {
		t.Fatal("a payment-mode (non-subscription) checkout session should not be Handled")
	}
}

func TestVerifyWebhook_SubscriptionCreated_TrialingMapsToTrialStarted(t *testing.T) {
	p := New(testConfig())
	tenantID := "b6f1e2b0-5b2a-4b7d-9b1a-000000000002"
	body := buildEvent("evt_sub_created", "customer.subscription.created", 1700000000, `{
		"id": "sub_456",
		"object": "subscription",
		"status": "trialing",
		"customer": "cus_456",
		"metadata": {"wpmgr_tenant_id": "`+tenantID+`"}
	}`)
	headers := sign(t, body, testConfig().WebhookSecret)

	ev, err := p.VerifyWebhook(body, headers)
	if err != nil {
		t.Fatalf("VerifyWebhook: %v", err)
	}
	if ev.Kind != billing.EventTrialStarted {
		t.Fatalf("Kind = %q, want trial_started", ev.Kind)
	}
	if ev.TenantID.String() != tenantID {
		t.Fatalf("TenantID = %s, want %s", ev.TenantID, tenantID)
	}
	if ev.ProviderSubscriptionID != "sub_456" {
		t.Fatalf("ProviderSubscriptionID = %q", ev.ProviderSubscriptionID)
	}
}

func TestVerifyWebhook_SubscriptionDeleted_MapsToCanceled(t *testing.T) {
	p := New(testConfig())
	body := buildEvent("evt_sub_deleted", "customer.subscription.deleted", 1700000000,
		`{"id": "sub_789", "object": "subscription", "status": "canceled", "customer": "cus_789"}`)
	headers := sign(t, body, testConfig().WebhookSecret)

	ev, err := p.VerifyWebhook(body, headers)
	if err != nil {
		t.Fatalf("VerifyWebhook: %v", err)
	}
	if ev.Kind != billing.EventCanceled {
		t.Fatalf("Kind = %q, want canceled", ev.Kind)
	}
}

func TestVerifyWebhook_InvoicePaymentFailed_ReadsSubscriptionMetadataSnapshot(t *testing.T) {
	p := New(testConfig())
	tenantID := "b6f1e2b0-5b2a-4b7d-9b1a-000000000003"
	body := buildEvent("evt_invoice_failed", "invoice.payment_failed", 1700000000, `{
		"id": "in_1",
		"object": "invoice",
		"customer": "cus_999",
		"parent": {
			"type": "subscription_details",
			"subscription_details": {
				"subscription": "sub_999",
				"metadata": {"wpmgr_tenant_id": "`+tenantID+`"}
			}
		}
	}`)
	headers := sign(t, body, testConfig().WebhookSecret)

	ev, err := p.VerifyWebhook(body, headers)
	if err != nil {
		t.Fatalf("VerifyWebhook: %v", err)
	}
	if ev.Kind != billing.EventPaymentFailed {
		t.Fatalf("Kind = %q, want payment_failed", ev.Kind)
	}
	if ev.TenantID.String() != tenantID {
		t.Fatalf("TenantID = %s, want %s (from the invoice's subscription_details metadata snapshot)", ev.TenantID, tenantID)
	}
	if ev.ProviderSubscriptionID != "sub_999" {
		t.Fatalf("ProviderSubscriptionID = %q", ev.ProviderSubscriptionID)
	}
	if ev.ProviderCustomerID != "cus_999" {
		t.Fatalf("ProviderCustomerID = %q", ev.ProviderCustomerID)
	}
}

func TestVerifyWebhook_ChargeRefunded_NoSubscriptionReference(t *testing.T) {
	p := New(testConfig())
	body := buildEvent("evt_refund", "charge.refunded", 1700000000,
		`{"id": "ch_1", "object": "charge", "customer": "cus_111"}`)
	headers := sign(t, body, testConfig().WebhookSecret)

	ev, err := p.VerifyWebhook(body, headers)
	if err != nil {
		t.Fatalf("VerifyWebhook: %v", err)
	}
	if ev.Kind != billing.EventRefunded {
		t.Fatalf("Kind = %q, want refunded", ev.Kind)
	}
	if ev.ProviderSubscriptionID != "" {
		t.Fatalf("ProviderSubscriptionID should be empty for a bare charge event, got %q", ev.ProviderSubscriptionID)
	}
}

// ---------------------------------------------------------------------------
// CancelSubscription — cancel at period end, never immediate.
// ---------------------------------------------------------------------------

func TestCancelSubscriptionParams_CancelsAtPeriodEndNotImmediately(t *testing.T) {
	params := cancelSubscriptionParams()
	if params.CancelAtPeriodEnd == nil || !*params.CancelAtPeriodEnd {
		t.Fatal("CancelAtPeriodEnd must be true — cancellation is scheduled for the end of the current billing period, never immediate")
	}
}

// ---------------------------------------------------------------------------
// MapPriceToPlan / subscription-state mapping.
// ---------------------------------------------------------------------------

func TestMapPriceToPlan(t *testing.T) {
	p := New(testConfig())
	tests := []struct {
		price  string
		want   billing.Tier
		wantOK bool
	}{
		{"price_starter", billing.TierStarter, true},
		{"price_agency", billing.TierAgency, true},
		{"price_scale", billing.TierScale, true},
		{"price_does_not_exist", "", false},
	}
	for _, tt := range tests {
		got, ok := p.MapPriceToPlan(tt.price)
		if ok != tt.wantOK || (ok && got != tt.want) {
			t.Errorf("MapPriceToPlan(%q) = (%q, %v), want (%q, %v)", tt.price, got, ok, tt.want, tt.wantOK)
		}
	}
}

func TestToSubscription_MapsStatusAndResolvesPrice(t *testing.T) {
	p := New(testConfig())
	periodEnd := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	sub := &stripesdk.Subscription{
		ID:       "sub_1",
		Status:   stripesdk.SubscriptionStatusActive,
		Customer: &stripesdk.Customer{ID: "cus_1"},
		Items: &stripesdk.SubscriptionItemList{Data: []*stripesdk.SubscriptionItem{
			{CurrentPeriodEnd: periodEnd.Unix(), Price: &stripesdk.Price{ID: "price_agency"}},
		}},
	}

	got := p.toSubscription(sub)
	if got.ID != "sub_1" || got.CustomerID != "cus_1" {
		t.Fatalf("unexpected id/customer: %+v", got)
	}
	if got.Status != billing.StatusActive {
		t.Fatalf("Status = %q, want active", got.Status)
	}
	if !got.PlanResolved || got.Plan != billing.TierAgency {
		t.Fatalf("Plan/PlanResolved = %q/%v, want agency/true", got.Plan, got.PlanResolved)
	}
	if !got.CurrentPeriodEnd.Equal(periodEnd) {
		t.Fatalf("CurrentPeriodEnd = %v, want %v", got.CurrentPeriodEnd, periodEnd)
	}
}

func TestToSubscription_UnknownPriceIsUnresolved(t *testing.T) {
	p := New(testConfig())
	sub := &stripesdk.Subscription{
		ID:     "sub_2",
		Status: stripesdk.SubscriptionStatusActive,
		Items: &stripesdk.SubscriptionItemList{Data: []*stripesdk.SubscriptionItem{
			{Price: &stripesdk.Price{ID: "price_not_in_config"}},
		}},
	}
	got := p.toSubscription(sub)
	if got.PlanResolved {
		t.Fatal("expected PlanResolved=false for a price not in the tier map")
	}
}

func TestMapStatus_FullMatrix(t *testing.T) {
	tests := []struct {
		in   stripesdk.SubscriptionStatus
		want billing.Status
	}{
		{stripesdk.SubscriptionStatusActive, billing.StatusActive},
		{stripesdk.SubscriptionStatusTrialing, billing.StatusTrialing},
		{stripesdk.SubscriptionStatusPastDue, billing.StatusPastDue},
		{stripesdk.SubscriptionStatusUnpaid, billing.StatusPastDue},
		{stripesdk.SubscriptionStatusCanceled, billing.StatusCanceled},
		{stripesdk.SubscriptionStatusIncompleteExpired, billing.StatusCanceled},
		{stripesdk.SubscriptionStatusPaused, billing.StatusPaused},
		{stripesdk.SubscriptionStatusIncomplete, billing.StatusNone},
		{stripesdk.SubscriptionStatus("some_future_status"), billing.StatusNone},
	}
	for _, tt := range tests {
		if got := mapStatus(tt.in); got != tt.want {
			t.Errorf("mapStatus(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Config.Configured
// ---------------------------------------------------------------------------

func TestConfigConfigured(t *testing.T) {
	if (Config{}).Configured() {
		t.Fatal("an all-empty Config should not be Configured")
	}
	if testConfig().Configured() != true {
		t.Fatal("a fully-populated Config should be Configured")
	}
	partial := testConfig()
	partial.PriceScale = ""
	if partial.Configured() {
		t.Fatal("a partially-populated Config should NOT be Configured")
	}
}
