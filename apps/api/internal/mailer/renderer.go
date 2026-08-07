package mailer

import (
	"bytes"
	"embed"
	"fmt"
	htmltmpl "html/template"
	texttmpl "text/template"
)

// templateFS holds the checked-in, brand-matched email templates. HTML files use
// html/template (context-aware auto-escaping of names/URLs); the matching .txt
// files use text/template and carry the hand-authored plaintext alternative
// (each actionable email's .txt embeds the raw link). ADR-045 Appendix A.
//
//go:embed templates/*.tmpl
var templateFS embed.FS

// subjects maps a template name to its (static) subject line.
var subjects = map[string]string{
	"test":             "Your WPMgr SMTP test email",
	"password_reset":   "Reset your WPMgr password",
	"password_changed": "Your WPMgr password was changed",
	"verify_email":     "Verify your WPMgr email address",
	// Security notice: an account gained a way to sign in (a provider linked
	// onto an existing account). The holder is told even when they did it.
	"sign_in_method_added": "A new sign-in method was added to your WPMgr account",
	"invite":               "You have been invited to WPMgr",
	"account_exists":       "You already have a WPMgr account",
	"site_invite":          "You have been invited to a site on WPMgr",
	"site_shared":          "A site was shared with you on WPMgr",
	// Track B (m49): backup-event notification emails.
	"backup_completed": "Backup completed",
	"backup_failed":    "Backup failed — action required",
	// m62: per-site email alerts + digest.
	"email_failure_alert": "Email delivery failures detected",
	"email_digest":        "Your WPMgr email delivery digest",
	// m103 (GH #247): vulnerability alerting.
	"vuln_alert": "New vulnerabilities detected",
	// m64: white-label client reports.
	"report_ready": "Your website report is ready",
	// m66: client portal.
	"client_portal_invite": "You have been invited to a client portal",
}

// TemplateRenderer renders the embedded HTML + plaintext templates. It parses
// once at construction so Render is allocation-light and concurrency-safe.
type TemplateRenderer struct {
	html *htmltmpl.Template
	text *texttmpl.Template
}

// NewTemplateRenderer parses the embedded templates. It fails fast at startup if
// a template is malformed so a broken template never ships silently.
func NewTemplateRenderer() (*TemplateRenderer, error) {
	htmlT, err := htmltmpl.New("").ParseFS(templateFS, "templates/*.html.tmpl")
	if err != nil {
		return nil, fmt.Errorf("parse html email templates: %w", err)
	}
	textT, err := texttmpl.New("").ParseFS(templateFS, "templates/*.txt.tmpl")
	if err != nil {
		return nil, fmt.Errorf("parse text email templates: %w", err)
	}
	return &TemplateRenderer{html: htmlT, text: textT}, nil
}

// Render executes the named HTML + plaintext templates against data and returns
// the subject + both bodies. name is the logical template (e.g. "password_reset");
// the files are "<name>.html.tmpl" / "<name>.txt.tmpl".
func (r *TemplateRenderer) Render(name string, data map[string]any) (Email, error) {
	subject, ok := subjects[name]
	if !ok {
		return Email{}, fmt.Errorf("unknown email template %q", name)
	}

	var htmlBuf bytes.Buffer
	if err := r.html.ExecuteTemplate(&htmlBuf, name+".html.tmpl", data); err != nil {
		return Email{}, fmt.Errorf("render html %s: %w", name, err)
	}

	var textBuf bytes.Buffer
	if err := r.text.ExecuteTemplate(&textBuf, name+".txt.tmpl", data); err != nil {
		return Email{}, fmt.Errorf("render text %s: %w", name, err)
	}

	return Email{Subject: subject, HTML: htmlBuf.String(), Text: textBuf.String()}, nil
}
