package mailer

import (
	"path/filepath"
	"strings"
	"testing"
)

// sampleData returns representative variables for each template so the render
// test exercises every {{.Field}} the templates reference.
func sampleData(name string) map[string]any {
	common := map[string]any{
		"ProductName":  "WPMgr",
		"BaseURL":      "https://manage.wpmgr.app",
		"SupportEmail": "support@wpmgr.app",
		"Year":         2026,
		"PreviewText":  "preview",
	}
	switch name {
	case "test":
		common["RecipientEmail"] = "ops@example.com"
	case "password_reset":
		common["Name"] = "Sam"
		common["ResetURL"] = "https://manage.wpmgr.app/reset-password?token=abc123"
		common["ExpiresMinutes"] = "30"
	case "password_changed":
		common["Name"] = "Sam"
		common["When"] = "2026-06-02 14:00 UTC"
		common["IP"] = "203.0.113.7"
	case "sign_in_method_added":
		// Mirrors the shape built in auth/social_notify.go
		// sendSignInMethodAdded.
		common["Name"] = "Sam"
		common["Provider"] = "Google"
		common["When"] = "2026-08-07 14:00 UTC"
		common["SecurityURL"] = "https://manage.wpmgr.app/settings/security"
	case "verify_email":
		common["Name"] = "Sam"
		common["VerifyURL"] = "https://manage.wpmgr.app/verify-email?token=def456"
		common["ExpiresHours"] = "24"
	case "invite":
		common["Name"] = "Sam"
		common["InviterName"] = "Alex"
		common["OrgName"] = "Acme"
		common["Role"] = "admin"
		common["AcceptURL"] = "https://manage.wpmgr.app/accept?token=ghi789"
		common["ExpiresHours"] = "168"
	case "account_exists":
		common["Name"] = "Sam"
		common["LoginURL"] = "https://manage.wpmgr.app/login"
		common["ResetURL"] = "https://manage.wpmgr.app/forgot-password"
	case "site_invite":
		common["Name"] = "there"
		common["InviterName"] = "Alex"
		common["SiteName"] = "example.com"
		common["Role"] = "viewer"
		common["AcceptURL"] = "https://manage.wpmgr.app/accept?token=site123"
		common["ExpiresHours"] = "168"
	case "site_shared":
		common["Name"] = "Sam"
		common["InviterName"] = "Alex"
		common["SiteName"] = "example.com"
		common["Role"] = "viewer"
		common["DashboardURL"] = "https://manage.wpmgr.app/shared-with-me"
	case "email_failure_alert":
		// Mirrors the production shape built in email/notify.go sendAlert:
		// counts are ints, samples carry Subject/To/Error.
		common["SiteName"] = "example.com"
		common["SiteURL"] = "https://example.com"
		common["SiteEmailURL"] = "https://manage.wpmgr.app/sites/abc/email/log"
		common["FailureCount"] = 3
		common["WindowMinutes"] = 60
		common["Samples"] = []map[string]any{
			{"Subject": "Order receipt", "To": "buyer@example.com", "Error": "550 mailbox unavailable"},
			{"Subject": "Password reset", "To": "user@example.com", "Error": "timeout connecting to provider"},
		}
	case "client_portal_invite":
		common["Name"] = "there"
		common["InviterName"] = "Alex Agency"
		common["ClientName"] = "Acme Corp"
		common["AgencyName"] = "Alex Agency"
		common["AcceptURL"] = "https://manage.wpmgr.app/accept?token=portal123"
		common["ExpiresHours"] = "168"
	case "email_digest":
		// Mirrors the production shape built in email/notify.go buildDigestData:
		// aggregate counts are int64, per-site rows carry int64 counts.
		common["PeriodLabel"] = "June 2026"
		common["From"] = "2026-06-01"
		common["To"] = "2026-06-30"
		common["Total"] = int64(120)
		common["SentCount"] = int64(110)
		common["FailedCount"] = int64(7)
		common["BouncedCount"] = int64(3)
		common["SiteCount"] = int64(2)
		common["Sites"] = []map[string]any{
			{"SiteName": "example.com", "SiteURL": "https://example.com", "Sent": int64(80), "Failed": int64(5), "Bounced": int64(2)},
			{"SiteName": "shop.example.com", "SiteURL": "https://shop.example.com", "Sent": int64(30), "Failed": int64(0), "Bounced": int64(1)},
		}
		common["TopFailures"] = []map[string]any{
			{"SiteName": "example.com", "Subject": "Order receipt", "Error": "550 mailbox unavailable"},
		}
		common["DashboardURL"] = "https://manage.wpmgr.app/email"
		// m103 (GH #247): mirrors the shape buildDigestData adds when
		// alert_configs.vuln_include_in_digest is on.
		common["OpenVulnCount"] = 4
		common["CriticalHighCount"] = 2
		common["TopVulns"] = []map[string]any{
			{"SiteName": "example.com", "Component": "Plugin: Rank Math SEO", "Severity": "Critical", "CVE": "CVE-2024-12345"},
			{"SiteName": "shop.example.com", "Component": "Plugin: WooCommerce", "Severity": "High", "CVE": ""},
		}
		common["VulnDashboardURL"] = "https://manage.wpmgr.app/vulnerabilities"
	case "vuln_alert":
		// Mirrors the production shape built in vuln/alertdispatch.go
		// buildVulnAlertEmailData: severity-rank-desc grouped per site, an
		// empty FixedVersion is pre-formatted to "no fixed version yet", and
		// OverflowCount is set to exercise the "+N more" summary line.
		common["NewCount"] = 3
		common["SiteCount"] = 2
		common["Sites"] = []map[string]any{
			{
				"SiteName": "example.com",
				"SiteURL":  "https://example.com",
				"Findings": []map[string]any{
					{"Component": "Plugin: Rank Math SEO", "InstalledVersion": "1.0.98", "FixedVersion": "1.0.114", "Severity": "Critical", "CVE": "CVE-2024-12345"},
					{"Component": "Theme: Astra", "InstalledVersion": "4.6.0", "FixedVersion": "no fixed version yet", "Severity": "Unknown (no CVSS yet)", "CVE": ""},
				},
			},
			{
				"SiteName": "shop.example.com",
				"SiteURL":  "https://shop.example.com",
				"Findings": []map[string]any{
					{"Component": "Plugin: WooCommerce", "InstalledVersion": "8.0.0", "FixedVersion": "8.0.3", "Severity": "High", "CVE": ""},
				},
			},
		}
		common["OverflowCount"] = 5
		common["DashboardURL"] = "https://manage.wpmgr.app/vulnerabilities"
		common["FooterNote"] = "You are receiving this because vulnerability alerts are enabled for this account. Manage this in Alerts settings."
	}
	return common
}

// TestRenderAllTemplates renders every template and asserts each produces a
// non-empty subject + HTML + plaintext.
func TestRenderAllTemplates(t *testing.T) {
	r, err := NewTemplateRenderer()
	if err != nil {
		t.Fatalf("NewTemplateRenderer: %v", err)
	}
	for name := range subjects {
		em, err := r.Render(name, sampleData(name))
		if err != nil {
			t.Fatalf("render %s: %v", name, err)
		}
		if em.Subject == "" || strings.TrimSpace(em.HTML) == "" || strings.TrimSpace(em.Text) == "" {
			t.Fatalf("render %s: empty subject/html/text", name)
		}
		// HTML must carry the bulletproof <html> boilerplate.
		if !strings.Contains(strings.ToLower(em.HTML), "<html") {
			t.Errorf("render %s: html missing <html> root", name)
		}
	}
}

// TestPlaintextContainsActionURL enforces the security/UX requirement that every
// actionable email's PLAINTEXT alternative embeds the literal link (text-only
// clients must still be able to complete the action).
func TestPlaintextContainsActionURL(t *testing.T) {
	r, err := NewTemplateRenderer()
	if err != nil {
		t.Fatalf("NewTemplateRenderer: %v", err)
	}
	cases := map[string]string{
		"password_reset":       "https://manage.wpmgr.app/reset-password?token=abc123",
		"verify_email":         "https://manage.wpmgr.app/verify-email?token=def456",
		"invite":               "https://manage.wpmgr.app/accept?token=ghi789",
		"client_portal_invite": "https://manage.wpmgr.app/accept?token=portal123",
		// The security notice is only useful if the reader can reach the page
		// that lists (and removes) sign-in methods, including from a text-only
		// client.
		"sign_in_method_added": "https://manage.wpmgr.app/settings/security",
	}
	for name, url := range cases {
		em, err := r.Render(name, sampleData(name))
		if err != nil {
			t.Fatalf("render %s: %v", name, err)
		}
		if !strings.Contains(em.Text, url) {
			t.Errorf("plaintext for %s must contain the action URL %q", name, url)
		}
	}
}

// TestSubjectsTemplatesCompleteness is the regression lock: every embedded
// template file must have a subjects entry AND a matching .txt.tmpl file, and
// every subjects key must have both .html.tmpl and .txt.tmpl files. A mismatch
// between the subjects map and the templates directory is what caused the
// client_portal_invite bug (template existed, subjects entry was absent) to
// ship undetected — TestRenderAllTemplates only iterates the subjects map keys,
// so a missing entry silently skips the template.
func TestSubjectsTemplatesCompleteness(t *testing.T) {
	// Gather all HTML template base names from the embedded FS (skip _partials).
	htmlFiles, err := templateFS.ReadDir("templates")
	if err != nil {
		t.Fatalf("read templates dir: %v", err)
	}

	fromFiles := make(map[string]bool) // base names present as HTML files
	for _, f := range htmlFiles {
		name := f.Name()
		if strings.HasPrefix(name, "_") {
			continue // skip _partials
		}
		if !strings.HasSuffix(name, ".html.tmpl") {
			continue
		}
		base := strings.TrimSuffix(name, ".html.tmpl")
		fromFiles[base] = true
	}

	// Every HTML template file must have a subjects entry.
	for base := range fromFiles {
		if _, ok := subjects[base]; !ok {
			t.Errorf("template file %q exists but subjects map has no entry for it", base)
		}
		// Also verify the matching .txt.tmpl file exists.
		txtName := "templates/" + base + ".txt.tmpl"
		if _, ferr := templateFS.Open(txtName); ferr != nil {
			t.Errorf("template %q has no matching %q", base+".html.tmpl", txtName)
		}
	}

	// Every subjects entry must have both template files.
	for key := range subjects {
		htmlName := filepath.Join("templates", key+".html.tmpl")
		if _, ferr := templateFS.Open(htmlName); ferr != nil {
			t.Errorf("subjects entry %q has no %q file", key, htmlName)
		}
		txtName := filepath.Join("templates", key+".txt.tmpl")
		if _, ferr := templateFS.Open(txtName); ferr != nil {
			t.Errorf("subjects entry %q has no %q file", key, txtName)
		}
	}
}
