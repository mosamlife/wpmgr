package report

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/metrics"
	"github.com/mosamlife/wpmgr/apps/api/internal/report/render/html"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

// GH #414 phase 5 — scheduled reports exclude paused sites from the uptime
// numbers and NAME them as paused.
//
// The failure this guards is specific and it is what the feature was reported
// for: phase 2 stopped the uptime prober for a paused site, so its daily series
// is empty for every paused day. A report that kept rendering the uptime section
// showed a month of floor-height sparkline bars, which is visually identical to
// a month of downtime, in a document the agency emails to its client. The
// paused site must appear, must say why it has no uptime figures, and must not
// enter the fleet average as a 0%.

// pauseFixture builds a client fleet with `paused` paused sites and `active`
// active ones, every active site observed at the given uptime percentages.
type pauseFixture struct {
	sites      []site.Site
	uptimePcts map[uuid.UUID]float64
	// probed records every site id the uptime source was asked about, so a test
	// can assert a paused site cost no query rather than merely no output.
	probed []uuid.UUID
}

func newPauseFixture(t *testing.T, activePcts []float64, pausedCount int) *pauseFixture {
	t.Helper()
	f := &pauseFixture{uptimePcts: map[uuid.UUID]float64{}}
	for i, pct := range activePcts {
		id := uuid.New()
		f.sites = append(f.sites, site.Site{ID: id, Name: "active-" + string(rune('a'+i)), URL: "https://a.example"})
		f.uptimePcts[id] = pct
	}
	pausedAt := time.Date(2026, 5, 3, 9, 0, 0, 0, time.UTC)
	for i := 0; i < pausedCount; i++ {
		id := uuid.New()
		f.sites = append(f.sites, site.Site{
			ID:                     id,
			Name:                   "paused-" + string(rune('a'+i)),
			URL:                    "https://p.example",
			MonitoringPausedAt:     &pausedAt,
			MonitoringPausedReason: "migrating to new host",
		})
	}
	return f
}

func (f *pauseFixture) sources() Sources {
	return Sources{
		ListClientSites: func(context.Context, uuid.UUID, uuid.UUID) ([]site.Site, error) {
			return f.sites, nil
		},
		QueryUptimeAggregateRange: func(_ context.Context, _, siteID uuid.UUID, _, _ time.Time) (metrics.Aggregate, error) {
			f.probed = append(f.probed, siteID)
			pct, ok := f.uptimePcts[siteID]
			if !ok {
				// A paused site reaching here is itself the defect; return a
				// perfect score so the assertion fails loudly on the average
				// rather than silently agreeing by accident.
				return metrics.Aggregate{Checks: 100, UpChecks: 100, UptimePct: 100}, nil
			}
			up := uint64(pct * 10)
			return metrics.Aggregate{Checks: 1000, UpChecks: up, UptimePct: pct}, nil
		},
	}
}

// renderHTML runs the REAL renderer the delivery path uses, not a stub: the
// struct field is not what the client reads, the rendered document is.
func renderHTML(t *testing.T, rd ReportData) string {
	t.Helper()
	r, err := html.NewRenderer()
	if err != nil {
		t.Fatalf("html.NewRenderer: %v", err)
	}
	out, err := r.Render(rd, nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	return string(out)
}

func buildFixture(t *testing.T, f *pauseFixture) ReportData {
	t.Helper()
	rd, err := BuildReportData(context.Background(), f.sources(), BuildInput{
		TenantID:    uuid.New(),
		ClientID:    uuid.New(),
		Client:      ClientInfo{Name: "Acme"},
		AgencyName:  "Agency",
		PeriodStart: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
		PeriodEnd:   time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("BuildReportData: %v", err)
	}
	return rd
}

// TestReportNamesPausedSiteRatherThanShowingGaps is the headline assertion:
// the paused site is present, flagged, carries its reason, and has NO uptime
// section at all.
func TestReportNamesPausedSiteRatherThanShowingGaps(t *testing.T) {
	f := newPauseFixture(t, []float64{99, 97}, 1)
	rd := buildFixture(t, f)

	if len(rd.Sites) != 3 {
		t.Fatalf("the paused site must stay IN the report: want 3 site reports, got %d", len(rd.Sites))
	}
	var paused *SiteReport
	for i := range rd.Sites {
		if strings.HasPrefix(rd.Sites[i].Name, "paused-") {
			paused = &rd.Sites[i]
		}
	}
	if paused == nil {
		t.Fatal("paused site was dropped from the report entirely")
	}
	if !paused.MonitoringPaused {
		t.Error("paused site is not flagged MonitoringPaused; the report cannot name it as paused")
	}
	if paused.MonitoringPausedAt == nil {
		t.Error("paused site carries no MonitoringPausedAt; the report cannot say since when")
	}
	if paused.MonitoringPausedReason != "migrating to new host" {
		t.Errorf("paused reason lost: %q", paused.MonitoringPausedReason)
	}
	if paused.Uptime != nil {
		t.Errorf("paused site kept an uptime section (%.2f%%); that is the month-of-gaps defect", paused.Uptime.UptimePct)
	}
	// The uptime source must not even be consulted for a paused site.
	for _, id := range f.probed {
		if id == paused.SiteID {
			t.Error("uptime source was queried for a paused site; the skip must precede the query")
		}
	}
	// The active sites keep theirs.
	for _, sr := range rd.Sites {
		if strings.HasPrefix(sr.Name, "active-") && sr.Uptime == nil {
			t.Errorf("active site %s lost its uptime section", sr.Name)
		}
	}
}

// TestFleetAveragePercentageExcludesPausedSitesFromTheDenominator is the
// hand-computed check the brief asks for: 99 and 97 over TWO sites is 98.0, not
// 98.0-dragged-down-by-a-third-site.
func TestFleetAveragePercentageExcludesPausedSitesFromTheDenominator(t *testing.T) {
	f := newPauseFixture(t, []float64{99, 97}, 1)
	rd := buildFixture(t, f)

	const want = 98.0 // (99 + 97) / 2, by hand, over the UNPAUSED sites only.
	if diff := rd.Totals.AvgUptimePct - want; diff > 0.001 || diff < -0.001 {
		t.Errorf("AvgUptimePct = %.4f, want %.4f — a paused site must not enter the denominator", rd.Totals.AvgUptimePct, want)
	}
	if rd.Totals.UptimeSiteCount != 2 {
		t.Errorf("UptimeSiteCount = %d, want 2 (the measured population)", rd.Totals.UptimeSiteCount)
	}
	if rd.Totals.PausedSiteCount != 1 {
		t.Errorf("PausedSiteCount = %d, want 1", rd.Totals.PausedSiteCount)
	}
	if rd.Totals.SiteCount != 3 {
		t.Errorf("SiteCount = %d, want 3 — the client is still billed for three sites", rd.Totals.SiteCount)
	}
	// The old arithmetic would have divided by 3 and produced 65.33.
	old := (99.0 + 97.0) / 3.0
	if diff := rd.Totals.AvgUptimePct - old; diff < 0.001 && diff > -0.001 {
		t.Errorf("AvgUptimePct still divides by len(sites) (%.4f)", old)
	}
}

// TestPausingAWorstSiteCannotFlatterTheAverage is the incentive check named in
// the brief: pause your worst site and the number must not improve.
func TestPausingAWorstSiteCannotFlatterTheAverage(t *testing.T) {
	// Baseline: three active sites, one of them terrible.
	base := newPauseFixture(t, []float64{100, 100, 40}, 0)
	baseRD := buildFixture(t, base)
	wantBase := (100.0 + 100.0 + 40.0) / 3.0
	if diff := baseRD.Totals.AvgUptimePct - wantBase; diff > 0.001 || diff < -0.001 {
		t.Fatalf("baseline AvgUptimePct = %.4f, want %.4f", baseRD.Totals.AvgUptimePct, wantBase)
	}

	// Now pause the worst one. The average over the REMAINING measured sites is
	// legitimately 100 — that is honest, because the report also says the
	// average covers 2 of 3 sites and names the third as paused. What must NOT
	// happen is the paused site being counted as healthy inside a denominator
	// of 3, which would print 100% over "3 sites" and read as a perfect fleet.
	withPause := newPauseFixture(t, []float64{100, 100}, 1)
	pausedRD := buildFixture(t, withPause)
	if pausedRD.Totals.UptimeSiteCount != 2 {
		t.Errorf("UptimeSiteCount = %d, want 2 — the report must disclose the shrunken denominator", pausedRD.Totals.UptimeSiteCount)
	}
	if pausedRD.Totals.SiteCount == pausedRD.Totals.UptimeSiteCount {
		t.Error("SiteCount and UptimeSiteCount agree, so the reader cannot tell a site was excluded")
	}
	if pausedRD.Totals.PausedSiteCount != 1 {
		t.Errorf("PausedSiteCount = %d, want 1", pausedRD.Totals.PausedSiteCount)
	}
}

// TestNothingMeasuredIsNotZeroPercent — an all-paused client gets "no data",
// never a 0.0% that reads as a total outage.
func TestNothingMeasuredIsNotZeroPercent(t *testing.T) {
	f := newPauseFixture(t, nil, 2)
	rd := buildFixture(t, f)
	if rd.Totals.UptimeSiteCount != 0 {
		t.Fatalf("UptimeSiteCount = %d, want 0", rd.Totals.UptimeSiteCount)
	}
	out := renderHTML(t, rd)
	if strings.Contains(out, "0.0%") {
		t.Error("an all-paused report printed 0.0% average uptime, which reads as a total outage")
	}
}

// TestRenderedReportSaysPausedAndSaysBackupsContinue pins the rendered HTML,
// because the struct field is not what the client reads.
func TestRenderedReportSaysPausedAndSaysBackupsContinue(t *testing.T) {
	f := newPauseFixture(t, []float64{99}, 1)
	rd := buildFixture(t, f)
	body := renderHTML(t, rd)
	if !strings.Contains(body, "Monitoring paused") {
		t.Error("rendered report never says the site's monitoring is paused")
	}
	if !strings.Contains(body, "Backups continue as normal") {
		t.Error("rendered report does not say backups continue; a client will assume they stopped")
	}
	if !strings.Contains(body, "migrating to new host") {
		t.Error("rendered report drops the operator's reason, leaving the pause unexplained")
	}
	if !strings.Contains(body, "monitored") {
		t.Error("the Avg Uptime label does not disclose the shrunken denominator")
	}
}
