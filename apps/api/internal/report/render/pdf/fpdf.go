package pdf

import (
	"bytes"
	"fmt"
	"math"
	"strings"

	fpdflib "codeberg.org/go-pdf/fpdf"

	"github.com/mosamlife/wpmgr/apps/api/internal/report/reportdata"
)

// Renderer is the thin interface seam for PDF rendering (LOCKED caveat e).
// The concrete fpdf-backed implementation is the only type in this package.
type Renderer interface {
	Render(data reportdata.ReportData, logo []byte) ([]byte, error)
}

// FpdfRenderer renders ReportData to a binary PDF using native fpdf vector
// primitives. Charts are NEVER derived from the HTML twin's SVG — they are
// drawn from the ReportData series directly as fpdf Line/Rect/Polygon calls.
type FpdfRenderer struct{}

// NewFpdfRenderer constructs the fpdf-backed renderer.
func NewFpdfRenderer() *FpdfRenderer { return &FpdfRenderer{} }

const (
	marginL  = 15.0
	marginT  = 15.0
	marginR  = 15.0
	pageW    = 210.0 // A4
	pageH    = 297.0
	contentW = pageW - marginL - marginR
)

// Render renders a PDF from ReportData + optional validated logo bytes.
func (r *FpdfRenderer) Render(data reportdata.ReportData, logo []byte) ([]byte, error) {
	f := fpdflib.New("P", "mm", "A4", "")
	f.SetMargins(marginL, marginT, marginR)
	f.SetAutoPageBreak(true, 15)
	if err := registerFonts(f); err != nil {
		return nil, err
	}
	f.AddPage()

	// -------------------------------------------------------------------------
	// Header
	// -------------------------------------------------------------------------
	// Logo (validated bytes only — never hot-link logo_url).
	if len(logo) > 0 {
		imgOpts := fpdflib.ImageOptions{ImageType: "png"}
		if len(logo) >= 4 && !(logo[0] == 0x89 && logo[1] == 0x50) {
			imgOpts.ImageType = "jpg"
		}
		f.RegisterImageOptionsReader("logo", imgOpts, bytes.NewReader(logo))
		f.ImageOptions("logo", marginL, marginT, 40, 0, false, imgOpts, 0, "")
		f.SetY(marginT + 18)
	}

	// Client name + period.
	accent := colorFromHex(data.AccentColor)
	f.SetFont("dejavu", "B", 18)
	f.SetTextColor(15, 23, 42)
	f.CellFormat(contentW, 10, data.ClientName, "", 1, "L", false, 0, "")
	f.SetFont("dejavu", "", 11)
	f.SetTextColor(100, 116, 139)
	f.CellFormat(contentW, 7, "Report for "+data.PeriodLabel, "", 1, "L", false, 0, "")
	f.CellFormat(contentW, 6, "Prepared by "+data.AgencyName, "", 1, "L", false, 0, "")

	// Accent line.
	f.SetDrawColor(accent[0], accent[1], accent[2])
	f.SetLineWidth(0.8)
	y := f.GetY() + 3
	f.Line(marginL, y, marginL+contentW, y)
	f.SetLineWidth(0.2)
	f.Ln(5)

	if data.IntroText != "" {
		f.SetFont("dejavu", "", 10)
		f.SetTextColor(55, 65, 81)
		f.MultiCell(contentW, 5, data.IntroText, "", "L", false)
		f.Ln(3)
	}

	// -------------------------------------------------------------------------
	// Overview totals
	// -------------------------------------------------------------------------
	if data.Sections.Overview {
		sectionHeader(f, accent, "Overview")
		drawTotalsGrid(f, data)
		f.Ln(4)
	}

	// -------------------------------------------------------------------------
	// Per-site sections
	// -------------------------------------------------------------------------
	for _, s := range data.Sites {
		sectionHeader(f, accent, s.Name)
		f.SetFont("dejavu", "", 9)
		f.SetTextColor(107, 114, 128)
		f.CellFormat(contentW, 5, s.URL, "", 1, "L", false, 0, "")
		f.Ln(2)

		if s.MonitoringPaused {
			drawPausedRow(f, s)
		}
		if s.Uptime != nil {
			drawUptimeRow(f, s.Uptime)
		}
		if s.Backups != nil {
			drawBackupRow(f, s.Backups)
		}
		if s.Updates != nil {
			drawUpdatesRow(f, s.Updates)
		}
		if s.Performance != nil && len(s.Performance.Metrics) > 0 {
			drawPerfRow(f, s.Performance)
		}
		if s.Email != nil {
			drawEmailRow(f, s.Email)
		}
		f.Ln(3)
	}

	// -------------------------------------------------------------------------
	// Closing text + powered-by footer
	// -------------------------------------------------------------------------
	if data.ClosingText != "" {
		f.SetFont("dejavu", "", 10)
		f.SetTextColor(55, 65, 81)
		f.MultiCell(contentW, 5, data.ClosingText, "", "L", false)
		f.Ln(4)
	}

	if data.ShowPoweredBy {
		f.SetFont("dejavu", "", 9)
		f.SetTextColor(156, 163, 175)
		f.CellFormat(contentW, 6, "Prepared with WPMgr", "", 1, "C", false, 0, "")
	}

	// Render to bytes.
	var buf bytes.Buffer
	if err := f.Output(&buf); err != nil {
		return nil, fmt.Errorf("pdf render: %w", err)
	}
	return buf.Bytes(), nil
}

// ---------------------------------------------------------------------------
// Drawing helpers — native fpdf vector primitives (no SVG path reuse)
// ---------------------------------------------------------------------------

func sectionHeader(f *fpdflib.Fpdf, accent [3]int, title string) {
	f.SetFont("dejavu", "B", 13)
	f.SetTextColor(15, 23, 42)
	f.SetFillColor(248, 249, 250)
	f.CellFormat(contentW, 8, title, "", 1, "L", true, 0, "")
	// Accent underline.
	f.SetDrawColor(accent[0], accent[1], accent[2])
	f.SetLineWidth(0.5)
	y := f.GetY()
	f.Line(marginL, y, marginL+contentW, y)
	f.SetLineWidth(0.2)
	f.Ln(3)
}

func drawTotalsGrid(f *fpdflib.Fpdf, data reportdata.ReportData) {
	// GH #414 phase 5 — the average is over the MEASURED population, so when any
	// site was paused the label says which population, and when NOTHING was
	// measured it prints an em dash rather than 0.0%, which would read as a
	// total outage.
	avgUptime := "—"
	if data.Totals.UptimeSiteCount > 0 {
		avgUptime = fmt.Sprintf("%.1f%%", data.Totals.AvgUptimePct)
	}
	avgLabel := "Avg Uptime"
	if data.Totals.PausedSiteCount > 0 {
		avgLabel = fmt.Sprintf("Avg Uptime (%d of %d)", data.Totals.UptimeSiteCount, data.Totals.SiteCount)
	}
	cells := []struct{ val, label string }{
		{fmt.Sprintf("%d", data.Totals.SiteCount), "Sites"},
		{avgUptime, avgLabel},
		{fmt.Sprintf("%d", data.Totals.Incidents), "Incidents"},
		{fmt.Sprintf("%d", data.Totals.BackupsCount), "Backups"},
		{fmt.Sprintf("%d", data.Totals.UpdatesApplied), "Updates"},
	}
	if data.Sections.Email {
		cells = append(cells, struct{ val, label string }{fmt.Sprintf("%d", data.Totals.EmailsSent), "Emails Sent"})
	}
	n := len(cells)
	cellW := contentW / float64(n)
	for _, c := range cells {
		f.SetFont("dejavu", "B", 16)
		f.SetTextColor(59, 130, 246)
		f.CellFormat(cellW, 9, c.val, "", 0, "C", false, 0, "")
	}
	f.Ln(9)
	for _, c := range cells {
		f.SetFont("dejavu", "", 9)
		f.SetTextColor(107, 114, 128)
		f.CellFormat(cellW, 5, c.label, "", 0, "C", false, 0, "")
	}
	f.Ln(6)
}

// drawPausedRow states, in the client's own PDF, that this site's monitoring is
// paused and why no uptime figures follow.
//
// GH #414 phase 5. It is the PDF twin of the .paused-note block in
// report.html.tmpl and it must keep saying the same three things: monitoring is
// paused, uptime is therefore absent rather than zero, and BACKUPS CONTINUE.
// The last clause is not filler — a client reading "monitoring paused" on a
// maintenance report will otherwise assume their backups stopped too, and that
// is the one assumption this feature must never leave standing.
//
// Grey, never red: a pause is a decision, not an incident.
func drawPausedRow(f *fpdflib.Fpdf, s reportdata.SiteReport) {
	f.SetFont("dejavu", "B", 10)
	f.SetTextColor(75, 85, 99)
	title := "Monitoring paused"
	if s.MonitoringPausedAt != nil {
		title += " since " + s.MonitoringPausedAt.Format("2 Jan 2006")
	}
	f.CellFormat(contentW, 5, title, "", 1, "L", false, 0, "")
	f.SetFont("dejavu", "", 9)
	f.SetTextColor(107, 114, 128)
	f.MultiCell(contentW, 4.5,
		"Uptime checks are not running for this site, so no uptime figures are shown for this period. Backups continue as normal.",
		"", "L", false)
	if s.MonitoringPausedReason != "" {
		f.SetFont("dejavu", "I", 9)
		f.MultiCell(contentW, 4.5, "Reason: "+s.MonitoringPausedReason, "", "L", false)
	}
	f.Ln(2)
}

func drawUptimeRow(f *fpdflib.Fpdf, u *reportdata.UptimeSection) {
	f.SetFont("dejavu", "B", 10)
	f.SetTextColor(55, 65, 81)
	f.CellFormat(contentW, 5, "Uptime", "", 1, "L", false, 0, "")
	f.SetFont("dejavu", "", 10)
	row := fmt.Sprintf("%.2f%% uptime  ·  %.0fms avg latency  ·  %d incident(s)",
		u.UptimePct, u.AvgLatencyMs, u.Incidents)
	if u.TLSExpiry != nil {
		row += fmt.Sprintf("  ·  TLS expires %s", u.TLSExpiry.Format("2 Jan 2006"))
	}
	f.CellFormat(contentW, 5, row, "", 1, "L", false, 0, "")

	// Sparkline: draw native Rect bars from daily series.
	if len(u.Daily) > 0 {
		drawSparkline(f, f.GetX(), f.GetY()+1, contentW, 8, u.Daily)
		f.Ln(10)
	} else {
		f.Ln(2)
	}

	if u.PartialCoverage {
		drawPartialCoverageNote(f, u)
	}
	if u.CoverageUnknown {
		drawCoverageUnknownNote(f)
	}
}

// drawCoverageUnknownNote is the PDF twin of the "Coverage unconfirmed"
// .paused-note block in report.html.tmpl. It is what a FAILED pause-history
// read renders as: the checks are real and are shown, but the document does
// not claim the period was monitored end to end when nothing could confirm it.
func drawCoverageUnknownNote(f *fpdflib.Fpdf) {
	f.SetFont("dejavu", "B", 9)
	f.SetTextColor(75, 85, 99)
	f.CellFormat(contentW, 4.5, "Coverage unconfirmed", "", 1, "L", false, 0, "")
	f.SetFont("dejavu", "", 9)
	f.SetTextColor(107, 114, 128)
	f.MultiCell(contentW, 4.5,
		"The monitoring pause history for this site could not be read, so we cannot confirm the prober ran for the whole period. The figures above cover every check that was recorded.",
		"", "L", false)
	f.Ln(1)
}

// drawPartialCoverageNote states, next to the uptime figures it qualifies,
// that they cover only part of the reporting period. It is the PDF twin of
// the "Partial coverage" .paused-note block in report.html.tmpl.
//
// GH #414 phase 5 follow-up. The aggregator now classifies a pause interval's
// overlap with the reporting window as none/partial/full and suppresses the
// section only on full (drawPausedRow handles that case, above). A partial
// overlap leaves real, measured data in u — it must render — but a reader
// must not mistake a month with a hole in it for a complete measurement, so
// this must sit beside the numbers, not in a footnote.
func drawPartialCoverageNote(f *fpdflib.Fpdf, u *reportdata.UptimeSection) {
	f.SetFont("dejavu", "B", 9)
	f.SetTextColor(75, 85, 99)
	f.CellFormat(contentW, 4.5, "Partial coverage", "", 1, "L", false, 0, "")
	f.SetFont("dejavu", "", 9)
	f.SetTextColor(107, 114, 128)
	f.MultiCell(contentW, 4.5,
		fmt.Sprintf("Monitoring was paused for part of this period — about %s went unmeasured, so the figures above are partial, not a full month.", humanizeHours(u.UnmonitoredHours)),
		"", "L", false)
	f.Ln(1)
}

// humanizeHours formats UnmonitoredHours the way a person would say it, not
// as a raw float. Kept identical in wording to the HTML twin's helper of the
// same name (internal/report/render/html/renderer.go) — the two renderers
// must never disagree about how much of the period went unmeasured.
func humanizeHours(hours float64) string {
	if hours < 1 {
		return "under an hour"
	}
	if hours < 24 {
		h := int(math.Round(hours))
		if h == 1 {
			return "1 hour"
		}
		return fmt.Sprintf("%d hours", h)
	}
	d := int(math.Round(hours / 24))
	if d == 1 {
		return "1 day"
	}
	return fmt.Sprintf("%d days", d)
}

// drawSparkline draws a bar chart from UptimeDay series using native fpdf Rect.
// Charts are NEVER the HTML twin's inline SVG — native primitives only.
func drawSparkline(f *fpdflib.Fpdf, x, y, w, h float64, days []reportdata.UptimeDay) {
	if len(days) == 0 {
		return
	}
	barW := w / float64(len(days)) * 0.8
	gap := w / float64(len(days)) * 0.2
	for i, d := range days {
		barH := d.UptimePct / 100 * h
		if barH < 0.5 {
			barH = 0.5
		}
		bx := x + float64(i)*(barW+gap)
		by := y + h - barH
		r, g, b := uptimeRGB(d.UptimePct)
		f.SetFillColor(r, g, b)
		f.Rect(bx, by, barW, barH, "F")
	}
}

func uptimeRGB(pct float64) (int, int, int) {
	if pct >= 99.9 {
		return 22, 163, 74 // green
	} else if pct >= 95 {
		return 202, 138, 4 // amber
	}
	return 220, 38, 38 // red
}

func drawBackupRow(f *fpdflib.Fpdf, b *reportdata.BackupSection) {
	f.SetFont("dejavu", "B", 10)
	f.SetTextColor(55, 65, 81)
	f.CellFormat(contentW, 5, "Backups", "", 1, "L", false, 0, "")
	f.SetFont("dejavu", "", 10)
	row := fmt.Sprintf("%d completed  ·  %s total", b.CompletedInPeriod, bytesHumanPDF(b.TotalBytes))
	if b.LastCompletedAt != nil {
		row += fmt.Sprintf("  ·  last %s", b.LastCompletedAt.Format("2 Jan 2006"))
	}
	f.CellFormat(contentW, 5, row, "", 1, "L", false, 0, "")
	f.Ln(2)
}

func drawUpdatesRow(f *fpdflib.Fpdf, u *reportdata.UpdateSection) {
	f.SetFont("dejavu", "B", 10)
	f.SetTextColor(55, 65, 81)
	f.CellFormat(contentW, 5, "Updates", "", 1, "L", false, 0, "")
	f.SetFont("dejavu", "", 10)
	row := fmt.Sprintf("%d applied  ·  %d plugins  ·  %d themes  ·  %d core", u.Total, u.Plugins, u.Themes, u.Core)
	if u.Failed > 0 {
		row += fmt.Sprintf("  ·  %d failed", u.Failed)
	}
	f.CellFormat(contentW, 5, row, "", 1, "L", false, 0, "")
	f.Ln(2)
}

func drawPerfRow(f *fpdflib.Fpdf, p *reportdata.PerfSection) {
	if len(p.Metrics) == 0 {
		return
	}
	f.SetFont("dejavu", "B", 10)
	f.SetTextColor(55, 65, 81)
	f.CellFormat(contentW, 5, "Core Web Vitals (p75, all devices)", "", 1, "L", false, 0, "")
	f.SetFont("dejavu", "", 10)

	// Draw rating bars using native Rect (not SVG).
	barH := 4.0
	for _, m := range p.Metrics {
		label := fmt.Sprintf("%s: %s (%s)", strings.ToUpper(m.Metric), cwvDisplayPDF(m.Metric, m.P75), m.Rating)
		f.CellFormat(40, barH+1, label, "", 0, "L", false, 0, "")
		r, g, b := ratingRGB(m.Rating)
		// Bar proportional to p75 relative to "poor" threshold.
		fillW := math.Min(contentW-50, contentW-50)
		f.SetFillColor(r, g, b)
		f.Rect(f.GetX(), f.GetY()+0.5, fillW*ratingFillPct(m.Metric, m.P75), barH, "F")
		f.Ln(barH + 2)
	}
	f.Ln(2)
}

func ratingFillPct(metric string, p75 float64) float64 {
	thresholds := map[string]float64{
		"lcp": 4000, "inp": 500, "cls": 250, "ttfb": 1800, "fcp": 3000,
	}
	max, ok := thresholds[metric]
	if !ok || max == 0 {
		return 0.5
	}
	pct := p75 / max
	if pct > 1 {
		pct = 1
	}
	return pct
}

func ratingRGB(rating string) (int, int, int) {
	switch rating {
	case "good":
		return 22, 163, 74
	case "needs_improvement":
		return 202, 138, 4
	default:
		return 220, 38, 38
	}
}

func cwvDisplayPDF(metric string, p75 float64) string {
	if metric == "cls" {
		return fmt.Sprintf("%.3f", p75/1000)
	}
	return fmt.Sprintf("%.0fms", p75)
}

func drawEmailRow(f *fpdflib.Fpdf, e *reportdata.EmailSection) {
	f.SetFont("dejavu", "B", 10)
	f.SetTextColor(55, 65, 81)
	f.CellFormat(contentW, 5, "Email", "", 1, "L", false, 0, "")
	f.SetFont("dejavu", "", 10)
	row := fmt.Sprintf("%d sent  ·  %d failed  ·  %d bounced", e.Sent, e.Failed, e.Bounced)
	f.CellFormat(contentW, 5, row, "", 1, "L", false, 0, "")
	f.Ln(2)
}

func bytesHumanPDF(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

// colorFromHex parses a 6-digit hex color (#rrggbb) and returns [r,g,b].
// Falls back to the blue accent on any parse failure.
func colorFromHex(hex string) [3]int {
	hex = strings.TrimPrefix(hex, "#")
	if len(hex) != 6 {
		return [3]int{59, 130, 246}
	}
	parse := func(s string) int {
		var v int
		for _, c := range s {
			v <<= 4
			switch {
			case c >= '0' && c <= '9':
				v += int(c - '0')
			case c >= 'a' && c <= 'f':
				v += int(c-'a') + 10
			case c >= 'A' && c <= 'F':
				v += int(c-'A') + 10
			}
		}
		return v
	}
	return [3]int{parse(hex[0:2]), parse(hex[2:4]), parse(hex[4:6])}
}
