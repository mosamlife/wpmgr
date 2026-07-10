package pricing

// handler.go — the PUBLIC pricing endpoint:
//
//	GET /api/v1/pricing
//
// Unauthenticated, hosted-only. Mirrors how the billing webhook and RUM
// ingest endpoints mount directly on the root engine (no session, no tenant
// gate — see internal/server/server.go). Registered ONLY when WPMGR_HOSTED
// is enabled (cmd/wpmgr/main.go wires internal/server.Deps.PricingH to nil
// on self-host, exactly like Deps.BillingH/BillingWebhookH), so a self-host
// install correctly 404s here.

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// Handler serves GET /api/v1/pricing.
type Handler struct {
	svc *Service
}

// NewHandler builds the pricing Handler.
func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// RegisterPublic mounts GET /api/v1/pricing on the root engine — no session,
// no tenant gate; the response is identical for every caller. Callers must
// only invoke this when hosted billing is enabled (see the package doc
// comment) — this method itself does not check that, mirroring
// billing.WebhookHandler.RegisterPublic's nil/non-nil-gated-by-the-caller
// convention.
func (h *Handler) RegisterPublic(r *gin.Engine) {
	r.GET("/api/v1/pricing", h.handle)
}

func (h *Handler) handle(c *gin.Context) {
	raw := h.svc.GetPricing(c.Request.Context())
	c.Header("Cache-Control", "public, max-age=3600")
	c.Data(http.StatusOK, "application/json; charset=utf-8", raw)
}
