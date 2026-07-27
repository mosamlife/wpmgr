package uptime

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
)

// Audit action names for the uptime alert lifecycle.
const (
	ActionAlertSent          = "uptime.alert.sent"
	ActionAlertConfigChanged = "alert.config.changed"
	// ActionAppHealthSettingsChanged (m108, GH #291 Phase 3) - a site's
	// app-health settings (app_probe_path / app_alerts_disabled) were saved.
	ActionAppHealthSettingsChanged = "site.app_health_settings.changed"
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
// includes the endpoint's response body. payload is `any` so this helper
// serves any alert-producing domain's payload shape (see WebhookPoster doc).
func (d *Dispatcher) sendWebhook(ctx context.Context, url, secret string, payload any) (SendResult, error) {
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

// PostSignedWebhook posts an arbitrary JSON-serializable payload to url,
// signed with the SAME HMAC-SHA256-over-body scheme (X-WPMgr-Signature
// header) and delivered over the SAME SSRF-hardened client as
// Fire/FireSecurityEvent. It is a thin passthrough to sendWebhook, exported
// so other alert-producing domains can reuse this channel's signing +
// delivery exactly instead of duplicating it — e.g. internal/vuln's batched
// vulnerability-findings webhook (m103, GH #247), which has its own payload
// shape (spanning multiple sites) that does not fit the uptime-specific
// WebhookPayload struct. Returns an error only on an actual delivery failure
// (never merely "no webhook configured" — an empty url is a silent no-op,
// mirroring sendWebhook's SendResultSkipped case).
func (d *Dispatcher) PostSignedWebhook(ctx context.Context, url, secret string, payload any) error {
	if url == "" {
		return nil
	}
	_, err := d.sendWebhook(ctx, url, secret, payload)
	return err
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
	// m108 (GH #291 Phase 3): the app-health kinds get their own copy -
	// deliberately worded to never say the site is "down" (reachability's
	// word), since a page cache can keep visitors served while the
	// application itself fails its health check.
	switch a.Kind {
	case AlertAppRecovery:
		subject = fmt.Sprintf("[WPMgr] APP RECOVERED: %s is responding again", name)
		body = fmt.Sprintf("Your site %s (%s) failed its application-health check and has now recovered, as of %s.\n\nThis is a SEPARATE signal from ordinary uptime: visitors may have kept seeing a cached page throughout the incident.",
			name, a.SiteURL, a.FiredAt.UTC().Format(time.RFC3339))
		return subject, body
	case AlertAppDown:
		subject = fmt.Sprintf("[WPMgr] APP DOWN: %s's application is not responding (the site may still LOOK up)", name)
		body = fmt.Sprintf("Your site %s (%s) is failing its application-health check as of %s.\nDetail: %s\n\nThis is independent of ordinary uptime: a page cache can keep serving visitors a stale copy while WordPress itself is not responding. wp-admin, logins, forms and checkout are likely already broken.",
			name, a.SiteURL, a.FiredAt.UTC().Format(time.RFC3339), appProbeReasonCopy(a.Error))
		return subject, body
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

// appProbeReasonCopy maps an AppProbeReason* value (app_probe.go) carried on
// Alert.Error for an AlertAppDown alert onto human copy. Only the two
// CONCLUSIVE-false reasons (AppProbeReasonRest5xx, AppProbeReasonWPFatalError)
// can ever reach here - EvaluateApp only fires on AppVerdictDown, which
// app_probe.go only ever classifies from those two - but the default branch
// stays defensive rather than assuming that invariant holds forever.
func appProbeReasonCopy(reason string) string {
	switch reason {
	case AppProbeReasonRest5xx:
		return "the application-health check returned HTTP 500 (PHP itself returned an error)"
	case AppProbeReasonWPFatalError:
		return "the application-health check received a WordPress critical-error page (HTTP 200 with a fatal-error screen)"
	case "":
		return "application-health check failed"
	default:
		return "application-health check failed (" + reason + ")"
	}
}

// FireAppAggregate delivers the fleet circuit breaker's aggregate
// notification (m108, GH #291 Phase 3 section 2) to the tenant's configured
// channels and records the SAME kind of audit trail as Fire, keyed on the
// tenant itself (there is no single site to attribute this to). Reuses the
// SAME email/webhook/audit machinery as every other alert this dispatcher
// sends - no parallel notification system.
func (d *Dispatcher) FireAppAggregate(ctx context.Context, cfg AlertConfig, alert AppAggregateAlert) {
	subject, body := renderAppAggregateEmail(alert)
	emailResult, emailErr := d.sendEmail(ctx, cfg.EmailRecipients, subject, body)
	if emailErr != nil {
		d.logger.Warn("uptime app aggregate alert email failed",
			slog.String("tenant_id", alert.TenantID.String()),
			slog.Bool("recovered", alert.Recovered),
			slog.Any("error", emailErr))
	}

	event := "uptime.app_down_aggregate"
	switch {
	case alert.Recovered:
		event = "uptime.app_recovery_aggregate"
	case alert.Updated:
		event = "uptime.app_down_aggregate_update"
	}
	payload := WebhookPayload{
		Event:              event,
		TenantID:           alert.TenantID.String(),
		Error:              fmt.Sprintf("%d/%d alert-eligible sites app-down", alert.DownCount, alert.EligibleCount),
		FiredAt:            alert.FiredAt,
		AppDownCount:       alert.DownCount,
		AppEligibleCount:   alert.EligibleCount,
		AppSuppressedSites: alert.SuppressedSites,
	}
	webhookResult, webhookErr := d.sendWebhook(ctx, cfg.WebhookURL, cfg.WebhookSecret, payload)
	if webhookErr != nil {
		d.logger.Warn("uptime app aggregate alert webhook failed",
			slog.String("tenant_id", alert.TenantID.String()),
			slog.Bool("recovered", alert.Recovered),
			slog.Any("error", webhookErr))
	}

	if d.audit == nil {
		return
	}
	meta := channelMetadata(emailResult, webhookResult)
	meta["kind"] = event
	meta["down_count"] = alert.DownCount
	meta["eligible_count"] = alert.EligibleCount
	meta["suppressed_sites"] = alert.SuppressedSites
	_, _ = d.audit.Record(ctx, audit.Event{
		TenantID:   alert.TenantID,
		ActorType:  audit.ActorSystem,
		Action:     ActionAlertSent,
		TargetType: "tenant",
		TargetID:   alert.TenantID.String(),
		Metadata:   meta,
	})
}

// renderAppAggregateEmail renders the circuit-breaker's own notification.
// The body ALWAYS states the exact count and names what was suppressed (GH
// #291 Phase 3 section 2: "so nothing is silently swallowed") - never a bare
// "something is wrong" message.
func renderAppAggregateEmail(a AppAggregateAlert) (subject, body string) {
	if a.Recovered {
		subject = fmt.Sprintf("[WPMgr] APP ALERTS RESUMED: fleet-wide app-health issue has cleared (%d/%d sites)", a.DownCount, a.EligibleCount)
		body = fmt.Sprintf("The fleet-wide application-health issue detected across your account has cleared as of %s.\n\n%d of %d alert-eligible sites are currently app-down (below the alert ratio). Individual per-site app-health alerts have resumed.",
			a.FiredAt.UTC().Format(time.RFC3339), a.DownCount, a.EligibleCount)
		return subject, body
	}
	if a.Updated {
		// Fix 3: still tripped, but materially worse than the original trip
		// notification - deliberately DIFFERENT subject/copy from the
		// initial SUPPRESSED notification below, so an operator scanning
		// their inbox can tell this is an update, not a duplicate.
		// SuppressedSites here is the LIVE, currently-down set (see the
		// AppAggregateAlert.SuppressedSites doc comment) - "currently
		// affected", not "newly affected since last time".
		subject = fmt.Sprintf("[WPMgr] APP ALERTS STILL SUPPRESSED: now %d/%d sites are simultaneously app-down", a.DownCount, a.EligibleCount)
		affected := "none named"
		if len(a.SuppressedSites) > 0 {
			affected = strings.Join(a.SuppressedSites, ", ")
		}
		body = fmt.Sprintf(
			"The fleet-wide application-health issue detected on your account has WORSENED: %d of %d alert-eligible sites are now simultaneously failing their application-health check, as of %s (up from the count in the last notification).\n\n"+
				"Currently affected sites: %s\n\n"+
				"Individual per-site app-down alerts remain SUPPRESSED and collapsed into this single notification. You will get exactly one more notification when this clears.",
			a.DownCount, a.EligibleCount, a.FiredAt.UTC().Format(time.RFC3339), affected,
		)
		return subject, body
	}
	subject = fmt.Sprintf("[WPMgr] APP ALERTS SUPPRESSED: %d/%d sites are simultaneously app-down", a.DownCount, a.EligibleCount)
	suppressed := "none named"
	if len(a.SuppressedSites) > 0 {
		suppressed = strings.Join(a.SuppressedSites, ", ")
	}
	body = fmt.Sprintf(
		"%d of %d alert-eligible sites on your account are simultaneously failing their application-health check, as of %s.\n\n"+
			"This many sites breaking at the same time is far more likely to be a shared host/network issue, or this monitoring feature itself, than %d unrelated sites breaking independently - so individual per-site app-down alerts have been SUPPRESSED and collapsed into this single notification instead.\n\n"+
			"Suppressed sites: %s\n\n"+
			"You will get exactly one more notification when this clears.",
		a.DownCount, a.EligibleCount, a.FiredAt.UTC().Format(time.RFC3339), a.DownCount, suppressed,
	)
	return subject, body
}
