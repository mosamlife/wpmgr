package uptime

import (
	"context"
	"net/url"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/metrics"
)

// SiteVerifier verifies a site belongs to the caller's tenant (a Postgres,
// RLS-scoped lookup) BEFORE any ClickHouse query is issued. This is the tenant
// boundary for the uptime API: ClickHouse is then queried by tenant_id+site_id.
type SiteVerifier interface {
	// VerifySite returns the site's name and whether it exists in the tenant. A
	// not-found returns ok=false (the handler maps it to 404).
	VerifySite(ctx context.Context, tenantID, siteID uuid.UUID) (name string, ok bool, err error)
	// ListSiteIDs returns the IDs of all sites in the tenant (for the summary).
	ListSiteIDs(ctx context.Context, tenantID uuid.UUID) ([]uuid.UUID, error)
}

// SummaryItem is the current up/down snapshot for one site in the summary.
type SummaryItem struct {
	SiteID     uuid.UUID
	Up         bool
	HTTPStatus int
	LastCheck  *time.Time
	TLSExpiry  *time.Time
	Found      bool
}

// UptimeReport is the windowed uptime status for one site.
type UptimeReport struct {
	SiteID       uuid.UUID
	Window       time.Duration
	UptimePct    float64
	AvgLatencyMs float64
	Checks       uint64
	Up           bool
	LastCheck    *time.Time
	TLSExpiry    *time.Time
	TLSIssuer    string
	TLSSubject   string
	Series       []metrics.Point
}

// Service serves the tenant-scoped uptime reads and the alert-config CRUD. It
// composes the Postgres repo (tenant verification + config) and the metrics
// store (time-series — backed by ClickHouse or Postgres depending on
// deployment), always verifying tenant ownership in Postgres before querying
// the metrics backend.
type Service struct {
	repo     Repo
	store    metrics.Store
	verifier SiteVerifier
}

// NewService builds the uptime Service.
func NewService(repo Repo, store metrics.Store, verifier SiteVerifier) *Service {
	return &Service{repo: repo, store: store, verifier: verifier}
}

// Uptime returns the windowed uptime report for a site. It first verifies the
// site belongs to tenantID (Postgres/RLS) — a foreign site yields a 404 — then
// queries ClickHouse scoped by tenant_id+site_id.
func (s *Service) Uptime(ctx context.Context, tenantID, siteID uuid.UUID, window time.Duration, seriesBuckets int) (UptimeReport, error) {
	if _, ok, err := s.verifier.VerifySite(ctx, tenantID, siteID); err != nil {
		return UptimeReport{}, err
	} else if !ok {
		return UptimeReport{}, domain.NotFound("site_not_found", "site not found")
	}

	rep := UptimeReport{SiteID: siteID, Window: window}
	agg, err := s.store.QueryAggregate(ctx, tenantID, siteID, window)
	if err != nil {
		return UptimeReport{}, domain.Internal("uptime_query_failed", "failed to query uptime metrics").WithCause(err)
	}
	rep.UptimePct = agg.UptimePct
	rep.AvgLatencyMs = agg.AvgLatencyMs
	rep.Checks = agg.Checks

	latest, err := s.store.QueryLatest(ctx, tenantID, siteID)
	if err != nil {
		return UptimeReport{}, domain.Internal("uptime_query_failed", "failed to query latest uptime").WithCause(err)
	}
	if latest.Found {
		rep.Up = latest.Up
		lc := latest.CheckedAt
		rep.LastCheck = &lc
		if !latest.TLSExpiry.IsZero() {
			te := latest.TLSExpiry
			rep.TLSExpiry = &te
		}
		rep.TLSIssuer = latest.TLSIssuer
		rep.TLSSubject = latest.TLSSubject
	}

	series, err := s.store.QuerySeries(ctx, tenantID, siteID, window, seriesBuckets)
	if err != nil {
		return UptimeReport{}, domain.Internal("uptime_query_failed", "failed to query uptime series").WithCause(err)
	}
	rep.Series = series
	return rep, nil
}

// Summary returns the current up/down status for every site in the tenant
// (latest recorded probe per site). Sites are enumerated from Postgres (RLS);
// per-site latest status comes from ClickHouse scoped by tenant_id+site_id.
func (s *Service) Summary(ctx context.Context, tenantID uuid.UUID) ([]SummaryItem, error) {
	ids, err := s.verifier.ListSiteIDs(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	out := make([]SummaryItem, 0, len(ids))
	for _, id := range ids {
		latest, qerr := s.store.QueryLatest(ctx, tenantID, id)
		if qerr != nil {
			return nil, domain.Internal("uptime_query_failed", "failed to query uptime summary").WithCause(qerr)
		}
		item := SummaryItem{SiteID: id, Found: latest.Found}
		if latest.Found {
			item.Up = latest.Up
			item.HTTPStatus = int(latest.HTTPStatus)
			lc := latest.CheckedAt
			item.LastCheck = &lc
			if !latest.TLSExpiry.IsZero() {
				te := latest.TLSExpiry
				item.TLSExpiry = &te
			}
		}
		out = append(out, item)
	}
	return out, nil
}

// GetAlertConfig returns the tenant's alert config. When none exists yet it
// returns a zero-value enabled config (the tenant simply hasn't set recipients).
func (s *Service) GetAlertConfig(ctx context.Context, tenantID uuid.UUID) (AlertConfig, error) {
	cfg, found, err := s.repo.GetAlertConfig(ctx, tenantID)
	if err != nil {
		return AlertConfig{}, err
	}
	if !found {
		return AlertConfig{TenantID: tenantID, EmailRecipients: []string{}, Enabled: true}, nil
	}
	return cfg, nil
}

// SaveAlertConfig validates and upserts the tenant's alert config.
func (s *Service) SaveAlertConfig(ctx context.Context, cfg AlertConfig) (AlertConfig, error) {
	if len(cfg.EmailRecipients) > 50 {
		return AlertConfig{}, domain.Validation("too_many_recipients", "at most 50 email recipients are allowed")
	}
	if cfg.WebhookURL != "" {
		// Reject non-http(s) schemes (file://, gopher://, etc.). The SSRF client
		// also blocks them at dial, but rejecting at write-time keeps the registry
		// clean and gives the operator a clear error.
		u, err := url.Parse(cfg.WebhookURL)
		if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") {
			return AlertConfig{}, domain.Validation("webhook_url_scheme", "webhook_url must be an http or https URL")
		}
	}
	return s.repo.UpsertAlertConfig(ctx, cfg)
}
