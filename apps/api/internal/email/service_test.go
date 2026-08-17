package email

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

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
	// sitePlain / orgPlain are the plaintexts the two secret ciphertexts
	// decrypt to. They differ so a test can tell WHICH credential a push
	// carried, which is the whole point of the org-fallback tests.
	sitePlain string
	orgPlain  string
	// inheriting is what ListEmailInheritingSites returns: sites with no
	// config row of their own, which is who org propagation pushes to.
	inheriting []InheritingSite
	// secretReadErr makes the ciphertext reads fail, simulating a database
	// that will not answer rather than one that answers "no secret".
	secretReadErr error
	// siteGetErr makes GetSiteConfig fail outright (not ErrNotFound), which is
	// the "cannot tell a correction from a move to another account" case on the
	// save-time credential rebind check.
	siteGetErr error

	// m62 named connections, keyed by configID+connection_key. connCiphertext
	// holds what the provider_secret_encrypted column stores, keyed by
	// connection key, so a test can seed a credential that will not decrypt.
	conns          map[string]Connection
	connCiphertext map[string][]byte
	// connGetErr makes the read of a stored connection fail, which is the
	// "cannot tell a correction from a move" case.
	connGetErr error
	// What UpsertConnection was last asked to write, and how often.
	connUpserts        int
	lastConnSetSecret  bool
	lastConnCiphertext []byte
	// Which config rows SetWebhookFields was asked to write, in order.
	webhookWrites []uuid.UUID
	// suppressionDeleteErr is what DeleteSuppression returns. The repo can now
	// say "refused" and "absent" as well as "done", and the service has to turn
	// each into a different answer (see suppression_delete_refusal_test.go).
	suppressionDeleteErr error

	// connectedSiteFacts is what ListConnectedSiteEmailCoverage returns, keyed
	// by tenant so a cross-tenant test can seed two tenants' fleets
	// independently (GH #381). Each fact carries both the agent_version and
	// whether the site routes its mail through WPMgr.
	connectedSiteFacts map[uuid.UUID][]ConnectedSiteEmailFact
	// connectedSiteFactsErr, when set, makes ListConnectedSiteEmailCoverage
	// fail — PR #447 bot review finding 2: this must degrade GetNotifySettings
	// to settings-without-coverage, never fail the whole GET.
	connectedSiteFactsErr error

	// GH #381 phase 5 — maybeAlertFailures exit-path control knobs. Each is
	// nil/zero by default, which preserves the pre-phase-5 fake behaviour
	// (GetNotifySettings -> ErrNotFound, ClaimAlertSlot -> throttled, GetSiteRef
	// -> ErrNotFound) so every existing test keeps passing unchanged.
	alertAccumulateErr error
	alertSettings      *NotifySettings
	alertSettingsErr   error
	alertClaimState    *AlertState
	alertClaimErr      error
	alertSiteRef       *SiteRef
	alertSiteRefErr    error
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		site:           make(map[string]Config),
		org:            make(map[uuid.UUID]Config),
		conns:          make(map[string]Connection),
		connCiphertext: make(map[string][]byte),
		sitePlain:      "stored_secret",
		orgPlain:       "org_secret",
	}
}

func siteKey(tenantID, siteID uuid.UUID) string {
	return tenantID.String() + "/" + siteID.String()
}

func (r *fakeRepo) GetSiteConfig(_ context.Context, tenantID, siteID uuid.UUID) (Config, error) {
	// siteGetErr simulates a database that will not answer, as distinct from
	// one that answers "no row". The rebind check must refuse the write in that
	// case rather than guess (see credential_rebind_test.go).
	if r.siteGetErr != nil {
		return Config{}, r.siteGetErr
	}
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
	if r.secretReadErr != nil {
		return nil, r.secretReadErr
	}
	if cfg, ok := r.site[siteKey(tenantID, siteID)]; ok && cfg.SecretSet {
		if r.corruptCt {
			return []byte("undecryptable"), nil
		}
		b, _ := (&fakeEncryptor{}).Encrypt([]byte(r.sitePlain))
		return b, nil
	}
	return nil, nil
}

func (r *fakeRepo) GetOrgSecretCiphertext(_ context.Context, tenantID uuid.UUID) ([]byte, error) {
	if cfg, ok := r.org[tenantID]; ok && cfg.SecretSet {
		if r.corruptCt {
			return []byte("undecryptable"), nil
		}
		b, _ := (&fakeEncryptor{}).Encrypt([]byte(r.orgPlain))
		return b, nil
	}
	return nil, nil
}

func (r *fakeRepo) UpsertSiteConfig(_ context.Context, in upsertRepoInput) (Config, error) {
	r.storedSetSecret = in.SetSecret
	r.storedCt = in.SecretCiphertext
	// Mirror the nil-sentinel in site_email.sql: SetSecret=false preserves the
	// stored ciphertext, so a row that already had a secret still reports one.
	secretSet := in.SetSecret && len(in.SecretCiphertext) > 0
	if !in.SetSecret && in.SiteID != nil {
		if prev, ok := r.site[siteKey(in.TenantID, *in.SiteID)]; ok {
			secretSet = prev.SecretSet
		}
	}
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
		SecretSet:      secretSet,
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
	return r.suppressionDeleteErr
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

func (r *fakeRepo) SetWebhookFields(_ context.Context, _, configID uuid.UUID, _, _ []byte, _ bool, _ []string) (Config, error) {
	r.webhookWrites = append(r.webhookWrites, configID)
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

// m62 — named-connection registry.

func connRowKey(configID uuid.UUID, key string) string {
	return configID.String() + "/" + key
}

func (r *fakeRepo) ListConnections(_ context.Context, _, configID uuid.UUID) ([]Connection, error) {
	var out []Connection
	for _, c := range r.conns {
		if c.ConfigID == configID {
			out = append(out, c)
		}
	}
	return out, nil
}

func (r *fakeRepo) GetConnection(_ context.Context, _, configID uuid.UUID, key string) (Connection, error) {
	if r.connGetErr != nil {
		return Connection{}, r.connGetErr
	}
	if c, ok := r.conns[connRowKey(configID, key)]; ok {
		return c, nil
	}
	return Connection{}, ErrNotFound
}

func (r *fakeRepo) UpsertConnection(_ context.Context, in ConnectionUpsertInput, ct []byte, setSecret bool) (Connection, error) {
	r.connUpserts++
	r.lastConnSetSecret = setSecret
	r.lastConnCiphertext = ct
	prev := r.conns[connRowKey(in.ConfigID, in.ConnectionKey)]
	// Mirror UpsertEmailConnection: set_secret=false preserves the stored
	// ciphertext, set_secret=true writes exactly what was handed over (NULL
	// included).
	stored := prev.SecretSet
	if setSecret {
		stored = len(ct) > 0
		r.connCiphertext[in.ConnectionKey] = ct
	}
	c := Connection{
		ID:            uuid.New(),
		TenantID:      in.TenantID,
		ConfigID:      in.ConfigID,
		ConnectionKey: in.ConnectionKey,
		Provider:      in.Provider,
		FromAddress:   in.FromAddress,
		FromName:      in.FromName,
		Config:        in.Config,
		SecretSet:     stored,
	}
	r.conns[connRowKey(in.ConfigID, in.ConnectionKey)] = c
	return c, nil
}

func (r *fakeRepo) DeleteConnection(_ context.Context, _, configID uuid.UUID, key string) error {
	delete(r.conns, connRowKey(configID, key))
	return nil
}

func (r *fakeRepo) GetConnectionSecretCiphertexts(_ context.Context, _, configID uuid.UUID) ([]ConnectionSecretRow, error) {
	var out []ConnectionSecretRow
	for _, c := range r.conns {
		if c.ConfigID != configID {
			continue
		}
		out = append(out, ConnectionSecretRow{
			ConnectionKey:           c.ConnectionKey,
			ProviderSecretEncrypted: r.connCiphertext[c.ConnectionKey],
		})
	}
	return out, nil
}

// addConnection seeds a stored connection plus the ciphertext of its credential.
// plain == "" seeds a connection with no credential at all.
func (r *fakeRepo) addConnection(c Connection, plain string) {
	if plain != "" {
		ct, _ := (&fakeEncryptor{}).Encrypt([]byte(plain))
		r.connCiphertext[c.ConnectionKey] = ct
		c.SecretSet = true
	}
	r.conns[connRowKey(c.ConfigID, c.ConnectionKey)] = c
}

func (r *fakeRepo) ListEmailInheritingSites(_ context.Context, _ uuid.UUID) ([]InheritingSite, error) {
	return r.inheriting, nil
}

func (r *fakeRepo) GetSiteRef(_ context.Context, _, _ uuid.UUID) (SiteRef, error) {
	if r.alertSiteRefErr != nil {
		return SiteRef{}, r.alertSiteRefErr
	}
	if r.alertSiteRef != nil {
		return *r.alertSiteRef, nil
	}
	return SiteRef{}, ErrNotFound
}

func (r *fakeRepo) GetNotifySettings(_ context.Context, _ uuid.UUID) (NotifySettings, error) {
	if r.alertSettingsErr != nil {
		return NotifySettings{}, r.alertSettingsErr
	}
	if r.alertSettings != nil {
		return *r.alertSettings, nil
	}
	return NotifySettings{}, ErrNotFound
}

func (r *fakeRepo) UpsertNotifySettings(_ context.Context, in NotifySettings) (NotifySettings, error) {
	return in, nil
}

func (r *fakeRepo) AccumulateAlertFailures(_ context.Context, _, _ uuid.UUID, _ int64) error {
	return r.alertAccumulateErr
}

// ClaimAlertSlot mirrors the real Repo's transactional-outbox contract: when
// a claim is configured to succeed (alertClaimState set) it invokes onClaim
// (the mailer EnqueueTx call), and an onClaim error is propagated as this
// call's error — exactly like a rolled-back transaction — instead of a
// claimed state ever being returned alongside a failed send.
func (r *fakeRepo) ClaimAlertSlot(_ context.Context, _, _ uuid.UUID, _ int64, _ int, onClaim func(tx pgx.Tx) error) (*AlertState, error) {
	if r.alertClaimErr != nil {
		return nil, r.alertClaimErr
	}
	if r.alertClaimState == nil {
		return nil, nil // nil state, nil error == throttled
	}
	if onClaim != nil {
		if err := onClaim(nil); err != nil {
			return nil, err // rolled back
		}
	}
	return r.alertClaimState, nil
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

func (r *fakeRepo) ListConnectedSiteEmailCoverage(_ context.Context, tenantID uuid.UUID) ([]ConnectedSiteEmailFact, error) {
	if r.connectedSiteFactsErr != nil {
		return nil, r.connectedSiteFactsErr
	}
	return r.connectedSiteFacts[tenantID], nil
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

// A stored ciphertext that will not decrypt must resolve to "say nothing", and
// must report the failure. Sending an empty string is what deleted working
// credentials; sending some other credential would hide the failure.
func TestService_ResolveSitePushSecret_DecryptFailureSaysNothingAndReports(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()

	repo := newFakeRepo()
	repo.corruptCt = true
	repo.org[tenantID] = Config{TenantID: tenantID, Provider: "smtp", Config: map[string]any{}, SecretSet: true}
	cfg := Config{TenantID: tenantID, SiteID: &siteID, Provider: "smtp", Config: map[string]any{}, SecretSet: true}
	repo.site[siteKey(tenantID, siteID)] = cfg

	svc := NewService(&Repo{}, &fakeEncryptor{}, nil)
	svc.repo = repo

	push, decryptFailed := svc.resolveSitePushSecret(context.Background(), tenantID, siteID, cfg, nil)
	if !decryptFailed {
		t.Error("expected the decrypt failure to be reported, not swallowed")
	}
	if push.plain != nil {
		t.Errorf("expected no secret so the push omits the field, got %q", *push.plain)
	}
	if push.clear {
		t.Error("a decrypt failure must not be turned into a revoke: the credential may still be fine on the site")
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
// GH #380 — the org credential must never be paired with a site-supplied endpoint
// ---------------------------------------------------------------------------

// THE ATTACK. PermEmailManage is not an org-level permission, so a site-scoped
// collaborator reaches PUT /sites/:id/email/config. They save a config for the
// one site they hold, naming a collector they control as the SMTP host and
// supplying no secret. The org credential must not travel to it, and the site
// must not be left able to use one it already holds against the new host.
func TestService_UpsertSiteConfig_SiteSuppliedHostNeverReceivesOrgSecret(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()

	repo := newFakeRepo()
	repo.orgPlain = "SG.org-sendgrid-api-key"
	repo.org[tenantID] = Config{
		TenantID:  tenantID,
		Provider:  "smtp",
		Config:    map[string]any{"host": "smtp.org-relay.example", "port": float64(587), "username": "org@example.com", "encryption": "tls"},
		SecretSet: true,
	}

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
		Config: map[string]any{
			"host":       "collector.attacker.example",
			"port":       float64(587),
			"username":   "org@example.com",
			"encryption": "tls",
		},
		Mappings: map[string]any{},
		// No secret: the whole point is to make the CP supply one.
	})
	if err != nil {
		t.Fatalf("UpsertSiteConfig: unexpected error: %v", err)
	}
	if agent.syncCalled != 1 {
		t.Fatalf("expected one sync, got %d", agent.syncCalled)
	}
	if got := agent.syncLastReq.Secret; got != nil {
		t.Fatalf("the org credential reached a site-supplied endpoint: secret %q pushed with host %v",
			*got, agent.syncLastReq.Config["host"])
	}
	if !agent.syncLastReq.ClearSecret {
		t.Error("a push to an endpoint the stored credential was not issued for must clear it, " +
			"otherwise the site keeps authenticating to the new host with the old password")
	}
}

// The same attack through the test-send door, which is the one that actually
// makes the agent perform AUTH LOGIN against the attacker's collector.
func TestService_SendTest_SiteSuppliedHostNeverReceivesOrgSecret(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()

	repo := newFakeRepo()
	repo.orgPlain = "SG.org-sendgrid-api-key"
	repo.org[tenantID] = Config{
		TenantID:  tenantID,
		Provider:  "smtp",
		Config:    map[string]any{"host": "smtp.org-relay.example", "port": float64(587)},
		SecretSet: true,
	}
	// The site row the attacker just saved: their host, no secret of its own.
	repo.site[siteKey(tenantID, siteID)] = Config{
		TenantID: tenantID,
		SiteID:   &siteID,
		Provider: "smtp",
		Config:   map[string]any{"host": "collector.attacker.example", "port": float64(587)},
	}

	agent := &fakeAgentClient{}
	svc := NewService(&Repo{}, &fakeEncryptor{}, nil)
	svc.repo = repo
	svc.SetAgentClient(agent, &fakeSiteLookup{})

	if _, err := svc.SendTest(context.Background(), tenantID, siteID, TestSendInput{To: "drop@attacker.example"}); err != nil {
		t.Fatalf("SendTest: unexpected error: %v", err)
	}
	if got := agent.syncLastReq.Secret; got != nil {
		t.Fatalf("the org credential reached a site-supplied endpoint: secret %q pushed with host %v",
			*got, agent.syncLastReq.Config["host"])
	}
	if !agent.syncLastReq.ClearSecret {
		t.Error("the pre-test sync must clear the stored credential rather than let the agent " +
			"AUTH LOGIN to the attacker's host with it")
	}
}

// The legitimate inheritance case the GH #380 fix exists for: the operator
// opens a site that inherits the org config, changes nothing about the
// endpoint, and saves. The site now has a row of its own with no secret, and
// the org credential is exactly the right one to keep using.
func TestService_UpsertSiteConfig_UnchangedInheritedEndpointKeepsOrgSecret(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()

	orgConfig := map[string]any{
		"host": "smtp.org-relay.example", "port": float64(587),
		"username": "org@example.com", "encryption": "tls", "auth": true,
	}

	repo := newFakeRepo()
	repo.orgPlain = "org-relay-password"
	repo.org[tenantID] = Config{TenantID: tenantID, Provider: "smtp", Config: orgConfig, SecretSet: true}

	agent := &fakeAgentClient{}
	svc := NewService(&Repo{}, &fakeEncryptor{}, nil)
	svc.repo = repo
	svc.SetAgentClient(agent, &fakeSiteLookup{})

	// The page round-trips the inherited config back, plus a per-site toggle.
	siteConfig := map[string]any{}
	for k, v := range orgConfig {
		siteConfig[k] = v
	}
	_, err := svc.UpsertSiteConfig(context.Background(), UpsertInput{
		TenantID:      tenantID,
		SiteID:        &siteID,
		Provider:      "smtp",
		LogEmails:     true,
		StoreBody:     true,
		RetentionDays: 30,
		Config:        siteConfig,
		Mappings:      map[string]any{},
	})
	if err != nil {
		t.Fatalf("UpsertSiteConfig: unexpected error: %v", err)
	}
	got := agent.syncLastReq.Secret
	if got == nil {
		t.Fatal("a site row that only mirrors the org endpoint must keep using the org credential, got nil")
	}
	if *got != "org-relay-password" {
		t.Errorf("expected the org credential, got %q", *got)
	}
	if agent.syncLastReq.ClearSecret {
		t.Error("clear_secret must not be set when a credential is being pushed")
	}
}

// Switching a site to a different provider is a different credential audience
// even when no endpoint field changed, so the org credential must not follow.
func TestService_UpsertSiteConfig_ProviderSwitchDoesNotInheritOrgSecret(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()

	repo := newFakeRepo()
	repo.orgPlain = "SG.org-sendgrid-api-key"
	repo.org[tenantID] = Config{TenantID: tenantID, Provider: "sendgrid", Config: map[string]any{}, SecretSet: true}

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
		Config:        map[string]any{"host": "smtp.somewhere.example"},
		Mappings:      map[string]any{},
	})
	if err != nil {
		t.Fatalf("UpsertSiteConfig: unexpected error: %v", err)
	}
	if got := agent.syncLastReq.Secret; got != nil {
		t.Fatalf("a sendgrid org key must not be pushed to an smtp site config, got %q", *got)
	}
	if !agent.syncLastReq.ClearSecret {
		t.Error("a provider switch must revoke the credential the old provider was using")
	}
}

// An explicit clear from the API must reach the agent as clear_secret and must
// null the stored column, not store the ciphertext of an empty string.
func TestService_UpsertSiteConfig_ExplicitClearEmitsClearSecret(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()

	repo := newFakeRepo()
	repo.org[tenantID] = Config{TenantID: tenantID, Provider: "smtp", Config: map[string]any{}, SecretSet: true}

	agent := &fakeAgentClient{}
	svc := NewService(&Repo{}, &fakeEncryptor{}, nil)
	svc.repo = repo
	svc.SetAgentClient(agent, &fakeSiteLookup{})

	empty := ""
	saved, err := svc.UpsertSiteConfig(context.Background(), UpsertInput{
		TenantID:      tenantID,
		SiteID:        &siteID,
		Provider:      "smtp",
		LogEmails:     true,
		RetentionDays: 14,
		Config:        map[string]any{},
		Mappings:      map[string]any{},
		SecretRaw:     &empty,
	})
	if err != nil {
		t.Fatalf("UpsertSiteConfig: unexpected error: %v", err)
	}
	if !agent.syncLastReq.ClearSecret {
		t.Error("an explicit clear must reach the agent as clear_secret:true")
	}
	if agent.syncLastReq.Secret != nil {
		t.Errorf("an explicit clear must not also carry a secret, got %q", *agent.syncLastReq.Secret)
	}
	if !repo.storedSetSecret {
		t.Error("an explicit clear must write the secret column, not preserve it")
	}
	if repo.storedCt != nil {
		t.Error("an explicit clear must null the secret column, not store the ciphertext of an empty string")
	}
	if saved.SecretSet {
		t.Error("secret_set must be false after an explicit clear")
	}
}

// A site whose own stored credential decrypts to an empty string has an
// explicit clear at rest. It must never fall through to the org credential.
func TestService_ResolveSitePushSecret_EmptyStoredSecretDoesNotFallThroughToOrg(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()

	repo := newFakeRepo()
	repo.sitePlain = ""
	repo.orgPlain = "SG.org-sendgrid-api-key"
	repo.org[tenantID] = Config{TenantID: tenantID, Provider: "smtp", Config: map[string]any{}, SecretSet: true}
	cfg := Config{TenantID: tenantID, SiteID: &siteID, Provider: "smtp", Config: map[string]any{}, SecretSet: true}
	repo.site[siteKey(tenantID, siteID)] = cfg

	svc := NewService(&Repo{}, &fakeEncryptor{}, nil)
	svc.repo = repo

	push, decryptFailed := svc.resolveSitePushSecret(context.Background(), tenantID, siteID, cfg, nil)
	if decryptFailed {
		t.Fatal("an empty stored secret is not a decrypt failure")
	}
	if push.plain != nil {
		t.Fatalf("expected no credential, got %q", *push.plain)
	}
	if !push.clear {
		t.Error("an empty stored secret is an explicit clear at rest")
	}
}

// Revoking is destructive, so it may only follow from knowing there is no
// credential. A database that would not answer is not an answer.
func TestService_ResolveSitePushSecret_UnreadableSecretIsNotTreatedAsAbsent(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()

	repo := newFakeRepo()
	repo.secretReadErr = errors.New("connection reset by peer")
	cfg := Config{TenantID: tenantID, SiteID: &siteID, Provider: "smtp", Config: map[string]any{}, SecretSet: true}
	repo.site[siteKey(tenantID, siteID)] = cfg

	svc := NewService(&Repo{}, &fakeEncryptor{}, nil)
	svc.repo = repo

	push, decryptFailed := svc.resolveSitePushSecret(context.Background(), tenantID, siteID, cfg, nil)
	if decryptFailed {
		t.Error("a read failure is not a decrypt failure")
	}
	if push.clear {
		t.Error("a database error must never revoke a working credential")
	}
	if push.plain != nil {
		t.Errorf("expected the push to say nothing, got %q", *push.plain)
	}
}

// PropagateOrgConfig must not push a config it could not resolve the org
// credential for: every agent in the field before this fix reads an omitted
// secret the way it read an empty one.
func TestService_PropagateOrgConfig_AbortsWhenOrgSecretWillNotDecrypt(t *testing.T) {
	tenantID := uuid.New()

	repo := newFakeRepo()
	repo.corruptCt = true
	repo.org[tenantID] = Config{TenantID: tenantID, Provider: "smtp", Config: map[string]any{}, SecretSet: true}

	agent := &fakeAgentClient{}
	svc := NewService(&Repo{}, &fakeEncryptor{}, nil)
	svc.repo = repo
	svc.SetAgentClient(agent, &fakeSiteLookup{})

	res, err := svc.PropagateOrgConfig(context.Background(), tenantID)
	if err == nil {
		t.Error("expected the undecryptable org credential to be reported, not swallowed")
	}
	if res.Synced != 0 {
		t.Errorf("expected no site to be pushed, got %d", res.Synced)
	}
	if agent.syncCalled != 0 {
		t.Errorf("no config may be pushed without a resolvable credential, sync called %d times", agent.syncCalled)
	}
}

// Clearing the org credential must revoke it from the sites it was pushed to.
// Inheriting sites have no config row of their own, so any credential they hold
// came from an org push and nothing else can take it back.
func TestService_PropagateOrgConfig_ClearedOrgSecretRevokesFromInheritingSites(t *testing.T) {
	tenantID := uuid.New()

	repo := newFakeRepo()
	repo.org[tenantID] = Config{TenantID: tenantID, Provider: "smtp", Config: map[string]any{}, SecretSet: false}
	repo.inheriting = []InheritingSite{{ID: uuid.New(), URL: "https://a.example"}}

	agent := &fakeAgentClient{}
	svc := NewService(&Repo{}, &fakeEncryptor{}, nil)
	svc.repo = repo
	svc.SetAgentClient(agent, &fakeSiteLookup{})

	if _, err := svc.PropagateOrgConfig(context.Background(), tenantID); err != nil {
		t.Fatalf("PropagateOrgConfig: unexpected error: %v", err)
	}
	if agent.syncCalled != 1 {
		t.Fatalf("expected one push, got %d", agent.syncCalled)
	}
	if !agent.syncLastReq.ClearSecret {
		t.Error("an org config with no secret must revoke the credential it previously pushed")
	}
	if agent.syncLastReq.Secret != nil {
		t.Errorf("expected no secret on the wire, got %q", *agent.syncLastReq.Secret)
	}
}

// UpsertSiteConfig must not push either: the save still succeeds, the agent is
// left alone.
func TestService_UpsertSiteConfig_UndecryptableSecretSkipsThePush(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()

	repo := newFakeRepo()
	repo.corruptCt = true
	repo.site[siteKey(tenantID, siteID)] = Config{
		TenantID: tenantID, SiteID: &siteID, Provider: "smtp", Config: map[string]any{}, SecretSet: true,
	}

	agent := &fakeAgentClient{}
	svc := NewService(&Repo{}, &fakeEncryptor{}, nil)
	svc.repo = repo
	svc.SetAgentClient(agent, &fakeSiteLookup{})

	if _, err := svc.UpsertSiteConfig(context.Background(), UpsertInput{
		TenantID:      tenantID,
		SiteID:        &siteID,
		Provider:      "smtp",
		LogEmails:     true,
		RetentionDays: 14,
		Config:        map[string]any{},
		Mappings:      map[string]any{},
	}); err != nil {
		t.Fatalf("UpsertSiteConfig: the save must still succeed: %v", err)
	}
	if agent.syncCalled != 0 {
		t.Errorf("a config with no resolvable credential must not be pushed, sync called %d times", agent.syncCalled)
	}
}

// ---------------------------------------------------------------------------
// The named-connection registry: the same credential rules, per entry (GH #380)
// ---------------------------------------------------------------------------

// THE ATTACK, through the connection registry. The stored connection credential
// is preserved by UpsertEmailConnection whenever a request carries no secret, so
// rewriting the host of an existing connection and sending nothing re-points
// that credential at the new host on the next push. This is the top-level
// escalation with a connection key on it.
func TestService_UpsertConnection_MovedEndpointRevokesThePreservedCredential(t *testing.T) {
	tenantID := uuid.New()
	configID := uuid.New()

	repo := newFakeRepo()
	repo.addConnection(Connection{
		TenantID:      tenantID,
		ConfigID:      configID,
		ConnectionKey: "billing",
		Provider:      "smtp",
		Config: map[string]any{
			"host": "smtp.org-relay.example", "port": float64(587),
			"username": "billing@example.com", "encryption": "tls",
		},
	}, "org-relay-password")

	svc := NewService(&Repo{}, &fakeEncryptor{}, nil)
	svc.repo = repo

	conn, err := svc.UpsertConnection(context.Background(), ConnectionUpsertInput{
		TenantID:      tenantID,
		ConfigID:      configID,
		ConnectionKey: "billing",
		Provider:      "smtp",
		Config: map[string]any{
			"host": "collector.attacker.example", "port": float64(587),
			"username": "billing@example.com", "encryption": "tls",
		},
		// No secret: the whole point is to make the control plane supply one.
	})
	if err != nil {
		t.Fatalf("UpsertConnection: unexpected error: %v", err)
	}
	if !repo.lastConnSetSecret || len(repo.lastConnCiphertext) != 0 {
		t.Fatalf("a connection pointed at a new endpoint must have its stored credential revoked, "+
			"got set_secret=%v ciphertext_len=%d", repo.lastConnSetSecret, len(repo.lastConnCiphertext))
	}
	if conn.SecretSet {
		t.Error("the saved connection still reports a credential that was revoked")
	}
}

// The legitimate edit: same account, different label. Nothing about the
// credential moves, so nothing may be destroyed either. This is the assertion
// that stops the guard above being written as "any change clears".
func TestService_UpsertConnection_UnchangedAudienceKeepsTheStoredCredential(t *testing.T) {
	tenantID := uuid.New()
	configID := uuid.New()
	cfg := map[string]any{
		"host": "smtp.org-relay.example", "port": float64(587),
		"username": "billing@example.com", "encryption": "tls", "auth": true,
	}

	repo := newFakeRepo()
	repo.addConnection(Connection{
		TenantID: tenantID, ConfigID: configID, ConnectionKey: "billing",
		Provider: "smtp", Config: cfg,
	}, "org-relay-password")

	svc := NewService(&Repo{}, &fakeEncryptor{}, nil)
	svc.repo = repo

	same := map[string]any{}
	for k, v := range cfg {
		same[k] = v
	}
	same["auto_tls"] = true // a setting that cannot redirect a credential

	conn, err := svc.UpsertConnection(context.Background(), ConnectionUpsertInput{
		TenantID: tenantID, ConfigID: configID, ConnectionKey: "billing",
		Provider: "smtp", Config: same, FromName: "Billing",
	})
	if err != nil {
		t.Fatalf("UpsertConnection: unexpected error: %v", err)
	}
	if repo.lastConnSetSecret {
		t.Error("re-saving the same account must preserve the stored credential, not rewrite the column")
	}
	if !conn.SecretSet {
		t.Error("the connection lost its credential on an edit that moved nothing")
	}
}

// Emptying the field is an explicit revoke, and it has to work on an instance
// whose encryption key is not wired: writing a NULL needs no key, and refusing
// to revoke because of a missing key leaves the credential live on the site.
func TestService_UpsertConnection_ExplicitClearNullsTheColumnWithoutAnEncryptor(t *testing.T) {
	tenantID := uuid.New()
	configID := uuid.New()

	repo := newFakeRepo()
	repo.addConnection(Connection{
		TenantID: tenantID, ConfigID: configID, ConnectionKey: "billing",
		Provider: "smtp", Config: map[string]any{"host": "smtp.example"},
	}, "org-relay-password")

	svc := NewService(&Repo{}, nil, nil) // no encryptor
	svc.repo = repo

	empty := ""
	conn, err := svc.UpsertConnection(context.Background(), ConnectionUpsertInput{
		TenantID: tenantID, ConfigID: configID, ConnectionKey: "billing",
		Provider: "smtp", Config: map[string]any{"host": "smtp.example"},
		SecretRaw: &empty,
	})
	if err != nil {
		t.Fatalf("an explicit revoke must not need an encryptor: %v", err)
	}
	if !repo.lastConnSetSecret || len(repo.lastConnCiphertext) != 0 {
		t.Fatalf("an explicit clear must NULL the column, got set_secret=%v ciphertext_len=%d",
			repo.lastConnSetSecret, len(repo.lastConnCiphertext))
	}
	if conn.SecretSet {
		t.Error("a cleared connection still reports secret_set=true")
	}
}

// Unreadable is not absent, and it is not unchanged either. If the previous
// settings cannot be read there is no way to tell a correction from a move, so
// the write is refused rather than guessed at in either direction.
func TestService_UpsertConnection_UnreadablePreviousSettingsRefuseTheWrite(t *testing.T) {
	repo := newFakeRepo()
	repo.connGetErr = errors.New("connection reset by peer")

	svc := NewService(&Repo{}, &fakeEncryptor{}, nil)
	svc.repo = repo

	_, err := svc.UpsertConnection(context.Background(), ConnectionUpsertInput{
		TenantID: uuid.New(), ConfigID: uuid.New(), ConnectionKey: "billing",
		Provider: "smtp", Config: map[string]any{"host": "smtp.example"},
	})
	if err == nil {
		t.Fatal("expected a refusal when the stored connection cannot be read")
	}
	if !containsCode(err, "email_get_connection") {
		t.Errorf("expected email_get_connection, got %v", err)
	}
	if repo.connUpserts != 0 {
		t.Errorf("nothing may be written when the previous settings are unknown, wrote %d times", repo.connUpserts)
	}
}

// A brand new connection has no stored credential to rebind, so the audience
// check must not turn a first save into an error or a pointless revoke.
func TestService_UpsertConnection_NewConnectionIsNotTreatedAsARebind(t *testing.T) {
	repo := newFakeRepo()
	svc := NewService(&Repo{}, &fakeEncryptor{}, nil)
	svc.repo = repo

	if _, err := svc.UpsertConnection(context.Background(), ConnectionUpsertInput{
		TenantID: uuid.New(), ConfigID: uuid.New(), ConnectionKey: "billing",
		Provider: "smtp", Config: map[string]any{"host": "smtp.example"},
	}); err != nil {
		t.Fatalf("UpsertConnection: unexpected error: %v", err)
	}
	if repo.lastConnSetSecret {
		t.Error("a first save must say nothing about a credential that does not exist yet")
	}
}

// The wire half of the same contract: what a push says about each connection.
func TestService_BuildAgentConfigReq_ConnectionSecretsSpeakTheThreeStates(t *testing.T) {
	tenantID := uuid.New()
	configID := uuid.New()

	repo := newFakeRepo()
	repo.addConnection(Connection{
		TenantID: tenantID, ConfigID: configID, ConnectionKey: "live",
		Provider: "smtp", Config: map[string]any{"host": "smtp.example"},
	}, "live-password")
	// Revoked in the control plane: the column is NULL. Before this fix the push
	// merely omitted the secret, which the agent reads as "keep what you have",
	// so a connection credential could never be revoked at all.
	repo.addConnection(Connection{
		TenantID: tenantID, ConfigID: configID, ConnectionKey: "revoked",
		Provider: "smtp", Config: map[string]any{"host": "smtp.example"},
	}, "")
	// Stored but undecryptable: the encryption key changed under it. That says
	// nothing about whether the site should still have a credential.
	repo.addConnection(Connection{
		TenantID: tenantID, ConfigID: configID, ConnectionKey: "unreadable",
		Provider: "smtp", Config: map[string]any{"host": "smtp.example"}, SecretSet: true,
	}, "")
	repo.connCiphertext["unreadable"] = []byte("undecryptable")

	svc := NewService(&Repo{}, &fakeEncryptor{}, nil)
	svc.repo = repo

	req := svc.buildAgentConfigReq(Config{ID: configID, TenantID: tenantID, Provider: "smtp"}, pushSecret{})

	live, ok := req.Connections["live"]
	if !ok || live.Secret == nil || *live.Secret != "live-password" {
		t.Fatalf("a stored connection credential must be pushed, got %+v", live)
	}
	if live.ClearSecret {
		t.Error("a connection with a credential must not also ask for it to be cleared")
	}
	revoked := req.Connections["revoked"]
	if !revoked.ClearSecret || revoked.Secret != nil {
		t.Errorf("a connection with no stored credential must ask the site to drop the one it holds, got %+v", revoked)
	}
	unreadable := req.Connections["unreadable"]
	if unreadable.ClearSecret || unreadable.Secret != nil {
		t.Errorf("an unreadable credential must say nothing rather than revoke, got %+v", unreadable)
	}
}

// A provider added to the catalog and forgotten here used to fall to the
// default, come back with no audience fields, and compare as the same audience
// as any config of that provider, which is the loosest possible answer on the
// one path that lends out the organisation credential.
func TestCredentialAudienceFieldsCoverTheCatalog(t *testing.T) {
	for _, p := range Catalog {
		if _, known := credentialAudienceFields(p.Slug); !known {
			t.Errorf("provider %q is in the catalog but has no entry in credentialAudienceFields; "+
				"add the fields that decide where its credential goes (an empty list is a deliberate answer)", p.Slug)
		}
	}
	if _, known := credentialAudienceFields("a_provider_that_does_not_exist"); known {
		t.Error("an unknown provider must not be reported as a known audience")
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
