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
	g.POST("/portal", authz.RequirePermission(authz.PermBillingManage), h.createPortal)
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
// caller-supplied selector — the request never names a provider price id
// directly; Service.CreateCheckout validates tier against the paid ladder and
// the provider adapter resolves it to a price server-side.
type checkoutRequest struct {
	Tier string `json:"tier"`
}

type checkoutResponse struct {
	URL string `json:"url"`
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

	sess, err := h.svc.CreateCheckout(c.Request.Context(), p.TenantID, Tier(body.Tier), email, successURL, cancelURL, actor)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, checkoutResponse{URL: sess.URL})
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

func bindJSON(c *gin.Context, dst any) error {
	dec := json.NewDecoder(c.Request.Body)
	if err := dec.Decode(dst); err != nil {
		return domain.Validation("invalid_body", "request body is not valid JSON: "+err.Error())
	}
	return nil
}
