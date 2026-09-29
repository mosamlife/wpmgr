package billing

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

func fakeNamed(name string) Provider {
	return &stubProvider{name: name}
}

type stubProvider struct {
	Provider
	name string
}

func (s *stubProvider) Name() string { return s.name }

func TestRegistryNames_StripeFirst(t *testing.T) {
	if got := NewRegistry(fakeNamed("stripe")).Names(); !reflect.DeepEqual(got, []string{"stripe"}) {
		t.Fatalf("Stripe-only Names() = %v", got)
	}
	if got := NewRegistry(fakeNamed("razorpay"), fakeNamed("stripe")).Names(); !reflect.DeepEqual(got, []string{"stripe", "razorpay"}) {
		t.Fatalf("Stripe+Razorpay Names() = %v, want [stripe razorpay]", got)
	}
	var nilReg *Registry
	if got := nilReg.Names(); got == nil || len(got) != 0 {
		t.Fatalf("nil registry Names() = %#v, want an empty non-nil slice", got)
	}
	if got := NewRegistry(fakeNamed("razorpay")).DefaultCheckoutProvider(); got != "razorpay" {
		t.Fatalf("DefaultCheckoutProvider with only razorpay = %q", got)
	}
	if got := NewRegistry(fakeNamed("razorpay"), fakeNamed("stripe")).DefaultCheckoutProvider(); got != "stripe" {
		t.Fatalf("DefaultCheckoutProvider with both = %q, want stripe", got)
	}
}

func TestSummaryJSON_AvailableProvidersIsAnArray(t *testing.T) {
	b, err := json.Marshal(Summary{AvailableProviders: NewRegistry().Names()})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if _, ok := m["available_providers"].([]any); !ok {
		t.Fatalf("available_providers = %v, want a JSON array (never null)", m["available_providers"])
	}
}

// TestProviderLocked_ReasonReachesResponseBody renders each
// billing_provider_locked reason through the central error mapper and reads
// details.reason back from the response body.
func TestProviderLocked_ReasonReachesResponseBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, reason := range []string{ReasonNeedsSupport, ReasonPendingAtProvider, ReasonProviderChanged} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		httpx.Error(c, providerLocked(reason, "locked"))
		if w.Code != http.StatusConflict {
			t.Fatalf("%s: status = %d, want 409", reason, w.Code)
		}
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: body %s: %v", reason, w.Body.String(), err)
		}
		details, _ := body["details"].(map[string]any)
		if body["code"] != "billing_provider_locked" || details["reason"] != reason {
			t.Fatalf("%s: body = %s, want code billing_provider_locked and details.reason", reason, w.Body.String())
		}
	}
}

func TestRequireHuman_AllowsOnlySignedInPeople(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := map[string]struct {
		p      *domain.Principal
		status int
	}{
		"user":    {&domain.Principal{Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: uuid.New()}, http.StatusOK},
		"api key": {&domain.Principal{Type: domain.PrincipalAPIKey, APIKeyID: uuid.New(), TenantID: uuid.New()}, http.StatusForbidden},
		"no type": {&domain.Principal{TenantID: uuid.New()}, http.StatusForbidden},
		"none":    {nil, http.StatusForbidden},
	}
	for name, tc := range cases {
		r := gin.New()
		r.Use(func(c *gin.Context) {
			if tc.p != nil {
				c.Request = c.Request.WithContext(domain.WithPrincipal(c.Request.Context(), *tc.p))
			}
			c.Next()
		})
		r.POST("/x", RequireHuman(), func(c *gin.Context) { c.Status(http.StatusOK) })
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/x", nil))
		if w.Code != tc.status {
			t.Fatalf("%s: status = %d, want %d (body %s)", name, w.Code, tc.status, w.Body.String())
		}
		if tc.status == http.StatusForbidden {
			var body map[string]any
			_ = json.Unmarshal(w.Body.Bytes(), &body)
			if body["code"] != "billing_human_required" {
				t.Fatalf("%s: code = %v, want billing_human_required", name, body["code"])
			}
		}
	}
}

func TestCheckoutRefusal_Matrix(t *testing.T) {
	cases := []struct {
		status  Status
		sub     string
		code    string
		refresh bool
	}{
		{StatusComped, "", "billing_comped", false},
		{StatusActive, "sub_1", "billing_subscription_exists", false},
		{StatusTrialing, "sub_1", "billing_subscription_exists", false},
		{StatusPastDue, "sub_1", "billing_subscription_exists", false},
		{StatusPaused, "sub_1", "billing_subscription_exists", false},
		{StatusNone, "sub_1", "billing_subscription_pending", true},
		{StatusNone, "", "", false},
		{StatusCanceled, "sub_1", "", false},
	}
	for _, tc := range cases {
		err, refresh := checkoutRefusal(tenantBillingProfile{Status: tc.status, ProviderSubscriptionID: tc.sub})
		if tc.code == "" {
			if err != nil {
				t.Errorf("%s/%q: refused with %v, want allowed", tc.status, tc.sub, err)
			}
			continue
		}
		de, ok := domain.AsDomain(err)
		if !ok || de.Code != tc.code || de.Kind != domain.KindConflict {
			t.Errorf("%s/%q: err = %v, want 409 %s", tc.status, tc.sub, err, tc.code)
		}
		if (refresh != "") != tc.refresh {
			t.Errorf("%s/%q: refresh sub = %q, want refresh=%v", tc.status, tc.sub, refresh, tc.refresh)
		}
	}
}
