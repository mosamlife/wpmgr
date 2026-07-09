package perf

import (
	"context"
	"errors"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

// ErrNotFound is returned when a per-site config or stats row does not exist yet.
var ErrNotFound = errors.New("perf: not found")

// Repo is the persistence layer for the Performance Suite. Operator reads/writes
// run under InTenantTx (app.tenant_id GUC); agent-reported writes (cache stats,
// install-state) run under InAgentTx (app.agent GUC). updated_at is set by the
// m36 queries via now() (no trigger). The encrypted CDN credentials column is
// only ever read by GetConfigWithCredentials — the operator-facing GetConfig
// strips it.
type Repo struct {
	pool *db.Pool
}

// NewRepo wires a Repo with the shared pgx pool.
func NewRepo(pool *db.Pool) *Repo { return &Repo{pool: pool} }

// GetConfig returns the per-site performance config WITHOUT the encrypted CDN
// credentials (CDNHasCredentials reflects whether ciphertext exists). Returns
// ErrNotFound when no row exists yet. Operator read path (InTenantTx).
func (r *Repo) GetConfig(ctx context.Context, tenantID, siteID uuid.UUID) (Config, error) {
	row, _, err := r.getConfigRow(ctx, tenantID, siteID)
	if err != nil {
		return Config{}, err
	}
	return configFromRow(row), nil
}

// GetCDNCredentialsCiphertext returns the encrypted CDN credentials blob and the
// stored provider for a site, or (nil, "", nil) when none configured. Operator
// read path (InTenantTx) — the SERVICE decrypts; the repo never decrypts.
func (r *Repo) GetCDNCredentialsCiphertext(ctx context.Context, tenantID, siteID uuid.UUID) (ciphertext []byte, provider string, err error) {
	row, found, gerr := r.getConfigRow(ctx, tenantID, siteID)
	if gerr != nil {
		if errors.Is(gerr, ErrNotFound) {
			return nil, "", nil
		}
		return nil, "", gerr
	}
	if !found {
		return nil, "", nil
	}
	return row.CdnCredentialsEncrypted, derefStr(row.CdnProvider), nil
}

func (r *Repo) getConfigRow(ctx context.Context, tenantID, siteID uuid.UUID) (sqlc.SitePerfConfig, bool, error) {
	var out sqlc.SitePerfConfig
	found := false
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row, qerr := sqlc.New(tx).GetPerfConfig(ctx, siteID)
		if qerr != nil {
			if errors.Is(qerr, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return qerr
		}
		out = row
		found = true
		return nil
	})
	if err != nil {
		return sqlc.SitePerfConfig{}, false, err
	}
	return out, found, nil
}

// UpsertConfigInput carries the operator-supplied config plus the (already
// encrypted) CDN credentials ciphertext. The agent-reported install-state columns
// are intentionally NOT settable here (UpsertPerfConfig leaves them untouched).
type UpsertConfigInput struct {
	Config Config
	// CDNCredentialsEncrypted is the age-encrypted credentials blob. nil leaves
	// the column unchanged ONLY if the caller passes the existing ciphertext;
	// the m36 UpsertPerfConfig always writes the column, so the service must pass
	// the prior ciphertext when the operator did not supply new credentials.
	CDNCredentialsEncrypted []byte
}

// UpsertConfig inserts-or-updates the per-site config. Operator write path
// (InTenantTx). Returns the stored config (without credentials).
func (r *Repo) UpsertConfig(ctx context.Context, in UpsertConfigInput) (Config, error) {
	c := in.Config
	var out sqlc.SitePerfConfig
	err := r.pool.InTenantTx(ctx, c.TenantID, func(tx pgx.Tx) error {
		row, qerr := sqlc.New(tx).UpsertPerfConfig(ctx, sqlc.UpsertPerfConfigParams{
			SiteID:                    c.SiteID,
			TenantID:                  c.TenantID,
			CacheEnabled:              c.CacheEnabled,
			CacheLoggedIn:             c.CacheLoggedIn,
			CacheMobile:               c.CacheMobile,
			CacheRefresh:              c.CacheRefresh,
			CacheRefreshInterval:      c.CacheRefreshInterval,
			CacheLinkPrefetch:         c.CacheLinkPrefetch,
			CacheBypassUrls:           coalesce(c.CacheBypassURLs),
			CacheBypassCookies:        coalesce(c.CacheBypassCookies),
			CacheIncludeQueries:       coalesce(c.CacheIncludeQueries),
			CacheIncludeCookies:       coalesce(c.CacheIncludeCookies),
			PreloadConcurrency:        int32(c.PreloadConcurrency),
			PreloadDelayMs:            int32(c.PreloadDelayMs),
			PreloadBatchSize:          int32(c.PreloadBatchSize),
			PreloadMaxLoad:            float32(c.PreloadMaxLoad),
			CssJsMinify:               c.CSSJSMinify,
			CssRucss:                  c.CSSRucss,
			CssRucssIncludeSelectors:  coalesce(c.CSSRucssIncludeSelectors),
			CssJsSelfHostThirdParty:   c.CSSJSSelfHostThirdParty,
			JsDelay:                   c.JSDelay,
			JsDelayMethod:             c.JSDelayMethod,
			JsDelayExcludes:           coalesce(c.JSDelayExcludes),
			JsDelayThirdParty:         c.JSDelayThirdParty,
			JsDelayThirdPartyExcludes: coalesce(c.JSDelayThirdPartyExcludes),
			FontsDisplaySwap:          c.FontsDisplaySwap,
			FontsOptimizeGoogle:       c.FontsOptimizeGoogle,
			FontsPreload:              c.FontsPreload,
			LazyLoad:                  c.LazyLoad,
			LazyLoadExclusions:        coalesce(c.LazyLoadExclusions),
			ProperlySizeImages:        c.ProperlySizeImages,
			YoutubePlaceholder:        c.YouTubePlaceholder,
			SelfHostGravatars:         c.SelfHostGravatars,
			CdnEnabled:                c.CDNEnabled,
			CdnUrl:                    strPtr(c.CDNURL),
			CdnFileTypes:              c.CDNFileTypes,
			CdnProvider:               strPtr(c.CDNProvider),
			CdnCredentialsEncrypted:   in.CDNCredentialsEncrypted,
			DbAutoClean:               c.DBAutoClean,
			DbAutoCleanInterval:       c.DBAutoCleanInterval,
			DbPostRevisions:           c.DBPostRevisions,
			DbPostAutoDrafts:          c.DBPostAutoDrafts,
			DbPostTrashed:             c.DBPostTrashed,
			DbCommentsSpam:            c.DBCommentsSpam,
			DbCommentsTrashed:         c.DBCommentsTrashed,
			DbTransientsExpired:       c.DBTransientsExpired,
			DbOptimizeTables:          c.DBOptimizeTables,
			BloatDisableBlockCss:      c.BloatDisableBlockCSS,
			BloatDisableDashicons:     c.BloatDisableDashicons,
			BloatDisableEmojis:        c.BloatDisableEmojis,
			BloatDisableJqueryMigrate: c.BloatDisableJQueryMig,
			BloatDisableXmlRpc:        c.BloatDisableXMLRPC,
			BloatDisableRssFeed:       c.BloatDisableRSSFeed,
			BloatDisableOembeds:       c.BloatDisableOembeds,
			BloatHeartbeatControl:     c.BloatHeartbeatControl,
			BloatPostRevisionsControl: c.BloatPostRevisionControl,
			ConfigVersion:             int32(c.ConfigVersion),
			WooCacheableSession:       c.WooCacheableSession,
			FontsTranscodeWoff2:       c.FontsTranscodeWOFF2,
			FontsSubset:               c.FontsSubset,
			FontsSubsetMode:           c.FontsSubsetMode,
			FontsSubsetRange:          c.FontsSubsetRange,
			RumEnabled:                c.RumEnabled,
			RumSampleRate:             float32(c.RumSampleRate),
			MaxDistinctCountries:      int32(c.MaxDistinctCountries),
			MinSampleCount:            int32(c.MinSampleCount),
		})
		if qerr != nil {
			return qerr
		}
		out = row
		return nil
	})
	if err != nil {
		return Config{}, err
	}
	return configFromRow(out), nil
}

// UpdateInstallState records the agent-observed server/install facts. Agent write
// path (InAgentTx). The tenant scoping is enforced by the agent identity + the
// site_id; the row's tenant_id is never changed here.
func (r *Repo) UpdateInstallState(ctx context.Context, siteID uuid.UUID, serverSoftware string, dropinInstalled, wpCacheConstantSet, htaccessManaged bool) error {
	return r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		_, qerr := sqlc.New(tx).UpdatePerfInstallState(ctx, sqlc.UpdatePerfInstallStateParams{
			ServerSoftware:     strPtr(serverSoftware),
			DropinInstalled:    dropinInstalled,
			WpCacheConstantSet: wpCacheConstantSet,
			HtaccessManaged:    htaccessManaged,
			SiteID:             siteID,
		})
		if qerr != nil {
			if errors.Is(qerr, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return qerr
		}
		return nil
	})
}

// UpdateBeaconKeyAcked records the agent's config-ack signal for whether it
// currently holds a non-empty rum_beacon_key (GH #174). Agent write path
// (InAgentTx). Returns needsReconcile=true when the row is rum-enabled with a
// hash already minted but the agent just reported present=false — the exact
// "hash committed, plaintext never delivered/lost" stuck state — so the caller
// can enqueue a re-mint reconcile job. Returns ErrNotFound when no config row
// exists yet for the site (the operator has never saved a config).
func (r *Repo) UpdateBeaconKeyAcked(ctx context.Context, siteID uuid.UUID, present bool) (needsReconcile bool, err error) {
	err = r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		row, qerr := sqlc.New(tx).UpdateBeaconKeyAcked(ctx, sqlc.UpdateBeaconKeyAckedParams{
			Present: present,
			SiteID:  siteID,
		})
		if qerr != nil {
			if errors.Is(qerr, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return qerr
		}
		needsReconcile = row.RumEnabled && row.BeaconKeyHash != nil && !present
		return nil
	})
	if err != nil {
		return false, err
	}
	return needsReconcile, nil
}

// UpdateWooFragmentsSupported stamps the agent-reported woo_theme_fragments_supported
// flag and woo_fragments_probed_at. Agent write path (InAgentTx) — the agent is
// the sole writer; operators can never set this via the API.
// Returns (rowsAffected, error); callers log when rowsAffected == 0 (no config
// row yet for the site; the agent will re-report after the operator saves one).
func (r *Repo) UpdateWooFragmentsSupported(ctx context.Context, siteID uuid.UUID, supported bool) (int64, error) {
	var n int64
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		rows, qerr := sqlc.New(tx).UpdateWooThemeFragmentsSupported(ctx, sqlc.UpdateWooThemeFragmentsSupportedParams{
			WooThemeFragmentsSupported: &supported,
			SiteID:                     siteID,
		})
		n = rows
		return qerr
	})
	return n, err
}

// GetCacheStats returns the latest reported cache gauges for a site. Operator
// read path (InTenantTx). Returns ErrNotFound when the agent has not reported yet.
func (r *Repo) GetCacheStats(ctx context.Context, tenantID, siteID uuid.UUID) (CacheStats, error) {
	var out CacheStats
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row, qerr := sqlc.New(tx).GetCacheStats(ctx, siteID)
		if qerr != nil {
			if errors.Is(qerr, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return qerr
		}
		out = cacheStatsFromRow(row)
		return nil
	})
	if err != nil {
		return CacheStats{}, err
	}
	return out, nil
}

// UpsertCacheStats overwrites the cache gauges for a site (no history). Agent
// write path (InAgentTx) — the agent is a cross-tenant system actor reporting
// for its own bound site; tenant_id is re-asserted from the verified identity.
func (r *Repo) UpsertCacheStats(ctx context.Context, s CacheStats) (CacheStats, error) {
	var out CacheStats
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		row, qerr := sqlc.New(tx).UpsertCacheStats(ctx, sqlc.UpsertCacheStatsParams{
			SiteID:           s.SiteID,
			TenantID:         s.TenantID,
			CachedPagesCount: int32(s.CachedPagesCount),
			CacheSizeBytes:   s.CacheSizeBytes,
			LastPurgedAt:     tsPtr(s.LastPurgedAt),
			LastPurgeKind:    strPtr(s.LastPurgeKind),
			LastPreloadAt:    tsPtr(s.LastPreloadAt),
			PreloadPending:   int32(s.PreloadPending),
			PreloadTotal:     int32(s.PreloadTotal),
		})
		if qerr != nil {
			return qerr
		}
		out = cacheStatsFromRow(row)
		return nil
	})
	if err != nil {
		return CacheStats{}, err
	}
	return out, nil
}

// MarkCachePurged stamps the "Last purge" gauge (site_cache_stats.last_purged_at
// + last_purge_kind) when an operator purge runs from the control plane. Operator
// write path (InTenantTx) — dashboard purges are tenant-scoped. The agent never
// reports a purge time, so this is the gauge's writer; UpsertCacheStats uses
// GREATEST so a later agent stats push cannot wipe or regress it. kind is the
// purge scope ("all" | "url").
func (r *Repo) MarkCachePurged(ctx context.Context, tenantID, siteID uuid.UUID, kind string) error {
	return r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		return sqlc.New(tx).MarkCachePurged(ctx, sqlc.MarkCachePurgedParams{
			SiteID:        siteID,
			TenantID:      tenantID,
			LastPurgeKind: strPtr(kind),
		})
	})
}

// RecordPurgeInput is one cache_purge_audit row.
type RecordPurgeInput struct {
	TenantID        uuid.UUID
	SiteID          uuid.UUID
	Kind            PurgeKind
	InitiatorUserID uuid.UUID // uuid.Nil for system-initiated
	TargetURLs      []string
}

// RecordPurge appends a cache_purge_audit entry. Operator write path
// (InTenantTx) — purges are operator-initiated through the dashboard.
func (r *Repo) RecordPurge(ctx context.Context, in RecordPurgeInput) (PurgeAuditEntry, error) {
	var out PurgeAuditEntry
	urls := coalesce(in.TargetURLs)
	err := r.pool.InTenantTx(ctx, in.TenantID, func(tx pgx.Tx) error {
		row, qerr := sqlc.New(tx).InsertCachePurgeAudit(ctx, sqlc.InsertCachePurgeAuditParams{
			TenantID:        in.TenantID,
			SiteID:          in.SiteID,
			Kind:            string(in.Kind),
			InitiatorUserID: uuidToPg(in.InitiatorUserID),
			TargetUrls:      urls,
			UrlsCount:       int32(len(urls)),
		})
		if qerr != nil {
			return qerr
		}
		out = purgeAuditFromRow(row)
		return nil
	})
	if err != nil {
		return PurgeAuditEntry{}, err
	}
	return out, nil
}

// ListPurgeAudit returns a page of purge-audit entries for a site, newest first.
// Operator read path (InTenantTx).
func (r *Repo) ListPurgeAudit(ctx context.Context, tenantID, siteID uuid.UUID, limit, offset int32) ([]PurgeAuditEntry, error) {
	if limit <= 0 {
		limit = 50
	}
	var out []PurgeAuditEntry
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, qerr := sqlc.New(tx).ListCachePurgeAuditForSite(ctx, sqlc.ListCachePurgeAuditForSiteParams{
			TenantID:  tenantID,
			SiteID:    siteID,
			RowOffset: offset,
			RowLimit:  limit,
		})
		if qerr != nil {
			return qerr
		}
		out = make([]PurgeAuditEntry, 0, len(rows))
		for _, row := range rows {
			out = append(out, purgeAuditFromRow(row))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// db-clean scheduling (M38)
// ---------------------------------------------------------------------------

// DueDBCleanSite is a minimal view of site_perf_config for the scheduler
// sweep — only the fields the worker needs to decide and advance the schedule.
type DueDBCleanSite struct {
	SiteID              uuid.UUID
	TenantID            uuid.UUID
	DBAutoCleanInterval string
	NextDBCleanAt       *time.Time
}

// GetDueDBCleanSites returns up to limit sites where db_auto_clean=true and
// next_db_clean_at IS NULL or <= now(). Cross-tenant (InAgentTx).
func (r *Repo) GetDueDBCleanSites(ctx context.Context, limit int) ([]DueDBCleanSite, error) {
	var out []DueDBCleanSite
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		rows, qerr := sqlc.New(tx).GetDueDBCleanSites(ctx, int32(limit))
		if qerr != nil {
			return qerr
		}
		out = make([]DueDBCleanSite, 0, len(rows))
		for _, row := range rows {
			out = append(out, DueDBCleanSite{
				SiteID:              row.SiteID,
				TenantID:            row.TenantID,
				DBAutoCleanInterval: row.DbAutoCleanInterval,
				NextDBCleanAt:       tsToTimePtr(row.NextDbCleanAt),
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// UpdateNextDBCleanAt advances next_db_clean_at after a clean job is dispatched.
// Cross-tenant (InAgentTx) — the sweeper runs as the agent actor.
func (r *Repo) UpdateNextDBCleanAt(ctx context.Context, siteID uuid.UUID, nextAt time.Time) error {
	return r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		return sqlc.New(tx).UpdateNextDBCleanAt(ctx, sqlc.UpdateNextDBCleanAtParams{
			SiteID:        siteID,
			NextDbCleanAt: pgtype.Timestamptz{Time: nextAt, Valid: true},
		})
	})
}

// SetActiveDBCleanJob stamps the in-flight db_clean watchdog columns.
// Runs under InAgentTx (the scheduled and operator paths both use this).
func (r *Repo) SetActiveDBCleanJob(ctx context.Context, siteID uuid.UUID, jobID string, startedAt time.Time) error {
	return r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		return sqlc.New(tx).SetActiveDBCleanJob(ctx, sqlc.SetActiveDBCleanJobParams{
			SiteID:               siteID,
			ActiveDbCleanJobID:   strPtr(jobID),
			ActiveDbCleanStarted: pgtype.Timestamptz{Time: startedAt, Valid: true},
		})
	})
}

// ClearActiveDBCleanJob clears the in-flight db_clean watchdog columns.
func (r *Repo) ClearActiveDBCleanJob(ctx context.Context, siteID uuid.UUID) error {
	return r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		return sqlc.New(tx).ClearActiveDBCleanJob(ctx, siteID)
	})
}

// SetActiveDBScanJob stamps the in-flight db_scan watchdog columns.
func (r *Repo) SetActiveDBScanJob(ctx context.Context, siteID uuid.UUID, jobID string, startedAt time.Time) error {
	return r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		return sqlc.New(tx).SetActiveDBScanJob(ctx, sqlc.SetActiveDBScanJobParams{
			SiteID:              siteID,
			ActiveDbScanJobID:   strPtr(jobID),
			ActiveDbScanStarted: pgtype.Timestamptz{Time: startedAt, Valid: true},
		})
	})
}

// ClearActiveDBScanJob clears the in-flight db_scan watchdog columns.
func (r *Repo) ClearActiveDBScanJob(ctx context.Context, siteID uuid.UUID) error {
	return r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		return sqlc.New(tx).ClearActiveDBScanJob(ctx, siteID)
	})
}

// ActiveDBScanState holds the watchdog columns read from site_perf_config for
// the GET /perf/db/scan response. Active is true when a scan is in progress.
type ActiveDBScanState struct {
	Active    bool
	JobID     string    // "" when Active is false
	StartedAt time.Time // zero when Active is false
}

// GetActiveDBScanState reads the active_db_scan_job_id and
// active_db_scan_started watchdog columns from site_perf_config for a single
// site. Returns ErrNotFound when the site has no perf config row yet (i.e. the
// perf suite has never been configured). The state is read under InTenantTx so
// RLS scopes it to the calling tenant.
func (r *Repo) GetActiveDBScanState(ctx context.Context, tenantID, siteID uuid.UUID) (ActiveDBScanState, error) {
	row, found, err := r.getConfigRow(ctx, tenantID, siteID)
	if err != nil {
		if err == ErrNotFound {
			return ActiveDBScanState{}, ErrNotFound
		}
		return ActiveDBScanState{}, err
	}
	if !found {
		return ActiveDBScanState{}, ErrNotFound
	}
	if row.ActiveDbScanJobID == nil {
		return ActiveDBScanState{}, nil
	}
	return ActiveDBScanState{
		Active:    true,
		JobID:     *row.ActiveDbScanJobID,
		StartedAt: row.ActiveDbScanStarted.Time,
	}, nil
}

// StalledDBCleanJob is a minimal view returned by the watchdog sweep.
type StalledDBCleanJob struct {
	SiteID   uuid.UUID
	TenantID uuid.UUID
	JobID    string
}

// GetStalledDBCleanJobs returns rows where active_db_clean_started is older
// than cleanThreshold. Cross-tenant (InAgentTx).
func (r *Repo) GetStalledDBCleanJobs(ctx context.Context, cleanThreshold time.Duration) ([]StalledDBCleanJob, error) {
	var out []StalledDBCleanJob
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		rows, qerr := sqlc.New(tx).GetStalledDBCleanJobs(ctx, durationToInterval(cleanThreshold))
		if qerr != nil {
			return qerr
		}
		out = make([]StalledDBCleanJob, 0, len(rows))
		for _, row := range rows {
			jobID := ""
			if row.ActiveDbCleanJobID != nil {
				jobID = *row.ActiveDbCleanJobID
			}
			out = append(out, StalledDBCleanJob{
				SiteID:   row.SiteID,
				TenantID: row.TenantID,
				JobID:    jobID,
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// StalledDBScanJob is a minimal view returned by the watchdog sweep.
type StalledDBScanJob struct {
	SiteID   uuid.UUID
	TenantID uuid.UUID
	JobID    string
}

// GetStalledDBScanJobs returns rows where active_db_scan_started is older
// than scanThreshold. Cross-tenant (InAgentTx).
func (r *Repo) GetStalledDBScanJobs(ctx context.Context, scanThreshold time.Duration) ([]StalledDBScanJob, error) {
	var out []StalledDBScanJob
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		rows, qerr := sqlc.New(tx).GetStalledDBScanJobs(ctx, durationToInterval(scanThreshold))
		if qerr != nil {
			return qerr
		}
		out = make([]StalledDBScanJob, 0, len(rows))
		for _, row := range rows {
			jobID := ""
			if row.ActiveDbScanJobID != nil {
				jobID = *row.ActiveDbScanJobID
			}
			out = append(out, StalledDBScanJob{
				SiteID:   row.SiteID,
				TenantID: row.TenantID,
				JobID:    jobID,
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// DBScanResultInput carries the parameters for upserting a scan result.
// Phase 2.1: TablesJSON carries the per-table inventory JSON alongside CategoriesJSON.
// Phase 3.3 (M41): OrphanedOptionsJSON, OrphanedCronJSON, InstalledPluginsJSON
//
//	carry the orphan-enumeration output from agents >= 0.16.0. nil/empty values
//	default to '[]' so rows from older agents remain safe to read.
type DBScanResultInput struct {
	SiteID         uuid.UUID
	TenantID       uuid.UUID
	JobID          string
	CategoriesJSON []byte
	// TablesJSON is the per-table inventory serialised as a JSON array of
	// DBScanTableInventoryRow objects (Phase 2.1). nil/empty ⇒ defaults to '[]'.
	TablesJSON []byte
	// OrphanedOptionsJSON is the []OrphanedOptionItem array serialised as JSON
	// (Phase 3.3). nil/empty ⇒ defaults to '[]' (backward-compat with agents < 0.16.0).
	OrphanedOptionsJSON []byte
	// OrphanedCronJSON is the []OrphanedCronItem array serialised as JSON
	// (Phase 3.3). nil/empty ⇒ defaults to '[]'.
	OrphanedCronJSON []byte
	// InstalledPluginsJSON is the []InstalledPluginItem snapshot serialised as JSON
	// (Phase 3.3). nil/empty ⇒ defaults to '[]'.
	InstalledPluginsJSON []byte
	DBSizeBytes          int64
	TableCount           int
	ScannedAt            time.Time
}

// UpsertDBScanResult persists (or refreshes) the latest db_scan result.
// Operator write path via InTenantTx (the scan is operator-triggered; the
// result is stored on behalf of the authenticated tenant).
// Phase 3.3: nil/empty orphan/plugin JSON slices default to '[]' so rows
// from agents < 0.16.0 (which omit those fields) are stored safely.
// Phase 3.4 (M42): also inserts a size-history data point inside the same
// transaction so the scan row and the history row land atomically.
func (r *Repo) UpsertDBScanResult(ctx context.Context, in DBScanResultInput) error {
	tablesJSON := in.TablesJSON
	if len(tablesJSON) == 0 {
		tablesJSON = []byte("[]")
	}
	orphanedOptionsJSON := in.OrphanedOptionsJSON
	if len(orphanedOptionsJSON) == 0 {
		orphanedOptionsJSON = []byte("[]")
	}
	orphanedCronJSON := in.OrphanedCronJSON
	if len(orphanedCronJSON) == 0 {
		orphanedCronJSON = []byte("[]")
	}
	installedPluginsJSON := in.InstalledPluginsJSON
	if len(installedPluginsJSON) == 0 {
		installedPluginsJSON = []byte("[]")
	}
	return r.pool.InTenantTx(ctx, in.TenantID, func(tx pgx.Tx) error {
		_, qerr := sqlc.New(tx).UpsertDBScanResult(ctx, sqlc.UpsertDBScanResultParams{
			SiteID:               in.SiteID,
			TenantID:             in.TenantID,
			JobID:                in.JobID,
			CategoriesJson:       in.CategoriesJSON,
			TablesJson:           tablesJSON,
			OrphanedOptionsJson:  orphanedOptionsJSON,
			OrphanedCronJson:     orphanedCronJSON,
			InstalledPluginsJson: installedPluginsJSON,
			DbSizeBytes:          in.DBSizeBytes,
			TableCount:           int32(in.TableCount),
			ScannedAt:            in.ScannedAt,
		})
		if qerr != nil {
			return qerr
		}
		// Append a size-history data point in the same transaction (M42).
		// ON CONFLICT DO NOTHING on (site_id, scanned_at) makes this
		// idempotent if the operator retriggers a scan within the same second.
		_, qerr = sqlc.New(tx).InsertDBSizeHistory(ctx, sqlc.InsertDBSizeHistoryParams{
			SiteID:      in.SiteID,
			TenantID:    in.TenantID,
			DbSizeBytes: in.DBSizeBytes,
			TableCount:  int32(in.TableCount),
			ScannedAt:   in.ScannedAt,
		})
		return qerr
	})
}

// RecordDBSizeHistoryFromDiagnostics appends a site_db_size_history data
// point sourced from the agent's DAILY diagnostics push rather than a manual
// "Scan database" run (GH #196). Before this tap, UpsertDBScanResult was the
// ONLY writer, so the 90-day trend + fleet growth read never populated unless
// an operator clicked "Scan database" — this taps the wp-paths-sizes size the
// agent already ships on every diagnostics push instead.
//
// table_count is carried forward from the most recent site_db_size_history
// row for the site (a prior manual scan or a prior diagnostics-sourced
// point); diagnostics does not carry a table inventory, so 0 is used only
// when no prior row exists at all. The chart plots db_size_bytes, so a
// carried-forward (or zero) table_count does not affect what operators see.
//
// Operator-tenant write path via InTenantTx — mirrors UpsertDBScanResult.
// Idempotent via the (site_id, scanned_at) unique constraint (ON CONFLICT DO
// NOTHING): passing the diagnostics push's own collected_at as scanned_at
// naturally dedups to one point per push.
func (r *Repo) RecordDBSizeHistoryFromDiagnostics(ctx context.Context, tenantID, siteID uuid.UUID, dbSizeBytes int64, scannedAt time.Time) error {
	return r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		tableCount, qerr := q.GetLatestDBSizeHistoryTableCount(ctx, sqlc.GetLatestDBSizeHistoryTableCountParams{
			SiteID:   siteID,
			TenantID: tenantID,
		})
		if qerr != nil {
			if !errors.Is(qerr, pgx.ErrNoRows) {
				return qerr
			}
			tableCount = 0
		}
		_, qerr = q.InsertDBSizeHistory(ctx, sqlc.InsertDBSizeHistoryParams{
			SiteID:      siteID,
			TenantID:    tenantID,
			DbSizeBytes: dbSizeBytes,
			TableCount:  tableCount,
			ScannedAt:   scannedAt,
		})
		return qerr
	})
}

// GetDBSizeHistory returns size-trend data points for a site from `since`
// onwards (up to 366 points), ordered oldest-first. Operator read path
// (InTenantTx).
func (r *Repo) GetDBSizeHistory(ctx context.Context, tenantID, siteID uuid.UUID, since time.Time) ([]DbSizeTrendPoint, error) {
	var out []DbSizeTrendPoint
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, qerr := sqlc.New(tx).GetDBSizeHistory(ctx, sqlc.GetDBSizeHistoryParams{
			SiteID:   siteID,
			TenantID: tenantID,
			Since:    since,
		})
		if qerr != nil {
			return qerr
		}
		out = make([]DbSizeTrendPoint, 0, len(rows))
		for _, row := range rows {
			out = append(out, DbSizeTrendPoint{
				ID:          row.ID,
				SiteID:      row.SiteID,
				TenantID:    row.TenantID,
				DBSizeBytes: row.DbSizeBytes,
				TableCount:  int(row.TableCount),
				ScannedAt:   row.ScannedAt,
				CreatedAt:   row.CreatedAt,
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetFleetDbHealth returns one FleetSiteDbSummary per scanned site within the
// tenant, ordered by db_size_bytes descending. Rows are gathered from
// site_db_scan_results (joined with sites for the name) and size_bounds from
// site_db_size_history for the growth calculation. Tenant-scoped via InTenantTx
// (RLS enforces the tenant_id constraint — never reads across tenants).
// `since` is the earliest history point to consider for growth computation.
func (r *Repo) GetFleetDbHealth(ctx context.Context, tenantID uuid.UUID, since time.Time) ([]FleetSiteDbSummary, error) {
	var out []FleetSiteDbSummary
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, qerr := sqlc.New(tx).GetFleetDbHealth(ctx, sqlc.GetFleetDbHealthParams{
			TenantID: tenantID,
			Since:    since,
		})
		if qerr != nil {
			return qerr
		}
		out = make([]FleetSiteDbSummary, 0, len(rows))
		for _, row := range rows {
			firstSize := toInt64Interface(row.FirstSizeBytes)
			lastSize := toInt64Interface(row.LastSizeBytes)
			growthBytes := lastSize - firstSize

			out = append(out, FleetSiteDbSummary{
				SiteID:               row.SiteID,
				SiteName:             row.SiteName,
				DBSizeBytes:          row.DbSizeBytes,
				TableCount:           int(row.TableCount),
				OrphanedOptionsCount: int(row.OrphanedOptionsCount),
				OrphanedCronCount:    int(row.OrphanedCronCount),
				ScannedAt:            row.ScannedAt,
				GrowthBytes:          growthBytes,
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// toInt64Interface safely coerces the interface{} values that sqlc emits for
// COALESCE expressions over nullable bigint columns. pgx scans bigint/int8 as
// int64 when the column is not null; the COALESCE expression over two possibly-
// null sources may arrive as int64 or int32 depending on the Postgres type
// inference. We handle both to be safe.
func toInt64Interface(v interface{}) int64 {
	if v == nil {
		return 0
	}
	switch t := v.(type) {
	case int64:
		return t
	case int32:
		return int64(t)
	case float64:
		return int64(t)
	default:
		return 0
	}
}

// PruneDBSizeHistory deletes size-history rows older than retention across all
// tenants. Cross-tenant write path (InAgentTx / app.agent GUC). Returns the
// count of deleted rows.
func (r *Repo) PruneDBSizeHistory(ctx context.Context, retention time.Duration) (int64, error) {
	cutoff := time.Now().UTC().Add(-retention)
	var deleted int64
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		ct, qerr := sqlc.New(tx).PruneDBSizeHistory(ctx, cutoff)
		if qerr != nil {
			return qerr
		}
		deleted = ct
		return nil
	})
	if err != nil {
		return 0, err
	}
	return deleted, nil
}

// ---------------------------------------------------------------------------
// site_cache_hit_ratio_history (M52 / #162)
// ---------------------------------------------------------------------------

// InsertCacheHitRatioHistoryTx appends one hit-ratio data point for a site
// under InTenantTx (tenant_isolation RLS policy). The ratioPct argument is
// pre-computed by the caller: round(100*hit/(hit+miss), 2).
// ON CONFLICT DO NOTHING on (site_id, sampled_at) ensures idempotency.
func (r *Repo) InsertCacheHitRatioHistoryTx(ctx context.Context, tenantID, siteID uuid.UUID, hitCount, missCount int64, ratioPct float64, sampledAt time.Time) error {
	// pgtype.Numeric for nullable numeric(5,2): encode the float64 as a big.Int
	// mantissa with the decimal exponent so pgx transmits it correctly.
	rpNum := pgtype.Numeric{}
	_ = rpNum.Scan(ratioPct)
	return r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := sqlc.New(tx).InsertCacheHitRatioHistory(ctx, sqlc.InsertCacheHitRatioHistoryParams{
			SiteID:    siteID,
			TenantID:  tenantID,
			HitCount:  hitCount,
			MissCount: missCount,
			RatioPct:  rpNum,
			SampledAt: sampledAt,
		})
		return err
	})
}

// GetCacheHitRatioHistory returns daily-aggregated hit-ratio trend data points
// for a site from `since` onwards (up to 366 points), ordered oldest-first. The
// SQL query aggregates hourly rows into one point per UTC calendar day, orders
// DESC to retrieve the most recent days first, and this mapper reverses the
// slice back to ASC so callers always receive chronological order. Operator read
// path (InTenantTx).
func (r *Repo) GetCacheHitRatioHistory(ctx context.Context, tenantID, siteID uuid.UUID, since time.Time) ([]CacheHitRatioPoint, error) {
	var out []CacheHitRatioPoint
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, qerr := sqlc.New(tx).GetCacheHitRatioHistory(ctx, sqlc.GetCacheHitRatioHistoryParams{
			SiteID:   siteID,
			TenantID: tenantID,
			Since:    since,
		})
		if qerr != nil {
			return qerr
		}
		// Rows arrive newest-first (ORDER BY … DESC). Collect and then reverse so
		// the output slice is ordered oldest-first for the chart consumer.
		out = make([]CacheHitRatioPoint, 0, len(rows))
		for _, row := range rows {
			rp := 0.0
			if row.RatioPct.Valid {
				f, _ := row.RatioPct.Float64Value()
				rp = f.Float64
			}
			out = append(out, CacheHitRatioPoint{
				HitCount:  row.HitCount,
				MissCount: row.MissCount,
				RatioPct:  rp,
				SampledAt: row.SampledAt,
			})
		}
		// Reverse DESC→ASC so the caller always receives chronological order.
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// PruneCacheHitRatioHistory deletes hit-ratio-history rows older than
// retention across all tenants. Cross-tenant write path (InAgentTx / app.agent
// GUC). Returns the count of deleted rows.
func (r *Repo) PruneCacheHitRatioHistory(ctx context.Context, retention time.Duration) (int64, error) {
	cutoff := time.Now().UTC().Add(-retention)
	var deleted int64
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		ct, qerr := sqlc.New(tx).PruneCacheHitRatioHistory(ctx, cutoff)
		if qerr != nil {
			return qerr
		}
		deleted = ct
		return nil
	})
	if err != nil {
		return 0, err
	}
	return deleted, nil
}

// DBScanResult is the model returned from a scan result lookup.
// Phase 2.1: TablesJSON holds the per-table inventory JSONB column.
// Phase 3.3 (M41): OrphanedOptionsJSON, OrphanedCronJSON, InstalledPluginsJSON
//
//	hold the orphan-enumeration columns. They default to '[]' for rows from
//	agents < 0.16.0 via the schema DEFAULT.
type DBScanResult struct {
	SiteID         uuid.UUID
	TenantID       uuid.UUID
	JobID          string
	CategoriesJSON []byte
	// TablesJSON holds the per-table inventory as serialised JSON (Phase 2.1).
	TablesJSON []byte
	// OrphanedOptionsJSON holds []OrphanedOptionItem as serialised JSON (Phase 3.3).
	// '[]' when agent < 0.16.0 omitted it.
	OrphanedOptionsJSON []byte
	// OrphanedCronJSON holds []OrphanedCronItem as serialised JSON (Phase 3.3).
	// '[]' when agent < 0.16.0 omitted it.
	OrphanedCronJSON []byte
	// InstalledPluginsJSON holds []InstalledPluginItem snapshot as serialised JSON (Phase 3.3).
	// '[]' when agent < 0.16.0 omitted it.
	InstalledPluginsJSON []byte
	DBSizeBytes          int64
	TableCount           int
	ScannedAt            time.Time
	CreatedAt            time.Time
}

// ---------------------------------------------------------------------------
// site_db_clean_results (M71)
// ---------------------------------------------------------------------------

// DBCleanResultInput carries the parameters for upserting a clean result.
// It is assembled from the final done=true progress push in HandleDBCleanProgress.
type DBCleanResultInput struct {
	SiteID   uuid.UUID
	TenantID uuid.UUID
	JobID    string
	// ResultJSON is the per-category {rows_deleted, bytes_freed, state} map
	// serialised as JSONB. nil/empty defaults to '{}'.
	ResultJSON  []byte
	RowsDeleted int64
	BytesFreed  int64
	CleanedAt   time.Time
}

// DBCleanResult is the model returned from a clean result lookup.
type DBCleanResult struct {
	SiteID      uuid.UUID
	TenantID    uuid.UUID
	JobID       string
	ResultJSON  []byte
	RowsDeleted int64
	BytesFreed  int64
	CleanedAt   time.Time
	CreatedAt   time.Time
}

// UpsertDBCleanResult persists (or refreshes) the latest db_clean result.
// Operator write path via InTenantTx (the clean is operator- or CP-triggered;
// the result is stored on behalf of the authenticated tenant).
func (r *Repo) UpsertDBCleanResult(ctx context.Context, in DBCleanResultInput) error {
	resultJSON := in.ResultJSON
	if len(resultJSON) == 0 {
		resultJSON = []byte("{}")
	}
	return r.pool.InTenantTx(ctx, in.TenantID, func(tx pgx.Tx) error {
		_, qerr := sqlc.New(tx).UpsertDBCleanResult(ctx, sqlc.UpsertDBCleanResultParams{
			SiteID:      in.SiteID,
			TenantID:    in.TenantID,
			JobID:       in.JobID,
			ResultJson:  resultJSON,
			RowsDeleted: in.RowsDeleted,
			BytesFreed:  in.BytesFreed,
			CleanedAt:   in.CleanedAt,
		})
		return qerr
	})
}

// GetDBCleanResult returns the latest clean result for a site.
// Returns ErrNotFound when no clean has been run yet.
func (r *Repo) GetDBCleanResult(ctx context.Context, tenantID, siteID uuid.UUID) (DBCleanResult, error) {
	var out DBCleanResult
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row, qerr := sqlc.New(tx).GetDBCleanResult(ctx, sqlc.GetDBCleanResultParams{
			SiteID:   siteID,
			TenantID: tenantID,
		})
		if qerr != nil {
			if errors.Is(qerr, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return qerr
		}
		out = DBCleanResult{
			SiteID:      row.SiteID,
			TenantID:    row.TenantID,
			JobID:       row.JobID,
			ResultJSON:  row.ResultJson,
			RowsDeleted: row.RowsDeleted,
			BytesFreed:  row.BytesFreed,
			CleanedAt:   row.CleanedAt,
			CreatedAt:   row.CreatedAt,
		}
		return nil
	})
	if err != nil {
		return DBCleanResult{}, err
	}
	return out, nil
}

// ActiveDBCleanState holds the watchdog columns read from site_perf_config for
// the GET /perf/db/clean response. Active is true when a clean is in progress.
type ActiveDBCleanState struct {
	Active    bool
	JobID     string    // "" when Active is false
	StartedAt time.Time // zero when Active is false
}

// GetActiveDBCleanState reads the active_db_clean_job_id and
// active_db_clean_started watchdog columns from site_perf_config for a single
// site. Returns ErrNotFound when the site has no perf config row yet (i.e. the
// perf suite has never been configured). The state is read under InTenantTx so
// RLS scopes it to the calling tenant.
func (r *Repo) GetActiveDBCleanState(ctx context.Context, tenantID, siteID uuid.UUID) (ActiveDBCleanState, error) {
	row, found, err := r.getConfigRow(ctx, tenantID, siteID)
	if err != nil {
		return ActiveDBCleanState{}, err
	}
	if !found {
		return ActiveDBCleanState{}, ErrNotFound
	}
	if row.ActiveDbCleanJobID == nil {
		return ActiveDBCleanState{}, nil
	}
	return ActiveDBCleanState{
		Active:    true,
		JobID:     *row.ActiveDbCleanJobID,
		StartedAt: row.ActiveDbCleanStarted.Time,
	}, nil
}

// GetDBScanResult returns the latest scan result for a site.
// Returns ErrNotFound when no scan has been run yet.
func (r *Repo) GetDBScanResult(ctx context.Context, tenantID, siteID uuid.UUID) (DBScanResult, error) {
	var out DBScanResult
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row, qerr := sqlc.New(tx).GetDBScanResult(ctx, sqlc.GetDBScanResultParams{
			SiteID:   siteID,
			TenantID: tenantID,
		})
		if qerr != nil {
			if errors.Is(qerr, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return qerr
		}
		out = DBScanResult{
			SiteID:               row.SiteID,
			TenantID:             row.TenantID,
			JobID:                row.JobID,
			CategoriesJSON:       row.CategoriesJson,
			TablesJSON:           row.TablesJson,
			OrphanedOptionsJSON:  row.OrphanedOptionsJson,
			OrphanedCronJSON:     row.OrphanedCronJson,
			InstalledPluginsJSON: row.InstalledPluginsJson,
			DBSizeBytes:          row.DbSizeBytes,
			TableCount:           int(row.TableCount),
			ScannedAt:            row.ScannedAt,
			CreatedAt:            row.CreatedAt,
		}
		return nil
	})
	if err != nil {
		return DBScanResult{}, err
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// row <-> model mapping
// ---------------------------------------------------------------------------

func configFromRow(row sqlc.SitePerfConfig) Config {
	return Config{
		SiteID:                     row.SiteID,
		TenantID:                   row.TenantID,
		CacheEnabled:               row.CacheEnabled,
		CacheLoggedIn:              row.CacheLoggedIn,
		CacheMobile:                row.CacheMobile,
		CacheRefresh:               row.CacheRefresh,
		CacheRefreshInterval:       row.CacheRefreshInterval,
		CacheLinkPrefetch:          row.CacheLinkPrefetch,
		CacheBypassURLs:            coalesce(row.CacheBypassUrls),
		CacheBypassCookies:         coalesce(row.CacheBypassCookies),
		CacheIncludeQueries:        coalesce(row.CacheIncludeQueries),
		CacheIncludeCookies:        coalesce(row.CacheIncludeCookies),
		PreloadConcurrency:         int(row.PreloadConcurrency),
		PreloadDelayMs:             int(row.PreloadDelayMs),
		PreloadBatchSize:           int(row.PreloadBatchSize),
		PreloadMaxLoad:             float64(row.PreloadMaxLoad),
		CSSJSMinify:                row.CssJsMinify,
		CSSRucss:                   row.CssRucss,
		CSSRucssIncludeSelectors:   coalesce(row.CssRucssIncludeSelectors),
		CSSJSSelfHostThirdParty:    row.CssJsSelfHostThirdParty,
		JSDelay:                    row.JsDelay,
		JSDelayMethod:              row.JsDelayMethod,
		JSDelayExcludes:            coalesce(row.JsDelayExcludes),
		JSDelayThirdParty:          row.JsDelayThirdParty,
		JSDelayThirdPartyExcludes:  coalesce(row.JsDelayThirdPartyExcludes),
		FontsDisplaySwap:           row.FontsDisplaySwap,
		FontsOptimizeGoogle:        row.FontsOptimizeGoogle,
		FontsPreload:               row.FontsPreload,
		LazyLoad:                   row.LazyLoad,
		LazyLoadExclusions:         coalesce(row.LazyLoadExclusions),
		ProperlySizeImages:         row.ProperlySizeImages,
		YouTubePlaceholder:         row.YoutubePlaceholder,
		SelfHostGravatars:          row.SelfHostGravatars,
		CDNEnabled:                 row.CdnEnabled,
		CDNURL:                     derefStr(row.CdnUrl),
		CDNFileTypes:               row.CdnFileTypes,
		CDNProvider:                derefStr(row.CdnProvider),
		CDNHasCredentials:          len(row.CdnCredentialsEncrypted) > 0,
		DBAutoClean:                row.DbAutoClean,
		DBAutoCleanInterval:        row.DbAutoCleanInterval,
		DBPostRevisions:            row.DbPostRevisions,
		DBPostAutoDrafts:           row.DbPostAutoDrafts,
		DBPostTrashed:              row.DbPostTrashed,
		DBCommentsSpam:             row.DbCommentsSpam,
		DBCommentsTrashed:          row.DbCommentsTrashed,
		DBTransientsExpired:        row.DbTransientsExpired,
		DBOptimizeTables:           row.DbOptimizeTables,
		NextDBCleanAt:              tsToTimePtr(row.NextDbCleanAt),
		BloatDisableBlockCSS:       row.BloatDisableBlockCss,
		BloatDisableDashicons:      row.BloatDisableDashicons,
		BloatDisableEmojis:         row.BloatDisableEmojis,
		BloatDisableJQueryMig:      row.BloatDisableJqueryMigrate,
		BloatDisableXMLRPC:         row.BloatDisableXmlRpc,
		BloatDisableRSSFeed:        row.BloatDisableRssFeed,
		BloatDisableOembeds:        row.BloatDisableOembeds,
		BloatHeartbeatControl:      row.BloatHeartbeatControl,
		BloatPostRevisionControl:   row.BloatPostRevisionsControl,
		ServerSoftware:             derefStr(row.ServerSoftware),
		DropinInstalled:            row.DropinInstalled,
		WPCacheConstantSet:         row.WpCacheConstantSet,
		HtaccessManaged:            row.HtaccessManaged,
		WooCacheableSession:        row.WooCacheableSession,
		WooThemeFragmentsSupported: row.WooThemeFragmentsSupported,
		WooFragmentsProbedAt:       tsToTimePtr(row.WooFragmentsProbedAt),
		FontsTranscodeWOFF2:        row.FontsTranscodeWoff2,
		FontsSubset:                row.FontsSubset,
		FontsSubsetMode:            row.FontsSubsetMode,
		FontsSubsetRange:           row.FontsSubsetRange,
		RumEnabled:                 row.RumEnabled,
		RumSampleRate:              float64(row.RumSampleRate),
		MaxDistinctCountries:       int(row.MaxDistinctCountries),
		MinSampleCount:             int(row.MinSampleCount),
		BeaconKeySet:               row.BeaconKeyHash != nil,
		BeaconKeyAckedPresent:      row.BeaconKeyAckedPresent,
		BeaconKeyAckedAt:           tsToTimePtr(row.BeaconKeyAckedAt),
		ConfigVersion:              int(row.ConfigVersion),
		CreatedAt:                  row.CreatedAt,
		UpdatedAt:                  row.UpdatedAt,
	}
}

func cacheStatsFromRow(row sqlc.SiteCacheStat) CacheStats {
	s := CacheStats{
		SiteID:           row.SiteID,
		TenantID:         row.TenantID,
		CachedPagesCount: int(row.CachedPagesCount),
		CacheSizeBytes:   row.CacheSizeBytes,
		LastPurgeKind:    derefStr(row.LastPurgeKind),
		PreloadPending:   int(row.PreloadPending),
		PreloadTotal:     int(row.PreloadTotal),
		ReportedAt:       row.ReportedAt,
	}
	if row.LastPurgedAt.Valid {
		t := row.LastPurgedAt.Time
		s.LastPurgedAt = &t
	}
	if row.LastPreloadAt.Valid {
		t := row.LastPreloadAt.Time
		s.LastPreloadAt = &t
	}
	return s
}

func purgeAuditFromRow(row sqlc.CachePurgeAudit) PurgeAuditEntry {
	e := PurgeAuditEntry{
		ID:         row.ID,
		TenantID:   row.TenantID,
		SiteID:     row.SiteID,
		Kind:       row.Kind,
		TargetURLs: coalesce(row.TargetUrls),
		URLsCount:  int(row.UrlsCount),
		CreatedAt:  row.CreatedAt,
	}
	if row.InitiatorUserID.Valid {
		e.InitiatorUserID = row.InitiatorUserID.Bytes
	}
	return e
}

// ---------------------------------------------------------------------------
// pgtype / pointer helpers
// ---------------------------------------------------------------------------

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func coalesce(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func uuidToPg(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: id != uuid.Nil}
}

func tsPtr(t *time.Time) pgtype.Timestamptz {
	if t == nil || t.IsZero() {
		return pgtype.Timestamptz{Valid: false}
	}
	return pgtype.Timestamptz{Time: *t, Valid: true}
}

func tsToTimePtr(ts pgtype.Timestamptz) *time.Time {
	if !ts.Valid {
		return nil
	}
	t := ts.Time
	return &t
}

// durationToInterval converts a time.Duration to a pgtype.Interval suitable
// for passing to the GetStalledDB* queries (::interval cast).
func durationToInterval(d time.Duration) pgtype.Interval {
	// pgtype.Interval stores microseconds in the Microseconds field.
	return pgtype.Interval{Microseconds: d.Microseconds(), Valid: true}
}

// ---------------------------------------------------------------------------
// P3.8 — orphan-delete watchdog columns (active_orphan_delete_job_id /
// active_orphan_delete_started on site_perf_config)
// ---------------------------------------------------------------------------

// SetActiveDBOrphanDeleteJob stamps the in-flight db_orphan_delete watchdog
// columns. Runs under InAgentTx.
func (r *Repo) SetActiveDBOrphanDeleteJob(ctx context.Context, siteID uuid.UUID, jobID string, startedAt time.Time) error {
	return r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE site_perf_config
			    SET active_orphan_delete_job_id  = $1,
			        active_orphan_delete_started = $2
			  WHERE site_id = $3`,
			jobID, startedAt, siteID,
		)
		return err
	})
}

// ClearActiveDBOrphanDeleteJob clears the in-flight db_orphan_delete watchdog
// columns. Runs under InAgentTx.
func (r *Repo) ClearActiveDBOrphanDeleteJob(ctx context.Context, siteID uuid.UUID) error {
	return r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE site_perf_config
			    SET active_orphan_delete_job_id  = NULL,
			        active_orphan_delete_started = NULL
			  WHERE site_id = $1`,
			siteID,
		)
		return err
	})
}

// StalledDBOrphanDeleteJob is a minimal view returned by the orphan-delete
// watchdog sweep.
type StalledDBOrphanDeleteJob struct {
	SiteID   uuid.UUID
	TenantID uuid.UUID
	JobID    string
}

// GetStalledDBOrphanDeleteJobs returns rows where
// active_orphan_delete_started is older than threshold. Cross-tenant
// (InAgentTx).
func (r *Repo) GetStalledDBOrphanDeleteJobs(ctx context.Context, threshold time.Duration) ([]StalledDBOrphanDeleteJob, error) {
	var out []StalledDBOrphanDeleteJob
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		rows, qerr := tx.Query(ctx,
			`SELECT site_id, tenant_id, active_orphan_delete_job_id
			   FROM site_perf_config
			  WHERE active_orphan_delete_started IS NOT NULL
			    AND active_orphan_delete_started < now() - $1::interval`,
			durationToInterval(threshold),
		)
		if qerr != nil {
			return qerr
		}
		defer rows.Close()
		for rows.Next() {
			var s StalledDBOrphanDeleteJob
			var jobID *string
			if serr := rows.Scan(&s.SiteID, &s.TenantID, &jobID); serr != nil {
				return serr
			}
			if jobID != nil {
				s.JobID = *jobID
			}
			out = append(out, s)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// font_results catalog (M55 — Phase 2 dashboard read-model)
// ---------------------------------------------------------------------------

// UpsertFontResultInput is the agent-supplied payload for one font catalog row.
// tenant_id + site_id MUST come from the verified agent identity — never from
// the body (same security invariant as font_transcode_results).
type UpsertFontResultInput struct {
	TenantID     uuid.UUID
	SiteID       uuid.UUID
	SourceHash   string
	Family       string
	SourceFile   string
	OriginalExt  string
	OriginalSize int
	Woff2Size    int    // 0 = not yet produced
	SubsetSize   int    // 0 = not yet produced
	UnicodeRange string // empty unless subset
	State        FontResultState
	ErrorDetail  string // non-empty when State == FontResultNegative
}

// UpsertFontResult inserts or updates a per-(site, source_hash) font result
// row. savings_pct is CP-derived from best output size vs original_size.
// Agent write path (InAgentTx).
func (r *Repo) UpsertFontResult(ctx context.Context, in UpsertFontResultInput) (FontResult, error) {
	var out sqlc.FontResult
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		var qerr error
		out, qerr = sqlc.New(tx).UpsertFontResult(ctx, sqlc.UpsertFontResultParams{
			TenantID:     in.TenantID,
			SiteID:       in.SiteID,
			SourceHash:   in.SourceHash,
			Family:       strPtr(in.Family),
			SourceFile:   strPtr(in.SourceFile),
			OriginalExt:  strPtr(in.OriginalExt),
			OriginalSize: int32Ptr(in.OriginalSize),
			Woff2Size:    int32Ptr(in.Woff2Size),
			SubsetSize:   int32Ptr(in.SubsetSize),
			UnicodeRange: strPtr(in.UnicodeRange),
			State:        string(in.State),
			ErrorDetail:  strPtr(in.ErrorDetail),
		})
		return qerr
	})
	if err != nil {
		return FontResult{}, err
	}
	return fontResultFromRow(out), nil
}

// ListFontResultsForSite returns a page of font catalog rows for the dashboard.
// Ordered by updated_at DESC, id DESC. Operator read path (InTenantTx).
func (r *Repo) ListFontResultsForSite(ctx context.Context, tenantID, siteID uuid.UUID, limit, offset int32) ([]FontResult, error) {
	if limit <= 0 {
		limit = 50
	}
	var out []FontResult
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, qerr := sqlc.New(tx).ListFontResultsForSite(ctx, sqlc.ListFontResultsForSiteParams{
			TenantID:  tenantID,
			SiteID:    siteID,
			RowLimit:  limit,
			RowOffset: offset,
		})
		if qerr != nil {
			return qerr
		}
		out = make([]FontResult, 0, len(rows))
		for _, row := range rows {
			out = append(out, fontResultFromRow(row))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// fontResultFromRow maps a sqlc.FontResult row to the domain FontResult type.
func fontResultFromRow(row sqlc.FontResult) FontResult {
	return FontResult{
		ID:           row.ID,
		TenantID:     row.TenantID,
		SiteID:       row.SiteID,
		SourceHash:   row.SourceHash,
		Family:       derefStr(row.Family),
		SourceFile:   derefStr(row.SourceFile),
		OriginalExt:  derefStr(row.OriginalExt),
		OriginalSize: derefInt32(row.OriginalSize),
		Woff2Size:    derefInt32(row.Woff2Size),
		SubsetSize:   derefInt32(row.SubsetSize),
		UnicodeRange: derefStr(row.UnicodeRange),
		State:        FontResultState(row.State),
		ErrorDetail:  derefStr(row.ErrorDetail),
		SavingsPct:   floatFromNumericPerf(row.SavingsPct),
		CreatedAt:    row.CreatedAt,
		UpdatedAt:    row.UpdatedAt,
	}
}

// int32Ptr converts an int to *int32, returning nil for zero values (so NULL is
// stored for unset optional sizes like Woff2Size before transcoding completes).
func int32Ptr(n int) *int32 {
	if n == 0 {
		return nil
	}
	v := int32(n)
	return &v
}

// derefInt32 dereferences a *int32 to int, returning 0 for nil.
func derefInt32(p *int32) int {
	if p == nil {
		return 0
	}
	return int(*p)
}

// floatFromNumericPerf converts a pgtype.Numeric back to a float64. An invalid
// (NULL) numeric maps to 0. Mirrors floatFromNumeric in rucss/repo.
func floatFromNumericPerf(n pgtype.Numeric) float64 {
	if !n.Valid || n.Int == nil {
		return 0
	}
	f := new(big.Float).SetInt(n.Int)
	scale := new(big.Float).SetFloat64(pow10Perf(int(n.Exp)))
	f.Mul(f, scale)
	out, _ := f.Float64()
	return out
}

// pow10Perf returns 10^exp as a float64 (handles negative exponents).
func pow10Perf(exp int) float64 {
	if exp == 0 {
		return 1
	}
	base := 10.0
	result := 1.0
	if exp < 0 {
		for i := 0; i < -exp; i++ {
			result /= base
		}
	} else {
		for i := 0; i < exp; i++ {
			result *= base
		}
	}
	return result
}
