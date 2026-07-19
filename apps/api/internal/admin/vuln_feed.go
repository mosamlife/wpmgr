package admin

// vuln_feed.go — superadmin management of the Wordfence Intelligence API key.
//
// Storage: instance_settings table (m80). Key name "vuln_feed.wordfence_api_key".
// The plaintext key is NEVER returned or logged; only the configured/source/ok
// status is surfaced. Encrypted at rest via cryptbox (age X25519), same
// primitive as SMTP settings and site destinations.
//
// KeyResolver interface satisfies vuln.APIKeyResolver — the admin package
// provides the concrete implementation that the vuln FeedWorker calls at
// job-run time to get the effective API key without requiring a restart.

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/cryptbox"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// instanceSettingKey is the key for the Wordfence API key in instance_settings.
const instanceSettingKey = "vuln_feed.wordfence_api_key"

// ---------------------------------------------------------------------------
// InstanceSettingsStore interface + InstanceSettingsRepo concrete impl
// ---------------------------------------------------------------------------

// InstanceSettingsStore is the persistence surface for instance_settings.
// The concrete *InstanceSettingsRepo satisfies it; the interface allows test
// fakes to be injected without a real DB pool.
type InstanceSettingsStore interface {
	Get(ctx context.Context, key string) (enc []byte, ok bool, err error)
	Set(ctx context.Context, key string, enc []byte) error
	Delete(ctx context.Context, key string) error
	UpdatedAt(ctx context.Context, key string) (t time.Time, ok bool, err error)
}

// InstanceSettingsRepo provides raw access to the instance_settings table.
// All operations run under pool.InAgentTx (no tenant GUC; app.agent='on' is
// the RLS unlock). The real access control is requireSuperadmin at the HTTP layer.
type InstanceSettingsRepo struct {
	pool *db.Pool
}

// NewInstanceSettingsRepo builds an InstanceSettingsRepo.
func NewInstanceSettingsRepo(pool *db.Pool) *InstanceSettingsRepo {
	return &InstanceSettingsRepo{pool: pool}
}

// Compile-time check that *InstanceSettingsRepo satisfies InstanceSettingsStore.
var _ InstanceSettingsStore = (*InstanceSettingsRepo)(nil)

// Get returns the encrypted value for key, or (nil, false, nil) when unset.
func (r *InstanceSettingsRepo) Get(ctx context.Context, key string) ([]byte, bool, error) {
	var enc []byte
	var ok bool
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		// Scan value_enc (bytea) directly; sqlc is not generated for this table.
		var raw []byte
		err := tx.QueryRow(ctx,
			`SELECT value_enc FROM instance_settings WHERE key = $1`, key,
		).Scan(&raw)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		enc = raw
		ok = len(raw) > 0
		return nil
	})
	return enc, ok, err
}

// Set upserts the encrypted value for key, setting updated_at = now().
func (r *InstanceSettingsRepo) Set(ctx context.Context, key string, enc []byte) error {
	return r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO instance_settings (key, value_enc, updated_at)
			 VALUES ($1, $2, now())
			 ON CONFLICT (key) DO UPDATE SET value_enc = EXCLUDED.value_enc, updated_at = now()`,
			key, enc,
		)
		return err
	})
}

// Delete removes the row for key. No-op if not present.
func (r *InstanceSettingsRepo) Delete(ctx context.Context, key string) error {
	return r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM instance_settings WHERE key = $1`, key)
		return err
	})
}

// UpdatedAt returns the last-updated timestamp for key, or (zero, false, nil) when unset.
func (r *InstanceSettingsRepo) UpdatedAt(ctx context.Context, key string) (time.Time, bool, error) {
	var t time.Time
	var found bool
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		var ts pgtype.Timestamptz
		err := tx.QueryRow(ctx,
			`SELECT updated_at FROM instance_settings WHERE key = $1`, key).Scan(&ts)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		if ts.Valid {
			t = ts.Time
			found = true
		}
		return nil
	})
	return t, found, err
}

// ---------------------------------------------------------------------------
// VulnFeedKeyService
// ---------------------------------------------------------------------------

// VulnFeedKeyService manages the UI-stored Wordfence Intelligence API key.
// It also satisfies vuln.APIKeyResolver: the FeedWorker calls ResolveAPIKey at
// job-run time so a newly-set UI key takes effect without a restart.
type VulnFeedKeyService struct {
	repo    InstanceSettingsStore
	age     *cryptbox.AgeIdentity
	envKey  string // WPMGR_WORDFENCE_API_KEY env fallback
	enqueue VulnFeedEnqueuer
	log     *slog.Logger
}

// VulnFeedEnqueuer enqueues an immediate Wordfence feed refresh job.
// *vuln.RiverFeedRefreshEnqueuer satisfies this interface.
type VulnFeedEnqueuer interface {
	EnqueueFeedRefresh(ctx context.Context) error
}

// NewVulnFeedKeyService builds the service.
// repo must satisfy InstanceSettingsStore (typically *InstanceSettingsRepo).
// envKey is the WPMGR_WORDFENCE_API_KEY value (may be empty).
// enqueue may be nil; in that case TriggerSync returns a ServiceUnavailable error.
func NewVulnFeedKeyService(
	repo InstanceSettingsStore,
	age *cryptbox.AgeIdentity,
	envKey string,
	enqueue VulnFeedEnqueuer,
	log *slog.Logger,
) *VulnFeedKeyService {
	if log == nil {
		log = slog.Default()
	}
	return &VulnFeedKeyService{
		repo:    repo,
		age:     age,
		envKey:  envKey,
		enqueue: enqueue,
		log:     log,
	}
}

// SetEnqueuer wires in the feed refresh enqueuer after River has started.
// Called once at boot after startRiver returns, mirroring the pattern used by
// the rescan enqueuer.
func (s *VulnFeedKeyService) SetEnqueuer(enqueue VulnFeedEnqueuer) {
	s.enqueue = enqueue
}

// ---------------------------------------------------------------------------
// ResolveAPIKey — satisfies vuln.APIKeyResolver
// ---------------------------------------------------------------------------

// ResolveAPIKey returns the effective Wordfence Intelligence API key and its source.
// Priority: UI-set instance_settings key (encrypted at rest) > WPMGR_WORDFENCE_API_KEY env > ("","none").
// Any decrypt failure is logged and falls through to the env key.
// The key is NEVER logged at any level.
func (s *VulnFeedKeyService) ResolveAPIKey(ctx context.Context) (key, source string) {
	enc, ok, err := s.repo.Get(ctx, instanceSettingKey)
	if err != nil {
		s.log.Warn("admin: instance_settings get failed; falling back to env key", slog.Any("error", err))
	} else if ok && len(enc) > 0 {
		plain, derr := s.age.Decrypt(enc)
		if derr != nil {
			s.log.Warn("admin: failed to decrypt UI-stored Wordfence key; falling back to env key", slog.Any("error", derr))
		} else if k := strings.TrimSpace(string(plain)); k != "" {
			return k, "ui"
		}
	}
	if k := strings.TrimSpace(s.envKey); k != "" {
		return k, "env"
	}
	return "", "none"
}

// ---------------------------------------------------------------------------
// SetKey / ClearKey / Status / TriggerSync
// ---------------------------------------------------------------------------

// VulnFeedStatus is the masked status returned by the feed-status endpoint.
// The plaintext key is NEVER included.
type VulnFeedStatus struct {
	Configured  bool    `json:"configured"`
	Source      string  `json:"source"` // "ui" | "env" | "none"
	FeedOK      bool    `json:"feed_ok"`
	RecordCount int     `json:"record_count"`
	LastSynced  *string `json:"last_synced,omitempty"` // RFC3339
	LastError   string  `json:"last_error,omitempty"`

	// EnrichmentAvailable/LastEnrichmentAt track the Production-feed CVSS
	// enrichment pipeline independently of FeedOK (which tracks Scanner-driven
	// detection freshness) — see vuln.FeedMeta doc.
	EnrichmentAvailable bool    `json:"enrichment_available"`
	LastEnrichmentAt    *string `json:"last_enrichment_at,omitempty"` // RFC3339
}

// feedMetaReader is a narrow slice of the vuln repo used by the status endpoint.
// *vuln.Repo satisfies this interface.
type feedMetaReader interface {
	GetFeedMetaPlain(ctx context.Context) (ok bool, recordCount int, lastSynced *time.Time, lastError string, err error)
}

// SetKey validates, encrypts, and stores a new Wordfence Intelligence API key,
// then enqueues an immediate feed sync. The plaintext key is never persisted or
// logged — only the age-encrypted ciphertext is stored in instance_settings.
func (s *VulnFeedKeyService) SetKey(ctx context.Context, plainKey string) error {
	k := strings.TrimSpace(plainKey)
	if k == "" {
		return domain.Validation("key_required", "Wordfence Intelligence API key must not be empty")
	}
	// Basic sanity: the key should be a non-trivial string. We do not validate
	// the exact format (Wordfence Intelligence keys may vary) but we reject keys
	// that are obviously wrong (< 8 chars).
	if len(k) < 8 {
		return domain.Validation("key_too_short", "Wordfence Intelligence API key appears too short; check the value")
	}

	enc, err := s.age.Encrypt([]byte(k))
	if err != nil {
		return domain.Internal("key_encrypt_failed", "failed to encrypt the API key").WithCause(err)
	}
	if err := s.repo.Set(ctx, instanceSettingKey, enc); err != nil {
		return domain.Internal("key_store_failed", "failed to store the API key").WithCause(err)
	}
	return nil
}

// ClearKey removes the UI-stored key. The worker falls back to the env key
// (WPMGR_WORDFENCE_API_KEY) or no-ops when neither is set.
func (s *VulnFeedKeyService) ClearKey(ctx context.Context) error {
	if err := s.repo.Delete(ctx, instanceSettingKey); err != nil {
		return domain.Internal("key_clear_failed", "failed to clear the API key").WithCause(err)
	}
	return nil
}

// TriggerSync enqueues an immediate feed refresh job. Returns 202 if the job
// was accepted; returns ServiceUnavailable when the enqueuer is not wired.
func (s *VulnFeedKeyService) TriggerSync(ctx context.Context) error {
	if s.enqueue == nil {
		return domain.ServiceUnavailable("feed_enqueuer_not_wired", "feed refresh enqueuer is not available")
	}
	if err := s.enqueue.EnqueueFeedRefresh(ctx); err != nil {
		return domain.Internal("feed_enqueue_failed", "failed to enqueue feed refresh").WithCause(err)
	}
	return nil
}
