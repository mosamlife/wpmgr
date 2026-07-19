// alertdispatch.go — m103 (GH #247) vulnerability alerting.
//
// Vulnerability alerts are the THIRD signal on the existing per-tenant alert
// channel (alert_configs), alongside uptime downtime/recovery and
// notify_security. A NEW finding is status='open' AND notified_at IS NULL
// (see Repo.ClaimUnnotifiedFindings). RescanSiteWorker.Work enqueues a
// debounced, batched AlertDispatchArgs job after every successful rescan (see
// worker.go); DispatchVulnAlerts below does the actual claim + gate + send
// fan-out, once per tenant that currently has unnotified findings.
package vuln

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ---------------------------------------------------------------------------
// Local interfaces — declared here (not imported from internal/uptime or
// internal/mailer) so this package does not cross-import sibling domains.
// The concrete adapters satisfying these are wired in cmd/wpmgr/main.go.
// ---------------------------------------------------------------------------

// AlertChannelConfig is the subset of a tenant's alert_configs row the vuln
// dispatcher needs. Declared locally (mirrors the MailerEnqueuer pattern in
// internal/email/notify.go) instead of importing internal/uptime.AlertConfig,
// so this package has no hard dependency on the uptime domain.
type AlertChannelConfig struct {
	Enabled             bool
	EmailRecipients     []string
	WebhookURL          string
	WebhookSecret       string
	NotifyVulns         bool
	VulnMinSeverity     string
	VulnIncludeInDigest bool
}

// AlertConfigReader resolves a tenant's alert channel + vulnerability
// alerting gate/threshold. The adapter wired in cmd/wpmgr/main.go delegates
// to *uptime.Service.GetAlertConfig.
type AlertConfigReader interface {
	GetVulnAlertChannel(ctx context.Context, tenantID uuid.UUID) (AlertChannelConfig, error)
}

// AlertMailer enqueues a durable templated email ("vuln_alert") within the
// SAME pgx transaction as the caller's finding-claim tx, so the email is
// enqueued only if that transaction actually commits (transactional outbox —
// see dispatchTenant). *mailer.Enqueuer satisfies this via its EnqueueTx
// method (wired in cmd/wpmgr/main.go); declared locally so this package does
// not import internal/mailer's whole surface.
type AlertMailer interface {
	EnqueueTx(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, recipients []string, template string, data map[string]any) error
}

// WebhookPoster posts a signed vulnerability-alert webhook payload to a
// tenant's configured endpoint. The adapter wired in cmd/wpmgr/main.go
// delegates to *uptime.Dispatcher.PostSignedWebhook, reusing its HMAC
// signing + SSRF-hardened delivery exactly rather than duplicating it here.
type WebhookPoster interface {
	PostSignedWebhook(ctx context.Context, url, secret string, payload any) error
}

// ---------------------------------------------------------------------------
// Webhook payload shapes (security.vuln_new_findings)
// ---------------------------------------------------------------------------

// VulnAlertWebhookPayload is the JSON body POSTed for the batched
// vulnerability-findings webhook event. Unlike the uptime Dispatcher's
// per-site WebhookPayload, this can span multiple sites in one delivery — a
// whole rescan wave collapses to ONE dispatch (see the debounce enqueue in
// worker.go). Includes the FULL filtered finding set (no display cap — that
// is an email-rendering constraint only).
type VulnAlertWebhookPayload struct {
	Event     string                 `json:"event"`
	TenantID  string                 `json:"tenant_id"`
	NewCount  int                    `json:"new_count"`
	SiteCount int                    `json:"site_count"`
	Sites     []VulnAlertWebhookSite `json:"sites"`
	FiredAt   time.Time              `json:"fired_at"`
}

// VulnAlertWebhookSite is one site's group of findings in the payload.
type VulnAlertWebhookSite struct {
	SiteID   string                    `json:"site_id"`
	SiteName string                    `json:"site_name"`
	SiteURL  string                    `json:"site_url"`
	Findings []VulnAlertWebhookFinding `json:"findings"`
}

// VulnAlertWebhookFinding is one finding entry in the payload.
type VulnAlertWebhookFinding struct {
	Component        string `json:"component"`
	InstalledVersion string `json:"installed_version"`
	FixedVersion     string `json:"fixed_version,omitempty"`
	Severity         string `json:"severity"`
	CVE              string `json:"cve,omitempty"`
}

// VulnAlertEvent is the webhook event-type constant for a batched
// vulnerability-alert dispatch.
const VulnAlertEvent = "security.vuln_new_findings"

// ---------------------------------------------------------------------------
// Severity threshold + display helpers
// ---------------------------------------------------------------------------

// severityThresholdRank orders the four SELECTABLE alert-threshold severities
// low(1) < medium(2) < high(3) < critical(4). SeverityUnknown deliberately has
// no entry — it is never a threshold value and always passes the filter (see
// passesSeverityThreshold).
var severityThresholdRank = map[string]int{
	SeverityLow:      1,
	SeverityMedium:   2,
	SeverityHigh:     3,
	SeverityCritical: 4,
}

// passesSeverityThreshold reports whether severity clears minSeverity.
// SeverityUnknown ALWAYS passes regardless of minSeverity: it represents the
// newest, un-enriched Scanner-only findings (no CVSS/rating yet) — GH #247's
// motivating CVE sat as 'unknown' pre-#245, and an enrichment upgrade never
// resets notified_at (see Repo.UpsertFinding), so a finding excluded from its
// FIRST alert at 'unknown' would never get a second chance to alert once
// enriched. An unrecognised severity value (defensive; should not happen
// given the DB CHECK) fails closed (does not alert).
func passesSeverityThreshold(severity, minSeverity string) bool {
	if severity == SeverityUnknown {
		return true
	}
	rank, ok := severityThresholdRank[severity]
	if !ok {
		return false
	}
	minRank, ok := severityThresholdRank[minSeverity]
	if !ok {
		minRank = severityThresholdRank[SeverityHigh] // safety net: default to the documented default threshold
	}
	return rank >= minRank
}

// severityDisplayRank orders severities for GROUPED DISPLAY (email/webhook
// ordering), NOT for the threshold filter: critical, high, unknown, medium,
// low — mirrors the ORDER BY CASE used by ListOpenFindings/FleetOpenFindings.
var severityDisplayRank = map[string]int{
	SeverityCritical: 1,
	SeverityHigh:     2,
	SeverityUnknown:  3,
	SeverityMedium:   4,
	SeverityLow:      5,
}

// SeverityLabel formats a severity value for human display in emails.
func SeverityLabel(severity string) string {
	switch severity {
	case SeverityCritical:
		return "Critical"
	case SeverityHigh:
		return "High"
	case SeverityMedium:
		return "Medium"
	case SeverityLow:
		return "Low"
	case SeverityUnknown:
		return "Unknown (no CVSS yet)"
	default:
		return severity
	}
}

// ComponentLabel formats a finding's kind+name for human display, e.g.
// "Plugin: Rank Math SEO".
func ComponentLabel(kind, name string) string {
	switch kind {
	case KindPlugin:
		return "Plugin: " + name
	case KindTheme:
		return "Theme: " + name
	case KindCore:
		return "WordPress Core"
	default:
		return name
	}
}

// fixedVersionDisplay returns the fixed version, or a human note when none is
// published yet.
func fixedVersionDisplay(v string) string {
	if v == "" {
		return "no fixed version yet"
	}
	return v
}

// ---------------------------------------------------------------------------
// Service wiring
// ---------------------------------------------------------------------------

// SetMailer wires the templated-email enqueuer for vuln alert emails.
func (s *Service) SetMailer(m AlertMailer) { s.mailer = m }

// SetWebhookPoster wires the signed-webhook poster for vuln alert webhooks.
func (s *Service) SetWebhookPoster(w WebhookPoster) { s.webhook = w }

// SetAlertConfigReader wires the tenant alert-channel reader.
func (s *Service) SetAlertConfigReader(r AlertConfigReader) { s.alertCfg = r }

// SetPublicBase sets the public base URL used to build dashboard links in
// vulnerability alert emails/webhooks (e.g. "https://manage.wpmgr.app").
func (s *Service) SetPublicBase(base string) { s.publicBase = base }

// ---------------------------------------------------------------------------
// Dispatch
// ---------------------------------------------------------------------------

// maxVulnAlertEmailFindings caps the number of findings rendered in a single
// alert email; the rest are summarised as "+N more". The webhook payload is
// never capped.
const maxVulnAlertEmailFindings = 20

// DispatchVulnAlerts is the vuln_alert_dispatch River job's entry point. It
// enumerates every tenant with at least one open, not-yet-notified finding
// and dispatches each independently — one tenant's failure is logged and
// never blocks the others. Safe to run concurrently (per-tenant claims are
// row-locked; see Repo.ClaimUnnotifiedFindings).
func (s *Service) DispatchVulnAlerts(ctx context.Context) error {
	if s.alertCfg == nil {
		// Not wired (e.g. a self-host boot path that never called
		// SetAlertConfigReader) — nothing to gate on, so skip cleanly rather
		// than claim findings nobody can ever be notified about.
		return nil
	}
	tenantIDs, err := s.repo.ListTenantsWithUnnotifiedFindings(ctx)
	if err != nil {
		return fmt.Errorf("vuln: list tenants with unnotified findings: %w", err)
	}
	for _, tenantID := range tenantIDs {
		if dErr := s.dispatchTenant(ctx, tenantID); dErr != nil {
			s.logger.Warn("vuln: alert dispatch failed for tenant",
				slog.String("tenant_id", tenantID.String()), slog.Any("error", dErr))
		}
	}
	return nil
}

// dispatchTenant claims one tenant's unnotified findings and, if any pass the
// tenant's notify_vulns + severity gate, sends a batched email (enqueued in
// the SAME tx as the claim — transactional outbox) and fires a signed webhook
// (best-effort, after the tx commits — never inside a DB transaction).
func (s *Service) dispatchTenant(ctx context.Context, tenantID uuid.UUID) error {
	cfg, err := s.alertCfg.GetVulnAlertChannel(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("get alert channel: %w", err)
	}

	var toSend []ClaimedFinding
	txErr := s.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		claimed, cErr := s.repo.ClaimUnnotifiedFindings(ctx, tx, tenantID)
		if cErr != nil {
			return cErr
		}
		if len(claimed) == 0 {
			return nil
		}
		if !cfg.Enabled || !cfg.NotifyVulns {
			// Rows stay stamped (claimed above); nothing to send. Enabling
			// notify_vulns later never floods the tenant with this backlog.
			return nil
		}
		filtered := filterBySeverity(claimed, cfg.VulnMinSeverity)
		if len(filtered) == 0 {
			return nil
		}
		toSend = filtered

		if s.mailer != nil && len(cfg.EmailRecipients) > 0 {
			data := buildVulnAlertEmailData(filtered, s.publicBase)
			if mErr := s.mailer.EnqueueTx(ctx, tx, tenantID, cfg.EmailRecipients, "vuln_alert", data); mErr != nil {
				// Roll back: the claim is undone, so these findings remain
				// unnotified and are retried on the next dispatch cycle.
				return fmt.Errorf("enqueue vuln alert email: %w", mErr)
			}
		}
		return nil
	})
	if txErr != nil {
		return txErr
	}

	if len(toSend) > 0 && cfg.WebhookURL != "" && s.webhook != nil {
		payload := buildVulnAlertWebhookPayload(tenantID, toSend)
		if wErr := s.webhook.PostSignedWebhook(ctx, cfg.WebhookURL, cfg.WebhookSecret, payload); wErr != nil {
			s.logger.Warn("vuln: alert webhook failed",
				slog.String("tenant_id", tenantID.String()), slog.Any("error", wErr))
		}
	}
	return nil
}

// filterBySeverity returns the subset of claimed findings that clear the
// tenant's alert threshold (see passesSeverityThreshold).
func filterBySeverity(claimed []ClaimedFinding, minSeverity string) []ClaimedFinding {
	out := make([]ClaimedFinding, 0, len(claimed))
	for _, c := range claimed {
		if passesSeverityThreshold(c.Finding.Severity, minSeverity) {
			out = append(out, c)
		}
	}
	return out
}

// sortedForDisplay returns a copy of claimed sorted for grouped display: by
// each SITE's most-severe finding first, then within a site by severity rank,
// CVSS score (desc), and first-seen (desc) — mirroring the
// ListOpenFindings/FleetOpenFindings ORDER BY convention.
func sortedForDisplay(claimed []ClaimedFinding) []ClaimedFinding {
	siteMinRank := make(map[uuid.UUID]int, len(claimed))
	for _, c := range claimed {
		r := severityDisplayRank[c.Finding.Severity]
		if cur, ok := siteMinRank[c.Finding.SiteID]; !ok || r < cur {
			siteMinRank[c.Finding.SiteID] = r
		}
	}
	out := make([]ClaimedFinding, len(claimed))
	copy(out, claimed)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if siteMinRank[a.Finding.SiteID] != siteMinRank[b.Finding.SiteID] {
			return siteMinRank[a.Finding.SiteID] < siteMinRank[b.Finding.SiteID]
		}
		if a.Finding.SiteID != b.Finding.SiteID {
			return a.SiteName < b.SiteName
		}
		ra, rb := severityDisplayRank[a.Finding.Severity], severityDisplayRank[b.Finding.Severity]
		if ra != rb {
			return ra < rb
		}
		as, bs := a.Finding.CVSSScore, b.Finding.CVSSScore
		if as != nil && bs != nil && *as != *bs {
			return *as > *bs
		}
		if (as == nil) != (bs == nil) {
			return bs == nil // non-nil score sorts before nil
		}
		return a.Finding.FirstSeen.After(b.Finding.FirstSeen)
	})
	return out
}

// buildVulnAlertEmailData builds the data map handed to the mailer's
// "vuln_alert" template. Shape:
//
//	{NewCount, SiteCount, Sites:[{SiteName, SiteURL, Findings:[{Component,
//	 InstalledVersion, FixedVersion, Severity, CVE}]}], OverflowCount,
//	 DashboardURL, FooterNote}
//
// Findings are capped at maxVulnAlertEmailFindings across ALL sites combined
// (OverflowCount = however many were cut); the webhook payload is never
// capped (see buildVulnAlertWebhookPayload).
func buildVulnAlertEmailData(claimed []ClaimedFinding, publicBase string) map[string]any {
	sorted := sortedForDisplay(claimed)
	total := len(sorted)

	siteCount := map[uuid.UUID]bool{}
	for _, c := range sorted {
		siteCount[c.Finding.SiteID] = true
	}

	capped := sorted
	overflow := 0
	if total > maxVulnAlertEmailFindings {
		capped = sorted[:maxVulnAlertEmailFindings]
		overflow = total - maxVulnAlertEmailFindings
	}

	type siteOrderEntry struct {
		siteID   uuid.UUID
		siteName string
		siteURL  string
	}
	var order []siteOrderEntry
	bySite := map[uuid.UUID][]map[string]any{}
	for _, c := range capped {
		if _, ok := bySite[c.Finding.SiteID]; !ok {
			order = append(order, siteOrderEntry{c.Finding.SiteID, c.SiteName, c.SiteURL})
		}
		bySite[c.Finding.SiteID] = append(bySite[c.Finding.SiteID], map[string]any{
			"Component":        ComponentLabel(c.Finding.Kind, c.Finding.Name),
			"InstalledVersion": c.Finding.InstalledVersion,
			"FixedVersion":     fixedVersionDisplay(c.Finding.FixedVersion),
			"Severity":         SeverityLabel(c.Finding.Severity),
			"CVE":              c.Finding.CVE,
		})
	}

	sites := make([]map[string]any, 0, len(order))
	for _, o := range order {
		sites = append(sites, map[string]any{
			"SiteName": o.siteName,
			"SiteURL":  o.siteURL,
			"Findings": bySite[o.siteID],
		})
	}

	return map[string]any{
		"NewCount":      total,
		"SiteCount":     len(siteCount),
		"Sites":         sites,
		"OverflowCount": overflow,
		"DashboardURL":  publicBase + "/vulnerabilities",
		"FooterNote":    "You are receiving this because vulnerability alerts are enabled for this account. Manage this in Alerts settings.",
	}
}

// buildVulnAlertWebhookPayload builds the signed webhook body. Includes the
// FULL filtered finding set — the 20-item cap in buildVulnAlertEmailData is
// an email-rendering constraint only; webhook consumers are machines.
func buildVulnAlertWebhookPayload(tenantID uuid.UUID, claimed []ClaimedFinding) VulnAlertWebhookPayload {
	sorted := sortedForDisplay(claimed)

	type siteOrderEntry struct {
		siteID   uuid.UUID
		siteName string
		siteURL  string
	}
	var order []siteOrderEntry
	bySite := map[uuid.UUID][]VulnAlertWebhookFinding{}
	siteSet := map[uuid.UUID]bool{}
	for _, c := range sorted {
		siteSet[c.Finding.SiteID] = true
		if _, ok := bySite[c.Finding.SiteID]; !ok {
			order = append(order, siteOrderEntry{c.Finding.SiteID, c.SiteName, c.SiteURL})
		}
		bySite[c.Finding.SiteID] = append(bySite[c.Finding.SiteID], VulnAlertWebhookFinding{
			Component:        ComponentLabel(c.Finding.Kind, c.Finding.Name),
			InstalledVersion: c.Finding.InstalledVersion,
			FixedVersion:     c.Finding.FixedVersion,
			Severity:         c.Finding.Severity,
			CVE:              c.Finding.CVE,
		})
	}

	sites := make([]VulnAlertWebhookSite, 0, len(order))
	for _, o := range order {
		sites = append(sites, VulnAlertWebhookSite{
			SiteID:   o.siteID.String(),
			SiteName: o.siteName,
			SiteURL:  o.siteURL,
			Findings: bySite[o.siteID],
		})
	}

	return VulnAlertWebhookPayload{
		Event:     VulnAlertEvent,
		TenantID:  tenantID.String(),
		NewCount:  len(sorted),
		SiteCount: len(siteSet),
		Sites:     sites,
		FiredAt:   time.Now().UTC(),
	}
}
