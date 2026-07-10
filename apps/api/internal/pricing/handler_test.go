package pricing

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/mosamlife/wpmgr/apps/api/internal/billing"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// TestRegisterPublicServesPricing proves the happy path: once RegisterPublic
// is called, GET /api/v1/pricing returns 200, a public Cache-Control header,
// and a well-formed pricing body.
func TestRegisterPublicServesPricing(t *testing.T) {
	stripe := &fakeStripeProvider{priceFn: func(billing.Tier) (int64, string, string, error) {
		return 1500, "USD", "month", nil
	}}
	svc := NewService(billing.NewRegistry(stripe), nil, slog.Default())
	h := NewHandler(svc)

	engine := gin.New()
	h.RegisterPublic(engine)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/pricing", nil)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=3600" {
		t.Fatalf("Cache-Control = %q, want %q", got, "public, max-age=3600")
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["currency_default"] != "USD" {
		t.Fatalf("currency_default = %v, want USD", body["currency_default"])
	}
}

// TestRouteNotMountedReturns404 proves the self-host (WPMGR_HOSTED=false)
// contract: when the caller (cmd/wpmgr/main.go, gated on cfg.Hosted.Enabled)
// never calls RegisterPublic at all, GET /api/v1/pricing 404s — the SAME
// mechanism internal/server.Deps.PricingH's nil case relies on.
func TestRouteNotMountedReturns404(t *testing.T) {
	engine := gin.New() // RegisterPublic deliberately never called

	req := httptest.NewRequest(http.MethodGet, "/api/v1/pricing", nil)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (route must not be mounted on self-host)", rec.Code)
	}
}

// TestHandlerNeverErrorsEvenOnTotalProviderFailure proves the HTTP layer
// itself has no error path: Service.GetPricing never returns an error, so
// the handler always writes 200 (backed by the static fallback when every
// provider fails).
func TestHandlerNeverErrorsEvenOnTotalProviderFailure(t *testing.T) {
	failing := &fakeStripeProvider{priceFn: func(billing.Tier) (int64, string, string, error) {
		return 0, "", "", context.DeadlineExceeded
	}}
	svc := NewService(billing.NewRegistry(failing), nil, slog.Default())
	h := NewHandler(svc)

	engine := gin.New()
	h.RegisterPublic(engine)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/pricing", nil)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 even when every provider fails (static fallback)", rec.Code)
	}
}
