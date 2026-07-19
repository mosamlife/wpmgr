// Package mailer is WPMgr's neutral transactional-email service: it loads the
// instance SMTP transport (DB row first, env fallback), renders brand HTML +
// plaintext templates, and sends over an SSRF-hardened go-mail client. It is
// imported by internal/settings (send-test) and the auth flows (reset /
// activation / invite) without any of them depending on internal/uptime, where
// the original SMTP code lived (ADR-045).
package mailer

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	"code.dny.dev/ssrf"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/wneessen/go-mail"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

// Transport is a decrypted SMTP relay config ready to dial. The Password lives
// in memory only here and is NEVER logged.
type Transport struct {
	Host             string
	Port             int
	Username         string
	Password         string
	From             string
	FromName         string
	TLSMode          string // "starttls" | "tls" | "none"
	AllowInsecureTLS bool
}

// Configured reports whether the transport has the minimum to send (a host and a
// From address).
func (t Transport) Configured() bool {
	return strings.TrimSpace(t.Host) != "" && strings.TrimSpace(t.From) != ""
}

// Email is a rendered message: subject + HTML body + plaintext alternative.
type Email struct {
	Subject string
	HTML    string
	Text    string
}

// Resolver loads the active SMTP transport. The bool is false when no transport
// is configured at all (DB row disabled/empty AND no env fallback), in which
// case the caller treats email as unavailable rather than erroring.
type Resolver interface {
	Resolve(ctx context.Context) (Transport, bool, error)
}

// Renderer turns (template name, data) into a rendered Email.
type Renderer interface {
	Render(name string, data map[string]any) (Email, error)
}

// Service renders + sends transactional mail and records each attempt in
// email_log. It is safe for concurrent use.
type Service struct {
	resolver     Resolver
	renderer     Renderer
	pool         *db.Pool
	logger       *slog.Logger
	baseURL      string
	supportEmail string
}

// NewService builds the mailer service. baseURL is WPMGR_PUBLIC_BASE_URL (used
// to build links inside templates); supportEmail is shown in footers.
func NewService(resolver Resolver, renderer Renderer, pool *db.Pool, baseURL, supportEmail string, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		resolver:     resolver,
		renderer:     renderer,
		pool:         pool,
		logger:       logger,
		baseURL:      strings.TrimRight(baseURL, "/"),
		supportEmail: supportEmail,
	}
}

// Enabled reports whether a usable SMTP transport is currently configured.
func (s *Service) Enabled(ctx context.Context) bool {
	_, ok, err := s.resolver.Resolve(ctx)
	return err == nil && ok
}

// enrich injects the common template variables every email expects so callers
// only pass the per-email fields.
func (s *Service) enrich(template string, data map[string]any) map[string]any {
	if data == nil {
		data = map[string]any{}
	}
	setDefault(data, "ProductName", "WPMgr")
	setDefault(data, "BaseURL", s.baseURL)
	setDefault(data, "SupportEmail", s.supportEmail)
	setDefault(data, "Year", time.Now().Year())
	setDefault(data, "PreviewText", defaultPreview(template))
	return data
}

// Deliver renders the template and sends it over the resolved transport,
// updating the email_log row. Returns a non-nil error ONLY for transient
// transport failures (so the River worker retries); a permanent condition
// (SMTP unconfigured, render failure) marks the row failed and returns nil.
func (s *Service) Deliver(ctx context.Context, emailLogID uuid.UUID, recipients []string, template string, data map[string]any) error {
	transport, ok, err := s.resolver.Resolve(ctx)
	if err != nil {
		s.markFailed(ctx, emailLogID, "resolve smtp transport")
		s.logger.Error("mailer resolve failed", slog.String("err", err.Error()))
		return nil // not retryable from the worker's POV
	}
	if !ok || !transport.Configured() {
		s.markFailed(ctx, emailLogID, "smtp not configured")
		s.logger.Warn("email skipped: smtp not configured", slog.String("template", template))
		return nil
	}

	em, err := s.renderer.Render(template, s.enrich(template, data))
	if err != nil {
		s.markFailed(ctx, emailLogID, "render template")
		s.logger.Error("mailer render failed", slog.String("template", template), slog.String("err", err.Error()))
		return nil
	}

	if err := sendMail(ctx, transport, recipients, em); err != nil {
		// Transient: record + return the error so River retries this attempt.
		s.markFailed(ctx, emailLogID, scrubSMTPError(err))
		s.logger.Warn("email send failed", slog.String("template", template), slog.String("err", err.Error()))
		return fmt.Errorf("send %s: %w", template, err)
	}

	s.markSent(ctx, emailLogID)
	return nil
}

// SendStatus is the REAL outcome of a SendMessage call — as opposed to merely
// "recipients were configured" (GH #144: the uptime alert dispatcher used to
// record "emailed: true" in the audit log whenever recipients existed, even
// though the send was never attempted — SMTP had been frozen to a boot-time
// no-op mailer — or had failed).
type SendStatus string

const (
	SendSent    SendStatus = "sent"
	SendSkipped SendStatus = "skipped"
	SendFailed  SendStatus = "failed"
)

// SendOutcome is the result of a SendMessage call: the real delivery status
// plus a coarse, ALREADY-SCRUBBED reason set whenever Status != SendSent.
type SendOutcome struct {
	Status SendStatus
	Reason string
}

// SendMessage sends a plain-text, unrendered message (no template) over the
// resolved instance SMTP transport — the SAME per-send DB-first/env-fallback
// resolution as Deliver/SendTest. It exists for callers that build their own
// subject/body outside the template system (uptime/security alerts, GH #144)
// and need the REAL delivery outcome instead of a fire-and-forget best-effort
// send, so they can record an honest audit trail rather than "emailed:
// configured". Logging to email_log is best-effort: a log-write failure never
// blocks or fails the send.
func (s *Service) SendMessage(ctx context.Context, recipients []string, subject, text string) (SendOutcome, error) {
	if len(recipients) == 0 {
		return SendOutcome{Status: SendSkipped, Reason: "no_recipients"}, nil
	}
	transport, ok, err := s.resolver.Resolve(ctx)
	if err != nil {
		s.logger.Error("mailer resolve failed", slog.String("err", err.Error()))
		return SendOutcome{Status: SendFailed, Reason: "resolve_smtp"}, fmt.Errorf("resolve smtp transport: %w", err)
	}
	if !ok || !transport.Configured() {
		s.logger.Warn("message skipped: smtp not configured", slog.String("subject", subject))
		return SendOutcome{Status: SendSkipped, Reason: "smtp_not_configured"}, nil
	}

	logID := s.insertLog(ctx, uuid.Nil, recipients, subject, "uptime_alert")
	if err := sendMail(ctx, transport, recipients, Email{Subject: subject, Text: text}); err != nil {
		reason := scrubSMTPError(err)
		if logID != uuid.Nil {
			s.markFailed(ctx, logID, reason)
		}
		s.logger.Warn("message send failed", slog.String("subject", subject), slog.String("err", err.Error()))
		return SendOutcome{Status: SendFailed, Reason: reason}, fmt.Errorf("send message: %w", err)
	}
	if logID != uuid.Nil {
		s.markSent(ctx, logID)
	}
	return SendOutcome{Status: SendSent}, nil
}

// SendTest renders + sends the "test" template synchronously over an explicit
// transport (the just-submitted or stored SMTP config). It returns a SCRUBBED
// error suitable for showing the operator (no internal IPs/hostnames/timing),
// while the full error is logged server-side.
func (s *Service) SendTest(ctx context.Context, transport Transport, to string) error {
	if !transport.Configured() {
		return errors.New("SMTP is not configured: set a host and From address first")
	}
	em, err := s.renderer.Render("test", s.enrich("test", map[string]any{"RecipientEmail": to}))
	if err != nil {
		s.logger.Error("mailer render test failed", slog.String("err", err.Error()))
		return errors.New("could not render the test email")
	}
	// Best-effort audit row (tenant-less; instance mail).
	logID := s.insertLog(ctx, uuid.Nil, []string{to}, em.Subject, "test")
	if err := sendMail(ctx, transport, []string{to}, em); err != nil {
		if logID != uuid.Nil {
			s.markFailed(ctx, logID, scrubSMTPError(err))
		}
		s.logger.Warn("smtp test send failed", slog.String("err", err.Error()))
		return errors.New(scrubSMTPError(err))
	}
	if logID != uuid.Nil {
		s.markSent(ctx, logID)
	}
	return nil
}

// ---- email_log helpers (all under app.agent='on' via InAgentTx) ------------

// insertLog writes a pending email_log row and returns its id (uuid.Nil on
// failure — logging is best-effort and must never block a send). tenantID may be
// uuid.Nil for instance/auth mail (stored as NULL).
func (s *Service) insertLog(ctx context.Context, tenantID uuid.UUID, recipients []string, subject, template string) uuid.UUID {
	var id uuid.UUID
	err := s.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).InsertEmailLog(ctx, sqlc.InsertEmailLogParams{
			TenantID:    pgUUID(tenantID),
			ToAddresses: recipients,
			Subject:     subject,
			Template:    template,
		})
		if err != nil {
			return err
		}
		id = row.ID
		return nil
	})
	if err != nil {
		s.logger.Warn("email_log insert failed", slog.String("err", err.Error()))
		return uuid.Nil
	}
	return id
}

// insertLogTx writes a pending email_log row using the CALLER'S transaction
// instead of opening a new one via InAgentTx — used by Enqueuer.EnqueueTx so
// the log row + the send_email River job insert commit atomically with
// whatever else the caller is doing in the same tx (see EnqueueTx doc).
// tx must already carry a GUC (app.tenant_id or app.agent) satisfying
// email_log's RLS policies — the caller's InTenantTx/InAgentTx wrapper does
// this; insertLogTx itself sets nothing.
func (s *Service) insertLogTx(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, recipients []string, subject, template string) (uuid.UUID, error) {
	row, err := sqlc.New(tx).InsertEmailLog(ctx, sqlc.InsertEmailLogParams{
		TenantID:    pgUUID(tenantID),
		ToAddresses: recipients,
		Subject:     subject,
		Template:    template,
	})
	if err != nil {
		return uuid.Nil, err
	}
	return row.ID, nil
}

func (s *Service) markSent(ctx context.Context, id uuid.UUID) {
	if id == uuid.Nil {
		return
	}
	_ = s.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		return sqlc.New(tx).MarkEmailSent(ctx, id)
	})
}

func (s *Service) markFailed(ctx context.Context, id uuid.UUID, reason string) {
	if id == uuid.Nil {
		return
	}
	r := reason
	_ = s.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		return sqlc.New(tx).MarkEmailFailed(ctx, sqlc.MarkEmailFailedParams{ID: id, Error: &r})
	})
}

// ---- low-level SMTP send (SSRF-hardened) -----------------------------------

// sendMail builds a multipart/alternative message (plaintext + HTML) and sends
// it over an SSRF-guarded go-mail client. The SMTP host is treated as untrusted
// even though only an owner can set it: the dialer pins the resolved IP and
// rejects private/metadata ranges, restricted to SMTP ports 25/465/587.
func sendMail(ctx context.Context, t Transport, recipients []string, em Email) error {
	if len(recipients) == 0 {
		return nil
	}
	msg := mail.NewMsg()
	if t.FromName != "" {
		if err := msg.FromFormat(t.FromName, t.From); err != nil {
			return fmt.Errorf("smtp from: %w", err)
		}
	} else if err := msg.From(t.From); err != nil {
		return fmt.Errorf("smtp from: %w", err)
	}
	if err := msg.To(recipients...); err != nil {
		return fmt.Errorf("smtp to: %w", err)
	}
	msg.Subject(em.Subject)
	// Plaintext primary + HTML alternative -> multipart/alternative with the
	// richer HTML part last, which is what clients prefer. When there is no
	// HTML part (e.g. SendMessage's unrendered alerts) skip the alternative
	// entirely — an EMPTY HTML part would still be the "preferred" part of a
	// multipart/alternative and could render blank in HTML-capable clients.
	msg.SetBodyString(mail.TypeTextPlain, em.Text)
	if em.HTML != "" {
		msg.AddAlternativeString(mail.TypeTextHTML, em.HTML)
	}

	port := t.Port
	if port == 0 {
		port = 587
	}
	opts := []mail.Option{
		mail.WithPort(port),
		mail.WithTimeout(20 * time.Second),
		mail.WithDialContextFunc(ssrfDialContext()),
	}
	switch strings.ToLower(t.TLSMode) {
	case "tls":
		opts = append(opts, mail.WithSSLPort(false), mail.WithTLSPolicy(mail.TLSMandatory))
	case "none":
		opts = append(opts, mail.WithTLSPolicy(mail.NoTLS))
	default: // starttls
		opts = append(opts, mail.WithTLSPolicy(mail.TLSMandatory))
	}
	if t.AllowInsecureTLS {
		opts = append(opts, mail.WithTLSConfig(insecureTLS()))
	}
	if t.Username != "" {
		opts = append(opts, mail.WithSMTPAuth(mail.SMTPAuthPlain), mail.WithUsername(t.Username), mail.WithPassword(t.Password))
	}

	client, err := mail.NewClient(t.Host, opts...)
	if err != nil {
		return fmt.Errorf("smtp client: %w", err)
	}
	if err := client.DialAndSendWithContext(ctx, msg); err != nil {
		return fmt.Errorf("smtp send: %w", err)
	}
	return nil
}

// ssrfDialContext returns a go-mail dial func whose net.Dialer.Control rejects
// SMTP connections to private/loopback/metadata IPs (resolve-then-pin), limited
// to the standard SMTP ports. Identical for production and the /test path.
func ssrfDialContext() func(ctx context.Context, network, address string) (net.Conn, error) {
	guardian := ssrf.New(ssrf.WithPorts(25, 465, 587))
	dialer := &net.Dialer{
		Timeout:   15 * time.Second,
		KeepAlive: 30 * time.Second,
		Control:   guardian.Safe,
	}
	return dialer.DialContext
}

// ---- small helpers ---------------------------------------------------------

// insecureTLS builds the TLS config used only when an owner explicitly enables
// allow_insecure_tls for a legacy internal relay with a self-signed cert.
func insecureTLS() *tls.Config {
	return &tls.Config{InsecureSkipVerify: true} //nolint:gosec // explicit owner opt-in for internal relays
}

// scrubSMTPError maps a raw SMTP/transport error onto a safe, operator-facing
// message that reveals NO internal IPs, hostnames, or timing — the test endpoint
// would otherwise be a port-scan / SSRF oracle. The full error is logged
// server-side by the caller.
func scrubSMTPError(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "prohibited"), strings.Contains(msg, "ssrf"), strings.Contains(msg, "not allowed"):
		return "The SMTP host is not allowed: it resolves to a private, loopback, or restricted address."
	case strings.Contains(msg, "535"), strings.Contains(msg, "auth"), strings.Contains(msg, "credential"):
		return "Authentication failed. Check the username and password."
	case strings.Contains(msg, "x509"), strings.Contains(msg, "certificate"), strings.Contains(msg, "tls"):
		return "TLS negotiation failed. Check the TLS mode, or enable insecure TLS only for a trusted internal relay."
	case strings.Contains(msg, "timeout"), strings.Contains(msg, "deadline"):
		return "Timed out connecting to the SMTP server. Check the host and port."
	case strings.Contains(msg, "no such host"), strings.Contains(msg, "lookup"):
		return "Could not resolve the SMTP host. Check the hostname."
	case strings.Contains(msg, "connection refused"), strings.Contains(msg, "dial"):
		return "Could not connect to the SMTP server. Check the host and port."
	default:
		return "The SMTP server rejected the message. Check the settings and try again."
	}
}

func setDefault(m map[string]any, k string, v any) {
	if _, ok := m[k]; !ok {
		m[k] = v
	}
}

func defaultPreview(template string) string {
	switch template {
	case "password_reset":
		return "Reset your WPMgr password."
	case "password_changed":
		return "Your WPMgr password was changed."
	case "verify_email":
		return "Verify your WPMgr email address."
	case "invite":
		return "You've been invited to WPMgr."
	case "test":
		return "Your WPMgr SMTP configuration works."
	case "backup_completed":
		return "A backup of your site finished successfully."
	case "backup_failed":
		return "A backup of your site did not complete."
	default:
		return "A message from WPMgr."
	}
}

// pgUUID converts a uuid.UUID to a pgtype.UUID (Valid=false for the zero UUID,
// which maps to SQL NULL for the tenant_id on instance/auth mail).
func pgUUID(id uuid.UUID) pgtype.UUID {
	if id == uuid.Nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: id, Valid: true}
}
