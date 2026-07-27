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
		return AlertConfig{
			TenantID:            tenantID,
			EmailRecipients:     []string{},
			Enabled:             true,
			VulnMinSeverity:     VulnSeverityHigh,
			VulnIncludeInDigest: true,
		}, nil
	}
	return cfg, nil
}

// FleetSiteIDs returns the set of site IDs accessible to the principal for fleet
// queries. For org-scoped principals it returns all tenant site IDs; for
// site-scoped principals it returns p.AllowedSiteIDs.
func (s *Service) FleetSiteIDs(ctx context.Context, tenantID uuid.UUID, p domain.Principal) ([]uuid.UUID, error) {
	if p.Scope == domain.ScopeSite {
		return p.AllowedSiteIDs, nil
	}
	return s.verifier.ListSiteIDs(ctx, tenantID)
}

// GetFleetStatus returns the fleet-wide uptime status for the principal's
// accessible sites, with summary counts and per-site items.
//
// Data sourcing: Postgres-resident fields (name, url, connection_state,
// health_status, in_incident) come from the repo (GetFleetSiteInfo). Uptime
// metrics (up, last_probe_at, uptime_pct_7d, avg_latency_ms, tls_expiry) come
// from the metrics.Store (QueryFleetUptime) — a single batch query per
// backend. This ensures ClickHouse deployments return real data instead of all-
// null results: previously the service read from Postgres site_uptime_probes
// directly, which is never written by the ClickHouse path.
func (s *Service) GetFleetStatus(ctx context.Context, tenantID uuid.UUID, siteIDs []uuid.UUID) (FleetStatusResponse, error) {
	// 1. Fetch Postgres-resident site fields (one query, InTenantTx/RLS).
	infos, err := s.repo.GetFleetSiteInfo(ctx, tenantID, siteIDs)
	if err != nil {
		return FleetStatusResponse{}, err
	}

	// 2. Fetch uptime metrics from the active store (ClickHouse or Postgres) in
	//    a single batch query — avoids N+1 per-site round-trips.
	const window7d = 7 * 24 * time.Hour
	uptimeMap, err := s.store.QueryFleetUptime(ctx, tenantID, siteIDs, window7d)
	if err != nil {
		return FleetStatusResponse{}, domain.Internal("fleet_uptime_metrics_failed", "failed to query fleet uptime metrics").WithCause(err)
	}

	// 3. Merge: build FleetStatusItem per site, deriving status from store data.
	items := make([]FleetStatusItem, 0, len(infos))
	for _, info := range infos {
		item := FleetStatusItem{
			SiteID:           info.SiteID,
			Name:             info.Name,
			URL:              info.URL,
			ConnectionState:  info.ConnectionState,
			HealthStatus:     info.HealthStatus,
			InIncident:       info.InIncident,
			LatencySparkline: []float64{},
		}

		if um, ok := uptimeMap[info.SiteID]; ok {
			item.Up = um.Up
			item.LastProbeAt = um.LastProbeAt
			item.TLSExpiry = um.TLSExpiry
			if um.UptimePct7d != nil {
				item.UptimePct7d = *um.UptimePct7d
			}
			item.AvgLatencyMs = um.AvgLatencyMs
			// Derive total_ms pointer for deriveFleetStatus threshold check.
			var totalMsPtr *float64
			if um.AvgLatencyMs != nil {
				totalMsPtr = um.AvgLatencyMs
			}
			item.Status, item.StatusReason = deriveFleetStatus(um.Up, totalMsPtr, info.ConnectionState, info.DisconnectedReason)
		} else {
			item.Status = FleetStatusUnknown
		}

		items = append(items, item)
	}

	var resp FleetStatusResponse
	resp.Items = items
	resp.Summary = FleetStatusCounts{}
	for _, it := range items {
		switch it.Status {
		case FleetStatusUp:
			resp.Summary.Up++
		case FleetStatusDegraded:
			resp.Summary.Degraded++
		case FleetStatusDown:
			resp.Summary.Down++
		default:
			resp.Summary.Unknown++
		}
	}
	return resp, nil
}

// GetFleetIncidents returns open incidents and recently-alerted sites for the
// principal's accessible sites.
//
// LIMITATION: site_alert_state stores only the CURRENT transition memory.
// Full historical incident logs are not persisted; this endpoint returns open
// incidents (in_incident=true) and recently-alerted sites (last_alert_at >=
// since). ended_at/duration_seconds are estimated from state updated_at for
// closed incidents, not from a true incident-close record.
func (s *Service) GetFleetIncidents(ctx context.Context, tenantID uuid.UUID, siteIDs []uuid.UUID, since time.Time, limit int) ([]FleetIncidentItem, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	return s.repo.GetFleetIncidents(ctx, tenantID, siteIDs, since, limit)
}

// incidentProbeWindowLimit bounds the number of raw probe rows the
// incident-detail endpoint renders in its timeline.
const incidentProbeWindowLimit = 40

// GetIncidentSummary returns the tenant-scoped incident row (without the
// probe timeline). The handler calls this FIRST so it can authorize a
// site-scoped principal against SiteID before spending a metrics-store
// round-trip on a request that may end up denied.
func (s *Service) GetIncidentSummary(ctx context.Context, tenantID, incidentID uuid.UUID) (IncidentSummary, error) {
	summary, found, err := s.repo.GetIncidentByID(ctx, tenantID, incidentID)
	if err != nil {
		return IncidentSummary{}, err
	}
	if !found {
		return IncidentSummary{}, domain.NotFound("incident_not_found", "incident not found")
	}
	return summary, nil
}

// GetIncidentDetail assembles the full incident-detail DTO from an
// already-authorized IncidentSummary: a probe timeline over the incident's
// window (metrics.Store — degrades gracefully to an empty slice when the
// window has no data) and the 30-day flapping count.
func (s *Service) GetIncidentDetail(ctx context.Context, tenantID uuid.UUID, summary IncidentSummary) (IncidentDetail, error) {
	to := time.Now()
	if summary.EndedAt != nil {
		to = *summary.EndedAt
	}
	samples, err := s.store.QueryProbeWindow(ctx, tenantID, summary.SiteID, summary.StartedAt, to, incidentProbeWindowLimit)
	if err != nil {
		return IncidentDetail{}, domain.Internal("incident_probe_window_failed", "failed to query incident probe timeline").WithCause(err)
	}

	count, err := s.repo.CountRecentIncidents(ctx, tenantID, summary.SiteID)
	if err != nil {
		return IncidentDetail{}, err
	}

	det := IncidentDetail{
		ID:               summary.ID,
		SiteID:           summary.SiteID,
		Name:             summary.SiteName,
		URL:              summary.SiteURL,
		StartedAt:        summary.StartedAt,
		EndedAt:          summary.EndedAt,
		Ongoing:          summary.EndedAt == nil,
		PeakStatus:       summary.PeakStatus,
		LastHTTPStatus:   summary.LastHTTPStatus,
		Reason:           summary.Reason,
		IncidentCount30d: int(count),
		Probes:           make([]IncidentProbe, 0, len(samples)),
		// ProbesTruncated is set when the store returned a FULL page — there
		// may be more probes in the window than we asked for/rendered.
		ProbesTruncated: len(samples) >= incidentProbeWindowLimit,
	}
	if summary.EndedAt != nil {
		dur := int64(summary.EndedAt.Sub(summary.StartedAt).Seconds())
		if dur < 0 {
			dur = 0
		}
		det.DurationSeconds = &dur
	}
	for _, p := range samples {
		det.Probes = append(det.Probes, IncidentProbe{
			ProbedAt:   p.ProbedAt,
			Up:         p.Up,
			HTTPStatus: int(p.HTTPStatus),
			TotalMs:    p.TotalMs,
			Error:      p.Error,
		})
	}
	return det, nil
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
	if !ValidVulnMinSeverity(cfg.VulnMinSeverity) {
		return AlertConfig{}, domain.Validation("invalid_vuln_min_severity",
			"vuln_min_severity must be one of: critical, high, medium, low")
	}
	return s.repo.UpsertAlertConfig(ctx, cfg)
}
