package html

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/report/reportdata"
)

// GH #414 report-renderer review, finding 1. See the PDF twin
// (../pdf/paused_window_test.go) for the full account: the .paused-note
// block's "no uptime figures are shown for this period" sentence was gated
// on .MonitoringPaused (current pause state) while the uptime section below
// it is gated on .Uptime (whether a pause interval overlapped the reporting
// window). A site paused AFTER the window closed satisfies both, so the
// document denied and then showed the same figures. HTML and PDF are
// separate code paths and must agree.

func pausedWindowFixtureHTML(paused bool, pausedAt *time.Time, uptime *reportdata.UptimeSection) reportdata.ReportData {
	sr := reportdata.SiteReport{
		SiteID:             uuid.New(),
		Name:               "site-a",
		URL:                "https://a.example",
		MonitoringPaused:   paused,
		MonitoringPausedAt: pausedAt,
		Uptime:             uptime,
	}
	return reportdata.ReportData{
		SchemaVersion: 1,
		GeneratedAt:   time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC),
		PeriodStart:   time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
		PeriodEnd:     time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		PeriodLabel:   "1 May 2026 - 31 May 2026",
		ClientID:      uuid.New(),
		ClientName:    "Acme",
		AgencyName:    "Agency",
		ShowPoweredBy: true,
		Sections:      reportdata.DefaultSectionFlags(),
		Totals: reportdata.ReportTotals{
			SiteCount: 1, AvgUptimePct: 99.2, UptimeSiteCount: 1,
		},
		Sites: []reportdata.SiteReport{sr},
	}
}

func fullUptimeFixtureHTML() *reportdata.UptimeSection {
	return &reportdata.UptimeSection{
		UptimePct:    99.2,
		AvgLatencyMs: 210,
		Checks:       1000,
		DownChecks:   8,
		Incidents:    2,
	}
}

// TestHTMLPausedAfterWindowShowsUptimeNoDenial is finding 1's regression
// test: a site paused after the reporting window closed must not have the
// document both deny uptime figures and show them.
func TestHTMLPausedAfterWindowShowsUptimeNoDenial(t *testing.T) {
	pausedAt := time.Date(2026, 6, 5, 0, 0, 0, 0, time.UTC) // after PeriodEnd (1 Jun)
	data := pausedWindowFixtureHTML(true, &pausedAt, fullUptimeFixtureHTML())

	r, err := NewRenderer()
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}
	out, err := r.Render(data, nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	body := string(out)

	if !strings.Contains(body, "Monitoring paused") {
		t.Error("rendered report drops the pause title for a currently-paused site")
	}
	if !strings.Contains(body, "Backups continue as normal") {
		t.Error("rendered report drops the unconditional backups reassurance")
	}
	if !strings.Contains(body, "99.20") {
		t.Errorf("rendered report does not show uptime figures for a site paused after the window closed:\n%s", body)
	}
	if strings.Contains(body, "no uptime figures are shown") {
		t.Error("rendered report denies uptime figures directly above the uptime figures it also shows")
	}
}

// TestHTMLPausedFullWindowStillSuppresses is the over-fire control: a site
// whose pause genuinely covers the whole window (.Uptime is nil, exactly as
// the aggregator leaves it on full overlap) must still print the notice and
// still omit the uptime section.
func TestHTMLPausedFullWindowStillSuppresses(t *testing.T) {
	pausedAt := time.Date(2026, 4, 20, 0, 0, 0, 0, time.UTC) // before PeriodStart, covers whole window
	data := pausedWindowFixtureHTML(true, &pausedAt, nil)

	r, err := NewRenderer()
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}
	out, err := r.Render(data, nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	body := string(out)

	if !strings.Contains(body, "no uptime figures are shown") {
		t.Error("rendered report drops the no-uptime notice for a site paused across the whole window")
	}
	if strings.Contains(body, "99.20") {
		t.Error("rendered report shows uptime figures for a site whose uptime section is nil")
	}
}

// TestHTMLNoPauseUnchanged is the second over-fire control: a site with no
// pause at all renders exactly as before.
func TestHTMLNoPauseUnchanged(t *testing.T) {
	data := pausedWindowFixtureHTML(false, nil, fullUptimeFixtureHTML())

	r, err := NewRenderer()
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}
	out, err := r.Render(data, nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	body := string(out)

	if strings.Contains(body, "Monitoring paused") {
		t.Error("rendered report shows a pause title for a site that was never paused")
	}
	if strings.Contains(body, "no uptime figures are shown") {
		t.Error("rendered report shows the no-uptime notice for a site that was never paused")
	}
	if !strings.Contains(body, "99.20") {
		t.Error("rendered report drops uptime figures for an unpaused site")
	}
}
