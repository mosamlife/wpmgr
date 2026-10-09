package main

import (
	"context"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/aitrust"
	"github.com/mosamlife/wpmgr/apps/api/internal/mailer"
)

// aiLaunchNoticeKind is the name the email log shows for the AI launch
// notice.
const aiLaunchNoticeKind = "ai_launch_notice"

// aiLaunchNoticeMailer satisfies aitrust.LaunchNoticeMailer with the shared
// transactional mailer, which resolves the SMTP transport on every send. A
// notice counts as delivered only when the mailer reports it sent: a skipped
// send (no mail transport set up, no recipients) or a failed one is not, so
// the notifier releases its claim for a later run.
type aiLaunchNoticeMailer struct {
	svc *mailer.Service
}

var _ aitrust.LaunchNoticeMailer = aiLaunchNoticeMailer{}

func (m aiLaunchNoticeMailer) SendLaunchNotice(ctx context.Context, tenantID uuid.UUID, recipients []string, subject, text string) (bool, error) {
	if m.svc == nil {
		return false, nil
	}
	out, err := m.svc.SendTenantMessage(ctx, tenantID, aiLaunchNoticeKind, recipients, subject, text)
	return out.Status == mailer.SendSent, err
}
