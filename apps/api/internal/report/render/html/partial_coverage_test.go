package html

import (
	"bytes"
	"html/template"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/report/reportdata"
)

// GH #414 aggregator follow-up. reportdata.UptimeSection now carries
// PartialCoverage and UnmonitoredHours (see reportdata.go), computed from real
// pause/window overlap, but as reported by the agent that built it: "renderer
// prose ... doesn't yet say anything about partial coverage — the data
// contract carries it, but nothing prints it." A partially-covered month
// rendered identically to a fully-covered one, which is the exact defect the
// aggregator fix existed to remove, just moved one layer out. These tests
// pin that the HTML renderer now states it, in prose, next to the numbers it
// qualifies, and that a fully-covered period is untouched.

func partialFixture(t *testing.T, partial bool) reportdata.ReportData {
	t.Helper()
	u := &reportdata.UptimeSection{
		UptimePct:    99.2,
		AvgLatencyMs: 210,
		Checks:       1000,
		DownChecks:   8,
		Incidents:    2,
	}
	if partial {
		u.PartialCoverage = true
		u.UnmonitoredHours = 504 // 21 days, same figure as the aggregator's own
		// TestPauseResumedMidWindowIsPartialCoverage fixture (pauseEnd.Sub(pauseStart)
		// for a pause running 2026-05-05..2026-05-26).
	}
	return reportdata.ReportData{
		SchemaVersion: 1,
		GeneratedAt:   time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		PeriodStart:   time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
		PeriodEnd:     time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		PeriodLabel:   "1 May 2026 – 31 May 2026",
		ClientID:      uuid.New(),
		ClientName:    "Acme",
		AgencyName:    "Agency",
		ShowPoweredBy: true,
		Sections:      reportdata.DefaultSectionFlags(),
		Totals: reportdata.ReportTotals{
			SiteCount: 1, AvgUptimePct: 99.2, UptimeSiteCount: 1,
		},
		Sites: []reportdata.SiteReport{
			{
				SiteID: uuid.New(),
				Name:   "site-a",
				URL:    "https://a.example",
				Uptime: u,
			},
		},
	}
}

// TestHTMLStatesPartialCoverage is the headline assertion: the rendered
// document says, near the uptime figures, that monitoring was paused for
// part of the period and how much went unmeasured, in words a person would
// use ("21 days"), not the raw float (504).
func TestHTMLStatesPartialCoverage(t *testing.T) {
	r, err := NewRenderer()
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}
	out, err := r.Render(partialFixture(t, true), nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	body := string(out)
	if !strings.Contains(body, "Partial coverage") {
		t.Error("rendered report never says coverage is partial")
	}
	if !strings.Contains(body, "21 days") {
		t.Errorf("rendered report does not humanize UnmonitoredHours (504) as \"21 days\":\n%s", body)
	}
	if strings.Contains(body, "504") {
		t.Error("rendered report prints the raw UnmonitoredHours float instead of a human duration")
	}
}

// TestHTMLFullCoverageUnchanged is the negative and the byte-identity proof
// together. It renders a fully-covered site (PartialCoverage=false) with the
// CURRENT template and separately with the template exactly as it stood at
// 3ac5f15 — the aggregator-fix commit this work builds on, before the prose
// change — and requires byte-for-byte identical output. The new block is
// gated entirely behind {{if .Uptime.PartialCoverage}}, so a full-coverage
// render must not move a single byte.
func TestHTMLFullCoverageUnchanged(t *testing.T) {
	data := partialFixture(t, false)

	r, err := NewRenderer()
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}
	after, err := r.Render(data, nil)
	if err != nil {
		t.Fatalf("Render (current): %v", err)
	}
	if strings.Contains(string(after), "Partial coverage") {
		t.Error("full-coverage render mentions partial coverage")
	}

	oldSrc, err := exec.Command("git", "show", "3ac5f15:apps/api/internal/report/render/html/templates/report.html.tmpl").Output()
	if err != nil {
		t.Fatalf("git show 3ac5f15 template: %v", err)
	}
	oldTmpl, err := template.New("report.html.tmpl").Funcs(funcMap()).Parse(string(oldSrc))
	if err != nil {
		t.Fatalf("parse pre-fix template: %v", err)
	}
	td := renderData{ReportData: data}
	td.ReportData.LogoURL = ""
	var buf bytes.Buffer
	if err := oldTmpl.Execute(&buf, td); err != nil {
		t.Fatalf("execute pre-fix template: %v", err)
	}
	before := buf.Bytes()

	if !bytes.Equal(before, after) {
		t.Errorf("full-coverage render changed vs 3ac5f15:\n--- before (3ac5f15) ---\n%s\n--- after ---\n%s", before, after)
	}
}
