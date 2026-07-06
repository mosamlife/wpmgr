package uptime

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
)

// Audit action names for the uptime alert lifecycle.
const (
	ActionAlertSent          = "uptime.alert.sent"
	ActionAlertConfigChanged = "alert.config.changed"
)

// Transition is the decision the alert state machine makes for one probe result
// applied to a site's prior AlertState.
type Transition struct {
	// NewState is the state to persist after applying the probe.
	NewState AlertState
	// FireDown is true when this probe crossed the down threshold for the first
	// time (transition into incident) — fire ONE downtime alert.
	FireDown bool
	// FireRecovery is true when this probe recovered an open incident — fire ONE
	// recovery alert.
	FireRecovery bool
}

// Evaluate is the pure transition/de-dupe logic: given a site's prior state, the
// latest probe (up/down), and the consecutive-down threshold, it returns the
// next state and whether to fire a down or recovery alert. It alerts ONLY on a
// transition (down crossed the threshold while not already in an incident;
// recovery while in an incident), so the periodic evaluator never spams.
//
// "Downtime > N consecutive checks" fires when consecutive_down reaches N
// (threshold) AND we are not already in an incident.
func Evaluate(prev AlertState, up bool, threshold int, now time.Time) Transition {
	if threshold < 1 {
		threshold = 1
	}
	next := prev
	t := Transition{}

	if up {
		next.LastStatus = StatusUp
		next.ConsecutiveDown = 0
		if prev.InIncident {
			// Recovery transition: clear the incident and fire exactly one recovery.
			next.InIncident = false
			t.FireRecovery = true
			ts := now
			next.LastAlertAt = &ts
		}
		t.NewState = next
		return t
	}

	// Down probe.
	next.LastStatus = StatusDown
	next.ConsecutiveDown = prev.ConsecutiveDown + 1
	if !prev.InIncident && int(next.ConsecutiveDown) >= threshold {
		// First crossing of the threshold: open an incident and fire one alert.
		next.InIncident = true
		t.FireDown = true
		ts := now
		next.LastAlertAt = &ts
	}
	// Already in an incident, or not yet at threshold: no alert (de-dupe).
	t.NewState = next
	return t
}

// auditSink is the narrow slice of *audit.Recorder the Dispatcher needs. An
// interface so Fire/FireSecurityEvent are unit-testable with a stub sink
// instead of a live Postgres pool.
type auditSink interface {
	Record(ctx context.Context, e audit.Event) (audit.Entry, error)
}

// Dispatcher delivers fired alerts to a tenant's configured channels (email +
// webhook) and records an audit event carrying the REAL delivery outcome of
// each channel (GH #144 — the audit trail used to record "emailed"/"webhooked"
// as "a recipient/URL was configured", not "the send actually succeeded").
// Both channels are best-effort and independent: one failing is logged but
// does not block the other.
type Dispatcher struct {
	mailer  Mailer
	webhook WebhookPoster
	audit   auditSink
	logger  *slog.Logger
}

// NewDispatcher builds an alert Dispatcher.
func NewDispatcher(mailer Mailer, webhook WebhookPoster, rec auditSink, logger *slog.Logger) *Dispatcher {
	if logger == nil {
		logger = slog.Default()
	}
	return &Dispatcher{mailer: mailer, webhook: webhook, audit: rec, logger: logger}
}

// Fire delivers one alert to the tenant's channels and records it in the audit
// log WITH the real delivery outcome of each channel (GH #144). Returns nil
// even when a channel errors (delivery is best-effort and the transition has
// already been recorded by the caller); errors are logged.
func (d *Dispatcher) Fire(ctx context.Context, cfg AlertConfig, alert Alert) {
	subject, body := renderEmail(alert)
	emailResult, emailErr := d.sendEmail(ctx, cfg.EmailRecipients, subject, body)
	if emailErr != nil {
		d.logger.Warn("uptime alert email failed",
			slog.String("site_id", alert.SiteID.String()),
			slog.String("kind", string(alert.Kind)),
			slog.Any("error", emailErr))
	}

	payload := WebhookPayload{
		Event:      "uptime." + string(alert.Kind),
		TenantID:   alert.TenantID.String(),
		SiteID:     alert.SiteID.String(),
		SiteURL:    alert.SiteURL,
		SiteName:   alert.SiteName,
		HTTPStatus: alert.HTTPStatus,
		Error:      alert.Error,
		FiredAt:    alert.FiredAt,
	}
	webhookResult, webhookErr := d.sendWebhook(ctx, cfg.WebhookURL, cfg.WebhookSecret, payload)
	if webhookErr != nil {
		d.logger.Warn("uptime alert webhook failed",
			slog.String("site_id", alert.SiteID.String()),
			slog.String("kind", string(alert.Kind)),
			slog.Any("error", webhookErr))
	}

	d.recordAudit(ctx, alert, emailResult, webhookResult)
}

// sendEmail sends the alert email over the dispatcher's mailer (when one is
// wired and recipients are configured) and returns the REAL delivery outcome
// — never "sent" merely because recipients exist (GH #144). The returned error
// is the raw, unscrubbed error for server-side logging only; SendResult.Reason
// is always safe to persist (the Mailer implementation is responsible for
// scrubbing it, e.g. internal/mailer.Service via scrubSMTPError).
func (d *Dispatcher) sendEmail(ctx context.Context, recipients []string, subject, body string) (SendResult, error) {
	if d.mailer == nil || len(recipients) == 0 {
		return SendResult{Status: SendResultSkipped, Reason: "no_recipients_configured"}, nil
	}
	res, err := d.mailer.Send(ctx, recipients, subject, body)
	if err != nil {
		if res.Status == "" {
			res.Status = SendResultFailed
		}
		if res.Reason == "" {
			res.Reason = "send_failed"
		}
		return res, err
	}
	if res.Status == "" {
		res.Status = SendResultSent
	}
	return res, nil
}

// sendWebhook POSTs the alert webhook (when configured) and returns the REAL
// delivery outcome, with a coarse/scrubbed reason on failure that NEVER
// includes the endpoint's response body.
func (d *Dispatcher) sendWebhook(ctx context.Context, url, secret string, payload WebhookPayload) (SendResult, error) {
	if d.webhook == nil || url == "" {
		return SendResult{Status: SendResultSkipped, Reason: "webhook_not_configured"}, nil
	}
	if err := d.webhook.Post(ctx, url, secret, payload); err != nil {
		return SendResult{Status: SendResultFailed, Reason: classifyWebhookError(err)}, err
	}
	return SendResult{Status: SendResultSent}, nil
}

// FireSecurityEvent delivers a high-severity ADR-037 activity-log event to the
// tenant's configured channels, reusing the SAME Mailer + WebhookPoster as the
// uptime down/recovery path (no parallel notification system). The caller is
// responsible for gating on cfg.NotifySecurity; this method always delivers.
// Both channels are best-effort and independent.
func (d *Dispatcher) FireSecurityEvent(ctx context.Context, cfg AlertConfig, ev SecurityEvent) {
	name := ev.SiteName
	if name == "" {
		name = ev.SiteURL
	}
	if name == "" {
		name = ev.SiteID.String()
	}
	subject := fmt.Sprintf("[WPMgr] SECURITY: event on %s", name)
	body := fmt.Sprintf("Security event on %s: %s\n\nEvent: %s\nSeverity: %s\nDetected at: %s",
		name, ev.Summary, ev.EventType, ev.Severity, ev.FiredAt.UTC().Format(time.RFC3339))

	emailResult, emailErr := d.sendEmail(ctx, cfg.EmailRecipients, subject, body)
	if emailErr != nil {
		d.logger.Warn("security alert email failed",
			slog.String("site_id", ev.SiteID.String()),
			slog.String("event_type", ev.EventType),
			slog.Any("error", emailErr))
	}

	payload := WebhookPayload{
		Event:    "security." + ev.EventType,
		TenantID: ev.TenantID.String(),
		SiteID:   ev.SiteID.String(),
		SiteURL:  ev.SiteURL,
		SiteName: ev.SiteName,
		Error:    ev.Summary,
		FiredAt:  ev.FiredAt,
	}
	webhookResult, webhookErr := d.sendWebhook(ctx, cfg.WebhookURL, cfg.WebhookSecret, payload)
	if webhookErr != nil {
		d.logger.Warn("security alert webhook failed",
			slog.String("site_id", ev.SiteID.String()),
			slog.String("event_type", ev.EventType),
			slog.Any("error", webhookErr))
	}

	if d.audit != nil {
		meta := channelMetadata(emailResult, webhookResult)
		meta["kind"] = string(AlertSecurity)
		meta["event_type"] = ev.EventType
		meta["severity"] = ev.Severity
		meta["summary"] = ev.Summary
		meta["site_url"] = ev.SiteURL
		_, _ = d.audit.Record(ctx, audit.Event{
			TenantID:   ev.TenantID,
			ActorType:  audit.ActorSystem,
			Action:     ActionAlertSent,
			TargetType: "site",
			TargetID:   ev.SiteID.String(),
			Metadata:   meta,
		})
	}
}

// channelMetadata builds the shared email/webhook outcome fields persisted on
// every alert audit row: "email_status"/"webhook_status" always, plus
// "email_reason"/"webhook_reason" ONLY when the corresponding channel did not
// succeed (both are coarse, already-scrubbed codes — never raw SMTP/webhook
// error detail, credentials, or response bodies).
func channelMetadata(email, webhook SendResult) map[string]any {
	meta := map[string]any{
		"email_status":   email.Status,
		"webhook_status": webhook.Status,
	}
	if email.Status != SendResultSent && email.Reason != "" {
		meta["email_reason"] = email.Reason
	}
	if webhook.Status != SendResultSent && webhook.Reason != "" {
		meta["webhook_reason"] = webhook.Reason
	}
	return meta
}

func (d *Dispatcher) recordAudit(ctx context.Context, alert Alert, email, webhook SendResult) {
	if d.audit == nil {
		return
	}
	meta := channelMetadata(email, webhook)
	meta["kind"] = string(alert.Kind)
	meta["site_url"] = alert.SiteURL
	meta["http_status"] = alert.HTTPStatus
	meta["error"] = alert.Error
	_, _ = d.audit.Record(ctx, audit.Event{
		TenantID:   alert.TenantID,
		ActorType:  audit.ActorSystem,
		Action:     ActionAlertSent,
		TargetType: "site",
		TargetID:   alert.SiteID.String(),
		Metadata:   meta,
	})
}

func renderEmail(a Alert) (subject, body string) {
	name := a.SiteName
	if name == "" {
		name = a.SiteURL
	}
	if a.Kind == AlertRecovery {
		subject = fmt.Sprintf("[WPMgr] RECOVERED: %s is back up", name)
		body = fmt.Sprintf("Your site %s (%s) has recovered and is responding again as of %s.",
			name, a.SiteURL, a.FiredAt.UTC().Format(time.RFC3339))
		return subject, body
	}
	// wp_fatal_error/wp_db_error (issue #132) are WordPress fatal-error pages
	// served with HTTP 200 — the site DID respond, so "is unreachable" is
	// misleading; give operators copy that matches what scanFatal actually
	// found instead of a generic reachability message.
	detail := a.Error
	switch a.Error {
	case "wp_fatal_error":
		subject = fmt.Sprintf("[WPMgr] DOWN: %s is serving a critical-error page (HTTP 200)", name)
		detail = "WordPress critical error: the site returns HTTP 200 with a fatal-error page"
	case "wp_db_error":
		subject = fmt.Sprintf("[WPMgr] DOWN: %s has a database connection error", name)
		detail = "Error establishing a database connection (site returns HTTP 200)"
	default:
		subject = fmt.Sprintf("[WPMgr] DOWN: %s is unreachable", name)
		if detail == "" && a.HTTPStatus > 0 {
			detail = fmt.Sprintf("HTTP %d", a.HTTPStatus)
		}
	}
	body = fmt.Sprintf("Your site %s (%s) appears to be DOWN as of %s.\nDetail: %s",
		name, a.SiteURL, a.FiredAt.UTC().Format(time.RFC3339), detail)
	return subject, body
}
