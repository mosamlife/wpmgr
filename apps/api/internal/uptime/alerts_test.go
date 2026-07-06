package uptime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
)

// TestEvaluateTransitionDedupe exercises the alert state machine: N consecutive
// downs fire exactly ONE down alert (on the threshold crossing), subsequent
// downs are de-duped, and the first up after an incident fires exactly ONE
// recovery.
func TestEvaluateTransitionDedupe(t *testing.T) {
	const threshold = 2
	now := time.Now()
	st := AlertState{LastStatus: StatusUnknown}

	// Probe 1: down. consecutive=1 < threshold ⇒ no alert.
	tr := Evaluate(st, false, threshold, now)
	if tr.FireDown || tr.FireRecovery {
		t.Fatalf("probe 1: expected no alert, got %+v", tr)
	}
	if tr.NewState.ConsecutiveDown != 1 || tr.NewState.InIncident {
		t.Fatalf("probe 1: unexpected state %+v", tr.NewState)
	}
	st = tr.NewState

	// Probe 2: down. consecutive=2 >= threshold ⇒ fire ONE down alert.
	tr = Evaluate(st, false, threshold, now)
	if !tr.FireDown || tr.FireRecovery {
		t.Fatalf("probe 2: expected one down alert, got %+v", tr)
	}
	if !tr.NewState.InIncident {
		t.Fatalf("probe 2: expected in_incident, got %+v", tr.NewState)
	}
	st = tr.NewState

	// Probe 3: down again. Already in incident ⇒ de-duped (no alert).
	tr = Evaluate(st, false, threshold, now)
	if tr.FireDown || tr.FireRecovery {
		t.Fatalf("probe 3: expected de-dupe (no alert), got %+v", tr)
	}
	if tr.NewState.ConsecutiveDown != 3 {
		t.Fatalf("probe 3: expected consecutive=3, got %d", tr.NewState.ConsecutiveDown)
	}
	st = tr.NewState

	// Probe 4: up. In incident ⇒ fire ONE recovery, clear incident.
	tr = Evaluate(st, true, threshold, now)
	if !tr.FireRecovery || tr.FireDown {
		t.Fatalf("probe 4: expected one recovery alert, got %+v", tr)
	}
	if tr.NewState.InIncident || tr.NewState.ConsecutiveDown != 0 || tr.NewState.LastStatus != StatusUp {
		t.Fatalf("probe 4: unexpected recovered state %+v", tr.NewState)
	}
	st = tr.NewState

	// Probe 5: up again. Not in incident ⇒ no alert (no recovery spam).
	tr = Evaluate(st, true, threshold, now)
	if tr.FireDown || tr.FireRecovery {
		t.Fatalf("probe 5: expected no alert, got %+v", tr)
	}
}

// TestEvaluateThresholdOne fires immediately on the first down when threshold=1.
func TestEvaluateThresholdOne(t *testing.T) {
	tr := Evaluate(AlertState{LastStatus: StatusUnknown}, false, 1, time.Now())
	if !tr.FireDown {
		t.Fatalf("threshold=1: expected immediate down alert, got %+v", tr)
	}
}

// ---------------------------------------------------------------------------
// GH #144 — Fire must record the REAL email/webhook delivery outcome, not
// merely "recipients/URL were configured". Every test below FAILS against the
// pre-fix recordAudit (which set a bare "emailed"/"webhooked" bool from
// len(cfg.EmailRecipients)>0 / cfg.WebhookURL!="" and never looked at the
// mailer's actual return value): meta["email_status"] would be nil (absent),
// never equal to "skipped"/"failed"/"sent", and the legacy "emailed" key
// would be true even for a skipped/failed send.
// ---------------------------------------------------------------------------

// outcomeMailerStub is a configurable stub Mailer that reports a fixed
// SendResult/error and records whether it was invoked.
type outcomeMailerStub struct {
	result SendResult
	err    error
	called bool
}

func (m *outcomeMailerStub) Send(context.Context, []string, string, string) (SendResult, error) {
	m.called = true
	return m.result, m.err
}

// captureAuditSink is a stub auditSink that records every Event passed to
// Record, so Dispatcher tests can assert on the persisted metadata without a
// live Postgres pool.
type captureAuditSink struct {
	events []audit.Event
}

func (s *captureAuditSink) Record(_ context.Context, e audit.Event) (audit.Entry, error) {
	s.events = append(s.events, e)
	return audit.Entry{}, nil
}

func testAlert() Alert {
	return Alert{
		Kind:       AlertDown,
		TenantID:   uuid.New(),
		SiteID:     uuid.New(),
		SiteURL:    "https://site.example.com",
		HTTPStatus: 503,
		Error:      "http status 503",
		FiredAt:    time.Now(),
	}
}

// TestFireRecordsSkippedEmailOutcome: a mailer reporting a SKIPPED send (e.g.
// SMTP not configured) must be recorded as skipped, never as a truthy "sent".
func TestFireRecordsSkippedEmailOutcome(t *testing.T) {
	mailer := &outcomeMailerStub{result: SendResult{Status: SendResultSkipped, Reason: "smtp_not_configured"}}
	sink := &captureAuditSink{}
	d := NewDispatcher(mailer, nil, sink, nil)

	cfg := AlertConfig{TenantID: uuid.New(), EmailRecipients: []string{"ops@example.com"}}
	d.Fire(context.Background(), cfg, testAlert())

	if !mailer.called {
		t.Fatal("expected the mailer to be called")
	}
	if len(sink.events) != 1 {
		t.Fatalf("expected exactly one audit event, got %d", len(sink.events))
	}
	meta := sink.events[0].Metadata
	if meta["email_status"] != SendResultSkipped {
		t.Fatalf("expected email_status=%q, got %v (full metadata: %+v)", SendResultSkipped, meta["email_status"], meta)
	}
	if meta["email_reason"] != "smtp_not_configured" {
		t.Fatalf("expected email_reason=%q, got %v", "smtp_not_configured", meta["email_reason"])
	}
	if v, ok := meta["emailed"]; ok {
		t.Fatalf("expected no legacy 'emailed' key on new rows, got %v", v)
	}
}

// TestFireRecordsFailedEmailOutcome asserts a mailer-reported failure is
// recorded as "failed" with its reason, not "sent"/emailed.
func TestFireRecordsFailedEmailOutcome(t *testing.T) {
	mailer := &outcomeMailerStub{
		result: SendResult{Status: SendResultFailed, Reason: "smtp_send_failed"},
		err:    errors.New("smtp: 421 connection timed out"),
	}
	sink := &captureAuditSink{}
	d := NewDispatcher(mailer, nil, sink, nil)

	cfg := AlertConfig{TenantID: uuid.New(), EmailRecipients: []string{"ops@example.com"}}
	d.Fire(context.Background(), cfg, testAlert())

	meta := sink.events[0].Metadata
	if meta["email_status"] != SendResultFailed {
		t.Fatalf("expected email_status=%q, got %v", SendResultFailed, meta["email_status"])
	}
	if meta["email_reason"] != "smtp_send_failed" {
		t.Fatalf("expected email_reason=%q, got %v", "smtp_send_failed", meta["email_reason"])
	}
}

// TestFireRecordsSentEmailOutcome asserts a successful send is recorded as
// "sent" with no email_reason key at all.
func TestFireRecordsSentEmailOutcome(t *testing.T) {
	mailer := &outcomeMailerStub{result: SendResult{Status: SendResultSent}}
	sink := &captureAuditSink{}
	d := NewDispatcher(mailer, nil, sink, nil)

	cfg := AlertConfig{TenantID: uuid.New(), EmailRecipients: []string{"ops@example.com"}}
	d.Fire(context.Background(), cfg, testAlert())

	meta := sink.events[0].Metadata
	if meta["email_status"] != SendResultSent {
		t.Fatalf("expected email_status=%q, got %v", SendResultSent, meta["email_status"])
	}
	if _, ok := meta["email_reason"]; ok {
		t.Fatalf("expected no email_reason key on a successful send, got %v", meta["email_reason"])
	}
}

// TestFireNoRecipientsSkipsMailerAndRecordsSkipped is the no-recipients-
// configured case: the mailer must NOT be called at all (there is nothing to
// send to), and the audit row must show a skip with a distinguishing reason —
// never a truthy "emailed" the way the pre-fix code recorded it.
func TestFireNoRecipientsSkipsMailerAndRecordsSkipped(t *testing.T) {
	mailer := &outcomeMailerStub{result: SendResult{Status: SendResultSent}} // would be a lie if ever called
	sink := &captureAuditSink{}
	d := NewDispatcher(mailer, nil, sink, nil)

	cfg := AlertConfig{TenantID: uuid.New()} // no EmailRecipients, no WebhookURL
	d.Fire(context.Background(), cfg, testAlert())

	if mailer.called {
		t.Fatal("expected the mailer NOT to be called when no recipients are configured")
	}
	meta := sink.events[0].Metadata
	if meta["email_status"] != SendResultSkipped {
		t.Fatalf("expected email_status=%q, got %v", SendResultSkipped, meta["email_status"])
	}
	if meta["email_reason"] != "no_recipients_configured" {
		t.Fatalf("expected email_reason=%q, got %v", "no_recipients_configured", meta["email_reason"])
	}
	if meta["webhook_status"] != SendResultSkipped {
		t.Fatalf("expected webhook_status=%q, got %v", SendResultSkipped, meta["webhook_status"])
	}
}
