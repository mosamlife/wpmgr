package uptime

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/httpclient"
)

type stubMailer struct {
	mu         sync.Mutex
	calls      int
	recipients []string
	subject    string
}

func (m *stubMailer) Send(_ context.Context, recipients []string, subject, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	m.recipients = recipients
	m.subject = subject
	return nil
}

// TestDispatcherFiresBothChannels asserts the dispatcher calls the mailer with
// the right recipients AND POSTs a correctly-signed webhook body.
func TestDispatcherFiresBothChannels(t *testing.T) {
	const secret = "test-webhook-secret"
	var (
		mu       sync.Mutex
		gotBody  []byte
		gotSig   string
		gotCount int
	)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotBody = body
		gotSig = r.Header.Get("X-WPMgr-Signature")
		gotCount++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer hook.Close()

	mailer := &stubMailer{}
	// Allow loopback so the webhook httptest server is reachable.
	poster := NewSSRFWebhookPoster(httpclient.New(httpclient.Config{Timeout: 5 * time.Second, AllowPrivateNetworks: true}))
	d := NewDispatcher(mailer, poster, nil, nil)

	tenantID, siteID := uuid.New(), uuid.New()
	cfg := AlertConfig{
		TenantID:        tenantID,
		EmailRecipients: []string{"ops@example.com", "oncall@example.com"},
		WebhookURL:      hook.URL,
		WebhookSecret:   secret,
		Enabled:         true,
	}
	alert := Alert{
		Kind:       AlertDown,
		TenantID:   tenantID,
		SiteID:     siteID,
		SiteURL:    "https://site.example.com",
		HTTPStatus: 503,
		Error:      "http status 503",
		FiredAt:    time.Now(),
	}
	d.Fire(context.Background(), cfg, alert)

	mailer.mu.Lock()
	if mailer.calls != 1 {
		t.Fatalf("expected mailer called once, got %d", mailer.calls)
	}
	if len(mailer.recipients) != 2 || mailer.recipients[0] != "ops@example.com" {
		t.Fatalf("unexpected recipients: %v", mailer.recipients)
	}
	mailer.mu.Unlock()

	mu.Lock()
	defer mu.Unlock()
	if gotCount != 1 {
		t.Fatalf("expected webhook posted once, got %d", gotCount)
	}
	// Verify the HMAC-SHA256 signature over the exact body.
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(gotBody)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if gotSig != want {
		t.Fatalf("webhook signature mismatch: got %q want %q", gotSig, want)
	}
	var payload WebhookPayload
	if err := json.Unmarshal(gotBody, &payload); err != nil {
		t.Fatalf("webhook body not valid JSON: %v", err)
	}
	if payload.Event != "uptime.down" || payload.SiteID != siteID.String() {
		t.Fatalf("unexpected webhook payload: %+v", payload)
	}
}
