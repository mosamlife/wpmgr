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

// Mailer sends an alert email to recipients. An interface so the evaluator can
// be tested with a stub sink and the SMTP transport stays swappable.
type Mailer interface {
	Send(ctx context.Context, recipients []string, subject, body string) error
}

// SMTPMailer delivers alert emails over the self-host SMTP relay via go-mail
// (ADR-029). When SMTP is unconfigured it is replaced by NoopMailer in wiring;
// this type assumes a configured host. SMTP credentials are NEVER logged.
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

// NoopMailer is used when SMTP is unconfigured: email alerts log and no-op so
// webhook alerts still fire.
type NoopMailer struct{ logger *slog.Logger }

// NewNoopMailer builds a NoopMailer.
func NewNoopMailer(logger *slog.Logger) *NoopMailer {
	if logger == nil {
		logger = slog.Default()
	}
	return &NoopMailer{logger: logger}
}

// Send logs that the email was skipped (no recipients are leaked at info level
// beyond their count).
func (m *NoopMailer) Send(_ context.Context, recipients []string, subject, _ string) error {
	if len(recipients) == 0 {
		return nil
	}
	m.logger.Info("smtp not configured: alert email skipped",
		slog.Int("recipients", len(recipients)), slog.String("subject", subject))
	return nil
}

// WebhookPoster delivers a signed alert webhook. An interface so the evaluator
// can be tested without real network I/O.
type WebhookPoster interface {
	Post(ctx context.Context, url, secret string, payload WebhookPayload) error
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
// The Client.Do path already applies bounded retries with backoff.
func (p *SSRFWebhookPoster) Post(ctx context.Context, url, secret string, payload WebhookPayload) error {
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
