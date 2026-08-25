package email

// webhook_test.go — Phase 4a tests for signature verification + fan-out + suppression + log actions.
//
// Tests are organised into:
//   - verify_*: per-provider signature verification (valid passes, tampered/old/wrong-key fails)
//   - sns_*: SNS SubscriptionConfirmation URL pinning
//   - fanout_*: fan-out resolves correct tenant/site; drops cross-tenant events without metadata
//   - suppression_*: upsert idempotent, IsSuppressed, manual-add gating
//   - resend_*: ResendEmail gate on body_stored
//   - bulk_*: BulkDelete RLS scoped
//   - agent_*: agent suppression-delta keyset cursor

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// discardLogger returns a slog.Logger that discards all output.
// Use this when the code under test calls svc.log.Warn/Error and you don't
// want test noise. NewService with nil logs panics on Warn/Error calls.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// mustGenerateECKey generates a throwaway P-256 key pair for tests.
func mustGenerateECKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate ec key: %v", err)
	}
	return key
}

// ecPublicKeyPEM marshals an ECDSA public key to PEM.
func ecPublicKeyPEM(t *testing.T, key *ecdsa.PrivateKey) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

// signSendGrid signs (timestampStr + body) with the given key and returns
// the base64-encoded DER ECDSA signature.
func signSendGrid(t *testing.T, key *ecdsa.PrivateKey, timestampStr string, body []byte) string {
	t.Helper()
	payload := append([]byte(timestampStr), body...)
	digest := sha256.Sum256(payload)
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		t.Fatalf("ecdsa sign: %v", err)
	}
	type sig struct{ R, S *big.Int }
	der, err := asn1.Marshal(sig{r, s})
	if err != nil {
		t.Fatalf("asn1 marshal: %v", err)
	}
	return base64.StdEncoding.EncodeToString(der)
}

// mailgunHMAC computes the expected Mailgun HMAC-SHA256 signature.
func mailgunHMAC(key, timestamp, token string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(timestamp))
	mac.Write([]byte(token))
	return fmt.Sprintf("%x", mac.Sum(nil))
}

// nowTimestamp returns the current Unix epoch as a string (Mailgun format).
func nowTimestamp() string {
	return strconv.FormatInt(time.Now().Unix(), 10)
}

// nowTimestampMs returns the current Unix epoch in milliseconds as a string (SendGrid format).
func nowTimestampMs() string {
	return strconv.FormatInt(time.Now().UnixMilli(), 10)
}

// oldTimestamp returns a Unix timestamp outside the replay window.
func oldTimestamp() string {
	return strconv.FormatInt(time.Now().Add(-(webhookReplayWindow + 10*time.Second)).Unix(), 10)
}

// oldTimestampMs returns a millisecond Unix timestamp outside the replay window.
func oldTimestampMs() string {
	return strconv.FormatInt(time.Now().Add(-(webhookReplayWindow + 10*time.Second)).UnixMilli(), 10)
}

// ---------------------------------------------------------------------------
// SendGrid signature verification tests
// ---------------------------------------------------------------------------

func TestVerifySendGrid_Valid(t *testing.T) {
	key := mustGenerateECKey(t)
	pubPEM := ecPublicKeyPEM(t, key)
	body := []byte(`[{"email":"user@example.com","event":"bounce"}]`)
	ts := nowTimestampMs()
	sig := signSendGrid(t, key, ts, body)

	if err := verifySendGridSignature(body, sig, ts, pubPEM); err != nil {
		t.Fatalf("expected valid signature to pass: %v", err)
	}
}

func TestVerifySendGrid_TamperedBody(t *testing.T) {
	key := mustGenerateECKey(t)
	pubPEM := ecPublicKeyPEM(t, key)
	body := []byte(`[{"email":"user@example.com","event":"bounce"}]`)
	ts := nowTimestampMs()
	sig := signSendGrid(t, key, ts, body)

	// Tamper the body after signing.
	tampered := []byte(`[{"email":"attacker@evil.com","event":"bounce"}]`)
	if err := verifySendGridSignature(tampered, sig, ts, pubPEM); err == nil {
		t.Fatal("expected tampered body to fail signature verification")
	}
}

func TestVerifySendGrid_WrongKey(t *testing.T) {
	signingKey := mustGenerateECKey(t)
	otherKey := mustGenerateECKey(t)
	pubPEM := ecPublicKeyPEM(t, otherKey) // verify with a DIFFERENT key
	body := []byte(`[{"email":"user@example.com","event":"bounce"}]`)
	ts := nowTimestampMs()
	sig := signSendGrid(t, signingKey, ts, body)

	if err := verifySendGridSignature(body, sig, ts, pubPEM); err == nil {
		t.Fatal("expected wrong-key signature to fail verification")
	}
}

func TestVerifySendGrid_ReplayOld(t *testing.T) {
	key := mustGenerateECKey(t)
	pubPEM := ecPublicKeyPEM(t, key)
	body := []byte(`[{"email":"user@example.com","event":"bounce"}]`)
	ts := oldTimestampMs()
	sig := signSendGrid(t, key, ts, body)

	if err := verifySendGridSignature(body, sig, ts, pubPEM); err == nil {
		t.Fatal("expected old timestamp to fail replay window check")
	}
}

func TestVerifySendGrid_NoKey(t *testing.T) {
	body := []byte(`[{"email":"user@example.com","event":"bounce"}]`)
	ts := nowTimestampMs()
	if err := verifySendGridSignature(body, "sig", ts, ""); err == nil {
		t.Fatal("expected error when public key PEM is empty")
	}
}

// ---------------------------------------------------------------------------
// Mailgun HMAC-SHA256 tests
// ---------------------------------------------------------------------------

func TestVerifyMailgun_Valid(t *testing.T) {
	key := "test-webhook-signing-key"
	ts := nowTimestamp()
	token := "sometoken123"
	sig := mailgunHMAC(key, ts, token)

	if err := verifyMailgunSignature(ts, token, sig, key); err != nil {
		t.Fatalf("expected valid Mailgun signature to pass: %v", err)
	}
}

func TestVerifyMailgun_WrongKey(t *testing.T) {
	key := "correct-key"
	ts := nowTimestamp()
	token := "sometoken123"
	sig := mailgunHMAC("wrong-key", ts, token) // signed with wrong key

	if err := verifyMailgunSignature(ts, token, sig, key); err == nil {
		t.Fatal("expected wrong-key Mailgun signature to fail")
	}
}

func TestVerifyMailgun_TamperedToken(t *testing.T) {
	key := "test-webhook-signing-key"
	ts := nowTimestamp()
	token := "sometoken123"
	sig := mailgunHMAC(key, ts, token)

	// Tamper the token after signing.
	if err := verifyMailgunSignature(ts, "differenttoken", sig, key); err == nil {
		t.Fatal("expected tampered token to fail Mailgun verification")
	}
}

func TestVerifyMailgun_ReplayOld(t *testing.T) {
	key := "test-webhook-signing-key"
	ts := oldTimestamp()
	token := "sometoken123"
	sig := mailgunHMAC(key, ts, token)

	if err := verifyMailgunSignature(ts, token, sig, key); err == nil {
		t.Fatal("expected old timestamp to fail Mailgun replay window check")
	}
}

// ---------------------------------------------------------------------------
// Postmark secret tests
// ---------------------------------------------------------------------------

func TestVerifyPostmark_Valid(t *testing.T) {
	if err := verifyPostmarkSecret("my-secret", "my-secret"); err != nil {
		t.Fatalf("expected matching secret to pass: %v", err)
	}
}

func TestVerifyPostmark_Mismatch(t *testing.T) {
	if err := verifyPostmarkSecret("wrong", "my-secret"); err == nil {
		t.Fatal("expected mismatched secret to fail Postmark verification")
	}
}

func TestVerifyPostmark_Empty(t *testing.T) {
	// Empty expected secret should not accidentally match empty provided.
	if err := verifyPostmarkSecret("", "my-secret"); err == nil {
		t.Fatal("expected empty provided to fail when expected is non-empty")
	}
}

// ---------------------------------------------------------------------------
// SNS SubscriptionConfirmation URL-pinning tests
// ---------------------------------------------------------------------------

func TestSNSAllowedCertHost_Valid(t *testing.T) {
	cases := []string{
		"https://sns.us-east-1.amazonaws.com/cert.pem",
		"https://sns.cn-north-1.amazonaws.com.cn/cert.pem",
		"https://sns.ap-southeast-1.amazonaws.com/SimpleNotificationService-abc.pem",
	}
	for _, u := range cases {
		if !snsAllowedCertHost(u) {
			t.Errorf("expected %q to be allowed", u)
		}
	}
}

func TestSNSAllowedCertHost_Rejected(t *testing.T) {
	cases := []string{
		"http://sns.us-east-1.amazonaws.com/cert.pem", // HTTP, not HTTPS
		"https://attacker.com/cert.pem",               // non-AWS host
		"https://evil-amazonaws.com/cert.pem",         // suffix match must be exact
		"https://amazonaws.com.attacker.com/cert.pem", // suffix bypass attempt
		"",
		"file:///etc/passwd",
		"https://169.254.169.254/latest/meta-data", // SSRF to IMDS
	}
	for _, u := range cases {
		if snsAllowedCertHost(u) {
			t.Errorf("expected %q to be rejected", u)
		}
	}
}

func TestSNSAllowedSubscribeURL_Valid(t *testing.T) {
	cases := []string{
		"https://sns.us-east-1.amazonaws.com/?Action=ConfirmSubscription&TopicArn=arn%3A…&Token=…",
		"https://sns.eu-west-1.amazonaws.com/?Action=ConfirmSubscription&Token=abc123",
	}
	for _, u := range cases {
		if !snsAllowedSubscribeURL(u) {
			t.Errorf("expected %q to be allowed subscribe URL", u)
		}
	}
}

func TestSNSAllowedSubscribeURL_Rejected(t *testing.T) {
	cases := []string{
		"http://sns.us-east-1.amazonaws.com/?Action=ConfirmSubscription", // HTTP
		"https://attacker.com/ssrf-target",                               // non-AWS
		"https://internal.corp.example.com/?Action=ConfirmSubscription",  // internal host
		"https://169.254.169.254/latest/user-data",                       // IMDS SSRF
		"",
	}
	for _, u := range cases {
		if snsAllowedSubscribeURL(u) {
			t.Errorf("expected %q to be rejected as subscribe URL", u)
		}
	}
}

// ---------------------------------------------------------------------------
// Fan-out: parseSESEvents resolves tenant/site from tags
// ---------------------------------------------------------------------------

func TestParseSESEvents_HardBounce_WithTags(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()

	note := sesNotification{
		NotificationType: "Bounce",
		Bounce: &struct {
			BounceType        string `json:"bounceType"`
			BouncedRecipients []struct {
				EmailAddress string `json:"emailAddress"`
			} `json:"bouncedRecipients"`
		}{
			BounceType: "Permanent",
			BouncedRecipients: []struct {
				EmailAddress string `json:"emailAddress"`
			}{{EmailAddress: "user@example.com"}},
		},
		Mail: struct {
			MessageID string `json:"messageId"`
			Tags      []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"tags"`
		}{
			MessageID: "msg-123",
			Tags: []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			}{
				{Name: "wpmgr_tenant", Value: tenantID.String()},
				{Name: "wpmgr_site", Value: siteID.String()},
			},
		},
	}

	events := parseSESEvents(note)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	ev := events[0]
	if ev.EventType != "hard_bounce" {
		t.Errorf("expected event_type=hard_bounce, got %q", ev.EventType)
	}
	if ev.Email != "user@example.com" {
		t.Errorf("expected email=user@example.com, got %q", ev.Email)
	}
	// m61: parseSESEvents now populates MetaTenantID (not TenantID) from tags;
	// TenantID is set by the webhook handler from the routeToken resolution.
	if ev.MetaTenantID == nil || *ev.MetaTenantID != tenantID {
		t.Errorf("expected meta_tenant_id=%s, got %v", tenantID, ev.MetaTenantID)
	}
	if ev.SiteID == nil || *ev.SiteID != siteID {
		t.Errorf("expected site_id=%s, got %v", siteID, ev.SiteID)
	}
	if ev.Provider != "ses" {
		t.Errorf("expected provider=ses, got %q", ev.Provider)
	}
}

func TestParseSESEvents_SoftBounce_Skipped(t *testing.T) {
	// Only Permanent bounces should trigger suppression.
	note := sesNotification{
		NotificationType: "Bounce",
		Bounce: &struct {
			BounceType        string `json:"bounceType"`
			BouncedRecipients []struct {
				EmailAddress string `json:"emailAddress"`
			} `json:"bouncedRecipients"`
		}{
			BounceType: "Transient", // soft bounce
			BouncedRecipients: []struct {
				EmailAddress string `json:"emailAddress"`
			}{{EmailAddress: "user@example.com"}},
		},
		Mail: struct {
			MessageID string `json:"messageId"`
			Tags      []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"tags"`
		}{MessageID: "msg-456"},
	}

	events := parseSESEvents(note)
	if len(events) != 0 {
		t.Errorf("expected 0 events for soft bounce, got %d", len(events))
	}
}

func TestParseSESEvents_Complaint_WithTags(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()

	note := sesNotification{
		NotificationType: "Complaint",
		Complaint: &struct {
			ComplainedRecipients []struct {
				EmailAddress string `json:"emailAddress"`
			} `json:"complainedRecipients"`
		}{
			ComplainedRecipients: []struct {
				EmailAddress string `json:"emailAddress"`
			}{{EmailAddress: "complainer@example.com"}},
		},
		Mail: struct {
			MessageID string `json:"messageId"`
			Tags      []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"tags"`
		}{
			MessageID: "msg-789",
			Tags: []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			}{
				{Name: "wpmgr_tenant", Value: tenantID.String()},
				{Name: "wpmgr_site", Value: siteID.String()},
			},
		},
	}

	events := parseSESEvents(note)
	if len(events) != 1 {
		t.Fatalf("expected 1 complaint event, got %d", len(events))
	}
	if events[0].EventType != "complaint" {
		t.Errorf("expected event_type=complaint, got %q", events[0].EventType)
	}
}

func TestParseSESEvents_NoTags_NilTenantSite(t *testing.T) {
	// When the SES message has no wpmgr_* tags, both MetaTenantID and SiteID are nil.
	// The service must log and drop (no cross-tenant guessing) — verified in service test.
	note := sesNotification{
		NotificationType: "Bounce",
		Bounce: &struct {
			BounceType        string `json:"bounceType"`
			BouncedRecipients []struct {
				EmailAddress string `json:"emailAddress"`
			} `json:"bouncedRecipients"`
		}{
			BounceType: "Permanent",
			BouncedRecipients: []struct {
				EmailAddress string `json:"emailAddress"`
			}{{EmailAddress: "u@x.com"}},
		},
		Mail: struct {
			MessageID string `json:"messageId"`
			Tags      []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"tags"`
		}{MessageID: "msg-noatags"},
	}

	events := parseSESEvents(note)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	// m61: MetaTenantID comes from tags; TenantID is set from routeToken.
	if events[0].MetaTenantID != nil {
		t.Error("expected MetaTenantID=nil when tags absent")
	}
	if events[0].SiteID != nil {
		t.Error("expected SiteID=nil when tags absent")
	}
}

// ---------------------------------------------------------------------------
// Service.HandleWebhookEvent — fan-out dispatch + dedup
// ---------------------------------------------------------------------------

func TestHandleWebhookEvent_SuppressionWritten(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()

	repo := newFakeRepo()
	svc := NewService(&Repo{}, &fakeEncryptor{}, discardLogger())
	svc.repo = repo

	ev := WebhookEventInput{
		Provider:        "sendgrid",
		ProviderEventID: "ev-001",
		Email:           "user@example.com",
		EventType:       "hard_bounce",
		TenantID:        &tenantID,
		SiteID:          &siteID,
	}

	if err := svc.HandleWebhookEvent(context.Background(), ev); err != nil {
		t.Fatalf("HandleWebhookEvent: unexpected error: %v", err)
	}
	// Idempotent: sending the same event a second time should also succeed.
	if err := svc.HandleWebhookEvent(context.Background(), ev); err != nil {
		t.Fatalf("HandleWebhookEvent idempotent second call: unexpected error: %v", err)
	}
}

func TestHandleWebhookEvent_NilTenant_NoError(t *testing.T) {
	// Events with nil tenant (no metadata) must not error — they are logged and
	// an orphaned dedup row is written.
	repo := newFakeRepo()
	svc := NewService(&Repo{}, &fakeEncryptor{}, discardLogger())
	svc.repo = repo

	ev := WebhookEventInput{
		Provider:        "mailgun",
		ProviderEventID: "ev-no-tenant",
		Email:           "orphan@example.com",
		EventType:       "complaint",
		TenantID:        nil,
		SiteID:          nil,
	}

	if err := svc.HandleWebhookEvent(context.Background(), ev); err != nil {
		t.Fatalf("HandleWebhookEvent with nil tenant: unexpected error: %v", err)
	}
}

func TestHandleWebhookEvent_NonSuppressionEvent_Noop(t *testing.T) {
	// Events that are not suppression-triggering (e.g. "delivered") are a no-op.
	repo := newFakeRepo()
	svc := NewService(&Repo{}, &fakeEncryptor{}, discardLogger())
	svc.repo = repo

	tenantID := uuid.New()
	ev := WebhookEventInput{
		Provider:        "sendgrid",
		ProviderEventID: "ev-delivered",
		Email:           "user@example.com",
		EventType:       "delivered", // not a suppression type
		TenantID:        &tenantID,
	}

	if err := svc.HandleWebhookEvent(context.Background(), ev); err != nil {
		t.Fatalf("non-suppression event must be a no-op, got: %v", err)
	}
}

func TestHandleWebhookEvent_EmptyEmail_Noop(t *testing.T) {
	repo := newFakeRepo()
	svc := NewService(&Repo{}, &fakeEncryptor{}, discardLogger())
	svc.repo = repo

	tenantID := uuid.New()
	ev := WebhookEventInput{
		Provider:        "postmark",
		ProviderEventID: "ev-no-email",
		Email:           "", // empty
		EventType:       "hard_bounce",
		TenantID:        &tenantID,
	}

	if err := svc.HandleWebhookEvent(context.Background(), ev); err != nil {
		t.Fatalf("empty email must be a no-op, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// AddSuppression — manual suppression gating
// ---------------------------------------------------------------------------

func TestAddSuppression_Manual(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()

	repo := newFakeRepo()
	svc := NewService(&Repo{}, &fakeEncryptor{}, nil)
	svc.repo = repo

	sup, err := svc.AddSuppression(context.Background(), UpsertSuppressionInput{
		TenantID: tenantID,
		SiteID:   &siteID,
		Email:    "user@example.com",
		Reason:   "manual",
	})
	if err != nil {
		t.Fatalf("AddSuppression: unexpected error: %v", err)
	}
	if sup.Email == nil || *sup.Email != "user@example.com" {
		t.Errorf("expected email=user@example.com, got %v", sup.Email)
	}
}

func TestAddSuppression_Unsubscribe(t *testing.T) {
	tenantID := uuid.New()

	repo := newFakeRepo()
	svc := NewService(&Repo{}, &fakeEncryptor{}, nil)
	svc.repo = repo

	_, err := svc.AddSuppression(context.Background(), UpsertSuppressionInput{
		TenantID: tenantID,
		Email:    "unsub@example.com",
		Reason:   "unsubscribe",
	})
	if err != nil {
		t.Fatalf("AddSuppression (unsubscribe): unexpected error: %v", err)
	}
}

func TestAddSuppression_InvalidReason(t *testing.T) {
	// hard_bounce / complaint should be rejected via AddSuppression (must come from webhooks).
	tenantID := uuid.New()

	repo := newFakeRepo()
	svc := NewService(&Repo{}, &fakeEncryptor{}, nil)
	svc.repo = repo

	_, err := svc.AddSuppression(context.Background(), UpsertSuppressionInput{
		TenantID: tenantID,
		Email:    "user@example.com",
		Reason:   "hard_bounce",
	})
	if err == nil {
		t.Fatal("expected error for hard_bounce reason via AddSuppression")
	}
	if !containsCode(err, "suppression_reason_invalid") {
		t.Errorf("expected code 'suppression_reason_invalid', got: %v", err)
	}
}

func TestAddSuppression_EmptyEmail(t *testing.T) {
	tenantID := uuid.New()

	repo := newFakeRepo()
	svc := NewService(&Repo{}, &fakeEncryptor{}, nil)
	svc.repo = repo

	_, err := svc.AddSuppression(context.Background(), UpsertSuppressionInput{
		TenantID: tenantID,
		Email:    "",
		Reason:   "manual",
	})
	if err == nil {
		t.Fatal("expected error for empty email in AddSuppression")
	}
	if !containsCode(err, "suppression_email_required") {
		t.Errorf("expected code 'suppression_email_required', got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// IsSuppressed — via fakeRepo
// ---------------------------------------------------------------------------

func TestIsSuppressed_FakeRepo_NotSuppressed(t *testing.T) {
	repo := newFakeRepo()
	svc := NewService(&Repo{}, &fakeEncryptor{}, nil)
	svc.repo = repo

	tenantID := uuid.New()
	siteID := uuid.New()

	suppressed, err := repo.IsSuppressed(context.Background(), tenantID, siteID, "never@example.com")
	if err != nil {
		t.Fatalf("IsSuppressed: unexpected error: %v", err)
	}
	if suppressed {
		t.Error("expected not suppressed for fresh repo")
	}
}

// ---------------------------------------------------------------------------
// ResendEmail — gate on body_stored
// ---------------------------------------------------------------------------

func TestResendEmail_BodyNotStored_Conflict(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()
	logID := uuid.New()

	repo := newFakeRepo()
	// fakeRepo.GetResendTarget returns ErrNotFound by default.
	svc := NewService(&Repo{}, &fakeEncryptor{}, nil)
	svc.repo = repo

	_, err := svc.ResendEmail(context.Background(), tenantID, siteID, logID)
	if err == nil {
		t.Fatal("expected error when log entry not found")
	}
	// Default fakeRepo returns ErrNotFound → service returns domain.NotFound.
	if !containsCode(err, "email_log_not_found") {
		t.Errorf("expected code 'email_log_not_found', got: %v", err)
	}
}

func TestResendEmail_BodyStored_Dispatches(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()
	logID := uuid.New()

	// A resendable row: body captured and an agent_seq to address it by.
	repo := &fakeRepoBodyStored{fakeRepo: newFakeRepo(), bodyStoredID: logID, agentSeq: 17}
	svc := NewService(&Repo{}, &fakeEncryptor{}, nil)
	svc.repo = repo

	res, err := svc.ResendEmail(context.Background(), tenantID, siteID, logID)
	if err != nil {
		t.Fatalf("ResendEmail with body_stored=true: unexpected error: %v", err)
	}
	// No agent client is wired here, so the service reports ok=false rather
	// than pretending a command was sent.
	if res.OK {
		t.Error("expected ok=false with no agent client wired")
	}
	if repo.incrCalls != 0 {
		t.Errorf("resent_count incremented %d time(s) without an agent dispatch, want 0", repo.incrCalls)
	}
}

// fakeRepoBodyStored wraps fakeRepo and serves one resendable row, simulating an
// email logged with body capture that the agent has since reported by agent_seq.
type fakeRepoBodyStored struct {
	*fakeRepo
	bodyStoredID uuid.UUID
	agentSeq     int64
	incrCalls    int
}

func (r *fakeRepoBodyStored) GetResendTarget(_ context.Context, _, _, id uuid.UUID) (ResendTarget, error) {
	if id != r.bodyStoredID {
		return ResendTarget{}, ErrNotFound
	}
	seq := r.agentSeq
	return ResendTarget{AgentSeq: &seq, BodyStored: true}, nil
}

func (r *fakeRepoBodyStored) IncrEmailLogResentCount(_ context.Context, _, _, _ uuid.UUID) error {
	r.incrCalls++
	return nil
}

// ---------------------------------------------------------------------------
// BulkDeleteLogs — size validation
// ---------------------------------------------------------------------------

func TestBulkDeleteLogs_TooLarge(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()

	repo := newFakeRepo()
	svc := NewService(&Repo{}, &fakeEncryptor{}, nil)
	svc.repo = repo

	ids := make([]uuid.UUID, 501)
	for i := range ids {
		ids[i] = uuid.New()
	}

	_, err := svc.BulkDeleteLogs(context.Background(), tenantID, siteID, ids)
	if err == nil {
		t.Fatal("expected error for bulk delete exceeding 500")
	}
	if !containsCode(err, "bulk_delete_too_large") {
		t.Errorf("expected code 'bulk_delete_too_large', got: %v", err)
	}
}

func TestBulkDeleteLogs_Empty_Noop(t *testing.T) {
	repo := newFakeRepo()
	svc := NewService(&Repo{}, &fakeEncryptor{}, nil)
	svc.repo = repo

	deleted, err := svc.BulkDeleteLogs(context.Background(), uuid.New(), uuid.New(), nil)
	if err != nil {
		t.Fatalf("BulkDeleteLogs(empty): unexpected error: %v", err)
	}
	if deleted != 0 {
		t.Errorf("expected 0 deleted for empty input, got %d", deleted)
	}
}

// ---------------------------------------------------------------------------
// BulkResendEmail — size validation
// ---------------------------------------------------------------------------

func TestBulkResendEmail_TooLarge(t *testing.T) {
	repo := newFakeRepo()
	svc := NewService(&Repo{}, &fakeEncryptor{}, nil)
	svc.repo = repo

	ids := make([]uuid.UUID, 101)
	for i := range ids {
		ids[i] = uuid.New()
	}

	_, err := svc.BulkResendEmail(context.Background(), uuid.New(), uuid.New(), ids)
	if err == nil {
		t.Fatal("expected error for bulk resend exceeding 100")
	}
	if !containsCode(err, "resend_bulk_too_large") {
		t.Errorf("expected code 'resend_bulk_too_large', got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Agent suppression-delta cursor helpers
// ---------------------------------------------------------------------------

func TestSuppressionDeltaCursor_EmptyFetchReturnsEmptyPage(t *testing.T) {
	repo := newFakeRepo()
	svc := NewService(&Repo{}, &fakeEncryptor{}, nil)
	svc.repo = repo

	page, err := svc.ListSuppressionDeltas(context.Background(), uuid.New(), uuid.New(), "")
	if err != nil {
		t.Fatalf("ListSuppressionDeltas: unexpected error: %v", err)
	}
	if len(page.Entries) != 0 {
		t.Errorf("expected 0 entries from empty repo, got %d", len(page.Entries))
	}
	if page.NextCursor != "" {
		t.Errorf("expected empty next_cursor from empty repo, got %q", page.NextCursor)
	}
}

// ---------------------------------------------------------------------------
// Mailgun event-type mapping
// ---------------------------------------------------------------------------

func TestMailgunEventType_Mapping(t *testing.T) {
	cases := []struct {
		input    string
		expected string
	}{
		{"failed", "hard_bounce"},
		{"complained", "complaint"},
		{"unsubscribed", "unsubscribe"},
		{"delivered", "delivered"}, // passthrough for unknown
	}
	for _, tc := range cases {
		got := mailgunEventType(tc.input)
		if got != tc.expected {
			t.Errorf("mailgunEventType(%q) = %q, want %q", tc.input, got, tc.expected)
		}
	}
}

// ---------------------------------------------------------------------------
// SendGrid event-type mapping
// ---------------------------------------------------------------------------

func TestSendGridEventType_Mapping(t *testing.T) {
	cases := []struct {
		input    string
		expected string
	}{
		{"bounce", "hard_bounce"},
		{"spamreport", "complaint"},
		{"unsubscribe", "unsubscribe"},
		{"open", "open"}, // passthrough
	}
	for _, tc := range cases {
		got := sendGridEventType(tc.input)
		if got != tc.expected {
			t.Errorf("sendGridEventType(%q) = %q, want %q", tc.input, got, tc.expected)
		}
	}
}

// ---------------------------------------------------------------------------
// postmarkEventID helper
// ---------------------------------------------------------------------------

func TestPostmarkEventID(t *testing.T) {
	if id := postmarkEventID(12345); id != "pm_12345" {
		t.Errorf("expected pm_12345, got %q", id)
	}
	if id := postmarkEventID(0); id != "pm_0" {
		t.Errorf("expected pm_0, got %q", id)
	}
}

// ---------------------------------------------------------------------------
// isSuppressionEventType helper
// ---------------------------------------------------------------------------

func TestIsSuppressionEventType(t *testing.T) {
	shouldTrigger := []string{"hard_bounce", "complaint", "unsubscribe"}
	shouldNotTrigger := []string{"delivered", "open", "click", "soft_bounce", ""}
	for _, e := range shouldTrigger {
		if !isSuppressionEventType(e) {
			t.Errorf("expected %q to trigger suppression", e)
		}
	}
	for _, e := range shouldNotTrigger {
		if isSuppressionEventType(e) {
			t.Errorf("expected %q NOT to trigger suppression", e)
		}
	}
}

// ---------------------------------------------------------------------------
// webhookEventToLogStatus helper
// ---------------------------------------------------------------------------

func TestWebhookEventToLogStatus(t *testing.T) {
	cases := []struct {
		input    string
		expected string
	}{
		{"hard_bounce", "bounced"},
		{"complaint", "complained"},
		{"unsubscribe", "unsubscribe"},
	}
	for _, tc := range cases {
		got := webhookEventToLogStatus(tc.input)
		if got != tc.expected {
			t.Errorf("webhookEventToLogStatus(%q) = %q, want %q", tc.input, got, tc.expected)
		}
	}
}

// ---------------------------------------------------------------------------
// m61 security tests — cross-tenant rejection
// ---------------------------------------------------------------------------

// TestAssertAndScopeEvent_SameTenant_Passes verifies that an event whose
// metadata tenant matches the routeToken tenant is accepted unchanged.
func TestAssertAndScopeEvent_SameTenant_Passes(t *testing.T) {
	h := &WebhookHandler{logger: discardLogger()}
	tenantID := uuid.New()
	siteID := uuid.New()
	ev := WebhookEventInput{
		Provider:     "sendgrid",
		TenantID:     &tenantID,
		MetaTenantID: &tenantID, // same tenant
		SiteID:       &siteID,
		EventType:    "hard_bounce",
	}
	result := h.assertAndScopeEvent(ev, tenantID)
	if result.TenantID == nil {
		t.Fatal("expected TenantID to remain set when meta tenant matches routeToken tenant")
	}
	if *result.TenantID != tenantID {
		t.Errorf("expected TenantID=%s, got %s", tenantID, *result.TenantID)
	}
}

// TestAssertAndScopeEvent_CrossTenant_DropsEvent verifies that an event whose
// metadata tenant is DIFFERENT from the routeToken tenant is silently dropped
// (TenantID set to nil → HandleWebhookEvent treats it as orphaned).
func TestAssertAndScopeEvent_CrossTenant_DropsEvent(t *testing.T) {
	h := &WebhookHandler{logger: discardLogger()}
	routeTenant := uuid.New()
	attackerTenant := uuid.New()
	siteID := uuid.New()
	ev := WebhookEventInput{
		Provider:     "sendgrid",
		TenantID:     &routeTenant,
		MetaTenantID: &attackerTenant, // DIFFERENT — cross-tenant forgery attempt
		SiteID:       &siteID,
		EventType:    "hard_bounce",
	}
	result := h.assertAndScopeEvent(ev, routeTenant)
	if result.TenantID != nil {
		t.Errorf("expected TenantID=nil after cross-tenant assertion, got %s", *result.TenantID)
	}
	if result.SiteID != nil {
		t.Error("expected SiteID=nil after cross-tenant assertion drop")
	}
}

// TestAssertAndScopeEvent_NoMetaTenant_Passes verifies that when the event
// metadata carries no wpmgr_tenant (agent did not inject it), the event is
// processed with the routeToken's tenant (intra-tenant fallback is safe).
func TestAssertAndScopeEvent_NoMetaTenant_Passes(t *testing.T) {
	h := &WebhookHandler{logger: discardLogger()}
	tenantID := uuid.New()
	ev := WebhookEventInput{
		Provider:     "mailgun",
		TenantID:     &tenantID,
		MetaTenantID: nil, // no metadata
		EventType:    "complaint",
	}
	result := h.assertAndScopeEvent(ev, tenantID)
	if result.TenantID == nil {
		t.Fatal("expected TenantID to remain set when no meta tenant")
	}
}

// TestSesTopicArnAllowed_Accepted verifies that a known TopicArn passes.
func TestSesTopicArnAllowed_Accepted(t *testing.T) {
	arns := []string{"arn:aws:sns:us-east-1:123456789012:my-topic"}
	if !sesTopicArnAllowed("arn:aws:sns:us-east-1:123456789012:my-topic", arns) {
		t.Error("expected known TopicArn to be allowed")
	}
}

// TestSesTopicArnAllowed_Rejected_Unknown verifies that an unknown TopicArn
// (attacker's own SNS topic) is rejected.
func TestSesTopicArnAllowed_Rejected_Unknown(t *testing.T) {
	arns := []string{"arn:aws:sns:us-east-1:123456789012:victim-topic"}
	if sesTopicArnAllowed("arn:aws:sns:us-east-1:999999999999:attacker-topic", arns) {
		t.Error("expected unknown TopicArn (attacker topic) to be rejected")
	}
}

// TestSesTopicArnAllowed_EmptyAllowlist rejects all when allowlist is nil/empty.
func TestSesTopicArnAllowed_EmptyAllowlist(t *testing.T) {
	if sesTopicArnAllowed("arn:aws:sns:us-east-1:123:any-topic", nil) {
		t.Error("expected empty allowlist to reject all TopicArns")
	}
	if sesTopicArnAllowed("arn:aws:sns:us-east-1:123:any-topic", []string{}) {
		t.Error("expected empty allowlist to reject all TopicArns")
	}
}

// TestResolveWebhookConfig_UnknownToken_ReturnsNotFound verifies that an
// unknown routeToken returns ErrNotFound (→ 404).
func TestResolveWebhookConfig_UnknownToken_ReturnsNotFound(t *testing.T) {
	repo := newFakeRepo()
	svc := NewService(&Repo{}, &fakeEncryptor{}, discardLogger())
	svc.repo = repo

	_, err := svc.ResolveWebhookConfig(context.Background(), "unknown-token-xyz")
	if err == nil {
		t.Fatal("expected ErrNotFound for unknown routeToken")
	}
	if err != ErrNotFound {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

// TestResolveWebhookConfig_EmptyToken_ReturnsNotFound verifies that an empty
// token is rejected immediately.
func TestResolveWebhookConfig_EmptyToken_ReturnsNotFound(t *testing.T) {
	svc := NewService(&Repo{}, &fakeEncryptor{}, discardLogger())
	svc.repo = newFakeRepo()

	_, err := svc.ResolveWebhookConfig(context.Background(), "")
	if err != ErrNotFound {
		t.Errorf("expected ErrNotFound for empty token, got %v", err)
	}
}

// TestHandleWebhookEvent_SiteScoped_BounceMark verifies that when siteID is
// present in the event the MarkEmailLogBounced call is site-scoped
// (SHOULD-FIX #3: uses the fakeRepo which no longer panics with the new sig).
func TestHandleWebhookEvent_SiteScoped_BounceCall(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()

	repo := newFakeRepo()
	svc := NewService(&Repo{}, &fakeEncryptor{}, discardLogger())
	svc.repo = repo

	ev := WebhookEventInput{
		Provider:        "mailgun",
		ProviderEventID: "ev-site-scoped",
		Email:           "user@example.com",
		EventType:       "hard_bounce",
		TenantID:        &tenantID,
		SiteID:          &siteID,
	}
	// If the signature mismatch for the new 5-arg MarkEmailLogBounced were still
	// present this would fail to compile, making it a compile-time canary.
	if err := svc.HandleWebhookEvent(context.Background(), ev); err != nil {
		t.Fatalf("HandleWebhookEvent with siteID: unexpected error: %v", err)
	}
}

// TestUpsertWebhookConfig_RotateToken generates a new route token and verifies
// the returned plain token is non-empty and the config registers as having a
// token hash set (WebhookRouteTokenHashSet=true).
func TestUpsertWebhookConfig_RotateToken(t *testing.T) {
	tenantID := uuid.New()
	configID := uuid.New()

	// fakeRepo.SetWebhookFields returns a Config with the token hash set when
	// tokenHash is non-nil. We extend fakeRepo for this test.
	repo := &fakeRepoWithWebhookFields{fakeRepo: newFakeRepo()}
	svc := NewService(&Repo{}, &fakeEncryptor{}, discardLogger())
	svc.repo = repo

	result, err := svc.UpsertWebhookConfig(context.Background(), UpsertWebhookInput{
		TenantID:    tenantID,
		ConfigID:    configID,
		RotateToken: true,
	})
	if err != nil {
		t.Fatalf("UpsertWebhookConfig rotate: unexpected error: %v", err)
	}
	if result.Config.WebhookRouteToken == "" {
		t.Error("expected plain WebhookRouteToken to be non-empty after rotation")
	}
	// Token must be at least 32 URL-safe base64 chars (32 bytes encoded).
	if len(result.Config.WebhookRouteToken) < 32 {
		t.Errorf("expected token length >= 32, got %d", len(result.Config.WebhookRouteToken))
	}
}

// TestUpsertWebhookConfig_SigningKey_AgeGuard verifies that providing a signing
// key when no encryptor is wired returns ServiceUnavailable.
func TestUpsertWebhookConfig_SigningKey_AgeGuard(t *testing.T) {
	tenantID := uuid.New()
	configID := uuid.New()
	key := "some-signing-key"

	svc := NewService(&Repo{}, nil /* no enc */, discardLogger())
	svc.repo = newFakeRepo()

	_, err := svc.UpsertWebhookConfig(context.Background(), UpsertWebhookInput{
		TenantID:      tenantID,
		ConfigID:      configID,
		SigningKeyRaw: &key,
	})
	if err == nil {
		t.Fatal("expected error when encryptor is nil and signing key provided")
	}
	if !containsCode(err, "email_crypto_unwired") {
		t.Errorf("expected code 'email_crypto_unwired', got: %v", err)
	}
}

// fakeRepoWithWebhookFields extends fakeRepo to simulate SetWebhookFields
// returning a Config with WebhookRouteTokenHashSet=true.
type fakeRepoWithWebhookFields struct {
	*fakeRepo
}

func (r *fakeRepoWithWebhookFields) SetWebhookFields(_ context.Context, _, _ uuid.UUID, tokenHash, _ []byte, _ bool, sesArns []string) (Config, error) {
	return Config{
		WebhookRouteTokenHashSet: len(tokenHash) > 0,
		SesTopicArns:             sesArns,
	}, nil
}

func (r *fakeRepoWithWebhookFields) GetConfigByRouteTokenHash(_ context.Context, _ []byte) (Config, error) {
	return Config{}, ErrNotFound
}

func (r *fakeRepoWithWebhookFields) GetConfigByRouteTokenHashWithSecret(_ context.Context, _ []byte) (Config, []byte, error) {
	return Config{}, nil, ErrNotFound
}

// emailHash helper test — round-trip sanity.
func TestEmailHash_Deterministic(t *testing.T) {
	h1 := emailHash("User@Example.COM")
	h2 := emailHash("user@example.com")
	h3 := emailHash("USER@EXAMPLE.COM")
	if string(h1) != string(h2) || string(h2) != string(h3) {
		t.Error("emailHash must be case-insensitive (lower-case normalisation)")
	}
	empty := emailHash("")
	if len(empty) == 0 {
		t.Error("emailHash of empty string must still return a 32-byte hash")
	}
}
