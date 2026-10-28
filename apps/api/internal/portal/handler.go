// Package portal serves the m66 read-only client portal endpoints.
//
// All routes live under /api/v1/portal and are gated by
// authz.RequireClientPortal() (session user resolved via client_members, no
// write permissions). Per-site sub-routes additionally carry
// authz.RequireSiteAccess("siteId"). No portal route takes a :clientId path
// parameter — client identity is derived from p.ClientIDs (eliminates IDOR).
package portal

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/backup"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/metrics"
	"github.com/mosamlife/wpmgr/apps/api/internal/report"
	"github.com/mosamlife/wpmgr/apps/api/internal/rum"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
	"github.com/mosamlife/wpmgr/apps/api/internal/uptime"
)

// SiteService is the subset of site.Service needed by the portal.
type SiteService interface {
	List(ctx context.Context, in site.ListInput) ([]site.Site, error)
}

// UptimeService is the subset of uptime.Service needed by the portal.
type UptimeService interface {
	Uptime(ctx context.Context, tenantID, siteID uuid.UUID, window time.Duration, seriesBuckets int) (uptime.UptimeReport, error)
}

// BackupRepo is the subset of backup.Repo needed by the portal.
type BackupRepo interface {
	ListSnapshotsForSite(ctx context.Context, tenantID, siteID uuid.UUID, limit, offset int32) ([]backup.Snapshot, error)
}

// ReportService is the subset of report.Service needed by the portal.
type ReportService interface {
	PresignReportURLs(ctx context.Context, rpt report.GeneratedReport) (string, string)
}

// RumStore is the subset of rum.Store needed by the portal.
type RumStore interface {
	GetDailyRollups(ctx context.Context, siteID, tenantID uuid.UUID, sinceDay time.Time) ([]rum.DailyRollup, error)
	ComputeP75(rollups []rum.HourlyRollup, minSampleCount int) []rum.P75Result
}

// MetricsStore is the subset of metrics.Store needed by the portal for cheap
// per-site aggregate queries. Used by listSites (uptime_30d_pct + tls_expires_at)
// and summary (fleet-level uptime daily series via the report aggregator path).
// We do NOT use uptime.Service.Uptime here — that always calls QuerySeries
// which defaults to 100 buckets per site, too expensive for a fleet list.
type MetricsStore interface {
	QueryAggregate(ctx context.Context, tenantID, siteID uuid.UUID, window time.Duration) (metrics.Aggregate, error)
	QueryLatest(ctx context.Context, tenantID, siteID uuid.UUID) (metrics.Latest, error)
}

// Handler serves the portal read-only endpoints.
type Handler struct {
	pool          *db.Pool
	sites         SiteService
	uptimeSvc     UptimeService
	backupRepo    BackupRepo
	reportSvc     ReportService
	rumStore      RumStore
	metricsStore  MetricsStore    // nil when uptime store is unavailable
	reportSources *report.Sources // nil when report blob store is unavailable
}

// NewHandler builds the portal handler.
func NewHandler(pool *db.Pool, sites SiteService, uptimeSvc UptimeService, backupRepo BackupRepo, reportSvc ReportService, rumStore RumStore) *Handler {
	return &Handler{
		pool:       pool,
		sites:      sites,
		uptimeSvc:  uptimeSvc,
		backupRepo: backupRepo,
		reportSvc:  reportSvc,
		rumStore:   rumStore,
	}
}

// SetMetricsStore wires the metrics store for cheap per-site aggregate queries.
// Called from main.go after NewHandler when the metrics store is available.
func (h *Handler) SetMetricsStore(ms MetricsStore) {
	h.metricsStore = ms
}

// SetReportSources wires the report aggregator sources for the /summary endpoint.
// Called from main.go after NewHandler when the report blob store is available.
// The caller MUST nil out Sources.GetFleetStatsBySite before passing (email stats
// are never exposed in the portal per the security contract).
func (h *Handler) SetReportSources(src report.Sources) {
	cp := src
	cp.GetFleetStatsBySite = nil // enforce: email source never exposed in portal
	// The summary shows update counts only, so the per-site version-table
	// query (GH #969) would be read and thrown away on every request.
	cp.ListReportUpdateItems = nil
	h.reportSources = &cp
}

const (
	// sitePageSize is the most rows one site.Service.List call returns; a
	// larger Limit is clamped to it, not refused.
	sitePageSize = 200
	// maxSitePages bounds the walk, so a list that ignored its offset could
	// not loop for ever.
	maxSitePages = 100
)

// listAllSites reads every site the principal can see, one page at a time.
// Every page is requested with the same principal, which is what restricts the
// rows to the sites it may see; only the offset changes. The summary uses it
// to look up each report site's connection state and last backup, and a single
// call stops at one page, so every site past the first 200 looked disconnected
// with no backup.
func listAllSites(ctx context.Context, sites SiteService, tenantID uuid.UUID, principal site.ScopedPrincipal) ([]site.Site, error) {
	seen := make(map[uuid.UUID]struct{})
	out := []site.Site{}
	for page := 0; page < maxSitePages; page++ {
		rows, err := sites.List(ctx, site.ListInput{
			TenantID:  tenantID,
			Limit:     sitePageSize,
			Offset:    int32(page * sitePageSize),
			Principal: principal,
		})
		if err != nil {
			return nil, err
		}
		for _, s := range rows {
			if _, dup := seen[s.ID]; dup {
				continue
			}
			seen[s.ID] = struct{}{}
			out = append(out, s)
		}
		if len(rows) < sitePageSize {
			return out, nil
		}
	}
	return nil, fmt.Errorf("portal: more than %d sites in scope", sitePageSize*maxSitePages)
}

// Register mounts portal routes on the authenticated v1 group.
// The portal group gate (RequireClientPortal) requires session + auth +
// RequireTenant (from v1) to have already run.
func (h *Handler) Register(r *gin.RouterGroup) {
	g := r.Group("/portal", authz.RequireClientPortal())
	g.GET("/overview", h.overview)
	g.GET("/summary", h.summary)
	g.GET("/sites", h.listSites)
	g.GET("/reports", h.listReports)
	g.GET("/reports/:reportId/download", h.downloadReport)

	perSite := g.Group("/sites/:siteId", authz.RequireSiteAccess("siteId"))
	perSite.GET("/uptime", h.siteUptime)
	perSite.GET("/backups", h.siteBackups)
	perSite.GET("/updates", h.siteUpdates)
	perSite.GET("/vitals", h.siteVitals)
}

// ---------------------------------------------------------------------------
// DTOs
// ---------------------------------------------------------------------------

type overviewClientDTO struct {
	ID      string  `json:"id"`
	Name    string  `json:"name"`
	LogoURL *string `json:"logo_url,omitempty"`
	Color   *string `json:"color,omitempty"`
}

type overviewDTO struct {
	Client      overviewClientDTO `json:"client"`
	AgencyName  string            `json:"agency_name"`
	SiteCount   int               `json:"site_count"`
	ReportCount int               `json:"report_count"`
}

type portalSiteDTO struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	URL          string   `json:"url"`
	Status       string   `json:"status"`
	LastBackupAt *string  `json:"last_backup_at,omitempty"`
	Uptime30dPct *float64 `json:"uptime_30d_pct,omitempty"`
	TLSExpiresAt *string  `json:"tls_expires_at,omitempty"`
}

type portalSiteListDTO struct {
	Items []portalSiteDTO `json:"items"`
}

type incidentDTO struct {
	StartedAt       string `json:"started_at"`
	EndedAt         string `json:"ended_at,omitempty"`
	DurationSeconds int64  `json:"duration_seconds"`
}

type portalUptimeDTO struct {
	Range        string        `json:"range"`
	UptimePct    float64       `json:"uptime_pct"`
	AvgLatencyMs float64       `json:"avg_latency_ms"`
	TLSExpiresAt *string       `json:"tls_expires_at,omitempty"`
	Incidents    []incidentDTO `json:"incidents"`
}

type portalBackupItemDTO struct {
	ID          string  `json:"id"`
	Kind        string  `json:"kind"`
	Status      string  `json:"status"`
	SizeBytes   int64   `json:"size_bytes"`
	CreatedAt   string  `json:"created_at"`
	CompletedAt *string `json:"completed_at,omitempty"`
}

type portalBackupListDTO struct {
	Items []portalBackupItemDTO `json:"items"`
}

type portalUpdateItemDTO struct {
	Type        string  `json:"type"`
	Name        string  `json:"name"`
	FromVersion string  `json:"from_version"`
	ToVersion   string  `json:"to_version"`
	Status      string  `json:"status"`
	FinishedAt  *string `json:"finished_at,omitempty"`
}

type portalUpdateListDTO struct {
	Items []portalUpdateItemDTO `json:"items"`
}

type portalVitalMetricDTO struct {
	Metric  string  `json:"metric"`
	P75     float64 `json:"p75"`
	Rating  string  `json:"rating"`
	Samples int64   `json:"samples"`
}

type portalVitalsDTO struct {
	Range   string                 `json:"range"`
	Metrics []portalVitalMetricDTO `json:"metrics"`
}

type portalReportItemDTO struct {
	ID          string  `json:"id"`
	ClientID    string  `json:"client_id"`
	PeriodStart string  `json:"period_start"`
	PeriodEnd   string  `json:"period_end"`
	CreatedAt   string  `json:"created_at"`
	CompletedAt *string `json:"completed_at,omitempty"`
}

type portalReportListDTO struct {
	Items []portalReportItemDTO `json:"items"`
}

type portalDownloadDTO struct {
	URL       string `json:"url"`
	ExpiresAt string `json:"expires_at"`
}

// ---------------------------------------------------------------------------
// Portal summary DTOs (exhaustive — no extra fields per security contract)
// ---------------------------------------------------------------------------

type portalUptimeDayDTO struct {
	Day       string  `json:"day"`        // YYYY-MM-DD
	UptimePct float64 `json:"uptime_pct"` // 0–100
}

type portalVitalsRatingCountDTO struct {
	Good            int `json:"good"`
	NeedsImprovement int `json:"needs_improvement"`
	Poor             int `json:"poor"`
}

type portalVitalsDistributionDTO struct {
	LCP portalVitalsRatingCountDTO `json:"lcp"`
	INP portalVitalsRatingCountDTO `json:"inp"`
	CLS portalVitalsRatingCountDTO `json:"cls"`
}

type portalSummaryTotalsDTO struct {
	SiteCount      int      `json:"site_count"`
	AvgUptimePct   *float64 `json:"avg_uptime_pct"`   // null when no checks
	Incidents      int      `json:"incidents"`
	BackupsCount   int64    `json:"backups_count"`
	UpdatesApplied int64    `json:"updates_applied"`
	UpdatesFailed  int64    `json:"updates_failed"`
}

type portalSummarySiteDTO struct {
	ID              string               `json:"id"`
	Name            string               `json:"name"`
	URL             string               `json:"url"`
	Status          string               `json:"status"`
	UptimePct       *float64             `json:"uptime_pct"`        // null when no checks
	UptimeDaily     []portalUptimeDayDTO `json:"uptime_daily"`
	Incidents       int                  `json:"incidents"`
	LastBackupAt    *string              `json:"last_backup_at"`    // null when none
	BackupsInPeriod int64                `json:"backups_in_period"`
	UpdatesInPeriod int64                `json:"updates_in_period"`
	VitalsRating    *string              `json:"vitals_rating"`     // null when no samples
	TLSExpiresAt    *string              `json:"tls_expires_at"`    // null when unknown
}

type portalSummaryLatestReportDTO struct {
	ID          string `json:"id"`
	PeriodStart string `json:"period_start"` // YYYY-MM-DD
	PeriodEnd   string `json:"period_end"`
	CompletedAt string `json:"completed_at"` // RFC3339
}

type portalRecentWorkItemDTO struct {
	Type       string `json:"type"`        // "update" | "backup"
	SiteID     string `json:"site_id"`
	SiteName   string `json:"site_name"`
	Label      string `json:"label"`
	OccurredAt string `json:"occurred_at"` // RFC3339
}

type portalSummaryDTO struct {
	GeneratedAt         string                        `json:"generated_at"`
	PeriodStart         string                        `json:"period_start"`
	PeriodEnd           string                        `json:"period_end"`
	PeriodLabel         string                        `json:"period_label"`
	Totals              portalSummaryTotalsDTO        `json:"totals"`
	VitalsOverall       *string                       `json:"vitals_overall"`        // null when no samples
	VitalsDistribution  *portalVitalsDistributionDTO  `json:"vitals_distribution"`
	UptimeDaily         []portalUptimeDayDTO          `json:"uptime_daily"`
	Sites               []portalSummarySiteDTO        `json:"sites"`
	LatestReport        *portalSummaryLatestReportDTO `json:"latest_report"`
	RecentWork          []portalRecentWorkItemDTO     `json:"recent_work"`
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

// summary builds and returns the full portal dashboard payload.
// It calls report.BuildReportData for each client (with the email source
// disabled), then filters the site list to p.AllowedSiteIDs, recomputes
// totals, fetches the recent-work feed, and fetches the latest report row.
func (h *Handler) summary(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok || len(p.ClientIDs) == 0 {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}

	// Parse ?range — accept 7d/30d/90d, clamp everything else to 30d.
	rangeParam := c.DefaultQuery("range", "30d")
	var rangeDays int
	switch rangeParam {
	case "7d":
		rangeDays = 7
	case "90d":
		rangeDays = 90
	default:
		rangeDays = 30
		rangeParam = "30d"
	}

	now := time.Now().UTC()
	periodEnd := now
	periodStart := now.AddDate(0, 0, -rangeDays)

	// Build an allowed-site lookup set for O(1) filtering.
	allowedSet := make(map[uuid.UUID]struct{}, len(p.AllowedSiteIDs))
	for _, id := range p.AllowedSiteIDs {
		allowedSet[id] = struct{}{}
	}

	// -----------------------------------------------------------------------
	// Load sites list (name/URL/status/last_backup_at) for label and status
	// joins. This is the same RLS-scoped list listSites uses.
	// -----------------------------------------------------------------------
	allSites, err := listAllSites(c.Request.Context(), h.sites, p.TenantID, p)
	if err != nil {
		httpx.Error(c, domain.Internal("portal_summary_sites_failed", "failed to list sites").WithCause(err))
		return
	}
	// Build fast lookup maps from site list.
	siteByID := make(map[uuid.UUID]site.Site, len(allSites))
	for _, s := range allSites {
		siteByID[s.ID] = s
	}

	// -----------------------------------------------------------------------
	// Call BuildReportData for each client in p.ClientIDs (email source nil).
	// Collect all per-site SiteReport structs, then filter to AllowedSiteIDs.
	// -----------------------------------------------------------------------
	generatedAt := time.Now().UTC()

	// Aggregate all site reports from all clients, then filter.
	type clientBrandRow struct {
		id    uuid.UUID
		name  string
		color string
	}

	var allSiteReports []report.SiteReport
	var agencyName string

	if h.reportSources != nil {
		// Load agency name best-effort.
		_ = h.pool.RunTenantTx(c.Request.Context(), p, func(tx pgx.Tx) error {
			return tx.QueryRow(c.Request.Context(), "SELECT name FROM tenants WHERE id = $1", p.TenantID).Scan(&agencyName)
		})

		for _, clientID := range p.ClientIDs {
			rd, rdErr := report.BuildReportData(c.Request.Context(), *h.reportSources, report.BuildInput{
				TenantID:    p.TenantID,
				ClientID:    clientID,
				Client:      report.ClientInfo{},
				AgencyName:  agencyName,
				Schedule:    nil,
				PeriodStart: periodStart,
				PeriodEnd:   periodEnd,
			})
			if rdErr != nil {
				// degrade-to-empty for this client, continue
				continue
			}
			allSiteReports = append(allSiteReports, rd.Sites...)
		}
	}

	// Filter site reports to AllowedSiteIDs (defense-in-depth).
	filtered := filterSiteReports(allSiteReports, allowedSet)

	// -----------------------------------------------------------------------
	// Recompute totals over the filtered set.
	// -----------------------------------------------------------------------
	var (
		uptimeSiteCount  int
		uptimePctSum     float64
		totalIncidents   int
		totalBackups     int64
		totalUpdatesAppl int64
		totalUpdatesFail int64
	)

	siteDTOs := make([]portalSummarySiteDTO, 0, len(filtered))
	for _, sr := range filtered {
		s := siteByID[sr.SiteID]

		dto := portalSummarySiteDTO{
			ID:          sr.SiteID.String(),
			Name:        sr.Name,
			URL:         sr.URL,
			Status:      string(s.ConnectionState),
			UptimeDaily: []portalUptimeDayDTO{},
		}

		// Status from the live site list (BuildReportData doesn't carry it).
		if dto.Status == "" {
			dto.Status = "disconnected"
		}

		// last_backup_at from the live site list (more up-to-date than report).
		if s.LastBackupAt != nil {
			v := s.LastBackupAt.UTC().Format(time.RFC3339)
			dto.LastBackupAt = &v
		}

		if sr.Uptime != nil {
			v := sr.Uptime.UptimePct
			dto.UptimePct = &v
			dto.Incidents = sr.Uptime.Incidents
			uptimeSiteCount++
			uptimePctSum += sr.Uptime.UptimePct
			totalIncidents += sr.Uptime.Incidents

			for _, ud := range sr.Uptime.Daily {
				dto.UptimeDaily = append(dto.UptimeDaily, portalUptimeDayDTO{
					Day:       ud.Day.Format("2006-01-02"),
					UptimePct: ud.UptimePct,
				})
			}

			if sr.Uptime.TLSExpiry != nil {
				v2 := sr.Uptime.TLSExpiry.UTC().Format(time.RFC3339)
				dto.TLSExpiresAt = &v2
			}
		}

		if sr.Backups != nil {
			dto.BackupsInPeriod = sr.Backups.CompletedInPeriod
			totalBackups += sr.Backups.CompletedInPeriod
		}

		if sr.Updates != nil {
			dto.UpdatesInPeriod = sr.Updates.Total
			totalUpdatesAppl += sr.Updates.Total
			totalUpdatesFail += sr.Updates.Failed
		}

		if sr.Performance != nil {
			worstRating := worstCWVRating(sr.Performance.Metrics)
			if worstRating != "" {
				dto.VitalsRating = &worstRating
			}
		}

		siteDTOs = append(siteDTOs, dto)
	}

	// Compute avg uptime.
	var avgUptimePct *float64
	if uptimeSiteCount > 0 {
		v := uptimePctSum / float64(uptimeSiteCount)
		avgUptimePct = &v
	}

	totals := portalSummaryTotalsDTO{
		SiteCount:      len(filtered),
		AvgUptimePct:   avgUptimePct,
		Incidents:      totalIncidents,
		BackupsCount:   totalBackups,
		UpdatesApplied: totalUpdatesAppl,
		UpdatesFailed:  totalUpdatesFail,
	}

	// -----------------------------------------------------------------------
	// Fleet uptime daily: average per day across all sites with data.
	// -----------------------------------------------------------------------
	fleetUptimeDaily := buildFleetUptimeDaily(filtered)

	// -----------------------------------------------------------------------
	// Vitals distribution: per-metric rating counts across filtered sites.
	// -----------------------------------------------------------------------
	vitalsOverall, vitalsDistribution := buildVitalsDistribution(filtered)

	// -----------------------------------------------------------------------
	// Latest report: first completed row across all client IDs.
	// -----------------------------------------------------------------------
	var latestReport *portalSummaryLatestReportDTO
	var reportRows []sqlc.ListCompletedReportsForClientsRow
	_ = h.pool.RunTenantTx(c.Request.Context(), p, func(tx pgx.Tx) error {
		var qerr error
		reportRows, qerr = sqlc.New(tx).ListCompletedReportsForClients(c.Request.Context(), sqlc.ListCompletedReportsForClientsParams{
			TenantID:        p.TenantID,
			ClientIds:       p.ClientIDs,
			CursorCreatedAt: pgtype.Timestamptz{Valid: false},
			CursorID:        uuid.Nil,
			RowLimit:        1,
		})
		return qerr
	})
	if len(reportRows) > 0 {
		r := reportRows[0]
		lr := &portalSummaryLatestReportDTO{
			ID:          r.ID.String(),
			PeriodStart: r.PeriodStart.UTC().Format("2006-01-02"),
			PeriodEnd:   r.PeriodEnd.UTC().Format("2006-01-02"),
		}
		if r.CompletedAt.Valid {
			lr.CompletedAt = r.CompletedAt.Time.UTC().Format(time.RFC3339)
		}
		latestReport = lr
	}

	// -----------------------------------------------------------------------
	// Recent work feed: merge updates + backups, sort desc, cap 20.
	// Run under RunTenantTx so app.site_scope RLS double-gates both tables.
	// -----------------------------------------------------------------------
	since := periodStart
	recentWork := h.buildRecentWork(c.Request.Context(), p, since, siteByID)

	// -----------------------------------------------------------------------
	// Assemble and return.
	// -----------------------------------------------------------------------
	dto := portalSummaryDTO{
		GeneratedAt:        generatedAt.Format(time.RFC3339),
		PeriodStart:        periodStart.Format(time.RFC3339),
		PeriodEnd:          periodEnd.Format(time.RFC3339),
		PeriodLabel:        formatPeriodLabel(periodStart, periodEnd),
		Totals:             totals,
		VitalsOverall:      vitalsOverall,
		VitalsDistribution: vitalsDistribution,
		UptimeDaily:        fleetUptimeDaily,
		Sites:              siteDTOs,
		LatestReport:       latestReport,
		RecentWork:         recentWork,
	}
	c.JSON(http.StatusOK, dto)
}

// buildRecentWork fetches the recent-work feed: successful updates + completed
// backups since the period start, merged and capped at 20, descending by time.
// Runs under RunTenantTx so the m19 site_scope RLS policies double-gate both
// update_tasks and backup_snapshots. site_ids is always AllowedSiteIDs only.
func (h *Handler) buildRecentWork(ctx context.Context, p domain.Principal, since time.Time, siteByID map[uuid.UUID]site.Site) []portalRecentWorkItemDTO {
	if len(p.AllowedSiteIDs) == 0 {
		return []portalRecentWorkItemDTO{}
	}

	// Fetch more than 20 so after merge we can sort and trim.
	const fetchLimit = 40

	type workItem struct {
		itemType   string
		siteID     uuid.UUID
		label      string
		occurredAt time.Time
	}

	var items []workItem

	_ = h.pool.RunTenantTx(ctx, p, func(tx pgx.Tx) error {
		// Updates.
		taskRows, terr := sqlc.New(tx).ListAppliedTasksForSites(ctx, sqlc.ListAppliedTasksForSitesParams{
			TenantID: p.TenantID,
			SiteIds:  p.AllowedSiteIDs,
			Since:    pgtype.Timestamptz{Time: since, Valid: true},
			RowLimit: fetchLimit,
		})
		if terr == nil {
			for _, row := range taskRows {
				if !row.FinishedAt.Valid {
					continue
				}
				label := row.TargetSlug
				if row.FromVersion != "" && row.ToVersion != "" {
					label = fmt.Sprintf("%s %s -> %s", row.TargetSlug, row.FromVersion, row.ToVersion)
				}
				items = append(items, workItem{
					itemType:   "update",
					siteID:     row.SiteID,
					label:      label,
					occurredAt: row.FinishedAt.Time,
				})
			}
		}

		// Backups.
		snapRows, serr := sqlc.New(tx).ListRecentCompletedSnapshotsForSites(ctx, sqlc.ListRecentCompletedSnapshotsForSitesParams{
			TenantID: p.TenantID,
			SiteIds:  p.AllowedSiteIDs,
			Since:    pgtype.Timestamptz{Time: since, Valid: true},
			RowLimit: fetchLimit,
		})
		if serr == nil {
			for _, row := range snapRows {
				if !row.FinishedAt.Valid {
					continue
				}
				label := fmt.Sprintf("%s backup (%s)", row.Kind, humanSize(row.TotalSize))
				items = append(items, workItem{
					itemType:   "backup",
					siteID:     row.SiteID,
					label:      label,
					occurredAt: row.FinishedAt.Time,
				})
			}
		}
		return nil
	})

	// Sort descending, cap 20.
	sort.Slice(items, func(i, j int) bool {
		return items[i].occurredAt.After(items[j].occurredAt)
	})
	if len(items) > 20 {
		items = items[:20]
	}

	result := make([]portalRecentWorkItemDTO, 0, len(items))
	for _, it := range items {
		s := siteByID[it.siteID]
		result = append(result, portalRecentWorkItemDTO{
			Type:       it.itemType,
			SiteID:     it.siteID.String(),
			SiteName:   s.Name,
			Label:      it.label,
			OccurredAt: it.occurredAt.UTC().Format(time.RFC3339),
		})
	}
	return result
}

func (h *Handler) overview(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok || len(p.ClientIDs) == 0 {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}

	var brands []sqlc.GetClientBrandsByIDsRow
	var tenantName string
	var reportCount int

	err := h.pool.RunTenantTx(c.Request.Context(), p, func(tx pgx.Tx) error {
		var qerr error
		brands, qerr = sqlc.New(tx).GetClientBrandsByIDs(c.Request.Context(), sqlc.GetClientBrandsByIDsParams{
			Ids:      p.ClientIDs,
			TenantID: p.TenantID,
		})
		if qerr != nil {
			return qerr
		}
		_ = tx.QueryRow(c.Request.Context(),
			"SELECT name FROM tenants WHERE id = $1", p.TenantID,
		).Scan(&tenantName)
		// Report count (cross-client gate: client_id = ANY(p.ClientIDs)).
		_ = tx.QueryRow(c.Request.Context(),
			`SELECT count(*) FROM generated_reports WHERE tenant_id = $1 AND client_id = ANY($2::uuid[]) AND status = 'completed'`,
			p.TenantID, p.ClientIDs,
		).Scan(&reportCount)
		return nil
	})
	if err != nil {
		httpx.Error(c, domain.Internal("portal_overview_failed", "failed to load portal overview").WithCause(err))
		return
	}

	// Use the earliest-created client for primary branding (ordered ASC in query).
	var primary sqlc.GetClientBrandsByIDsRow
	if len(brands) > 0 {
		primary = brands[0]
	}

	// Site count from p.AllowedSiteIDs (already scoped to the client's sites).
	dto := overviewDTO{
		Client: overviewClientDTO{
			ID:      primary.ID.String(),
			Name:    primary.Name,
			LogoURL: primary.LogoUrl,
			Color:   primary.Color,
		},
		AgencyName:  tenantName,
		SiteCount:   len(p.AllowedSiteIDs),
		ReportCount: reportCount,
	}
	c.JSON(http.StatusOK, dto)
}

func (h *Handler) listSites(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}

	// Pass the principal so repo.List uses RunTenantTx and the InScopedTenantTx
	// path restricts rows via app.allowed_site_ids (the RESTRICTIVE site_scope
	// policy). Without Principal the repo falls back to InTenantTx, which would
	// return ALL tenant sites instead of just the client's sites.
	sites, err := h.sites.List(c.Request.Context(), site.ListInput{
		TenantID:  p.TenantID,
		Limit:     200,
		Offset:    0,
		Principal: p,
	})
	if err != nil {
		httpx.Error(c, domain.Internal("portal_sites_failed", "failed to list sites").WithCause(err))
		return
	}

	items := make([]portalSiteDTO, 0, len(sites))
	for _, s := range sites {
		d := portalSiteDTO{
			ID:     s.ID.String(),
			Name:   s.Name,
			URL:    s.URL,
			Status: string(s.ConnectionState),
		}
		if s.LastBackupAt != nil {
			v := s.LastBackupAt.UTC().Format(time.RFC3339)
			d.LastBackupAt = &v
		}
		// Fill uptime_30d_pct and tls_expires_at from the metrics store.
		// The metrics store runs InAgentTx (RLS-bypassing) but IDs come only
		// from the already-RLS-scoped site list, satisfying the security contract.
		if h.metricsStore != nil {
			agg, aerr := h.metricsStore.QueryAggregate(c.Request.Context(), p.TenantID, s.ID, 30*24*time.Hour)
			if aerr == nil && agg.Checks > 0 {
				v := agg.UptimePct
				d.Uptime30dPct = &v
			}
			latest, lerr := h.metricsStore.QueryLatest(c.Request.Context(), p.TenantID, s.ID)
			if lerr == nil && latest.Found && !latest.TLSExpiry.IsZero() {
				v := latest.TLSExpiry.UTC().Format(time.RFC3339)
				d.TLSExpiresAt = &v
			}
		}
		items = append(items, d)
	}
	c.JSON(http.StatusOK, portalSiteListDTO{Items: items})
}

func (h *Handler) siteUptime(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}
	siteID, err := uuid.Parse(c.Param("siteId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_site_id", "siteId is not a valid UUID"))
		return
	}

	rangeParam := c.DefaultQuery("range", "30d")
	window := parseUptimeWindow(rangeParam)

	if h.uptimeSvc == nil {
		c.JSON(http.StatusOK, portalUptimeDTO{Range: rangeParam, Incidents: []incidentDTO{}})
		return
	}

	rpt, err := h.uptimeSvc.Uptime(c.Request.Context(), p.TenantID, siteID, window, 0)
	if err != nil {
		if e, ok2 := domain.AsDomain(err); ok2 && e.Kind == domain.KindNotFound {
			httpx.Error(c, domain.NotFound("site_not_found", "site not found"))
			return
		}
		httpx.Error(c, domain.Internal("portal_uptime_failed", "failed to load uptime").WithCause(err))
		return
	}

	dto := portalUptimeDTO{
		Range:     rangeParam,
		UptimePct: rpt.UptimePct,
		AvgLatencyMs: rpt.AvgLatencyMs,
		Incidents: []incidentDTO{},
	}
	if rpt.TLSExpiry != nil {
		v := rpt.TLSExpiry.UTC().Format(time.RFC3339)
		dto.TLSExpiresAt = &v
	}
	c.JSON(http.StatusOK, dto)
}

func (h *Handler) siteBackups(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}
	siteID, err := uuid.Parse(c.Param("siteId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_site_id", "siteId is not a valid UUID"))
		return
	}

	limitStr := c.DefaultQuery("limit", "20")
	limit, _ := strconv.Atoi(limitStr)
	if limit <= 0 || limit > 20 {
		limit = 20
	}

	if h.backupRepo == nil {
		c.JSON(http.StatusOK, portalBackupListDTO{Items: []portalBackupItemDTO{}})
		return
	}

	snaps, err := h.backupRepo.ListSnapshotsForSite(c.Request.Context(), p.TenantID, siteID, int32(limit), 0)
	if err != nil {
		httpx.Error(c, domain.Internal("portal_backups_failed", "failed to list backups").WithCause(err))
		return
	}

	items := make([]portalBackupItemDTO, 0, len(snaps))
	for _, s := range snaps {
		// Status filter: completed only per spec. The repo returns all statuses;
		// filter here so the limit is consistent.
		if s.Status != backup.StatusCompleted {
			continue
		}
		d := portalBackupItemDTO{
			ID:        s.ID.String(),
			Kind:      s.Kind,
			Status:    s.Status,
			SizeBytes: s.TotalSize,
			CreatedAt: s.CreatedAt.UTC().Format(time.RFC3339),
		}
		if s.FinishedAt != nil {
			v := s.FinishedAt.UTC().Format(time.RFC3339)
			d.CompletedAt = &v
		}
		items = append(items, d)
	}
	c.JSON(http.StatusOK, portalBackupListDTO{Items: items})
}

func (h *Handler) siteUpdates(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}
	siteID, err := uuid.Parse(c.Param("siteId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_site_id", "siteId is not a valid UUID"))
		return
	}

	limitStr := c.DefaultQuery("limit", "50")
	limit, _ := strconv.Atoi(limitStr)
	if limit <= 0 || limit > 50 {
		limit = 50
	}

	var tasks []sqlc.ListAppliedTasksForSiteRow
	err = h.pool.RunTenantTx(c.Request.Context(), p, func(tx pgx.Tx) error {
		var qerr error
		tasks, qerr = sqlc.New(tx).ListAppliedTasksForSite(c.Request.Context(), sqlc.ListAppliedTasksForSiteParams{
			SiteID:   siteID,
			TenantID: p.TenantID,
			RowLimit: int32(limit),
		})
		return qerr
	})
	if err != nil {
		httpx.Error(c, domain.Internal("portal_updates_failed", "failed to list updates").WithCause(err))
		return
	}

	items := make([]portalUpdateItemDTO, 0, len(tasks))
	for _, t := range tasks {
		d := portalUpdateItemDTO{
			Type:        t.TargetType,
			Name:        t.TargetSlug,
			FromVersion: t.FromVersion,
			ToVersion:   t.ToVersion,
			Status:      t.Status,
		}
		if t.FinishedAt.Valid {
			v := t.FinishedAt.Time.UTC().Format(time.RFC3339)
			d.FinishedAt = &v
		}
		items = append(items, d)
	}
	c.JSON(http.StatusOK, portalUpdateListDTO{Items: items})
}

func (h *Handler) siteVitals(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}
	siteID, err := uuid.Parse(c.Param("siteId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_site_id", "siteId is not a valid UUID"))
		return
	}

	rangeParam := c.DefaultQuery("range", "28d")

	if h.rumStore == nil {
		c.JSON(http.StatusOK, portalVitalsDTO{Range: rangeParam, Metrics: []portalVitalMetricDTO{}})
		return
	}

	sinceDay := time.Now().UTC().AddDate(0, 0, -28)
	rollups, err := h.rumStore.GetDailyRollups(c.Request.Context(), siteID, p.TenantID, sinceDay)
	if err != nil {
		httpx.Error(c, domain.Internal("portal_vitals_failed", "failed to load vitals").WithCause(err))
		return
	}

	// Convert daily rollups to HourlyRollup shape for ComputeP75 (compatible
	// subset; bucket_counts and sample_count are the only fields used).
	hourlyShape := make([]rum.HourlyRollup, 0, len(rollups))
	for _, dr := range rollups {
		hourlyShape = append(hourlyShape, rum.HourlyRollup{
			RollupKey:    dr.RollupKey,
			SampleCount:  dr.SampleCount,
			BucketCounts: dr.BucketCounts,
		})
	}

	// Filter to device="" (all-devices aggregate, the 0.33.5 convention) for
	// the portal. A blank device row is the cross-device aggregate.
	allDevices := make([]rum.HourlyRollup, 0, len(hourlyShape))
	for _, r := range hourlyShape {
		if r.Device == "" {
			allDevices = append(allDevices, r)
		}
	}

	results := h.rumStore.ComputeP75(allDevices, 50)
	metrics := make([]portalVitalMetricDTO, 0, len(results))
	for _, r := range results {
		if r.Metric != "lcp" && r.Metric != "inp" && r.Metric != "cls" {
			continue
		}
		if r.P75Milli == 0 {
			continue // suppressed (below min sample count)
		}
		metrics = append(metrics, portalVitalMetricDTO{
			Metric:  r.Metric,
			P75:     r.P75Milli,
			Rating:  vitalRating(r.Metric, r.P75Milli),
			Samples: r.SampleCount,
		})
	}
	c.JSON(http.StatusOK, portalVitalsDTO{Range: rangeParam, Metrics: metrics})
}

func (h *Handler) listReports(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok || len(p.ClientIDs) == 0 {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}

	var rows []sqlc.ListCompletedReportsForClientsRow
	err := h.pool.RunTenantTx(c.Request.Context(), p, func(tx pgx.Tx) error {
		var qerr error
		rows, qerr = sqlc.New(tx).ListCompletedReportsForClients(c.Request.Context(), sqlc.ListCompletedReportsForClientsParams{
			TenantID:        p.TenantID,
			ClientIds:       p.ClientIDs,
			CursorCreatedAt: pgtype.Timestamptz{Valid: false},
			CursorID:        uuid.Nil,
			RowLimit:        50,
		})
		return qerr
	})
	if err != nil {
		httpx.Error(c, domain.Internal("portal_reports_failed", "failed to list reports").WithCause(err))
		return
	}

	items := make([]portalReportItemDTO, 0, len(rows))
	for _, r := range rows {
		d := portalReportItemDTO{
			ID:          r.ID.String(),
			ClientID:    r.ClientID.String(),
			PeriodStart: r.PeriodStart.UTC().Format("2006-01-02"),
			PeriodEnd:   r.PeriodEnd.UTC().Format("2006-01-02"),
			CreatedAt:   r.CreatedAt.UTC().Format(time.RFC3339),
		}
		if r.CompletedAt.Valid {
			v := r.CompletedAt.Time.UTC().Format(time.RFC3339)
			d.CompletedAt = &v
		}
		items = append(items, d)
	}
	c.JSON(http.StatusOK, portalReportListDTO{Items: items})
}

func (h *Handler) downloadReport(c *gin.Context) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok || len(p.ClientIDs) == 0 {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}
	reportID, err := uuid.Parse(c.Param("reportId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_report_id", "reportId is not a valid UUID"))
		return
	}

	formatParam := c.DefaultQuery("format", "html")

	var row sqlc.GetCompletedReportForPortalRow
	err = h.pool.RunTenantTx(c.Request.Context(), p, func(tx pgx.Tx) error {
		var qerr error
		row, qerr = sqlc.New(tx).GetCompletedReportForPortal(c.Request.Context(), sqlc.GetCompletedReportForPortalParams{
			ID:        reportID,
			TenantID:  p.TenantID,
			ClientIds: p.ClientIDs,
		})
		return qerr
	})
	if err != nil {
		if err == pgx.ErrNoRows {
			// Opaque 404: does not reveal whether report exists for another client.
			httpx.Error(c, domain.NotFound("report_not_found", "report not found"))
			return
		}
		httpx.Error(c, domain.Internal("portal_report_download_failed", "failed to load report").WithCause(err))
		return
	}

	if h.reportSvc == nil {
		httpx.Error(c, domain.Unavailable("report_download_unavailable", "report download is not configured"))
		return
	}

	// Construct a domain GeneratedReport from the portal query row to reuse
	// the existing PresignReportURLs machinery.
	rpt := report.GeneratedReport{
		ID:          row.ID,
		TenantID:    row.TenantID,
		ClientID:    row.ClientID,
		PeriodStart: row.PeriodStart,
		PeriodEnd:   row.PeriodEnd,
		Status:      row.Status,
		HTMLBlobKey: row.HtmlBlobKey,
		PDFBlobKey:  row.PdfBlobKey,
	}
	if row.CompletedAt.Valid {
		t := row.CompletedAt.Time
		rpt.CompletedAt = &t
	}

	htmlURL, pdfURL := h.reportSvc.PresignReportURLs(c.Request.Context(), rpt)
	var url string
	switch formatParam {
	case "pdf":
		url = pdfURL
	default:
		url = htmlURL
	}
	if url == "" {
		httpx.Error(c, domain.Unavailable("report_download_unavailable", "report download URL could not be generated"))
		return
	}

	// Presigned URL expiry: mirrors report.presignTTL (15 minutes).
	expiresAt := time.Now().UTC().Add(15 * time.Minute)
	c.JSON(http.StatusOK, portalDownloadDTO{
		URL:       url,
		ExpiresAt: expiresAt.Format(time.RFC3339),
	})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// parseUptimeWindow converts the range query param to a duration.
func parseUptimeWindow(r string) time.Duration {
	switch r {
	case "24h":
		return 24 * time.Hour
	case "7d":
		return 7 * 24 * time.Hour
	case "90d":
		return 90 * 24 * time.Hour
	default: // "30d"
		return 30 * 24 * time.Hour
	}
}

// vitalRating returns the CrUX-grade for a metric value (good/needs-improvement/poor).
func vitalRating(metric string, p75 float64) string {
	switch metric {
	case "lcp":
		if p75 < 2500 {
			return "good"
		} else if p75 < 4000 {
			return "needs-improvement"
		}
		return "poor"
	case "inp":
		if p75 < 200 {
			return "good"
		} else if p75 < 500 {
			return "needs-improvement"
		}
		return "poor"
	case "cls":
		// CLS values are in milli-units (thousandths of a unit).
		if p75 < 100 {
			return "good"
		} else if p75 < 250 {
			return "needs-improvement"
		}
		return "poor"
	}
	return "unknown"
}

// filterSiteReports keeps only site reports whose SiteID is in allowed. This
// is the defense-in-depth gate between the report aggregator (which lists ALL
// sites of the client under InTenantTx, not the principal's RLS scope) and the
// summary response. Removing this filter would let a stale or diverged
// AllowedSiteIDs set leak other sites' data; portal_summary_test.go pins it.
func filterSiteReports(reports []report.SiteReport, allowed map[uuid.UUID]struct{}) []report.SiteReport {
	filtered := make([]report.SiteReport, 0, len(reports))
	for _, sr := range reports {
		if _, ok := allowed[sr.SiteID]; ok {
			filtered = append(filtered, sr)
		}
	}
	return filtered
}

// worstCWVRating returns the worst (most degraded) CWV rating across the given
// metrics. Returns "" when metrics is empty or all are empty. Rating order:
// poor > needs-improvement > good.
func worstCWVRating(metrics []report.PerfMetric) string {
	worst := ""
	for _, m := range metrics {
		r := m.Rating
		if r == "poor" {
			return "poor"
		}
		if r == "needs_improvement" || r == "needs-improvement" {
			worst = "needs-improvement"
		} else if r == "good" && worst == "" {
			worst = "good"
		}
	}
	return worst
}

// buildFleetUptimeDaily computes a fleet-average uptime-day series by averaging
// per-site daily series (only sites that have uptime data). Days with no data
// are excluded.
func buildFleetUptimeDaily(sites []report.SiteReport) []portalUptimeDayDTO {
	type dayAcc struct {
		sum float64
		n   int
	}
	byDay := make(map[string]*dayAcc)
	var order []string
	seen := make(map[string]bool)

	for _, sr := range sites {
		if sr.Uptime == nil {
			continue
		}
		for _, ud := range sr.Uptime.Daily {
			k := ud.Day.Format("2006-01-02")
			if !seen[k] {
				seen[k] = true
				order = append(order, k)
				byDay[k] = &dayAcc{}
			}
			acc := byDay[k]
			acc.sum += ud.UptimePct
			acc.n++
		}
	}

	sort.Strings(order)
	result := make([]portalUptimeDayDTO, 0, len(order))
	for _, k := range order {
		acc := byDay[k]
		avg := 0.0
		if acc.n > 0 {
			avg = acc.sum / float64(acc.n)
		}
		result = append(result, portalUptimeDayDTO{
			Day:       k,
			UptimePct: math.Round(avg*100) / 100,
		})
	}
	return result
}

// buildVitalsDistribution counts site-level CWV ratings across all filtered
// sites and returns the worst-overall rating and a per-metric distribution.
// Sites with no performance data are excluded from the counts.
func buildVitalsDistribution(sites []report.SiteReport) (*string, *portalVitalsDistributionDTO) {
	dist := &portalVitalsDistributionDTO{}
	hasSamples := false

	for _, sr := range sites {
		if sr.Performance == nil || len(sr.Performance.Metrics) == 0 {
			continue
		}
		for _, m := range sr.Performance.Metrics {
			// Normalize to the portal vocabulary (needs-improvement not needs_improvement).
			r := m.Rating
			if r == "needs_improvement" {
				r = "needs-improvement"
			}
			switch m.Metric {
			case "lcp":
				hasSamples = true
				switch r {
				case "good":
					dist.LCP.Good++
				case "needs-improvement":
					dist.LCP.NeedsImprovement++
				case "poor":
					dist.LCP.Poor++
				}
			case "inp":
				hasSamples = true
				switch r {
				case "good":
					dist.INP.Good++
				case "needs-improvement":
					dist.INP.NeedsImprovement++
				case "poor":
					dist.INP.Poor++
				}
			case "cls":
				hasSamples = true
				switch r {
				case "good":
					dist.CLS.Good++
				case "needs-improvement":
					dist.CLS.NeedsImprovement++
				case "poor":
					dist.CLS.Poor++
				}
			}
		}
	}

	if !hasSamples {
		return nil, nil
	}

	// Overall = worst across all sites.
	overall := "good"
	for _, sr := range sites {
		if sr.Performance == nil {
			continue
		}
		w := worstCWVRating(sr.Performance.Metrics)
		if w == "poor" {
			overall = "poor"
			break
		}
		if w == "needs-improvement" {
			overall = "needs-improvement"
		}
	}
	return &overall, dist
}

// formatPeriodLabel formats the period as "12 May 2026 – 10 Jun 2026".
func formatPeriodLabel(from, to time.Time) string {
	return fmt.Sprintf("%d %s %d – %d %s %d",
		from.Day(), from.Format("Jan"), from.Year(),
		to.Day(), to.Format("Jan"), to.Year())
}

// humanSize formats a byte count as a human-readable string (e.g. "12.3 MB").
func humanSize(bytes int64) string {
	if bytes == 0 {
		return "0 B"
	}
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}
