package perf

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/rum"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

// AgentPerfClient is the subset of agentcmd.Client the perf service needs to
// push config + cache commands. *agentcmd.Client satisfies it. Declared as an
// interface so tests substitute a fake without the SSRF transport.
type AgentPerfClient interface {
	SyncPerfConfig(ctx context.Context, siteID uuid.UUID, siteURL string, req agentcmd.PerfConfigRequest) (agentcmd.PerfConfigResult, error)
	CacheEnable(ctx context.Context, siteID uuid.UUID, siteURL string, req agentcmd.CacheEnableRequest) (agentcmd.CacheEnableResult, error)
	CacheDisable(ctx context.Context, siteID uuid.UUID, siteURL string, req agentcmd.CacheDisableRequest) (agentcmd.CacheDisableResult, error)
	CachePurge(ctx context.Context, siteID uuid.UUID, siteURL string, req agentcmd.CachePurgeRequest) (agentcmd.CachePurgeResult, error)
	CachePreload(ctx context.Context, siteID uuid.UUID, siteURL string, req agentcmd.CachePreloadRequest) (agentcmd.CachePreloadResult, error)
	RucssCompute(ctx context.Context, siteID uuid.UUID, siteURL string, req agentcmd.RucssComputeRequest) (agentcmd.RucssComputeResult, error)
	DBClean(ctx context.Context, siteID uuid.UUID, siteURL string, req agentcmd.DBCleanRequest) (agentcmd.DBCleanResult, error)
	// M39 — synchronous db_scan command (READ-ONLY, full result in ACK body).
	DBScan(ctx context.Context, siteID uuid.UUID, siteURL string, req agentcmd.DBScanRequest) (agentcmd.DBScanResult, error)
	// Phase 2.2/2.5 — synchronous per-table DDL action (optimize/repair/drop/empty/analyze/convert_innodb).
	DBTableAction(ctx context.Context, siteID uuid.UUID, siteURL string, req agentcmd.DBTableActionRequest) (agentcmd.DBTableActionResult, error)
	// Phase 3.8 — async destructive orphan deletion.
	DBOrphanDelete(ctx context.Context, siteID uuid.UUID, siteURL string, req agentcmd.DBOrphanDeleteRequest) (agentcmd.DBOrphanDeleteResult, error)
	// #188 — synchronous serialization-safe search-replace (dry-run capable).
	SearchReplace(ctx context.Context, siteID uuid.UUID, siteURL string, req agentcmd.SearchReplaceRequest) (agentcmd.SearchReplaceResult, error)
	// #189 — local database snapshot (create/list/revert/delete).
	DbSnapshot(ctx context.Context, siteID uuid.UUID, siteURL string, req agentcmd.DbSnapshotRequest) (agentcmd.DbSnapshotResult, error)
	// #190 — media library cleaner (scan/isolate/restore/delete).
	MediaClean(ctx context.Context, siteID uuid.UUID, siteURL string, req agentcmd.MediaCleanRequest) (agentcmd.MediaCleanResult, error)
}

// BackupChecker reports whether a site has a successful backup within a given
// lookback window. It is used by the db_table_action drop/empty handler to emit
// the advisory backup-warning (non-blocking). *backup.Service satisfies it via
// a narrow adapter wired in main.
type BackupChecker interface {
	HasRecentBackup(ctx context.Context, tenantID, siteID uuid.UUID, within time.Duration) (bool, error)
}

// SiteLookup resolves a site's agent URL (wired in main via a narrow adapter so
// this package stays free of a site import cycle).
type SiteLookup interface {
	GetSiteURL(ctx context.Context, tenantID, siteID uuid.UUID) (string, error)
	// ListSiteIDs returns all site IDs for the tenant. Used by the fleet RUM
	// aggregate endpoint to compute sites_total.
	ListSiteIDs(ctx context.Context, tenantID uuid.UUID) ([]uuid.UUID, error)
	// ListSitesMeta returns the name+url for every site in the tenant in a
	// single bulk call (GH #202). Used to enrich the fleet RUM worst-offenders
	// list, which only carries site IDs from the rollup aggregation, without
	// an N+1 GetSiteURL round-trip per offender.
	ListSitesMeta(ctx context.Context, tenantID uuid.UUID) ([]SiteMeta, error)
}

// SiteMeta is the minimal display info (name + url) for a site, returned in
// bulk by SiteLookup.ListSitesMeta.
type SiteMeta struct {
	ID   uuid.UUID
	Name string
	URL  string
}

// EventPublisher publishes perf SSE envelopes on the shared tenant bus.
type EventPublisher interface {
	Publish(ctx context.Context, ev site.ConnectionEvent) error
}

// Decryptor decrypts the age-encrypted CDN credentials. *cryptbox.AgeIdentity
// satisfies it. Decrypt is the only path that touches plaintext credentials and
// they NEVER leave the service.
type Decryptor interface {
	Decrypt(ciphertext []byte) ([]byte, error)
	Encrypt(plaintext []byte) ([]byte, error)
}

// CDNPurger purges a CDN edge cache best-effort. The concrete implementation
// (cdn.go) is SSRF-guarded. A nil purger means "no CDN integration wired".
type CDNPurger interface {
	// Purge wipes the whole zone (urls empty) or specific urls. Best-effort: an
	// error is logged by the caller and never fails the local purge.
	Purge(ctx context.Context, creds CDNCredentials, siteURL string, urls []string) error
}

// BeaconKeyRotator is the subset of *rum.BeaconKeyRepo the perf service needs
// to mint/rotate a RUM beacon key. Declared as an interface (rather than the
// concrete *rum.BeaconKeyRepo) so tests can substitute a fake without a live
// DB. *rum.BeaconKeyRepo satisfies it.
type BeaconKeyRotator interface {
	// RotateBeaconKey rotates the beacon key for a site: the existing
	// beacon_key_hash becomes beacon_key_hash_prev (grace window — in-flight
	// beacons signed with the old key still resolve via LookupRumBeaconKey)
	// and newKeyHash becomes the current hash.
	RotateBeaconKey(ctx context.Context, tenantID, siteID uuid.UUID, newKeyHash []byte) error
}

// RumBeaconReconcileEnqueuer enqueues a River job that calls
// Service.RotateBeaconKey for one site (GH #174 ack-based self-heal). The
// concrete implementation (worker.go) wraps river.Client[pgx.Tx].Insert;
// declared as an interface so tests can substitute a fake.
type RumBeaconReconcileEnqueuer interface {
	EnqueueRumBeaconReconcile(ctx context.Context, args RumBeaconReconcileArgs) error
}

// repository is the persistence surface the service needs. *Repo satisfies it;
// declared as an interface so the service is unit-testable with a fake (no DB).
type repository interface {
	GetConfig(ctx context.Context, tenantID, siteID uuid.UUID) (Config, error)
	UpsertConfig(ctx context.Context, in UpsertConfigInput) (Config, error)
	GetCDNCredentialsCiphertext(ctx context.Context, tenantID, siteID uuid.UUID) ([]byte, string, error)
	UpdateInstallState(ctx context.Context, siteID uuid.UUID, serverSoftware string, dropinInstalled, wpCacheConstantSet, htaccessManaged bool) error
	// GH #174 — ack-based RUM beacon-key presence signal. Returns
	// needsReconcile=true when the site is rum-enabled with a hash already
	// minted but the agent just reported it does not hold the key.
	UpdateBeaconKeyAcked(ctx context.Context, siteID uuid.UUID, present bool) (needsReconcile bool, err error)
	// M53 / #169 — agent-reported WooCommerce theme probe result (tri-state after M67).
	// Returns (rowsAffected, error); 0 rows means no config row exists yet for the site.
	UpdateWooFragmentsSupported(ctx context.Context, siteID uuid.UUID, supported bool) (int64, error)
	GetCacheStats(ctx context.Context, tenantID, siteID uuid.UUID) (CacheStats, error)
	UpsertCacheStats(ctx context.Context, s CacheStats) (CacheStats, error)
	MarkCachePurged(ctx context.Context, tenantID, siteID uuid.UUID, kind string) error
	RecordPurge(ctx context.Context, in RecordPurgeInput) (PurgeAuditEntry, error)
	ListPurgeAudit(ctx context.Context, tenantID, siteID uuid.UUID, limit, offset int32) ([]PurgeAuditEntry, error)
	// M38 — CP-owned db-clean scheduling.
	GetDueDBCleanSites(ctx context.Context, limit int) ([]DueDBCleanSite, error)
	UpdateNextDBCleanAt(ctx context.Context, siteID uuid.UUID, nextAt time.Time) error
	// M39 — watchdog columns for db_clean + db_scan.
	SetActiveDBCleanJob(ctx context.Context, siteID uuid.UUID, jobID string, startedAt time.Time) error
	ClearActiveDBCleanJob(ctx context.Context, siteID uuid.UUID) error
	SetActiveDBScanJob(ctx context.Context, siteID uuid.UUID, jobID string, startedAt time.Time) error
	ClearActiveDBScanJob(ctx context.Context, siteID uuid.UUID) error
	GetActiveDBScanState(ctx context.Context, tenantID, siteID uuid.UUID) (ActiveDBScanState, error)
	GetActiveDBCleanState(ctx context.Context, tenantID, siteID uuid.UUID) (ActiveDBCleanState, error)
	GetStalledDBCleanJobs(ctx context.Context, cleanThreshold time.Duration) ([]StalledDBCleanJob, error)
	GetStalledDBScanJobs(ctx context.Context, scanThreshold time.Duration) ([]StalledDBScanJob, error)
	// M39 — db_scan result persistence.
	UpsertDBScanResult(ctx context.Context, in DBScanResultInput) error
	GetDBScanResult(ctx context.Context, tenantID, siteID uuid.UUID) (DBScanResult, error)
	// M71 — db_clean result persistence.
	UpsertDBCleanResult(ctx context.Context, in DBCleanResultInput) error
	GetDBCleanResult(ctx context.Context, tenantID, siteID uuid.UUID) (DBCleanResult, error)
	// M42 — DB-size trend history (Phase 3.4).
	GetDBSizeHistory(ctx context.Context, tenantID, siteID uuid.UUID, since time.Time) ([]DbSizeTrendPoint, error)
	PruneDBSizeHistory(ctx context.Context, retention time.Duration) (int64, error)
	// M52 / #162 — cache hit-ratio history.
	InsertCacheHitRatioHistoryTx(ctx context.Context, tenantID, siteID uuid.UUID, hitCount, missCount int64, ratioPct float64, sampledAt time.Time) error
	GetCacheHitRatioHistory(ctx context.Context, tenantID, siteID uuid.UUID, since time.Time) ([]CacheHitRatioPoint, error)
	PruneCacheHitRatioHistory(ctx context.Context, retention time.Duration) (int64, error)
	// P3.7 — Fleet / Portfolio DB Health aggregate (tenant-level).
	GetFleetDbHealth(ctx context.Context, tenantID uuid.UUID, since time.Time) ([]FleetSiteDbSummary, error)
	// P3.8 — watchdog columns for db_orphan_delete.
	SetActiveDBOrphanDeleteJob(ctx context.Context, siteID uuid.UUID, jobID string, startedAt time.Time) error
	ClearActiveDBOrphanDeleteJob(ctx context.Context, siteID uuid.UUID) error
	GetStalledDBOrphanDeleteJobs(ctx context.Context, threshold time.Duration) ([]StalledDBOrphanDeleteJob, error)
}

var _ repository = (*Repo)(nil)

// Service orchestrates the Performance Suite control plane.
type Service struct {
	repo          repository
	agent         AgentPerfClient
	sites         SiteLookup
	events        EventPublisher
	decryptor     Decryptor
	cdn           CDNPurger
	backupChecker BackupChecker
	logger        *slog.Logger
	// beaconKeyRepo is the RUM beacon-key persistence layer. Used during
	// UpdateConfig to generate/rotate the key when RUM is first enabled, and
	// unconditionally by RotateBeaconKey.
	// nil = RUM beacon-key management disabled (tests/degraded mode).
	beaconKeyRepo BeaconKeyRotator
	// cpBaseURL is the control-plane public base URL (e.g.
	// "https://manage.example.com"). Used to derive the RUM ingest URL that is
	// pushed to the agent as rum_ingest_url.
	cpBaseURL string
	// rumReconcileEnqueuer enqueues the GH #174 ack-based beacon-key reconcile
	// job. nil = the ack-based self-heal is disabled (the mint gate and the
	// operator rotate-key endpoint still work; only the automatic "an ack just
	// told us the agent lost the key" retry is skipped).
	rumReconcileEnqueuer RumBeaconReconcileEnqueuer
}

// NewService builds the perf service. agent/sites/events/decryptor/cdn may be nil
// in degraded environments; the service degrades to domain errors (never panics).
func NewService(repo repository, decryptor Decryptor, events EventPublisher, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{repo: repo, decryptor: decryptor, events: events, logger: logger}
}

// SetAgentClient wires the agent command client + the site URL lookup.
func (s *Service) SetAgentClient(agent AgentPerfClient, sites SiteLookup) {
	s.agent = agent
	s.sites = sites
}

// SetCDNPurger wires the SSRF-guarded CDN purger.
func (s *Service) SetCDNPurger(p CDNPurger) { s.cdn = p }

// SetBackupChecker wires the backup recency checker for the drop/empty advisory
// backup-warning nudge (non-blocking).
func (s *Service) SetBackupChecker(b BackupChecker) { s.backupChecker = b }

// SetBeaconKeyRepo wires the RUM beacon-key repository and the CP public base
// URL used to derive rum_ingest_url in the agent push payload.
// Call this from main after creating the service; nil disables RUM beacon
// provisioning (the endpoint still works but keys are never generated).
func (s *Service) SetBeaconKeyRepo(repo BeaconKeyRotator, cpBaseURL string) {
	s.beaconKeyRepo = repo
	s.cpBaseURL = cpBaseURL
}

// SetRumBeaconReconcileEnqueuer wires the GH #174 ack-based reconcile job
// enqueuer. Call this from main after River has started; nil disables the
// automatic self-heal (MarkConfigApplied logs and skips instead of enqueuing).
func (s *Service) SetRumBeaconReconcileEnqueuer(e RumBeaconReconcileEnqueuer) {
	s.rumReconcileEnqueuer = e
}

// ---------------------------------------------------------------------------
// config
// ---------------------------------------------------------------------------

// validJSDelayMethods are the accepted js_delay_method values.
var validJSDelayMethods = map[string]bool{"defer": true, "async": true, "interaction": true}

// validRefreshIntervals are accepted cache_refresh_interval values.
var validRefreshIntervals = map[string]bool{
	"30min": true, "1hour": true, "2hours": true, "6hours": true,
	"12hours": true, "daily": true, "weekly": true,
}

// validCleanIntervals are accepted db_auto_clean_interval values.
var validCleanIntervals = map[string]bool{"daily": true, "weekly": true, "monthly": true}

// validCDNProviders are accepted cdn_provider values.
var validCDNProviders = map[string]bool{"cloudflare": true, "bunny": true, "keycdn": true}

// validCDNFileTypes are accepted cdn_file_types values.
var validCDNFileTypes = map[string]bool{"all": true, "images": true, "css_js": true}

// GetConfig returns the stored config, or the zero/default config when no row
// exists yet. CDN credentials are never included.
func (s *Service) GetConfig(ctx context.Context, tenantID, siteID uuid.UUID) (Config, error) {
	cfg, err := s.repo.GetConfig(ctx, tenantID, siteID)
	if err == ErrNotFound {
		return defaultConfig(tenantID, siteID), nil
	}
	if err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// UpdateConfigInput is the validated operator config change. CDNCredentialsRaw,
// when non-nil, is fresh plaintext credentials to encrypt+store; nil leaves the
// stored ciphertext unchanged.
type UpdateConfigInput struct {
	Config            Config
	CDNCredentialsRaw *CDNCredentials
}

// UpdateConfig validates + persists the config, bumps config_version, and pushes
// perf.config.update to the agent (best-effort). Returns the stored config. The
// agent push failure is surfaced as a non-domain error so the handler can store
// + warn (mirrors security.SaveConfig).
func (s *Service) UpdateConfig(ctx context.Context, tenantID, siteID uuid.UUID, in UpdateConfigInput) (Config, error) {
	cfg := in.Config
	cfg.TenantID = tenantID
	cfg.SiteID = siteID

	if err := s.validateConfig(&cfg); err != nil {
		return Config{}, err
	}

	// Resolve the CDN credentials ciphertext to persist. The m36 UpsertPerfConfig
	// always writes the column, so when the operator did NOT supply new
	// credentials we must pass the existing ciphertext through unchanged.
	ciphertext, credErr := s.resolveCDNCiphertext(ctx, tenantID, siteID, cfg, in.CDNCredentialsRaw)
	if credErr != nil {
		return Config{}, credErr
	}

	// Bump config_version monotonically so a stale agent can detect it.
	prev, _ := s.repo.GetConfig(ctx, tenantID, siteID)
	cfg.ConfigVersion = prev.ConfigVersion + 1

	saved, err := s.repo.UpsertConfig(ctx, UpsertConfigInput{Config: cfg, CDNCredentialsEncrypted: ciphertext})
	if err != nil {
		return Config{}, err
	}

	s.publish(ctx, tenantID, siteID, site.EventPerfConfigUpdated, map[string]any{
		"config_version": saved.ConfigVersion,
		"cache_enabled":  saved.CacheEnabled,
	})

	// M56 — RUM beacon-key provisioning (best-effort, config already persisted).
	// When RUM is enabled and either (a) no beacon key exists for the site yet,
	// OR (b) the agent's most recent config-ack said it does NOT hold the key
	// (GH #174 — the one best-effort mint+push on first-enable was lost, e.g.
	// the agent was down), (re-)generate one now and capture the plaintext so
	// it can be sent to the agent in this push. On a healthy subsequent save
	// (key already set AND already acked present) the plaintext is never
	// re-derivable, so we send an empty string and the agent retains its copy —
	// this is what makes an operator's routine config save NOT churn the key on
	// every single save.
	var freshBeaconKey string // non-empty only when freshly generated/rotated
	if saved.RumEnabled && s.beaconKeyRepo != nil && (!saved.BeaconKeySet || !saved.BeaconKeyAckedPresent) {
		pt, mintErr := s.mintAndRotateBeaconKey(ctx, tenantID, siteID)
		if mintErr == nil {
			freshBeaconKey = pt
			// Mark BeaconKeySet true so toPerfConfigRequest can reflect the correct
			// state without an extra DB round-trip.
			saved.BeaconKeySet = true
		} else {
			s.logger.Warn("rum: beacon key mint/rotate failed", slog.Any("error", mintErr))
		}
	}

	// Push to the agent (best-effort, config already persisted).
	if s.agent != nil && s.sites != nil {
		siteURL, lookupErr := s.sites.GetSiteURL(ctx, tenantID, siteID)
		if lookupErr == nil {
			req := toPerfConfigRequest(saved, freshBeaconKey, s.cpBaseURL)
			res, pushErr := s.agent.SyncPerfConfig(ctx, siteID, siteURL, req)
			if pushErr != nil {
				return saved, fmt.Errorf("config stored but agent push failed: %w", pushErr)
			}
			// Refresh the verify card from the state the agent observed while
			// applying the config (drop-in / WP_CACHE / .htaccess / server).
			_ = s.repo.UpdateInstallState(ctx, siteID, res.ServerSoftware, res.DropinInstalled, res.WPCacheConstantSet, res.HtaccessManaged)
		}
	}
	return saved, nil
}

// mintAndRotateBeaconKey generates a fresh RUM beacon key, rotates the stored
// hash (the existing beacon_key_hash moves to beacon_key_hash_prev for the
// grace window so any in-flight beacon signed with the old key still resolves
// via LookupRumBeaconKey), and returns the plaintext. Returns ("", err) on any
// failure — generation or persistence — never a partial success. Callers
// decide whether a failure here is fatal (RotateBeaconKey, the reconcile
// worker) or best-effort (UpdateConfig's opportunistic mint, which logs and
// continues so a beacon-key hiccup never blocks an otherwise-valid config
// save).
func (s *Service) mintAndRotateBeaconKey(ctx context.Context, tenantID, siteID uuid.UUID) (plaintext string, err error) {
	if s.beaconKeyRepo == nil {
		return "", fmt.Errorf("rum beacon-key repo not wired")
	}
	pt, keyHash, genErr := rum.GenerateBeaconKey()
	if genErr != nil {
		return "", fmt.Errorf("generate beacon key: %w", genErr)
	}
	if rotErr := s.beaconKeyRepo.RotateBeaconKey(ctx, tenantID, siteID, keyHash); rotErr != nil {
		return "", fmt.Errorf("rotate beacon key: %w", rotErr)
	}
	return pt, nil
}

// RotateBeaconKey unconditionally mints a fresh RUM beacon key for a site and
// pushes it to the agent (GH #174). Unlike UpdateConfig's opportunistic mint
// (which only fires on first-enable, or when the ack signal says the agent
// lost its copy), this ALWAYS rotates. It is BOTH:
//
//  1. The deterministic operator escape hatch behind
//     POST /sites/{siteId}/perf/rum/rotate-key — lets an operator force a
//     fresh mint+push on demand instead of editing the database directly.
//  2. The primitive the ack-based RumBeaconReconcileWorker calls when a
//     config-ack reports the agent does not hold the key it should.
//
// The plaintext is sent to the agent in this ONE push only; RotateBeaconKey
// itself never returns it to the caller, logs it, or stores it anywhere but
// the (never-persisted) local variable used to build the push payload.
func (s *Service) RotateBeaconKey(ctx context.Context, tenantID, siteID uuid.UUID) error {
	if s.beaconKeyRepo == nil {
		return domain.ServiceUnavailable("rum_beacon_repo_unwired", "RUM beacon-key management is not configured")
	}
	// Fail fast on every precondition that would prevent the push BEFORE
	// touching the hash: rotating a key we already know we cannot deliver
	// would recreate the exact "hash committed, plaintext never delivered"
	// stuck state this method exists to fix — deterministically instead of via
	// a race.
	if s.agent == nil || s.sites == nil {
		return domain.ServiceUnavailable("agent_unwired", "agent client not configured")
	}
	cfg, err := s.repo.GetConfig(ctx, tenantID, siteID)
	if err != nil {
		if err == ErrNotFound {
			return domain.NotFound("perf_config_not_found", "no performance config exists for this site yet")
		}
		return err
	}
	siteURL, lookupErr := s.sites.GetSiteURL(ctx, tenantID, siteID)
	if lookupErr != nil {
		return lookupErr
	}

	freshKey, mintErr := s.mintAndRotateBeaconKey(ctx, tenantID, siteID)
	if mintErr != nil {
		return fmt.Errorf("rotate beacon key: %w", mintErr)
	}
	cfg.BeaconKeySet = true

	// Bump config_version so the push can never be mistaken for a repeat of the
	// last version the agent already applied. Carry the existing CDN ciphertext
	// forward unchanged (UpsertPerfConfig always writes the column — mirrors
	// EnableCache/DisableCache exactly).
	cfg.ConfigVersion++
	ct, _, _ := s.repo.GetCDNCredentialsCiphertext(ctx, tenantID, siteID)
	saved, err := s.repo.UpsertConfig(ctx, UpsertConfigInput{Config: cfg, CDNCredentialsEncrypted: ct})
	if err != nil {
		return err
	}

	res, pushErr := s.agent.SyncPerfConfig(ctx, siteID, siteURL, toPerfConfigRequest(saved, freshKey, s.cpBaseURL))
	if pushErr != nil {
		// The hash is already rotated + persisted; the push failure is surfaced
		// as a non-domain error so the caller (handler or River worker) can
		// decide how to react — mirrors UpdateConfig's "config stored but agent
		// push failed" pattern exactly. A subsequent rotate-key call or the
		// next ack-based reconcile pass will retry.
		return fmt.Errorf("beacon key rotated but agent push failed: %w", pushErr)
	}
	_ = s.repo.UpdateInstallState(ctx, siteID, res.ServerSoftware, res.DropinInstalled, res.WPCacheConstantSet, res.HtaccessManaged)
	return nil
}

// resolveCDNCiphertext encrypts fresh credentials, or returns the prior stored
// ciphertext when none supplied (so the UPSERT never blanks them out).
func (s *Service) resolveCDNCiphertext(ctx context.Context, tenantID, siteID uuid.UUID, cfg Config, raw *CDNCredentials) ([]byte, error) {
	if raw != nil {
		if s.decryptor == nil {
			return nil, domain.ServiceUnavailable("perf_crypto_unwired", "credential encryption is not configured")
		}
		raw.Provider = cfg.CDNProvider
		plain, err := json.Marshal(raw)
		if err != nil {
			return nil, domain.Internal("perf_cred_marshal", "failed to marshal credentials")
		}
		ct, err := s.decryptor.Encrypt(plain)
		if err != nil {
			return nil, domain.Internal("perf_cred_encrypt", "failed to encrypt credentials")
		}
		return ct, nil
	}
	// No new creds: carry forward the existing ciphertext.
	prior, _, err := s.repo.GetCDNCredentialsCiphertext(ctx, tenantID, siteID)
	if err != nil {
		return nil, err
	}
	return prior, nil
}

// validateConfig validates + normalizes the config in place.
func (s *Service) validateConfig(cfg *Config) error {
	if cfg.CacheRefreshInterval == "" {
		cfg.CacheRefreshInterval = "2hours"
	}
	if !validRefreshIntervals[cfg.CacheRefreshInterval] {
		return domain.Validation("invalid_cache_refresh_interval", fmt.Sprintf("cache_refresh_interval %q is not allowed", cfg.CacheRefreshInterval))
	}
	if cfg.JSDelayMethod == "" {
		cfg.JSDelayMethod = "defer"
	}
	if !validJSDelayMethods[cfg.JSDelayMethod] {
		return domain.Validation("invalid_js_delay_method", fmt.Sprintf("js_delay_method %q is not allowed", cfg.JSDelayMethod))
	}
	if cfg.DBAutoCleanInterval == "" {
		cfg.DBAutoCleanInterval = "weekly"
	}
	if !validCleanIntervals[cfg.DBAutoCleanInterval] {
		return domain.Validation("invalid_db_clean_interval", fmt.Sprintf("db_auto_clean_interval %q is not allowed", cfg.DBAutoCleanInterval))
	}
	if cfg.CDNFileTypes == "" {
		cfg.CDNFileTypes = "all"
	}
	if !validCDNFileTypes[cfg.CDNFileTypes] {
		return domain.Validation("invalid_cdn_file_types", fmt.Sprintf("cdn_file_types %q is not allowed", cfg.CDNFileTypes))
	}
	if cfg.CDNEnabled {
		if cfg.CDNURL == "" {
			return domain.Validation("invalid_cdn_url", "cdn_url is required when cdn_enabled is true")
		}
		if u, err := url.Parse(cfg.CDNURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return domain.Validation("invalid_cdn_url", "cdn_url must be a valid http(s) URL")
		}
	}
	if cfg.CDNProvider != "" && !validCDNProviders[cfg.CDNProvider] {
		return domain.Validation("invalid_cdn_provider", fmt.Sprintf("cdn_provider %q is not allowed", cfg.CDNProvider))
	}
	cfg.CacheBypassURLs = normalize(cfg.CacheBypassURLs)
	cfg.CacheBypassCookies = normalize(cfg.CacheBypassCookies)
	cfg.CacheIncludeQueries = normalize(cfg.CacheIncludeQueries)
	cfg.CacheIncludeCookies = normalize(cfg.CacheIncludeCookies)
	cfg.CSSRucssIncludeSelectors = normalize(cfg.CSSRucssIncludeSelectors)
	cfg.JSDelayExcludes = normalize(cfg.JSDelayExcludes)
	cfg.JSDelayThirdPartyExcludes = normalize(cfg.JSDelayThirdPartyExcludes)
	cfg.LazyLoadExclusions = normalize(cfg.LazyLoadExclusions)
	// Preload throttle (M37): clamp (do not reject) out-of-range knobs to the
	// nearest bound for forward-compat. These mirror the agent's local clamps.
	cfg.PreloadConcurrency = clampInt(cfg.PreloadConcurrency, 1, 4)
	cfg.PreloadDelayMs = clampInt(cfg.PreloadDelayMs, 0, 10000)
	cfg.PreloadBatchSize = clampInt(cfg.PreloadBatchSize, 1, 500)
	cfg.PreloadMaxLoad = clampFloat(cfg.PreloadMaxLoad, 0, 64)
	return nil
}

// clampInt returns v constrained to the inclusive [lo, hi] range.
func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// clampFloat returns v constrained to the inclusive [lo, hi] range.
func clampFloat(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// ---------------------------------------------------------------------------
// cache stats
// ---------------------------------------------------------------------------

// GetCacheStats returns the latest reported gauges, or a zero-valued (but
// non-error) stats struct when the agent has not reported yet.
func (s *Service) GetCacheStats(ctx context.Context, tenantID, siteID uuid.UUID) (CacheStats, error) {
	stats, err := s.repo.GetCacheStats(ctx, tenantID, siteID)
	if err == ErrNotFound {
		return CacheStats{SiteID: siteID, TenantID: tenantID}, nil
	}
	if err != nil {
		return CacheStats{}, err
	}
	return stats, nil
}

// ReportCacheStats persists the agent-reported gauges (InAgentTx) and emits
// cache.stats.updated. Called from the agent-facing endpoint; tenant + site come
// from the verified agent identity (the handler asserts the binding).
//
// M52 / #162: when the report carries a non-zero window delta
// (CacheHitCount + CacheMissCount > 0), one hit-ratio history row is appended
// under a separate InTenantTx. The two transactions are both short and
// independent; a history-insert failure is logged but does NOT fail the gauge
// upsert (the gauges are the primary signal; history is best-effort).
func (s *Service) ReportCacheStats(ctx context.Context, stats CacheStats) (CacheStats, error) {
	saved, err := s.repo.UpsertCacheStats(ctx, stats)
	if err != nil {
		return CacheStats{}, err
	}
	// Append a hit-ratio history point when the agent supplies a non-zero
	// window delta. Both counts must be present (sum > 0) to form a meaningful
	// ratio; skip silently when the agent omits them (older agents, or a window
	// where no requests were served).
	if stats.CacheHitCount+stats.CacheMissCount > 0 {
		total := stats.CacheHitCount + stats.CacheMissCount
		ratioPct := math.Round(float64(stats.CacheHitCount)/float64(total)*100*100) / 100
		sampledAt := time.Now().UTC()
		if herr := s.repo.InsertCacheHitRatioHistoryTx(ctx, saved.TenantID, saved.SiteID, stats.CacheHitCount, stats.CacheMissCount, ratioPct, sampledAt); herr != nil {
			s.logger.Warn("cache hit-ratio history insert failed",
				slog.String("site_id", saved.SiteID.String()),
				slog.Any("error", herr),
			)
		}
	}
	s.publish(ctx, saved.TenantID, saved.SiteID, site.EventCacheStatsUpdated, map[string]any{
		"cached_pages_count": saved.CachedPagesCount,
		"cache_size_bytes":   saved.CacheSizeBytes,
		"preload_pending":    saved.PreloadPending,
		"preload_total":      saved.PreloadTotal,
	})
	// Derive the preload live-progress frames from the reported gauges so the
	// dashboard's Preload bar advances and (crucially) terminates. The agent
	// reports {preload_total, preload_pending} from its warm cron; total>0 means
	// a warm pass is in flight or just finished. This is what stops the loader
	// from "spinning forever" — nothing else publishes these two events.
	if saved.PreloadTotal > 0 {
		done := saved.PreloadTotal - saved.PreloadPending
		if done < 0 {
			done = 0
		}
		s.publish(ctx, saved.TenantID, saved.SiteID, site.EventCachePreloadProgress, map[string]any{
			"done": done, "total": saved.PreloadTotal,
		})
		if saved.PreloadPending == 0 {
			s.publish(ctx, saved.TenantID, saved.SiteID, site.EventCachePreloadCompleted, map[string]any{
				"done": saved.PreloadTotal, "total": saved.PreloadTotal,
			})
		}
	}
	return saved, nil
}

// MarkConfigApplied records the agent's install-state report (InAgentTx) and,
// when rumBeaconPresent is non-nil, the GH #174 ack-based beacon-key presence
// signal. Called from the perf/config-ack agent endpoint.
//
// rumBeaconPresent is nil when the agent omitted the field (pre-#174 agents,
// or any agent that does not report it) — treated as "no signal", never as
// "absent", so an old agent can never spuriously trigger the reconcile churn
// below. When non-nil, false on a site that is rum-enabled with a hash
// already minted enqueues a RumBeaconReconcileWorker job that calls
// RotateBeaconKey — the systemic self-heal that makes the "hash committed,
// plaintext never delivered/lost" stuck state unable to persist past one ack
// cycle, instead of requiring an operator to notice zero RUM samples and
// intervene (or wait for their next unrelated config save to happen to
// re-trigger the mint gate).
func (s *Service) MarkConfigApplied(ctx context.Context, tenantID, siteID uuid.UUID, serverSoftware string, dropinInstalled, wpCacheConstantSet, htaccessManaged bool, rumBeaconPresent *bool) error {
	if err := s.repo.UpdateInstallState(ctx, siteID, serverSoftware, dropinInstalled, wpCacheConstantSet, htaccessManaged); err != nil {
		return err
	}
	if rumBeaconPresent == nil {
		return nil
	}
	needsReconcile, ackErr := s.repo.UpdateBeaconKeyAcked(ctx, siteID, *rumBeaconPresent)
	if ackErr != nil {
		// Best-effort: the ack signal is a self-heal aid, not a correctness
		// requirement, and must never fail the ack response the agent is
		// waiting on. A missing config row here would be surprising immediately
		// after UpdateInstallState succeeded on the same row, but is handled
		// the same tolerant way regardless of cause.
		s.logger.Warn("rum: failed to record beacon-key ack",
			slog.String("site_id", siteID.String()), slog.Any("error", ackErr))
		return nil
	}
	if needsReconcile && s.rumReconcileEnqueuer != nil {
		if enqErr := s.rumReconcileEnqueuer.EnqueueRumBeaconReconcile(ctx, RumBeaconReconcileArgs{TenantID: tenantID, SiteID: siteID}); enqErr != nil {
			s.logger.Warn("rum: failed to enqueue beacon reconcile",
				slog.String("site_id", siteID.String()), slog.Any("error", enqErr))
		}
	}
	return nil
}

// MarkWooFragmentsSupported records the agent-reported WooCommerce theme-probe
// result (InAgentTx). Called from the stats-report agent endpoint whenever the
// agent probes and reports woo_theme_fragments_supported. This is READ-ONLY from
// the operator API; only the agent writes it. A 0-row result (no config row yet
// for the site) is logged at debug level and treated as a no-op — the agent will
// re-report on its next heartbeat after the operator saves a config.
func (s *Service) MarkWooFragmentsSupported(ctx context.Context, siteID uuid.UUID, supported bool) error {
	n, err := s.repo.UpdateWooFragmentsSupported(ctx, siteID, supported)
	if err != nil {
		return err
	}
	if n == 0 {
		s.logger.Debug("woo fragments probe: no config row yet for site (no-op)",
			slog.String("site_id", siteID.String()),
			slog.Bool("supported", supported))
	}
	return nil
}

// ---------------------------------------------------------------------------
// enable / disable
// ---------------------------------------------------------------------------

// EnableCache flips cache_enabled on, pushes cache.enable to the agent, and emits
// cache.enabled. Returns the agent detail. A persisted config row is required;
// when absent we seed a default first.
func (s *Service) EnableCache(ctx context.Context, tenantID, siteID uuid.UUID) (string, error) {
	cfg, err := s.GetConfig(ctx, tenantID, siteID)
	if err != nil {
		return "", err
	}
	cfg.CacheEnabled = true
	cfg.ConfigVersion = cfg.ConfigVersion + 1
	ct, _, _ := s.repo.GetCDNCredentialsCiphertext(ctx, tenantID, siteID)
	if _, err := s.repo.UpsertConfig(ctx, UpsertConfigInput{Config: cfg, CDNCredentialsEncrypted: ct}); err != nil {
		return "", err
	}
	detail := "cache enabled"
	if s.agent != nil && s.sites != nil {
		siteURL, lookupErr := s.sites.GetSiteURL(ctx, tenantID, siteID)
		if lookupErr != nil {
			return "", lookupErr
		}
		res, perr := s.agent.CacheEnable(ctx, siteID, siteURL, agentcmd.CacheEnableRequest{ConfigVersion: cfg.ConfigVersion})
		if perr != nil {
			return "", perr
		}
		detail = res.Detail
		// Record the REAL install-state the agent reported (drop-in present,
		// WP_CACHE define written, .htaccess block managed). Previously
		// wp_cache_constant_set was hardcoded false, so the dashboard's verify
		// card always read "not set" even when the define was written.
		_ = s.repo.UpdateInstallState(ctx, siteID, res.ServerSoftware, res.DropinInstalled, res.WPCacheConstantSet, res.HtaccessManaged)

		// Push the FULL perf config so the optimization flags (minify, lazy-load,
		// font-swap, …) actually land in the agent's wpmgr_perf_config option.
		// cache_enable alone only writes WP_CACHE + the drop-in/.htaccess; without
		// this push the request-path optimizer stays inert (no minify) because its
		// flags default off. Best-effort: caching is already on if this fails.
		// Note: no fresh beacon key is generated here — beacon-key provisioning
		// only happens in UpdateConfig where the operator explicitly enables RUM.
		if _, pushErr := s.agent.SyncPerfConfig(ctx, siteID, siteURL, toPerfConfigRequest(cfg, "", s.cpBaseURL)); pushErr != nil {
			s.logger.Warn("cache enabled but optimize-config push failed",
				slog.String("site_id", siteID.String()), slog.Any("error", pushErr))
		}
	}
	s.publish(ctx, tenantID, siteID, site.EventCacheEnabled, map[string]any{"config_version": cfg.ConfigVersion})
	return detail, nil
}

// DisableCache flips cache_enabled off, pushes cache.disable, and emits
// cache.disabled.
func (s *Service) DisableCache(ctx context.Context, tenantID, siteID uuid.UUID) (string, error) {
	cfg, err := s.GetConfig(ctx, tenantID, siteID)
	if err != nil {
		return "", err
	}
	cfg.CacheEnabled = false
	cfg.ConfigVersion = cfg.ConfigVersion + 1
	ct, _, _ := s.repo.GetCDNCredentialsCiphertext(ctx, tenantID, siteID)
	if _, err := s.repo.UpsertConfig(ctx, UpsertConfigInput{Config: cfg, CDNCredentialsEncrypted: ct}); err != nil {
		return "", err
	}
	detail := "cache disabled"
	if s.agent != nil && s.sites != nil {
		siteURL, lookupErr := s.sites.GetSiteURL(ctx, tenantID, siteID)
		if lookupErr != nil {
			return "", lookupErr
		}
		res, perr := s.agent.CacheDisable(ctx, siteID, siteURL, agentcmd.CacheDisableRequest{})
		if perr != nil {
			return "", perr
		}
		detail = res.Detail
	}
	s.publish(ctx, tenantID, siteID, site.EventCacheDisabled, map[string]any{"config_version": cfg.ConfigVersion})
	return detail, nil
}

// ---------------------------------------------------------------------------
// purge / preload
// ---------------------------------------------------------------------------

// PurgeInput is one purge request.
type PurgeInput struct {
	Scope       PurgeKind // PurgeKindAll | PurgeKindURL
	URLs        []string
	InitiatorID uuid.UUID // operator user id (uuid.Nil for system)
	// DeleteEverything marks the destructive "delete the whole cache directory"
	// flavour. It still maps to a scope=all agent purge but is recorded and gated
	// separately (PermSiteCacheDeleteAll) by the caller.
	DeleteEverything bool
}

// Purge orchestrates a cache purge: record cache_purge_audit, emit
// cache.purge.started, push cache.purge to the agent, purge the CDN if
// configured (best-effort), then emit cache.purge.completed. The audit record is
// written BEFORE the agent call so a failed agent purge is still attributable.
func (s *Service) Purge(ctx context.Context, tenantID, siteID uuid.UUID, in PurgeInput) (PurgeAuditEntry, string, error) {
	if in.Scope != PurgeKindAll && in.Scope != PurgeKindURL {
		return PurgeAuditEntry{}, "", domain.Validation("invalid_scope", "scope must be 'all' or 'url'")
	}
	if in.Scope == PurgeKindURL && len(in.URLs) == 0 {
		return PurgeAuditEntry{}, "", domain.Validation("missing_urls", "url scope requires at least one url")
	}

	entry, err := s.repo.RecordPurge(ctx, RecordPurgeInput{
		TenantID:        tenantID,
		SiteID:          siteID,
		Kind:            in.Scope,
		InitiatorUserID: in.InitiatorID,
		TargetURLs:      in.URLs,
	})
	if err != nil {
		return PurgeAuditEntry{}, "", err
	}

	s.publish(ctx, tenantID, siteID, site.EventCachePurgeStarted, map[string]any{
		"kind":              string(in.Scope),
		"urls_count":        len(in.URLs),
		"delete_everything": in.DeleteEverything,
	})

	detail := "purge requested"
	if s.agent != nil && s.sites != nil {
		siteURL, lookupErr := s.sites.GetSiteURL(ctx, tenantID, siteID)
		if lookupErr != nil {
			return entry, "", lookupErr
		}
		purgeReq := agentcmd.CachePurgeRequest{Scope: string(in.Scope), URLs: in.URLs}
		// The agent reads the singular `url` for scope=url; set it so a targeted
		// purge actually removes the file (the operator "Purge URL" path).
		if in.Scope == PurgeKindURL && len(in.URLs) > 0 {
			purgeReq.URL = in.URLs[0]
		}
		res, perr := s.agent.CachePurge(ctx, siteID, siteURL, purgeReq)
		if perr != nil {
			return entry, "", perr
		}
		detail = res.Detail
		// CDN purge (best-effort): only when a provider + credentials are configured.
		s.maybePurgeCDN(ctx, tenantID, siteID, siteURL, in.URLs)
	}

	// Stamp the "Last purge" dashboard gauge now. The agent's periodic stats push
	// never reports a purge time, so this is the gauge's writer for operator
	// purges; UpsertCacheStats uses GREATEST so a later agent push cannot wipe it.
	// Best-effort: the purge already succeeded, so a stamp failure must not fail it.
	if err := s.repo.MarkCachePurged(ctx, tenantID, siteID, string(in.Scope)); err != nil {
		s.logger.Warn("purge: failed to stamp last_purged_at gauge",
			"site_id", siteID, "kind", string(in.Scope), "error", err)
	}

	s.publish(ctx, tenantID, siteID, site.EventCachePurgeCompleted, map[string]any{
		"kind":       string(in.Scope),
		"urls_count": len(in.URLs),
	})
	return entry, detail, nil
}

// maybePurgeCDN decrypts the stored CDN credentials and purges the edge cache,
// best-effort. Any failure is logged and swallowed — it must NEVER fail the
// local origin purge.
func (s *Service) maybePurgeCDN(ctx context.Context, tenantID, siteID uuid.UUID, siteURL string, urls []string) {
	if s.cdn == nil || s.decryptor == nil {
		return
	}
	ct, provider, err := s.repo.GetCDNCredentialsCiphertext(ctx, tenantID, siteID)
	if err != nil || len(ct) == 0 || provider == "" {
		return
	}
	plain, err := s.decryptor.Decrypt(ct)
	if err != nil {
		s.logger.Warn("cdn purge: decrypt credentials failed", slog.String("site_id", siteID.String()), slog.Any("error", err))
		return
	}
	var creds CDNCredentials
	if err := json.Unmarshal(plain, &creds); err != nil {
		s.logger.Warn("cdn purge: unmarshal credentials failed", slog.String("site_id", siteID.String()))
		return
	}
	if creds.Provider == "" {
		creds.Provider = provider
	}
	if err := s.cdn.Purge(ctx, creds, siteURL, urls); err != nil {
		s.logger.Warn("cdn purge failed (best-effort)",
			slog.String("site_id", siteID.String()),
			slog.String("provider", creds.Provider),
			slog.Any("error", err))
	}
}

// Preload starts a background warm pass: record (kind=preload), emit
// cache.preload.started, push cache.preload to the agent. Progress + completion
// arrive later via the agent's stats reports.
func (s *Service) Preload(ctx context.Context, tenantID, siteID uuid.UUID, initiatorID uuid.UUID) (string, error) {
	if _, err := s.repo.RecordPurge(ctx, RecordPurgeInput{
		TenantID:        tenantID,
		SiteID:          siteID,
		Kind:            PurgeKindPreload,
		InitiatorUserID: initiatorID,
	}); err != nil {
		return "", err
	}
	detail := "preload requested"
	total := 0
	if s.agent != nil && s.sites != nil {
		siteURL, lookupErr := s.sites.GetSiteURL(ctx, tenantID, siteID)
		if lookupErr != nil {
			return "", lookupErr
		}
		// Mode "full" ⇒ the agent enumerates every cacheable URL (all public post
		// types incl. products, taxonomies, authors) and warms desktop + mobile.
		res, perr := s.agent.CachePreload(ctx, siteID, siteURL, agentcmd.CachePreloadRequest{Mode: "full"})
		if perr != nil {
			return "", perr
		}
		detail = res.Detail
		total = res.Total
	}
	// Publish `started` AFTER the agent call so it carries the REAL denominator the
	// dashboard's live progress bar needs. Previously this fired before the call
	// with an empty map (total:0), leaving the bar indeterminate with no percent.
	s.publish(ctx, tenantID, siteID, site.EventCachePreloadStarted, map[string]any{"total": total})
	return detail, nil
}

// ComputeRucss triggers an operator-initiated Remove-Unused-CSS computation. It
// pushes rucss_compute to the agent, which self-fetches the target URL(s)
// out-of-band (same-host only) so the request-path optimizer runs the RUCSS stage
// and posts each page to the CP — enqueuing a compute job. The
// queued → computing → completed lifecycle is streamed over the rucss.* SSE
// events. urls empty ⇒ the agent computes the home page. RUCSS is otherwise
// passive (visitor-driven), so this is the operator's deterministic trigger.
func (s *Service) ComputeRucss(ctx context.Context, tenantID, siteID uuid.UUID, urls []string) (string, error) {
	if s.agent == nil || s.sites == nil {
		return "", domain.ServiceUnavailable("agent_unwired", "agent client not configured")
	}
	siteURL, err := s.sites.GetSiteURL(ctx, tenantID, siteID)
	if err != nil {
		return "", err
	}
	// Purge the target URLs FIRST (best-effort). On hosts with a static-file
	// fast-path — e.g. the nginx `try_files` snippet that serves the cached .gz
	// directly, or a host-level page cache — an already-cached URL is served
	// WITHOUT invoking PHP, so the agent's compute self-fetch would never reach
	// the request-path optimizer + RUCSS stage. Deleting the cache file makes the
	// next render fall through to PHP, where the optimizer runs and posts the page
	// for RUCSS. Empty urls ⇒ the home page (what the agent computes by default).
	purgeURLs := urls
	if len(purgeURLs) == 0 {
		purgeURLs = []string{siteURL}
	}
	// The agent's cache_purge reads the SINGULAR `url` for scope=url, so purge
	// each target URL individually (best-effort; a purge failure must not block
	// the compute).
	for _, u := range purgeURLs {
		if _, perr := s.agent.CachePurge(ctx, siteID, siteURL, agentcmd.CachePurgeRequest{Scope: "url", URL: u}); perr != nil {
			s.logger.Warn("rucss compute: pre-purge failed (continuing)",
				slog.String("site_id", siteID.String()), slog.String("url", u), slog.Any("error", perr))
		}
	}
	res, perr := s.agent.RucssCompute(ctx, siteID, siteURL, agentcmd.RucssComputeRequest{URLs: urls})
	if perr != nil {
		return "", perr
	}
	return res.Detail, nil
}

// ---------------------------------------------------------------------------
// db clean
// ---------------------------------------------------------------------------

// dbCleanTasksFromConfig translates the operator's per-flag config into the
// 14-standard category id strings. The CP is the authoritative source of which
// tasks run; the agent never reads its local PerfConfig flags on the command path.
func dbCleanTasksFromConfig(cfg Config) []string {
	tasks := make([]string, 0, 7)
	if cfg.DBPostRevisions {
		tasks = append(tasks, "revisions")
	}
	if cfg.DBPostAutoDrafts {
		tasks = append(tasks, "auto_drafts")
	}
	if cfg.DBPostTrashed {
		tasks = append(tasks, "trashed_posts")
	}
	if cfg.DBCommentsSpam {
		tasks = append(tasks, "spam_comments")
	}
	if cfg.DBCommentsTrashed {
		tasks = append(tasks, "trashed_comments")
	}
	if cfg.DBTransientsExpired {
		tasks = append(tasks, "expired_transients")
	}
	if cfg.DBOptimizeTables {
		tasks = append(tasks, "optimize_tables")
	}
	return tasks
}

// DBClean runs an ad-hoc (operator-triggered) database cleanup scoped to the
// per-site db_* config flags.
//
// Flow (M38 async model):
//  1. Translate db_* config flags into the tasks []string.
//  2. Emit db.clean.started SSE (so the UI can show the in-progress state).
//  3. POST the db_clean command to the agent; agent ACKs immediately with
//     {ok, job_id} then runs async, posting per-category results to
//     progress_endpoint.
//  4. On ok=false: emit db.clean.failed SSE; return the agent's detail as error.
//  5. On ok=true: return — the agent drives completion via progress pushes.
//
// Returns the job_id minted by this call (for correlation) and any error.
func (s *Service) DBClean(ctx context.Context, tenantID, siteID uuid.UUID, cpBaseURL string) (jobID string, err error) {
	cfg, cfgErr := s.GetConfig(ctx, tenantID, siteID)
	if cfgErr != nil {
		return "", cfgErr
	}
	tasks := dbCleanTasksFromConfig(cfg)
	jobID = uuid.New().String()

	s.publish(ctx, tenantID, siteID, site.EventDbCleanStarted, map[string]any{
		"job_id":  jobID,
		"tasks":   tasks,
		"trigger": "manual",
	})

	// Stamp watchdog columns so the sweeper can detect a stalled job.
	if wErr := s.repo.SetActiveDBCleanJob(ctx, siteID, jobID, time.Now().UTC()); wErr != nil {
		s.logger.Warn("db-clean: failed to stamp watchdog columns",
			slog.String("job_id", jobID), slog.String("site_id", siteID.String()), slog.Any("error", wErr))
	}

	if s.agent == nil || s.sites == nil {
		// No agent wired: emit completed immediately with zero counts.
		_ = s.repo.ClearActiveDBCleanJob(ctx, siteID)
		s.publish(ctx, tenantID, siteID, site.EventDbCleanCompleted, map[string]any{
			"job_id":       jobID,
			"rows_deleted": 0,
			"bytes_freed":  0,
			"categories":   map[string]any{},
		})
		return jobID, nil
	}

	siteURL, lookupErr := s.sites.GetSiteURL(ctx, tenantID, siteID)
	if lookupErr != nil {
		return "", lookupErr
	}

	progressEndpoint := ""
	if cpBaseURL != "" {
		progressEndpoint = strings.TrimRight(cpBaseURL, "/") + "/agent/v1/db-clean/progress"
	}

	res, perr := s.agent.DBClean(ctx, siteID, siteURL, agentcmd.DBCleanRequest{
		JobID:            jobID,
		Tasks:            tasks,
		ProgressEndpoint: progressEndpoint,
	})
	if perr != nil {
		// Transport error — not a semantic refusal. Clear watchdog immediately.
		_ = s.repo.ClearActiveDBCleanJob(ctx, siteID)
		s.publish(ctx, tenantID, siteID, site.EventDbCleanFailed, map[string]any{
			"job_id": jobID,
			"detail": perr.Error(),
		})
		return "", perr
	}
	if !res.OK {
		_ = s.repo.ClearActiveDBCleanJob(ctx, siteID)
		s.publish(ctx, tenantID, siteID, site.EventDbCleanFailed, map[string]any{
			"job_id": jobID,
			"detail": res.Detail,
		})
		return "", fmt.Errorf("db_clean refused by agent: %s", res.Detail)
	}
	// Agent accepted. Completion/progress arrive via /agent/v1/db-clean/progress.
	// The watchdog columns are cleared by HandleDBCleanProgress when done=true.
	return jobID, nil
}

// DBCleanScheduled runs a scheduled (CP-initiated) database cleanup for one site.
// It mirrors DBClean but sets trigger="scheduled" in the started SSE payload.
func (s *Service) DBCleanScheduled(ctx context.Context, tenantID, siteID uuid.UUID, cpBaseURL string) (jobID string, err error) {
	cfg, cfgErr := s.GetConfig(ctx, tenantID, siteID)
	if cfgErr != nil {
		return "", cfgErr
	}
	tasks := dbCleanTasksFromConfig(cfg)
	jobID = uuid.New().String()

	s.publish(ctx, tenantID, siteID, site.EventDbCleanStarted, map[string]any{
		"job_id":  jobID,
		"tasks":   tasks,
		"trigger": "scheduled",
	})

	// Stamp watchdog columns.
	if wErr := s.repo.SetActiveDBCleanJob(ctx, siteID, jobID, time.Now().UTC()); wErr != nil {
		s.logger.Warn("db-clean-scheduled: failed to stamp watchdog columns",
			slog.String("job_id", jobID), slog.String("site_id", siteID.String()), slog.Any("error", wErr))
	}

	if s.agent == nil || s.sites == nil {
		_ = s.repo.ClearActiveDBCleanJob(ctx, siteID)
		s.publish(ctx, tenantID, siteID, site.EventDbCleanCompleted, map[string]any{
			"job_id":       jobID,
			"rows_deleted": 0,
			"bytes_freed":  0,
			"categories":   map[string]any{},
		})
		return jobID, nil
	}

	siteURL, lookupErr := s.sites.GetSiteURL(ctx, tenantID, siteID)
	if lookupErr != nil {
		_ = s.repo.ClearActiveDBCleanJob(ctx, siteID)
		return "", lookupErr
	}

	progressEndpoint := ""
	if cpBaseURL != "" {
		progressEndpoint = strings.TrimRight(cpBaseURL, "/") + "/agent/v1/db-clean/progress"
	}

	res, perr := s.agent.DBClean(ctx, siteID, siteURL, agentcmd.DBCleanRequest{
		JobID:            jobID,
		Tasks:            tasks,
		ProgressEndpoint: progressEndpoint,
	})
	if perr != nil {
		_ = s.repo.ClearActiveDBCleanJob(ctx, siteID)
		s.publish(ctx, tenantID, siteID, site.EventDbCleanFailed, map[string]any{
			"job_id": jobID,
			"detail": perr.Error(),
		})
		return "", perr
	}
	if !res.OK {
		_ = s.repo.ClearActiveDBCleanJob(ctx, siteID)
		s.publish(ctx, tenantID, siteID, site.EventDbCleanFailed, map[string]any{
			"job_id": jobID,
			"detail": res.Detail,
		})
		return "", fmt.Errorf("db_clean refused by agent: %s", res.Detail)
	}
	return jobID, nil
}

// DBCleanProgressInput carries one per-category progress push from the agent.
type DBCleanProgressInput struct {
	JobID       string
	Category    string
	RowsDeleted int
	BytesFreed  int
	State       string
	Detail      string
	Done        bool
	TenantID    uuid.UUID
	SiteID      uuid.UUID
}

// HandleDBCleanProgress processes one per-category progress push from the agent.
// It emits db.clean.progress SSE for non-final pushes and db.clean.completed for
// the final push (done=true). If job_id is unknown (CP restarted mid-job) the
// event is still processed — we never 404 on unknown job_id.
func (s *Service) HandleDBCleanProgress(ctx context.Context, in DBCleanProgressInput) error {
	if in.Done {
		// Final push: emit terminal event BEFORE clearing the watchdog so that a
		// failed publish does not leave the job stuck in active state with no rescue
		// path (mirrors the db.scan.completed ordering fix in DBScan). The watchdog
		// sweeper needs the columns set until the terminal SSE is published.
		if in.State == "error" {
			s.publish(ctx, in.TenantID, in.SiteID, site.EventDbCleanFailed, map[string]any{
				"job_id": in.JobID,
				"detail": in.Detail,
			})
			// Clear the watchdog columns only after the terminal event is published.
			if cErr := s.repo.ClearActiveDBCleanJob(ctx, in.SiteID); cErr != nil {
				s.logger.Warn("db-clean: failed to clear watchdog after error terminal",
					slog.String("job_id", in.JobID), slog.String("site_id", in.SiteID.String()), slog.Any("error", cErr))
			}
			return nil
		}

		// Persist the clean result so GET /perf/db/clean can serve it without
		// relying on SSE delivery (mirrors the db_scan result persist in DBScan).
		completedPayload := map[string]any{
			"job_id":       in.JobID,
			"rows_deleted": in.RowsDeleted,
			"bytes_freed":  in.BytesFreed,
			"categories": map[string]any{
				in.Category: map[string]any{
					"rows_deleted": in.RowsDeleted,
					"bytes_freed":  in.BytesFreed,
					"state":        in.State,
				},
			},
		}
		resultJSON, jsonErr := json.Marshal(completedPayload["categories"])
		if jsonErr != nil {
			resultJSON = []byte("{}")
		}
		if uErr := s.repo.UpsertDBCleanResult(ctx, DBCleanResultInput{
			SiteID:      in.SiteID,
			TenantID:    in.TenantID,
			JobID:       in.JobID,
			ResultJSON:  resultJSON,
			RowsDeleted: int64(in.RowsDeleted),
			BytesFreed:  int64(in.BytesFreed),
			CleanedAt:   time.Now().UTC(),
		}); uErr != nil {
			s.logger.Warn("db-clean: failed to persist clean result",
				slog.String("job_id", in.JobID), slog.String("site_id", in.SiteID.String()), slog.Any("error", uErr))
		}

		// Emit db.clean.completed AFTER persist but BEFORE clearing the watchdog,
		// so the ordering is: persist → publish → clear (matches scan invariant).
		s.publish(ctx, in.TenantID, in.SiteID, site.EventDbCleanCompleted, completedPayload)

		// Clear the watchdog columns — job is no longer in-flight.
		if cErr := s.repo.ClearActiveDBCleanJob(ctx, in.SiteID); cErr != nil {
			s.logger.Warn("db-clean: failed to clear watchdog after completion",
				slog.String("job_id", in.JobID), slog.String("site_id", in.SiteID.String()), slog.Any("error", cErr))
		}
		return nil
	}
	// Non-final push: emit per-category progress.
	data := map[string]any{
		"job_id":       in.JobID,
		"category":     in.Category,
		"rows_deleted": in.RowsDeleted,
		"bytes_freed":  in.BytesFreed,
		"state":        in.State,
	}
	if in.Detail != "" {
		data["detail"] = in.Detail
	}
	s.publish(ctx, in.TenantID, in.SiteID, site.EventDbCleanProgress, data)
	return nil
}

// ---------------------------------------------------------------------------
// db scan (M39 Phase 2) — synchronous read-only scan
// ---------------------------------------------------------------------------

// DBScan runs a synchronous read-only database scan against the site's agent.
// The agent performs information_schema queries and returns the full result in
// the ACK body (no async progress). The CP emits:
//
//  1. db.scan.started  — before the agent command is sent
//  2. db.scan.completed — after ok=true ACK (with the full category map)
//  3. db.scan.failed   — on transport error or ok=false
//
// Returns the job_id and the full scan result (non-nil on success) so the
// handler can embed both in the 200 ACK body. SSE is a hint only; the ACK is
// the truth. On error the result pointer is nil.
func (s *Service) DBScan(ctx context.Context, tenantID, siteID uuid.UUID, categories []string) (string, *DBScanResult, error) {
	jobID := uuid.New().String()

	s.publish(ctx, tenantID, siteID, site.EventDbScanStarted, map[string]any{
		"job_id":     jobID,
		"categories": categories,
		"trigger":    "manual",
	})

	// Stamp watchdog columns (3-minute threshold).
	if wErr := s.repo.SetActiveDBScanJob(ctx, siteID, jobID, time.Now().UTC()); wErr != nil {
		s.logger.Warn("db-scan: failed to stamp watchdog columns",
			slog.String("job_id", jobID), slog.String("site_id", siteID.String()), slog.Any("error", wErr))
	}

	if s.agent == nil || s.sites == nil {
		if cErr := s.repo.ClearActiveDBScanJob(ctx, siteID); cErr != nil {
			s.logger.Warn("db-scan: failed to clear watchdog after missing agent",
				slog.String("job_id", jobID), slog.String("site_id", siteID.String()), slog.Any("error", cErr))
		}
		s.publish(ctx, tenantID, siteID, site.EventDbScanFailed, map[string]any{
			"job_id": jobID,
			"detail": "agent client not configured",
		})
		return jobID, nil, fmt.Errorf("agent client not configured")
	}

	siteURL, lookupErr := s.sites.GetSiteURL(ctx, tenantID, siteID)
	if lookupErr != nil {
		if cErr := s.repo.ClearActiveDBScanJob(ctx, siteID); cErr != nil {
			s.logger.Warn("db-scan: failed to clear watchdog after site URL lookup failure",
				slog.String("job_id", jobID), slog.String("site_id", siteID.String()), slog.Any("error", cErr))
		}
		s.publish(ctx, tenantID, siteID, site.EventDbScanFailed, map[string]any{
			"job_id": jobID,
			"detail": lookupErr.Error(),
		})
		return jobID, nil, lookupErr
	}

	// Scan timeout: use 90s when orphan categories are requested (Phase 3.3),
	// because the orphan-enumeration passes (source-scan + prefix matching across
	// all installed plugins) can take significantly longer than a pure
	// information_schema scan. 60s is still used for non-orphan scans.
	scanTimeout := 60 * time.Second
	if includesOrphanCategories(categories) {
		scanTimeout = 90 * time.Second
	}
	scanCtx, cancel := context.WithTimeout(ctx, scanTimeout)
	defer cancel()

	res, perr := s.agent.DBScan(scanCtx, siteID, siteURL, agentcmd.DBScanRequest{
		JobID:      jobID,
		Categories: categories,
	})
	if perr != nil {
		if cErr := s.repo.ClearActiveDBScanJob(ctx, siteID); cErr != nil {
			s.logger.Warn("db-scan: failed to clear watchdog after agent transport error",
				slog.String("job_id", jobID), slog.String("site_id", siteID.String()), slog.Any("error", cErr))
		}
		s.publish(ctx, tenantID, siteID, site.EventDbScanFailed, map[string]any{
			"job_id": jobID,
			"detail": perr.Error(),
		})
		return jobID, nil, perr
	}
	if !res.OK {
		if cErr := s.repo.ClearActiveDBScanJob(ctx, siteID); cErr != nil {
			s.logger.Warn("db-scan: failed to clear watchdog after agent refusal",
				slog.String("job_id", jobID), slog.String("site_id", siteID.String()), slog.Any("error", cErr))
		}
		s.publish(ctx, tenantID, siteID, site.EventDbScanFailed, map[string]any{
			"job_id": jobID,
			"detail": res.Detail,
		})
		return jobID, nil, fmt.Errorf("db_scan refused by agent: %s", res.Detail)
	}

	// Persist the scan result for the operator GET endpoint.
	catJSON, jsonErr := json.Marshal(res.Categories)
	if jsonErr != nil {
		catJSON = []byte("{}")
	}
	// Phase 2.1: marshal the per-table inventory alongside categories.
	tablesJSON, tablesJSONErr := json.Marshal(res.Tables)
	if tablesJSONErr != nil || len(res.Tables) == 0 {
		tablesJSON = []byte("[]")
	}
	// Phase 3.3: marshal orphan-enumeration fields. Agents < 0.16.0 return nil
	// slices which marshal to "null"; normalise those to "[]" for storage.
	orphanedOptionsJSON := marshalJSONArray(res.OrphanedOptions)
	orphanedCronJSON := marshalJSONArray(res.OrphanedCron)
	installedPluginsJSON := marshalJSONArray(res.InstalledPlugins)

	scannedAt := time.Unix(res.ScannedAt, 0).UTC()
	if uErr := s.repo.UpsertDBScanResult(ctx, DBScanResultInput{
		SiteID:               siteID,
		TenantID:             tenantID,
		JobID:                jobID,
		CategoriesJSON:       catJSON,
		TablesJSON:           tablesJSON,
		OrphanedOptionsJSON:  orphanedOptionsJSON,
		OrphanedCronJSON:     orphanedCronJSON,
		InstalledPluginsJSON: installedPluginsJSON,
		DBSizeBytes:          res.DBSizeBytes,
		TableCount:           res.TableCount,
		ScannedAt:            scannedAt,
	}); uErr != nil {
		s.logger.Warn("db-scan: failed to persist scan result",
			slog.String("job_id", jobID), slog.String("site_id", siteID.String()), slog.Any("error", uErr))
	}

	// Emit db.scan.completed BEFORE clearing the watchdog so that a failed
	// publish does not leave the job stuck in active state with no rescue path
	// (the watchdog sweeper would have to wait for its timeout window). Phase
	// 3.3 fields are included so the orphan panel can display without a reload.
	s.publish(ctx, tenantID, siteID, site.EventDbScanCompleted, map[string]any{
		"job_id":            jobID,
		"categories":        res.Categories,
		"tables":            res.Tables,
		"orphaned_options":  res.OrphanedOptions,
		"orphaned_cron":     res.OrphanedCron,
		"installed_plugins": res.InstalledPlugins,
		"db_size_bytes":     res.DBSizeBytes,
		"table_count":       res.TableCount,
		"scanned_at":        res.ScannedAt,
	})

	// Clear watchdog — scan complete.
	if cErr := s.repo.ClearActiveDBScanJob(ctx, siteID); cErr != nil {
		s.logger.Warn("db-scan: failed to clear watchdog after completion",
			slog.String("job_id", jobID), slog.String("site_id", siteID.String()), slog.Any("error", cErr))
	}

	// Return the full result alongside the job ID so the handler can embed it
	// directly in the 200 ACK body. SSE is a hint; the ACK is the truth.
	result := &DBScanResult{
		SiteID:               siteID,
		TenantID:             tenantID,
		JobID:                jobID,
		CategoriesJSON:       catJSON,
		TablesJSON:           tablesJSON,
		OrphanedOptionsJSON:  orphanedOptionsJSON,
		OrphanedCronJSON:     orphanedCronJSON,
		InstalledPluginsJSON: installedPluginsJSON,
		DBSizeBytes:          res.DBSizeBytes,
		TableCount:           res.TableCount,
		ScannedAt:            scannedAt,
	}
	return jobID, result, nil
}

// GetLatestScan returns the latest stored db_scan result for a site.
// Returns nil (no error) when no scan has been run yet.
func (s *Service) GetLatestScan(ctx context.Context, tenantID, siteID uuid.UUID) (*DBScanResult, error) {
	result, err := s.repo.GetDBScanResult(ctx, tenantID, siteID)
	if err == ErrNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// GetActiveDBScanState returns the in-progress db_scan watchdog state for a
// site. ErrNotFound (no perf config row) is surfaced to the caller; the handler
// treats it as "not active" without returning an error to the client.
func (s *Service) GetActiveDBScanState(ctx context.Context, tenantID, siteID uuid.UUID) (ActiveDBScanState, error) {
	return s.repo.GetActiveDBScanState(ctx, tenantID, siteID)
}

// GetActiveDBCleanState returns the in-progress db_clean watchdog state for a
// site. ErrNotFound (no perf config row) is surfaced to the caller; the handler
// treats it as "not active" without returning an error to the client.
func (s *Service) GetActiveDBCleanState(ctx context.Context, tenantID, siteID uuid.UUID) (ActiveDBCleanState, error) {
	return s.repo.GetActiveDBCleanState(ctx, tenantID, siteID)
}

// GetLatestClean returns the latest stored db_clean result for a site.
// Returns nil (no error) when no clean has been run yet.
func (s *Service) GetLatestClean(ctx context.Context, tenantID, siteID uuid.UUID) (*DBCleanResult, error) {
	result, err := s.repo.GetDBCleanResult(ctx, tenantID, siteID)
	if err == ErrNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// ---------------------------------------------------------------------------
// db health / size trend (M42 Phase 3.4)
// ---------------------------------------------------------------------------

// GetDBHealth returns the DB-size trend for the given site over the last
// `days` days. days is caller-clamped to [7, 365] before this call.
func (s *Service) GetDBHealth(ctx context.Context, tenantID, siteID uuid.UUID, days int) (DBHealthResponse, error) {
	since := time.Now().UTC().AddDate(0, 0, -days)
	points, err := s.repo.GetDBSizeHistory(ctx, tenantID, siteID, since)
	if err != nil {
		return DBHealthResponse{}, err
	}
	growthBytes, growthPct := computeGrowth(points)
	return DBHealthResponse{
		Points:      points,
		GrowthBytes: growthBytes,
		GrowthPct:   growthPct,
	}, nil
}

// ---------------------------------------------------------------------------
// cache hit-ratio health (M52 / #162)
// ---------------------------------------------------------------------------

// GetCacheHealth returns the cache hit-ratio trend for the given site over the
// last `days` days. days is caller-clamped to [7, 365] before this call.
// AvgRatioPct is the arithmetic mean across Points; zero when Points is empty.
func (s *Service) GetCacheHealth(ctx context.Context, tenantID, siteID uuid.UUID, days int) (CacheHealthResponse, error) {
	since := time.Now().UTC().AddDate(0, 0, -days)
	points, err := s.repo.GetCacheHitRatioHistory(ctx, tenantID, siteID, since)
	if err != nil {
		return CacheHealthResponse{}, err
	}
	var sum float64
	for _, p := range points {
		sum += p.RatioPct
	}
	var avg float64
	if len(points) > 0 {
		avg = math.Round(sum/float64(len(points))*100) / 100
	}
	return CacheHealthResponse{
		Points:      points,
		AvgRatioPct: avg,
	}, nil
}

// ---------------------------------------------------------------------------
// fleet db health (P3.7)
// ---------------------------------------------------------------------------

// fleetTopN is the maximum number of per-site entries in FleetDbHealth.TopSites.
const fleetTopN = 10

// GetFleetDbHealth aggregates database health across all of the tenant's sites
// that have at least one completed scan. `days` is the lookback window for the
// growth calculation (clamped [7,365] by the caller). The result is READ-ONLY
// and NEVER crosses tenant boundaries — the repo call runs inside InTenantTx.
// ListAllSiteIDs returns all site IDs for the tenant. Used by the fleet RUM
// handler to build the full candidate set and compute sites_total.
func (s *Service) ListAllSiteIDs(ctx context.Context, tenantID uuid.UUID) ([]uuid.UUID, error) {
	return s.sites.ListSiteIDs(ctx, tenantID)
}

// ListSitesMeta returns the name+url for every site in the tenant in one bulk
// call (GH #202). Used by the fleet RUM handler to enrich the worst-offenders
// list (capped to <=10 rows) with a display name/url after sort+cap, so the
// lookup never scales with the tenant's total reporting site count.
func (s *Service) ListSitesMeta(ctx context.Context, tenantID uuid.UUID) ([]SiteMeta, error) {
	return s.sites.ListSitesMeta(ctx, tenantID)
}

func (s *Service) GetFleetDbHealth(ctx context.Context, tenantID uuid.UUID, days int) (FleetDbHealth, error) {
	since := time.Now().UTC().AddDate(0, 0, -days)
	rows, err := s.repo.GetFleetDbHealth(ctx, tenantID, since)
	if err != nil {
		return FleetDbHealth{}, err
	}

	var out FleetDbHealth
	out.TopSites = make([]FleetSiteDbSummary, 0)
	out.SitesNeedingReview = make([]FleetSiteDbReview, 0)

	for _, row := range rows {
		out.TotalSitesScanned++
		out.TotalDBSizeBytes += row.DBSizeBytes
		out.TotalTableCount += row.TableCount
		out.TotalOrphanedOptions += row.OrphanedOptionsCount
		out.TotalOrphanedCron += row.OrphanedCronCount
		if row.OrphanedOptionsCount > 0 || row.OrphanedCronCount > 0 {
			out.SitesWithOrphans++
			out.SitesNeedingReview = append(out.SitesNeedingReview, FleetSiteDbReview{
				SiteID:               row.SiteID,
				SiteName:             row.SiteName,
				OrphanedOptionsCount: row.OrphanedOptionsCount,
				OrphanedCronCount:    row.OrphanedCronCount,
			})
		}
	}

	// SitesNeedingReview carries EVERY flagged site (not capped by fleetTopN)
	// so the FE can enumerate the full "sites with items to review" set even
	// when a flagged site isn't among the top-10-by-DB-size (GH #197). Sort
	// by total orphan count descending, then by name for stable ordering.
	sort.Slice(out.SitesNeedingReview, func(i, j int) bool {
		a, b := out.SitesNeedingReview[i], out.SitesNeedingReview[j]
		aTotal := a.OrphanedOptionsCount + a.OrphanedCronCount
		bTotal := b.OrphanedOptionsCount + b.OrphanedCronCount
		if aTotal != bTotal {
			return aTotal > bTotal
		}
		return a.SiteName < b.SiteName
	})

	// Cap the top-N list. The SQL result is already ordered by db_size_bytes
	// DESC so the first fleetTopN rows are the largest sites.
	n := len(rows)
	if n > fleetTopN {
		n = fleetTopN
	}
	out.TopSites = rows[:n]

	return out, nil
}

// computeGrowth derives the absolute and percent DB-size growth from the
// first to the last point in the series. Returns zeros when fewer than two
// points are available. GrowthPct is rounded to two decimal places.
func computeGrowth(points []DbSizeTrendPoint) (growthBytes int64, growthPct float64) {
	if len(points) < 2 {
		return 0, 0
	}
	first := points[0].DBSizeBytes
	last := points[len(points)-1].DBSizeBytes
	growthBytes = last - first
	if first > 0 {
		raw := float64(growthBytes) / float64(first) * 100
		growthPct = math.Round(raw*100) / 100
	}
	return
}

// ---------------------------------------------------------------------------
// db table action (Phase 2.2/2.5) — per-table DDL: optimize / repair / drop / empty / analyze / convert_innodb
// ---------------------------------------------------------------------------

// validTableActions is the set of accepted action strings.
var validTableActions = map[string]bool{
	"optimize":       true,
	"repair":         true,
	"drop":           true,
	"empty":          true,
	"analyze":        true,
	"convert_innodb": true,
}

// destructiveTableActions are the actions that require the non-core safety gate
// on the agent side AND the type-to-confirm validation on the CP side.
// Both drop and empty are refused for WP-core and unclassified tables by the
// agent (layer 1); the CP does not distinguish between owner types — that gate
// is agent-side only.
var destructiveTableActions = map[string]bool{
	"drop":  true,
	"empty": true,
}

// DBTableActionInput carries the validated parameters for a per-table action.
// Confirm is the operator-supplied type-to-confirm token (required for
// drop/empty). For a single-table drop Confirm must equal the table name; for
// bulk drop Confirm must equal "DROP N TABLES" where N = len(Tables).
type DBTableActionInput struct {
	Action  string
	Tables  []string
	Confirm string // required only for drop/empty; validated before dispatch
}

// DBTableActionOutput is the CP response to a db_table_action dispatch. The
// agent's per-table results are passed through verbatim.
type DBTableActionOutput struct {
	JobID         string
	Results       []agentcmd.DBTableActionTableResult
	BackupWarning string // non-empty when no recent backup found (advisory only)
}

// DBTableAction validates the input, checks the backup advisory, dispatches the
// signed db_table_action command to the agent, and emits SSE events.
//
// Safety layers enforced on the CP side (before dispatch):
//  1. Valid action value.
//  2. Non-empty tables list, max 200 tables per call (matches the agent's cap so
//     a confirmed bulk action is never silently truncated on the agent side).
//  3. For drop/empty: type-to-confirm validation.
//  4. Advisory backup nudge (non-blocking): if no successful backup in last 24 h,
//     BackupWarning is set in the output.
//
// The agent enforces its own non-core gate (layer 1 in the contract — refuses
// owner_type 'core' and 'unknown'; allows plugin/theme/orphan) and exact-match
// information_schema validation (layer 2) independently. The CP does not send
// any classification data to the agent.
func (s *Service) DBTableAction(ctx context.Context, tenantID, siteID uuid.UUID, in DBTableActionInput) (DBTableActionOutput, error) {
	// --- input validation ---
	if !validTableActions[in.Action] {
		return DBTableActionOutput{}, domain.Validation("invalid_table_action",
			fmt.Sprintf("action %q is not valid; must be one of: optimize, repair, drop, empty, analyze, convert_innodb", in.Action))
	}
	if len(in.Tables) == 0 {
		return DBTableActionOutput{}, domain.Validation("missing_tables", "tables must contain at least one table name")
	}
	if len(in.Tables) > 200 {
		return DBTableActionOutput{}, domain.Validation("too_many_tables", "tables may contain at most 200 entries per call")
	}

	// --- type-to-confirm gate for destructive actions ---
	if destructiveTableActions[in.Action] {
		if len(in.Tables) == 1 {
			if in.Confirm != in.Tables[0] {
				return DBTableActionOutput{}, domain.Validation("confirm_mismatch",
					"confirm must equal the table name for a single-table "+in.Action)
			}
		} else {
			expected := fmt.Sprintf("%s %d TABLES", strings.ToUpper(in.Action), len(in.Tables))
			if in.Confirm != expected {
				return DBTableActionOutput{}, domain.Validation("confirm_mismatch",
					fmt.Sprintf("confirm must equal %q for a bulk %s of %d tables", expected, in.Action, len(in.Tables)))
			}
		}
	}

	// --- advisory backup nudge (non-blocking) ---
	backupWarning := ""
	if destructiveTableActions[in.Action] && s.backupChecker != nil {
		hasRecent, checkErr := s.backupChecker.HasRecentBackup(ctx, tenantID, siteID, 24*time.Hour)
		if checkErr != nil {
			s.logger.Warn("db-table-action: backup recency check failed (continuing)",
				slog.String("site_id", siteID.String()), slog.Any("error", checkErr))
		} else if !hasRecent {
			backupWarning = "No recent backup found. Consider backing up before running destructive table actions."
		}
	}

	// --- agent dispatch ---
	if s.agent == nil || s.sites == nil {
		return DBTableActionOutput{}, domain.ServiceUnavailable("agent_unwired", "agent client not configured")
	}
	siteURL, err := s.sites.GetSiteURL(ctx, tenantID, siteID)
	if err != nil {
		return DBTableActionOutput{}, err
	}

	jobID := uuid.New().String()
	res, agentErr := s.agent.DBTableAction(ctx, siteID, siteURL, agentcmd.DBTableActionRequest{
		JobID:  jobID,
		Action: in.Action,
		Tables: in.Tables,
	})
	if agentErr != nil {
		s.publish(ctx, tenantID, siteID, site.EventDbTableActionFailed, map[string]any{
			"job_id": jobID,
			"action": in.Action,
			"detail": agentErr.Error(),
		})
		return DBTableActionOutput{}, agentErr
	}
	if !res.OK {
		s.publish(ctx, tenantID, siteID, site.EventDbTableActionFailed, map[string]any{
			"job_id": jobID,
			"action": in.Action,
			"detail": res.Detail,
		})
		return DBTableActionOutput{}, fmt.Errorf("db_table_action refused by agent: %s", res.Detail)
	}

	s.publish(ctx, tenantID, siteID, site.EventDbTableActionCompleted, map[string]any{
		"job_id":  jobID,
		"action":  in.Action,
		"results": res.Results,
	})

	return DBTableActionOutput{
		JobID:         jobID,
		Results:       res.Results,
		BackupWarning: backupWarning,
	}, nil
}

// ---------------------------------------------------------------------------
// P3.8 — orphan delete (destructive, async, GATED by re-classify + confirm)
// ---------------------------------------------------------------------------

// OrphanDeleteRequestItem is one item from the operator's POST body.
type OrphanDeleteRequestItem struct {
	Kind      string `json:"kind"` // "option" | "cron" | "table"
	Name      string `json:"name"`
	OwnerSlug string `json:"owner_slug"`
}

// OrphanDeleteInput is the validated operator input for DBOrphanDelete.
type OrphanDeleteInput struct {
	Items   []OrphanDeleteRequestItem
	Confirm string
}

// OrphanDeleteOutput is returned by Service.DBOrphanDelete on success.
type OrphanDeleteOutput struct {
	JobID         string
	AcceptedCount int    // items that passed re-classify and were signed
	DroppedCount  int    // items filtered out by re-classify
	BackupWarning string // non-empty when no recent backup found (advisory)
}

// DBOrphanDelete is the P3.8 destructive orphan-deletion service method.
//
// Safety sequence:
//  1. Re-run GetOrphansReport against the latest scan (live corpus).
//  2. Filter the operator's requested items to those currently DeletableEligible
//     AND whose owner_slug matches the report. Survivors only are signed.
//  3. Reject if zero items survive.
//  4. Validate the type-to-confirm token against the REQUESTED count (operators
//     confirm the count they see in the UI; re-classify may drop some).
//  5. Emit db.orphan.delete.started SSE.
//  6. Sign and dispatch the db_orphan_delete command to the agent (ASYNC).
//  7. Record the watchdog columns so the sweeper can detect a stalled job.
func (s *Service) DBOrphanDelete(
	ctx context.Context,
	corpusSource CorpusSource,
	tenantID, siteID uuid.UUID,
	cpBaseURL string,
	in OrphanDeleteInput,
) (OrphanDeleteOutput, error) {
	if len(in.Items) == 0 {
		return OrphanDeleteOutput{}, domain.Validation("missing_items", "items must contain at least one entry")
	}
	if len(in.Items) > 500 {
		return OrphanDeleteOutput{}, domain.Validation("too_many_items", "items may contain at most 500 entries per call")
	}

	// --- Validate the type-to-confirm token (before re-classify, against the
	//     requested count the operator saw in the UI) ---
	expectedConfirm := orphanDeleteExpectedConfirm(in.Items)
	if strings.ToUpper(in.Confirm) != strings.ToUpper(expectedConfirm) {
		return OrphanDeleteOutput{}, domain.Validation("confirm_mismatch",
			fmt.Sprintf("confirm must equal %q", expectedConfirm))
	}

	// --- Re-classify: run GetOrphansReport with the latest scan ---
	if corpusSource == nil {
		return OrphanDeleteOutput{}, domain.ServiceUnavailable("corpus_unwired", "corpus reader not configured")
	}
	report, err := s.GetOrphansReport(ctx, corpusSource, tenantID, siteID)
	if err != nil {
		return OrphanDeleteOutput{}, err
	}
	if !report.SnapshotAvailable {
		return OrphanDeleteOutput{}, domain.Validation("no_snapshot", "installed-plugin snapshot is unavailable; run a fresh scan with agent >= 0.16.0 first")
	}

	// Build a lookup map from the live report: "kind:name" → OrphanItem.
	reportMap := make(map[string]OrphanItem, len(report.Options)+len(report.Cron)+len(report.Tables))
	for _, it := range report.Options {
		reportMap["option:"+it.Name] = it
	}
	for _, it := range report.Cron {
		reportMap["cron:"+it.Name] = it
	}
	for _, it := range report.Tables {
		reportMap["table:"+it.Name] = it
	}

	// Filter to survivors: only items currently DeletableEligible with matching owner_slug.
	survivors := make([]agentcmd.OrphanDeleteItem, 0, len(in.Items))
	dropped := 0
	for _, req := range in.Items {
		key := req.Kind + ":" + req.Name
		ri, found := reportMap[key]
		if !found || !ri.DeletableEligible || ri.OwnerSlug != req.OwnerSlug {
			dropped++
			continue
		}
		// Use the report's owner_slug (not the raw request) to prevent slug spoofing.
		survivors = append(survivors, agentcmd.OrphanDeleteItem{
			Kind:      req.Kind,
			Name:      req.Name,
			OwnerSlug: ri.OwnerSlug,
		})
	}

	if len(survivors) == 0 {
		return OrphanDeleteOutput{}, domain.Validation("no_eligible_items",
			"none of the requested items are currently eligible for deletion")
	}

	// --- Advisory backup nudge (non-blocking) ---
	backupWarning := ""
	if s.backupChecker != nil {
		hasRecent, checkErr := s.backupChecker.HasRecentBackup(ctx, tenantID, siteID, 24*time.Hour)
		if checkErr != nil {
			s.logger.Warn("db-orphan-delete: backup recency check failed (continuing)",
				slog.String("site_id", siteID.String()), slog.Any("error", checkErr))
		} else if !hasRecent {
			backupWarning = "No recent backup found. Consider backing up before running destructive orphan deletions."
		}
	}

	// --- Agent dispatch ---
	if s.agent == nil || s.sites == nil {
		return OrphanDeleteOutput{}, domain.ServiceUnavailable("agent_unwired", "agent client not configured")
	}
	siteURL, err := s.sites.GetSiteURL(ctx, tenantID, siteID)
	if err != nil {
		return OrphanDeleteOutput{}, err
	}

	jobID := uuid.New().String()

	progressEndpoint := ""
	if cpBaseURL != "" {
		progressEndpoint = strings.TrimRight(cpBaseURL, "/") + "/agent/v1/db-orphan-delete/progress"
	}

	// Emit started event before dispatch so the UI knows a job is running.
	s.publish(ctx, tenantID, siteID, site.EventDbOrphanDeleteStarted, map[string]any{
		"job_id":         jobID,
		"site_id":        siteID.String(),
		"accepted_count": len(survivors),
		"dropped_count":  dropped,
	})

	// Stamp watchdog columns.
	if wErr := s.repo.SetActiveDBOrphanDeleteJob(ctx, siteID, jobID, time.Now().UTC()); wErr != nil {
		s.logger.Warn("db-orphan-delete: failed to stamp watchdog columns",
			slog.String("job_id", jobID), slog.String("site_id", siteID.String()), slog.Any("error", wErr))
	}

	res, agentErr := s.agent.DBOrphanDelete(ctx, siteID, siteURL, agentcmd.DBOrphanDeleteRequest{
		JobID:            jobID,
		Items:            survivors,
		ProgressEndpoint: progressEndpoint,
	})
	if agentErr != nil {
		_ = s.repo.ClearActiveDBOrphanDeleteJob(ctx, siteID)
		s.publish(ctx, tenantID, siteID, site.EventDbOrphanDeleteFailed, map[string]any{
			"job_id":  jobID,
			"site_id": siteID.String(),
			"detail":  agentErr.Error(),
		})
		return OrphanDeleteOutput{}, agentErr
	}
	if !res.OK {
		_ = s.repo.ClearActiveDBOrphanDeleteJob(ctx, siteID)
		s.publish(ctx, tenantID, siteID, site.EventDbOrphanDeleteFailed, map[string]any{
			"job_id":  jobID,
			"site_id": siteID.String(),
			"detail":  res.Detail,
		})
		return OrphanDeleteOutput{}, fmt.Errorf("db_orphan_delete refused by agent: %s", res.Detail)
	}

	return OrphanDeleteOutput{
		JobID:         jobID,
		AcceptedCount: len(survivors),
		DroppedCount:  dropped,
		BackupWarning: backupWarning,
	}, nil
}

// orphanDeleteExpectedConfirm computes the type-to-confirm token the operator
// must type for a given set of requested items.
//
// Grammar:
//   - Exactly 1 item: the item's Name value.
//   - Multiple items, all same kind: "DELETE N OPTIONS|CRON|TABLES" (uppercased).
//   - Multiple items, mixed kinds: "DELETE N ORPHANS".
func orphanDeleteExpectedConfirm(items []OrphanDeleteRequestItem) string {
	if len(items) == 1 {
		return items[0].Name
	}
	// Check if all items are the same kind.
	kind := items[0].Kind
	sameKind := true
	for _, it := range items[1:] {
		if it.Kind != kind {
			sameKind = false
			break
		}
	}
	n := len(items)
	if sameKind {
		kindLabel := kindToLabel(kind)
		return fmt.Sprintf("DELETE %d %s", n, kindLabel)
	}
	return fmt.Sprintf("DELETE %d ORPHANS", n)
}

// kindToLabel maps an orphan item kind to the plural uppercase label used in
// the type-to-confirm token (e.g. "option" → "OPTIONS").
func kindToLabel(kind string) string {
	switch kind {
	case "option":
		return "OPTIONS"
	case "cron":
		return "CRON"
	case "table":
		return "TABLES"
	default:
		return strings.ToUpper(kind) + "S"
	}
}

// HandleDBOrphanDeleteProgress processes one batched progress push from the
// agent at POST /agent/v1/db-orphan-delete/progress. It emits:
//   - db.orphan.delete.progress for non-final pushes (done=false).
//   - db.orphan.delete.completed for the final push (done=true, no error).
//   - db.orphan.delete.failed if the agent signals a fatal error (done=true,
//     any result has status="error" at the top level — not per-item error which
//     is normal).
func (s *Service) HandleDBOrphanDeleteProgress(ctx context.Context, in DBOrphanDeleteProgressInput) error {
	if in.Done {
		_ = s.repo.ClearActiveDBOrphanDeleteJob(ctx, in.SiteID)
		s.publish(ctx, in.TenantID, in.SiteID, site.EventDbOrphanDeleteCompleted, map[string]any{
			"job_id":          in.JobID,
			"site_id":         in.SiteID.String(),
			"deleted_options": in.DeletedOptions,
			"deleted_cron":    in.DeletedCron,
			"deleted_tables":  in.DeletedTables,
			"skipped":         in.Skipped,
			"bytes_freed":     0, // options + cron are small; tables are cleaned elsewhere
		})
		return nil
	}
	s.publish(ctx, in.TenantID, in.SiteID, site.EventDbOrphanDeleteProgress, map[string]any{
		"job_id":          in.JobID,
		"site_id":         in.SiteID.String(),
		"deleted_options": in.DeletedOptions,
		"deleted_cron":    in.DeletedCron,
		"deleted_tables":  in.DeletedTables,
		"skipped":         in.Skipped,
	})
	return nil
}

// DBOrphanDeleteProgressInput carries one batched progress push from the agent.
type DBOrphanDeleteProgressInput struct {
	JobID          string
	Results        []agentcmd.OrphanDeleteItemResult
	DeletedOptions int
	DeletedCron    int
	DeletedTables  int
	Skipped        int
	Done           bool
	TenantID       uuid.UUID
	SiteID         uuid.UUID
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// includesOrphanCategories reports whether the requested category set includes
// the Phase 3.3 orphan-scan categories ("orphaned_options" or "orphaned_cron").
// An empty slice means "all categories" and the orphan scan is included by
// definition, so a nil/empty categories slice also returns true. This drives
// the extended 90-second timeout for orphan-enumeration scans.
func includesOrphanCategories(categories []string) bool {
	if len(categories) == 0 {
		// Empty ⇒ all categories including orphan ones.
		return true
	}
	for _, c := range categories {
		if c == "orphaned_options" || c == "orphaned_cron" {
			return true
		}
	}
	return false
}

// marshalJSONArray serialises v to JSON. If v is nil or marshals to "null"
// it returns []byte("[]") — this normalises nil slices from agents < 0.16.0
// to an empty JSON array for consistent JSONB storage.
func marshalJSONArray(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil || string(b) == "null" {
		return []byte("[]")
	}
	return b
}

// sanitizeEditURL returns u unchanged when its scheme is http or https, and nil
// otherwise. This guards the web client's anchor href sink against non-http(s)
// schemes that a compromised agent could inject.
func sanitizeEditURL(u *string) *string {
	if u == nil {
		return nil
	}
	p, err := url.Parse(*u)
	if err != nil || (p.Scheme != "http" && p.Scheme != "https") {
		return nil
	}
	return u
}

func (s *Service) publish(ctx context.Context, tenantID, siteID uuid.UUID, eventType string, data map[string]any) {
	if s.events == nil {
		return
	}
	if err := s.events.Publish(ctx, site.ConnectionEvent{
		Type: eventType, TenantID: tenantID, SiteID: siteID, Data: data,
	}); err != nil {
		s.logger.WarnContext(ctx, "perf: failed to publish SSE event",
			slog.String("event_type", eventType),
			slog.String("site_id", siteID.String()),
			slog.Any("error", err))
	}
}

func defaultConfig(tenantID, siteID uuid.UUID) Config {
	return Config{
		SiteID:               siteID,
		TenantID:             tenantID,
		CacheRefreshInterval: "2hours",
		CacheLinkPrefetch:    true,
		CSSJSMinify:          true,
		JSDelayMethod:        "defer",
		FontsDisplaySwap:     true,
		LazyLoad:             true,
		ProperlySizeImages:   true,
		CDNFileTypes:         "all",
		DBAutoCleanInterval:  "weekly",
		PreloadConcurrency:   1,
		PreloadDelayMs:       500,
		PreloadBatchSize:     50,
		PreloadMaxLoad:       0,
		ConfigVersion:        0,
		CreatedAt:            time.Now(),
		UpdatedAt:            time.Now(),
	}
}

func normalize(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// mapCDNFileTypes translates the operator-facing CDN file-type values into
// the agent-side enum. The public API and database keep "images" and "css_js";
// the agent expects "image" and "css_js_font". An empty value defaults to
// "all" so a fresh config never accidentally disables the rewrite scope.
func mapCDNFileTypes(cpValue string) string {
	switch cpValue {
	case "images":
		return "image"
	case "css_js":
		return "css_js_font"
	case "all":
		return "all"
	default:
		return "all"
	}
}

// toPerfConfigRequest maps a stored Config to the agent command body. CDN
// credentials are deliberately omitted (the CP holds them).
//
// freshBeaconKey is the PLAINTEXT beacon key, non-empty only when freshly
// generated or rotated in this push. On subsequent pushes where the key is
// unchanged, pass "" — the CP stores only the hash and cannot resend the
// plaintext. cpBaseURL is used to derive rum_ingest_url (e.g.
// "https://manage.example.com" → "https://manage.example.com/rum/ingest").
func toPerfConfigRequest(c Config, freshBeaconKey, cpBaseURL string) agentcmd.PerfConfigRequest {
	var ingestURL string
	if c.RumEnabled && cpBaseURL != "" {
		ingestURL = strings.TrimRight(cpBaseURL, "/") + "/rum/ingest"
	}
	return agentcmd.PerfConfigRequest{
		ConfigVersion:            c.ConfigVersion,
		CacheEnabled:             c.CacheEnabled,
		CacheLoggedIn:            c.CacheLoggedIn,
		CacheMobile:              c.CacheMobile,
		CacheRefresh:             c.CacheRefresh,
		CacheRefreshInterval:     c.CacheRefreshInterval,
		CacheLinkPrefetch:        c.CacheLinkPrefetch,
		CacheBypassURLs:          coalesce(c.CacheBypassURLs),
		CacheBypassCookies:       coalesce(c.CacheBypassCookies),
		CacheIncludeQueries:      coalesce(c.CacheIncludeQueries),
		CacheIncludeCookies:      coalesce(c.CacheIncludeCookies),
		PreloadConcurrency:       c.PreloadConcurrency,
		PreloadDelayMs:           c.PreloadDelayMs,
		PreloadBatchSize:         c.PreloadBatchSize,
		PreloadMaxLoad:           c.PreloadMaxLoad,
		CSSJSMinify:              c.CSSJSMinify,
		CSSRucss:                 c.CSSRucss,
		CSSRucssIncludeSelect:    coalesce(c.CSSRucssIncludeSelectors),
		CSSJSSelfHostThirdParty:  c.CSSJSSelfHostThirdParty,
		JSDelay:                  c.JSDelay,
		JSDelayMethod:            c.JSDelayMethod,
		JSDelayExcludes:          coalesce(c.JSDelayExcludes),
		JSDelayThirdParty:        c.JSDelayThirdParty,
		JSDelayThirdPartyExc:     coalesce(c.JSDelayThirdPartyExcludes),
		FontsDisplaySwap:         c.FontsDisplaySwap,
		FontsOptimizeGoogle:      c.FontsOptimizeGoogle,
		FontsPreload:             c.FontsPreload,
		FontsTranscodeWOFF2:      c.FontsTranscodeWOFF2,
		LazyLoad:                 c.LazyLoad,
		LazyLoadExclusions:       coalesce(c.LazyLoadExclusions),
		ProperlySizeImages:       c.ProperlySizeImages,
		YouTubePlaceholder:       c.YouTubePlaceholder,
		SelfHostGravatars:        c.SelfHostGravatars,
		CDNEnabled:               c.CDNEnabled,
		CDNURL:                   c.CDNURL,
		CDNFileType:              mapCDNFileTypes(c.CDNFileTypes),
		DBAutoClean:              c.DBAutoClean,
		DBAutoCleanInterval:      c.DBAutoCleanInterval,
		DBPostRevisions:          c.DBPostRevisions,
		DBPostAutoDrafts:         c.DBPostAutoDrafts,
		DBPostTrashed:            c.DBPostTrashed,
		DBCommentsSpam:           c.DBCommentsSpam,
		DBCommentsTrashed:        c.DBCommentsTrashed,
		DBTransientsExpired:      c.DBTransientsExpired,
		DBOptimizeTables:         c.DBOptimizeTables,
		BloatDisableBlockCSS:     c.BloatDisableBlockCSS,
		BloatDisableDashicons:    c.BloatDisableDashicons,
		BloatDisableEmojis:       c.BloatDisableEmojis,
		BloatDisableJQueryMig:    c.BloatDisableJQueryMig,
		BloatDisableXMLRPC:       c.BloatDisableXMLRPC,
		BloatDisableRSSFeed:      c.BloatDisableRSSFeed,
		BloatDisableOembeds:      c.BloatDisableOembeds,
		BloatHeartbeatControl:    c.BloatHeartbeatControl,
		BloatPostRevisionControl: c.BloatPostRevisionControl,
		// M53 / #169 — WooCommerce cacheable-session flag.
		WooCacheableSession: c.WooCacheableSession,
		// M56 / RUM — Real User Monitoring fields.
		RumEnabled:    c.RumEnabled,
		RumSampleRate: c.RumSampleRate,
		RumBeaconKey:  freshBeaconKey, // non-empty only on first-enable/rotation
		RumIngestURL:  ingestURL,
	}
}

// ---------------------------------------------------------------------------
// #188 — search-replace tool (synchronous, dry-run capable)
// ---------------------------------------------------------------------------

// minSearchReplaceLength is the minimum byte length for the search string.
// Mirrors the agent's MIN_SEARCH_LENGTH constant. Enforced on the CP before
// the agent call so the error surfaces in a clean domain error, not a raw
// agent refusal.
const minSearchReplaceLength = 3

// SearchReplaceInput is the validated operator input for SearchReplace.
type SearchReplaceInput struct {
	Search  string   // exact search string (min 3 bytes)
	Replace string   // replacement string (may be empty)
	DryRun  bool     // true => preview only, no writes
	Tables  []string // optional table allowlist
}

// SearchReplaceOutput is returned by Service.SearchReplace.
type SearchReplaceOutput struct {
	JobID         string
	TablesScanned int
	RowsMatched   int
	RowsChanged   int // always 0 when DryRun=true
	BackupWarning string
}

// SearchReplace dispatches the serialization-safe search_replace command to
// the site's agent. The command is SYNCHRONOUS: the full result (or preview
// when dry_run=true) is returned in the ACK body.
//
// Safety layers enforced on the CP side before dispatch:
//  1. search length >= 3 bytes (prevents excessively broad replacements).
//  2. BackupChecker advisory (non-blocking): emits BackupWarning when no
//     recent backup is found and dry_run=false.
//  3. RequireSiteAccess + PermSiteWrite enforced at the handler layer.
func (s *Service) SearchReplace(ctx context.Context, tenantID, siteID uuid.UUID, in SearchReplaceInput) (SearchReplaceOutput, error) {
	// --- input validation ---
	if len(in.Search) < minSearchReplaceLength {
		return SearchReplaceOutput{}, domain.Validation("search_too_short",
			fmt.Sprintf("search must be at least %d bytes", minSearchReplaceLength))
	}

	// --- advisory backup nudge (non-blocking, live-run only) ---
	backupWarning := ""
	if !in.DryRun && s.backupChecker != nil {
		hasRecent, checkErr := s.backupChecker.HasRecentBackup(ctx, tenantID, siteID, 24*time.Hour)
		if checkErr != nil {
			s.logger.Warn("search-replace: backup recency check failed (continuing)",
				slog.String("site_id", siteID.String()), slog.Any("error", checkErr))
		} else if !hasRecent {
			backupWarning = "No recent backup found. Consider backing up before running a search-replace."
		}
	}

	// --- agent dispatch ---
	if s.agent == nil || s.sites == nil {
		return SearchReplaceOutput{}, domain.ServiceUnavailable("agent_unwired", "agent client not configured")
	}
	siteURL, err := s.sites.GetSiteURL(ctx, tenantID, siteID)
	if err != nil {
		return SearchReplaceOutput{}, err
	}

	jobID := uuid.New().String()
	res, agentErr := s.agent.SearchReplace(ctx, siteID, siteURL, agentcmd.SearchReplaceRequest{
		JobID:   jobID,
		Search:  in.Search,
		Replace: in.Replace,
		DryRun:  in.DryRun,
		Tables:  in.Tables,
	})
	if agentErr != nil {
		s.publish(ctx, tenantID, siteID, site.EventDbSearchReplaceFailed, map[string]any{
			"job_id": jobID,
			"detail": agentErr.Error(),
		})
		return SearchReplaceOutput{}, agentErr
	}
	if !res.OK {
		s.publish(ctx, tenantID, siteID, site.EventDbSearchReplaceFailed, map[string]any{
			"job_id": jobID,
			"detail": res.Detail,
		})
		return SearchReplaceOutput{}, fmt.Errorf("search_replace refused by agent: %s", res.Detail)
	}

	s.publish(ctx, tenantID, siteID, site.EventDbSearchReplaceCompleted, map[string]any{
		"job_id":         jobID,
		"dry_run":        in.DryRun,
		"tables_scanned": res.TablesScanned,
		"rows_matched":   res.RowsMatched,
		"rows_changed":   res.RowsChanged,
	})

	return SearchReplaceOutput{
		JobID:         jobID,
		TablesScanned: res.TablesScanned,
		RowsMatched:   res.RowsMatched,
		RowsChanged:   res.RowsChanged,
		BackupWarning: backupWarning,
	}, nil
}

// ---------------------------------------------------------------------------
// #189 — Database Snapshots
// ---------------------------------------------------------------------------

// DbSnapshotInput is the validated operator input for DbSnapshot.
type DbSnapshotInput struct {
	// Action is one of "create", "list", "revert", "delete".
	Action string
	// Label is an optional human-readable tag attached to a new snapshot.
	Label string
	// Retention is the maximum number of snapshots to keep (1–20, default 5).
	Retention int
	// SnapshotID identifies an existing snapshot (required for revert/delete).
	SnapshotID string
	// Confirm must equal "REVERT" for the revert action.
	Confirm string
	// SkipSafetySnapshot suppresses the auto-safety snapshot before a revert.
	SkipSafetySnapshot bool
}

// DbSnapshotOutput is returned by Service.DbSnapshot for all four actions.
type DbSnapshotOutput struct {
	// OK mirrors the agent's ok field.
	OK bool
	// Snapshot is the newly created entry (action=create).
	Snapshot *agentcmd.DbSnapshotEntry
	// SnapshotID is the ID of the new snapshot (action=create, convenience accessor).
	SnapshotID string
	// Snapshots is the manifest list (action=list).
	Snapshots []agentcmd.DbSnapshotEntry
	// Detail is a human-readable description of the outcome.
	Detail string
	// SafetyID is the auto-safety snapshot taken before a revert, may be "".
	SafetyID string
}

// ---------------------------------------------------------------------------
// #190 — media library cleaner
// ---------------------------------------------------------------------------

// MediaCleanScanInput is the validated operator input for a scan page.
type MediaCleanScanInput struct {
	// JobID is unused for scan but kept for contract uniformity.
	JobID string
	// Offset is the zero-based attachment offset for pagination.
	Offset int
	// Limit is the number of candidates per page (1–500, default 100).
	Limit int
}

// MediaCleanScanOutput is returned by Service.MediaCleanScan.
type MediaCleanScanOutput struct {
	OK               bool
	Total            int
	Candidates       []agentcmd.MediaCleanCandidate
	HasMore          bool
	Truncated        bool
	TotalAttachments int
	ReferencedCount  int
	UnusedCount      int
	Referenced       []agentcmd.MediaCleanReferenced
	Detail           string
}

// MediaCleanIsolateInput is the validated operator input for isolate.
type MediaCleanIsolateInput struct {
	// JobID is a CP-minted UUID v4 for idempotency. Required.
	JobID         string
	AttachmentIDs []int64
}

// MediaCleanIsolateOutput is returned by Service.MediaCleanIsolate.
// ManifestID is the opaque quarantine manifest identifier returned by the
// agent. The operator must store this and pass it back for restore/delete.
type MediaCleanIsolateOutput struct {
	OK              bool
	JobID           string
	Moved           int
	ManifestID      string
	EntriesRecorded int
	PerAttachment   []agentcmd.MediaCleanIsolatePer
	Detail          string
}

// MediaCleanRestoreInput is the validated operator input for restore.
type MediaCleanRestoreInput struct {
	// JobID is a CP-minted UUID v4 for idempotency.
	JobID string
	// QuarantineIDs are the opaque manifest IDs returned by prior isolate calls.
	QuarantineIDs []string
}

// MediaCleanRestoreOutput is returned by Service.MediaCleanRestore.
type MediaCleanRestoreOutput struct {
	OK       bool
	JobID    string
	Restored int
	Detail   string
}

// MediaCleanDeleteInput is the validated operator input for the permanent delete.
type MediaCleanDeleteInput struct {
	// JobID is a CP-minted UUID v4 for idempotency.
	JobID string
	// QuarantineIDs are the opaque manifest IDs to permanently remove.
	QuarantineIDs []string
	// Confirm MUST equal "DELETE" (case-sensitive). The agent enforces this
	// independently via hash_equals so a mutated body cannot bypass the gate.
	Confirm string
}

// MediaCleanDeleteOutput is returned by Service.MediaCleanDelete.
type MediaCleanDeleteOutput struct {
	OK               bool
	JobID            string
	Deleted          int
	PostsDeleted     int
	PostsFailed      int
	FilesDeleted     int
	EntriesProcessed int
	Results          []agentcmd.MediaCleanDeleteResult
	Detail           string
}

const mediaCleanDeleteConfirmToken = "DELETE"

// MediaCleanScan dispatches a read-only scan page to the site's agent and
// returns the candidate batch. The scan is stateless per request; the caller
// paginates via offset/limit using the total returned in each response.
func (s *Service) MediaCleanScan(ctx context.Context, tenantID, siteID uuid.UUID, in MediaCleanScanInput) (MediaCleanScanOutput, error) {
	if s.agent == nil {
		return MediaCleanScanOutput{}, domain.ServiceUnavailable("agent_unwired", "agent client not configured")
	}

	siteURL, err := s.sites.GetSiteURL(ctx, tenantID, siteID)
	if err != nil {
		return MediaCleanScanOutput{}, err
	}

	limit := in.Limit
	if limit < 1 || limit > 500 {
		limit = 100
	}
	offset := in.Offset
	if offset < 0 {
		offset = 0
	}

	res, agentErr := s.agent.MediaClean(ctx, siteID, siteURL, agentcmd.MediaCleanRequest{
		Action: "scan",
		Limit:  limit,
		Offset: offset,
	})
	if agentErr != nil {
		s.publish(ctx, tenantID, siteID, site.EventMediaCleanScanFailed, map[string]any{
			"error":  agentErr.Error(),
			"offset": offset,
			"limit":  limit,
		})
		return MediaCleanScanOutput{}, agentErr
	}
	if !res.OK {
		s.publish(ctx, tenantID, siteID, site.EventMediaCleanScanFailed, map[string]any{
			"detail": res.Detail,
			"offset": offset,
			"limit":  limit,
		})
		return MediaCleanScanOutput{}, fmt.Errorf("media_clean scan refused by agent: %s", res.Detail)
	}

	// Sanitize URL fields: only http/https links are forwarded to the web client.
	// Any other scheme (e.g. javascript:, data:) is nil'd here as a defense-in-
	// depth guard; the agent is an untrusted boundary.
	// - edit_url on usages: guards the anchor href sink.
	// - thumb on candidates and referenced: guards the <img src> sink.
	candidates := res.Candidates
	if candidates == nil {
		candidates = []agentcmd.MediaCleanCandidate{}
	}
	for i, c := range candidates {
		candidates[i].Thumb = sanitizeEditURL(c.Thumb)
	}

	rawReferenced := res.Referenced
	referenced := make([]agentcmd.MediaCleanReferenced, len(rawReferenced))
	for i, ref := range rawReferenced {
		ref.Thumb = sanitizeEditURL(ref.Thumb)
		sanitized := make([]agentcmd.MediaCleanUsage, len(ref.Usages))
		for j, u := range ref.Usages {
			u.EditURL = sanitizeEditURL(u.EditURL)
			sanitized[j] = u
		}
		ref.Usages = sanitized
		referenced[i] = ref
	}

	s.publish(ctx, tenantID, siteID, site.EventMediaCleanScanCompleted, map[string]any{
		"candidate_count":   len(candidates),
		"total":             res.Total,
		"has_more":          res.HasMore,
		"truncated":         res.Truncated,
		"total_attachments": res.TotalAttachments,
		"referenced_count":  res.ReferencedCount,
		"offset":            offset,
	})

	return MediaCleanScanOutput{
		OK:               res.OK,
		Total:            res.Total,
		Candidates:       candidates,
		HasMore:          res.HasMore,
		Truncated:        res.Truncated,
		TotalAttachments: res.TotalAttachments,
		ReferencedCount:  res.ReferencedCount,
		UnusedCount:      res.UnusedCount,
		Referenced:       referenced,
		Detail:           res.Detail,
	}, nil
}

// MediaCleanIsolate moves the given attachments to the agent-side quarantine
// directory (reversible). The agent writes a manifest; the returned ManifestID
// must be stored by the operator and passed to restore or delete.
func (s *Service) MediaCleanIsolate(ctx context.Context, tenantID, siteID uuid.UUID, in MediaCleanIsolateInput) (MediaCleanIsolateOutput, error) {
	if s.agent == nil {
		return MediaCleanIsolateOutput{}, domain.ServiceUnavailable("agent_unwired", "agent client not configured")
	}
	if in.JobID == "" {
		return MediaCleanIsolateOutput{}, domain.Validation("missing_job_id", "job_id is required for isolate")
	}
	if len(in.AttachmentIDs) == 0 {
		return MediaCleanIsolateOutput{}, domain.Validation("no_attachment_ids", "at least one attachment_id is required")
	}
	if len(in.AttachmentIDs) > 200 {
		return MediaCleanIsolateOutput{}, domain.Validation("too_many_attachments", "batch limit is 200 attachments per request")
	}

	siteURL, err := s.sites.GetSiteURL(ctx, tenantID, siteID)
	if err != nil {
		return MediaCleanIsolateOutput{}, err
	}

	res, agentErr := s.agent.MediaClean(ctx, siteID, siteURL, agentcmd.MediaCleanRequest{
		Action:        "isolate",
		JobID:         in.JobID,
		AttachmentIDs: in.AttachmentIDs,
	})
	if agentErr != nil {
		s.publish(ctx, tenantID, siteID, site.EventMediaCleanIsolateFailed, map[string]any{
			"error":         agentErr.Error(),
			"requested_ids": len(in.AttachmentIDs),
		})
		return MediaCleanIsolateOutput{}, agentErr
	}
	if !res.OK {
		s.publish(ctx, tenantID, siteID, site.EventMediaCleanIsolateFailed, map[string]any{
			"detail":        res.Detail,
			"requested_ids": len(in.AttachmentIDs),
		})
		return MediaCleanIsolateOutput{}, fmt.Errorf("media_clean isolate refused by agent: %s", res.Detail)
	}

	perAttachment := res.PerAttachment
	if perAttachment == nil {
		perAttachment = []agentcmd.MediaCleanIsolatePer{}
	}

	s.publish(ctx, tenantID, siteID, site.EventMediaCleanIsolateDone, map[string]any{
		"moved":            res.Moved,
		"manifest_id":      res.ManifestID,
		"entries_recorded": res.EntriesRecorded,
	})

	return MediaCleanIsolateOutput{
		OK:              res.OK,
		JobID:           res.JobID,
		Moved:           res.Moved,
		ManifestID:      res.ManifestID,
		EntriesRecorded: res.EntriesRecorded,
		PerAttachment:   perAttachment,
		Detail:          res.Detail,
	}, nil
}

// MediaCleanRestore reverses an isolate — moves quarantined files back to
// their original paths using the agent-side manifest records.
func (s *Service) MediaCleanRestore(ctx context.Context, tenantID, siteID uuid.UUID, in MediaCleanRestoreInput) (MediaCleanRestoreOutput, error) {
	if s.agent == nil {
		return MediaCleanRestoreOutput{}, domain.ServiceUnavailable("agent_unwired", "agent client not configured")
	}
	if in.JobID == "" {
		return MediaCleanRestoreOutput{}, domain.Validation("missing_job_id", "job_id is required for restore")
	}
	if len(in.QuarantineIDs) == 0 {
		return MediaCleanRestoreOutput{}, domain.Validation("no_quarantine_ids", "at least one quarantine_id is required")
	}
	if len(in.QuarantineIDs) > 200 {
		return MediaCleanRestoreOutput{}, domain.Validation("too_many_quarantine_ids", "batch limit is 200 quarantine IDs per request")
	}

	siteURL, err := s.sites.GetSiteURL(ctx, tenantID, siteID)
	if err != nil {
		return MediaCleanRestoreOutput{}, err
	}

	res, agentErr := s.agent.MediaClean(ctx, siteID, siteURL, agentcmd.MediaCleanRequest{
		Action:        "restore",
		JobID:         in.JobID,
		QuarantineIDs: in.QuarantineIDs,
	})
	if agentErr != nil {
		s.publish(ctx, tenantID, siteID, site.EventMediaCleanRestoreFailed, map[string]any{
			"error":               agentErr.Error(),
			"requested_manifests": len(in.QuarantineIDs),
		})
		return MediaCleanRestoreOutput{}, agentErr
	}
	if !res.OK {
		s.publish(ctx, tenantID, siteID, site.EventMediaCleanRestoreFailed, map[string]any{
			"detail":              res.Detail,
			"requested_manifests": len(in.QuarantineIDs),
		})
		return MediaCleanRestoreOutput{}, fmt.Errorf("media_clean restore refused by agent: %s", res.Detail)
	}

	s.publish(ctx, tenantID, siteID, site.EventMediaCleanRestoreDone, map[string]any{
		"restored": res.Restored,
	})

	return MediaCleanRestoreOutput{
		OK:       res.OK,
		JobID:    res.JobID,
		Restored: res.Restored,
		Detail:   res.Detail,
	}, nil
}

// MediaCleanDelete permanently removes quarantined attachments from the
// filesystem and force-deletes their attachment posts. This action is
// IRREVERSIBLE. The caller must supply confirm="DELETE"; the agent enforces
// this independently via hash_equals.
func (s *Service) MediaCleanDelete(ctx context.Context, tenantID, siteID uuid.UUID, in MediaCleanDeleteInput) (MediaCleanDeleteOutput, error) {
	if s.agent == nil {
		return MediaCleanDeleteOutput{}, domain.ServiceUnavailable("agent_unwired", "agent client not configured")
	}
	if in.JobID == "" {
		return MediaCleanDeleteOutput{}, domain.Validation("missing_job_id", "job_id is required for delete")
	}
	if len(in.QuarantineIDs) == 0 {
		return MediaCleanDeleteOutput{}, domain.Validation("no_quarantine_ids", "at least one quarantine_id is required")
	}
	if len(in.QuarantineIDs) > 200 {
		return MediaCleanDeleteOutput{}, domain.Validation("too_many_quarantine_ids", "batch limit is 200 quarantine IDs per request")
	}
	if in.Confirm != mediaCleanDeleteConfirmToken {
		return MediaCleanDeleteOutput{}, domain.Validation("confirm_required",
			`confirm must equal "DELETE" to permanently remove quarantined attachments`)
	}

	siteURL, err := s.sites.GetSiteURL(ctx, tenantID, siteID)
	if err != nil {
		return MediaCleanDeleteOutput{}, err
	}

	res, agentErr := s.agent.MediaClean(ctx, siteID, siteURL, agentcmd.MediaCleanRequest{
		Action:        "delete",
		JobID:         in.JobID,
		QuarantineIDs: in.QuarantineIDs,
		Confirm:       in.Confirm,
	})
	if agentErr != nil {
		s.publish(ctx, tenantID, siteID, site.EventMediaCleanDeleteFailed, map[string]any{
			"error":               agentErr.Error(),
			"requested_manifests": len(in.QuarantineIDs),
		})
		return MediaCleanDeleteOutput{}, agentErr
	}
	if !res.OK {
		s.publish(ctx, tenantID, siteID, site.EventMediaCleanDeleteFailed, map[string]any{
			"detail":              res.Detail,
			"requested_manifests": len(in.QuarantineIDs),
		})
		return MediaCleanDeleteOutput{}, fmt.Errorf("media_clean delete refused by agent: %s", res.Detail)
	}

	results := res.Results
	if results == nil {
		results = []agentcmd.MediaCleanDeleteResult{}
	}

	s.publish(ctx, tenantID, siteID, site.EventMediaCleanDeleteDone, map[string]any{
		"deleted":           res.Deleted,
		"posts_deleted":     res.PostsDeleted,
		"posts_failed":      res.PostsFailed,
		"files_deleted":     res.FilesDeleted,
		"entries_processed": res.EntriesProcessed,
	})

	return MediaCleanDeleteOutput{
		OK:               res.OK,
		JobID:            res.JobID,
		Deleted:          res.Deleted,
		PostsDeleted:     res.PostsDeleted,
		PostsFailed:      res.PostsFailed,
		FilesDeleted:     res.FilesDeleted,
		EntriesProcessed: res.EntriesProcessed,
		Results:          results,
		Detail:           res.Detail,
	}, nil
}

// MediaCleanQuarantineList fetches all quarantine manifests currently present
// on the site's agent. The action is read-only (no files are moved).
// A nil manifests slice from the agent is normalised to an empty slice so the
// caller always receives a valid JSON array.
func (s *Service) MediaCleanQuarantineList(ctx context.Context, tenantID, siteID uuid.UUID) ([]agentcmd.MediaCleanManifest, error) {
	if s.agent == nil {
		return nil, domain.ServiceUnavailable("agent_unwired", "agent client not configured")
	}

	siteURL, err := s.sites.GetSiteURL(ctx, tenantID, siteID)
	if err != nil {
		return nil, err
	}

	res, agentErr := s.agent.MediaClean(ctx, siteID, siteURL, agentcmd.MediaCleanRequest{
		Action: "list",
	})
	if agentErr != nil {
		return nil, agentErr
	}
	if !res.OK {
		return nil, fmt.Errorf("media_clean list refused by agent: %s", res.Detail)
	}

	manifests := res.Manifests
	if manifests == nil {
		manifests = []agentcmd.MediaCleanManifest{}
	}
	return manifests, nil
}

// DbSnapshot dispatches the db_snapshot command to the site's agent.
// The command is synchronous for all four actions.
func (s *Service) DbSnapshot(ctx context.Context, tenantID, siteID uuid.UUID, in DbSnapshotInput) (DbSnapshotOutput, error) {
	if s.agent == nil {
		return DbSnapshotOutput{}, domain.ServiceUnavailable("agent_unwired", "agent client not configured")
	}

	siteURL, err := s.sites.GetSiteURL(ctx, tenantID, siteID)
	if err != nil {
		return DbSnapshotOutput{}, err
	}

	retention := in.Retention
	if retention < 1 {
		retention = 5
	}

	res, agentErr := s.agent.DbSnapshot(ctx, siteID, siteURL, agentcmd.DbSnapshotRequest{
		Action:             in.Action,
		Label:              in.Label,
		Retention:          retention,
		SnapshotID:         in.SnapshotID,
		Confirm:            in.Confirm,
		SkipSafetySnapshot: in.SkipSafetySnapshot,
	})
	if agentErr != nil {
		return DbSnapshotOutput{}, agentErr
	}

	// Agent ok=false is NOT wrapped in an error — the caller (handler) must
	// inspect DbSnapshotOutput.OK and surface the detail. This mirrors the
	// DBScan / DBTableAction pattern: transport errors (non-2xx, network) are
	// returned as err; logical refusals are returned as ok=false in the result.
	snapshots := res.Snapshots
	if snapshots == nil {
		snapshots = []agentcmd.DbSnapshotEntry{}
	}

	snapshotID := ""
	if res.Snapshot != nil {
		snapshotID = res.Snapshot.ID
	}

	return DbSnapshotOutput{
		OK:         res.OK,
		Snapshot:   res.Snapshot,
		SnapshotID: snapshotID,
		Snapshots:  snapshots,
		Detail:     res.Detail,
		SafetyID:   res.SafetyID,
	}, nil
}
