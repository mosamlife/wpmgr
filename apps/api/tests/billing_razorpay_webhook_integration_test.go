package tests

// billing_razorpay_webhook_integration_test.go — proves the Razorpay adapter
// is a real, second Provider plugged into the SAME provider-agnostic webhook
// flow as Stripe: a razorpay-shaped webhook (real HMAC signature, real
// envelope shape, a real GetSubscription REST round trip against an
// httptest.Server standing in for the Razorpay API) resolves via the
// registry (no more 404) and drives tenants.plan/plan_status to active via
// the EXACT SAME Service.ProcessWebhook/state-machine path Stripe uses.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mosamlife/wpmgr/apps/api/internal/billing"
	billingrazorpay "github.com/mosamlife/wpmgr/apps/api/internal/billing/razorpay"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

func razorpaySignHex(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// TestRazorpayWebhook_ResolvesProviderAndActivatesTenant is the "no more
// 404" regression guard: before this adapter existed, POST
// /webhooks/billing/razorpay 404'd for EVERY tenant (ProcessWebhook's
// registry lookup failed for any provider name it didn't recognize). Here the
// registry has a real *razorpay.Provider registered, a real webhook
// signature is verified, and the subsequent GetSubscription re-fetch is a
// real HTTP round trip (against an httptest.Server standing in for
// api.razorpay.com) — proving the full pull-is-the-truth path, not just the
// signature check.
func TestRazorpayWebhook_ResolvesProviderAndActivatesTenant(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "billing-razorpay-webhook")
	setTenantProvider(t, pool, tenant, "razorpay", "cust_rzp_1", "")

	const webhookSecret = "whsec_test_razorpay"
	mux := http.NewServeMux()
	mux.HandleFunc("/subscriptions/sub_rzp_1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"sub_rzp_1","plan_id":"plan_starter_usd","customer_id":"cust_rzp_1","status":"active","current_end":1900000000}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	rzp := billingrazorpay.New(billingrazorpay.Config{
		KeyID:          "rzp_test_key",
		KeySecret:      "rzp_test_secret",
		WebhookSecret:  webhookSecret,
		PlanStarterUSD: "plan_starter_usd",
		PlanStarterINR: "plan_starter_inr",
		PlanAgencyUSD:  "plan_agency_usd",
		PlanAgencyINR:  "plan_agency_inr",
		PlanScaleUSD:   "plan_scale_usd",
		PlanScaleINR:   "plan_scale_inr",
		BaseURL:        srv.URL,
	})

	svc := billing.New(pool, nil, true, domain.SystemClock{}, slog.Default())
	svc.SetProviders(billing.NewRegistry(rzp), "razorpay")

	payload := fmt.Sprintf(`{
		"subscription": {"entity": {
			"id": "sub_rzp_1", "plan_id": "plan_starter_usd", "customer_id": "cust_rzp_1",
			"status": "active", "current_end": 1900000000,
			"notes": {"wpmgr_tenant_id": %q}
		}}
	}`, tenant)
	body := []byte(fmt.Sprintf(`{"entity":"event","event":"subscription.charged","contains":["subscription"],"payload":%s,"created_at":1700000000}`, payload))

	headers := http.Header{}
	headers.Set("X-Razorpay-Signature", razorpaySignHex(body, webhookSecret))
	headers.Set("X-Razorpay-Event-Id", "evt_rzp_charged_1")

	if err := svc.ProcessWebhook(ctx, "razorpay", body, headers); err != nil {
		t.Fatalf("ProcessWebhook(razorpay): %v", err)
	}

	plan, status := getTenantPlanStatus(t, pool, tenant)
	if plan != string(billing.TierStarter) || status != "active" {
		t.Fatalf("plan/status = %s/%s, want starter/active", plan, status)
	}
	if got := countBillingEvents(t, pool, "razorpay", "evt_rzp_charged_1"); got != 1 {
		t.Fatalf("billing_events rows for evt_rzp_charged_1 = %d, want 1", got)
	}
}

// TestRazorpayWebhook_ForgedSignatureRejected proves the 401 path: a webhook
// claiming to be from Razorpay, but signed with the wrong secret, is
// rejected before touching the tenant or the billing_events ledger.
func TestRazorpayWebhook_ForgedSignatureRejected(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "billing-razorpay-forged")
	setTenantProvider(t, pool, tenant, "razorpay", "cust_rzp_2", "")

	rzp := billingrazorpay.New(billingrazorpay.Config{
		KeyID:          "rzp_test_key",
		KeySecret:      "rzp_test_secret",
		WebhookSecret:  "whsec_real_secret",
		PlanStarterUSD: "plan_starter_usd",
		PlanStarterINR: "plan_starter_inr",
		PlanAgencyUSD:  "plan_agency_usd",
		PlanAgencyINR:  "plan_agency_inr",
		PlanScaleUSD:   "plan_scale_usd",
		PlanScaleINR:   "plan_scale_inr",
	})
	svc := billing.New(pool, nil, true, domain.SystemClock{}, slog.Default())
	svc.SetProviders(billing.NewRegistry(rzp), "razorpay")

	body := []byte(`{"entity":"event","event":"subscription.charged","contains":[],"payload":{},"created_at":1700000000}`)
	headers := http.Header{}
	headers.Set("X-Razorpay-Signature", razorpaySignHex(body, "attacker_secret"))
	headers.Set("X-Razorpay-Event-Id", "evt_rzp_forged")

	err := svc.ProcessWebhook(ctx, "razorpay", body, headers)
	if err == nil {
		t.Fatal("expected an error for a forged Razorpay webhook signature")
	}
	if got := countBillingEvents(t, pool, "razorpay", "evt_rzp_forged"); got != 0 {
		t.Fatalf("a rejected signature must never reach the billing_events ledger, found %d rows", got)
	}
}
