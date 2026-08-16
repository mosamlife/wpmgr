package html

import (
	"bytes"
	"os"
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
// CURRENT template and compares it byte-for-byte against
// testdata/full_coverage_pre_partial_coverage.html, a committed fixture
// captured from the template exactly as it stood at 3ac5f15 — the
// aggregator-fix commit this work builds on, before the prose change. The new
// block is gated entirely behind {{if .Uptime.PartialCoverage}}, so a
// full-coverage render must not move a single byte.
//
// The fixture is a fixed, reviewed artifact, not something this test (or any
// test) regenerates. It was produced once, offline, by executing the pre-fix
// template (via `git show 3ac5f15:.../report.html.tmpl`, which requires
// developer-machine git history CI does not have) against the same
// partialFixture(t, false) data this test builds today, then committed. A
// future change to the template's full-coverage output must edit this
// fixture deliberately, as a visible diff in review — see
// testdata/full_coverage_pre_partial_coverage.html for how to regenerate it
// if that ever legitimately needs to happen.
//
// This is safe against the fixture's own randomness: partialFixture assigns
// fresh uuid.New() values to ClientID and Sites[].SiteID on every call, but
// neither field is ever rendered into the HTML template (confirmed by
// `grep -n "ClientID\|SiteID" templates/*.tmpl`, zero matches), so the
// captured bytes do not depend on which random UUIDs were live when the
// fixture was generated.
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

	before, err := os.ReadFile("testdata/full_coverage_pre_partial_coverage.html")
	if err != nil {
		t.Fatalf("read golden fixture: %v", err)
	}

	if !bytes.Equal(before, after) {
		t.Errorf("full-coverage render changed vs testdata/full_coverage_pre_partial_coverage.html (the pre-#414 3ac5f15 template):\n--- before (golden) ---\n%s\n--- after ---\n%s", before, after)
	}
}
