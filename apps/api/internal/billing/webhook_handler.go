package billing

// webhook_handler.go — the PUBLIC payment-provider webhook endpoint:
//
//	POST /webhooks/billing/:provider
//
// Mirrors how the email domain mounts its own public provider webhooks
// (internal/email/webhook_handler.go, RegisterPublic on the root engine, no
// session/tenant middleware — the provider's own signature is the auth).
// Registered unconditionally by cmd/wpmgr/main.go whenever a WebhookHandler
// is wired (hosted billing on); ProcessWebhook itself 404s for an unknown
// provider name, so mounting one path segment (":provider") covers Stripe
// today and any future adapter without another route.

import (
	"io"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// maxWebhookBodyBytes bounds the webhook body read. Provider event payloads
// (Stripe included) are small — well under 256 KiB even for a fully expanded
// checkout.session.completed — 1 MiB is generous headroom.
const maxWebhookBodyBytes = 1 << 20

// WebhookHandler serves the public payment-provider webhook endpoint.
type WebhookHandler struct {
	svc    *Service
	logger *slog.Logger
}

// NewWebhookHandler builds the webhook handler.
func NewWebhookHandler(svc *Service, logger *slog.Logger) *WebhookHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &WebhookHandler{svc: svc, logger: logger}
}

// RegisterPublic mounts POST /webhooks/billing/:provider on the root engine —
// no session, no tenant gate; the provider's own request signature is the
// entire authentication boundary (verified inside Service.ProcessWebhook).
func (h *WebhookHandler) RegisterPublic(r *gin.Engine) {
	r.POST("/webhooks/billing/:provider", h.handle)
}

func (h *WebhookHandler) handle(c *gin.Context) {
	provider := c.Param("provider")

	body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxWebhookBodyBytes))
	if err != nil {
		httpx.Error(c, domain.Validation("webhook_body_read", "could not read request body"))
		return
	}

	if err := h.svc.ProcessWebhook(c.Request.Context(), provider, body, c.Request.Header); err != nil {
		h.logger.Warn("billing webhook: processing failed",
			slog.String("provider", provider), slog.Any("error", err))
		httpx.Error(c, err)
		return
	}

	c.Status(http.StatusOK)
}
