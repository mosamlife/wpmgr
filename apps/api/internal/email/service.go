package email

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// Encryptor age-encrypts and decrypts provider secrets. *cryptbox.AgeIdentity
// satisfies it. Declared as an interface so the service is unit-testable with a
// fake, and so the age-guard can be checked without importing cryptbox.
type Encryptor interface {
	Encrypt(plaintext []byte) ([]byte, error)
	Decrypt(ciphertext []byte) ([]byte, error)
}

// AgentEmailClient is the CP->agent command surface for email operations.
// *agentcmd.Client satisfies it. Declared as an interface for testability.
type AgentEmailClient interface {
	SyncEmailConfig(ctx context.Context, siteID uuid.UUID, siteURL string, req agentcmd.EmailConfigRequest) (agentcmd.EmailConfigResult, error)
	SendTestEmail(ctx context.Context, siteID uuid.UUID, siteURL string, req agentcmd.SendTestEmailRequest) (agentcmd.SendTestEmailResult, error)
	// ResendEmail is the Phase 4b agent command for resending a stored email.
	// Phase 4a: the client stub returns ok=false until the agent implements it.
	ResendEmail(ctx context.Context, siteID uuid.UUID, siteURL string, req agentcmd.ResendEmailRequest) (agentcmd.ResendEmailResult, error)
}

// SiteLookup resolves a site's agent URL. The perf package's pattern.
type SiteLookup interface {
	GetSiteURL(ctx context.Context, tenantID, siteID uuid.UUID) (string, error)
}

// repository is the persistence surface. *Repo satisfies it.
type repository interface {
	GetSiteConfig(ctx context.Context, tenantID, siteID uuid.UUID) (Config, error)
	GetOrgConfig(ctx context.Context, tenantID uuid.UUID) (Config, error)
	GetSecretCiphertext(ctx context.Context, tenantID, siteID uuid.UUID) ([]byte, error)
	GetOrgSecretCiphertext(ctx context.Context, tenantID uuid.UUID) ([]byte, error)
	UpsertSiteConfig(ctx context.Context, in upsertRepoInput) (Config, error)
	UpsertOrgConfig(ctx context.Context, in upsertRepoInput) (Config, error)
	ListSiteConfigs(ctx context.Context, tenantID uuid.UUID, limit, offset int32) ([]Config, error)
	// Phase 3 — email log
	IngestLogBatch(ctx context.Context, tenantID, siteID uuid.UUID, entries []IngestEntry) (int64, error)
	ListSiteLog(ctx context.Context, tenantID, siteID uuid.UUID, f LogListFilter) (LogListPage, error)
	GetLogEntry(ctx context.Context, tenantID, siteID, id uuid.UUID) (LogDetail, error)
	ListFleetLog(ctx context.Context, tenantID uuid.UUID, f LogListFilter) (LogListPage, error)
	GetSiteStats(ctx context.Context, tenantID, siteID uuid.UUID, from, to time.Time) (EmailStats, error)
	GetFleetStats(ctx context.Context, tenantID uuid.UUID, from, to time.Time) (EmailStats, error)
	GetFleetDelivery(ctx context.Context, tenantID uuid.UUID, windowDays int) (DeliverabilityReport, error)
	DeleteLogsOlderThan(ctx context.Context, cutoffTs time.Time, batchSize int64) (int64, error)
	// Phase 4a — suppression + webhook dedup + log actions
	UpsertSuppression(ctx context.Context, in UpsertSuppressionInput) (Suppression, error)
	UpsertSuppressionTenantTx(ctx context.Context, in UpsertSuppressionInput) (Suppression, error)
	GetSuppression(ctx context.Context, tenantID, id uuid.UUID) (Suppression, error)
	IsSuppressed(ctx context.Context, tenantID, siteID uuid.UUID, email string) (bool, error)
	ListSiteSuppression(ctx context.Context, tenantID, siteID uuid.UUID, f SuppressionFilter) (SuppressionPage, error)
	ListFleetSuppression(ctx context.Context, tenantID uuid.UUID, f SuppressionFilter) (SuppressionPage, error)
	DeleteSuppression(ctx context.Context, tenantID, id uuid.UUID) error
	ListSuppressionDeltas(ctx context.Context, tenantID, siteID uuid.UUID, sinceCursor string, limit int) (SuppressionDeltaPage, error)
	InsertWebhookEventDedup(ctx context.Context, in WebhookEventInput, suppressionID *uuid.UUID) (bool, error)
	MarkEmailLogBounced(ctx context.Context, tenantID, siteID uuid.UUID, messageID, status string) error
	// m61: webhook security.
	GetConfigByRouteTokenHash(ctx context.Context, tokenHash []byte) (Config, error)
	GetConfigByRouteTokenHashWithSecret(ctx context.Context, tokenHash []byte) (Config, []byte, error)
	SetWebhookFields(ctx context.Context, tenantID, configID uuid.UUID, tokenHash, signingKeyCT []byte, setSigningKey bool, sesTopicArns []string) (Config, error)
	PruneWebhookDedup(ctx context.Context, cutoffTs time.Time) (int64, error)
	GetEmailLogBodyStored(ctx context.Context, tenantID, siteID, id uuid.UUID) (bool, error)
	IncrEmailLogResentCount(ctx context.Context, tenantID, siteID, id uuid.UUID) error
	DeleteEmailLogsBulk(ctx context.Context, tenantID, siteID uuid.UUID, ids []uuid.UUID) (int64, error)
	// m62 Area 2 — multi-connection CRUD
	ListConnections(ctx context.Context, tenantID, configID uuid.UUID) ([]Connection, error)
	GetConnection(ctx context.Context, tenantID, configID uuid.UUID, key string) (Connection, error)
	UpsertConnection(ctx context.Context, in ConnectionUpsertInput, secretCiphertext []byte, setSecret bool) (Connection, error)
	DeleteConnection(ctx context.Context, tenantID, configID uuid.UUID, key string) error
	GetConnectionSecretCiphertexts(ctx context.Context, tenantID, configID uuid.UUID) ([]ConnectionSecretRow, error)
	// m62 Area 1 — org propagation
	ListEmailInheritingSites(ctx context.Context, tenantID uuid.UUID) ([]InheritingSite, error)
	GetSiteRef(ctx context.Context, tenantID, siteID uuid.UUID) (SiteRef, error)
	// m62 Area 4 — notify settings + alert state + digest
	GetNotifySettings(ctx context.Context, tenantID uuid.UUID) (NotifySettings, error)
	UpsertNotifySettings(ctx context.Context, in NotifySettings) (NotifySettings, error)
	AccumulateAlertFailures(ctx context.Context, tenantID, siteID uuid.UUID, n int64) error
	ClaimAlertSlot(ctx context.Context, tenantID, siteID uuid.UUID, minFailures int64, throttleMinutes int) (*AlertState, error)
	ListDueDigests(ctx context.Context, limit int32) ([]NotifySettings, error)
	ClaimAdvanceDigest(ctx context.Context, tenantID uuid.UUID, newNextAt time.Time) (NotifySettings, error)
	GetFleetStatsBySite(ctx context.Context, tenantID uuid.UUID, from, to time.Time, limit int32) ([]SiteStatsRow, error)
	TopFailureSamples(ctx context.Context, tenantID uuid.UUID, from, to time.Time, limit int32) ([]FailureSample, error)
	TopFailureSamplesBySite(ctx context.Context, tenantID, siteID uuid.UUID, from, to time.Time, limit int32) ([]FailureSample, error)
}

// Service is the email domain business-logic layer. It owns:
//   - age-guard (refuses writes when no encryptor is wired)
//   - age-encrypt on secret writes; decrypt only when building a push command
//   - org-wide default resolution (per-site row → org default → ErrNotFound)
//   - provider validation
//   - dispatching sync_email_config and send_test_email commands to the agent
//   - m62: multi-connection CRUD, org propagation, alerts, digest
type Service struct {
	repo     repository
	enc      Encryptor // nil when WPMGR_SITE_DEST_AGE_SECRET not configured
	agent    AgentEmailClient
	siteLook SiteLookup
	log      *slog.Logger
	// pub is the site-event bus used to emit email.suppression_updated and
	// email.bounce SSE events. May be nil (guarded before use).
	pub EventPublisher
	// m62: River enqueuer for background jobs.
	enqueuer Enqueuer
	// m62: instance mailer for alert/digest emails.
	mailer       MailerEnqueuer
	mailerStatus MailerStatus
	// m62: public base URL for constructing dashboard links in emails.
	publicBase string
	// m103 (GH #247): supplies the digest's "new vulnerabilities" section.
	// Nil is safe — buildDigestData skips the section entirely.
	vulnDigest VulnDigestSource
}

// NewService builds the email service. enc may be nil (all secret-write paths
// return ServiceUnavailable("email_crypto_unwired")); agent may be nil (command
// dispatch paths return graceful errors until Phase 2).
func NewService(repo *Repo, enc Encryptor, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{repo: repo, enc: enc, log: log}
}

// SetAgentClient wires the CP->agent command client + site-URL resolver.
func (s *Service) SetAgentClient(agent AgentEmailClient, siteLook SiteLookup) {
	s.agent = agent
	s.siteLook = siteLook
}

// SetPublisher wires the SSE event publisher. Called from main.go after the
// publisher is constructed. A nil publisher is always safe (emits are skipped).
func (s *Service) SetPublisher(pub EventPublisher) {
	s.pub = pub
}

// SetEnqueuer wires the River job enqueuer for background propagation jobs.
func (s *Service) SetEnqueuer(eq Enqueuer) {
	s.enqueuer = eq
}

// SetMailer wires the instance mailer enqueuer for alert/digest emails.
func (s *Service) SetMailer(m MailerEnqueuer) {
	s.mailer = m
}

// SetMailerStatus wires the instance mailer status checker.
func (s *Service) SetMailerStatus(ms MailerStatus) {
	s.mailerStatus = ms
}

// SetPublicBase sets the public base URL used to construct dashboard links in
// notification emails (e.g. "https://manage.wpmgr.app"). Called from main.go.
func (s *Service) SetPublicBase(base string) {
	s.publicBase = base
}

// SetVulnDigestSource wires the vulnerability-digest data source (m103, GH
// #247). Called from main.go once both the uptime and vuln services exist.
func (s *Service) SetVulnDigestSource(v VulnDigestSource) {
	s.vulnDigest = v
}

// ---------------------------------------------------------------------------
// GetConfig — per-site config with org-wide fallback resolution
// ---------------------------------------------------------------------------

// GetConfig returns the resolved config for a site. If no per-site row exists it
// falls back to the org-wide default. Returns domain.NotFound when neither exists.
func (s *Service) GetConfig(ctx context.Context, tenantID, siteID uuid.UUID) (Config, error) {
	cfg, err := s.repo.GetSiteConfig(ctx, tenantID, siteID)
	if err == nil {
		return cfg, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Config{}, domain.Internal("email_get_config", "failed to load site email config").WithCause(err)
	}

	// Fall back to the org-wide default.
	orgCfg, err := s.repo.GetOrgConfig(ctx, tenantID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Config{}, domain.NotFound("email_config_not_found", "no email config for this site or org")
		}
		return Config{}, domain.Internal("email_get_org_config", "failed to load org email config").WithCause(err)
	}
	// Surface inherited config with the site's perspective (SiteID points to the
	// queried site so the frontend knows what was inherited). Inherited says so
	// explicitly, because the rewritten SiteID alone makes an org row
	// indistinguishable from a per-site one.
	orgCfg.SiteID = &siteID
	orgCfg.Inherited = true
	return orgCfg, nil
}

// GetOrgConfig returns the org-wide default config row.
func (s *Service) GetOrgConfig(ctx context.Context, tenantID uuid.UUID) (Config, error) {
	cfg, err := s.repo.GetOrgConfig(ctx, tenantID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Config{}, domain.NotFound("email_org_config_not_found", "no org-wide email config")
		}
		return Config{}, domain.Internal("email_get_org_config", "failed to load org email config").WithCause(err)
	}
	return cfg, nil
}

// ---------------------------------------------------------------------------
// UpsertConfig — per-site and org-wide
// ---------------------------------------------------------------------------

// UpsertSiteConfig creates or updates the per-site config. When in.SecretRaw is
// non-nil it age-encrypts it; nil preserves the existing stored ciphertext.
func (s *Service) UpsertSiteConfig(ctx context.Context, in UpsertInput) (Config, error) {
	if err := s.validateUpsert(in); err != nil {
		return Config{}, err
	}

	ri, err := s.buildRepoInput(ctx, in)
	if err != nil {
		return Config{}, err
	}

	saved, err := s.repo.UpsertSiteConfig(ctx, ri)
	if err != nil {
		return Config{}, domain.Internal("email_upsert_config", "failed to save email config").WithCause(err)
	}

	// Best-effort: push the decrypted config to the agent. A push failure is
	// non-fatal — the config is already saved. Log the warning and return the
	// saved config cleanly so the save always succeeds even when the agent is
	// offline.
	if s.agent != nil && s.siteLook != nil && in.SiteID != nil {
		siteURL, urlErr := s.siteLook.GetSiteURL(ctx, in.TenantID, *in.SiteID)
		if urlErr != nil {
			s.log.Warn("email: saved config but could not resolve site URL for agent sync",
				slog.String("site_id", in.SiteID.String()),
				slog.Any("error", urlErr),
			)
			return saved, nil
		}
		secret, secretErr := s.resolveEffectiveSecret(ctx, in, saved)
		if secretErr != nil {
			// The config is saved; the push is not attempted. Pushing a config
			// with no resolvable credential is exactly the shape that deleted
			// working passwords on agents in the field, whose published
			// versions read an omitted secret as a delete (GH #380).
			s.log.Error("email: saved config but the stored credential could not be decrypted; the agent push is skipped",
				slog.String("site_id", in.SiteID.String()),
				slog.Any("error", secretErr),
			)
			return saved, nil
		}
		req := s.buildAgentConfigReq(saved, secret)
		if _, syncErr := s.agent.SyncEmailConfig(ctx, *in.SiteID, siteURL, req); syncErr != nil {
			s.log.Warn("email: config stored but agent sync failed",
				slog.String("site_id", in.SiteID.String()),
				slog.Any("error", syncErr),
			)
			// Non-fatal: the save succeeded; return the saved config cleanly.
			return saved, nil
		}
	}

	return saved, nil
}

// UpsertOrgConfig creates or updates the org-wide default config.
func (s *Service) UpsertOrgConfig(ctx context.Context, in UpsertInput) (Config, error) {
	if in.SiteID != nil {
		return Config{}, domain.Validation("email_org_config_site_id", "org-wide config must have no site_id")
	}
	if err := s.validateUpsert(in); err != nil {
		return Config{}, err
	}

	ri, err := s.buildRepoInput(ctx, in)
	if err != nil {
		return Config{}, err
	}

	saved, err := s.repo.UpsertOrgConfig(ctx, ri)
	if err != nil {
		return Config{}, domain.Internal("email_upsert_org_config", "failed to save org email config").WithCause(err)
	}
	// m62 Area 1: enqueue propagation to inheriting sites. Best-effort —
	// a failure here does not roll back the config save.
	if s.enqueuer != nil {
		if eqErr := s.enqueuer.EnqueueOrgConfigPropagate(ctx, in.TenantID); eqErr != nil {
			s.log.Warn("email: org config saved but propagation enqueue failed",
				slog.String("tenant_id", in.TenantID.String()),
				slog.Any("error", eqErr),
			)
		}
	}
	return saved, nil
}

// ListSiteConfigs returns all per-site config rows for the tenant.
func (s *Service) ListSiteConfigs(ctx context.Context, tenantID uuid.UUID, limit, offset int32) ([]Config, error) {
	configs, err := s.repo.ListSiteConfigs(ctx, tenantID, limit, offset)
	if err != nil {
		return nil, domain.Internal("email_list_configs", "failed to list email configs").WithCause(err)
	}
	return configs, nil
}

// ---------------------------------------------------------------------------
// SendTest
// ---------------------------------------------------------------------------

// SendTest dispatches the send_test_email signed command to the site's agent.
// Phase 1: the agent does not yet implement this command and will return a
// "command not found" (404) response — that is expected until Phase 2. The
// route dispatches and surfaces the agent's response gracefully.
//
// TODO(phase2): the agent must implement send_test_email (see Phase 2 hooks
// section in the per-site-email plan).
func (s *Service) SendTest(ctx context.Context, tenantID, siteID uuid.UUID, in TestSendInput) (TestSendResult, error) {
	if s.agent == nil || s.siteLook == nil {
		// Agent not wired (signing key not configured). Surface gracefully.
		return TestSendResult{
			OK:     false,
			Detail: "agent command client not configured; test email cannot be dispatched",
		}, nil
	}

	siteURL, err := s.siteLook.GetSiteURL(ctx, tenantID, siteID)
	if err != nil {
		return TestSendResult{}, domain.NotFound("email_site_not_found", "site not found or not enrolled")
	}

	// Belt-and-suspenders: push the current email config to the agent before
	// sending so that a freshly saved config is always reflected, and so the
	// agent never hits "no email config — run sync_email_config first" on the
	// test path. Failures here surface as a clear TestSendResult rather than
	// the opaque downstream error from the agent.
	cfg, cfgErr := s.GetConfig(ctx, tenantID, siteID)
	if cfgErr != nil {
		return TestSendResult{OK: false, Detail: "could not load email config for agent sync: " + cfgErr.Error()}, nil
	}
	// Resolve the effective secret: per-site first, then the org secret for a
	// site that still shares the org's endpoint.
	syncSecret, decryptFailed := s.resolveSitePushSecret(ctx, tenantID, siteID, cfg, nil)
	if decryptFailed {
		// Report the real problem instead of pre-syncing a config the site
		// cannot authenticate with and then testing it (GH #380).
		return TestSendResult{OK: false, Detail: secretDecryptFailedDetail}, nil
	}
	syncReq := s.buildAgentConfigReq(cfg, syncSecret)
	if _, syncErr := s.agent.SyncEmailConfig(ctx, siteID, siteURL, syncReq); syncErr != nil {
		return TestSendResult{OK: false, Detail: "could not sync config to agent: " + syncErr.Error()}, nil
	}

	res, err := s.agent.SendTestEmail(ctx, siteID, siteURL, agentcmd.SendTestEmailRequest{
		To:      in.To,
		Subject: in.Subject,
		Body:    in.Body,
	})
	if err != nil {
		// Non-domain error from the agent (e.g. unknown command until Phase 2).
		// Surface as ok=false with the raw detail rather than a 5xx, matching
		// the perf/security pattern for non-fatal agent command failures.
		return TestSendResult{OK: false, Detail: err.Error()}, nil
	}
	// DELIBERATE: the agent's ok=false is carried through as this result's OK,
	// not raised as an error. Reporting whether the test send worked IS the whole
	// purpose of this call, and the handler shows OK plus Detail to the operator.
	return TestSendResult{OK: res.OK, Detail: res.Detail}, nil
}

// ---------------------------------------------------------------------------
// SyncConfigToAgent
// ---------------------------------------------------------------------------

// SyncConfigToAgent pushes the stored email config to the site's agent.
// This is the explicit "Sync to site" action — distinct from the implicit
// sync that runs on Save and the pre-sync that runs before SendTest.
//
// Errors from the agent command are returned as TestSendResult{OK:false}
// (non-fatal, graceful) so the handler always responds 200 and lets the
// frontend display the outcome. Domain errors (site not found, no config)
// are returned as TestSendResult{OK:false} for the same reason.
func (s *Service) SyncConfigToAgent(ctx context.Context, tenantID, siteID uuid.UUID) (TestSendResult, error) {
	if s.agent == nil || s.siteLook == nil {
		return TestSendResult{
			OK:     false,
			Detail: "agent command client not configured; cannot sync",
		}, nil
	}

	// Resolve effective config (per-site → org fallback).
	cfg, err := s.GetConfig(ctx, tenantID, siteID)
	if err != nil {
		// domain.NotFound is not a 5xx — surface as ok=false.
		return TestSendResult{OK: false, Detail: "no email config to sync"}, nil
	}

	// Resolve the effective decrypted secret: per-site first, then org fallback.
	// An operator pressed "Sync to site", so an unresolvable credential is
	// reported to them rather than returned as a success that quietly synced
	// everything except the one field that matters.
	secret, decryptFailed := s.resolveSitePushSecret(ctx, tenantID, siteID, cfg, nil)
	if decryptFailed {
		return TestSendResult{OK: false, Detail: secretDecryptFailedDetail}, nil
	}

	siteURL, urlErr := s.siteLook.GetSiteURL(ctx, tenantID, siteID)
	if urlErr != nil {
		return TestSendResult{}, domain.NotFound("email_site_not_found", "site not found or not enrolled")
	}

	req := s.buildAgentConfigReq(cfg, secret)
	if _, syncErr := s.agent.SyncEmailConfig(ctx, siteID, siteURL, req); syncErr != nil {
		return TestSendResult{OK: false, Detail: syncErr.Error()}, nil
	}
	return TestSendResult{OK: true, Detail: "email config synced to site agent"}, nil
}

// ---------------------------------------------------------------------------
// internal helpers
// ---------------------------------------------------------------------------

// validateUpsert validates the UpsertInput before any DB or crypto work.
func (s *Service) validateUpsert(in UpsertInput) error {
	if !ValidProviderSlug(in.Provider) {
		return domain.Validation("email_invalid_provider",
			"provider must be one of: smtp, ses, sendgrid, mailgun, postmark")
	}
	if in.RetentionDays < 1 || in.RetentionDays > 365 {
		return domain.Validation("email_invalid_retention",
			"retention_days must be between 1 and 365")
	}
	return nil
}

// buildRepoInput resolves the secret ciphertext and assembles the upsertRepoInput.
// Age-guard: if SecretRaw is non-nil and no encryptor is wired, it returns
// ServiceUnavailable to prevent a plaintext secret reaching the DB.
func (s *Service) buildRepoInput(ctx context.Context, in UpsertInput) (upsertRepoInput, error) {
	ri := upsertRepoInput{
		TenantID:           in.TenantID,
		SiteID:             in.SiteID,
		Provider:           in.Provider,
		FromAddress:        in.FromAddress,
		FromName:           in.FromName,
		ForceFromEmail:     in.ForceFromEmail,
		ForceFromName:      in.ForceFromName,
		ReturnPath:         in.ReturnPath,
		Config:             in.Config,
		Mappings:           in.Mappings,
		DefaultConnection:  in.DefaultConnection,
		FallbackConnection: in.FallbackConnection,
		LogEmails:          in.LogEmails,
		StoreBody:          in.StoreBody,
		RetentionDays:      in.RetentionDays,
	}

	switch {
	case in.SecretRaw == nil:
		// The nil-sentinel: SetSecret=false leaves the stored ciphertext alone.

	case *in.SecretRaw == "":
		// An explicit clear. Write the column and null it, so secret_set stops
		// claiming a credential exists (GH #380). Storing the ciphertext of an
		// empty string instead left the row reporting secret_set=true for a
		// secret that is not there. No encryptor is needed to store a NULL, so
		// this path is deliberately outside the age-guard: revoking a
		// credential must work even on an instance whose key went missing.
		ri.SetSecret = true
		ri.SecretCiphertext = nil

	default:
		// Age-guard: refuse to store a plaintext secret with no encryptor.
		if s.enc == nil {
			return upsertRepoInput{}, domain.ServiceUnavailable(
				"email_crypto_unwired",
				"secret encryption is not configured (WPMGR_SITE_DEST_AGE_SECRET missing); "+
					"save the config without the secret first, or configure the key and restart",
			)
		}
		ct, err := s.enc.Encrypt([]byte(*in.SecretRaw))
		if err != nil {
			return upsertRepoInput{}, domain.Internal("email_encrypt_secret", "failed to encrypt provider secret").WithCause(err)
		}
		ri.SetSecret = true
		ri.SecretCiphertext = ct
	}

	return ri, nil
}

// ---------------------------------------------------------------------------
// Phase 3 — Email log ingest + viewer
// ---------------------------------------------------------------------------

// IngestLogBatch accepts a batch of agent-pushed log entries and upserts them
// into site_email_log. The tenant_id and site_id come exclusively from the
// verified agent identity (never the request body). Returns the max agent_seq
// accepted so the agent can advance its high-water cursor.
//
// Batch size is capped at maxIngestBatch; larger batches are rejected.
// m62: after a successful ingest, maybeAlertFailures is called best-effort for
// any entries with status=failed.
func (s *Service) IngestLogBatch(ctx context.Context, tenantID, siteID uuid.UUID, entries []IngestEntry) (IngestResult, error) {
	if len(entries) == 0 {
		return IngestResult{}, nil
	}
	if len(entries) > maxIngestBatch {
		return IngestResult{}, domain.Validation("email_ingest_batch_too_large",
			"batch exceeds the maximum of 500 entries per request")
	}
	maxSeq, err := s.repo.IngestLogBatch(ctx, tenantID, siteID, entries)
	if err != nil {
		return IngestResult{}, domain.Internal("email_ingest_log", "failed to ingest email log batch").WithCause(err)
	}

	// m62: count failures in this batch and maybe trigger an alert.
	failureCount := 0
	for _, e := range entries {
		if e.Status == "failed" {
			failureCount++
		}
	}
	if failureCount > 0 {
		go s.maybeAlertFailures(context.Background(), tenantID, siteID, failureCount)
	}

	return IngestResult{AckedThrough: maxSeq}, nil
}

// ListSiteLog returns a keyset-paginated list of email log entries for a site.
// Body is never included in the list response — use GetLogEntry for detail.
func (s *Service) ListSiteLog(ctx context.Context, tenantID, siteID uuid.UUID, f LogListFilter) (LogListPage, error) {
	page, err := s.repo.ListSiteLog(ctx, tenantID, siteID, f)
	if err != nil {
		return LogListPage{}, domain.Internal("email_list_log", "failed to list email log").WithCause(err)
	}
	return page, nil
}

// GetLogEntry returns a single email log entry including body (if stored) plus
// prev/next navigation IDs.
func (s *Service) GetLogEntry(ctx context.Context, tenantID, siteID, id uuid.UUID) (LogDetail, error) {
	detail, err := s.repo.GetLogEntry(ctx, tenantID, siteID, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return LogDetail{}, domain.NotFound("email_log_not_found", "email log entry not found")
		}
		return LogDetail{}, domain.Internal("email_get_log", "failed to fetch email log entry").WithCause(err)
	}
	return detail, nil
}

// ListFleetLog returns a keyset-paginated cross-site email log list for a tenant.
func (s *Service) ListFleetLog(ctx context.Context, tenantID uuid.UUID, f LogListFilter) (LogListPage, error) {
	page, err := s.repo.ListFleetLog(ctx, tenantID, f)
	if err != nil {
		return LogListPage{}, domain.Internal("email_list_fleet_log", "failed to list fleet email log").WithCause(err)
	}
	return page, nil
}

// GetSiteStats returns summary + per-day + per-provider email stats for a site.
func (s *Service) GetSiteStats(ctx context.Context, tenantID, siteID uuid.UUID, from, to time.Time) (EmailStats, error) {
	stats, err := s.repo.GetSiteStats(ctx, tenantID, siteID, from, to)
	if err != nil {
		return EmailStats{}, domain.Internal("email_get_stats", "failed to get email stats").WithCause(err)
	}
	return stats, nil
}

// GetFleetStats returns fleet-wide email stats for a tenant.
func (s *Service) GetFleetStats(ctx context.Context, tenantID uuid.UUID, from, to time.Time) (EmailStats, error) {
	stats, err := s.repo.GetFleetStats(ctx, tenantID, from, to)
	if err != nil {
		return EmailStats{}, domain.Internal("email_get_fleet_stats", "failed to get fleet email stats").WithCause(err)
	}
	return stats, nil
}

// GetFleetDelivery returns per-site deliverability aggregates for GET /email/deliverability.
// windowDays is clamped to [1, 365]; defaults to 30 when zero.
func (s *Service) GetFleetDelivery(ctx context.Context, tenantID uuid.UUID, windowDays int) (DeliverabilityReport, error) {
	report, err := s.repo.GetFleetDelivery(ctx, tenantID, windowDays)
	if err != nil {
		return DeliverabilityReport{}, domain.Internal("email_get_fleet_delivery", "failed to get fleet deliverability report").WithCause(err)
	}
	return report, nil
}

// PruneOldLogs deletes one batch of expired email log rows across all tenants.
// Returns the number of rows deleted; the caller should loop until 0.
// Called by the EmailLogGCWorker periodic River job.
func (s *Service) PruneOldLogs(ctx context.Context, cutoffTs time.Time, batchSize int64) (int64, error) {
	deleted, err := s.repo.DeleteLogsOlderThan(ctx, cutoffTs, batchSize)
	if err != nil {
		s.log.Error("email log retention: prune failed", slog.String("err", err.Error()))
		return 0, err
	}
	return deleted, nil
}

// ---------------------------------------------------------------------------
// Phase 4a — Webhook fan-out + suppression
// ---------------------------------------------------------------------------

// HandleWebhookEvent is the central dispatch point for a verified webhook event.
// It:
//  1. Skips if the event type is not a suppression trigger.
//  2. Deduplicates via InsertWebhookEventDedup (ON CONFLICT DO NOTHING).
//  3. Upserts the suppression row for hard_bounce / complaint.
//  4. Marks the matching site_email_log row as bounced/complained.
//
// The (tenant_id, site_id) come from the event metadata injected by the agent.
// If metadata is absent both are nil and the suppression row is orphaned
// (logged with a warning; no cross-tenant guessing).
func (s *Service) HandleWebhookEvent(ctx context.Context, ev WebhookEventInput) error {
	if !isSuppressionEventType(ev.EventType) {
		return nil // not a suppression-triggering event; nothing to do
	}
	if ev.Email == "" {
		return nil
	}

	// Log a warning when tenant metadata is absent but continue — we still
	// write an orphaned dedup row for idempotency, and we suppress the email
	// if we can resolve tenant later.
	if ev.TenantID == nil {
		s.log.Warn("email webhook: no tenant metadata; suppression row will be orphaned",
			slog.String("provider", ev.Provider),
			slog.String("event_id", ev.ProviderEventID),
			slog.String("email", ev.Email),
		)
	}

	var suppressionID *uuid.UUID
	if ev.TenantID != nil {
		// Upsert the suppression row.
		sup, err := s.repo.UpsertSuppression(ctx, UpsertSuppressionInput{
			TenantID: *ev.TenantID,
			SiteID:   ev.SiteID,
			Email:    ev.Email,
			Reason:   ev.EventType, // hard_bounce | complaint
			Provider: ev.Provider,
			EventAt:  ptrNow(),
			// Store masked email (lower-cased) for display in the operator UI.
			// Full address is masked per PII policy (not the body content).
			StorePlaintext: true,
		})
		if err != nil {
			s.log.Error("email webhook: upsert suppression failed",
				slog.String("err", err.Error()),
				slog.String("email", ev.Email),
			)
			return domain.Internal("webhook_suppression_upsert", "failed to upsert suppression").WithCause(err)
		}
		suppressionID = &sup.ID

		// SSE: notify the dashboard that a suppression row was written.
		var displayEmail string
		if sup.Email != nil {
			displayEmail = maskEmail(*sup.Email)
		}
		publishSuppressionUpdated(ctx, s.pub, *ev.TenantID, ev.SiteID, displayEmail, sup.Reason)

		// Best-effort: mark the matching log entry bounced/complained.
		// m61 SHOULD-FIX #3: pass siteID so the update is site-scoped.
		if ev.ProviderEventID != "" && ev.SiteID != nil {
			logStatus := webhookEventToLogStatus(ev.EventType)
			if err := s.repo.MarkEmailLogBounced(ctx, *ev.TenantID, *ev.SiteID, ev.ProviderEventID, logStatus); err != nil {
				s.log.Warn("email webhook: mark log bounced failed",
					slog.String("err", err.Error()),
					slog.String("message_id", ev.ProviderEventID),
				)
				// Non-fatal — the suppression write succeeded.
			}

			// SSE: notify the dashboard that a log entry was flipped to bounced/complained.
			if *ev.SiteID != uuid.Nil {
				publishBounce(ctx, s.pub, *ev.TenantID, *ev.SiteID, ev.ProviderEventID, logStatus)
			}
		}
	}

	// Dedup sentinel write (always, even for orphaned events).
	inserted, err := s.repo.InsertWebhookEventDedup(ctx, ev, suppressionID)
	if err != nil {
		s.log.Warn("email webhook: dedup insert failed", slog.String("err", err.Error()))
		// Non-fatal — the suppression was already written.
	}
	if !inserted {
		s.log.Debug("email webhook: duplicate event dropped",
			slog.String("provider", ev.Provider),
			slog.String("event_id", ev.ProviderEventID),
		)
	}
	return nil
}

// AddSuppression adds a manual suppression entry for a site or fleet.
// reason must be "manual" or "unsubscribe"; hard_bounce and complaint come from webhooks.
func (s *Service) AddSuppression(ctx context.Context, in UpsertSuppressionInput) (Suppression, error) {
	if in.Reason == "" {
		in.Reason = "manual"
	}
	if in.Reason != "manual" && in.Reason != "unsubscribe" {
		return Suppression{}, domain.Validation("suppression_reason_invalid",
			"manual suppression reason must be 'manual' or 'unsubscribe'")
	}
	if in.Email == "" {
		return Suppression{}, domain.Validation("suppression_email_required", "email is required")
	}
	sup, err := s.repo.UpsertSuppressionTenantTx(ctx, UpsertSuppressionInput{
		TenantID:       in.TenantID,
		SiteID:         in.SiteID,
		Email:          in.Email,
		Reason:         in.Reason,
		Provider:       "manual",
		StorePlaintext: true,
	})
	if err != nil {
		return Suppression{}, domain.Internal("suppression_add", "failed to add suppression").WithCause(err)
	}
	return sup, nil
}

// ListSiteSuppression returns a paginated suppression list for a site.
func (s *Service) ListSiteSuppression(ctx context.Context, tenantID, siteID uuid.UUID, f SuppressionFilter) (SuppressionPage, error) {
	page, err := s.repo.ListSiteSuppression(ctx, tenantID, siteID, f)
	if err != nil {
		return SuppressionPage{}, domain.Internal("suppression_list", "failed to list suppression").WithCause(err)
	}
	return page, nil
}

// ListFleetSuppression returns a paginated fleet-scope suppression list.
func (s *Service) ListFleetSuppression(ctx context.Context, tenantID uuid.UUID, f SuppressionFilter) (SuppressionPage, error) {
	page, err := s.repo.ListFleetSuppression(ctx, tenantID, f)
	if err != nil {
		return SuppressionPage{}, domain.Internal("suppression_list_fleet", "failed to list fleet suppression").WithCause(err)
	}
	return page, nil
}

// DeleteSuppression removes a suppression entry by id.
func (s *Service) DeleteSuppression(ctx context.Context, tenantID, id uuid.UUID) error {
	if err := s.repo.DeleteSuppression(ctx, tenantID, id); err != nil {
		return domain.Internal("suppression_delete", "failed to delete suppression").WithCause(err)
	}
	return nil
}

// ListSuppressionDeltas returns suppression entries created after the cursor
// for the agent suppression-fetch endpoint.
func (s *Service) ListSuppressionDeltas(ctx context.Context, tenantID, siteID uuid.UUID, sinceCursor string) (SuppressionDeltaPage, error) {
	page, err := s.repo.ListSuppressionDeltas(ctx, tenantID, siteID, sinceCursor, 500)
	if err != nil {
		return SuppressionDeltaPage{}, domain.Internal("suppression_deltas", "failed to list suppression deltas").WithCause(err)
	}
	return page, nil
}

// PruneWebhookDedup deletes webhook dedup rows older than the cutoff.
// Called by the GC worker.
func (s *Service) PruneWebhookDedup(ctx context.Context, cutoffTs time.Time) (int64, error) {
	deleted, err := s.repo.PruneWebhookDedup(ctx, cutoffTs)
	if err != nil {
		s.log.Error("webhook dedup gc: prune failed", slog.String("err", err.Error()))
		return 0, err
	}
	return deleted, nil
}

// ---------------------------------------------------------------------------
// Phase 4a — Log actions (resend + bulk delete)
// ---------------------------------------------------------------------------

// ResendEmail dispatches the resend_email agent command for a single log entry.
// Gate: body_stored must be true; returns 409 otherwise.
func (s *Service) ResendEmail(ctx context.Context, tenantID, siteID, logID uuid.UUID) (ResendResult, error) {
	bodyStored, err := s.repo.GetEmailLogBodyStored(ctx, tenantID, siteID, logID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return ResendResult{}, domain.NotFound("email_log_not_found", "email log entry not found")
		}
		return ResendResult{}, domain.Internal("resend_get_log", "failed to fetch log entry").WithCause(err)
	}
	if !bodyStored {
		return ResendResult{}, domain.Conflict("resend_body_not_stored",
			"resend is only available when body was captured at send time (body_stored=true); "+
				"this entry was sent without body capture enabled")
	}

	// Increment the resent_count counter before dispatching.
	if err := s.repo.IncrEmailLogResentCount(ctx, tenantID, siteID, logID); err != nil {
		return ResendResult{}, domain.Internal("resend_incr_count", "failed to increment resent_count").WithCause(err)
	}

	// Dispatch the signed resend_email agent command (Phase 4b implements the
	// agent side). Failure is non-fatal — the counter was already incremented
	// so the operator knows a resend was attempted.
	if s.agent == nil || s.siteLook == nil {
		return ResendResult{
			OK:     false,
			Detail: "agent command client not configured; resend dispatched counter incremented but command not sent",
		}, nil
	}

	siteURL, err := s.siteLook.GetSiteURL(ctx, tenantID, siteID)
	if err != nil {
		return ResendResult{OK: false, Detail: "site not enrolled or unavailable"}, nil
	}

	res, err := s.agent.ResendEmail(ctx, siteID, siteURL, agentcmd.ResendEmailRequest{LogID: logID.String()})
	if err != nil {
		return ResendResult{OK: false, Detail: err.Error()}, nil
	}
	// DELIBERATE: the agent's ok=false is propagated as this call's OK rather than
	// raised as an error. Every failure branch above returns the same shape, the
	// handler renders OK plus Detail to the operator, and BulkResendEmail reports
	// per-log outcomes from it. Nothing here reports success on a refusal.
	return ResendResult{OK: res.OK, Detail: res.Detail, MessageID: res.MessageID}, nil
}

// Bulk log-operation ceilings. These mirror the maxItems the OpenAPI schemas
// BulkResendRequest and BulkDeleteLogsRequest declare for log_ids; the handler
// rejects an oversized list before parsing it, and the checks below remain the
// authoritative gate for any non-HTTP caller.
const (
	MaxBulkResend = 100
	MaxBulkDelete = 500
)

// BulkResendEmail dispatches resend_email commands for multiple log entries.
// Each entry is processed independently; per-entry body_stored gate is checked.
func (s *Service) BulkResendEmail(ctx context.Context, tenantID, siteID uuid.UUID, logIDs []uuid.UUID) ([]BulkResendResult, error) {
	if len(logIDs) == 0 {
		return nil, nil
	}
	if len(logIDs) > MaxBulkResend {
		return nil, domain.Validation("resend_bulk_too_large", "bulk resend maximum is 100 entries per request")
	}
	results := make([]BulkResendResult, 0, len(logIDs))
	for _, id := range logIDs {
		res, err := s.ResendEmail(ctx, tenantID, siteID, id)
		if err != nil {
			var de *domain.Error
			if errors.As(err, &de) {
				results = append(results, BulkResendResult{LogID: id, OK: false, Detail: de.Message})
			} else {
				results = append(results, BulkResendResult{LogID: id, OK: false, Detail: err.Error()})
			}
			continue
		}
		results = append(results, BulkResendResult{LogID: id, OK: res.OK, Detail: res.Detail})
	}
	return results, nil
}

// BulkDeleteLogs deletes email log entries by id list.
// Returns the number of rows deleted.
func (s *Service) BulkDeleteLogs(ctx context.Context, tenantID, siteID uuid.UUID, logIDs []uuid.UUID) (int64, error) {
	if len(logIDs) == 0 {
		return 0, nil
	}
	if len(logIDs) > MaxBulkDelete {
		return 0, domain.Validation("bulk_delete_too_large", "bulk delete maximum is 500 entries per request")
	}
	deleted, err := s.repo.DeleteEmailLogsBulk(ctx, tenantID, siteID, logIDs)
	if err != nil {
		return 0, domain.Internal("bulk_delete_logs", "failed to delete log entries").WithCause(err)
	}
	return deleted, nil
}

// ---------------------------------------------------------------------------
// m61 — Webhook config management
// ---------------------------------------------------------------------------

// UpsertWebhookConfig sets the webhook security fields on a config row.
// It can rotate the route token (generating a new random token), store a new
// signing key (age-encrypted), and update the SES TopicArn allowlist.
//
// Returns the updated Config plus the plain route token when a rotation was
// requested (the only time the caller can see the plain token — store it immediately).
func (s *Service) UpsertWebhookConfig(ctx context.Context, in UpsertWebhookInput) (WebhookConfigResult, error) {
	var tokenHash []byte
	var plainToken string

	if in.RotateToken {
		var raw [32]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return WebhookConfigResult{}, domain.Internal("webhook_token_gen", "failed to generate route token").WithCause(err)
		}
		// Store the URL-safe base64 of the raw bytes as the token in the URL.
		// Hash it with SHA-256 for storage (token never at rest).
		plainToken = base64.RawURLEncoding.EncodeToString(raw[:])
		sum := sha256.Sum256([]byte(plainToken))
		tokenHash = sum[:]
	}

	var signingKeyCT []byte
	setSigningKey := false
	if in.SigningKeyRaw != nil {
		if s.enc == nil {
			return WebhookConfigResult{}, domain.ServiceUnavailable(
				"email_crypto_unwired",
				"secret encryption is not configured; cannot store webhook signing key",
			)
		}
		ct, err := s.enc.Encrypt([]byte(*in.SigningKeyRaw))
		if err != nil {
			return WebhookConfigResult{}, domain.Internal("webhook_signing_key_encrypt", "failed to encrypt webhook signing key").WithCause(err)
		}
		signingKeyCT = ct
		setSigningKey = true
	}

	var sesTopicArns []string
	if in.SesTopicArns != nil {
		sesTopicArns = *in.SesTopicArns
	}

	cfg, err := s.repo.SetWebhookFields(ctx, in.TenantID, in.ConfigID, tokenHash, signingKeyCT, setSigningKey, sesTopicArns)
	if err != nil {
		return WebhookConfigResult{}, domain.Internal("webhook_config_set", "failed to set webhook fields").WithCause(err)
	}

	if plainToken != "" {
		cfg.WebhookRouteToken = plainToken
	}

	return WebhookConfigResult{
		Config: cfg,
	}, nil
}

// ResolveWebhookConfig looks up a config row by routeToken (from the webhook URL)
// and returns the decrypted signing key for signature verification.
//
// It hashes the provided plain token and looks it up by hash — constant-time at
// the DB level (unique index scan). Returns ErrNotFound when unknown → 404.
func (s *Service) ResolveWebhookConfig(ctx context.Context, plainToken string) (WebhookResolvedConfig, error) {
	if strings.TrimSpace(plainToken) == "" {
		return WebhookResolvedConfig{}, ErrNotFound
	}
	sum := sha256.Sum256([]byte(plainToken))
	tokenHash := sum[:]

	cfg, signingKeyCT, err := s.repo.GetConfigByRouteTokenHashWithSecret(ctx, tokenHash)
	if err != nil {
		return WebhookResolvedConfig{}, err // ErrNotFound bubbles up as-is
	}

	var signingKeyPlain string
	if len(signingKeyCT) > 0 && s.enc != nil {
		plain, derr := s.enc.Decrypt(signingKeyCT)
		if derr != nil {
			return WebhookResolvedConfig{}, domain.Internal("webhook_signing_key_decrypt", "failed to decrypt webhook signing key").WithCause(derr)
		}
		signingKeyPlain = string(plain)
	}

	return WebhookResolvedConfig{
		Config:          cfg,
		SigningKeyPlain: signingKeyPlain,
	}, nil
}

// WebhookURL returns the public-facing URL for a config row's webhook endpoint.
// baseURL must not have a trailing slash (e.g. "https://manage.wpmgr.app").
// Returns "" when the config row has no route token yet.
func WebhookURL(baseURL, provider, plainToken string) string {
	if plainToken == "" {
		return ""
	}
	return fmt.Sprintf("%s/webhooks/email/%s/%s", baseURL, provider, plainToken)
}

// ---------------------------------------------------------------------------
// service helpers
// ---------------------------------------------------------------------------

func isSuppressionEventType(eventType string) bool {
	switch eventType {
	case "hard_bounce", "complaint", "unsubscribe":
		return true
	}
	return false
}

func webhookEventToLogStatus(eventType string) string {
	switch eventType {
	case "hard_bounce":
		return "bounced"
	case "complaint":
		return "complained"
	}
	return eventType
}

func ptrNow() *time.Time {
	t := time.Now().UTC()
	return &t
}

// buildAgentConfigReq maps a Config domain value and an already-resolved
// pushSecret into the wire shape sent to the agent. All four push paths use this
// so the mapping stays in one place.
//
// secret carries the three-state contract documented on
// agentcmd.EmailConfigRequest.Secret: say nothing, replace, or clear. An
// unresolved credential is never spelled as an empty string, which the agent
// would once have read as "delete the stored secret" (GH #380).
//
// m62: when cfg.ID is non-zero, connections are loaded and each one's secret
// resolved into the same three states (see connectionPushSecrets). Connection
// loading failures are logged but non-fatal — the push proceeds without
// connections rather than blocking the save, which leaves the agent's registry
// exactly as it is.
func (s *Service) buildAgentConfigReq(cfg Config, secret pushSecret) agentcmd.EmailConfigRequest {
	req := agentcmd.EmailConfigRequest{
		Provider:           cfg.Provider,
		FromAddress:        cfg.FromAddress,
		FromName:           cfg.FromName,
		ForceFromEmail:     cfg.ForceFromEmail,
		ForceFromName:      cfg.ForceFromName,
		ReturnPath:         cfg.ReturnPath,
		Config:             cfg.Config,
		Mappings:           cfg.Mappings,
		LogEmails:          cfg.LogEmails,
		StoreBody:          cfg.StoreBody,
		RetentionDays:      cfg.RetentionDays,
		DefaultConnection:  ptrStringVal(cfg.DefaultConnection),
		FallbackConnection: ptrStringVal(cfg.FallbackConnection),
	}
	secret.apply(&req)

	// m62: attach the named-connections registry if the config has an ID
	// (i.e. it was loaded from the DB, not a zero-value fallback).
	if cfg.ID != uuid.Nil && s.enc != nil {
		ctx := context.Background() // background — called outside a request
		secretRows, err := s.repo.GetConnectionSecretCiphertexts(ctx, cfg.TenantID, cfg.ID)
		if err != nil {
			s.log.Warn("email: could not load connection secrets for agent push", slog.Any("error", err))
		} else {
			states := s.connectionPushSecrets(secretRows)
			// Load full connection objects to build the registry.
			conns, cerr := s.repo.ListConnections(ctx, cfg.TenantID, cfg.ID)
			if cerr == nil && len(conns) > 0 {
				registry := make(map[string]agentcmd.EmailConnectionWire, len(conns))
				for _, c := range conns {
					wire := agentcmd.EmailConnectionWire{
						Provider:    c.Provider,
						Config:      c.Config,
						FromAddress: c.FromAddress,
						FromName:    c.FromName,
					}
					states[c.ConnectionKey].applyConnection(&wire)
					registry[c.ConnectionKey] = wire
				}
				req.Connections = registry
			}
		}
	}

	return req
}

// connectionPushSecrets resolves what one push says about each named
// connection's secret, keyed by connection key.
//
// It is resolveSitePushSecret's three states applied per registry entry, and it
// is what makes the per-connection clear_secret flag a live channel rather than
// a declared one: before this, a connection whose credential had been removed in
// the control plane was pushed with the secret merely omitted, which the agent
// reads as "keep what you have", so a connection credential could never be
// revoked at all.
//
//	stored ciphertext decrypts non-empty  replace
//	no stored ciphertext                  clear: the control plane owns this
//	                                      registry, so "none here" means none
//	stored ciphertext decrypts to empty   clear (an explicit revoke at rest)
//	stored ciphertext will not decrypt    say nothing
//
// The last line is the rule this whole issue turns on: unreadable is not absent.
// A key that changed under the ciphertext must never be spelled as a revoke.
func (s *Service) connectionPushSecrets(rows []ConnectionSecretRow) map[string]pushSecret {
	states := make(map[string]pushSecret, len(rows))
	for _, row := range rows {
		if len(row.ProviderSecretEncrypted) == 0 {
			states[row.ConnectionKey] = pushSecret{clear: true}
			continue
		}
		plain, derr := s.enc.Decrypt(row.ProviderSecretEncrypted)
		if derr != nil {
			s.log.Error("email: stored connection secret could not be decrypted; the agent push will say nothing about it",
				slog.String("connection_key", row.ConnectionKey),
				slog.Any("error", derr),
			)
			continue // absent from the map is the say-nothing state
		}
		if p := string(plain); p != "" {
			states[row.ConnectionKey] = pushSecret{plain: &p}
			continue
		}
		states[row.ConnectionKey] = pushSecret{clear: true}
	}
	return states
}

// ptrStringVal returns the dereferenced value of a *string, or "" if nil.
func ptrStringVal(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// ---------------------------------------------------------------------------
// m62 Area 2 — multi-connection CRUD
// ---------------------------------------------------------------------------

// maxConnectionsPerConfig is the maximum number of named connections allowed
// per config row (defensive; prevents accidental registry bloat).
const maxConnectionsPerConfig = 50

// connectionKeyPattern is the valid slug regex as enforced by the DB CHECK.
// Duplicated here for early validation before touching the DB.
// Pattern: ^[a-z0-9][a-z0-9_-]{0,31}$ and key must not equal "default".
func validConnectionKey(key string) bool {
	if key == "default" || len(key) < 1 || len(key) > 32 {
		return false
	}
	for i, ch := range key {
		switch {
		case ch >= 'a' && ch <= 'z':
		case ch >= '0' && ch <= '9':
		case ch == '_' || ch == '-':
			if i == 0 {
				return false // must start with [a-z0-9]
			}
		default:
			return false
		}
	}
	return true
}

// ListConnections returns all named connections for a config row.
func (s *Service) ListConnections(ctx context.Context, tenantID, configID uuid.UUID) ([]Connection, error) {
	conns, err := s.repo.ListConnections(ctx, tenantID, configID)
	if err != nil {
		return nil, domain.Internal("email_list_connections", "failed to list connections").WithCause(err)
	}
	return conns, nil
}

// UpsertConnection creates or updates a named connection. Validates the slug,
// age-encrypts the secret when provided, and returns the updated Connection.
//
// The secret follows the same three states as the top-level config (GH #380),
// and for the same reason: nothing may collapse "the operator said nothing"
// into "delete it", and nothing may carry a credential across to an account it
// was not issued for.
//
//	SecretRaw nil        keep the stored credential, unless the settings being
//	                     saved move the account it authenticates as, in which
//	                     case it is revoked here and cleared on the next push
//	SecretRaw non-empty  replace the stored credential
//	SecretRaw ""         an explicit revoke: the column is NULLed
func (s *Service) UpsertConnection(ctx context.Context, in ConnectionUpsertInput) (Connection, error) {
	if !validConnectionKey(in.ConnectionKey) {
		return Connection{}, domain.Validation("email_invalid_connection_key",
			"connection_key must match ^[a-z0-9][a-z0-9_-]{0,31}$ and must not be 'default'")
	}
	if !ValidProviderSlug(in.Provider) {
		return Connection{}, domain.Validation("email_invalid_provider",
			"provider must be one of: smtp, ses, sendgrid, mailgun, postmark")
	}
	if in.Config == nil {
		in.Config = map[string]any{}
	}

	var secretCiphertext []byte
	setSecret := false
	switch {
	case in.SecretRaw == nil:
		stored, getErr := s.repo.GetConnection(ctx, in.TenantID, in.ConfigID, in.ConnectionKey)
		switch {
		case errors.Is(getErr, ErrNotFound):
			// A brand new connection: nothing is stored, so nothing can be
			// rebound and nothing can be destroyed.
		case getErr != nil:
			// Without the previous settings there is no way to tell a
			// correction from a move to another account. Refusing the write is
			// the only answer that neither revokes a working credential nor
			// re-points one, and it is reported rather than guessed at.
			return Connection{}, domain.Internal("email_get_connection",
				"failed to load the stored connection; the change was not saved").WithCause(getErr)
		case stored.SecretSet && !sameConnectionAudience(stored, in):
			// The credential stored on this row was issued for the settings
			// being replaced. Preserving it would offer it to whatever endpoint
			// this request supplies, which is the connection-registry spelling
			// of the escalation closed on the top-level config.
			s.log.Info("email: connection settings moved the account the stored credential authenticates as; it is revoked and must be re-entered",
				slog.String("tenant_id", in.TenantID.String()),
				slog.String("connection_key", in.ConnectionKey),
				slog.String("stored_provider", stored.Provider),
				slog.String("provider", in.Provider),
			)
			setSecret = true // with a nil ciphertext the SQL writes NULL
		}

	case *in.SecretRaw == "":
		// An explicit revoke. Storing the ciphertext of an empty string instead
		// left the row reporting secret_set=true for a credential that is not
		// there. No encryptor is needed to write a NULL, so this is deliberately
		// outside the age-guard below: revoking must work even on an instance
		// whose key went missing.
		setSecret = true

	default:
		if s.enc == nil {
			return Connection{}, domain.ServiceUnavailable(
				"email_crypto_unwired",
				"secret encryption is not configured; cannot store connection secret",
			)
		}
		ct, err := s.enc.Encrypt([]byte(*in.SecretRaw))
		if err != nil {
			return Connection{}, domain.Internal("email_encrypt_connection_secret", "failed to encrypt connection secret").WithCause(err)
		}
		secretCiphertext = ct
		setSecret = true
	}

	conn, err := s.repo.UpsertConnection(ctx, in, secretCiphertext, setSecret)
	if err != nil {
		return Connection{}, domain.Internal("email_upsert_connection", "failed to save connection").WithCause(err)
	}
	return conn, nil
}

// DeleteConnection removes a named connection. Returns 409 Conflict when the
// key is referenced as default_connection or fallback_connection on the parent
// config row (checked in-service by loading the config row first).
func (s *Service) DeleteConnection(ctx context.Context, tenantID, configID uuid.UUID, key string) error {
	// Check that the config row is not referencing this key.
	// We do not cross-reference mappings (complex enough for v1).
	cfgRows, err := s.repo.ListSiteConfigs(ctx, tenantID, 1000, 0)
	if err == nil {
		for _, c := range cfgRows {
			if c.ID == configID {
				if (c.DefaultConnection != nil && *c.DefaultConnection == key) ||
					(c.FallbackConnection != nil && *c.FallbackConnection == key) {
					return domain.Conflict("email_connection_in_use",
						"connection is referenced as default_connection or fallback_connection; update the config before deleting")
				}
			}
		}
	}
	if err := s.repo.DeleteConnection(ctx, tenantID, configID, key); err != nil {
		return domain.Internal("email_delete_connection", "failed to delete connection").WithCause(err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// m62 Area 1 — org-wide config propagation
// ---------------------------------------------------------------------------

// PropagateOrgConfig fans the org-wide config out to all enrolled inheriting
// sites for the given tenant. At most 8 sites are pushed concurrently.
// Returns the counts of synced/failed/total. Errors are per-site and never
// fatal for the whole job.
func (s *Service) PropagateOrgConfig(ctx context.Context, tenantID uuid.UUID) (PropagateResult, error) {
	if s.agent == nil || s.siteLook == nil {
		return PropagateResult{}, nil // agent not wired — no-op
	}

	// Load the org config + effective secret.
	orgCfg, err := s.repo.GetOrgConfig(ctx, tenantID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return PropagateResult{}, nil // no org config to propagate
		}
		return PropagateResult{}, domain.Internal("propagate_get_org_cfg", "failed to load org config").WithCause(err)
	}
	orgSecret, decryptFailed := s.resolveOrgPushSecret(ctx, tenantID, orgCfg)
	if decryptFailed {
		// Fanning a config out to every inheriting site without the credential
		// it needs is the fleet-wide version of GH #380. Abort, loudly.
		return PropagateResult{}, domain.Internal("propagate_org_secret",
			"the stored org email credential could not be decrypted; no site was updated")
	}
	req := s.buildAgentConfigReq(orgCfg, orgSecret)

	// Enumerate inheriting sites.
	sites, err := s.repo.ListEmailInheritingSites(ctx, tenantID)
	if err != nil {
		return PropagateResult{}, domain.Internal("propagate_list_sites", "failed to list inheriting sites").WithCause(err)
	}

	if len(sites) == 0 {
		publishConfigPropagated(ctx, s.pub, tenantID, 0, 0, 0)
		return PropagateResult{}, nil
	}

	var mu sync.Mutex
	result := PropagateResult{Total: len(sites)}

	eg, egCtx := errgroup.WithContext(ctx)
	eg.SetLimit(8) // max 8 concurrent site pushes
	for _, site := range sites {
		site := site // capture
		eg.Go(func() error {
			pushCtx, cancel := context.WithTimeout(egCtx, 15*time.Second)
			defer cancel()
			_, syncErr := s.agent.SyncEmailConfig(pushCtx, site.ID, site.URL, req)
			mu.Lock()
			if syncErr != nil {
				result.Failed++
				s.log.Warn("email propagate: agent sync failed",
					slog.String("site_id", site.ID.String()),
					slog.Any("error", syncErr),
				)
			} else {
				result.Synced++
			}
			mu.Unlock()
			return nil // per-site failures are non-fatal for the errgroup
		})
	}
	_ = eg.Wait()

	publishConfigPropagated(ctx, s.pub, tenantID, result.Synced, result.Failed, result.Total)
	return result, nil
}

// ---------------------------------------------------------------------------
// m62 Area 4 — notify settings
// ---------------------------------------------------------------------------

// GetNotifySettings returns the org-level notify settings. When no row exists
// the service returns safe defaults with GET-always-200 semantics (lesson from
// 0.35.1 hotfix: never 404 on a settings GET).
func (s *Service) GetNotifySettings(ctx context.Context, tenantID uuid.UUID) (NotifySettings, error) {
	settings, err := s.repo.GetNotifySettings(ctx, tenantID)
	if errors.Is(err, ErrNotFound) {
		defaults := defaultNotifySettings(tenantID)
		defaults.InstanceMailerConfigured = s.mailerIsConfigured(ctx)
		return defaults, nil
	}
	if err != nil {
		return NotifySettings{}, domain.Internal("email_get_notify_settings", "failed to load notify settings").WithCause(err)
	}
	settings.InstanceMailerConfigured = s.mailerIsConfigured(ctx)
	return settings, nil
}

// PutNotifySettings validates and upserts the notify settings, computing
// next_digest_at from the scheduling fields.
func (s *Service) PutNotifySettings(ctx context.Context, in NotifySettingsUpsertInput) (NotifySettings, error) {
	if err := validateNotifySettings(in); err != nil {
		return NotifySettings{}, err
	}

	// When the digest is disabled, its scheduling fields are neither validated
	// nor required from the caller (see validateNotifySettings), so they may
	// arrive empty/zero. Normalize them to the column defaults here — the DB
	// CHECK constraints on digest_cadence/digest_day/digest_hour still apply
	// to every row regardless of digest_enabled, so we must never persist
	// whatever unvalidated value the caller happened to send.
	digestCadence, digestDay, digestHour, timezone := in.DigestCadence, in.DigestDay, in.DigestHour, in.Timezone
	var nextAt *time.Time
	if in.DigestEnabled {
		nextAt = nextDigestAt(in.DigestCadence, in.DigestDay, in.DigestHour, in.Timezone)
	} else {
		digestCadence, digestDay, digestHour, timezone = "weekly", 1, 8, "UTC"
	}

	settings := NotifySettings{
		TenantID:             in.TenantID,
		Enabled:              in.Enabled,
		Recipients:           in.Recipients,
		AlertOnFailure:       in.AlertOnFailure,
		AlertThrottleMinutes: in.AlertThrottleMinutes,
		DigestEnabled:        in.DigestEnabled,
		DigestCadence:        digestCadence,
		DigestDay:            digestDay,
		DigestHour:           digestHour,
		Timezone:             timezone,
		NextDigestAt:         nextAt,
	}

	saved, err := s.repo.UpsertNotifySettings(ctx, settings)
	if err != nil {
		return NotifySettings{}, domain.Internal("email_put_notify_settings", "failed to save notify settings").WithCause(err)
	}
	saved.InstanceMailerConfigured = s.mailerIsConfigured(ctx)
	return saved, nil
}

// errCodeValidation wraps a validation error for the email package. Used by
// notify.go which cannot import domain directly in errCode().
func errCodeValidation(code, msg string) error {
	return domain.Validation(code, msg)
}

// ---------------------------------------------------------------------------
// Secret resolution for agent pushes (GH #380)
// ---------------------------------------------------------------------------

// pushSecret is everything one agent push says about the provider secret. It is
// the CP-side spelling of the three-state wire contract documented on
// agentcmd.EmailConfigRequest.Secret:
//
//	{}                  say nothing; the site keeps the credential it has
//	{plain: &value}     replace the stored credential
//	{clear: true}       delete the stored credential
//
// plain and clear are never both set.
type pushSecret struct {
	plain *string
	clear bool
}

// apply writes this resolution onto an outbound request.
func (p pushSecret) apply(req *agentcmd.EmailConfigRequest) {
	req.Secret = p.plain
	req.ClearSecret = p.clear
}

// applyConnection writes this resolution onto one entry of the named-connection
// registry. The registry speaks the same three states through its own per-entry
// flag, so the two must not drift apart.
func (p pushSecret) applyConnection(w *agentcmd.EmailConnectionWire) {
	w.Secret = p.plain
	w.ClearSecret = p.clear
}

// resolveEffectiveSecret resolves what UpsertSiteConfig's push should say about
// the secret. override is in.SecretRaw: non-nil means the operator just typed
// something on the form, which takes precedence over anything stored.
func (s *Service) resolveEffectiveSecret(ctx context.Context, in UpsertInput, saved Config) (pushSecret, error) {
	// An org-wide row is not pushed from here; nothing site-specific to resolve.
	if in.SiteID == nil {
		return pushSecret{}, nil
	}
	push, decryptFailed := s.resolveSitePushSecret(ctx, in.TenantID, *in.SiteID, saved, in.SecretRaw)
	if decryptFailed {
		return pushSecret{}, errors.New("stored provider secret could not be decrypted")
	}
	return push, nil
}

// resolveSitePushSecret decides what a push to one site says about the secret.
//
// The order is: what the operator just typed, then the site's own stored
// credential, then the org-wide credential — but the org credential is only
// ever handed to a config that is still the org's own (sameCredentialAudience).
//
// SECURITY (GH #380). PermEmailManage is not an org-level permission, so a
// site-scoped collaborator can PUT a per-site email config. An ungated org
// fallback let them name any SMTP host, save with no secret, and have the
// control plane pair the ORG credential with their endpoint; a test send then
// made the agent AUTH LOGIN to it. The audience check is what makes that
// impossible: a credential only travels to the endpoint it was issued for.
//
// The same check is why a divergent config CLEARS rather than omits. Omitting
// leaves the agent using a credential it already stored against an endpoint its
// owner never chose, which is the same escalation one push later. Clearing on
// divergence is also what the pre-fix code did by accident, via the empty-string
// secret that this whole issue is about; here it is deliberate and narrow.
//
// The second return is true when a stored ciphertext exists but will not
// decrypt. That is never resolved by substituting some other credential — the
// caller aborts the push and reports it.
func (s *Service) resolveSitePushSecret(ctx context.Context, tenantID, siteID uuid.UUID, cfg Config, override *string) (pushSecret, bool) {
	// 1. The operator just typed something. An empty box is an explicit revoke.
	if override != nil {
		if *override == "" {
			return pushSecret{clear: true}, false
		}
		return pushSecret{plain: override}, false
	}

	// 2. No encryptor wired: nothing can be resolved, and nothing may be
	// destroyed on a guess. Say nothing.
	if s.enc == nil {
		return pushSecret{}, false
	}

	// 3. The site's own stored credential. cfg.Inherited matters here: for an
	// inherited row SecretSet describes the ORG's secret, not the site's.
	if !cfg.Inherited && cfg.SecretSet {
		ct, err := s.repo.GetSecretCiphertext(ctx, tenantID, siteID)
		if err != nil || len(ct) == 0 {
			// The row says a credential is stored but we could not read it.
			// "We do not know" is not "there is none": a transient database
			// error must never be allowed to revoke a working credential.
			s.log.Warn("email: could not read the stored provider secret; the push will say nothing about it",
				slog.String("site_id", siteID.String()),
				slog.Any("error", err),
			)
			return pushSecret{}, false
		}
		plain, dErr := s.enc.Decrypt(ct)
		if dErr != nil {
			s.log.Error("email: stored provider secret could not be decrypted; the agent push is aborted",
				slog.String("site_id", siteID.String()),
				slog.Any("error", dErr),
			)
			return pushSecret{}, true
		}
		if p := string(plain); p != "" {
			return pushSecret{plain: &p}, false
		}
		// A stored credential that decrypts to empty is an explicit clear at
		// rest. It must not fall through to the org credential.
		return pushSecret{clear: true}, false
	}

	// 4. The org-wide credential, for a site that has none of its own.
	orgCfg, orgErr := s.repo.GetOrgConfig(ctx, tenantID)
	switch {
	case errors.Is(orgErr, ErrNotFound):
		// No org config at all: there is no credential anywhere for this site,
		// so anything the site still holds is revoked.
		return pushSecret{clear: true}, false
	case orgErr != nil:
		// Same rule as above: an unreadable org row is not an absent one.
		s.log.Warn("email: could not read the org email config; the push will say nothing about the secret",
			slog.String("tenant_id", tenantID.String()),
			slog.Any("error", orgErr),
		)
		return pushSecret{}, false
	}
	if !cfg.Inherited && !sameCredentialAudience(cfg, orgCfg) {
		s.log.Info("email: site config no longer matches the org endpoint; the org credential is withheld and the site's stored one revoked",
			slog.String("site_id", siteID.String()),
			slog.String("site_provider", cfg.Provider),
			slog.String("org_provider", orgCfg.Provider),
		)
		return pushSecret{clear: true}, false
	}
	orgCt, ctErr := s.repo.GetOrgSecretCiphertext(ctx, tenantID)
	if ctErr != nil {
		s.log.Warn("email: could not read the org provider secret; the push will say nothing about it",
			slog.String("tenant_id", tenantID.String()),
			slog.Any("error", ctErr),
		)
		return pushSecret{}, false
	}
	if len(orgCt) == 0 {
		// The org genuinely has no credential, so neither may this site.
		return pushSecret{clear: true}, false
	}
	plain, dErr := s.enc.Decrypt(orgCt)
	if dErr != nil {
		s.log.Error("email: stored org provider secret could not be decrypted; the agent push is aborted",
			slog.String("tenant_id", tenantID.String()),
			slog.Any("error", dErr),
		)
		return pushSecret{}, true
	}
	if p := string(plain); p != "" {
		return pushSecret{plain: &p}, false
	}
	return pushSecret{clear: true}, false
}

// resolveOrgPushSecret decides what an org-wide push says about the secret.
//
// It is only used by PropagateOrgConfig, whose targets are sites with no config
// row of their own (see ListEmailInheritingSites). Two things follow: there is
// no site-supplied endpoint to check the credential against, and any credential
// those sites hold can only have arrived from an earlier org push. So when the
// org has no secret, propagation revokes rather than stays quiet — otherwise
// clearing the org credential would leave it live on every inheriting site.
func (s *Service) resolveOrgPushSecret(ctx context.Context, tenantID uuid.UUID, orgCfg Config) (pushSecret, bool) {
	// No encryptor: nothing can be resolved, and nothing may be destroyed on a
	// guess. This is the one case that stays quiet.
	if s.enc == nil {
		return pushSecret{}, false
	}
	if !orgCfg.SecretSet {
		return pushSecret{clear: true}, false
	}
	ct, err := s.repo.GetOrgSecretCiphertext(ctx, tenantID)
	if err != nil {
		// Unreadable is not absent; do not revoke a fleet on a database blip.
		s.log.Warn("email: could not read the org provider secret; the propagation will say nothing about it",
			slog.String("tenant_id", tenantID.String()),
			slog.Any("error", err),
		)
		return pushSecret{}, false
	}
	if len(ct) == 0 {
		return pushSecret{clear: true}, false
	}
	plain, dErr := s.enc.Decrypt(ct)
	if dErr != nil {
		s.log.Error("email: stored org provider secret could not be decrypted; the propagation is aborted",
			slog.String("tenant_id", tenantID.String()),
			slog.Any("error", dErr),
		)
		return pushSecret{}, true
	}
	if p := string(plain); p != "" {
		return pushSecret{plain: &p}, false
	}
	return pushSecret{clear: true}, false
}

// credentialAudienceFields lists, per provider, the non-secret config fields
// that decide WHERE a credential is presented and WHO it authenticates as. Two
// configs that agree on the provider and on every one of these fields are the
// same audience, so a credential issued for one is safe to present to the other.
//
// Fields that only change formatting or provider-side behaviour (auto_tls,
// track_opens, message_stream, return_path…) are deliberately absent: they
// cannot redirect a credential, and including them would break inheritance for
// a site that legitimately tweaks one.
//
// The second return says whether this table has an answer for the provider at
// all. It exists so that adding a provider to the catalog and forgetting this
// table fails CLOSED: without it, a new slug fell to the default, returned no
// fields, and every config of that provider compared as the same audience,
// which is the loosest possible answer on the one path that lends out the org
// credential. TestCredentialAudienceFieldsCoverTheCatalog keeps the two in step.
func credentialAudienceFields(provider string) ([]string, bool) {
	switch provider {
	case "smtp":
		// host and port are the destination; username is the identity;
		// encryption decides whether the password crosses the network in clear.
		return []string{"host", "port", "username", "encryption", "auth"}, true
	case "ses":
		// The secret access key is meaningless apart from its access key ID,
		// and the region selects the endpoint.
		return []string{"access_key", "region"}, true
	case "mailgun":
		return []string{"domain_name", "region"}, true
	case "sendgrid", "postmark":
		// The endpoint is the provider's own and is not operator-supplied.
		return nil, true
	default:
		// An unknown provider gets the strictest answer available: no shared
		// credential at all (see sameCredentialAudience).
		return nil, false
	}
}

// sameCredentialAudience reports whether a credential stored against org may be
// presented using site's config.
func sameCredentialAudience(site, org Config) bool {
	if site.Provider != org.Provider {
		return false
	}
	if !ValidProviderSlug(site.Provider) {
		// Never share a credential with a config we cannot reason about.
		return false
	}
	fields, known := credentialAudienceFields(site.Provider)
	if !known {
		return false
	}
	for _, key := range fields {
		if !sameConfigValue(site.Config[key], org.Config[key]) {
			return false
		}
	}
	return true
}

// sameConnectionAudience reports whether the credential already stored against a
// named connection may still be presented using the settings now being saved
// onto it.
//
// It is sameCredentialAudience for the connection registry. The registry stores
// each connection's credential on the connection's own row, and the upsert
// preserves that ciphertext whenever the request carries no secret
// (site_email.sql, UpsertEmailConnection). So an operator can rewrite the host,
// port or username of a connection, send no password, and have the credential
// issued for the old account carried forward to the new one — the same rebinding
// the top-level config closes by clearing on divergence.
func sameConnectionAudience(stored Connection, in ConnectionUpsertInput) bool {
	if stored.Provider != in.Provider {
		return false
	}
	fields, known := credentialAudienceFields(in.Provider)
	if !known {
		return false
	}
	for _, key := range fields {
		if !sameConfigValue(in.Config[key], stored.Config[key]) {
			return false
		}
	}
	return true
}

// sameConfigValue compares two values out of the provider config map. Both
// sides come from the same JSONB column so their Go types already agree;
// formatting them is a cheap way to stay correct for the string/number/bool mix
// the map holds, and an absent key compares equal to an empty one.
func sameConfigValue(a, b any) bool {
	return configValueKey(a) == configValueKey(b)
}

func configValueKey(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprintf("%v", v)
}

// secretDecryptFailedDetail is what an operator is told when a stored email
// credential will not decrypt. Plaintext that cannot be decrypted cannot be
// recovered by any code change; re-entering it is the only repair.
const secretDecryptFailedDetail = "the stored email credential could not be decrypted (the secrets-at-rest key changed); re-enter the SMTP password"

// decryptSecret is deliberately gone. It had no callers left once the push paths
// moved onto resolveSitePushSecret / resolveOrgPushSecret, and it read the ORG
// ciphertext on siteID == uuid.Nil with no audience check at all: the exact
// read this issue closed everywhere else. Leaving an unused, ungated way in
// would have been an invitation to wire it back up.
