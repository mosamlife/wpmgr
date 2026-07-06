package mailer

import (
	"context"
	"testing"
)

// stubResolver is a configurable Resolver for SendMessage tests. It never
// touches Postgres, so these tests exercise the per-send transport resolution
// (GH #144) without a live DB.
type stubResolver struct {
	transport Transport
	ok        bool
	err       error
	called    bool
}

func (r *stubResolver) Resolve(context.Context) (Transport, bool, error) {
	r.called = true
	return r.transport, r.ok, r.err
}

// TestSendMessageSkipsWhenSMTPNotConfigured asserts SendMessage resolves the
// transport ON EVERY CALL (not a boot-time snapshot) and reports "skipped"
// rather than silently dropping the message when no DB row or env fallback is
// configured — the GH #144 defect was that uptime alerts used a boot-time
// snapshot instead of resolving per-send at all.
func TestSendMessageSkipsWhenSMTPNotConfigured(t *testing.T) {
	resolver := &stubResolver{ok: false}
	svc := NewService(resolver, nil, nil, "https://manage.wpmgr.app", "support@wpmgr.app", nil)

	outcome, err := svc.SendMessage(context.Background(), []string{"ops@example.com"}, "subject", "body")
	if err != nil {
		t.Fatalf("expected nil error on skip, got %v", err)
	}
	if !resolver.called {
		t.Fatal("expected Resolve to be called (per-send resolution), but it was not")
	}
	if outcome.Status != SendSkipped {
		t.Fatalf("expected status %q, got %q", SendSkipped, outcome.Status)
	}
	if outcome.Reason != "smtp_not_configured" {
		t.Fatalf("expected reason %q, got %q", "smtp_not_configured", outcome.Reason)
	}
}

// TestSendMessageSkipsWhenNoRecipients asserts SendMessage never dials SMTP
// (and never calls Resolve) for an empty recipient list, and reports the
// skip rather than a silent no-op.
func TestSendMessageSkipsWhenNoRecipients(t *testing.T) {
	resolver := &stubResolver{ok: true, transport: Transport{Host: "smtp.example.com", From: "alerts@example.com"}}
	svc := NewService(resolver, nil, nil, "https://manage.wpmgr.app", "support@wpmgr.app", nil)

	outcome, err := svc.SendMessage(context.Background(), nil, "subject", "body")
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if resolver.called {
		t.Fatal("expected Resolve NOT to be called for an empty recipient list")
	}
	if outcome.Status != SendSkipped || outcome.Reason != "no_recipients" {
		t.Fatalf("expected {skipped,no_recipients}, got %+v", outcome)
	}
}
