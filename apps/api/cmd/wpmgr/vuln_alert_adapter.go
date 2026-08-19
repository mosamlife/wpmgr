package main

import (
	"context"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/email"
	"github.com/mosamlife/wpmgr/apps/api/internal/uptime"
	"github.com/mosamlife/wpmgr/apps/api/internal/vuln"
)

// vuln_alert_adapter.go — m103 (GH #247) wiring adapters. Each domain
// package (vuln, email) declares a NARROW local interface for what it needs
// from a sibling domain (internal/uptime's AlertConfig/Dispatcher) rather
// than importing that package directly (see internal/vuln/alertdispatch.go
// and internal/email/notify.go's doc comments) — these adapters are where
// the concrete cross-domain wiring actually happens, exactly like
// uptimeMailerAdapter in uptime_mailer.go.

// vulnAlertConfigAdapter satisfies vuln.AlertConfigReader by delegating to
// *uptime.Service.GetAlertConfig and mapping the fields the vuln dispatcher
// needs onto vuln.AlertChannelConfig.
type vulnAlertConfigAdapter struct {
	svc *uptime.Service
}

func (a vulnAlertConfigAdapter) GetVulnAlertChannel(ctx context.Context, tenantID uuid.UUID) (vuln.AlertChannelConfig, error) {
	cfg, err := a.svc.GetAlertConfig(ctx, tenantID)
	if err != nil {
		return vuln.AlertChannelConfig{}, err
	}
	return vuln.AlertChannelConfig{
		Enabled:             cfg.Enabled,
		EmailRecipients:     cfg.EmailRecipients,
		WebhookURL:          cfg.WebhookURL,
		WebhookSecret:       cfg.WebhookSecret,
		NotifyVulns:         cfg.NotifyVulns,
		VulnMinSeverity:     cfg.VulnMinSeverity,
		VulnIncludeInDigest: cfg.VulnIncludeInDigest,
	}, nil
}

// vulnWebhookAdapter satisfies vuln.WebhookPoster by delegating to
// *uptime.Dispatcher.PostSignedWebhook, reusing the SAME HMAC signing +
// SSRF-hardened delivery as the uptime/security alert channels instead of
// vuln standing up a parallel webhook client.
type vulnWebhookAdapter struct {
	d *uptime.Dispatcher
}

func (a vulnWebhookAdapter) PostSignedWebhook(ctx context.Context, url, secret string, payload any) error {
	return a.d.PostSignedWebhook(ctx, url, secret, payload)
}

// vulnDigestSourceAdapter satisfies email.VulnDigestSource by composing
// *uptime.Service (the vuln_include_in_digest gate, which lives on the
// alert_configs table, NOT the email domain's own notify settings) with
// *vuln.Service (the open-finding fleet summary) — a single call that gates
// AND fetches, so internal/email needs only one interface method.
type vulnDigestSourceAdapter struct {
	uptime *uptime.Service
	vuln   *vuln.Service
}

// maxDigestVulnFindings caps the "top findings" list in the email digest's
// vulnerability section.
const maxDigestVulnFindings = 5

func (a vulnDigestSourceAdapter) GetVulnDigestSummary(ctx context.Context, tenantID uuid.UUID) (email.VulnDigestSummary, bool, error) {
	cfg, err := a.uptime.GetAlertConfig(ctx, tenantID)
	if err != nil {
		return email.VulnDigestSummary{}, false, err
	}
	if !cfg.VulnIncludeInDigest {
		return email.VulnDigestSummary{}, false, nil
	}

	// GH #493 — the digest is a push to the customer, so it neither counts nor
	// names a paused site. GetFleetSummary (unfiltered) is the dashboard's;
	// calling it here is the defect this variant exists to prevent.
	fleet, _, err := a.vuln.GetFleetSummaryForDigest(ctx, tenantID, maxDigestVulnFindings)
	if err != nil {
		return email.VulnDigestSummary{}, false, err
	}

	summary := email.VulnDigestSummary{
		OpenCount:         fleet.TotalOpen,
		CriticalHighCount: fleet.Critical + fleet.High,
	}
	for _, f := range fleet.Findings {
		summary.Top = append(summary.Top, email.VulnDigestItem{
			SiteName:  f.SiteName,
			Component: vuln.ComponentLabel(f.Finding.Kind, f.Finding.Name),
			Severity:  vuln.SeverityLabel(f.Finding.Severity),
			CVE:       f.Finding.CVE,
		})
	}
	return summary, true, nil
}
