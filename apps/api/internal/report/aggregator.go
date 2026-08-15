package report

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/email"
	"github.com/mosamlife/wpmgr/apps/api/internal/metrics"
	"github.com/mosamlife/wpmgr/apps/api/internal/rum"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
	"github.com/mosamlife/wpmgr/apps/api/internal/update"
)

// RumMinSampleCount is the minimum sample count to emit a CWV metric.
// Uses the m57 default (rum_results_handler.go:72-75). Reports use the fixed
// default, NOT per-site config — keep the aggregator config-free.
const RumMinSampleCount = 30

// BuildInput is the input to BuildReportData.
type BuildInput struct {
	TenantID    uuid.UUID
	ClientID    uuid.UUID
	Client      ClientInfo
	AgencyName  string
	Schedule    *Schedule // nil for on-demand
	PeriodStart time.Time
	PeriodEnd   time.Time
}

// ClientInfo is the minimal client data needed for the report header.
type ClientInfo struct {
	Name    string
	Company string
	LogoURL string
	Color   string
}

// Sources is the set of data-access functions injected into the aggregator.
// Each field is a func so individual sources are substitutable in tests and
// so the aggregator stays testable in isolation (mirrors perf.RumResultsReader).
type Sources struct {
	// Site listing.
	ListClientSites func(ctx context.Context, tenantID uuid.UUID, clientID uuid.UUID) ([]site.Site, error)
	// Uptime data.
	QueryUptimeAggregateRange func(ctx context.Context, tenantID, siteID uuid.UUID, from, to time.Time) (metrics.Aggregate, error)
	QueryUptimeSeriesRange    func(ctx context.Context, tenantID, siteID uuid.UUID, from, to time.Time) ([]metrics.Point, error)
	QueryUptimeLatest         func(ctx context.Context, tenantID, siteID uuid.UUID) (metrics.Latest, error)
	// Backup data.
	GetBackupReportStats func(ctx context.Context, tenantID, siteID uuid.UUID, from, to time.Time) (sqlc.GetBackupReportStatsRow, error)
	GetLatestCompletedAt func(ctx context.Context, tenantID, siteID uuid.UUID) (*time.Time, error)
	// Update data.
	GetUpdateReportStats func(ctx context.Context, tenantID, siteID uuid.UUID, from, to time.Time) ([]sqlc.GetUpdateReportStatsRow, error)
	// RUM / performance data.
	GetDailyRollups func(ctx context.Context, siteID, tenantID uuid.UUID, sinceDay time.Time) ([]rum.DailyRollup, error)
	// Email data.
	GetFleetStatsBySite func(ctx context.Context, tenantID uuid.UUID, from, to time.Time, limit int32) ([]email.SiteStatsRow, error)
}

// BuildReportData aggregates all section data for the given client and period.
// A failing section source degrades that section to nil + slog.Warn — it does
// NOT fail the whole report. Only sections enabled by the schedule flags are
// populated.
func BuildReportData(ctx context.Context, sources Sources, in BuildInput) (ReportData, error) {
	sections := DefaultSectionFlags()
	if in.Schedule != nil {
		sections = in.Schedule.Sections
	}
	var introText, closingText string
	var poweredByRemoved bool
	if in.Schedule != nil {
		introText = in.Schedule.IntroText
		closingText = in.Schedule.ClosingText
		poweredByRemoved = in.Schedule.PoweredByRemoved
	}

	rd := ReportData{
		SchemaVersion: 1,
		GeneratedAt:   time.Now().UTC(),
		PeriodStart:   in.PeriodStart,
		PeriodEnd:     in.PeriodEnd,
		PeriodLabel:   formatPeriodLabel(in.PeriodStart, in.PeriodEnd),
		ClientID:      in.ClientID,
		ClientName:    in.Client.Name,
		Company:       in.Client.Company,
		AgencyName:    in.AgencyName,
		LogoURL:       in.Client.LogoURL,
		AccentColor:   in.Client.Color,
		IntroText:     introText,
		ClosingText:   closingText,
		ShowPoweredBy: !poweredByRemoved,
		Sections:      sections,
	}

	// Enumerate sites assigned to this client.
	sites, err := sources.ListClientSites(ctx, in.TenantID, in.ClientID)
	if err != nil {
		slog.Warn("report aggregator: list client sites failed",
			slog.String("client_id", in.ClientID.String()), slog.Any("error", err))
		rd.Sites = []SiteReport{}
		return rd, nil
	}

	// Pre-fetch email stats for all sites in one query.
	var emailStatsBySite map[uuid.UUID]email.SiteStatsRow
	if sections.Email && sources.GetFleetStatsBySite != nil {
		stats, sErr := sources.GetFleetStatsBySite(ctx, in.TenantID, in.PeriodStart, in.PeriodEnd, 500)
		if sErr != nil {
			slog.Warn("report aggregator: get fleet email stats failed", slog.Any("error", sErr))
		} else {
			emailStatsBySite = make(map[uuid.UUID]email.SiteStatsRow, len(stats))
			for _, row := range stats {
				emailStatsBySite[row.SiteID] = row
			}
		}
	}

	siteReports := make([]SiteReport, 0, len(sites))
	var totals ReportTotals
	totals.SiteCount = len(sites)

	for _, s := range sites {
		sr := SiteReport{
			SiteID: s.ID,
			Name:   s.Name,
			URL:    s.URL,
		}

		// GH #414 phase 5. A monitoring-paused site stays IN the report and is
		// NAMED as paused; what it loses is the uptime section, because the
		// prober stopped for it in phase 2 and its daily series is therefore
		// empty for every paused day. See SiteReport.MonitoringPaused for the
		// full argument, including why backups/updates/performance/email are
		// deliberately left alone.
		//
		// The flag is read from the site row the report already enumerated, not
		// from a second query: site.Service.List carries monitoring_paused_at
		// since phase 4a, inside the same tenant transaction and therefore under
		// the same RLS policies.
		if s.MonitoringPausedAt != nil {
			sr.MonitoringPaused = true
			sr.MonitoringPausedAt = s.MonitoringPausedAt
			sr.MonitoringPausedReason = s.MonitoringPausedReason
			totals.PausedSiteCount++
		}

		// Uptime section. Skipped entirely for a paused site — and skipped
		// BEFORE the source is called, so a paused site costs no query either.
		if sections.Uptime && sources.QueryUptimeAggregateRange != nil && !sr.MonitoringPaused {
			us := buildUptimeSection(ctx, sources, in.TenantID, s.ID, in.PeriodStart, in.PeriodEnd)
			if us != nil {
				sr.Uptime = us
				totals.AvgUptimePct += us.UptimePct
				totals.Incidents += us.Incidents
				// The denominator is the population that was MEASURED, counted
				// here at the one place a site contributes to the numerator, so
				// the two can never be computed over different sets.
				totals.UptimeSiteCount++
			}
		}

		// Backup section.
		if sections.Backups && sources.GetBackupReportStats != nil {
			bs := buildBackupSection(ctx, sources, in.TenantID, s.ID, in.PeriodStart, in.PeriodEnd)
			if bs != nil {
				sr.Backups = bs
				totals.BackupsCount += bs.CompletedInPeriod
			}
		}

		// Updates section.
		if sections.Updates && sources.GetUpdateReportStats != nil {
			us2 := buildUpdateSection(ctx, sources, in.TenantID, s.ID, in.PeriodStart, in.PeriodEnd)
			if us2 != nil {
				sr.Updates = us2
				totals.UpdatesApplied += us2.Total
			}
		}

		// Performance section.
		if sections.Performance && sources.GetDailyRollups != nil {
			ps := buildPerfSection(ctx, sources, in.TenantID, s.ID, in.PeriodStart, in.PeriodEnd)
			sr.Performance = ps
		}

		// Email section (from pre-fetched batch).
		if sections.Email && emailStatsBySite != nil {
			if row, ok := emailStatsBySite[s.ID]; ok {
				sr.Email = &EmailSection{
					Total:   row.Total,
					Sent:    row.SentCount,
					Failed:  row.FailedCount,
					Bounced: row.BouncedCount,
				}
				totals.EmailsSent += row.SentCount
				totals.EmailsFailed += row.FailedCount
			}
		}

		siteReports = append(siteReports, sr)
	}

	// Compute average uptime over the MEASURED population, never over len(sites).
	//
	// GH #414 phase 5. Dividing by len(sites) let a monitoring-paused site — which
	// contributes no uptime section at all — enter the average as a 0%, so
	// pausing a site made the fleet number worse than the truth, and the same
	// arithmetic had always been silently counting never-probed sites as 0% too.
	// A site nobody measured is not a site measured at 0%. UptimeSiteCount is
	// incremented at the single point a site adds to the numerator above, so the
	// two halves of this division cannot drift apart.
	if totals.UptimeSiteCount > 0 {
		totals.AvgUptimePct = totals.AvgUptimePct / float64(totals.UptimeSiteCount)
	} else {
		// Nothing was measured: 0 is the zero value, and it means "no data",
		// which the renderers say in words rather than printing 0.0% as if it
		// were an observation.
		totals.AvgUptimePct = 0
	}
	rd.Sites = siteReports
	rd.Totals = totals
	return rd, nil
}

// ---------------------------------------------------------------------------
// Per-section builders
// ---------------------------------------------------------------------------

func buildUptimeSection(ctx context.Context, src Sources, tenantID, siteID uuid.UUID, from, to time.Time) *UptimeSection {
	agg, err := src.QueryUptimeAggregateRange(ctx, tenantID, siteID, from, to)
	if err != nil {
		slog.Warn("report aggregator: uptime aggregate failed",
			slog.String("site_id", siteID.String()), slog.Any("error", err))
		return nil
	}
	if agg.Checks == 0 {
		return nil
	}

	// Daily series for sparkline.
	var daily []UptimeDay
	if src.QueryUptimeSeriesRange != nil {
		points, serr := src.QueryUptimeSeriesRange(ctx, tenantID, siteID, from, to)
		if serr != nil {
			slog.Warn("report aggregator: uptime series failed",
				slog.String("site_id", siteID.String()), slog.Any("error", serr))
		} else {
			// Aggregate hourly points into daily buckets.
			daily = aggregateDailyBuckets(points)
		}
	}

	// Count incidents: maximal runs of buckets with DownChecks > 0.
	incidents := countIncidents(daily)

	var tlsExpiry *time.Time
	if src.QueryUptimeLatest != nil {
		latest, lerr := src.QueryUptimeLatest(ctx, tenantID, siteID)
		if lerr == nil && !latest.TLSExpiry.IsZero() {
			t := latest.TLSExpiry
			tlsExpiry = &t
		}
	}

	return &UptimeSection{
		UptimePct:    agg.UptimePct,
		AvgLatencyMs: agg.AvgLatencyMs,
		Checks:       agg.Checks,
		DownChecks:   agg.Checks - agg.UpChecks,
		Incidents:    incidents,
		TLSExpiry:    tlsExpiry,
		Daily:        daily,
	}
}

func buildBackupSection(ctx context.Context, src Sources, tenantID, siteID uuid.UUID, from, to time.Time) *BackupSection {
	stats, err := src.GetBackupReportStats(ctx, tenantID, siteID, from, to)
	if err != nil {
		slog.Warn("report aggregator: backup stats failed",
			slog.String("site_id", siteID.String()), slog.Any("error", err))
		return nil
	}
	bs := &BackupSection{
		CompletedInPeriod: stats.CompletedCount,
		TotalBytes:        stats.TotalBytes,
	}
	// Resolve last_completed_at.
	if t, ok := pgTimestamptzToTime(stats.LastCompletedAt); ok {
		bs.LastCompletedAt = &t
	} else if bs.CompletedInPeriod == 0 && src.GetLatestCompletedAt != nil {
		// Fall back to all-time latest when nothing completed in period.
		latest, ferr := src.GetLatestCompletedAt(ctx, tenantID, siteID)
		if ferr == nil {
			bs.LastCompletedAt = latest
		}
	}
	return bs
}

func buildUpdateSection(ctx context.Context, src Sources, tenantID, siteID uuid.UUID, from, to time.Time) *UpdateSection {
	rows, err := src.GetUpdateReportStats(ctx, tenantID, siteID, from, to)
	if err != nil {
		slog.Warn("report aggregator: update stats failed",
			slog.String("site_id", siteID.String()), slog.Any("error", err))
		return nil
	}
	us := &UpdateSection{}
	for _, row := range rows {
		switch row.TargetType {
		case update.TargetPlugin:
			us.Plugins += row.Succeeded
		case update.TargetTheme:
			us.Themes += row.Succeeded
		case update.TargetCore:
			us.Core += row.Succeeded
		}
		us.Failed += row.Failed
	}
	us.Total = us.Plugins + us.Themes + us.Core
	return us
}

func buildPerfSection(ctx context.Context, src Sources, tenantID, siteID uuid.UUID, from, to time.Time) *PerfSection {
	rollups, err := src.GetDailyRollups(ctx, siteID, tenantID, from)
	if err != nil {
		slog.Warn("report aggregator: RUM daily rollups failed",
			slog.String("site_id", siteID.String()), slog.Any("error", err))
		return nil
	}

	// Filter to period [from, to).
	filtered := make([]rum.DailyRollup, 0, len(rollups))
	for _, r := range rollups {
		if !r.BucketDay.Before(to) {
			continue
		}
		filtered = append(filtered, r)
	}
	if len(filtered) == 0 {
		return &PerfSection{Metrics: []PerfMetric{}}
	}

	// Aggregate all-devices: sum SampleCount + element-wise BucketCounts + max MaxValue.
	// Mirror rum_results_handler.go:84-164 all-devices aggregate.
	type agg struct {
		counts      []int64
		sampleCount int64
		maxVal      int32
	}
	byMetric := make(map[string]*agg)
	for _, r := range filtered {
		a, ok := byMetric[r.Metric]
		if !ok {
			a = &agg{counts: make([]int64, rum.NumBuckets)}
			byMetric[r.Metric] = a
		}
		a.sampleCount += r.SampleCount
		if r.MaxValue > a.maxVal {
			a.maxVal = r.MaxValue
		}
		if len(r.BucketCounts) == rum.NumBuckets {
			for i, c := range r.BucketCounts {
				a.counts[i] += int64(c)
			}
		}
	}

	metrics := make([]PerfMetric, 0, len(byMetric))
	for metric, a := range byMetric {
		if a.sampleCount < RumMinSampleCount {
			continue
		}
		p75 := rum.InterpolateP75FromCounts(a.counts, a.sampleCount, a.maxVal)
		metrics = append(metrics, PerfMetric{
			Metric:      metric,
			P75:         p75,
			Rating:      CWVRating(metric, p75),
			SampleCount: a.sampleCount,
		})
	}
	return &PerfSection{Metrics: metrics}
}

// ---------------------------------------------------------------------------
// Helper functions
// ---------------------------------------------------------------------------

// aggregateDailyBuckets collapses Points into daily UptimeDay buckets.
//
// The day is taken in UTC, deliberately and explicitly. Point.Bucket is a
// timestamptz the driver hands back in the process's local zone, and every
// bucket the metrics store produces is keyed to a UTC day boundary: from
// 0.61.125 a window of a day or more comes back as one point sitting exactly
// on UTC midnight (metrics.pgStore.querySeriesDaily, drawn from
// site_uptime_daily, whose day column is a UTC date). Reading .Day() off the
// local-zone value would put every one of those points in the PREVIOUS
// calendar day on any deployment running west of UTC, shifting a whole
// report's daily labels by one. Grouping in UTC keeps the report's days
// identical to the days the rollup itself is keyed by, on every deployment.
func aggregateDailyBuckets(points []metrics.Point) []UptimeDay {
	type dayKey struct{ y, m, d int }
	type dayAcc struct {
		checks, upChecks uint64
		totalLatency     float64
		n                int
	}
	days := make(map[dayKey]*dayAcc)
	var order []dayKey
	for _, p := range points {
		b := p.Bucket.UTC()
		k := dayKey{b.Year(), int(b.Month()), b.Day()}
		if _, ok := days[k]; !ok {
			days[k] = &dayAcc{}
			order = append(order, k)
		}
		acc := days[k]
		acc.checks += p.Checks
		acc.upChecks += p.UpChecks
		acc.totalLatency += p.AvgLatencyMs
		acc.n++
	}
	result := make([]UptimeDay, 0, len(order))
	for _, k := range order {
		acc := days[k]
		var upPct float64
		if acc.checks > 0 {
			upPct = float64(acc.upChecks) / float64(acc.checks) * 100
		}
		var avgLat float64
		if acc.n > 0 {
			avgLat = acc.totalLatency / float64(acc.n)
		}
		result = append(result, UptimeDay{
			Day:          time.Date(k.y, time.Month(k.m), k.d, 0, 0, 0, 0, time.UTC),
			UptimePct:    upPct,
			AvgLatencyMs: avgLat,
		})
	}
	return result
}

// countIncidents counts maximal consecutive runs of days with UptimePct < 100.
func countIncidents(daily []UptimeDay) int {
	count := 0
	inIncident := false
	for _, d := range daily {
		down := d.UptimePct < 100
		if down && !inIncident {
			count++
			inIncident = true
		} else if !down {
			inIncident = false
		}
	}
	return count
}

// formatPeriodLabel formats the period as "1 May 2026 – 31 May 2026".
func formatPeriodLabel(from, to time.Time) string {
	return fmt.Sprintf("%d %s %d – %d %s %d",
		from.Day(), from.Format("Jan"), from.Year(),
		to.Day(), to.Format("Jan"), to.Year(),
	)
}

// pgTimestamptzToTime converts an interface{} that may be a pgtype.Timestamptz
// or time.Time into a time.Time.
func pgTimestamptzToTime(v interface{}) (time.Time, bool) {
	switch t := v.(type) {
	case pgtype.Timestamptz:
		if t.Valid {
			return t.Time, true
		}
	case *time.Time:
		if t != nil {
			return *t, true
		}
	case time.Time:
		if !t.IsZero() {
			return t, true
		}
	}
	return time.Time{}, false
}
