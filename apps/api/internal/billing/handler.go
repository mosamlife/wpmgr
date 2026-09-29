package billing

// handler.go — the M16 Phase B operator-facing routes: GET the current
// billing summary, start a checkout, and mint a billing-management portal
// session. Mounted ONLY when hosted billing is enabled (see
// internal/server/server.go) — with the handler unmounted these three paths
// 404, which is the routes-contract test's whole point.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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
	g.POST("/checkout", authz.RequirePermission(authz.PermBillingManage), RequireHuman(), h.createCheckout)
	g.POST("/checkout/verify", authz.RequirePermission(authz.PermBillingManage), RequireHuman(), h.verifyCheckoutCallback)
	g.POST("/checkout/confirm", authz.RequirePermission(authz.PermBillingManage), RequireHuman(), h.confirmCheckout)
	g.POST("/portal", authz.RequirePermission(authz.PermBillingManage), RequireHuman(), h.createPortal)
	g.POST("/cancel", authz.RequirePermission(authz.PermBillingManage), RequireHuman(), h.cancelSubscription)
}

// RequireHuman is the billing allow-list: every billing action is taken by a
// signed-in human. Only a principal of type PrincipalUser passes; an API
// key, an MCP principal (which carries no type) and anything else get 403
// billing_human_required.
func RequireHuman() gin.HandlerFunc {
	return func(c *gin.Context) {
		p, ok := domain.PrincipalFromContext(c.Request.Context())
		if !ok || p.Type != domain.PrincipalUser {
			httpx.Error(c, domain.Forbidden("billing_human_required", "billing actions require a signed-in person"))
			c.Abort()
			return
		}
		c.Next()
	}
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
// server-side. provider ("stripe" | "razorpay") is the provider the customer
// chooses; empty means Stripe when it is registered, else the only
// registered provider. The request wins over the tenant's current provider,
// subject to the switch rule in Service.CreateCheckout: moving off another
// provider needs nothing pending or live there, and is otherwise refused
// with 409 billing_provider_locked. currency matters to Razorpay only, which
// charges in INR: omitted means INR, and USD is refused. Stripe ignores it
// and charges US$.
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

	// {CHECKOUT_SESSION_ID} is substituted by Stripe on redirect; the web
	// passes it to POST /billing/checkout/confirm.
	successURL := h.publicBaseURL + "/billing?checkout=success&session_id={CHECKOUT_SESSION_ID}"
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

// checkoutConfirmRequest is the POST /billing/checkout/confirm body.
type checkoutConfirmRequest struct {
	SessionID string `json:"session_id"`
}

type okResponse struct {
	OK bool `json:"ok"`
}

// confirmCheckout checks the checkout session the browser returned with and
// enqueues a refresh (see Service.ConfirmCheckout). 200 when the plan change
// had already landed, 202 when a refresh was enqueued.
func (h *Handler) confirmCheckout(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}
	var body checkoutConfirmRequest
	if err := bindJSON(c, &body); err != nil {
		httpx.Error(c, err)
		return
	}
	res, err := h.svc.ConfirmCheckout(c.Request.Context(), p.TenantID, body.SessionID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	status := http.StatusAccepted
	if res.Landed {
		status = http.StatusOK
	}
	c.JSON(status, okResponse{OK: true})
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

// cancelRequest is the optional POST /billing/cancel body. An absent body,
// or an absent when, means period_end.
type cancelRequest struct {
	When string `json:"when"`
}

// cancelSubscription is the provider-agnostic backend for the dashboard's
// "Cancel subscription" action — the ONLY cancellation path for a provider
// with no hosted portal (Razorpay; see billing.Provider.HasPortal). Tenant-
// scoped + owner-gated exactly like every other /billing route.
// {"when":"now"} is Cancel now (Service.CancelSubscriptionNow).
func (h *Handler) cancelSubscription(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}
	var body cancelRequest
	if err := bindOptionalJSON(c, &body); err != nil {
		httpx.Error(c, err)
		return
	}
	when, err := ParseCancelWhen(body.When)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	actor := Actor{Type: actorTypeFor(p.Type == domain.PrincipalAPIKey), ID: p.ActorID()}
	if when == CancelNow {
		err = h.svc.CancelSubscriptionNow(c.Request.Context(), p.TenantID, actor)
	} else {
		err = h.svc.CancelSubscription(c.Request.Context(), p.TenantID, actor)
	}
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, cancelResponse{OK: true})
}

// bindOptionalJSON is bindJSON for a body that may be absent: an empty body
// leaves dst unchanged.
func bindOptionalJSON(c *gin.Context, dst any) error {
	if c.Request.Body == nil {
		return nil
	}
	dec := json.NewDecoder(c.Request.Body)
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return domain.Validation("invalid_body", "request body is not valid JSON: "+err.Error())
	}
	return nil
}

func bindJSON(c *gin.Context, dst any) error {
	dec := json.NewDecoder(c.Request.Body)
	if err := dec.Decode(dst); err != nil {
		return domain.Validation("invalid_body", "request body is not valid JSON: "+err.Error())
	}
	return nil
}
