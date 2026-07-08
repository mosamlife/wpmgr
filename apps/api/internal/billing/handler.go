package billing

// handler.go — the M16 Phase B operator-facing routes: GET the current
// billing summary, start a checkout, and mint a billing-management portal
// session. Mounted ONLY when hosted billing is enabled (see
// internal/server/server.go) — with the handler unmounted these three paths
// 404, which is the routes-contract test's whole point.

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// UserEmailLookup resolves a user's email to prefill a Stripe (or future
// provider) checkout's customer_email field. Wired to auth.Repo.GetUserByID
// in cmd/wpmgr/main.go via a thin closure, so this package never imports
// internal/auth. A nil lookup, or a lookup that errors, simply leaves the
// checkout's email blank — the provider's hosted checkout page asks for it
// instead; a lookup failure must never block a checkout.
type UserEmailLookup func(ctx context.Context, userID uuid.UUID) (string, error)

// Handler serves the tenant-facing billing routes under /api/v1/billing.
type Handler struct {
	svc           *Service
	emailLookup   UserEmailLookup
	publicBaseURL string
}

// NewHandler builds the billing Handler. publicBaseURL (no trailing slash,
// e.g. "https://manage.wpmgr.app") is used to build the checkout
// success/cancel redirect URLs; emailLookup may be nil.
func NewHandler(svc *Service, emailLookup UserEmailLookup, publicBaseURL string) *Handler {
	return &Handler{svc: svc, emailLookup: emailLookup, publicBaseURL: publicBaseURL}
}

// Register mounts the billing routes on an already tenant-gated group
// (authz.RequireAuth + authz.RequireTenant — see internal/server/server.go's
// v1 group). Billing is an org-level, owner-only concern (PermBillingManage
// follows PermAuditManage's precedent exactly): RequireOrgScope blocks
// site-scoped collaborators outright, and RequirePermission then additionally
// requires the owner role.
func (h *Handler) Register(r *gin.RouterGroup) {
	g := r.Group("/billing", authz.RequireOrgScope())
	g.GET("", authz.RequirePermission(authz.PermBillingManage), h.getBilling)
	g.POST("/checkout", authz.RequirePermission(authz.PermBillingManage), h.createCheckout)
	g.POST("/checkout/verify", authz.RequirePermission(authz.PermBillingManage), h.verifyCheckoutCallback)
	g.POST("/portal", authz.RequirePermission(authz.PermBillingManage), h.createPortal)
	g.POST("/cancel", authz.RequirePermission(authz.PermBillingManage), h.cancelSubscription)
}

func (h *Handler) getBilling(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}
	out, err := h.svc.GetBillingSummary(c.Request.Context(), p.TenantID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// checkoutRequest is the POST /billing/checkout body. tier is the ONLY
// caller-supplied selector for the PRICE — the request never names a
// provider price id directly; Service.CreateCheckout validates tier against
// the paid ladder and the provider adapter resolves it to a price
// server-side. provider ("stripe" | "razorpay") lets the customer choose a
// payment provider at checkout; empty defaults to "stripe" (back-comfort for
// every caller written before Razorpay existed) UNLESS this tenant already
// has a pinned provider, which always wins (see CreateCheckout's "one tenant
// = one provider" resolution). currency ("USD" | "INR") is required when
// provider is "razorpay" — Razorpay has no single multi-currency price the
// way Stripe does, so the adapter resolves a plan PER (tier, currency); it is
// ignored for every other provider.
type checkoutRequest struct {
	Tier     string `json:"tier"`
	Provider string `json:"provider"`
	Currency string `json:"currency"`
}

// checkoutResponse is the wire shape of a successful POST /billing/checkout.
// Exactly one of url/razorpay is populated, mirroring billing.CheckoutSession
// exactly:
//
//   - Stripe: {"url": "https://checkout.stripe.com/..."}
//   - Razorpay: {"razorpay": {"subscription_id": "...", "key_id": "...",
//     "currency": "USD", "amount": 1500}} — the frontend hands this straight
//     to Razorpay's Checkout.js modal ("key", "subscription_id", "amount",
//     "currency" options).
type checkoutResponse struct {
	URL      string                `json:"url,omitempty"`
	Razorpay *RazorpayCheckoutData `json:"razorpay,omitempty"`
}

func (h *Handler) createCheckout(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}
	var body checkoutRequest
	if err := bindJSON(c, &body); err != nil {
		httpx.Error(c, err)
		return
	}

	email := ""
	if h.emailLookup != nil && p.Type == domain.PrincipalUser {
		if e, lerr := h.emailLookup(c.Request.Context(), p.UserID); lerr == nil {
			email = e
		}
	}

	successURL := h.publicBaseURL + "/billing?checkout=success"
	cancelURL := h.publicBaseURL + "/billing?checkout=cancel"
	actor := Actor{Type: actorTypeFor(p.Type == domain.PrincipalAPIKey), ID: p.ActorID()}

	sess, err := h.svc.CreateCheckout(c.Request.Context(), p.TenantID, Tier(body.Tier), body.Provider, body.Currency, email, successURL, cancelURL, actor)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, checkoutResponse{URL: sess.URL, Razorpay: sess.Razorpay})
}

// checkoutCallbackRequest is the POST /billing/checkout/verify body: the
// EXACT field names Razorpay's Checkout.js onSuccess handler hands the
// browser (razorpay_payment_id/razorpay_subscription_id/razorpay_signature),
// passed through verbatim so the frontend never has to rename them.
type checkoutCallbackRequest struct {
	RazorpayPaymentID      string `json:"razorpay_payment_id"`
	RazorpaySubscriptionID string `json:"razorpay_subscription_id"`
	RazorpaySignature      string `json:"razorpay_signature"`
}

type checkoutCallbackResponse struct {
	Verified bool `json:"verified"`
}

// verifyCheckoutCallback verifies a browser-returned checkout-completion
// callback — a UX confirmation ONLY (see Service.VerifyCheckoutCallback's
// doc comment: the webhook remains the sole source of truth for granting a
// plan). Tenant-scoped: the callback is verified against the CALLER's own
// tenant's pinned provider, never a caller-supplied provider/tenant.
func (h *Handler) verifyCheckoutCallback(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}
	var body checkoutCallbackRequest
	if err := bindJSON(c, &body); err != nil {
		httpx.Error(c, err)
		return
	}
	payload := map[string]string{
		"razorpay_payment_id":      body.RazorpayPaymentID,
		"razorpay_subscription_id": body.RazorpaySubscriptionID,
		"razorpay_signature":       body.RazorpaySignature,
	}
	if err := h.svc.VerifyCheckoutCallback(c.Request.Context(), p.TenantID, payload); err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, checkoutCallbackResponse{Verified: true})
}

type portalResponse struct {
	URL string `json:"url"`
}

func (h *Handler) createPortal(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}
	actor := Actor{Type: actorTypeFor(p.Type == domain.PrincipalAPIKey), ID: p.ActorID()}
	sess, err := h.svc.CreatePortalSession(c.Request.Context(), p.TenantID, actor)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, portalResponse{URL: sess.URL})
}

// cancelResponse is the wire shape of a successful POST /billing/cancel.
// PINNED contract: {"ok": true} — the ONLY signal the frontend gets back
// synchronously. Cancellation itself is scheduled for the end of the current
// billing period (see Service.CancelSubscription); the actual plan/status
// change lands later via the provider's webhook, exactly like the checkout
// success flow — the frontend should poll GET /billing afterward rather than
// expect this response to carry the new plan state.
type cancelResponse struct {
	OK bool `json:"ok"`
}

// cancelSubscription is the provider-agnostic backend for the dashboard's
// "Cancel subscription" action — the ONLY cancellation path for a provider
// with no hosted portal (Razorpay; see billing.Provider.HasPortal). Tenant-
// scoped + owner-gated exactly like every other /billing route.
func (h *Handler) cancelSubscription(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}
	actor := Actor{Type: actorTypeFor(p.Type == domain.PrincipalAPIKey), ID: p.ActorID()}
	if err := h.svc.CancelSubscription(c.Request.Context(), p.TenantID, actor); err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, cancelResponse{OK: true})
}

func bindJSON(c *gin.Context, dst any) error {
	dec := json.NewDecoder(c.Request.Body)
	if err := dec.Decode(dst); err != nil {
		return domain.Validation("invalid_body", "request body is not valid JSON: "+err.Error())
	}
	return nil
}
