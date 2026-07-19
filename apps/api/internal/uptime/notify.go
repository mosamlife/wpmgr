package uptime

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/wneessen/go-mail"

	"github.com/mosamlife/wpmgr/apps/api/internal/config"
	"github.com/mosamlife/wpmgr/apps/api/internal/httpclient"
)

// SendResult is the REAL outcome of one Mailer.Send call — as opposed to
// merely "recipients were configured" — so callers can record an honest audit
// trail (GH #144: the uptime alert dispatcher used to record "emailed: true"
// whenever recipients were configured, even though the send itself was never
// attempted or had failed).
type SendResult struct {
	// Status is one of SendResultSent, SendResultSkipped, or SendResultFailed.
	Status string
	// Reason is set whenever Status != SendResultSent: a coarse, ALREADY-
	// SCRUBBED code/message safe to persist in the audit log (never raw SMTP
	// host/credential/error detail).
	Reason string
}

// SendResult.Status values.
const (
	SendResultSent    = "sent"
	SendResultSkipped = "skipped"
	SendResultFailed  = "failed"
)

// Mailer sends an alert email to recipients and reports the real delivery
// outcome. An interface so the evaluator can be tested with a stub sink and
// the SMTP transport stays swappable.
type Mailer interface {
	Send(ctx context.Context, recipients []string, subject, body string) (SendResult, error)
}

// SMTPMailer delivers email over a boot-time-configured SMTP relay via go-mail
// (ADR-029). It is retained for the sharing/invitation legacy env-SMTP wiring
// (cmd/wpmgr/main.go) — it is NOT used for uptime/security alerts, which
// resolve their transport per-send from the DB-configured SMTP via
// internal/mailer.Service (see cmd/wpmgr/uptime_mailer.go, GH #144). SMTP
// credentials are NEVER logged.
type SMTPMailer struct {
	cfg    config.SMTPConfig
	logger *slog.Logger
}

// NewSMTPMailer builds an SMTPMailer from config.
func NewSMTPMailer(cfg config.SMTPConfig, logger *slog.Logger) *SMTPMailer {
	if logger == nil {
		logger = slog.Default()
	}
	return &SMTPMailer{cfg: cfg, logger: logger}
}

// Send delivers one email to all recipients. A send failure is returned so the
// caller can log it; alert delivery is best-effort (one channel failing must
// not block the other).
func (m *SMTPMailer) Send(ctx context.Context, recipients []string, subject, body string) error {
	if len(recipients) == 0 {
		return nil
	}
	msg := mail.NewMsg()
	if err := msg.From(m.cfg.From); err != nil {
		return fmt.Errorf("smtp from: %w", err)
	}
	if err := msg.To(recipients...); err != nil {
		return fmt.Errorf("smtp to: %w", err)
	}
	msg.Subject(subject)
	msg.SetBodyString(mail.TypeTextPlain, body)

	opts := []mail.Option{mail.WithPort(m.cfg.Port), mail.WithTimeout(15 * time.Second)}
	switch strings.ToLower(m.cfg.TLSMode) {
	case "tls":
		opts = append(opts, mail.WithSSLPort(false), mail.WithTLSPolicy(mail.TLSMandatory))
	case "none":
		opts = append(opts, mail.WithTLSPolicy(mail.NoTLS))
	default: // starttls
		opts = append(opts, mail.WithTLSPolicy(mail.TLSMandatory))
	}
	if m.cfg.Username != "" {
		opts = append(opts, mail.WithSMTPAuth(mail.SMTPAuthPlain), mail.WithUsername(m.cfg.Username), mail.WithPassword(m.cfg.Password))
	}

	client, err := mail.NewClient(m.cfg.Host, opts...)
	if err != nil {
		return fmt.Errorf("smtp client: %w", err)
	}
	if err := client.DialAndSendWithContext(ctx, msg); err != nil {
		return fmt.Errorf("smtp send: %w", err)
	}
	return nil
}

// WebhookPoster delivers a signed alert webhook. An interface so the evaluator
// can be tested without real network I/O. payload is `any` (not the
// uptime-specific WebhookPayload struct) so the SAME signing + SSRF-hardened
// delivery can be reused for other alert-producing domains' payload shapes —
// see Dispatcher.PostSignedWebhook, used by internal/vuln's batched
// vulnerability-findings webhook (m103, GH #247).
type WebhookPoster interface {
	Post(ctx context.Context, url, secret string, payload any) error
}

// WebhookPayload is the JSON body POSTed to an alert webhook.
type WebhookPayload struct {
	Event      string    `json:"event"` // "uptime.down" | "uptime.recovery"
	TenantID   string    `json:"tenant_id"`
	SiteID     string    `json:"site_id"`
	SiteURL    string    `json:"site_url"`
	SiteName   string    `json:"site_name,omitempty"`
	HTTPStatus int       `json:"http_status,omitempty"`
	Error      string    `json:"error,omitempty"`
	FiredAt    time.Time `json:"fired_at"`
}

// SSRFWebhookPoster posts the signed payload over the SSRF-hardened client (the
// webhook URL is user-controlled). Full Standard Webhooks is M14; here a simple
// HMAC-SHA256 signature header over the raw body plus bounded retries is enough.
type SSRFWebhookPoster struct {
	client *httpclient.Client
}

// NewSSRFWebhookPoster builds a webhook poster over the SSRF client.
func NewSSRFWebhookPoster(client *httpclient.Client) *SSRFWebhookPoster {
	return &SSRFWebhookPoster{client: client}
}

// Post marshals, signs (HMAC-SHA256 hex over the body), and POSTs the payload.
// The Client.Do path already applies bounded retries with backoff. payload is
// `any` and marshaled via encoding/json — any JSON-serializable struct works,
// not just WebhookPayload (see the interface doc above).
func (p *SSRFWebhookPoster) Post(ctx context.Context, url, secret string, payload any) error {
	if url == "" {
		return nil
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal webhook payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build webhook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "WPMgr-AlertWebhook/1.0")
	if secret != "" {
		mac := hmac.New(sha256.New, []byte(secret))
		_, _ = mac.Write(body)
		// NEVER log the secret; the signature header carries only the digest.
		req.Header.Set("X-WPMgr-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("webhook transport: %w", err)
	}
	defer func() { _, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16)); _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook endpoint returned status %d", resp.StatusCode)
	}
	return nil
}

// classifyWebhookError maps a Post error onto a coarse, stable reason code
// safe to persist in the audit log: NEVER the endpoint's response body, and
// deliberately not the raw error text either (which can echo the resolved
// destination host/IP from the SSRF-hardened dialer).
func classifyWebhookError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "returned status"):
		return "webhook_non_2xx_response"
	case strings.Contains(msg, "marshal webhook payload"):
		return "webhook_payload_marshal_failed"
	case strings.Contains(msg, "build webhook request"):
		return "webhook_request_build_failed"
	default:
		return "webhook_transport_failed"
	}
}
