package stripe

// ownership_test.go — shared-account isolation: which verified events count
// as WPMgr's, what a completed session contributes to the ledger payload, and
// the customer-filtered session expiry.

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/billing"
)

func verify(t *testing.T, p *Provider, id, evType, obj string) billing.Event {
	t.Helper()
	body := buildEvent(id, evType, 1700000000, obj)
	ev, err := p.VerifyWebhook(body, sign(t, body, testConfig().WebhookSecret))
	if err != nil {
		t.Fatalf("VerifyWebhook(%s): %v", evType, err)
	}
	return ev
}

func TestOwnershipZeroValueIsOwned(t *testing.T) {
	var ev billing.Event
	if ev.Ownership != billing.OwnershipOwned {
		t.Fatalf("zero Event.Ownership = %d, want OwnershipOwned (%d)", ev.Ownership, billing.OwnershipOwned)
	}
	if billing.OwnershipOwned != 0 {
		t.Fatalf("OwnershipOwned = %d, want the zero value", billing.OwnershipOwned)
	}
}

func TestVerifyWebhook_ForeignEvents(t *testing.T) {
	p := New(testConfig())
	otherTenant := uuid.NewString()
	cases := []struct {
		name, evType, obj string
	}{
		{
			// Another product's one-off checkout: payment mode, no metadata,
			// but a UUID-shaped client_reference_id, which must not count.
			name:   "other product checkout session",
			evType: "checkout.session.completed",
			obj:    `{"id":"cs_other","object":"checkout.session","mode":"payment","customer":"cus_other","client_reference_id":"` + otherTenant + `"}`,
		},
		{
			name:   "subscription-mode session without WPMgr metadata",
			evType: "checkout.session.completed",
			obj:    `{"id":"cs_other2","object":"checkout.session","mode":"subscription","customer":"cus_other","subscription":"sub_other","client_reference_id":"` + otherTenant + `"}`,
		},
		{
			name:   "charge refunded",
			evType: "charge.refunded",
			obj:    `{"id":"ch_1","object":"charge","customer":"cus_111"}`,
		},
		{
			name:   "invoice paid with no WPMgr metadata",
			evType: "invoice.paid",
			obj:    `{"id":"in_other","object":"invoice","customer":"cus_other","parent":{"type":"subscription_details","subscription_details":{"subscription":"sub_other","metadata":{"app":"other"}}}}`,
		},
		{
			name:   "subscription with another product's price",
			evType: "customer.subscription.updated",
			obj:    `{"id":"sub_other","object":"subscription","status":"active","customer":"cus_other","items":{"object":"list","data":[{"id":"si_1","price":{"id":"price_other"}}]}}`,
		},
		{
			name:   "customer created",
			evType: "customer.created",
			obj:    `{"id":"cus_new","object":"customer"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := verify(t, p, "evt_"+strings.ReplaceAll(tc.name, " ", "_"), tc.evType, tc.obj)
			if ev.Ownership != billing.OwnershipForeign {
				t.Fatalf("Ownership = %d, want OwnershipForeign", ev.Ownership)
			}
			if ev.Handled {
				t.Fatal("a foreign event must not be Handled")
			}
			if ev.TenantID != uuid.Nil {
				t.Fatalf("a foreign event must carry no tenant claim, got %s", ev.TenantID)
			}
		})
	}
}

func TestVerifyWebhook_OwnedEvents(t *testing.T) {
	p := New(testConfig())
	tenant := uuid.NewString()
	cases := []struct {
		name, evType, obj string
		handled           bool
	}{
		{
			name:    "session with app metadata",
			evType:  "checkout.session.completed",
			obj:     `{"id":"cs_1","object":"checkout.session","mode":"subscription","customer":"cus_1","subscription":"sub_1","metadata":{"app":"wpmgr"}}`,
			handled: true,
		},
		{
			name:    "subscription identified only by a WPMgr price",
			evType:  "customer.subscription.deleted",
			obj:     `{"id":"sub_2","object":"subscription","status":"canceled","customer":"cus_2","items":{"object":"list","data":[{"id":"si_2","price":{"id":"price_agency"}}]}}`,
			handled: true,
		},
		{
			name:    "invoice with tenant metadata",
			evType:  "invoice.paid",
			obj:     `{"id":"in_3","object":"invoice","customer":"cus_3","parent":{"type":"subscription_details","subscription_details":{"subscription":"sub_3","metadata":{"wpmgr_tenant_id":"` + tenant + `"}}}}`,
			handled: true,
		},
		{
			// Recorded and marked processed, never applied.
			name:    "unhandled invoice type with WPMgr metadata",
			evType:  "invoice.payment_action_required",
			obj:     `{"id":"in_4","object":"invoice","customer":"cus_4","parent":{"type":"subscription_details","subscription_details":{"subscription":"sub_4","metadata":{"app":"wpmgr"}}}}`,
			handled: false,
		},
		{
			name:    "expired WPMgr session",
			evType:  "checkout.session.expired",
			obj:     `{"id":"cs_5","object":"checkout.session","mode":"subscription","metadata":{"wpmgr_tenant_id":"` + tenant + `"}}`,
			handled: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := verify(t, p, "evt_"+strings.ReplaceAll(tc.name, " ", "_"), tc.evType, tc.obj)
			if ev.Ownership != billing.OwnershipOwned {
				t.Fatalf("Ownership = %d, want OwnershipOwned", ev.Ownership)
			}
			if ev.Handled != tc.handled {
				t.Fatalf("Handled = %t, want %t", ev.Handled, tc.handled)
			}
		})
	}
}

func TestVerifyWebhook_TaxIDUpdatedIsByCustomer(t *testing.T) {
	p := New(testConfig())
	ev := verify(t, p, "evt_taxid", "customer.tax_id.updated",
		`{"id":"txi_1","object":"tax_id","customer":"cus_tax","type":"eu_vat","value":"DE123456789","verification":{"status":"unverified"}}`)
	if ev.Ownership != billing.OwnershipByCustomer {
		t.Fatalf("Ownership = %d, want OwnershipByCustomer", ev.Ownership)
	}
	if !ev.Handled || ev.Kind != billing.EventTaxIDUpdated {
		t.Fatalf("Handled=%t Kind=%q, want true/%q", ev.Handled, ev.Kind, billing.EventTaxIDUpdated)
	}
	if ev.ProviderCustomerID != "cus_tax" || ev.TaxIDType != "eu_vat" || ev.TaxIDVerificationStatus != "unverified" {
		t.Fatalf("got customer=%q type=%q status=%q", ev.ProviderCustomerID, ev.TaxIDType, ev.TaxIDVerificationStatus)
	}
}

func TestVerifyWebhook_CheckoutSessionTaxDetails(t *testing.T) {
	p := New(testConfig())
	tenant := uuid.NewString()
	ev := verify(t, p, "evt_tax_details", "checkout.session.completed", `{
		"id":"cs_tax","object":"checkout.session","mode":"subscription",
		"customer":"cus_tax","subscription":"sub_tax",
		"metadata":{"wpmgr_tenant_id":"`+tenant+`"},
		"customer_details":{
			"address":{"country":"DE"},
			"tax_ids":[{"type":"eu_vat","value":"DE123456789"},{"type":"es_cif","value":"B12345678"}]
		}
	}`)
	if got := strings.Join(ev.TaxIDTypes, ","); got != "eu_vat,es_cif" {
		t.Fatalf("TaxIDTypes = %q, want eu_vat,es_cif", got)
	}
	if ev.BillingCountry != "DE" {
		t.Fatalf("BillingCountry = %q, want DE", ev.BillingCountry)
	}

	none := verify(t, p, "evt_tax_none", "checkout.session.completed",
		`{"id":"cs_none","object":"checkout.session","mode":"subscription","customer":"cus_n","subscription":"sub_n","metadata":{"wpmgr_tenant_id":"`+tenant+`"}}`)
	if none.TaxIDTypes == nil || len(none.TaxIDTypes) != 0 || none.BillingCountry != "" {
		t.Fatalf("no customer_details: TaxIDTypes=%#v BillingCountry=%q, want an empty non-nil slice and \"\"", none.TaxIDTypes, none.BillingCountry)
	}
}

// ---------------------------------------------------------------------------
// ExpireOpenCheckoutSessions: the customer-filtered list rule.
// ---------------------------------------------------------------------------

// recordingTransport answers Stripe API requests from canned bodies and
// records every request it saw.
type recordingTransport struct {
	mu       sync.Mutex
	requests []*http.Request
	respond  func(r *http.Request) (int, string)
}

func (rt *recordingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rt.mu.Lock()
	rt.requests = append(rt.requests, r)
	rt.mu.Unlock()
	status, body := rt.respond(r)
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewBufferString(body)),
		Request:    r,
	}, nil
}

func (rt *recordingTransport) seen() []*http.Request {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return append([]*http.Request(nil), rt.requests...)
}

func newRecordingProvider(respond func(r *http.Request) (int, string)) (*Provider, *recordingTransport) {
	rt := &recordingTransport{respond: respond}
	cfg := testConfig()
	cfg.HTTPClient = &http.Client{Transport: rt}
	return New(cfg), rt
}

func TestExpireOpenCheckoutSessions_EmptyCustomerSendsNothing(t *testing.T) {
	p, rt := newRecordingProvider(func(*http.Request) (int, string) {
		return 200, `{"object":"list","data":[],"has_more":false}`
	})
	n, err := p.ExpireOpenCheckoutSessions(context.Background(), "")
	if err == nil {
		t.Fatal("an empty customer must be refused with an error")
	}
	if n != 0 {
		t.Fatalf("expired = %d, want 0", n)
	}
	if got := len(rt.seen()); got != 0 {
		t.Fatalf("requests sent = %d, want 0 for an empty customer", got)
	}
}

func TestExpireOpenCheckoutSessions_FiltersPagesAndSkipsOtherCustomers(t *testing.T) {
	p, rt := newRecordingProvider(func(r *http.Request) (int, string) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/checkout/sessions" && r.URL.Query().Get("starting_after") == "":
			return 200, `{"object":"list","url":"/v1/checkout/sessions","has_more":true,"data":[
				{"id":"cs_a1","object":"checkout.session","status":"open","customer":"cus_A"},
				{"id":"cs_other","object":"checkout.session","status":"open","customer":"cus_OTHER"}]}`
		case r.Method == http.MethodGet && r.URL.Path == "/v1/checkout/sessions":
			return 200, `{"object":"list","url":"/v1/checkout/sessions","has_more":false,"data":[
				{"id":"cs_a2","object":"checkout.session","status":"open","customer":"cus_A"}]}`
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/expire"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/checkout/sessions/"), "/expire")
			return 200, `{"id":"` + id + `","object":"checkout.session","status":"expired","customer":"cus_A"}`
		}
		return 404, `{"error":{"type":"invalid_request_error","message":"unexpected request"}}`
	})

	n, err := p.ExpireOpenCheckoutSessions(context.Background(), "cus_A")
	if err != nil {
		t.Fatalf("ExpireOpenCheckoutSessions: %v", err)
	}
	if n != 2 {
		t.Fatalf("expired = %d, want 2 (one per page)", n)
	}

	var lists, expires []string
	for _, r := range rt.seen() {
		switch r.Method {
		case http.MethodGet:
			if got := r.URL.Query().Get("customer"); got != "cus_A" {
				t.Fatalf("list request %s carries customer=%q, want cus_A", r.URL.String(), got)
			}
			lists = append(lists, r.URL.Query().Get("starting_after"))
		case http.MethodPost:
			expires = append(expires, r.URL.Path)
		}
	}
	if len(lists) != 2 {
		t.Fatalf("list requests = %d (%v), want 2: the second page must be followed", len(lists), lists)
	}
	for _, path := range expires {
		if strings.Contains(path, "cs_other") {
			t.Fatalf("a session of another customer was expired: %s", path)
		}
	}
	if len(expires) != 2 {
		t.Fatalf("expire requests = %v, want cs_a1 and cs_a2", expires)
	}
}
