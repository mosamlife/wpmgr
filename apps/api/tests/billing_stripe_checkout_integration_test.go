package tests

// billing_stripe_checkout_integration_test.go — the checkout binding, the
// switch rule, the one-subscription rule and confirm, driven through
// billing.Service.CreateCheckout / ConfirmCheckout with the REAL Stripe
// adapter pointed at an in-memory Stripe account (fakeStripeAccount), and the
// database reached through the service's own pool as wpmgr_app.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/billing"
	billingstripe "github.com/mosamlife/wpmgr/apps/api/internal/billing/stripe"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

type fakeStripeSession struct {
	ID, Customer, Status, Mode, ClientRef, Subscription string
	Created                                             int64
}

type fakeStripeSub struct {
	ID, Customer, Status string
}

// fakeStripeAccount is a stateful stand-in for one Stripe account, shared by
// several products. It answers the calls the adapter makes and records every
// request. A list without a customer filter returns every object on the
// account, as the real API does.
type fakeStripeAccount struct {
	mu        sync.Mutex
	next      int
	clock     int64
	pageSize  int
	customers map[string]map[string]string
	sessions  []*fakeStripeSession
	subs      []*fakeStripeSub
	requests  []string
}

func newFakeStripeAccount() *fakeStripeAccount {
	return &fakeStripeAccount{clock: 1_800_000_000, pageSize: 2, customers: map[string]map[string]string{}}
}

func (f *fakeStripeAccount) id(prefix string) string {
	f.next++
	return fmt.Sprintf("%s_%04d", prefix, f.next)
}

// addSession seeds an open session (a foreign one has no customer).
func (f *fakeStripeAccount) addSession(customer, mode string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := &fakeStripeSession{ID: f.id("cs"), Customer: customer, Status: "open", Mode: mode, Created: f.clock}
	f.sessions = append(f.sessions, s)
	return s.ID
}

func (f *fakeStripeAccount) addSub(customer, status string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := &fakeStripeSub{ID: f.id("sub"), Customer: customer, Status: status}
	f.subs = append(f.subs, s)
	return s.ID
}

func (f *fakeStripeAccount) session(id string) *fakeStripeSession {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.sessions {
		if s.ID == id {
			return s
		}
	}
	return nil
}

func (f *fakeStripeAccount) openSessions(customer string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, s := range f.sessions {
		if s.Status == "open" && s.Customer == customer {
			out = append(out, s.ID)
		}
	}
	return out
}

func (f *fakeStripeAccount) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

func (f *fakeStripeAccount) countRequests(substr string) int {
	n := 0
	for _, r := range f.seen() {
		if strings.Contains(r, substr) {
			n++
		}
	}
	return n
}

func sessionJSON(s *fakeStripeSession) map[string]any {
	out := map[string]any{
		"id": s.ID, "object": "checkout.session", "status": s.Status, "created": s.Created,
		"mode": s.Mode, "client_reference_id": s.ClientRef, "url": "https://checkout.test/" + s.ID,
	}
	if s.Customer != "" {
		out["customer"] = s.Customer
	}
	if s.Subscription != "" {
		out["subscription"] = s.Subscription
	}
	return out
}

func (f *fakeStripeAccount) page(items []map[string]any, q url.Values, path string) map[string]any {
	start := 0
	if after := q.Get("starting_after"); after != "" {
		for i, it := range items {
			if it["id"] == after {
				start = i + 1
			}
		}
	}
	end := start + f.pageSize
	if end > len(items) {
		end = len(items)
	}
	return map[string]any{"object": "list", "url": path, "data": items[start:end], "has_more": end < len(items)}
}

func (f *fakeStripeAccount) RoundTrip(r *http.Request) (*http.Response, error) {
	var form url.Values
	if r.Body != nil {
		b, _ := io.ReadAll(r.Body)
		form, _ = url.ParseQuery(string(b))
	}
	f.mu.Lock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
	status, body := f.handle(r, form)
	f.mu.Unlock()
	raw, _ := json.Marshal(body)
	return &http.Response{
		StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(bytes.NewReader(raw)), Request: r,
	}, nil
}

// handle runs under f.mu.
func (f *fakeStripeAccount) handle(r *http.Request, form url.Values) (int, any) {
	q := r.URL.Query()
	p := r.URL.Path
	switch {
	case r.Method == http.MethodPost && p == "/v1/customers":
		id := f.id("cus")
		f.customers[id] = map[string]string{
			"wpmgr_tenant_id": form.Get("metadata[wpmgr_tenant_id]"), "app": form.Get("metadata[app]"),
		}
		return 200, map[string]any{"id": id, "object": "customer"}
	case r.Method == http.MethodPost && p == "/v1/checkout/sessions":
		s := &fakeStripeSession{
			ID: f.id("cs"), Customer: form.Get("customer"), Status: "open",
			Mode: form.Get("mode"), ClientRef: form.Get("client_reference_id"), Created: f.clock,
		}
		f.sessions = append(f.sessions, s)
		return 200, sessionJSON(s)
	case r.Method == http.MethodGet && p == "/v1/checkout/sessions":
		var items []*fakeStripeSession
		for _, s := range f.sessions {
			if (q.Get("customer") == "" || s.Customer == q.Get("customer")) && (q.Get("status") == "" || s.Status == q.Get("status")) {
				items = append(items, s)
			}
		}
		sort.Slice(items, func(i, j int) bool {
			if items[i].Created != items[j].Created {
				return items[i].Created > items[j].Created
			}
			return items[i].ID > items[j].ID
		})
		var data []map[string]any
		for _, s := range items {
			data = append(data, sessionJSON(s))
		}
		return 200, f.page(data, q, p)
	case r.Method == http.MethodPost && strings.HasPrefix(p, "/v1/checkout/sessions/") && strings.HasSuffix(p, "/expire"):
		id := strings.TrimSuffix(strings.TrimPrefix(p, "/v1/checkout/sessions/"), "/expire")
		for _, s := range f.sessions {
			if s.ID == id {
				s.Status = "expired"
				return 200, sessionJSON(s)
			}
		}
		return 404, map[string]any{"error": map[string]any{"type": "invalid_request_error", "message": "no such session"}}
	case r.Method == http.MethodGet && strings.HasPrefix(p, "/v1/checkout/sessions/"):
		id := strings.TrimPrefix(p, "/v1/checkout/sessions/")
		for _, s := range f.sessions {
			if s.ID == id {
				return 200, sessionJSON(s)
			}
		}
		return 404, map[string]any{"error": map[string]any{"type": "invalid_request_error", "message": "no such session"}}
	case r.Method == http.MethodGet && p == "/v1/subscriptions":
		var data []map[string]any
		for _, s := range f.subs {
			if q.Get("customer") == "" || s.Customer == q.Get("customer") {
				data = append(data, map[string]any{"id": s.ID, "object": "subscription", "status": s.Status, "customer": s.Customer})
			}
		}
		return 200, f.page(data, q, p)
	}
	return 404, map[string]any{"error": map[string]any{"type": "invalid_request_error", "message": "unexpected " + r.Method + " " + p}}
}

// stripeCheckoutRig is one Service with the real Stripe adapter over a fake
// account and a Razorpay-named fake, plus River for request-path refreshes.
type stripeCheckoutRig struct {
	h       *billingHarness
	acct    *fakeStripeAccount
	rzp     *fakeProvider
	pool    *db.Pool
	tenant  uuid.UUID
	actor   billing.Actor
	baseCtx context.Context
}

func newStripeCheckoutRig(t *testing.T, slug string) *stripeCheckoutRig {
	t.Helper()
	pool := startPostgres(t)
	acct := newFakeStripeAccount()
	sp := billingstripe.New(billingstripe.Config{
		SecretKey: "rk_test_x", WebhookSecret: "whsec_x",
		PriceStarter: "price_starter", PriceAgency: "price_agency", PriceScale: "price_scale",
		PortalConfigurationID: "bpc_wpmgr",
		HTTPClient:            &http.Client{Transport: acct},
	})
	rzp := newFakeProvider("razorpay")
	h := newBillingHarness(t, pool, sp, rzp)
	return &stripeCheckoutRig{
		h: h, acct: acct, rzp: rzp, pool: pool, tenant: seedTenant(t, pool, slug),
		actor: billing.Actor{Type: "user", ID: "user-1"}, baseCtx: context.Background(),
	}
}

func (r *stripeCheckoutRig) checkout(provider string) (billing.CheckoutSession, error) {
	return r.h.svc.CreateCheckout(r.baseCtx, r.tenant, billing.TierAgency, provider, "", "owner@example.com",
		"https://cp.test/billing?checkout=success", "https://cp.test/billing?checkout=cancel", r.actor)
}

// pin reads the tenant's stored provider, customer and subscription ids.
func (r *stripeCheckoutRig) pin(t *testing.T) (provider, customer, sub string) {
	t.Helper()
	var p, c, s *string
	if err := r.pool.QueryRow(r.baseCtx,
		`SELECT billing_provider, provider_customer_id, provider_subscription_id FROM tenants WHERE id = $1`,
		r.tenant).Scan(&p, &c, &s); err != nil {
		t.Fatalf("read pin: %v", err)
	}
	deref := func(v *string) string {
		if v == nil {
			return ""
		}
		return *v
	}
	return deref(p), deref(c), deref(s)
}

func (r *stripeCheckoutRig) setPin(t *testing.T, provider, customer, sub, status string) {
	t.Helper()
	nul := func(v string) any {
		if v == "" {
			return nil
		}
		return v
	}
	if _, err := r.pool.Exec(r.baseCtx,
		`UPDATE tenants SET billing_provider = $1, provider_customer_id = $2, provider_subscription_id = $3, plan_status = $4 WHERE id = $5`,
		nul(provider), nul(customer), nul(sub), status, r.tenant); err != nil {
		t.Fatalf("setPin: %v", err)
	}
}

func assertConflictCode(t *testing.T, err error, code string) *domain.Error {
	t.Helper()
	de, ok := domain.AsDomain(err)
	if !ok || de.Kind != domain.KindConflict || de.Code != code {
		t.Fatalf("err = %v, want 409 %s", err, code)
	}
	return de
}

func assertProviderLocked(t *testing.T, err error, reason string) {
	t.Helper()
	de := assertConflictCode(t, err, "billing_provider_locked")
	if got, _ := de.Details["reason"].(string); got != reason {
		t.Fatalf("details.reason = %q, want %q (details %v)", got, reason, de.Details)
	}
}

// ---------------------------------------------------------------------------
// Switch matrix.
// ---------------------------------------------------------------------------

func TestStripeCheckout_SwitchToRazorpayExpiresOpenSessionThenMovesPin(t *testing.T) {
	r := newStripeCheckoutRig(t, "switch-open-session")
	r.setPin(t, "stripe", "cus_A", "", "canceled")
	open := r.acct.addSession("cus_A", "subscription")

	if _, err := r.checkout("razorpay"); err != nil {
		t.Fatalf("switch checkout: %v", err)
	}
	if s := r.acct.session(open); s.Status != "expired" {
		t.Fatalf("cus_A's open session is %q, want expired before the pin moved", s.Status)
	}
	if p, c, s := r.pin(t); p != "razorpay" || c != "" || s != "" {
		t.Fatalf("pin = (%q, %q, %q), want (razorpay, '', '')", p, c, s)
	}
}

func TestStripeCheckout_SwitchWithNullCustomerSendsNoStripeRequest(t *testing.T) {
	r := newStripeCheckoutRig(t, "switch-null-customer")
	r.setPin(t, "stripe", "", "", "none")
	foreign := r.acct.addSession("", "payment") // another product's session, no customer

	if _, err := r.checkout("razorpay"); err != nil {
		t.Fatalf("switch checkout: %v", err)
	}
	if s := r.acct.session(foreign); s.Status != "open" {
		t.Fatalf("the other product's session is %q, want it left open", s.Status)
	}
	if got := r.acct.seen(); len(got) != 0 {
		t.Fatalf("Stripe requests = %v, want none for a Stripe pin with no customer", got)
	}
	if p, _, _ := r.pin(t); p != "razorpay" {
		t.Fatalf("pin = %q, want razorpay", p)
	}
}

func TestStripeCheckout_SwitchRefusals(t *testing.T) {
	t.Run("pending or live at Stripe", func(t *testing.T) {
		for _, st := range []string{"active", "incomplete"} {
			r := newStripeCheckoutRig(t, "switch-refuse-"+st)
			r.setPin(t, "stripe", "cus_A", "", "canceled")
			r.acct.addSub("cus_A", st)
			_, err := r.checkout("razorpay")
			assertProviderLocked(t, err, billing.ReasonPendingAtProvider)
			if p, c, _ := r.pin(t); p != "stripe" || c != "cus_A" {
				t.Fatalf("%s: pin moved to (%q, %q)", st, p, c)
			}
		}
	})
	t.Run("comped", func(t *testing.T) {
		r := newStripeCheckoutRig(t, "switch-refuse-comped")
		r.setPin(t, "stripe", "cus_A", "", "comped")
		_, err := r.checkout("razorpay")
		assertConflictCode(t, err, "billing_comped")
	})
	t.Run("none with a subscription id", func(t *testing.T) {
		r := newStripeCheckoutRig(t, "switch-refuse-none-with-id")
		r.setPin(t, "stripe", "cus_A", "sub_X", "none")
		_, err := r.checkout("razorpay")
		assertConflictCode(t, err, "billing_subscription_pending")
		if n := countBillingJobs(t, r.pool, "billing_refresh"); n != 1 {
			t.Fatalf("billing_refresh jobs = %d, want 1 enqueued to settle the stored subscription", n)
		}
	})
	t.Run("past_due", func(t *testing.T) {
		r := newStripeCheckoutRig(t, "switch-refuse-past-due")
		r.setPin(t, "stripe", "cus_A", "sub_X", "past_due")
		_, err := r.checkout("razorpay")
		assertConflictCode(t, err, "billing_subscription_exists")
	})
	t.Run("razorpay to stripe needs support", func(t *testing.T) {
		r := newStripeCheckoutRig(t, "switch-refuse-rzp")
		r.setPin(t, "razorpay", "", "", "canceled")
		_, err := r.checkout("stripe")
		assertProviderLocked(t, err, billing.ReasonNeedsSupport)
		if n := r.acct.countRequests("/v1/customers"); n != 0 {
			t.Fatalf("customer create requests = %d, want 0 before the switch is allowed", n)
		}
	})
}

func TestStripeCheckout_SameProviderKeepsCustomerAndSubscription(t *testing.T) {
	r := newStripeCheckoutRig(t, "same-provider-keep")
	r.setPin(t, "stripe", "cus_KEEP", "sub_OLD", "canceled")
	if _, err := r.checkout("stripe"); err != nil {
		t.Fatalf("checkout: %v", err)
	}
	if p, c, s := r.pin(t); p != "stripe" || c != "cus_KEEP" || s != "sub_OLD" {
		t.Fatalf("pin = (%q, %q, %q), want (stripe, cus_KEEP, sub_OLD)", p, c, s)
	}
	if n := r.acct.countRequests("POST /v1/customers"); n != 0 {
		t.Fatalf("customer create requests = %d, want 0 when one is stored", n)
	}
	if open := r.acct.openSessions("cus_KEEP"); len(open) != 1 {
		t.Fatalf("open sessions on cus_KEEP = %v, want exactly one", open)
	}
}

func TestStripeCheckout_FirstCheckoutPreCreatesWPMgrCustomer(t *testing.T) {
	r := newStripeCheckoutRig(t, "first-checkout-customer")
	if _, err := r.checkout(""); err != nil {
		t.Fatalf("checkout: %v", err)
	}
	p, c, _ := r.pin(t)
	if p != "stripe" || c == "" {
		t.Fatalf("pin = (%q, %q), want stripe with the pre-created customer", p, c)
	}
	r.acct.mu.Lock()
	meta := r.acct.customers[c]
	r.acct.mu.Unlock()
	if meta["wpmgr_tenant_id"] != r.tenant.String() || meta["app"] != "wpmgr" {
		t.Fatalf("customer %s metadata = %v, want tenant id and app=wpmgr", c, meta)
	}
	if open := r.acct.openSessions(c); len(open) != 1 {
		t.Fatalf("open sessions on %s = %v, want exactly one created on the stored customer", c, open)
	}
}

// ---------------------------------------------------------------------------
// Races, through the seams between the checks and the locked bind.
// ---------------------------------------------------------------------------

func TestStripeCheckout_Race_NullPinStripeVsRazorpay(t *testing.T) {
	for _, first := range []string{"stripe", "razorpay"} {
		t.Run(first+" binds first", func(t *testing.T) {
			r := newStripeCheckoutRig(t, "race-null-"+first)
			second := map[string]string{"stripe": "razorpay", "razorpay": "stripe"}[first]
			var innerErr error
			var fired bool
			r.h.svc.SetCheckoutTestHooks(billing.CheckoutTestHooks{BeforeBind: func() {
				if fired {
					return
				}
				fired = true
				r.h.svc.SetCheckoutTestHooks(billing.CheckoutTestHooks{})
				_, innerErr = r.checkout(first)
			}})
			_, err := r.checkout(second)
			if innerErr != nil {
				t.Fatalf("the first checkout (%s) failed: %v", first, innerErr)
			}
			assertProviderLocked(t, err, billing.ReasonProviderChanged)
			if p, _, _ := r.pin(t); p != first {
				t.Fatalf("pin = %q, want %q", p, first)
			}
			if second == "stripe" && r.acct.countRequests("POST /v1/checkout/sessions?") != 0 {
				t.Fatal("the refused Stripe checkout created a session")
			}
			if second == "razorpay" && r.rzp.lastCheckoutInput.TenantID == r.tenant {
				t.Fatal("the refused Razorpay checkout created a subscription")
			}
		})
	}
}

func TestStripeCheckout_Race_DoubleClickFromNullPinBothBind(t *testing.T) {
	r := newStripeCheckoutRig(t, "race-double-click")
	var innerErr error
	var fired bool
	r.h.svc.SetCheckoutTestHooks(billing.CheckoutTestHooks{BeforeBind: func() {
		if fired {
			return
		}
		fired = true
		r.h.svc.SetCheckoutTestHooks(billing.CheckoutTestHooks{})
		_, innerErr = r.checkout("stripe")
	}})
	_, outerErr := r.checkout("stripe")
	if innerErr != nil || outerErr != nil {
		t.Fatalf("double click: inner %v, outer %v; want both to bind", innerErr, outerErr)
	}
	_, c, _ := r.pin(t)
	if open := r.acct.openSessions(c); len(open) != 1 {
		t.Fatalf("open sessions on the stored customer %s = %v, want exactly one", c, open)
	}
	r.acct.mu.Lock()
	var onOther int
	for _, s := range r.acct.sessions {
		if s.Customer != c {
			onOther++
		}
	}
	r.acct.mu.Unlock()
	if onOther != 0 {
		t.Fatalf("%d sessions were created on a customer other than the stored one", onOther)
	}
}

func TestStripeCheckout_Race_SwitchCommitsBeforeStripeTabBinds(t *testing.T) {
	r := newStripeCheckoutRig(t, "race-switch-before-bind")
	r.setPin(t, "stripe", "cus_A", "", "canceled")
	var innerErr error
	var fired bool
	r.h.svc.SetCheckoutTestHooks(billing.CheckoutTestHooks{BeforeBind: func() {
		if fired {
			return
		}
		fired = true
		r.h.svc.SetCheckoutTestHooks(billing.CheckoutTestHooks{})
		_, innerErr = r.checkout("razorpay")
	}})
	_, err := r.checkout("stripe")
	if innerErr != nil {
		t.Fatalf("the switch failed: %v", innerErr)
	}
	assertProviderLocked(t, err, billing.ReasonProviderChanged)
	if n := r.acct.countRequests("POST /v1/checkout/sessions?"); n != 0 {
		t.Fatalf("the refused Stripe tab created %d sessions", n)
	}
}

func TestStripeCheckout_Race_SwitchAfterStripeTabBinds(t *testing.T) {
	for _, when := range []string{"before re-read", "after re-read"} {
		t.Run(when, func(t *testing.T) {
			r := newStripeCheckoutRig(t, "race-k5-"+strings.ReplaceAll(when, " ", "-"))
			r.setPin(t, "stripe", "cus_A", "", "canceled")
			var innerErr error
			var fired bool
			doSwitch := func() {
				if fired {
					return
				}
				fired = true
				r.h.svc.SetCheckoutTestHooks(billing.CheckoutTestHooks{})
				_, innerErr = r.checkout("razorpay")
			}
			if when == "before re-read" {
				r.h.svc.SetCheckoutTestHooks(billing.CheckoutTestHooks{AfterCreate: doSwitch})
			} else {
				r.h.svc.SetCheckoutTestHooks(billing.CheckoutTestHooks{AfterReread: doSwitch})
			}
			_, err := r.checkout("stripe")
			if innerErr != nil {
				t.Fatalf("the switch failed: %v", innerErr)
			}
			if when == "before re-read" {
				assertProviderLocked(t, err, billing.ReasonProviderChanged)
			}
			if open := r.acct.openSessions("cus_A"); len(open) != 0 {
				t.Fatalf("open sessions on cus_A = %v, want none after the switch", open)
			}
			if p, _, _ := r.pin(t); p != "razorpay" {
				t.Fatalf("pin = %q, want razorpay", p)
			}
		})
	}
}

// TestStripeCheckout_PostCommitSweepExpiresLateSession: a session created on
// the previous customer after the switch's provider check, and before its
// bind commits, is expired by the sweep that runs after the commit.
func TestStripeCheckout_PostCommitSweepExpiresLateSession(t *testing.T) {
	r := newStripeCheckoutRig(t, "switch-post-commit-sweep")
	r.setPin(t, "stripe", "cus_A", "", "canceled")
	var late string
	r.h.svc.SetCheckoutTestHooks(billing.CheckoutTestHooks{BeforeBind: func() {
		r.h.svc.SetCheckoutTestHooks(billing.CheckoutTestHooks{})
		late = r.acct.addSession("cus_A", "subscription")
	}})
	if _, err := r.checkout("razorpay"); err != nil {
		t.Fatalf("switch: %v", err)
	}
	if s := r.acct.session(late); s.Status != "expired" {
		t.Fatalf("the late session is %q, want expired by the post-commit sweep", s.Status)
	}
}

func TestStripeCheckout_Race_SwitchFromCheckedCustomerOnly(t *testing.T) {
	t.Run("re-pinned to a new customer", func(t *testing.T) {
		r := newStripeCheckoutRig(t, "race-cas-new-customer")
		r.setPin(t, "stripe", "cus_A", "", "canceled")
		r.h.svc.SetCheckoutTestHooks(billing.CheckoutTestHooks{BeforeBind: func() {
			r.h.svc.SetCheckoutTestHooks(billing.CheckoutTestHooks{})
			r.setPin(t, "stripe", "cus_B", "", "canceled")
		}})
		_, err := r.checkout("razorpay")
		assertProviderLocked(t, err, billing.ReasonProviderChanged)
		if p, c, _ := r.pin(t); p != "stripe" || c != "cus_B" {
			t.Fatalf("pin = (%q, %q), want cus_B untouched", p, c)
		}
	})
	t.Run("null customer filled by a same-provider tab", func(t *testing.T) {
		r := newStripeCheckoutRig(t, "race-cas-null-filled")
		r.setPin(t, "stripe", "", "", "none")
		r.h.svc.SetCheckoutTestHooks(billing.CheckoutTestHooks{BeforeBind: func() {
			r.h.svc.SetCheckoutTestHooks(billing.CheckoutTestHooks{})
			r.setPin(t, "stripe", "cus_B", "", "none")
		}})
		_, err := r.checkout("razorpay")
		assertProviderLocked(t, err, billing.ReasonProviderChanged)
		if p, c, _ := r.pin(t); p != "stripe" || c != "cus_B" {
			t.Fatalf("pin = (%q, %q), want cus_B untouched", p, c)
		}
	})
	t.Run("state changed under the lock gives the step-2 code", func(t *testing.T) {
		r := newStripeCheckoutRig(t, "race-cas-status")
		r.setPin(t, "stripe", "cus_A", "", "canceled")
		r.h.svc.SetCheckoutTestHooks(billing.CheckoutTestHooks{BeforeBind: func() {
			r.h.svc.SetCheckoutTestHooks(billing.CheckoutTestHooks{})
			r.setPin(t, "stripe", "cus_A", "sub_LIVE", "active")
		}})
		_, err := r.checkout("stripe")
		assertConflictCode(t, err, "billing_subscription_exists")
	})
}

// ---------------------------------------------------------------------------
// One subscription per customer.
// ---------------------------------------------------------------------------

func TestStripeCheckout_LiveSubscriptionAtStripeIsAdopted(t *testing.T) {
	r := newStripeCheckoutRig(t, "one-sub-adopt")
	r.setPin(t, "stripe", "cus_A", "", "none")
	r.acct.addSub("cus_A", "active")
	_, err := r.checkout("stripe")
	assertConflictCode(t, err, "billing_subscription_pending")
	if n := countBillingJobs(t, r.pool, "billing_refresh"); n != 1 {
		t.Fatalf("billing_refresh jobs = %d, want 1", n)
	}
	if n := r.acct.countRequests("POST /v1/checkout/sessions?"); n != 0 {
		t.Fatalf("a session was created while a subscription was live (%d)", n)
	}
}

func TestStripeCheckout_OlderSessionIsExpired(t *testing.T) {
	r := newStripeCheckoutRig(t, "one-sub-older-expired")
	r.setPin(t, "stripe", "cus_A", "", "none")
	old := r.acct.addSession("cus_A", "subscription")
	r.acct.mu.Lock()
	r.acct.clock++
	r.acct.mu.Unlock()
	if _, err := r.checkout("stripe"); err != nil {
		t.Fatalf("checkout: %v", err)
	}
	if s := r.acct.session(old); s.Status != "expired" {
		t.Fatalf("older session is %q, want expired", s.Status)
	}
	if open := r.acct.openSessions("cus_A"); len(open) != 1 {
		t.Fatalf("open sessions = %v, want one", open)
	}
}

func TestStripeCheckout_OwnSessionSupersededReturns409(t *testing.T) {
	r := newStripeCheckoutRig(t, "one-sub-own-superseded")
	r.setPin(t, "stripe", "cus_A", "", "none")
	// A greater session appears after this request created its own and
	// before it lists.
	var greater string
	r.h.svc.SetCheckoutTestHooks(billing.CheckoutTestHooks{AfterReread: func() {
		r.h.svc.SetCheckoutTestHooks(billing.CheckoutTestHooks{})
		r.acct.mu.Lock()
		r.acct.clock++
		r.acct.mu.Unlock()
		greater = r.acct.addSession("cus_A", "subscription")
	}})
	_, err := r.checkout("stripe")
	assertConflictCode(t, err, "billing_checkout_superseded")
	if open := r.acct.openSessions("cus_A"); len(open) != 1 || open[0] != greater {
		t.Fatalf("open sessions = %v, want only the greatest %s", open, greater)
	}
}

func TestStripeCheckout_SameSecondCounterExampleConverges(t *testing.T) {
	// A creates its session and lists before B's exists; B then creates a
	// session in the same second whose id sorts lower, and lists. Exactly one
	// session stays open: the greatest by (created, id).
	r := newStripeCheckoutRig(t, "one-sub-same-second")
	r.setPin(t, "stripe", "cus_A", "", "none")
	if _, err := r.checkout("stripe"); err != nil {
		t.Fatalf("A: %v", err)
	}
	if _, errB := r.checkout("stripe"); errB != nil {
		t.Fatalf("B: %v", errB)
	}
	open := r.acct.openSessions("cus_A")
	if len(open) != 1 {
		t.Fatalf("open sessions = %v, want exactly one", open)
	}
	r.acct.mu.Lock()
	var greatest *fakeStripeSession
	for _, s := range r.acct.sessions {
		if greatest == nil || s.Created > greatest.Created || (s.Created == greatest.Created && s.ID > greatest.ID) {
			greatest = s
		}
	}
	r.acct.mu.Unlock()
	if open[0] != greatest.ID {
		t.Fatalf("open session %s is not the greatest %s", open[0], greatest.ID)
	}
}

// ---------------------------------------------------------------------------
// Confirm.
// ---------------------------------------------------------------------------

func TestStripeConfirm_BindsToTenantAndCustomer(t *testing.T) {
	r := newStripeCheckoutRig(t, "confirm-binding")
	r.setPin(t, "stripe", "cus_A", "", "none")
	add := func(customer, mode, status, ref, sub string) string {
		r.acct.mu.Lock()
		defer r.acct.mu.Unlock()
		s := &fakeStripeSession{ID: r.acct.id("cs"), Customer: customer, Mode: mode, Status: status, ClientRef: ref, Subscription: sub, Created: r.acct.clock}
		r.acct.sessions = append(r.acct.sessions, s)
		return s.ID
	}
	good := add("cus_A", "subscription", "complete", r.tenant.String(), "sub_NEW")
	bad := map[string]string{
		"other tenant":   add("cus_A", "subscription", "complete", uuid.NewString(), "sub_NEW"),
		"other customer": add("cus_B", "subscription", "complete", r.tenant.String(), "sub_NEW"),
		"payment mode":   add("cus_A", "payment", "complete", r.tenant.String(), ""),
		"still open":     add("cus_A", "subscription", "open", r.tenant.String(), "sub_NEW"),
	}
	for name, id := range bad {
		_, err := r.h.svc.ConfirmCheckout(r.baseCtx, r.tenant, id)
		de, ok := domain.AsDomain(err)
		if !ok || de.Kind != domain.KindValidation {
			t.Fatalf("%s: err = %v, want 422", name, err)
		}
	}
	if _, err := r.h.svc.ConfirmCheckout(r.baseCtx, r.tenant, "../x"); err == nil {
		t.Fatal("a malformed session id was accepted")
	}
	if n := countBillingJobs(t, r.pool, "billing_refresh"); n != 0 {
		t.Fatalf("refused confirms enqueued %d refreshes", n)
	}
	res, err := r.h.svc.ConfirmCheckout(r.baseCtx, r.tenant, good)
	if err != nil || res.Landed {
		t.Fatalf("confirm = %+v, %v; want accepted and not yet landed", res, err)
	}
	if n := countBillingJobs(t, r.pool, "billing_refresh"); n != 1 {
		t.Fatalf("billing_refresh jobs = %d, want 1", n)
	}
}
