package perf

import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/rum"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// RumResultsReader is the read-seam for the operator RUM results routes.
// rum.StorePostgres satisfies it via GetHourlyRollups + ComputeP75; a thin
// adapter is wired in cmd/wpmgr/main.go via SetRumResultsReader.
type RumResultsReader struct {
	// GetHourlyRollups returns hourly rollup rows for a site since the given time.
	GetHourlyRollups func(ctx context.Context, siteID, tenantID uuid.UUID, since time.Time) ([]rum.HourlyRollup, error)
	// ComputeP75 interpolates the 75th percentile from rollup rows.
	// Rows below minSampleCount have P75Milli == 0 (suppressed).
	ComputeP75 func(rollups []rum.HourlyRollup, minSampleCount int) []rum.P75Result
	// GetDailyRollups returns daily rollup rows for a site since the given date.
	// Used by the trend endpoint (GetRumRollupDaily window read). The trend
	// handler aggregates in-Go via rum.InterpolateP75FromCounts so no separate
	// ComputeP75Daily callback is needed on this struct.
	GetDailyRollups func(ctx context.Context, siteID, tenantID uuid.UUID, since time.Time) ([]rum.DailyRollup, error)
	// GetHourlyRollupsForSites returns hourly rollup rows for a set of sites within
	// one tenant since the given time. Used by the fleet RUM aggregate endpoint to
	// compute cross-site p75 without N+1 DB round-trips.
	// Optional: when nil the fleet endpoint returns an empty response.
	GetHourlyRollupsForSites func(ctx context.Context, tenantID uuid.UUID, siteIDs []uuid.UUID, since time.Time) ([]rum.HourlyRollup, error)
}

// SetRumResultsReader wires the RUM results list reader. When nil the
// /perf/rum and /perf/rum/summary endpoints return empty responses.
func (h *Handler) SetRumResultsReader(r *RumResultsReader) { h.rum = r }

// rumSummary handles GET /api/v1/sites/:siteId/perf/rum/summary.
// Returns site-level Core Web Vitals p75 over the requested window (default
// 28 days, matching CrUX/GSC), with good/needs-improvement/poor ratings and
// the min_sample_count suppression floor enforced server-side.
func (h *Handler) rumSummary(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}

	// Parse optional window_days query param (default 28, clamped 1–365).
	windowDays := 28
	if s := c.Query("window_days"); s != "" {
		var n int
		if _, err := parseIntParam(s, &n, 1, 365); err == nil {
			windowDays = n
		}
	}

	if h.rum == nil {
		c.JSON(http.StatusOK, RumSummaryDTO{
			WindowDays:     windowDays,
			MinSampleCount: 0,
			Metrics:        []RumMetricSummary{},
		})
		return
	}

	// Fetch the site config to retrieve min_sample_count.
	cfg, cfgErr := h.svc.GetConfig(c.Request.Context(), p.TenantID, siteID)
	if cfgErr != nil {
		httpx.Error(c, cfgErr)
		return
	}
	minSampleCount := cfg.MinSampleCount
	if minSampleCount <= 0 {
		minSampleCount = 30 // default floor: matches column DEFAULT 30 (m57)
	}

	since := time.Now().UTC().AddDate(0, 0, -windowDays)
	rollups, err := h.rum.GetHourlyRollups(c.Request.Context(), siteID, p.TenantID, since)
	if err != nil {
		httpx.Error(c, err)
		return
	}

	// Build per-(metric, device, country) bucket sums from the raw rollups.
	// These are used both to compute device-level aggregates (country-collapsed)
	// and the all-devices aggregate (device="" row the frontend "All" tab consumes).
	type sliceKey struct {
		metric  string
		device  string
		country string
	}
	type bucketAcc struct {
		counts      []int64
		sampleCount int64
		maxVal      int32
	}
	perSlice := make(map[sliceKey]*bucketAcc)
	for _, r := range rollups {
		k := sliceKey{metric: r.Metric, device: r.Device, country: r.Country}
		acc, ok := perSlice[k]
		if !ok {
			acc = &bucketAcc{counts: make([]int64, rum.NumBuckets), maxVal: r.MaxValue}
			perSlice[k] = acc
		}
		acc.sampleCount += r.SampleCount
		if r.MaxValue > acc.maxVal {
			acc.maxVal = r.MaxValue
		}
		for i, c := range r.BucketCounts {
			if i < rum.NumBuckets {
				acc.counts[i] += int64(c)
			}
		}
	}

	// Build two sets of aggregated rows per metric:
	//
	//   1. Device-level rows (one per (metric, device), country-collapsed):
	//      sum bucket_counts + sample_count across all countries for that device.
	//
	//   2. All-devices row (device="", country-collapsed):
	//      sum across all devices AND countries.
	//
	// The frontend "All" tab expects device="" rows; per-device tabs expect the
	// device-named rows. Per-(device,country) granularity is NOT emitted here
	// (that level of detail belongs to the per-URL rumResults endpoint).

	type devKey struct {
		metric string
		device string // "" for the all-devices aggregate
	}
	devAcc := make(map[devKey]*bucketAcc)

	for k, src := range perSlice {
		// Device-level aggregate (country collapsed).
		dk := devKey{metric: k.metric, device: k.device}
		da, ok := devAcc[dk]
		if !ok {
			da = &bucketAcc{counts: make([]int64, rum.NumBuckets), maxVal: src.maxVal}
			devAcc[dk] = da
		}
		da.sampleCount += src.sampleCount
		if src.maxVal > da.maxVal {
			da.maxVal = src.maxVal
		}
		for i, v := range src.counts {
			da.counts[i] += v
		}

		// All-devices aggregate (device="" sentinel).
		ak := devKey{metric: k.metric, device: ""}
		aa, ok := devAcc[ak]
		if !ok {
			aa = &bucketAcc{counts: make([]int64, rum.NumBuckets), maxVal: src.maxVal}
			devAcc[ak] = aa
		}
		aa.sampleCount += src.sampleCount
		if src.maxVal > aa.maxVal {
			aa.maxVal = src.maxVal
		}
		for i, v := range src.counts {
			aa.counts[i] += v
		}
	}

	// Sort devKey for deterministic output: (metric ASC, device ASC) with ""
	// (all-devices) sorted last per metric so device rows appear first.
	sortedDevKeys := make([]devKey, 0, len(devAcc))
	for dk := range devAcc {
		sortedDevKeys = append(sortedDevKeys, dk)
	}
	sort.Slice(sortedDevKeys, func(i, j int) bool {
		a, b := sortedDevKeys[i], sortedDevKeys[j]
		if a.metric != b.metric {
			return a.metric < b.metric
		}
		// "" sorts last within a metric group.
		if a.device == "" && b.device != "" {
			return false
		}
		if a.device != "" && b.device == "" {
			return true
		}
		return a.device < b.device
	})

	metrics := make([]RumMetricSummary, 0, len(sortedDevKeys))
	for _, dk := range sortedDevKeys {
		acc := devAcc[dk]
		suppressed := acc.sampleCount < int64(minSampleCount)
		p75 := float64(0)
		if !suppressed {
			p75 = rum.InterpolateP75FromCounts(acc.counts, acc.sampleCount, acc.maxVal)
		}
		ms := RumMetricSummary{
			Metric:      dk.metric,
			Device:      dk.device,
			Country:     "",
			P75Ms:       p75,
			SampleCount: acc.sampleCount,
			Suppressed:  suppressed,
		}
		if !suppressed {
			ms.Rating = cwvRating(dk.metric, p75)
			ms.Distribution = foldBucketsIntoDistribution(dk.metric, acc.counts)
		}
		metrics = append(metrics, ms)
	}
	if metrics == nil {
		metrics = []RumMetricSummary{}
	}

	c.JSON(http.StatusOK, RumSummaryDTO{
		WindowDays:     windowDays,
		MinSampleCount: minSampleCount,
		Metrics:        metrics,
	})
}

// rumResults handles GET /api/v1/sites/:siteId/perf/rum.
// Returns per-URL/metric/device p75 breakdown rows for the dashboard table.
// All slices below min_sample_count are returned with Suppressed=true and
// P75Ms=0 — the dashboard renders "insufficient samples" for those rows.
func (h *Handler) rumResults(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}

	// Parse optional window_days query param (default 28, clamped 1–365).
	windowDays := 28
	if s := c.Query("window_days"); s != "" {
		var n int
		if _, err := parseIntParam(s, &n, 1, 365); err == nil {
			windowDays = n
		}
	}

	if h.rum == nil {
		c.JSON(http.StatusOK, gin.H{"items": []RumResultDTO{}})
		return
	}

	cfg, cfgErr := h.svc.GetConfig(c.Request.Context(), p.TenantID, siteID)
	if cfgErr != nil {
		httpx.Error(c, cfgErr)
		return
	}
	minSampleCount := cfg.MinSampleCount
	if minSampleCount <= 0 {
		minSampleCount = 100
	}

	since := time.Now().UTC().AddDate(0, 0, -windowDays)
	rollups, err := h.rum.GetHourlyRollups(c.Request.Context(), siteID, p.TenantID, since)
	if err != nil {
		httpx.Error(c, err)
		return
	}

	// Group rollups by (url_pattern, metric, device, country) for per-URL p75.
	// We reuse ComputeP75 which groups by (metric, device, country); to get
	// per-URL rows we group the raw rollups first then call ComputeP75 per group.
	type urlKey struct {
		urlPattern string
		metric     string
		device     string
		country    string
	}
	byURL := make(map[urlKey][]rum.HourlyRollup)
	for _, r := range rollups {
		k := urlKey{
			urlPattern: r.URLPattern,
			metric:     r.Metric,
			device:     r.Device,
			country:    r.Country,
		}
		byURL[k] = append(byURL[k], r)
	}

	items := make([]RumResultDTO, 0, len(byURL))
	for k, rows := range byURL {
		p75s := h.rum.ComputeP75(rows, minSampleCount)
		for _, r := range p75s {
			suppressed := r.P75Milli == 0 && r.SampleCount < int64(minSampleCount)
			item := RumResultDTO{
				URLPattern:  k.urlPattern,
				Metric:      r.Metric,
				Device:      r.Device,
				Country:     r.Country,
				P75Ms:       r.P75Milli,
				SampleCount: r.SampleCount,
				Suppressed:  suppressed,
			}
			if !suppressed {
				item.Rating = cwvRating(r.Metric, r.P75Milli)
			}
			items = append(items, item)
		}
	}
	if items == nil {
		items = []RumResultDTO{}
	}

	c.JSON(http.StatusOK, gin.H{"items": items})
}

// rumTrend handles GET /api/v1/sites/:siteId/perf/rum/trend.
// Returns a per-metric daily p75 trend series over window_days days (default 28,
// clamped to [1,90]). Each metric entry is a slice of RumTrendDayPoint ordered
// ascending by day. Days with zero rollup rows are omitted entirely; days below
// the min_sample_count floor appear with suppressed=true and p75_ms=0 so the
// client can render a gap rather than a misleading zero. CLS p75_ms is in
// milli-units (value*1000); the client divides by 1000 for display.
//
// Auth: same canReadSite gate as rumSummary (RequireSiteAccess + PermSiteRead,
// applied by the route registration in handler.go).
func (h *Handler) rumTrend(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}

	// Parse optional window_days (default 28, clamped [1,90]).
	windowDays := 28
	if s := c.Query("window_days"); s != "" {
		var n int
		if _, err := parseIntParam(s, &n, 1, 90); err == nil {
			windowDays = n
		}
	}

	if h.rum == nil || h.rum.GetDailyRollups == nil {
		c.JSON(http.StatusOK, emptyTrendResponse(windowDays, 0))
		return
	}

	// Fetch config for min_sample_count.
	cfg, cfgErr := h.svc.GetConfig(c.Request.Context(), p.TenantID, siteID)
	if cfgErr != nil {
		httpx.Error(c, cfgErr)
		return
	}
	minSampleCount := cfg.MinSampleCount
	if minSampleCount <= 0 {
		minSampleCount = 30
	}

	since := time.Now().UTC().AddDate(0, 0, -windowDays)
	dailyRows, err := h.rum.GetDailyRollups(c.Request.Context(), siteID, p.TenantID, since)
	if err != nil {
		httpx.Error(c, err)
		return
	}

	// Optional device filter (e.g. "desktop", "mobile"). When absent, all devices
	// are included and the aggregation collapses device + country into one
	// site-level point per (metric, day).
	deviceFilter := c.Query("device")

	// Aggregate: collapse url_patterns, countries, and (when deviceFilter is "")
	// devices into one histogram per (metric, day). The key intentionally omits
	// device so that:
	//   - When deviceFilter is set: only rows for that device are included (the
	//     filter above), and each (metric, day) cell accumulates across countries.
	//   - When deviceFilter is "" (All tab): all devices pass the filter and all
	//     collapse into the same (metric, day) cell, giving the all-devices series.
	type rumTrendKey struct {
		metric string
		day    time.Time
	}
	type rumTrendAcc struct {
		counts      []int64
		sampleCount int64
		maxVal      int32
	}
	accs := make(map[rumTrendKey]*rumTrendAcc)
	for _, r := range dailyRows {
		if deviceFilter != "" && r.Device != deviceFilter {
			continue
		}
		day := r.BucketDay.UTC().Truncate(24 * time.Hour)
		k := rumTrendKey{metric: r.Metric, day: day}
		acc, found := accs[k]
		if !found {
			acc = &rumTrendAcc{counts: make([]int64, rum.NumBuckets), maxVal: r.MaxValue}
			accs[k] = acc
		}
		acc.sampleCount += r.SampleCount
		if r.MaxValue > acc.maxVal {
			acc.maxVal = r.MaxValue
		}
		for i, cnt := range r.BucketCounts {
			if i < rum.NumBuckets {
				acc.counts[i] += int64(cnt)
			}
		}
	}

	// Sort keys: (metric ASC, day ASC) for deterministic output.
	sortedKeys := make([]rumTrendKey, 0, len(accs))
	for k := range accs {
		sortedKeys = append(sortedKeys, k)
	}
	sort.Slice(sortedKeys, func(i, j int) bool {
		a, b := sortedKeys[i], sortedKeys[j]
		if a.metric != b.metric {
			return a.metric < b.metric
		}
		return a.day.Before(b.day)
	})

	// Build the per-metric point slices.
	metricsMap := make(map[string][]RumTrendDayPoint)
	for _, k := range sortedKeys {
		tacc := accs[k]
		suppressed := tacc.sampleCount < int64(minSampleCount)
		pt := RumTrendDayPoint{
			Day:         k.day.Format("2006-01-02"),
			SampleCount: tacc.sampleCount,
			Suppressed:  suppressed,
		}
		if !suppressed {
			pt.P75Ms = rum.InterpolateP75FromCounts(tacc.counts, tacc.sampleCount, tacc.maxVal)
			pt.Rating = cwvRating(k.metric, pt.P75Ms)
		}
		metricsMap[k.metric] = append(metricsMap[k.metric], pt)
	}

	// Ensure all 5 standard metrics are present (empty slice if no data).
	for _, m := range []string{"lcp", "inp", "cls", "fcp", "ttfb"} {
		if _, exists := metricsMap[m]; !exists {
			metricsMap[m] = []RumTrendDayPoint{}
		}
	}

	c.JSON(http.StatusOK, RumTrendResponse{
		WindowDays:     windowDays,
		MinSampleCount: minSampleCount,
		Metrics:        metricsMap,
	})
}

// emptyTrendResponse returns an empty RumTrendResponse for the nil-reader path.
func emptyTrendResponse(windowDays, minSampleCount int) RumTrendResponse {
	m := make(map[string][]RumTrendDayPoint)
	for _, metric := range []string{"lcp", "inp", "cls", "fcp", "ttfb"} {
		m[metric] = []RumTrendDayPoint{}
	}
	return RumTrendResponse{
		WindowDays:     windowDays,
		MinSampleCount: minSampleCount,
		Metrics:        m,
	}
}

// ---------------------------------------------------------------------------
// Fleet RUM aggregate  GET /api/v1/perf/rum/fleet
// ---------------------------------------------------------------------------

// fleetRumMetric is the per-metric summary in the fleet aggregate.
// JSON field names are pinned to the frontend FleetRumMetric contract in
// apps/web/src/features/fleet/fleet-types.ts.
type fleetRumMetric struct {
	P75         *float64 `json:"p75"`
	GoodPct     *float64 `json:"good_pct"`
	NiPct       *float64 `json:"ni_pct"`
	PoorPct     *float64 `json:"poor_pct"`
	SampleCount int64    `json:"sample_count"`
}

// fleetPerMetric is the fixed per_metric object in FleetRumResponse.
// All five keys are always present (zero-value metric when no data).
type fleetPerMetric struct {
	LCP  fleetRumMetric `json:"lcp"`
	INP  fleetRumMetric `json:"inp"`
	CLS  fleetRumMetric `json:"cls"`
	FCP  fleetRumMetric `json:"fcp"`
	TTFB fleetRumMetric `json:"ttfb"`
}

// fleetRumWorstOffender is a site with poor CWV p75 ratings.
// JSON field names are pinned to the frontend FleetRumOffender contract.
type fleetRumWorstOffender struct {
	SiteID        uuid.UUID `json:"site_id"`
	Name          string    `json:"name"`
	URL           string    `json:"url"`
	LCPP75        *float64  `json:"lcp_p75"`
	INPP75        *float64  `json:"inp_p75"`
	CLSP75        *float64  `json:"cls_p75"`
	OverallRating string    `json:"overall_rating"`
	SampleCount   int64     `json:"sample_count"`
}

// fleetRumTrendPoint is one day's fleet-level p75 values for the three
// LWV Core metrics. JSON field names are pinned to FleetRumTrendPoint.
type fleetRumTrendPoint struct {
	Date   string   `json:"date"`
	LCPP75 *float64 `json:"lcp_p75"`
	INPP75 *float64 `json:"inp_p75"`
	CLSP75 *float64 `json:"cls_p75"`
}

// FleetRumResponse is the response body for GET /api/v1/perf/rum/fleet.
// JSON field names are pinned to the frontend FleetRumResponse contract in
// apps/web/src/features/fleet/fleet-types.ts.
type FleetRumResponse struct {
	SitesReporting int                     `json:"sites_reporting"`
	SitesTotal     int                     `json:"sites_total"`
	FleetPassPct   *float64                `json:"fleet_pass_pct"`
	PerMetric      fleetPerMetric          `json:"per_metric"`
	WorstOffenders []fleetRumWorstOffender `json:"worst_offenders"`
	Trend          []fleetRumTrendPoint    `json:"trend"`
}

// rumFleet handles GET /api/v1/perf/rum/fleet.
// Returns a fleet-level CWV aggregate across all tenant sites that reported
// RUM data in the window. Query params:
//
//	window_days — 1-365, default 28.
//	device      — desktop | mobile | tablet | all (default all).
//
// Response shape is pinned to FleetRumResponse / fleet-types.ts.
func (h *Handler) rumFleet(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())

	windowDays := 28
	if s := c.Query("window_days"); s != "" {
		var n int
		if _, err := parseIntParam(s, &n, 1, 365); err == nil {
			windowDays = n
		}
	}
	deviceFilter := c.Query("device") // empty or "all" = no device filter

	emptyResp := func(sitesTotal int) FleetRumResponse {
		return FleetRumResponse{
			SitesReporting: 0,
			SitesTotal:     sitesTotal,
			FleetPassPct:   nil,
			PerMetric:      fleetPerMetric{},
			WorstOffenders: []fleetRumWorstOffender{},
			Trend:          []fleetRumTrendPoint{},
		}
	}

	if h.rum == nil || h.rum.GetHourlyRollupsForSites == nil {
		c.JSON(http.StatusOK, emptyResp(0))
		return
	}

	// Enumerate all tenant site IDs (this endpoint is RequireOrgScope so
	// site-scoped principals are already blocked by middleware).
	siteIDs, err := h.svc.ListAllSiteIDs(c.Request.Context(), p.TenantID)
	if err != nil {
		httpx.Error(c, err)
		return
	}

	since := time.Now().UTC().AddDate(0, 0, -windowDays)
	rollups, err := h.rum.GetHourlyRollupsForSites(c.Request.Context(), p.TenantID, siteIDs, since)
	if err != nil {
		httpx.Error(c, err)
		return
	}

	if len(rollups) == 0 {
		c.JSON(http.StatusOK, emptyResp(len(siteIDs)))
		return
	}

	// Track which sites reported data.
	reportingSites := make(map[uuid.UUID]struct{})
	for _, r := range rollups {
		reportingSites[r.SiteID] = struct{}{}
	}

	// perSiteMetricAcc accumulates bucket histogram data per (siteID, metric).
	type siteMetricKey struct {
		siteID uuid.UUID
		metric string
	}
	type bucketAcc struct {
		counts      []int64
		sampleCount int64
		maxVal      int32
	}

	// Fleet-level acc per metric (all sites + devices collapsed).
	fleetAcc := make(map[string]*bucketAcc)
	// Per-site per-metric acc (for worst-offenders: we track all 3 CWV metrics).
	siteAcc := make(map[siteMetricKey]*bucketAcc)
	// Per-(metric, day) acc for trend (hourly rollups bucketed to UTC days).
	type trendKey struct {
		metric string
		day    string // "YYYY-MM-DD"
	}
	trendAcc := make(map[trendKey]*bucketAcc)

	for _, r := range rollups {
		// Apply device filter.
		if deviceFilter != "" && deviceFilter != "all" && r.Device != deviceFilter {
			continue
		}

		// Fleet-level accumulation.
		fa, ok := fleetAcc[r.Metric]
		if !ok {
			fa = &bucketAcc{counts: make([]int64, rum.NumBuckets)}
			fleetAcc[r.Metric] = fa
		}
		fa.sampleCount += r.SampleCount
		if r.MaxValue > fa.maxVal {
			fa.maxVal = r.MaxValue
		}
		for i, cnt := range r.BucketCounts {
			if i < rum.NumBuckets {
				fa.counts[i] += int64(cnt)
			}
		}

		// Per-site per-metric accumulation (worst-offenders need lcp/inp/cls).
		smk := siteMetricKey{siteID: r.SiteID, metric: r.Metric}
		sa, ok := siteAcc[smk]
		if !ok {
			sa = &bucketAcc{counts: make([]int64, rum.NumBuckets)}
			siteAcc[smk] = sa
		}
		sa.sampleCount += r.SampleCount
		if r.MaxValue > sa.maxVal {
			sa.maxVal = r.MaxValue
		}
		for i, cnt := range r.BucketCounts {
			if i < rum.NumBuckets {
				sa.counts[i] += int64(cnt)
			}
		}

		// Trend accumulation: bucket hourly rollups into UTC days.
		// Only track the three trend metrics: lcp, inp, cls.
		if r.Metric == "lcp" || r.Metric == "inp" || r.Metric == "cls" {
			// r.HourBucket is already a time.Time from the rum.HourlyRollup.
			// We extract the UTC date from BucketHour.
			dayStr := r.BucketHour.UTC().Format("2006-01-02")
			tk := trendKey{metric: r.Metric, day: dayStr}
			ta, ok := trendAcc[tk]
			if !ok {
				ta = &bucketAcc{counts: make([]int64, rum.NumBuckets)}
				trendAcc[tk] = ta
			}
			ta.sampleCount += r.SampleCount
			if r.MaxValue > ta.maxVal {
				ta.maxVal = r.MaxValue
			}
			for i, cnt := range r.BucketCounts {
				if i < rum.NumBuckets {
					ta.counts[i] += int64(cnt)
				}
			}
		}
	}

	const minSampleCount = 30

	// buildMetric converts a fleet-level bucket accumulator to a fleetRumMetric.
	buildMetric := func(metric string, fa *bucketAcc) fleetRumMetric {
		if fa == nil || fa.sampleCount < int64(minSampleCount) {
			return fleetRumMetric{SampleCount: 0}
		}
		p75 := rum.InterpolateP75FromCounts(fa.counts, fa.sampleCount, fa.maxVal)
		dist := foldBucketsIntoDistribution(metric, fa.counts)
		var goodPct, niPct, poorPct *float64
		if dist != nil {
			gp := float64(dist.GoodPct)
			np := float64(dist.NeedsImprovementPct)
			pp := float64(dist.PoorPct)
			goodPct = &gp
			niPct = &np
			poorPct = &pp
		}
		return fleetRumMetric{
			P75:         &p75,
			GoodPct:     goodPct,
			NiPct:       niPct,
			PoorPct:     poorPct,
			SampleCount: fa.sampleCount,
		}
	}

	perMetric := fleetPerMetric{
		LCP:  buildMetric("lcp", fleetAcc["lcp"]),
		INP:  buildMetric("inp", fleetAcc["inp"]),
		CLS:  buildMetric("cls", fleetAcc["cls"]),
		FCP:  buildMetric("fcp", fleetAcc["fcp"]),
		TTFB: buildMetric("ttfb", fleetAcc["ttfb"]),
	}

	// Fleet-pass-pct: fraction of reporting sites with "good" LCP p75.
	var fleetPassPct *float64
	var lcpGoodSites, lcpTotalSites int
	for smk, sa := range siteAcc {
		if smk.metric != "lcp" {
			continue
		}
		if sa.sampleCount < int64(minSampleCount) {
			continue
		}
		lcpTotalSites++
		siteP75 := rum.InterpolateP75FromCounts(sa.counts, sa.sampleCount, sa.maxVal)
		if cwvRating("lcp", siteP75) == "good" {
			lcpGoodSites++
		}
	}
	if lcpTotalSites > 0 {
		v := float64(lcpGoodSites) / float64(lcpTotalSites) * 100
		fleetPassPct = &v
	}

	// Worst offenders: collect unique sites with poor/needs-improvement LCP,
	// joined with their inp/cls p75 values.
	type offenderAcc struct {
		lcpP75  float64
		inpP75  *float64
		clsP75  *float64
		samples int64
		rating  string
	}
	offenderMap := make(map[uuid.UUID]*offenderAcc)
	for smk, sa := range siteAcc {
		if smk.metric != "lcp" {
			continue
		}
		if sa.sampleCount < int64(minSampleCount) {
			continue
		}
		p75 := rum.InterpolateP75FromCounts(sa.counts, sa.sampleCount, sa.maxVal)
		rating := cwvRating("lcp", p75)
		// normalise: frontend expects "needs-improvement" (hyphen), not underscore
		if rating == "needs_improvement" {
			rating = "needs-improvement"
		}
		if rating == "poor" || rating == "needs-improvement" {
			offenderMap[smk.siteID] = &offenderAcc{
				lcpP75:  p75,
				samples: sa.sampleCount,
				rating:  rating,
			}
		}
	}
	// Enrich offenders with inp/cls p75 values.
	for smk, sa := range siteAcc {
		oa, isOffender := offenderMap[smk.siteID]
		if !isOffender {
			continue
		}
		if sa.sampleCount < int64(minSampleCount) {
			continue
		}
		p75 := rum.InterpolateP75FromCounts(sa.counts, sa.sampleCount, sa.maxVal)
		switch smk.metric {
		case "inp":
			oa.inpP75 = &p75
		case "cls":
			oa.clsP75 = &p75
		}
	}

	worstOffenders := make([]fleetRumWorstOffender, 0, len(offenderMap))
	for siteID, oa := range offenderMap {
		lcpP75 := oa.lcpP75
		worstOffenders = append(worstOffenders, fleetRumWorstOffender{
			SiteID:        siteID,
			Name:          "", // name/url not available without N+1 lookup; frontend tolerates empty
			URL:           "",
			LCPP75:        &lcpP75,
			INPP75:        oa.inpP75,
			CLSP75:        oa.clsP75,
			OverallRating: oa.rating,
			SampleCount:   oa.samples,
		})
	}
	sort.Slice(worstOffenders, func(i, j int) bool {
		// Sort by LCP p75 descending (worst first).
		li, lj := float64(0), float64(0)
		if worstOffenders[i].LCPP75 != nil {
			li = *worstOffenders[i].LCPP75
		}
		if worstOffenders[j].LCPP75 != nil {
			lj = *worstOffenders[j].LCPP75
		}
		return li > lj
	})
	if len(worstOffenders) > 10 {
		worstOffenders = worstOffenders[:10]
	}

	// Build trend: one point per day with lcp_p75/inp_p75/cls_p75.
	// Collect all days across the three trend metrics, then join.
	trendDays := make(map[string]struct{})
	for tk := range trendAcc {
		trendDays[tk.day] = struct{}{}
	}
	sortedDays := make([]string, 0, len(trendDays))
	for d := range trendDays {
		sortedDays = append(sortedDays, d)
	}
	sort.Strings(sortedDays)

	trend := make([]fleetRumTrendPoint, 0, len(sortedDays))
	for _, day := range sortedDays {
		pt := fleetRumTrendPoint{Date: day}
		for _, metric := range []string{"lcp", "inp", "cls"} {
			ta := trendAcc[trendKey{metric: metric, day: day}]
			if ta == nil || ta.sampleCount < int64(minSampleCount) {
				continue
			}
			p75 := rum.InterpolateP75FromCounts(ta.counts, ta.sampleCount, ta.maxVal)
			switch metric {
			case "lcp":
				pt.LCPP75 = &p75
			case "inp":
				pt.INPP75 = &p75
			case "cls":
				pt.CLSP75 = &p75
			}
		}
		trend = append(trend, pt)
	}

	c.JSON(http.StatusOK, FleetRumResponse{
		SitesReporting: len(reportingSites),
		SitesTotal:     len(siteIDs),
		FleetPassPct:   fleetPassPct,
		PerMetric:      perMetric,
		WorstOffenders: worstOffenders,
		Trend:          trend,
	})
}

// rotateRumBeaconKey handles POST /api/v1/sites/:siteId/perf/rum/rotate-key
// (GH #174). Unconditionally mints a fresh RUM beacon key and pushes it to the
// agent — the deterministic operator recovery path for a beacon key that got
// stuck permanently empty on the agent because the one best-effort mint+push
// on first RUM-enable was lost (agent down/unreachable at that moment).
//
// PINNED response contract: {"ok": true, "beacon_key_set": true} on success.
// The plaintext key is NEVER included in the response, logged, or returned by
// the service layer at all — only this confirmation boolean.
func (h *Handler) rotateRumBeaconKey(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}
	if err := h.svc.RotateBeaconKey(c.Request.Context(), p.TenantID, siteID); err != nil {
		if _, isDomain := domain.AsDomain(err); isDomain {
			httpx.Error(c, err)
			return
		}
		// Non-domain = agent push failure AFTER the hash was already rotated and
		// persisted. Mirrors putConfig's identical "config stored but agent push
		// failed" handling: 200 + a warning header, because silently reporting
		// success would hide that the fresh key has NOT reached the agent yet.
		c.Header("X-Agent-Push-Warning", err.Error())
		c.JSON(http.StatusOK, gin.H{"ok": true, "beacon_key_set": true})
		return
	}
	h.record(c, p, audit.ActionRumBeaconKeyRotated, siteID, nil)
	c.JSON(http.StatusOK, gin.H{"ok": true, "beacon_key_set": true})
}

// parseIntParam parses an integer from a string, clamping to [min, max].
// Returns an error if the string is not a valid integer. Sets *out on success.
func parseIntParam(s string, out *int, minV, maxV int) (int, error) {
	var n int
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return 0, domain.Validation("invalid_param", "not a valid integer")
		}
		n = n*10 + int(ch-'0')
	}
	if n < minV {
		n = minV
	}
	if n > maxV {
		n = maxV
	}
	*out = n
	return n, nil
}
