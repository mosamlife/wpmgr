package mailer

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
)

// EmailQueue is the dedicated River queue for transactional email so a slow SMTP
// relay can't starve other work. Registered in startRiver (cmd/wpmgr).
const EmailQueue = "email"

// SendEmailArgs is the River job payload for a durable, retried email send. Data
// carries the per-template variables (JSON-serialized through River). It NEVER
// contains the raw token in a loggable field beyond the link already inside the
// template data.
type SendEmailArgs struct {
	EmailLogID uuid.UUID      `json:"email_log_id"`
	TenantID   uuid.UUID      `json:"tenant_id,omitempty"`
	Recipients []string       `json:"recipients"`
	Template   string         `json:"template"`
	Data       map[string]any `json:"data,omitempty"`
}

// Kind implements river.JobArgs.
func (SendEmailArgs) Kind() string { return "send_email" }

// InsertOpts pins the queue + a bounded retry budget for every send_email job.
func (SendEmailArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: EmailQueue, MaxAttempts: 5}
}

// SendEmailWorker delivers a queued email via the mailer Service, retrying
// transient SMTP failures (Service.Deliver returns an error only for those).
type SendEmailWorker struct {
	river.WorkerDefaults[SendEmailArgs]
	svc *Service
}

// NewSendEmailWorker builds the worker.
func NewSendEmailWorker(svc *Service) *SendEmailWorker {
	return &SendEmailWorker{svc: svc}
}

// Work delivers the email. A returned error triggers River's retry/backoff.
func (w *SendEmailWorker) Work(ctx context.Context, job *river.Job[SendEmailArgs]) error {
	return w.svc.Deliver(ctx, job.Args.EmailLogID, job.Args.Recipients, job.Args.Template, job.Args.Data)
}

// Enqueuer records an email_log row and enqueues a durable send_email job. Used
// by the auth flows (reset / verify / invite) so a transient SMTP outage is
// retried by River instead of lost. tenantID may be uuid.Nil for instance mail.
type Enqueuer struct {
	svc    *Service
	client *river.Client[pgx.Tx]
}

// NewEnqueuer wires the enqueuer once River has started.
func NewEnqueuer(svc *Service, client *river.Client[pgx.Tx]) *Enqueuer {
	return &Enqueuer{svc: svc, client: client}
}

// Enabled delegates to the underlying Service.Enabled, reporting whether a
// usable SMTP transport is currently configured. When false, a queued job will
// be delivered as a no-op (the log row marked "smtp not configured"), so
// callers surface email_sent=false to their clients.
func (e *Enqueuer) Enabled(ctx context.Context) bool {
	return e.svc.Enabled(ctx)
}

// Enqueue logs + schedules an email. The subject is resolved from the template's
// static subject map.
func (e *Enqueuer) Enqueue(ctx context.Context, tenantID uuid.UUID, recipients []string, template string, data map[string]any) error {
	subject, ok := subjects[template]
	if !ok {
		return fmt.Errorf("unknown email template %q", template)
	}
	logID := e.svc.insertLog(ctx, tenantID, recipients, subject, template)
	if _, err := e.client.Insert(ctx, SendEmailArgs{
		EmailLogID: logID,
		TenantID:   tenantID,
		Recipients: recipients,
		Template:   template,
		Data:       data,
	}, nil); err != nil {
		return fmt.Errorf("enqueue send_email: %w", err)
	}
	return nil
}

// EnqueueTx behaves like Enqueue but inserts BOTH the email_log row and the
// send_email River job inside the CALLER's transaction (tx) instead of
// opening new ones, so the enqueue commits or rolls back atomically with
// whatever else the caller is doing in the same tx. This is a transactional
// outbox: internal/vuln's alert dispatcher uses it to enqueue a batched
// vuln_alert email in the SAME tx as claiming the underlying findings
// (stamping notified_at) — if the email enqueue fails, the whole tx rolls
// back and the claim is undone, so the findings are retried on the next
// dispatch cycle instead of being silently claimed-but-unemailed.
func (e *Enqueuer) EnqueueTx(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, recipients []string, template string, data map[string]any) error {
	subject, ok := subjects[template]
	if !ok {
		return fmt.Errorf("unknown email template %q", template)
	}
	logID, err := e.svc.insertLogTx(ctx, tx, tenantID, recipients, subject, template)
	if err != nil {
		return fmt.Errorf("insert email log (tx): %w", err)
	}
	if _, err := e.client.InsertTx(ctx, tx, SendEmailArgs{
		EmailLogID: logID,
		TenantID:   tenantID,
		Recipients: recipients,
		Template:   template,
		Data:       data,
	}, nil); err != nil {
		return fmt.Errorf("enqueue send_email (tx): %w", err)
	}
	return nil
}
