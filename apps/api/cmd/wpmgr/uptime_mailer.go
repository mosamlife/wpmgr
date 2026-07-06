package main

import (
	"context"

	"github.com/mosamlife/wpmgr/apps/api/internal/mailer"
	"github.com/mosamlife/wpmgr/apps/api/internal/uptime"
)

// uptimeMailerAdapter satisfies uptime.Mailer by delegating to the shared
// ADR-045 transactional mailer (internal/mailer.Service), which resolves the
// SMTP transport from the smtp_settings DB row (age-decrypting the stored
// password, falling back to WPMGR_SMTP_* env) on EVERY send.
//
// GH #144: the previous wiring picked uptime.SMTPMailer or uptime.NoopMailer
// ONCE at boot from WPMGR_SMTP_* env. Since SMTP is configured in the
// dashboard (which writes the smtp_settings DB row, not env vars), that env
// check almost always failed and froze the uptime alert mailer to NoopMailer
// forever — every downtime/recovery/security alert email silently no-op'd,
// while the audit log recorded "emailed: true" because it only checked
// whether recipients were configured, not whether the send happened.
type uptimeMailerAdapter struct {
	svc *mailer.Service
}

// Send delegates to Service.SendMessage and maps its outcome onto
// uptime.SendResult, the outcome type the alert audit trail persists.
func (a uptimeMailerAdapter) Send(ctx context.Context, recipients []string, subject, body string) (uptime.SendResult, error) {
	outcome, err := a.svc.SendMessage(ctx, recipients, subject, body)
	return uptime.SendResult{Status: string(outcome.Status), Reason: outcome.Reason}, err
}
