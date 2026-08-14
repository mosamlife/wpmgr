package perf

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
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
}

// SiteLookup resolves a site's agent URL (wired in main via a narrow adapter so
// this package stays free of a site import cycle).
type SiteLookup interface {
	GetSiteURL(ctx context.Context, tenantID, siteID uuid.UUID) (string, error)
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

// repository is the persistence surface the service needs. *Repo satisfies it;
// declared as an interface so the service is unit-testable with a fake (no DB).
type repository interface {
	GetConfig(ctx context.Context, tenantID, siteID uuid.UUID) (Config, error)
	UpsertConfig(ctx context.Context, in UpsertConfigInput) (Config, error)
	GetCDNCredentialsCiphertext(ctx context.Context, tenantID, siteID uuid.UUID) ([]byte, string, error)
	UpdateInstallState(ctx context.Context, siteID uuid.UUID, serverSoftware string, dropinInstalled, wpCacheConstantSet, htaccessManaged bool) error
	GetCacheStats(ctx context.Context, tenantID, siteID uuid.UUID) (CacheStats, error)
	UpsertCacheStats(ctx context.Context, s CacheStats) (CacheStats, error)
	RecordPurge(ctx context.Context, in RecordPurgeInput) (PurgeAuditEntry, error)
	ListPurgeAudit(ctx context.Context, tenantID, siteID uuid.UUID, limit, offset int32) ([]PurgeAuditEntry, error)
}

var _ repository = (*Repo)(nil)

// Service orchestrates the Performance Suite control plane.
type Service struct {
	repo      repository
	agent     AgentPerfClient
	sites     SiteLookup
	events    EventPublisher
	decryptor Decryptor
	cdn       CDNPurger
	logger    *slog.Logger
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

	// Push to the agent (best-effort, config already persisted).
	if s.agent != nil && s.sites != nil {
		siteURL, lookupErr := s.sites.GetSiteURL(ctx, tenantID, siteID)
		if lookupErr == nil {
			res, pushErr := s.agent.SyncPerfConfig(ctx, siteID, siteURL, toPerfConfigRequest(saved))
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
func (s *Service) ReportCacheStats(ctx context.Context, stats CacheStats) (CacheStats, error) {
	saved, err := s.repo.UpsertCacheStats(ctx, stats)
	if err != nil {
		return CacheStats{}, err
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

// MarkConfigApplied records the agent's install-state report (InAgentTx). Called
// from the perf/config-ack agent endpoint.
func (s *Service) MarkConfigApplied(ctx context.Context, siteID uuid.UUID, serverSoftware string, dropinInstalled, wpCacheConstantSet, htaccessManaged bool) error {
	return s.repo.UpdateInstallState(ctx, siteID, serverSoftware, dropinInstalled, wpCacheConstantSet, htaccessManaged)
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
		if _, pushErr := s.agent.SyncPerfConfig(ctx, siteID, siteURL, toPerfConfigRequest(cfg)); pushErr != nil {
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
	s.publish(ctx, tenantID, siteID, site.EventCachePreloadStarted, map[string]any{})
	detail := "preload requested"
	if s.agent != nil && s.sites != nil {
		siteURL, lookupErr := s.sites.GetSiteURL(ctx, tenantID, siteID)
		if lookupErr != nil {
			return "", lookupErr
		}
		res, perr := s.agent.CachePreload(ctx, siteID, siteURL, agentcmd.CachePreloadRequest{})
		if perr != nil {
			return "", perr
		}
		detail = res.Detail
	}
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

// DBClean runs an ad-hoc database cleanup scoped to the per-site db_* config and
// emits db.clean.completed. Returns the agent detail.
func (s *Service) DBClean(ctx context.Context, tenantID, siteID uuid.UUID) (string, int, error) {
	cfg, err := s.GetConfig(ctx, tenantID, siteID)
	if err != nil {
		return "", 0, err
	}
	detail := "db clean requested"
	rows := 0
	if s.agent != nil && s.sites != nil {
		siteURL, lookupErr := s.sites.GetSiteURL(ctx, tenantID, siteID)
		if lookupErr != nil {
			return "", 0, lookupErr
		}
		res, perr := s.agent.DBClean(ctx, siteID, siteURL, agentcmd.DBCleanRequest{
			PostRevisions:     cfg.DBPostRevisions,
			PostAutoDrafts:    cfg.DBPostAutoDrafts,
			PostTrashed:       cfg.DBPostTrashed,
			CommentsSpam:      cfg.DBCommentsSpam,
			CommentsTrashed:   cfg.DBCommentsTrashed,
			TransientsExpired: cfg.DBTransientsExpired,
			OptimizeTables:    cfg.DBOptimizeTables,
		})
		if perr != nil {
			return "", 0, perr
		}
		detail = res.Detail
		rows = res.RowsCleaned
	}
	s.publish(ctx, tenantID, siteID, site.EventDbCleanCompleted, map[string]any{"rows_cleaned": rows})
	return detail, rows, nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func (s *Service) publish(ctx context.Context, tenantID, siteID uuid.UUID, eventType string, data map[string]any) {
	if s.events == nil {
		return
	}
	_ = s.events.Publish(ctx, site.ConnectionEvent{
		Type: eventType, TenantID: tenantID, SiteID: siteID, Data: data,
	})
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

// toPerfConfigRequest maps a stored Config to the agent command body. CDN
// credentials are deliberately omitted (the CP holds them).
func toPerfConfigRequest(c Config) agentcmd.PerfConfigRequest {
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
		LazyLoad:                 c.LazyLoad,
		LazyLoadExclusions:       coalesce(c.LazyLoadExclusions),
		ProperlySizeImages:       c.ProperlySizeImages,
		YouTubePlaceholder:       c.YouTubePlaceholder,
		SelfHostGravatars:        c.SelfHostGravatars,
		CDNEnabled:               c.CDNEnabled,
		CDNURL:                   c.CDNURL,
		CDNFileType:              c.CDNFileTypes,
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
	}
}
