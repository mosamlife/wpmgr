package razorpay

// provider_test.go — fast, pure unit tests plus a couple of httptest.Server
// -backed REST round-trip tests (CreateCheckout/GetSubscription). Webhook and
// checkout-callback signature tests sign independently with stdlib
// crypto/hmac (NOT by calling the adapter's own verifyHMACSHA256Hex), so a
// forged/wrong-secret signature genuinely exercises the rejection path rather
// than trivially agreeing with itself.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/billing"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

func testConfig() Config {
	return Config{
		KeyID:          "rzp_test_123",
		KeySecret:      "test_key_secret",
		WebhookSecret:  "test_webhook_secret",
		PlanStarterUSD: "plan_starter_usd",
		PlanStarterINR: "plan_starter_inr",
		PlanAgencyUSD:  "plan_agency_usd",
		PlanAgencyINR:  "plan_agency_inr",
		PlanScaleUSD:   "plan_scale_usd",
		PlanScaleINR:   "plan_scale_inr",
	}
}

// signHex independently computes what a genuine Razorpay signature would be
// — deliberately NOT calling verifyHMACSHA256Hex — so a test using the WRONG
// secret/body here proves the adapter's rejection path is real, not vacuous.
func signHex(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func buildEnvelope(event string, createdAt int64, payloadJSON string) []byte {
	return []byte(fmt.Sprintf(`{"entity":"event","event":%q,"contains":[],"payload":%s,"created_at":%d}`, event, payloadJSON, createdAt))
}

func webhookHeaders(body []byte, secret, eventID string) http.Header {
	h := http.Header{}
	h.Set(signatureHeader, signHex(body, secret))
	if eventID != "" {
		h.Set(eventIDHeader, eventID)
	}
	return h
}

// ---------------------------------------------------------------------------
// Config.Configured
// ---------------------------------------------------------------------------

func TestConfigConfigured(t *testing.T) {
	if (Config{}).Configured() {
		t.Fatal("an all-empty Config should not be Configured")
	}
	if !testConfig().Configured() {
		t.Fatal("a fully-populated Config should be Configured")
	}
	partial := testConfig()
	partial.PlanScaleINR = ""
	if partial.Configured() {
		t.Fatal("a partially-populated Config should NOT be Configured")
	}
}

func TestNameAndHasPortal(t *testing.T) {
	p := New(testConfig())
	if p.Name() != "razorpay" {
		t.Fatalf("Name() = %q, want razorpay", p.Name())
	}
	if p.HasPortal() {
		t.Fatal("HasPortal() should be false — Razorpay has no hosted portal")
	}
}

// ---------------------------------------------------------------------------
// MapPriceToPlan / status mapping — over BOTH currencies.
// ---------------------------------------------------------------------------

func TestMapPriceToPlan(t *testing.T) {
	p := New(testConfig())
	tests := []struct {
		planID string
		want   billing.Tier
		wantOK bool
	}{
		{"plan_starter_usd", billing.TierStarter, true},
		{"plan_starter_inr", billing.TierStarter, true},
		{"plan_agency_usd", billing.TierAgency, true},
		{"plan_agency_inr", billing.TierAgency, true},
		{"plan_scale_usd", billing.TierScale, true},
		{"plan_scale_inr", billing.TierScale, true},
		{"plan_does_not_exist", "", false},
	}
	for _, tt := range tests {
		got, ok := p.MapPriceToPlan(tt.planID)
		if ok != tt.wantOK || (ok && got != tt.want) {
			t.Errorf("MapPriceToPlan(%q) = (%q, %v), want (%q, %v)", tt.planID, got, ok, tt.want, tt.wantOK)
		}
	}
}

func TestMapStatus_FullMatrix(t *testing.T) {
	tests := []struct {
		in   string
		want billing.Status
	}{
		{"active", billing.StatusActive},
		{"pending", billing.StatusPastDue},
		{"halted", billing.StatusPastDue},
		{"cancelled", billing.StatusCanceled},
		{"completed", billing.StatusCanceled},
		{"paused", billing.StatusPaused},
		{"created", billing.StatusNone},
		{"authenticated", billing.StatusNone},
		{"some_future_status", billing.StatusNone},
	}
	for _, tt := range tests {
		if got := mapStatus(tt.in); got != tt.want {
			t.Errorf("mapStatus(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestToSubscription_UnknownPlanIsUnresolved(t *testing.T) {
	p := New(testConfig())
	got := p.toSubscription(subscriptionEntity{ID: "sub_x", Status: "active", PlanID: "plan_not_configured"})
	if got.PlanResolved {
		t.Fatal("expected PlanResolved=false for a plan id not in the tier map")
	}
}

// ---------------------------------------------------------------------------
// CreateCheckout — currency validation + (tier,currency) resolution.
// ---------------------------------------------------------------------------

func TestCreateCheckout_RejectsInvalidCurrency(t *testing.T) {
	p := New(testConfig())
	for _, cur := range []string{"", "EUR", "usd-x"} {
		_, err := p.CreateCheckout(context.Background(), billing.CheckoutInput{
			TenantID: uuid.New(), Plan: billing.TierStarter, Currency: cur,
		})
		de, ok := domain.AsDomain(err)
		if !ok || de.Kind != domain.KindValidation {
			t.Errorf("currency %q: want KindValidation, got %v", cur, err)
		}
	}
}

func TestCreateCheckout_UnconfiguredTierCurrencyCombo(t *testing.T) {
	cfg := testConfig()
	cfg.PlanScaleINR = "" // deliberately leave one (tier,currency) pair unconfigured
	p := New(cfg)

	_, err := p.CreateCheckout(context.Background(), billing.CheckoutInput{
		TenantID: uuid.New(), Plan: billing.TierScale, Currency: "INR",
	})
	de, ok := domain.AsDomain(err)
	if !ok || de.Kind != domain.KindValidation {
		t.Fatalf("want KindValidation for an unconfigured (tier,currency) pair, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// CreateCheckout / GetSubscription — REST round trips against an
// httptest.Server standing in for the Razorpay API.
// ---------------------------------------------------------------------------

func TestCreateCheckout_Success(t *testing.T) {
	var gotUser, gotPass string
	var gotSubscriptionBody []byte

	mux := http.NewServeMux()
	mux.HandleFunc("/plans/plan_starter_usd", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"plan_starter_usd","item":{"amount":1500,"currency":"USD"}}`)
	})
	mux.HandleFunc("/subscriptions", func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, _ = r.BasicAuth()
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read subscription create request body: %v", err)
		}
		gotSubscriptionBody = b
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"sub_new_1","plan_id":"plan_starter_usd","status":"created"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := testConfig()
	cfg.BaseURL = srv.URL
	p := New(cfg)

	tenantID := uuid.New()
	sess, err := p.CreateCheckout(context.Background(), billing.CheckoutInput{
		TenantID: tenantID,
		Plan:     billing.TierStarter,
		Currency: "usd", // lower-case, exercises the ToUpper normalization
	})
	if err != nil {
		t.Fatalf("CreateCheckout: %v", err)
	}
	if sess.URL != "" {
		t.Errorf("URL should be empty for a Razorpay checkout, got %q", sess.URL)
	}
	if sess.Razorpay == nil {
		t.Fatal("expected non-nil Razorpay checkout data")
	}
	if sess.Razorpay.SubscriptionID != "sub_new_1" {
		t.Errorf("SubscriptionID = %q, want sub_new_1", sess.Razorpay.SubscriptionID)
	}
	if sess.Razorpay.KeyID != cfg.KeyID {
		t.Errorf("KeyID = %q, want %q", sess.Razorpay.KeyID, cfg.KeyID)
	}
	if sess.Razorpay.Currency != "USD" || sess.Razorpay.AmountMinor != 1500 {
		t.Errorf("Currency/AmountMinor = %s/%d, want USD/1500", sess.Razorpay.Currency, sess.Razorpay.AmountMinor)
	}
	if gotUser != cfg.KeyID || gotPass != cfg.KeySecret {
		t.Errorf("basic auth = %s/%s, want %s/%s", gotUser, gotPass, cfg.KeyID, cfg.KeySecret)
	}

	var reqBody map[string]any
	if err := json.Unmarshal(gotSubscriptionBody, &reqBody); err != nil {
		t.Fatalf("decode subscription create request body: %v", err)
	}
	if reqBody["plan_id"] != "plan_starter_usd" {
		t.Errorf("plan_id = %v, want plan_starter_usd", reqBody["plan_id"])
	}
	notes, _ := reqBody["notes"].(map[string]any)
	if notes == nil || notes[tenantNotesKey] != tenantID.String() {
		t.Errorf("notes.%s = %v, want %s", tenantNotesKey, notes, tenantID)
	}
}

func TestGetSubscription_MapsStatusAndResolvesPlan(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/subscriptions/sub_1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"sub_1","plan_id":"plan_agency_inr","customer_id":"cust_1","status":"active","current_end":1700003600}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := testConfig()
	cfg.BaseURL = srv.URL
	p := New(cfg)

	sub, err := p.GetSubscription(context.Background(), "sub_1")
	if err != nil {
		t.Fatalf("GetSubscription: %v", err)
	}
	if sub.ID != "sub_1" || sub.CustomerID != "cust_1" {
		t.Fatalf("unexpected id/customer: %+v", sub)
	}
	if sub.Status != billing.StatusActive {
		t.Errorf("Status = %q, want active", sub.Status)
	}
	if !sub.PlanResolved || sub.Plan != billing.TierAgency {
		t.Errorf("Plan/PlanResolved = %q/%v, want agency/true", sub.Plan, sub.PlanResolved)
	}
	want := time.Unix(1700003600, 0).UTC()
	if !sub.CurrentPeriodEnd.Equal(want) {
		t.Errorf("CurrentPeriodEnd = %v, want %v", sub.CurrentPeriodEnd, want)
	}
}

func TestGetSubscription_APIErrorWrapsCleanly(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/subscriptions/sub_missing", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":{"code":"BAD_REQUEST_ERROR","description":"subscription not found"}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := testConfig()
	cfg.BaseURL = srv.URL
	p := New(cfg)

	_, err := p.GetSubscription(context.Background(), "sub_missing")
	de, ok := domain.AsDomain(err)
	if !ok || de.Kind != domain.KindInternal {
		t.Fatalf("want a wrapped KindInternal domain error, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// CancelSubscription — cancel-at-cycle-end, never immediate.
// ---------------------------------------------------------------------------

func TestCancelSubscription_Success(t *testing.T) {
	var gotMethod string
	var gotBody []byte

	mux := http.NewServeMux()
	mux.HandleFunc("/subscriptions/sub_cancel_1/cancel", func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read cancel request body: %v", err)
		}
		gotBody = b
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"sub_cancel_1","status":"active"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := testConfig()
	cfg.BaseURL = srv.URL
	p := New(cfg)

	if err := p.CancelSubscription(context.Background(), "sub_cancel_1"); err != nil {
		t.Fatalf("CancelSubscription: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}

	var reqBody map[string]any
	if err := json.Unmarshal(gotBody, &reqBody); err != nil {
		t.Fatalf("decode cancel request body: %v", err)
	}
	// cancel_at_cycle_end=1 (NOT immediate) — the customer keeps access
	// through what they already paid for.
	if v, _ := reqBody["cancel_at_cycle_end"].(float64); v != 1 {
		t.Errorf("cancel_at_cycle_end = %v, want 1 (cancel at cycle end, not immediate)", reqBody["cancel_at_cycle_end"])
	}
}

func TestCancelSubscription_APIErrorWrapsCleanly(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/subscriptions/sub_missing/cancel", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":{"code":"BAD_REQUEST_ERROR","description":"subscription not found"}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := testConfig()
	cfg.BaseURL = srv.URL
	p := New(cfg)

	err := p.CancelSubscription(context.Background(), "sub_missing")
	de, ok := domain.AsDomain(err)
	if !ok || de.Kind != domain.KindInternal {
		t.Fatalf("want a wrapped KindInternal domain error, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// CreatePortalSession — always "not supported".
// ---------------------------------------------------------------------------

func TestCreatePortalSession_NotSupported(t *testing.T) {
	p := New(testConfig())
	_, err := p.CreatePortalSession(context.Background(), "cust_1")
	de, ok := domain.AsDomain(err)
	if !ok || de.Kind != domain.KindUnavailable {
		t.Fatalf("want KindUnavailable (no hosted portal), got %v", err)
	}
}

// ---------------------------------------------------------------------------
// VerifyWebhook — signature verification.
// ---------------------------------------------------------------------------

func TestVerifyWebhook_ValidSignature_UnhandledEventType(t *testing.T) {
	p := New(testConfig())
	body := buildEnvelope("customer.created", 1700000000, `{}`)
	headers := webhookHeaders(body, testConfig().WebhookSecret, "evt_1")

	ev, err := p.VerifyWebhook(body, headers)
	if err != nil {
		t.Fatalf("VerifyWebhook: %v", err)
	}
	if ev.ProviderEventID != "evt_1" {
		t.Errorf("ProviderEventID = %q, want evt_1", ev.ProviderEventID)
	}
	if ev.ProviderEventType != "customer.created" {
		t.Errorf("ProviderEventType = %q", ev.ProviderEventType)
	}
	if ev.Handled {
		t.Fatal("an event type not in the acted-on list should not be Handled")
	}
}

func TestVerifyWebhook_ForgedSignature(t *testing.T) {
	p := New(testConfig())
	body := buildEnvelope("subscription.activated", 1700000000, `{}`)
	// Signed with the WRONG secret — a forged webhook attempt.
	headers := webhookHeaders(body, "attacker_secret", "evt_forged")

	if _, err := p.VerifyWebhook(body, headers); err == nil {
		t.Fatal("expected an error for a signature computed with the wrong secret")
	}
}

func TestVerifyWebhook_TamperedBody(t *testing.T) {
	p := New(testConfig())
	body := buildEnvelope("subscription.activated", 1700000000, `{}`)
	headers := webhookHeaders(body, testConfig().WebhookSecret, "evt_tampered")

	// Tamper with the body AFTER signing — the signature no longer matches.
	tampered := append(append([]byte{}, body...), '{')
	if _, err := p.VerifyWebhook(tampered, headers); err == nil {
		t.Fatal("expected an error for a body that does not match its signature")
	}
}

func TestVerifyWebhook_MissingSignatureHeader(t *testing.T) {
	p := New(testConfig())
	body := buildEnvelope("subscription.activated", 1700000000, `{}`)
	h := http.Header{}
	h.Set(eventIDHeader, "evt_1")

	if _, err := p.VerifyWebhook(body, h); err == nil {
		t.Fatal("expected an error when the X-Razorpay-Signature header is absent")
	}
}

func TestVerifyWebhook_MissingEventIDHeader(t *testing.T) {
	p := New(testConfig())
	body := buildEnvelope("subscription.activated", 1700000000, `{}`)
	h := http.Header{}
	h.Set(signatureHeader, signHex(body, testConfig().WebhookSecret))

	if _, err := p.VerifyWebhook(body, h); err == nil {
		t.Fatal("expected an error when the X-Razorpay-Event-Id header is absent")
	}
}

// ---------------------------------------------------------------------------
// VerifyWebhook — event normalization + tenant attribution.
// ---------------------------------------------------------------------------

func TestVerifyWebhook_SubscriptionCharged_AttributesTenant(t *testing.T) {
	p := New(testConfig())
	tenantID := uuid.New()
	payload := fmt.Sprintf(`{
		"subscription": {"entity": {
			"id": "sub_1", "plan_id": "plan_starter_usd", "customer_id": "cust_1",
			"status": "active", "current_end": 1700003600,
			"notes": {"wpmgr_tenant_id": %q}
		}},
		"payment": {"entity": {"id": "pay_1"}}
	}`, tenantID)
	body := buildEnvelope("subscription.charged", 1700000000, payload)
	headers := webhookHeaders(body, testConfig().WebhookSecret, "evt_charged")

	ev, err := p.VerifyWebhook(body, headers)
	if err != nil {
		t.Fatalf("VerifyWebhook: %v", err)
	}
	if !ev.Handled {
		t.Fatal("subscription.charged should be Handled")
	}
	if ev.Kind != billing.EventPaymentSucceeded {
		t.Errorf("Kind = %q, want payment_succeeded", ev.Kind)
	}
	if ev.TenantID != tenantID {
		t.Errorf("TenantID = %s, want %s", ev.TenantID, tenantID)
	}
	if ev.ProviderSubscriptionID != "sub_1" {
		t.Errorf("ProviderSubscriptionID = %q", ev.ProviderSubscriptionID)
	}
	if ev.ProviderCustomerID != "cust_1" {
		t.Errorf("ProviderCustomerID = %q", ev.ProviderCustomerID)
	}
}

func TestVerifyWebhook_SubscriptionPending_MapsToPastDue(t *testing.T) {
	p := New(testConfig())
	body := buildEnvelope("subscription.pending", 1700000000, `{"subscription":{"entity":{"id":"sub_2","status":"pending"}}}`)
	headers := webhookHeaders(body, testConfig().WebhookSecret, "evt_pending")

	ev, err := p.VerifyWebhook(body, headers)
	if err != nil {
		t.Fatalf("VerifyWebhook: %v", err)
	}
	if ev.Kind != billing.EventPastDue {
		t.Errorf("Kind = %q, want past_due", ev.Kind)
	}
}

func TestVerifyWebhook_SubscriptionHalted_MapsToPastDue(t *testing.T) {
	p := New(testConfig())
	body := buildEnvelope("subscription.halted", 1700000000, `{"subscription":{"entity":{"id":"sub_3","status":"halted"}}}`)
	headers := webhookHeaders(body, testConfig().WebhookSecret, "evt_halted")

	ev, err := p.VerifyWebhook(body, headers)
	if err != nil {
		t.Fatalf("VerifyWebhook: %v", err)
	}
	if ev.Kind != billing.EventPastDue {
		t.Errorf("Kind = %q, want past_due", ev.Kind)
	}
}

func TestVerifyWebhook_SubscriptionCancelled_MapsToCanceled(t *testing.T) {
	p := New(testConfig())
	body := buildEnvelope("subscription.cancelled", 1700000000, `{"subscription":{"entity":{"id":"sub_4","status":"cancelled"}}}`)
	headers := webhookHeaders(body, testConfig().WebhookSecret, "evt_cancelled")

	ev, err := p.VerifyWebhook(body, headers)
	if err != nil {
		t.Fatalf("VerifyWebhook: %v", err)
	}
	if ev.Kind != billing.EventCanceled {
		t.Errorf("Kind = %q, want canceled", ev.Kind)
	}
}

func TestVerifyWebhook_PaymentFailed_FallsBackToPaymentEntity(t *testing.T) {
	p := New(testConfig())
	tenantID := uuid.New()
	payload := fmt.Sprintf(`{"payment":{"entity":{"id":"pay_2","subscription_id":"sub_5","notes":{"wpmgr_tenant_id":%q}}}}`, tenantID)
	body := buildEnvelope("payment.failed", 1700000000, payload)
	headers := webhookHeaders(body, testConfig().WebhookSecret, "evt_payfail")

	ev, err := p.VerifyWebhook(body, headers)
	if err != nil {
		t.Fatalf("VerifyWebhook: %v", err)
	}
	if ev.Kind != billing.EventPaymentFailed {
		t.Errorf("Kind = %q, want payment_failed", ev.Kind)
	}
	if ev.ProviderSubscriptionID != "sub_5" {
		t.Errorf("ProviderSubscriptionID = %q, want sub_5 (read from the payment entity, no sibling subscription object)", ev.ProviderSubscriptionID)
	}
	if ev.TenantID != tenantID {
		t.Errorf("TenantID = %s, want %s", ev.TenantID, tenantID)
	}
}

// ---------------------------------------------------------------------------
// VerifyCheckoutCallback — the browser Checkout.js onSuccess callback.
// ---------------------------------------------------------------------------

func TestVerifyCheckoutCallback_Valid(t *testing.T) {
	p := New(testConfig())
	paymentID, subscriptionID := "pay_1", "sub_1"
	sig := signHex([]byte(paymentID+"|"+subscriptionID), testConfig().KeySecret)

	err := p.VerifyCheckoutCallback(map[string]string{
		"razorpay_payment_id":      paymentID,
		"razorpay_subscription_id": subscriptionID,
		"razorpay_signature":       sig,
	})
	if err != nil {
		t.Fatalf("VerifyCheckoutCallback: %v", err)
	}
}

func TestVerifyCheckoutCallback_ForgedSignature(t *testing.T) {
	p := New(testConfig())
	// Signed with the wrong secret (the webhook secret, not the key secret) —
	// a forged/mismatched callback.
	sig := signHex([]byte("pay_1|sub_1"), testConfig().WebhookSecret)

	err := p.VerifyCheckoutCallback(map[string]string{
		"razorpay_payment_id":      "pay_1",
		"razorpay_subscription_id": "sub_1",
		"razorpay_signature":       sig,
	})
	de, ok := domain.AsDomain(err)
	if !ok || de.Kind != domain.KindUnauthorized {
		t.Fatalf("want KindUnauthorized for a forged callback signature, got %v", err)
	}
}

func TestVerifyCheckoutCallback_MissingFields(t *testing.T) {
	p := New(testConfig())
	err := p.VerifyCheckoutCallback(map[string]string{})
	de, ok := domain.AsDomain(err)
	if !ok || de.Kind != domain.KindValidation {
		t.Fatalf("want KindValidation for missing callback fields, got %v", err)
	}
}
