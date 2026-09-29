package stripe

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	stripesdk "github.com/stripe/stripe-go/v86"

	"github.com/mosamlife/wpmgr/apps/api/internal/billing"
)

// formRecorder keeps the decoded form body of every POST the recording
// transport answers, keyed by path.
type formRecorder struct {
	mu    sync.Mutex
	forms map[string][]url.Values
}

func (f *formRecorder) record(r *http.Request) {
	if r.Method != http.MethodPost || r.Body == nil {
		return
	}
	b, _ := io.ReadAll(r.Body)
	v, _ := url.ParseQuery(string(b))
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.forms == nil {
		f.forms = map[string][]url.Values{}
	}
	f.forms[r.URL.Path] = append(f.forms[r.URL.Path], v)
}

func (f *formRecorder) last(path string) url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	all := f.forms[path]
	if len(all) == 0 {
		return nil
	}
	return all[len(all)-1]
}

func TestCheckoutSessionParams_TaxCardOnlyAndMetadata(t *testing.T) {
	forms := &formRecorder{}
	p, _ := newRecordingProvider(func(r *http.Request) (int, string) {
		forms.record(r)
		return 200, `{"id":"cs_1","object":"checkout.session","url":"https://checkout.test/cs_1"}`
	})
	fixed := time.Unix(1_800_000_000, 0)
	p.now = func() time.Time { return fixed }
	tenant := uuid.New()

	sess, err := p.CreateCheckout(context.Background(), billing.CheckoutInput{
		TenantID: tenant, Plan: billing.TierAgency, CustomerEmail: "owner@example.com",
		ProviderCustomerID: "cus_A", SuccessURL: "https://s", CancelURL: "https://c",
	})
	if err != nil {
		t.Fatalf("CreateCheckout: %v", err)
	}
	if sess.SessionID != "cs_1" || sess.URL == "" {
		t.Fatalf("session = %+v, want id cs_1 and a URL", sess)
	}
	f := forms.last("/v1/checkout/sessions")
	if f == nil {
		t.Fatal("no checkout session create request was sent")
	}
	want := map[string]string{
		"customer":                                     "cus_A",
		"mode":                                         "subscription",
		"client_reference_id":                          tenant.String(),
		"adaptive_pricing[enabled]":                    "false",
		"automatic_tax[enabled]":                       "true",
		"billing_address_collection":                   "required",
		"customer_update[address]":                     "auto",
		"customer_update[name]":                        "auto",
		"tax_id_collection[enabled]":                   "true",
		"tax_id_collection[required]":                  "if_supported",
		"payment_method_types[0]":                      "card",
		"metadata[wpmgr_tenant_id]":                    tenant.String(),
		"metadata[app]":                                "wpmgr",
		"subscription_data[metadata][wpmgr_tenant_id]": tenant.String(),
		"subscription_data[metadata][app]":             "wpmgr",
		"line_items[0][price]":                         "price_agency",
		"expires_at":                                   fmt.Sprint(fixed.Add(time.Hour).Unix()),
	}
	for k, v := range want {
		if got := f.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	for _, k := range []string{"customer_email", "payment_method_types[1]", "payment_method_configuration", "customer_creation"} {
		if f.Has(k) {
			t.Errorf("%s must not be sent, got %q", k, f.Get(k))
		}
	}
}

// TestCheckoutSessionParams_AdaptivePricingAlwaysOff proves every Checkout
// Session this adapter creates explicitly turns Adaptive Pricing OFF for
// that one session — never an account-wide setting — so a shared Stripe
// account's dashboard default (Adaptive Pricing on) can never let Stripe
// Checkout offer a non-USD currency, with its own conversion fee, to a
// WPMgr customer. WPMgr's rule is US$ for everyone.
func TestCheckoutSessionParams_AdaptivePricingAlwaysOff(t *testing.T) {
	forms := &formRecorder{}
	p, _ := newRecordingProvider(func(r *http.Request) (int, string) {
		forms.record(r)
		return 200, `{"id":"cs_1","object":"checkout.session","url":"https://checkout.test/cs_1"}`
	})
	_, err := p.CreateCheckout(context.Background(), billing.CheckoutInput{
		TenantID: uuid.New(), Plan: billing.TierStarter, CustomerEmail: "owner@example.com",
		ProviderCustomerID: "cus_A", SuccessURL: "https://s", CancelURL: "https://c",
	})
	if err != nil {
		t.Fatalf("CreateCheckout: %v", err)
	}
	f := forms.last("/v1/checkout/sessions")
	if f == nil {
		t.Fatal("no checkout session create request was sent")
	}
	if got := f.Get("adaptive_pricing[enabled]"); got != "false" {
		t.Fatalf("adaptive_pricing[enabled] = %q, want false", got)
	}
}

// TestCheckoutSessionParams_TaxIDRequired_TogglesWithConfig proves
// Config.TaxIDRequired drives tax_id_collection.required: true (the
// default) sends if_supported, false sends never — WITHOUT ever disabling
// collection itself, so a tax ID stays offered either way.
func TestCheckoutSessionParams_TaxIDRequired_TogglesWithConfig(t *testing.T) {
	tests := []struct {
		name          string
		taxIDRequired bool
		wantRequired  string
	}{
		{"required by default", true, string(stripesdk.CheckoutSessionTaxIDCollectionRequiredIfSupported)},
		{"required disabled", false, string(stripesdk.CheckoutSessionTaxIDCollectionRequiredNever)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.TaxIDRequired = tt.taxIDRequired
			p := New(cfg)
			params := p.checkoutSessionParams(billing.CheckoutInput{
				TenantID: uuid.New(), Plan: billing.TierStarter,
				ProviderCustomerID: "cus_A", SuccessURL: "https://s", CancelURL: "https://c",
			}, "price_starter")
			if params.TaxIDCollection == nil || params.TaxIDCollection.Enabled == nil || !*params.TaxIDCollection.Enabled {
				t.Fatal("tax_id_collection.enabled must stay true regardless of TaxIDRequired")
			}
			if params.TaxIDCollection.Required == nil || *params.TaxIDCollection.Required != tt.wantRequired {
				t.Fatalf("tax_id_collection.required = %v, want %q", params.TaxIDCollection.Required, tt.wantRequired)
			}
		})
	}
}

func TestCreateCheckout_EmptyCustomerSendsNothing(t *testing.T) {
	p, rt := newRecordingProvider(func(*http.Request) (int, string) { return 200, `{}` })
	_, err := p.CreateCheckout(context.Background(), billing.CheckoutInput{
		TenantID: uuid.New(), Plan: billing.TierStarter, CustomerEmail: "owner@example.com",
	})
	if err == nil {
		t.Fatal("a checkout with no stored customer must be refused")
	}
	if n := len(rt.seen()); n != 0 {
		t.Fatalf("requests sent = %d, want 0", n)
	}
}

func TestCreateCustomer_CarriesTenantAndApp(t *testing.T) {
	forms := &formRecorder{}
	p, _ := newRecordingProvider(func(r *http.Request) (int, string) {
		forms.record(r)
		return 200, `{"id":"cus_new","object":"customer"}`
	})
	tenant := uuid.New()
	id, err := p.CreateCustomer(context.Background(), tenant, "owner@example.com")
	if err != nil || id != "cus_new" {
		t.Fatalf("CreateCustomer = %q, %v", id, err)
	}
	f := forms.last("/v1/customers")
	if f.Get("metadata[wpmgr_tenant_id]") != tenant.String() || f.Get("metadata[app]") != "wpmgr" || f.Get("email") != "owner@example.com" {
		t.Fatalf("customer create form = %v", f)
	}
}

func TestCreatePortalSession_PassesConfiguration(t *testing.T) {
	forms := &formRecorder{}
	p, _ := newRecordingProvider(func(r *http.Request) (int, string) {
		forms.record(r)
		return 200, `{"id":"bps_1","object":"billing_portal.session","url":"https://portal.test"}`
	})
	if _, err := p.CreatePortalSession(context.Background(), "cus_A"); err != nil {
		t.Fatalf("CreatePortalSession: %v", err)
	}
	f := forms.last("/v1/billing_portal/sessions")
	if f.Get("configuration") != "bpc_wpmgr" || f.Get("customer") != "cus_A" {
		t.Fatalf("portal form = %v, want configuration=bpc_wpmgr customer=cus_A", f)
	}
}

func TestConfigured_RequiresPortalConfiguration(t *testing.T) {
	cfg := testConfig()
	cfg.PortalConfigurationID = ""
	if cfg.Configured() {
		t.Fatal("a config without the portal configuration must not be Configured")
	}
}

func TestIsTestModeKey(t *testing.T) {
	for key, want := range map[string]bool{
		"sk_test_1": true, "rk_test_1": true, "rk_live_1": false, "sk_live_1": false, "": false,
	} {
		if got := IsTestModeKey(key); got != want {
			t.Errorf("IsTestModeKey(%q) = %v, want %v", key, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// The customer-filtered list rule.
// ---------------------------------------------------------------------------

func TestListHelpers_EmptyCustomerSendsNothing(t *testing.T) {
	p, rt := newRecordingProvider(func(*http.Request) (int, string) {
		return 200, `{"object":"list","data":[],"has_more":false}`
	})
	ctx := context.Background()
	if _, err := p.listSubscriptions(ctx, ""); err == nil {
		t.Fatal("listSubscriptions with an empty customer must return an error")
	}
	if _, err := p.listOpenSessions(ctx, ""); err == nil {
		t.Fatal("listOpenSessions with an empty customer must return an error")
	}
	if _, _, err := p.PendingOrLiveSubscription(ctx, ""); err == nil {
		t.Fatal("PendingOrLiveSubscription with an empty customer must return an error")
	}
	if _, err := p.SupersedeOpenCheckoutSessions(ctx, "", "cs_x"); err == nil {
		t.Fatal("SupersedeOpenCheckoutSessions with an empty customer must return an error")
	}
	_, pending, err := p.HasPendingOrLive(ctx, "")
	if err != nil || pending {
		t.Fatalf("HasPendingOrLive(\"\") = pending %v, err %v; want nothing pending, no error", pending, err)
	}
	if n := len(rt.seen()); n != 0 {
		t.Fatalf("requests sent = %d, want 0 for an empty customer", n)
	}
}

func TestHasPendingOrLive_ExpiresSessionsThenReadsEveryPage(t *testing.T) {
	p, rt := newRecordingProvider(func(r *http.Request) (int, string) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/checkout/sessions":
			return 200, `{"object":"list","url":"/v1/checkout/sessions","has_more":false,"data":[
				{"id":"cs_a","object":"checkout.session","status":"open","customer":"cus_A"},
				{"id":"cs_foreign","object":"checkout.session","status":"open","customer":"cus_OTHER"}]}`
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/expire"):
			return 200, `{"id":"x","object":"checkout.session","status":"expired"}`
		case r.Method == http.MethodGet && r.URL.Path == "/v1/subscriptions" && r.URL.Query().Get("starting_after") == "":
			return 200, `{"object":"list","url":"/v1/subscriptions","has_more":true,"data":[
				{"id":"sub_old","object":"subscription","status":"canceled","customer":"cus_A"}]}`
		case r.Method == http.MethodGet && r.URL.Path == "/v1/subscriptions":
			return 200, `{"object":"list","url":"/v1/subscriptions","has_more":false,"data":[
				{"id":"sub_foreign","object":"subscription","status":"active","customer":"cus_OTHER"},
				{"id":"sub_inc","object":"subscription","status":"incomplete","customer":"cus_A"}]}`
		}
		return 404, `{"error":{"type":"invalid_request_error","message":"unexpected request"}}`
	})
	subID, pending, err := p.HasPendingOrLive(context.Background(), "cus_A")
	if err != nil {
		t.Fatalf("HasPendingOrLive: %v", err)
	}
	if !pending || subID != "sub_inc" {
		t.Fatalf("HasPendingOrLive = %q, %v; want sub_inc pending (found on the second page)", subID, pending)
	}
	var expired []string
	for _, r := range rt.seen() {
		if r.Method == http.MethodGet {
			if r.URL.Query().Get("customer") != "cus_A" {
				t.Fatalf("list request %s lacks customer=cus_A", r.URL.String())
			}
			if r.URL.Path == "/v1/subscriptions" && r.URL.Query().Get("status") != "all" {
				t.Fatalf("subscription list %s lacks status=all", r.URL.String())
			}
		}
		if r.Method == http.MethodPost {
			expired = append(expired, r.URL.Path)
		}
	}
	if len(expired) != 1 || !strings.Contains(expired[0], "cs_a") {
		t.Fatalf("expired = %v, want only cs_a (never another customer's session)", expired)
	}
}

func TestSubscriptionPendingOrLive_Statuses(t *testing.T) {
	live := map[string]bool{
		"active": true, "trialing": true, "past_due": true, "unpaid": true, "paused": true, "incomplete": true,
		"canceled": false, "incomplete_expired": false,
	}
	for st, want := range live {
		if got := subscriptionPendingOrLive(stripeStatus(st)); got != want {
			t.Errorf("%s: pending or live = %v, want %v", st, got, want)
		}
	}
}

// supersedeFixture answers an open-session list split over two pages and
// records the ids it was asked to expire.
func supersedeFixture(t *testing.T, page1, page2 string) (*Provider, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var expired []string
	p, _ := newRecordingProvider(func(r *http.Request) (int, string) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/checkout/sessions":
			if r.URL.Query().Get("customer") != "cus_A" {
				t.Errorf("list without customer=cus_A: %s", r.URL.String())
			}
			if r.URL.Query().Get("starting_after") == "" {
				return 200, `{"object":"list","url":"/v1/checkout/sessions","has_more":true,"data":[` + page1 + `]}`
			}
			return 200, `{"object":"list","url":"/v1/checkout/sessions","has_more":false,"data":[` + page2 + `]}`
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/expire"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/checkout/sessions/"), "/expire")
			mu.Lock()
			expired = append(expired, id)
			mu.Unlock()
			return 200, `{"id":"` + id + `","object":"checkout.session","status":"expired"}`
		}
		return 404, `{"error":{"type":"invalid_request_error","message":"unexpected request"}}`
	})
	return p, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), expired...) }
}

func sess(id string, created int64, customer string) string {
	return fmt.Sprintf(`{"id":%q,"object":"checkout.session","status":"open","created":%d,"customer":%q}`, id, created, customer)
}

func TestSupersede_GreatestOnSecondPageSurvives(t *testing.T) {
	p, expired := supersedeFixture(t,
		sess("cs_mine", 100, "cus_A")+","+sess("cs_old", 90, "cus_A"),
		sess("cs_new", 101, "cus_A")+","+sess("cs_foreign", 200, "cus_OTHER"))
	own, err := p.SupersedeOpenCheckoutSessions(context.Background(), "cus_A", "cs_mine")
	if err != nil {
		t.Fatalf("Supersede: %v", err)
	}
	if !own {
		t.Fatal("cs_mine is not the greatest, so the request's own session was superseded")
	}
	got := expired()
	if len(got) != 2 || !contains(got, "cs_mine") || !contains(got, "cs_old") {
		t.Fatalf("expired = %v, want cs_mine and cs_old; cs_new (page 2) survives, cs_foreign is never touched", got)
	}
}

func TestSupersede_SameSecondTieBreaksOnID(t *testing.T) {
	// Two sessions created in the same second: the greater id survives, in
	// whichever order the listing returns them.
	p, expired := supersedeFixture(t, sess("cs_b", 100, "cus_A"), sess("cs_a", 100, "cus_A"))
	own, err := p.SupersedeOpenCheckoutSessions(context.Background(), "cus_A", "cs_b")
	if err != nil || own {
		t.Fatalf("cs_b is the greatest: own superseded = %v, err %v", own, err)
	}
	if got := expired(); len(got) != 1 || got[0] != "cs_a" {
		t.Fatalf("expired = %v, want [cs_a]", got)
	}
	p2, expired2 := supersedeFixture(t, sess("cs_b", 100, "cus_A"), sess("cs_a", 100, "cus_A"))
	own, err = p2.SupersedeOpenCheckoutSessions(context.Background(), "cus_A", "cs_a")
	if err != nil || !own {
		t.Fatalf("cs_a is not the greatest: own superseded = %v, err %v", own, err)
	}
	if got := expired2(); len(got) != 1 || got[0] != "cs_a" {
		t.Fatalf("expired = %v, want [cs_a]", got)
	}
}

func stripeStatus(s string) stripesdk.SubscriptionStatus { return stripesdk.SubscriptionStatus(s) }

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
