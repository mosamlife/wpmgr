package perf

import (
	"context"
	"errors"
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
// row <-> model mapping
// ---------------------------------------------------------------------------

func configFromRow(row sqlc.SitePerfConfig) Config {
	return Config{
		SiteID:                    row.SiteID,
		TenantID:                  row.TenantID,
		CacheEnabled:              row.CacheEnabled,
		CacheLoggedIn:             row.CacheLoggedIn,
		CacheMobile:               row.CacheMobile,
		CacheRefresh:              row.CacheRefresh,
		CacheRefreshInterval:      row.CacheRefreshInterval,
		CacheLinkPrefetch:         row.CacheLinkPrefetch,
		CacheBypassURLs:           coalesce(row.CacheBypassUrls),
		CacheBypassCookies:        coalesce(row.CacheBypassCookies),
		CacheIncludeQueries:       coalesce(row.CacheIncludeQueries),
		CacheIncludeCookies:       coalesce(row.CacheIncludeCookies),
		PreloadConcurrency:        int(row.PreloadConcurrency),
		PreloadDelayMs:            int(row.PreloadDelayMs),
		PreloadBatchSize:          int(row.PreloadBatchSize),
		PreloadMaxLoad:            float64(row.PreloadMaxLoad),
		CSSJSMinify:               row.CssJsMinify,
		CSSRucss:                  row.CssRucss,
		CSSRucssIncludeSelectors:  coalesce(row.CssRucssIncludeSelectors),
		CSSJSSelfHostThirdParty:   row.CssJsSelfHostThirdParty,
		JSDelay:                   row.JsDelay,
		JSDelayMethod:             row.JsDelayMethod,
		JSDelayExcludes:           coalesce(row.JsDelayExcludes),
		JSDelayThirdParty:         row.JsDelayThirdParty,
		JSDelayThirdPartyExcludes: coalesce(row.JsDelayThirdPartyExcludes),
		FontsDisplaySwap:          row.FontsDisplaySwap,
		FontsOptimizeGoogle:       row.FontsOptimizeGoogle,
		FontsPreload:              row.FontsPreload,
		LazyLoad:                  row.LazyLoad,
		LazyLoadExclusions:        coalesce(row.LazyLoadExclusions),
		ProperlySizeImages:        row.ProperlySizeImages,
		YouTubePlaceholder:        row.YoutubePlaceholder,
		SelfHostGravatars:         row.SelfHostGravatars,
		CDNEnabled:                row.CdnEnabled,
		CDNURL:                    derefStr(row.CdnUrl),
		CDNFileTypes:              row.CdnFileTypes,
		CDNProvider:               derefStr(row.CdnProvider),
		CDNHasCredentials:         len(row.CdnCredentialsEncrypted) > 0,
		DBAutoClean:               row.DbAutoClean,
		DBAutoCleanInterval:       row.DbAutoCleanInterval,
		DBPostRevisions:           row.DbPostRevisions,
		DBPostAutoDrafts:          row.DbPostAutoDrafts,
		DBPostTrashed:             row.DbPostTrashed,
		DBCommentsSpam:            row.DbCommentsSpam,
		DBCommentsTrashed:         row.DbCommentsTrashed,
		DBTransientsExpired:       row.DbTransientsExpired,
		DBOptimizeTables:          row.DbOptimizeTables,
		BloatDisableBlockCSS:      row.BloatDisableBlockCss,
		BloatDisableDashicons:     row.BloatDisableDashicons,
		BloatDisableEmojis:        row.BloatDisableEmojis,
		BloatDisableJQueryMig:     row.BloatDisableJqueryMigrate,
		BloatDisableXMLRPC:        row.BloatDisableXmlRpc,
		BloatDisableRSSFeed:       row.BloatDisableRssFeed,
		BloatDisableOembeds:       row.BloatDisableOembeds,
		BloatHeartbeatControl:     row.BloatHeartbeatControl,
		BloatPostRevisionControl:  row.BloatPostRevisionsControl,
		ServerSoftware:            derefStr(row.ServerSoftware),
		DropinInstalled:           row.DropinInstalled,
		WPCacheConstantSet:        row.WpCacheConstantSet,
		HtaccessManaged:           row.HtaccessManaged,
		ConfigVersion:             int(row.ConfigVersion),
		CreatedAt:                 row.CreatedAt,
		UpdatedAt:                 row.UpdatedAt,
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
