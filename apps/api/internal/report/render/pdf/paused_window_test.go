package pdf

import (
	"bytes"
	"testing"
	"time"

	fpdflib "codeberg.org/go-pdf/fpdf"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/report/reportdata"
)

// GH #414 report-renderer review, finding 1. drawPausedRow's notice ("no
// uptime figures are shown for this period") was gated on
// SiteReport.MonitoringPaused — the site's CURRENT pause state — while the
// uptime section a few lines below is gated on SiteReport.Uptime, which is
// nil only when a pause interval overlapped the REPORTING WINDOW (see
// aggregator.go's overlapForIntervals and reportdata.go's SiteReport.Uptime
// doc comment). A site paused AFTER the window closed satisfies both:
// currently paused (so the title prints), but with no overlap against the
// window (so Uptime is fully populated). The document said "no uptime
// figures are shown for this period" directly above the very figures it
// denied.
//
// pausedWindowFixture builds one site and lets the caller set MonitoringPaused
// state and Uptime independently, exactly as the render layer receives them —
// this package never runs the aggregator, so the two must be wired by hand to
// reproduce the disagreement.
func pausedWindowFixture(paused bool, pausedAt *time.Time, uptime *reportdata.UptimeSection) reportdata.ReportData {
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
		Sections:      reportdata.DefaultSectionFlags(),
		Totals: reportdata.ReportTotals{
			SiteCount: 1, AvgUptimePct: 99.2, UptimeSiteCount: 1,
		},
		Sites: []reportdata.SiteReport{sr},
	}
}

func fullUptimeFixture() *reportdata.UptimeSection {
	return &reportdata.UptimeSection{
		UptimePct:    99.2,
		AvgLatencyMs: 210,
		Checks:       1000,
		DownChecks:   8,
		Incidents:    2,
	}
}

// TestPDFPausedAfterWindowShowsUptimeNoDenial is finding 1's regression test:
// a site paused after the reporting window closed must not have the document
// both deny uptime figures and show them.
func TestPDFPausedAfterWindowShowsUptimeNoDenial(t *testing.T) {
	fpdflib.SetDefaultCompression(false)
	t.Cleanup(func() { fpdflib.SetDefaultCompression(true) })

	pausedAt := time.Date(2026, 6, 5, 0, 0, 0, 0, time.UTC) // after PeriodEnd (1 Jun)
	data := pausedWindowFixture(true, &pausedAt, fullUptimeFixture())

	r := NewFpdfRenderer()
	out, err := r.Render(data, nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !bytes.Contains(out, asciiUTF16BE("Monitoring paused")) {
		t.Error("rendered PDF drops the pause title for a currently-paused site")
	}
	if !bytes.Contains(out, asciiUTF16BE("Backups continue as normal")) {
		t.Error("rendered PDF drops the unconditional backups reassurance")
	}
	if !bytes.Contains(out, asciiUTF16BE("avg latency")) {
		t.Error("rendered PDF does not show uptime figures for a site paused after the window closed")
	}
	if bytes.Contains(out, asciiUTF16BE("no uptime figures are shown")) {
		t.Error("rendered PDF denies uptime figures directly above the uptime figures it also shows")
	}
}

// TestPDFPausedFullWindowStillSuppresses is the over-fire control: a site
// whose pause genuinely covers the whole window (Uptime == nil, exactly as
// the aggregator leaves it on full overlap) must still print the notice and
// still omit the uptime section.
func TestPDFPausedFullWindowStillSuppresses(t *testing.T) {
	fpdflib.SetDefaultCompression(false)
	t.Cleanup(func() { fpdflib.SetDefaultCompression(true) })

	pausedAt := time.Date(2026, 4, 20, 0, 0, 0, 0, time.UTC) // before PeriodStart, covers whole window
	data := pausedWindowFixture(true, &pausedAt, nil)

	r := NewFpdfRenderer()
	out, err := r.Render(data, nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !bytes.Contains(out, asciiUTF16BE("no uptime figures are shown")) {
		t.Error("rendered PDF drops the no-uptime notice for a site paused across the whole window")
	}
	if bytes.Contains(out, asciiUTF16BE("avg latency")) {
		t.Error("rendered PDF shows uptime figures for a site whose uptime section is nil")
	}
}

// TestPDFNoPauseUnchanged is the second over-fire control: a site with no
// pause at all renders exactly as before — no paused-note of any kind, and
// its uptime figures print normally.
func TestPDFNoPauseUnchanged(t *testing.T) {
	fpdflib.SetDefaultCompression(false)
	t.Cleanup(func() { fpdflib.SetDefaultCompression(true) })

	data := pausedWindowFixture(false, nil, fullUptimeFixture())

	r := NewFpdfRenderer()
	out, err := r.Render(data, nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if bytes.Contains(out, asciiUTF16BE("Monitoring paused")) {
		t.Error("rendered PDF shows a pause title for a site that was never paused")
	}
	if bytes.Contains(out, asciiUTF16BE("no uptime figures are shown")) {
		t.Error("rendered PDF shows the no-uptime notice for a site that was never paused")
	}
	if !bytes.Contains(out, asciiUTF16BE("avg latency")) {
		t.Error("rendered PDF drops uptime figures for an unpaused site")
	}
}
