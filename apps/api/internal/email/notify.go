package email

// notify.go — alert + digest business logic for the email domain (m62 Area 4).
//
// Alert path (per IngestLogBatch call for agent-ingested status=failed):
//   maybeAlertFailures → AccumulateAlertFailures → ClaimAlertSlot
//     → enqueue send_email "email_failure_alert"
//
// Digest path (DigestWorker, hourly periodic):
//   ListDueDigests → per-tenant: ClaimAdvanceDigest → GetFleetStatsBySite
//     → TopFailureSamples → enqueue send_email "email_digest"

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/mailer"
)

// MailerEnqueuer is the subset of *mailer.Enqueuer the email service needs.
// Declared locally so the email package doesn't create a direct import cycle
// with internal/mailer and remains unit-testable with a fake.
type MailerEnqueuer interface {
	Enqueue(ctx context.Context, tenantID uuid.UUID, recipients []string, template string, data map[string]any) error
	// EnqueueTx enqueues within the caller's transaction (transactional
	// outbox — see maybeAlertFailures/Repo.ClaimAlertSlot). *mailer.Enqueuer
	// satisfies this via its own EnqueueTx method.
	EnqueueTx(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, recipients []string, template string, data map[string]any) error
}

// errAlertEnqueueFailed marks a ClaimAlertSlot failure that originated in the
// onClaim callback (the mailer enqueue) rather than in the claim UPDATE
// itself, so maybeAlertFailures can log the correct reason
// ("enqueue_failed" vs "claim_alert_slot_error") after the transaction has
// already rolled back.
var errAlertEnqueueFailed = errors.New("email alert enqueue failed")

// MailerStatus reports whether the instance mailer is currently configured.
// *mailer.Service satisfies this interface.
type MailerStatus interface {
	Enabled(ctx context.Context) bool
}

// VulnDigestSource supplies the "new vulnerabilities" section of the email
// digest (m103, GH #247). Declared locally (mirrors MailerEnqueuer) so
// internal/email never imports internal/uptime or internal/vuln directly;
// the adapter wired in cmd/wpmgr/main.go composes *uptime.Service (the
// vuln_include_in_digest gate, which lives on the alert_configs table, NOT
// this package's own NotifySettings) with *vuln.Service (open-finding data)
// to satisfy it.
type VulnDigestSource interface {
	// GetVulnDigestSummary returns the tenant's current open-vulnerability
	// summary and whether the digest should include it at all. When
	// included=false the caller must not render the section regardless of
	// summary.OpenCount (e.g. the flag is off, or the read itself failed).
	GetVulnDigestSummary(ctx context.Context, tenantID uuid.UUID) (summary VulnDigestSummary, included bool, err error)
}

// VulnDigestSummary is the vulnerability data for one tenant's digest section.
type VulnDigestSummary struct {
	OpenCount         int
	CriticalHighCount int
	// Top holds at most 5 findings, most-severe first.
	Top []VulnDigestItem
}

// VulnDigestItem is one finding row in the digest's "top vulnerabilities" list.
type VulnDigestItem struct {
	SiteName  string
	Component string
	Severity  string
	CVE       string
}

// maxAlertFailureSamples is the maximum number of recent failure samples
// included in a per-failure alert email body.
const maxAlertFailureSamples = 5

// alertGoroutineTimeout bounds the detached maybeAlertFailures goroutine
// launched from IngestLogBatch (service.go). It is detached deliberately to
// keep the ingest path fast, so a stalled downstream call (settings lookup,
// alert-slot claim, site-ref fetch, mailer enqueue) must not leak the
// goroutine forever. The work itself is a handful of single-row reads/writes
// plus one enqueue call, so 30s is generous headroom over the expected case
// while still bounding the worst case (GH #381 phase 5).
const alertGoroutineTimeout = 30 * time.Second

// alertDecisionEvent is the single grep-able event name for every decision
// maybeAlertFailures makes, whether or not it ends in a sent alert (GH #381
// phase 5). The issue this answers: a user reports no alert email and there
// is nothing to grep. Every exit path below now logs exactly one record
// carrying this event name and a `reason` field distinguishing it.
//
// NEVER add a recipient address to these fields. The failure_count is enough
// to diagnose; recipient addresses would land in operator logs and in
// whatever ships them onward.
const alertDecisionEvent = "email_alert_decision"

// logAlertDecision emits the single structured record for one exit path of
// maybeAlertFailures. errAttr may be nil.
func (s *Service) logAlertDecision(ctx context.Context, level slog.Level, reason string, tenantID, siteID uuid.UUID, failureCount int, errAttr error) {
	attrs := []slog.Attr{
		slog.String("reason", reason),
		slog.String("tenant_id", tenantID.String()),
		slog.String("site_id", siteID.String()),
		slog.Int("failure_count", failureCount),
	}
	if errAttr != nil {
		attrs = append(attrs, slog.Any("error", errAttr))
	}
	s.log.LogAttrs(ctx, level, alertDecisionEvent, attrs...)
}

// maybeAlertFailuresAsync is the entry point IngestLogBatch launches with
// `go`. It owns the goroutine's lifetime end to end: a bounded context so a
// stalled downstream call cannot leak the goroutine past alertGoroutineTimeout,
// and a recover() so a panic anywhere below cannot take down the API process
// serving every other tenant's ingest traffic — there was no recover() here
// before phase 5, so any panic in this detached goroutine was fatal to the
// whole binary.
func (s *Service) maybeAlertFailuresAsync(tenantID, siteID uuid.UUID, failureCount int) {
	ctx, cancel := context.WithTimeout(context.Background(), alertGoroutineTimeout)
	defer cancel()

	defer func() {
		if r := recover(); r != nil {
			s.log.LogAttrs(ctx, slog.LevelError, alertDecisionEvent,
				slog.String("reason", "panic"),
				slog.String("tenant_id", tenantID.String()),
				slog.String("site_id", siteID.String()),
				slog.Int("failure_count", failureCount),
				slog.Any("panic", r),
			)
		}
	}()

	s.maybeAlertFailures(ctx, tenantID, siteID, failureCount)
}

// maxDigestTopFailures is the maximum number of top-failure samples in digest.
const maxDigestTopFailures = 5

// maxDigestSites is the maximum number of per-site rows in digest.
const maxDigestSites = 20

// maybeAlertFailures is called best-effort after IngestLogBatch when any
// entries have status=failed. It accumulates the failure count, claims an
// alert slot if the throttle window has passed, then enqueues the alert email.
// All failures here are logged but never surfaced to the caller — this is
// strictly best-effort (the save has already succeeded).
func (s *Service) maybeAlertFailures(ctx context.Context, tenantID, siteID uuid.UUID, failureCount int) {
	if s.mailer == nil {
		s.logAlertDecision(ctx, slog.LevelWarn, "nil_mailer", tenantID, siteID, failureCount, nil)
		return
	}
	if failureCount <= 0 {
		// Unreachable in production: the sole caller (IngestLogBatch,
		// service.go) only launches this goroutine when failureCount > 0.
		// No log line here deliberately — see notify.go's phase-5 finding.
		return
	}

	// Accumulate failures into the per-site state row.
	if err := s.repo.AccumulateAlertFailures(ctx, tenantID, siteID, int64(failureCount)); err != nil {
		s.logAlertDecision(ctx, slog.LevelWarn, "accumulate_failed", tenantID, siteID, failureCount, err)
		return
	}

	// Try to claim an alert slot (single-statement conditional UPDATE).
	settings, err := s.repo.GetNotifySettings(ctx, tenantID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// No settings row = use defaults (but don't alert when disabled).
			s.logAlertDecision(ctx, slog.LevelDebug, "no_settings_row", tenantID, siteID, failureCount, nil)
		} else {
			s.logAlertDecision(ctx, slog.LevelWarn, "settings_lookup_error", tenantID, siteID, failureCount, err)
		}
		return
	}
	if !settings.Enabled || !settings.AlertOnFailure {
		reason := "disabled"
		if settings.Enabled {
			reason = "alert_on_failure_off"
		}
		s.logAlertDecision(ctx, slog.LevelDebug, reason, tenantID, siteID, failureCount, nil)
		return
	}
	if len(settings.Recipients) == 0 {
		s.logAlertDecision(ctx, slog.LevelDebug, "no_recipients", tenantID, siteID, failureCount, nil)
		return
	}

	// Resolve the email content BEFORE claiming the slot — mirrors
	// internal/vuln's dispatchTenant (fetch everything the send needs first,
	// THEN claim + enqueue atomically as the last step). This also means a
	// site-ref lookup failure can never land after the claim has already
	// committed the failures-since-alert reset.
	siteRef, refErr := s.repo.GetSiteRef(ctx, tenantID, siteID)
	if refErr != nil {
		s.logAlertDecision(ctx, slog.LevelWarn, "site_ref_error", tenantID, siteID, failureCount, refErr)
		return
	}

	// Fetch top failure samples (no bodies — privacy minimalism).
	samples, sErr := s.repo.TopFailureSamplesBySite(ctx, tenantID, siteID, time.Now().UTC().Add(-time.Duration(settings.AlertThrottleMinutes)*time.Minute), time.Now().UTC(), maxAlertFailureSamples)
	if sErr != nil {
		samples = nil // non-fatal
	}

	type sampleDTO struct {
		Subject  string
		To       string
		Provider string
		Error    string
	}
	dtoSamples := make([]sampleDTO, 0, len(samples))
	for _, smp := range samples {
		// Truncate error to 200 chars per spec (never bodies).
		errStr := smp.Error
		if len(errStr) > 200 {
			errStr = errStr[:200] + "..."
		}
		dtoSamples = append(dtoSamples, sampleDTO{
			Subject: smp.Subject,
			Error:   errStr,
		})
	}

	dashURL := fmt.Sprintf("%s/sites/%s/email/log", s.publicBase, siteID.String())

	data := map[string]any{
		"SiteName":      siteRef.Name,
		"SiteURL":       siteRef.URL,
		"SiteEmailURL":  dashURL,
		"FailureCount":  failureCount,
		"WindowMinutes": settings.AlertThrottleMinutes,
		"Samples":       dtoSamples,
	}

	// Claim the alert slot and enqueue the email in the SAME transaction
	// (transactional outbox — see Repo.ClaimAlertSlot). If the enqueue
	// fails, the whole transaction rolls back: the failures_since_alert
	// reset and last_alert_at bump revert together with the failed send, so
	// a genuine enqueue failure can never leave the site silently marked as
	// recently-alerted (GH #381's original bug, reintroduced by claiming and
	// enqueueing as two steps that could disagree).
	//
	// This still cannot double-send: the claim UPDATE row-locks
	// email_alert_state for the life of the transaction, so a second,
	// concurrent tick blocks on that row until this one commits or rolls
	// back. If it commits, the concurrent tick re-evaluates the throttle
	// predicate against the now-reset row and finds nothing to claim. If it
	// rolls back, the row reverts to its pre-claim state and the concurrent
	// tick is free to claim and send — never both.
	claimedState, claimErr := s.repo.ClaimAlertSlot(ctx, tenantID, siteID, 1, settings.AlertThrottleMinutes,
		func(tx pgx.Tx) error {
			if err := s.mailer.EnqueueTx(ctx, tx, tenantID, settings.Recipients, "email_failure_alert", data); err != nil {
				return fmt.Errorf("%w: %v", errAlertEnqueueFailed, err)
			}
			return nil
		})
	if claimErr != nil {
		reason := "claim_alert_slot_error"
		if errors.Is(claimErr, errAlertEnqueueFailed) {
			reason = "enqueue_failed"
		}
		s.logAlertDecision(ctx, slog.LevelWarn, reason, tenantID, siteID, failureCount, claimErr)
		return
	}
	if claimedState == nil {
		// Throttled — not an error, just not our turn yet.
		s.logAlertDecision(ctx, slog.LevelDebug, "throttled", tenantID, siteID, failureCount, nil)
		return
	}
	s.logAlertDecision(ctx, slog.LevelInfo, "alert_sent", tenantID, siteID, failureCount, nil)
}

// buildDigestData gathers per-tenant digest data for a given [from, to] window.
// Returns nil when the total is 0 (skip-send) per spec.
func (s *Service) buildDigestData(ctx context.Context, tenantID uuid.UUID, settings NotifySettings, from, to time.Time) (map[string]any, error) {
	siteRows, err := s.repo.GetFleetStatsBySite(ctx, tenantID, from, to, maxDigestSites)
	if err != nil {
		return nil, err
	}

	var total, sentCount, failedCount, bouncedCount int64
	for _, row := range siteRows {
		total += row.Total
		sentCount += row.SentCount
		failedCount += row.FailedCount
		bouncedCount += row.BouncedCount
	}

	// m103 (GH #247): fetch the vulnerability-digest section, gated on
	// alert_configs.vuln_include_in_digest (a flag on a DIFFERENT table than
	// this tenant's own email digest settings). Best-effort: a read failure
	// here must never block the rest of the digest — the section is simply
	// omitted, exactly like vulnIncluded=false.
	var vulnSummary VulnDigestSummary
	var vulnIncluded bool
	if s.vulnDigest != nil {
		vs, included, vErr := s.vulnDigest.GetVulnDigestSummary(ctx, tenantID)
		if vErr != nil {
			s.log.Warn("email digest: vuln summary fetch failed",
				slog.String("tenant_id", tenantID.String()), slog.Any("error", vErr))
		} else {
			vulnSummary, vulnIncluded = vs, included
		}
	}

	// Widened skip-send: previously this skipped whenever the EMAIL total was
	// 0, which would silently suppress a digest that has NOTHING to report
	// except open vulnerabilities. Skip only when there is truly nothing in
	// either section.
	if total == 0 && (!vulnIncluded || vulnSummary.OpenCount == 0) {
		return nil, nil // skip-send per spec
	}

	topFailures, fErr := s.repo.TopFailureSamples(ctx, tenantID, from, to, maxDigestTopFailures)
	if fErr != nil {
		topFailures = nil // non-fatal
	}

	type siteRow struct {
		SiteName string
		SiteURL  string
		Sent     int64
		Failed   int64
		Bounced  int64
	}
	type failureRow struct {
		SiteName string
		Subject  string
		Error    string
	}

	siteList := make([]siteRow, 0, len(siteRows))
	for _, r := range siteRows {
		ref, refErr := s.repo.GetSiteRef(ctx, tenantID, r.SiteID)
		siteName := r.SiteID.String()
		siteURL := ""
		if refErr == nil {
			siteName = ref.Name
			siteURL = ref.URL
		}
		siteList = append(siteList, siteRow{
			SiteName: siteName,
			SiteURL:  siteURL,
			Sent:     r.SentCount,
			Failed:   r.FailedCount,
			Bounced:  r.BouncedCount,
		})
	}

	failureList := make([]failureRow, 0, len(topFailures))
	for _, f := range topFailures {
		ref, refErr := s.repo.GetSiteRef(ctx, tenantID, f.SiteID)
		siteName := f.SiteID.String()
		if refErr == nil {
			siteName = ref.Name
		}
		errStr := f.Error
		if len(errStr) > 200 {
			errStr = errStr[:200] + "..."
		}
		failureList = append(failureList, failureRow{
			SiteName: siteName,
			Subject:  f.Subject,
			Error:    errStr,
		})
	}

	periodLabel := computePeriodLabel(settings.DigestCadence, from)

	data := map[string]any{
		"PeriodLabel":  periodLabel,
		"From":         from.Format("2006-01-02"),
		"To":           to.Format("2006-01-02"),
		"Total":        total,
		"SentCount":    sentCount,
		"FailedCount":  failedCount,
		"BouncedCount": bouncedCount,
		"SiteCount":    int64(len(siteRows)),
		"Sites":        siteList,
		"TopFailures":  failureList,
		"DashboardURL": s.publicBase + "/email",
	}

	if vulnIncluded {
		topVulns := make([]map[string]any, 0, len(vulnSummary.Top))
		for _, v := range vulnSummary.Top {
			topVulns = append(topVulns, map[string]any{
				"SiteName":  v.SiteName,
				"Component": v.Component,
				"Severity":  v.Severity,
				"CVE":       v.CVE,
			})
		}
		data["OpenVulnCount"] = vulnSummary.OpenCount
		data["CriticalHighCount"] = vulnSummary.CriticalHighCount
		data["TopVulns"] = topVulns
		data["VulnDashboardURL"] = s.publicBase + "/vulnerabilities"
	}

	return data, nil
}

// computePeriodLabel formats a human-readable period label (e.g. "July 2026").
func computePeriodLabel(cadence string, from time.Time) string {
	switch cadence {
	case "monthly":
		return from.Format("January 2006")
	case "daily":
		return from.Format("Mon 2 Jan 2006")
	}
	// Weekly: "Mon 30 Jun – Sun 6 Jul 2026"
	to := from.AddDate(0, 0, 6)
	if from.Month() == to.Month() {
		return fmt.Sprintf("%s %d–%s %d %s %d",
			from.Weekday().String()[:3], from.Day(),
			to.Weekday().String()[:3], to.Day(),
			from.Format("Jan"), from.Year(),
		)
	}
	return fmt.Sprintf("%s %d %s – %s %d %s %d",
		from.Weekday().String()[:3], from.Day(), from.Format("Jan"),
		to.Weekday().String()[:3], to.Day(), to.Format("Jan"), to.Year(),
	)
}

// nextDigestAt computes the next scheduled digest time given the settings.
// Returns nil when the timezone cannot be loaded or digest is disabled.
func nextDigestAt(cadence string, day, hour int, tz string) *time.Time {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		loc = time.UTC
	}
	now := time.Now().In(loc)

	var next time.Time
	switch cadence {
	case "daily":
		// Next occurrence of the given hour, today or tomorrow. digest_day is
		// not meaningful for a daily cadence (the UI only collects a send time).
		candidate := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, loc)
		if !candidate.After(now) {
			candidate = candidate.AddDate(0, 0, 1)
		}
		next = candidate
	case "monthly":
		d := day
		if d < 1 {
			d = 1
		}
		if d > 28 {
			d = 28
		}
		// Next occurrence of day d at the given hour.
		candidate := time.Date(now.Year(), now.Month(), d, hour, 0, 0, 0, loc)
		if !candidate.After(now) {
			candidate = time.Date(now.Year(), now.Month()+1, d, hour, 0, 0, 0, loc)
		}
		next = candidate
	default: // weekly
		// day 0=Sunday … 6=Saturday
		targetWD := time.Weekday(day % 7)
		daysUntil := int(targetWD) - int(now.Weekday())
		if daysUntil < 0 {
			daysUntil += 7
		}
		candidate := time.Date(now.Year(), now.Month(), now.Day()+daysUntil, hour, 0, 0, 0, loc)
		if !candidate.After(now) {
			candidate = candidate.AddDate(0, 0, 7)
		}
		next = candidate
	}
	t := next.UTC()
	return &t
}

// mailerIsConfigured returns true if the instance mailer is ready to send.
// Uses the MailerStatus interface injected by main.go.
func (s *Service) mailerIsConfigured(ctx context.Context) bool {
	if s.mailerStatus == nil {
		return false
	}
	return s.mailerStatus.Enabled(ctx)
}

// defaultNotifySettings returns the read-only defaults used when no row exists.
func defaultNotifySettings(tenantID uuid.UUID) NotifySettings {
	return NotifySettings{
		TenantID:             tenantID,
		Enabled:              false,
		Recipients:           []string{},
		AlertOnFailure:       true,
		AlertThrottleMinutes: 60,
		DigestEnabled:        false,
		DigestCadence:        "weekly",
		DigestDay:            1,
		DigestHour:           8,
		Timezone:             "UTC",
	}
}

// validateNotifySettings validates a PUT body and returns a domain error if invalid.
func validateNotifySettings(in NotifySettingsUpsertInput) error {
	if len(in.Recipients) > 20 {
		return errCode("too_many_notify_recipients", "recipients may not exceed 20")
	}
	for _, r := range in.Recipients {
		if !isValidEmail(r) {
			return errCode("invalid_recipient", "recipient is not a valid email address: "+r)
		}
	}
	if in.AlertThrottleMinutes < 15 || in.AlertThrottleMinutes > 1440 {
		return errCode("invalid_throttle", "alert_throttle_minutes must be between 15 and 1440")
	}

	// Digest fields are only meaningful (and only validated/enforced) when the
	// digest is actually enabled. This keeps the per-failure-alerts section
	// (recipients/throttle) saveable even when the digest is off or its fields
	// haven't been filled in by the caller — a digest_cadence/digest_day error
	// must never block an unrelated alerts-only save.
	if !in.DigestEnabled {
		return nil
	}

	switch in.DigestCadence {
	case "daily", "weekly", "monthly":
	default:
		return errCode("invalid_digest_cadence", "digest_cadence must be 'daily', 'weekly' or 'monthly'")
	}
	switch in.DigestCadence {
	case "weekly":
		if in.DigestDay < 0 || in.DigestDay > 6 {
			return errCode("invalid_digest_day", "digest_day must be 0–6 for weekly cadence")
		}
	case "monthly":
		if in.DigestDay < 1 || in.DigestDay > 28 {
			return errCode("invalid_digest_day", "digest_day must be 1–28 for monthly cadence")
		}
	default: // daily — digest_day is not collected by the UI and is not required
	}
	if in.DigestHour < 0 || in.DigestHour > 23 {
		return errCode("invalid_digest_hour", "digest_hour must be 0–23")
	}
	if _, err := time.LoadLocation(in.Timezone); err != nil {
		return errCode("invalid_timezone", "timezone is not a valid IANA timezone: "+in.Timezone)
	}
	return nil
}

// isValidEmail is a minimal email format check (not RFC-compliant; sufficient
// for a UI-entered recipient list).
func isValidEmail(s string) bool {
	at := false
	dot := false
	for i, c := range s {
		if c == '@' {
			if i == 0 || at {
				return false
			}
			at = true
		} else if c == '.' && at {
			dot = true
		}
	}
	return at && dot && len(s) > 5
}

// errCode wraps a validation error with a machine-readable code. Uses
// domain.Validation to stay consistent with the rest of the email service.
func errCode(code, msg string) error {
	// We can't import domain here without creating an import, so we construct
	// a domain.Error via the standard helper imported in service.go.
	return errCodeValidation(code, msg)
}

// Mailer notification interface — wired from main.go after mailerSvc is ready.
// Separate from the backup Mailer pattern to keep the interfaces minimal.
type mailerNotifier struct {
	enq  MailerEnqueuer
	stat MailerStatus
}

// ensure the concrete *mailer.Service satisfies MailerStatus at compile-time.
var _ MailerStatus = (*mailer.Service)(nil)
var _ MailerEnqueuer = (*mailer.Enqueuer)(nil)
