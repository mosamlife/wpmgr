package uptime

import (
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/api/gen"
)

// TestMergeAlertConfigUpdate_PreservesOmittedFields is the m103 (GH #247)
// regression test: before this fix, putAlertConfig unconditionally reset
// notify_security (and, less obviously, webhook_url/enabled) to a zero
// value/hardcoded default on every PUT because the handler never read them
// from the existing config at all. A PUT that only intends to change one
// field (e.g. rotating webhook_secret, or flipping notify_vulns) must leave
// every other field exactly as it was stored.
func TestMergeAlertConfigUpdate_PreservesOmittedFields(t *testing.T) {
	tenantID := uuid.New()
	existing := AlertConfig{
		TenantID:            tenantID,
		EmailRecipients:     []string{"ops@example.com"},
		WebhookURL:          "https://hooks.example.com/wpmgr",
		WebhookSecret:       "existing-secret",
		Enabled:             false, // deliberately non-default, to catch a hardcoded `Or(true)`
		NotifySecurity:      true,
		NotifyVulns:         true,
		VulnMinSeverity:     VulnSeverityCritical,
		VulnIncludeInDigest: false,
	}

	// An update body that only sets email_recipients — every other field is
	// omitted and must round-trip from `existing`.
	req := gen.AlertConfigUpdate{
		EmailRecipients: []string{"ops@example.com", "oncall@example.com"},
	}

	got := mergeAlertConfigUpdate(tenantID, existing, req)

	if len(got.EmailRecipients) != 2 {
		t.Fatalf("email_recipients should reflect the request body, got %v", got.EmailRecipients)
	}
	if got.WebhookURL != existing.WebhookURL {
		t.Errorf("webhook_url must be preserved when omitted: got %q, want %q", got.WebhookURL, existing.WebhookURL)
	}
	if got.WebhookSecret != existing.WebhookSecret {
		t.Errorf("webhook_secret must be preserved when omitted: got %q, want %q", got.WebhookSecret, existing.WebhookSecret)
	}
	if got.Enabled != existing.Enabled {
		t.Errorf("enabled must be preserved when omitted (not hardcoded true): got %v, want %v", got.Enabled, existing.Enabled)
	}
	if got.NotifySecurity != existing.NotifySecurity {
		t.Errorf("THE REGRESSION: notify_security must be preserved when omitted: got %v, want %v", got.NotifySecurity, existing.NotifySecurity)
	}
	if got.NotifyVulns != existing.NotifyVulns {
		t.Errorf("notify_vulns must be preserved when omitted: got %v, want %v", got.NotifyVulns, existing.NotifyVulns)
	}
	if got.VulnMinSeverity != existing.VulnMinSeverity {
		t.Errorf("vuln_min_severity must be preserved when omitted: got %q, want %q", got.VulnMinSeverity, existing.VulnMinSeverity)
	}
	if got.VulnIncludeInDigest != existing.VulnIncludeInDigest {
		t.Errorf("vuln_include_in_digest must be preserved when omitted: got %v, want %v", got.VulnIncludeInDigest, existing.VulnIncludeInDigest)
	}
}

// TestMergeAlertConfigUpdate_SetFieldsOverride proves the flip side: every
// field, when EXPLICITLY set in the request, overrides the stored value
// (mergeAlertConfigUpdate must not accidentally always-preserve).
func TestMergeAlertConfigUpdate_SetFieldsOverride(t *testing.T) {
	tenantID := uuid.New()
	existing := AlertConfig{
		TenantID:            tenantID,
		WebhookURL:          "https://old.example.com/hook",
		Enabled:             true,
		NotifySecurity:      false,
		NotifyVulns:         false,
		VulnMinSeverity:     VulnSeverityHigh,
		VulnIncludeInDigest: true,
	}
	req := gen.AlertConfigUpdate{
		WebhookURL:          gen.NewOptString("https://new.example.com/hook"),
		WebhookSecret:       gen.NewOptString("new-secret"),
		Enabled:             gen.NewOptBool(false),
		NotifySecurity:      gen.NewOptBool(true),
		NotifyVulns:         gen.NewOptBool(true),
		VulnMinSeverity:     gen.NewOptAlertConfigUpdateVulnMinSeverity(gen.AlertConfigUpdateVulnMinSeverityLow),
		VulnIncludeInDigest: gen.NewOptBool(false),
	}

	got := mergeAlertConfigUpdate(tenantID, existing, req)

	if got.WebhookURL != "https://new.example.com/hook" {
		t.Errorf("webhook_url should be overridden, got %q", got.WebhookURL)
	}
	if got.WebhookSecret != "new-secret" {
		t.Errorf("webhook_secret should be overridden, got %q", got.WebhookSecret)
	}
	if got.Enabled != false {
		t.Errorf("enabled should be overridden to false, got %v", got.Enabled)
	}
	if !got.NotifySecurity {
		t.Errorf("notify_security should be overridden to true")
	}
	if !got.NotifyVulns {
		t.Errorf("notify_vulns should be overridden to true")
	}
	if got.VulnMinSeverity != VulnSeverityLow {
		t.Errorf("vuln_min_severity should be overridden to low, got %q", got.VulnMinSeverity)
	}
	if got.VulnIncludeInDigest {
		t.Errorf("vuln_include_in_digest should be overridden to false")
	}
}

// TestSaveAlertConfig_InvalidVulnMinSeverity proves the severity enum is
// validated server-side (defense-in-depth beyond the DB CHECK constraint and
// the OpenAPI enum), and that a valid value is accepted.
func TestSaveAlertConfig_InvalidVulnMinSeverity(t *testing.T) {
	svc := &Service{repo: nil}
	_, err := svc.SaveAlertConfig(nil, AlertConfig{ //nolint:staticcheck // nil ctx: validation short-circuits before any repo/ctx use
		TenantID:        uuid.New(),
		VulnMinSeverity: "unknown", // deliberately invalid: not a selectable threshold
	})
	if err == nil {
		t.Fatal("expected a validation error for vuln_min_severity=unknown")
	}
}
