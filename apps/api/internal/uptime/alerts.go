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

// Dispatcher delivers fired alerts to a tenant's configured channels (email +
// webhook) and records an audit event. Both channels are best-effort and
// independent: one failing is logged but does not block the other.
type Dispatcher struct {
	mailer  Mailer
	webhook WebhookPoster
	audit   *audit.Recorder
	logger  *slog.Logger
}

// NewDispatcher builds an alert Dispatcher.
func NewDispatcher(mailer Mailer, webhook WebhookPoster, rec *audit.Recorder, logger *slog.Logger) *Dispatcher {
	if logger == nil {
		logger = slog.Default()
	}
	return &Dispatcher{mailer: mailer, webhook: webhook, audit: rec, logger: logger}
}

// Fire delivers one alert to the tenant's channels and records it in the audit
// log. Returns nil even when a channel errors (delivery is best-effort and the
// transition has already been recorded by the caller); errors are logged.
func (d *Dispatcher) Fire(ctx context.Context, cfg AlertConfig, alert Alert) {
	subject, body := renderEmail(alert)
	if d.mailer != nil && len(cfg.EmailRecipients) > 0 {
		if err := d.mailer.Send(ctx, cfg.EmailRecipients, subject, body); err != nil {
			d.logger.Warn("uptime alert email failed",
				slog.String("site_id", alert.SiteID.String()),
				slog.String("kind", string(alert.Kind)),
				slog.Any("error", err))
		}
	}
	if d.webhook != nil && cfg.WebhookURL != "" {
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
		if err := d.webhook.Post(ctx, cfg.WebhookURL, cfg.WebhookSecret, payload); err != nil {
			d.logger.Warn("uptime alert webhook failed",
				slog.String("site_id", alert.SiteID.String()),
				slog.String("kind", string(alert.Kind)),
				slog.Any("error", err))
		}
	}
	d.recordAudit(ctx, alert, len(cfg.EmailRecipients) > 0, cfg.WebhookURL != "")
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

	if d.mailer != nil && len(cfg.EmailRecipients) > 0 {
		if err := d.mailer.Send(ctx, cfg.EmailRecipients, subject, body); err != nil {
			d.logger.Warn("security alert email failed",
				slog.String("site_id", ev.SiteID.String()),
				slog.String("event_type", ev.EventType),
				slog.Any("error", err))
		}
	}
	if d.webhook != nil && cfg.WebhookURL != "" {
		payload := WebhookPayload{
			Event:    "security." + ev.EventType,
			TenantID: ev.TenantID.String(),
			SiteID:   ev.SiteID.String(),
			SiteURL:  ev.SiteURL,
			SiteName: ev.SiteName,
			Error:    ev.Summary,
			FiredAt:  ev.FiredAt,
		}
		if err := d.webhook.Post(ctx, cfg.WebhookURL, cfg.WebhookSecret, payload); err != nil {
			d.logger.Warn("security alert webhook failed",
				slog.String("site_id", ev.SiteID.String()),
				slog.String("event_type", ev.EventType),
				slog.Any("error", err))
		}
	}
	if d.audit != nil {
		_, _ = d.audit.Record(ctx, audit.Event{
			TenantID:   ev.TenantID,
			ActorType:  audit.ActorSystem,
			Action:     ActionAlertSent,
			TargetType: "site",
			TargetID:   ev.SiteID.String(),
			Metadata: map[string]any{
				"kind":       string(AlertSecurity),
				"event_type": ev.EventType,
				"severity":   ev.Severity,
				"summary":    ev.Summary,
				"site_url":   ev.SiteURL,
				"emailed":    len(cfg.EmailRecipients) > 0,
				"webhooked":  cfg.WebhookURL != "",
			},
		})
	}
}

func (d *Dispatcher) recordAudit(ctx context.Context, alert Alert, emailed, webhooked bool) {
	if d.audit == nil {
		return
	}
	_, _ = d.audit.Record(ctx, audit.Event{
		TenantID:   alert.TenantID,
		ActorType:  audit.ActorSystem,
		Action:     ActionAlertSent,
		TargetType: "site",
		TargetID:   alert.SiteID.String(),
		Metadata: map[string]any{
			"kind":        string(alert.Kind),
			"site_url":    alert.SiteURL,
			"http_status": alert.HTTPStatus,
			"error":       alert.Error,
			"emailed":     emailed,
			"webhooked":   webhooked,
		},
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
	subject = fmt.Sprintf("[WPMgr] DOWN: %s is unreachable", name)
	detail := a.Error
	if detail == "" && a.HTTPStatus > 0 {
		detail = fmt.Sprintf("HTTP %d", a.HTTPStatus)
	}
	body = fmt.Sprintf("Your site %s (%s) appears to be DOWN as of %s.\nDetail: %s",
		name, a.SiteURL, a.FiredAt.UTC().Format(time.RFC3339), detail)
	return subject, body
}
