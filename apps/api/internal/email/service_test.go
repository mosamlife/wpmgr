package email

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// ---------------------------------------------------------------------------
// fakes
// ---------------------------------------------------------------------------

// fakeEncryptor simulates age encryption with a simple reversible XOR
// (sufficient for testing the control-flow; never used in production).
type fakeEncryptor struct {
	encErr error
}

func (f *fakeEncryptor) Encrypt(plaintext []byte) ([]byte, error) {
	if f.encErr != nil {
		return nil, f.encErr
	}
	// Prepend a magic byte so we can detect "was encrypted" in tests.
	out := make([]byte, len(plaintext)+1)
	out[0] = 0xAE
	copy(out[1:], plaintext)
	return out, nil
}

func (f *fakeEncryptor) Decrypt(ciphertext []byte) ([]byte, error) {
	if len(ciphertext) < 1 || ciphertext[0] != 0xAE {
		return nil, errors.New("fake decrypt: not a fake ciphertext")
	}
	return ciphertext[1:], nil
}

// fakeRepo is an in-memory repository stub.
type fakeRepo struct {
	// site map: tenantID+siteID -> Config
	site map[string]Config
	// org map: tenantID -> Config
	org map[uuid.UUID]Config
	// storedCt tracks the ciphertext stored for the last upsert (for nil-sentinel tests)
	storedCt []byte
	// storedSetSecret tracks whether SetSecret was true on the last upsert
	storedSetSecret bool
	// corruptCt makes every stored ciphertext undecryptable, simulating a
	// secrets-at-rest key that changed after the secrets were written.
	corruptCt bool
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		site: make(map[string]Config),
		org:  make(map[uuid.UUID]Config),
	}
}

func siteKey(tenantID, siteID uuid.UUID) string {
	return tenantID.String() + "/" + siteID.String()
}

func (r *fakeRepo) GetSiteConfig(_ context.Context, tenantID, siteID uuid.UUID) (Config, error) {
	if cfg, ok := r.site[siteKey(tenantID, siteID)]; ok {
		return cfg, nil
	}
	return Config{}, ErrNotFound
}

func (r *fakeRepo) GetOrgConfig(_ context.Context, tenantID uuid.UUID) (Config, error) {
	if cfg, ok := r.org[tenantID]; ok {
		return cfg, nil
	}
	return Config{}, ErrNotFound
}

func (r *fakeRepo) GetSecretCiphertext(_ context.Context, tenantID, siteID uuid.UUID) ([]byte, error) {
	if cfg, ok := r.site[siteKey(tenantID, siteID)]; ok && cfg.SecretSet {
		if r.corruptCt {
			return []byte("undecryptable"), nil
		}
		// Return a fake ciphertext representing stored secret "stored_secret".
		b, _ := (&fakeEncryptor{}).Encrypt([]byte("stored_secret"))
		return b, nil
	}
	return nil, nil
}

func (r *fakeRepo) GetOrgSecretCiphertext(_ context.Context, tenantID uuid.UUID) ([]byte, error) {
	if cfg, ok := r.org[tenantID]; ok && cfg.SecretSet {
		if r.corruptCt {
			return []byte("undecryptable"), nil
		}
		b, _ := (&fakeEncryptor{}).Encrypt([]byte("stored_secret"))
		return b, nil
	}
	return nil, nil
}

func (r *fakeRepo) UpsertSiteConfig(_ context.Context, in upsertRepoInput) (Config, error) {
	r.storedSetSecret = in.SetSecret
	r.storedCt = in.SecretCiphertext
	id := uuid.New()
	cfg := Config{
		ID:             id,
		TenantID:       in.TenantID,
		SiteID:         in.SiteID,
		Provider:       in.Provider,
		FromAddress:    in.FromAddress,
		FromName:       in.FromName,
		ForceFromEmail: in.ForceFromEmail,
		ForceFromName:  in.ForceFromName,
		ReturnPath:     in.ReturnPath,
		Config:         in.Config,
		SecretSet:      in.SetSecret && len(in.SecretCiphertext) > 0,
		Mappings:       in.Mappings,
		LogEmails:      in.LogEmails,
		StoreBody:      in.StoreBody,
		RetentionDays:  in.RetentionDays,
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}
	if in.SiteID != nil {
		r.site[siteKey(in.TenantID, *in.SiteID)] = cfg
	}
	return cfg, nil
}

func (r *fakeRepo) UpsertOrgConfig(_ context.Context, in upsertRepoInput) (Config, error) {
	r.storedSetSecret = in.SetSecret
	r.storedCt = in.SecretCiphertext
	id := uuid.New()
	cfg := Config{
		ID:            id,
		TenantID:      in.TenantID,
		Provider:      in.Provider,
		SecretSet:     in.SetSecret && len(in.SecretCiphertext) > 0,
		LogEmails:     in.LogEmails,
		RetentionDays: in.RetentionDays,
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
	}
	r.org[in.TenantID] = cfg
	return cfg, nil
}

func (r *fakeRepo) ListSiteConfigs(_ context.Context, tenantID uuid.UUID, _, _ int32) ([]Config, error) {
	var out []Config
	for _, cfg := range r.site {
		if cfg.TenantID == tenantID {
			out = append(out, cfg)
		}
	}
	return out, nil
}

// Phase 3 stubs — log operations use a no-op in-memory implementation.

func (r *fakeRepo) IngestLogBatch(_ context.Context, _, _ uuid.UUID, entries []IngestEntry) (int64, error) {
	var max int64
	for _, e := range entries {
		if e.AgentSeq > max {
			max = e.AgentSeq
		}
	}
	return max, nil
}

func (r *fakeRepo) ListSiteLog(_ context.Context, _, _ uuid.UUID, _ LogListFilter) (LogListPage, error) {
	return LogListPage{}, nil
}

func (r *fakeRepo) GetLogEntry(_ context.Context, _, _, _ uuid.UUID) (LogDetail, error) {
	return LogDetail{}, ErrNotFound
}

func (r *fakeRepo) ListFleetLog(_ context.Context, _ uuid.UUID, _ LogListFilter) (LogListPage, error) {
	return LogListPage{}, nil
}

func (r *fakeRepo) GetSiteStats(_ context.Context, _, _ uuid.UUID, _, _ time.Time) (EmailStats, error) {
	return EmailStats{}, nil
}

func (r *fakeRepo) GetFleetStats(_ context.Context, _ uuid.UUID, _, _ time.Time) (EmailStats, error) {
	return EmailStats{}, nil
}

func (r *fakeRepo) GetFleetDelivery(_ context.Context, _ uuid.UUID, windowDays int) (DeliverabilityReport, error) {
	return DeliverabilityReport{WindowDays: windowDays, Items: []SiteDeliveryItem{}}, nil
}

func (r *fakeRepo) DeleteLogsOlderThan(_ context.Context, _ time.Time, _ int64) (int64, error) {
	return 0, nil
}

// Phase 4a stubs — suppression + webhook dedup + log actions.

func (r *fakeRepo) UpsertSuppression(_ context.Context, in UpsertSuppressionInput) (Suppression, error) {
	return Suppression{
		ID:       uuid.New(),
		TenantID: in.TenantID,
		SiteID:   in.SiteID,
		Email:    &in.Email,
		Reason:   in.Reason,
		Provider: in.Provider,
	}, nil
}

func (r *fakeRepo) UpsertSuppressionTenantTx(_ context.Context, in UpsertSuppressionInput) (Suppression, error) {
	return Suppression{
		ID:       uuid.New(),
		TenantID: in.TenantID,
		SiteID:   in.SiteID,
		Email:    &in.Email,
		Reason:   in.Reason,
		Provider: in.Provider,
	}, nil
}

func (r *fakeRepo) GetSuppression(_ context.Context, _, _ uuid.UUID) (Suppression, error) {
	return Suppression{}, ErrNotFound
}

func (r *fakeRepo) IsSuppressed(_ context.Context, _, _ uuid.UUID, _ string) (bool, error) {
	return false, nil
}

func (r *fakeRepo) ListSiteSuppression(_ context.Context, _, _ uuid.UUID, _ SuppressionFilter) (SuppressionPage, error) {
	return SuppressionPage{}, nil
}

func (r *fakeRepo) ListFleetSuppression(_ context.Context, _ uuid.UUID, _ SuppressionFilter) (SuppressionPage, error) {
	return SuppressionPage{}, nil
}

func (r *fakeRepo) DeleteSuppression(_ context.Context, _, _ uuid.UUID) error {
	return nil
}

func (r *fakeRepo) ListSuppressionDeltas(_ context.Context, _, _ uuid.UUID, _ string, _ int) (SuppressionDeltaPage, error) {
	return SuppressionDeltaPage{}, nil
}

func (r *fakeRepo) InsertWebhookEventDedup(_ context.Context, _ WebhookEventInput, _ *uuid.UUID) (bool, error) {
	return true, nil
}

func (r *fakeRepo) MarkEmailLogBounced(_ context.Context, _, _ uuid.UUID, _, _ string) error {
	return nil
}

func (r *fakeRepo) GetConfigByRouteTokenHash(_ context.Context, _ []byte) (Config, error) {
	return Config{}, ErrNotFound
}

func (r *fakeRepo) GetConfigByRouteTokenHashWithSecret(_ context.Context, _ []byte) (Config, []byte, error) {
	return Config{}, nil, ErrNotFound
}

func (r *fakeRepo) SetWebhookFields(_ context.Context, _, _ uuid.UUID, _, _ []byte, _ bool, _ []string) (Config, error) {
	return Config{}, nil
}

func (r *fakeRepo) PruneWebhookDedup(_ context.Context, _ time.Time) (int64, error) {
	return 0, nil
}

func (r *fakeRepo) GetEmailLogBodyStored(_ context.Context, _, _, _ uuid.UUID) (bool, error) {
	return false, ErrNotFound
}

func (r *fakeRepo) IncrEmailLogResentCount(_ context.Context, _, _, _ uuid.UUID) error {
	return nil
}

func (r *fakeRepo) DeleteEmailLogsBulk(_ context.Context, _, _ uuid.UUID, _ []uuid.UUID) (int64, error) {
	return 0, nil
}

// m62 stubs — required to satisfy the extended repository interface.

func (r *fakeRepo) ListConnections(_ context.Context, _, _ uuid.UUID) ([]Connection, error) {
	return nil, nil
}

func (r *fakeRepo) GetConnection(_ context.Context, _, _ uuid.UUID, _ string) (Connection, error) {
	return Connection{}, ErrNotFound
}

func (r *fakeRepo) UpsertConnection(_ context.Context, _ ConnectionUpsertInput, _ []byte, _ bool) (Connection, error) {
	return Connection{}, nil
}

func (r *fakeRepo) DeleteConnection(_ context.Context, _, _ uuid.UUID, _ string) error {
	return nil
}

func (r *fakeRepo) GetConnectionSecretCiphertexts(_ context.Context, _, _ uuid.UUID) ([]ConnectionSecretRow, error) {
	return nil, nil
}

func (r *fakeRepo) ListEmailInheritingSites(_ context.Context, _ uuid.UUID) ([]InheritingSite, error) {
	return nil, nil
}

func (r *fakeRepo) GetSiteRef(_ context.Context, _, _ uuid.UUID) (SiteRef, error) {
	return SiteRef{}, ErrNotFound
}

func (r *fakeRepo) GetNotifySettings(_ context.Context, _ uuid.UUID) (NotifySettings, error) {
	return NotifySettings{}, ErrNotFound
}

func (r *fakeRepo) UpsertNotifySettings(_ context.Context, in NotifySettings) (NotifySettings, error) {
	return in, nil
}

func (r *fakeRepo) AccumulateAlertFailures(_ context.Context, _, _ uuid.UUID, _ int64) error {
	return nil
}

func (r *fakeRepo) ClaimAlertSlot(_ context.Context, _, _ uuid.UUID, _ int64, _ int) (*AlertState, error) {
	return nil, nil // throttled
}

func (r *fakeRepo) ListDueDigests(_ context.Context, _ int32) ([]NotifySettings, error) {
	return nil, nil
}

func (r *fakeRepo) ClaimAdvanceDigest(_ context.Context, _ uuid.UUID, _ time.Time) (NotifySettings, error) {
	return NotifySettings{}, ErrNotFound
}

func (r *fakeRepo) GetFleetStatsBySite(_ context.Context, _ uuid.UUID, _, _ time.Time, _ int32) ([]SiteStatsRow, error) {
	return nil, nil
}

func (r *fakeRepo) TopFailureSamples(_ context.Context, _ uuid.UUID, _, _ time.Time, _ int32) ([]FailureSample, error) {
	return nil, nil
}

func (r *fakeRepo) TopFailureSamplesBySite(_ context.Context, _, _ uuid.UUID, _, _ time.Time, _ int32) ([]FailureSample, error) {
	return nil, nil
}

// ---------------------------------------------------------------------------
// tests
// ---------------------------------------------------------------------------

func TestService_GetConfig_OrgFallback(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()

	repo := newFakeRepo()
	// No per-site row; set an org-wide row.
	orgCfg := Config{
		ID:            uuid.New(),
		TenantID:      tenantID,
		Provider:      "sendgrid",
		LogEmails:     true,
		RetentionDays: 14,
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
	}
	repo.org[tenantID] = orgCfg

	svc := NewService(&Repo{}, &fakeEncryptor{}, nil)
	svc.repo = repo

	cfg, err := svc.GetConfig(context.Background(), tenantID, siteID)
	if err != nil {
		t.Fatalf("GetConfig: unexpected error: %v", err)
	}
	if cfg.Provider != "sendgrid" {
		t.Errorf("expected inherited org provider 'sendgrid', got %q", cfg.Provider)
	}
	// SiteID should be pointed at the queried site after inheritance.
	if cfg.SiteID == nil || *cfg.SiteID != siteID {
		t.Errorf("expected inherited config SiteID = %s, got %v", siteID, cfg.SiteID)
	}
}

func TestService_GetConfig_NotFound(t *testing.T) {
	repo := newFakeRepo()
	svc := NewService(&Repo{}, &fakeEncryptor{}, nil)
	svc.repo = repo

	_, err := svc.GetConfig(context.Background(), uuid.New(), uuid.New())
	if err == nil {
		t.Fatal("expected error when neither per-site nor org config exists")
	}
}

func TestService_UpsertSiteConfig_SecretEncrypted(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()
	secret := "super_secret_key"

	repo := newFakeRepo()
	enc := &fakeEncryptor{}
	svc := NewService(&Repo{}, enc, nil)
	svc.repo = repo

	sitePtr := &siteID
	in := UpsertInput{
		TenantID:      tenantID,
		SiteID:        sitePtr,
		Provider:      "sendgrid",
		SecretRaw:     &secret,
		LogEmails:     true,
		RetentionDays: 14,
		Config:        map[string]any{},
		Mappings:      map[string]any{},
	}
	saved, err := svc.UpsertSiteConfig(context.Background(), in)
	if err != nil {
		t.Fatalf("UpsertSiteConfig: unexpected error: %v", err)
	}
	if !saved.SecretSet {
		t.Error("expected SecretSet=true after providing a secret")
	}
	// The stored ciphertext must NOT be the plaintext.
	if string(repo.storedCt) == secret {
		t.Error("plaintext secret was stored — encryption did not run")
	}
	// Verify the fake ciphertext decrypts back to the original.
	plain, err := enc.Decrypt(repo.storedCt)
	if err != nil {
		t.Fatalf("decrypt stored ciphertext: %v", err)
	}
	if string(plain) != secret {
		t.Errorf("decrypt round-trip failed: got %q, want %q", string(plain), secret)
	}
}

func TestService_UpsertSiteConfig_NilSentinelPreservesSecret(t *testing.T) {
	// When SecretRaw is nil, SetSecret must be false in the repo call so the
	// existing ciphertext is preserved (the nil-sentinel SQL pattern).
	tenantID := uuid.New()
	siteID := uuid.New()

	repo := newFakeRepo()
	svc := NewService(&Repo{}, &fakeEncryptor{}, nil)
	svc.repo = repo

	sitePtr := &siteID
	// First upsert: set the secret.
	secret := "initial_key"
	_, err := svc.UpsertSiteConfig(context.Background(), UpsertInput{
		TenantID: tenantID, SiteID: sitePtr, Provider: "mailgun",
		SecretRaw: &secret, LogEmails: true, RetentionDays: 14,
		Config: map[string]any{}, Mappings: map[string]any{},
	})
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	// Second upsert: change only FromAddress, do NOT supply a secret.
	_, err = svc.UpsertSiteConfig(context.Background(), UpsertInput{
		TenantID: tenantID, SiteID: sitePtr, Provider: "mailgun",
		FromAddress: "new@example.com",
		// SecretRaw is nil — must preserve existing ciphertext.
		LogEmails: true, RetentionDays: 14,
		Config: map[string]any{}, Mappings: map[string]any{},
	})
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	// SetSecret must be false on the second call (nil-sentinel).
	if repo.storedSetSecret {
		t.Error("expected SetSecret=false when SecretRaw is nil (nil-sentinel must not overwrite existing ciphertext)")
	}
}

func TestService_UpsertSiteConfig_AgeGuard(t *testing.T) {
	// With no encryptor wired, providing a secret must return ServiceUnavailable.
	tenantID := uuid.New()
	siteID := uuid.New()
	secret := "key"

	svc := NewService(&Repo{}, nil /* no enc */, nil)
	svc.repo = newFakeRepo()

	sitePtr := &siteID
	_, err := svc.UpsertSiteConfig(context.Background(), UpsertInput{
		TenantID: tenantID, SiteID: sitePtr, Provider: "smtp",
		SecretRaw: &secret, LogEmails: true, RetentionDays: 14,
		Config: map[string]any{}, Mappings: map[string]any{},
	})
	if err == nil {
		t.Fatal("expected error when encryptor is nil and secret provided")
	}
	// Must be domain.KindServiceUnavailable.
	var domErr interface{ Error() string }
	if !errors.As(err, &domErr) {
		t.Errorf("expected a typed domain error, got %T: %v", err, err)
	}
	// Check that it is ServiceUnavailable (code: email_crypto_unwired).
	if !containsCode(err, "email_crypto_unwired") {
		t.Errorf("expected error code 'email_crypto_unwired', got: %v", err)
	}
}

func TestService_InvalidProvider(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()

	svc := NewService(&Repo{}, &fakeEncryptor{}, nil)
	svc.repo = newFakeRepo()

	sitePtr := &siteID
	_, err := svc.UpsertSiteConfig(context.Background(), UpsertInput{
		TenantID: tenantID, SiteID: sitePtr, Provider: "nonexistent_provider",
		LogEmails: true, RetentionDays: 14,
		Config: map[string]any{}, Mappings: map[string]any{},
	})
	if err == nil {
		t.Fatal("expected validation error for unknown provider")
	}
	if !containsCode(err, "email_invalid_provider") {
		t.Errorf("expected code 'email_invalid_provider', got: %v", err)
	}
}

func TestService_RLSTenantIsolation(t *testing.T) {
	// Two tenants must not be able to read each other's config through the service.
	// The DB-level RLS enforcement is tested in the real DB integration test
	// (internal/authz/rls_isolation_test.go pattern). Here we verify that the
	// service correctly returns ErrNotFound when no row exists for a tenant.
	tenantA := uuid.New()
	tenantB := uuid.New()
	siteID := uuid.New()

	repo := newFakeRepo()
	svc := NewService(&Repo{}, &fakeEncryptor{}, nil)
	svc.repo = repo

	// Store a config for tenant A.
	secret := "key"
	sitePtr := &siteID
	_, _ = svc.UpsertSiteConfig(context.Background(), UpsertInput{
		TenantID: tenantA, SiteID: sitePtr, Provider: "smtp",
		SecretRaw: &secret, LogEmails: true, RetentionDays: 14,
		Config: map[string]any{}, Mappings: map[string]any{},
	})

	// Tenant B querying the same site ID must get NotFound.
	_, err := svc.GetConfig(context.Background(), tenantB, siteID)
	if err == nil {
		t.Fatal("expected error when tenant B reads tenant A's config")
	}
}

// ---------------------------------------------------------------------------
// fakeAgentClient — captures SyncEmailConfig / SendTestEmail calls
// ---------------------------------------------------------------------------

type fakeAgentClient struct {
	syncCalled  int
	syncLastReq agentcmd.EmailConfigRequest
	syncErr     error

	sendTestCalled int
	sendTestErr    error
}

func (f *fakeAgentClient) SyncEmailConfig(_ context.Context, _ uuid.UUID, _ string, req agentcmd.EmailConfigRequest) (agentcmd.EmailConfigResult, error) {
	f.syncCalled++
	f.syncLastReq = req
	return agentcmd.EmailConfigResult{OK: true}, f.syncErr
}

func (f *fakeAgentClient) SendTestEmail(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.SendTestEmailRequest) (agentcmd.SendTestEmailResult, error) {
	f.sendTestCalled++
	if f.sendTestErr != nil {
		return agentcmd.SendTestEmailResult{}, f.sendTestErr
	}
	return agentcmd.SendTestEmailResult{OK: true, Detail: "sent"}, nil
}

func (f *fakeAgentClient) ResendEmail(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.ResendEmailRequest) (agentcmd.ResendEmailResult, error) {
	return agentcmd.ResendEmailResult{OK: true}, nil
}

// fakeSiteLookup always resolves to "https://example.com".
type fakeSiteLookup struct {
	urlErr error
}

func (f *fakeSiteLookup) GetSiteURL(_ context.Context, _, _ uuid.UUID) (string, error) {
	if f.urlErr != nil {
		return "", f.urlErr
	}
	return "https://example.com", nil
}

// ---------------------------------------------------------------------------
// UpsertSiteConfig agent-sync tests
// ---------------------------------------------------------------------------

func TestService_UpsertSiteConfig_DispatchesSyncEmailConfig(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()
	secret := "plaintext_secret"

	repo := newFakeRepo()
	enc := &fakeEncryptor{}
	agent := &fakeAgentClient{}
	look := &fakeSiteLookup{}

	svc := NewService(&Repo{}, enc, nil)
	svc.repo = repo
	svc.SetAgentClient(agent, look)

	sitePtr := &siteID
	_, err := svc.UpsertSiteConfig(context.Background(), UpsertInput{
		TenantID:      tenantID,
		SiteID:        sitePtr,
		Provider:      "smtp",
		SecretRaw:     &secret,
		LogEmails:     true,
		RetentionDays: 14,
		Config:        map[string]any{"host": "smtp.example.com"},
		Mappings:      map[string]any{},
	})
	if err != nil {
		t.Fatalf("UpsertSiteConfig: unexpected error: %v", err)
	}
	if agent.syncCalled != 1 {
		t.Errorf("expected SyncEmailConfig to be called once, called %d times", agent.syncCalled)
	}
	if agent.syncLastReq.Provider != "smtp" {
		t.Errorf("expected provider 'smtp' in sync req, got %q", agent.syncLastReq.Provider)
	}
	if agent.syncLastReq.Secret == nil || *agent.syncLastReq.Secret != secret {
		t.Errorf("expected decrypted secret %q in sync req, got %v", secret, agent.syncLastReq.Secret)
	}
}

func TestService_UpsertSiteConfig_AgentSyncFailureDoesNotFailSave(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()

	repo := newFakeRepo()
	enc := &fakeEncryptor{}
	agent := &fakeAgentClient{syncErr: errors.New("agent unreachable")}
	look := &fakeSiteLookup{}

	svc := NewService(&Repo{}, enc, nil)
	svc.repo = repo
	svc.SetAgentClient(agent, look)

	sitePtr := &siteID
	saved, err := svc.UpsertSiteConfig(context.Background(), UpsertInput{
		TenantID:      tenantID,
		SiteID:        sitePtr,
		Provider:      "sendgrid",
		LogEmails:     true,
		RetentionDays: 14,
		Config:        map[string]any{},
		Mappings:      map[string]any{},
	})
	// The save must succeed even though the agent sync failed.
	if err != nil {
		t.Fatalf("UpsertSiteConfig must succeed even when agent is offline, got: %v", err)
	}
	if saved.Provider != "sendgrid" {
		t.Errorf("expected saved config provider 'sendgrid', got %q", saved.Provider)
	}
	// The sync was attempted (called once).
	if agent.syncCalled != 1 {
		t.Errorf("expected SyncEmailConfig to be called once, called %d times", agent.syncCalled)
	}
}

func TestService_UpsertSiteConfig_NoAgentNilGuard(t *testing.T) {
	// When the agent client is not wired the save must still succeed without panic.
	tenantID := uuid.New()
	siteID := uuid.New()

	svc := NewService(&Repo{}, &fakeEncryptor{}, nil)
	svc.repo = newFakeRepo()
	// No SetAgentClient call.

	sitePtr := &siteID
	saved, err := svc.UpsertSiteConfig(context.Background(), UpsertInput{
		TenantID:      tenantID,
		SiteID:        sitePtr,
		Provider:      "mailgun",
		LogEmails:     true,
		RetentionDays: 14,
		Config:        map[string]any{},
		Mappings:      map[string]any{},
	})
	if err != nil {
		t.Fatalf("UpsertSiteConfig without agent: unexpected error: %v", err)
	}
	if saved.Provider != "mailgun" {
		t.Errorf("expected provider 'mailgun', got %q", saved.Provider)
	}
}

// ---------------------------------------------------------------------------
// SendTest pre-sync test
// ---------------------------------------------------------------------------

func TestService_SendTest_CallsSyncBeforeSend(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()

	repo := newFakeRepo()
	enc := &fakeEncryptor{}
	agent := &fakeAgentClient{}
	look := &fakeSiteLookup{}

	// Pre-populate a config row so GetConfig resolves.
	repo.site[siteKey(tenantID, siteID)] = Config{
		ID:            uuid.New(),
		TenantID:      tenantID,
		SiteID:        &siteID,
		Provider:      "smtp",
		LogEmails:     true,
		RetentionDays: 14,
		Config:        map[string]any{},
	}

	svc := NewService(&Repo{}, enc, nil)
	svc.repo = repo
	svc.SetAgentClient(agent, look)

	result, err := svc.SendTest(context.Background(), tenantID, siteID, TestSendInput{
		To: "test@example.com",
	})
	if err != nil {
		t.Fatalf("SendTest: unexpected error: %v", err)
	}
	if !result.OK {
		t.Errorf("expected ok=true, got ok=false: %s", result.Detail)
	}
	if agent.syncCalled != 1 {
		t.Errorf("expected SyncEmailConfig called once before SendTestEmail, called %d times", agent.syncCalled)
	}
	if agent.sendTestCalled != 1 {
		t.Errorf("expected SendTestEmail called once, called %d times", agent.sendTestCalled)
	}
}

func TestService_SendTest_SyncFailureReturnsClearError(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()

	repo := newFakeRepo()
	enc := &fakeEncryptor{}
	agent := &fakeAgentClient{syncErr: errors.New("timeout")}
	look := &fakeSiteLookup{}

	repo.site[siteKey(tenantID, siteID)] = Config{
		ID:            uuid.New(),
		TenantID:      tenantID,
		SiteID:        &siteID,
		Provider:      "ses",
		LogEmails:     true,
		RetentionDays: 14,
		Config:        map[string]any{},
	}

	svc := NewService(&Repo{}, enc, nil)
	svc.repo = repo
	svc.SetAgentClient(agent, look)

	result, err := svc.SendTest(context.Background(), tenantID, siteID, TestSendInput{
		To: "test@example.com",
	})
	if err != nil {
		t.Fatalf("SendTest sync failure must not return a domain error, got: %v", err)
	}
	if result.OK {
		t.Error("expected ok=false when sync fails")
	}
	if result.Detail == "" {
		t.Error("expected non-empty detail when sync fails")
	}
	// SendTestEmail must NOT be called when sync fails (agent has stale config).
	if agent.sendTestCalled != 0 {
		t.Errorf("SendTestEmail must not be called when sync fails, called %d times", agent.sendTestCalled)
	}
}

// ---------------------------------------------------------------------------
// GH #380 — a config push must never carry an unresolved secret as an empty one
// ---------------------------------------------------------------------------

// A site that inherits the org email config acquires its own config row with a
// NULL secret the first time anything is saved on its email page. That row must
// keep using the ORG credential, not push an empty secret to the agent.
func TestService_UpsertSiteConfig_RowWithoutOwnSecretPushesOrgSecret(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()

	repo := newFakeRepo()
	repo.org[tenantID] = Config{TenantID: tenantID, Provider: "smtp", SecretSet: true}

	agent := &fakeAgentClient{}
	svc := NewService(&Repo{}, &fakeEncryptor{}, nil)
	svc.repo = repo
	svc.SetAgentClient(agent, &fakeSiteLookup{})

	_, err := svc.UpsertSiteConfig(context.Background(), UpsertInput{
		TenantID:      tenantID,
		SiteID:        &siteID,
		Provider:      "smtp",
		LogEmails:     true,
		RetentionDays: 14,
		Config:        map[string]any{"host": "smtp.example.com"},
		Mappings:      map[string]any{},
		// SecretRaw is nil: the operator saved the page without retyping the
		// password, which is the normal case.
	})
	if err != nil {
		t.Fatalf("UpsertSiteConfig: unexpected error: %v", err)
	}
	if agent.syncCalled != 1 {
		t.Fatalf("expected one sync, got %d", agent.syncCalled)
	}
	got := agent.syncLastReq.Secret
	if got == nil {
		t.Fatal("expected the org secret to be pushed for a site row that has none of its own, got nil")
	}
	if *got != "stored_secret" {
		t.Errorf("expected the org secret %q, got %q", "stored_secret", *got)
	}
}

// A stored ciphertext that will not decrypt must resolve to "no secret to
// push". Sending a non-nil empty string is what deleted working credentials.
func TestService_ResolvePushSecret_DecryptFailureYieldsNilNotEmptyString(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()

	repo := newFakeRepo()
	repo.corruptCt = true
	cfg := Config{TenantID: tenantID, SiteID: &siteID, Provider: "smtp", SecretSet: true}
	repo.site[siteKey(tenantID, siteID)] = cfg

	svc := NewService(&Repo{}, &fakeEncryptor{}, nil)
	svc.repo = repo

	secret, decryptFailed := svc.resolvePushSecret(context.Background(), tenantID, siteID, cfg)
	if !decryptFailed {
		t.Error("expected the decrypt failure to be reported, not swallowed")
	}
	if secret != nil {
		t.Errorf("expected a nil secret so the push omits the field, got %q", *secret)
	}
}

// Pressing "Send test" used to pre-sync an empty secret and then report the
// authentication failure it had just caused. It must report the real problem
// and touch the agent not at all.
func TestService_SendTest_UndecryptableSecretNeitherSyncsNorTests(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()

	repo := newFakeRepo()
	repo.corruptCt = true
	repo.site[siteKey(tenantID, siteID)] = Config{
		TenantID: tenantID, SiteID: &siteID, Provider: "smtp", SecretSet: true,
	}

	agent := &fakeAgentClient{}
	svc := NewService(&Repo{}, &fakeEncryptor{}, nil)
	svc.repo = repo
	svc.SetAgentClient(agent, &fakeSiteLookup{})

	result, err := svc.SendTest(context.Background(), tenantID, siteID, TestSendInput{To: "someone@example.com"})
	if err != nil {
		t.Fatalf("SendTest: unexpected error: %v", err)
	}
	if result.OK {
		t.Error("expected ok=false when the stored credential cannot be decrypted")
	}
	if result.Detail != secretDecryptFailedDetail {
		t.Errorf("expected the actionable detail, got %q", result.Detail)
	}
	if agent.syncCalled != 0 {
		t.Errorf("sync_email_config must not be dispatched, called %d times", agent.syncCalled)
	}
	if agent.sendTestCalled != 0 {
		t.Errorf("send_test_email must not be dispatched, called %d times", agent.sendTestCalled)
	}
}

// GetConfig's org fallback rewrites SiteID to the queried site, which makes an
// inherited row look like a per-site one. The flag is the only thing that says
// the credential belongs to the org.
func TestService_GetConfig_OrgFallbackIsMarkedInherited(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()

	repo := newFakeRepo()
	repo.org[tenantID] = Config{TenantID: tenantID, Provider: "smtp", SecretSet: true}

	svc := NewService(&Repo{}, &fakeEncryptor{}, nil)
	svc.repo = repo

	cfg, err := svc.GetConfig(context.Background(), tenantID, siteID)
	if err != nil {
		t.Fatalf("GetConfig: unexpected error: %v", err)
	}
	if !cfg.Inherited {
		t.Error("expected the org fallback to be marked inherited")
	}

	repo.site[siteKey(tenantID, siteID)] = Config{TenantID: tenantID, SiteID: &siteID, Provider: "smtp"}
	own, err := svc.GetConfig(context.Background(), tenantID, siteID)
	if err != nil {
		t.Fatalf("GetConfig: unexpected error: %v", err)
	}
	if own.Inherited {
		t.Error("a site's own config row must not be marked inherited")
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// containsCode checks whether the error chain contains a domain.Error with the
// given Code field.
func containsCode(err error, code string) bool {
	var de *domain.Error
	if errors.As(err, &de) {
		return de.Code == code
	}
	return false
}
