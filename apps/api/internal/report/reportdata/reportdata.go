// Package reportdata defines the ReportData struct and all sub-types used
// as the single aggregation shape for HTML rendering, PDF rendering, and the
// JSON snapshot stored in generated_reports.data_snapshot. It lives in a
// separate package to break the import cycle between internal/report and
// internal/report/render/* (both need these types but render/* must not import
// the parent report package).
package reportdata

import (
	"time"

	"github.com/google/uuid"
)

// ReportData is the complete aggregated report for one client in one period.
// JSON tags are snake_case; this struct is the data_snapshot schema (v1).
type ReportData struct {
	SchemaVersion int       `json:"schema_version"` // 1
	GeneratedAt   time.Time `json:"generated_at"`
	PeriodStart   time.Time `json:"period_start"`
	PeriodEnd     time.Time `json:"period_end"`
	PeriodLabel   string    `json:"period_label"` // e.g. "1 May 2026 – 31 May 2026"

	// Identity / branding
	ClientID      uuid.UUID `json:"client_id"`
	ClientName    string    `json:"client_name"`
	Company       string    `json:"company,omitempty"`
	AgencyName    string    `json:"agency_name"`
	LogoURL       string    `json:"logo_url,omitempty"`
	AccentColor   string    `json:"accent_color,omitempty"`
	IntroText     string    `json:"intro_text,omitempty"`
	ClosingText   string    `json:"closing_text,omitempty"`
	ShowPoweredBy bool      `json:"show_powered_by"`

	Sections SectionFlags `json:"sections"`
	Totals   ReportTotals `json:"totals"`
	Sites    []SiteReport `json:"sites"`
}

// SectionFlags controls which sections appear in the report.
type SectionFlags struct {
	Overview    bool `json:"overview"`
	Uptime      bool `json:"uptime"`
	Backups     bool `json:"backups"`
	Updates     bool `json:"updates"`
	Performance bool `json:"performance"`
	Email       bool `json:"email"`
}

// DefaultSectionFlags returns all sections enabled.
func DefaultSectionFlags() SectionFlags {
	return SectionFlags{
		Overview:    true,
		Uptime:      true,
		Backups:     true,
		Updates:     true,
		Performance: true,
		Email:       true,
	}
}

// ReportTotals is the fleet-wide rollup across all client sites.
type ReportTotals struct {
	SiteCount    int     `json:"site_count"`
	AvgUptimePct float64 `json:"avg_uptime_pct"`
	// UptimeSiteCount is the DENOMINATOR AvgUptimePct was actually divided by:
	// the number of sites that contributed an uptime figure. It is written out
	// rather than left implicit because SiteCount is no longer that number and a
	// reader of the snapshot would otherwise have no way to check the average.
	//
	// GH #414 phase 5. The old denominator was len(sites), which made a fleet
	// percentage a lie in two independent directions. A MONITORING-PAUSED site
	// contributes no uptime section at all (see MonitoringPaused below), so
	// dividing by len(sites) would have counted it as 0% and dragged the average
	// down; and before pause existed, a site the prober had simply never reached
	// did the same thing silently. Both are the same defect — a site with no
	// observation is not a site observed at 0% — and both are fixed by dividing
	// by the population that was actually measured. The client portal's own
	// recomputation (internal/portal/handler.go) has always divided by the
	// contributing count, so this also ends a standing disagreement between the
	// report PDF and the portal summary of the same period.
	UptimeSiteCount int `json:"uptime_site_count"`
	// PausedSiteCount is how many of SiteCount had monitoring paused for the
	// period. Reported so "8 sites, average over 6" is answerable from the
	// snapshot without re-walking Sites.
	PausedSiteCount int   `json:"paused_site_count"`
	Incidents       int   `json:"incidents"`
	BackupsCount    int64 `json:"backups_count"`
	UpdatesApplied  int64 `json:"updates_applied"`
	EmailsSent      int64 `json:"emails_sent"`
	EmailsFailed    int64 `json:"emails_failed"`
}

// SiteReport is the per-site section of the report.
type SiteReport struct {
	SiteID uuid.UUID `json:"site_id"`
	Name   string    `json:"name"`
	URL    string    `json:"url"`

	// MonitoringPaused is GH #414 phase 5: the site is IN the report and NAMED
	// AS PAUSED, rather than dropped from it or shown with a month of gaps.
	//
	// Dropping it was rejected: a client who is billed for eight sites and
	// receives a report covering six has been handed a document that disagrees
	// with their invoice, and nothing in it says why. Leaving the uptime section
	// in was rejected for the reason the feature exists: the prober stops for a
	// paused site (uptime.listEnrolledForMonitoringProbeSQL), so its daily
	// series is empty for every paused day and the sparkline renders a month of
	// floor-height bars that are indistinguishable from a month of downtime.
	// Pause means "do not tell me", never "lie to me", and an unexplained
	// outage-shaped hole in a client-facing PDF is the loudest lie this feature
	// could tell.
	//
	// ONLY THE UPTIME SECTION IS SUPPRESSED. Backups keep running for a paused
	// site and keep being reported — data protection is not monitoring, and a
	// backup section that vanished on pause would understate the one thing the
	// agency is actually being paid for. Updates, performance and email are
	// likewise untouched: no phase filters their collection, so their numbers
	// are real and suppressing them would invent a gap rather than explain one.
	MonitoringPaused bool `json:"monitoring_paused,omitempty"`
	// MonitoringPausedAt is when the pause began, so the report can say "paused
	// since 3 May" instead of the bare word.
	MonitoringPausedAt *time.Time `json:"monitoring_paused_at,omitempty"`
	// MonitoringPausedReason is the operator's note, verbatim. It reaches a
	// CLIENT-FACING document, which is a deliberate call: the operator typed it
	// into a field whose label says it is shown to the client, and a reason
	// ("migrating to new host") is the whole difference between an explained
	// pause and an unexplained hole. Rendered escaped by html/template.
	MonitoringPausedReason string `json:"monitoring_paused_reason,omitempty"`

	Uptime      *UptimeSection `json:"uptime,omitempty"`
	Backups     *BackupSection `json:"backups,omitempty"`
	Updates     *UpdateSection `json:"updates,omitempty"`
	Performance *PerfSection   `json:"performance,omitempty"`
	Email       *EmailSection  `json:"email,omitempty"`
}

// UptimeSection holds uptime metrics for one site in the report period.
type UptimeSection struct {
	UptimePct    float64     `json:"uptime_pct"`
	AvgLatencyMs float64     `json:"avg_latency_ms"`
	Checks       uint64      `json:"checks"`
	DownChecks   uint64      `json:"down_checks"`
	Incidents    int         `json:"incidents"`
	TLSExpiry    *time.Time  `json:"tls_expiry,omitempty"`
	Daily        []UptimeDay `json:"daily"`

	// PartialCoverage is true when monitoring was paused for part, but not
	// all, of the reporting window (a real pause interval overlaps
	// [PeriodStart, PeriodEnd) without covering it entirely). The section is
	// still populated with real data — it is NOT suppressed — but it must
	// never be read as a complete measurement of the period: the prober was
	// off for UnmonitoredHours of it. A full-window pause suppresses the
	// section instead of setting this flag (SiteReport.Uptime is nil); a
	// pause with no overlap at all leaves this false. See the aggregator's
	// pause-interval overlap logic.
	PartialCoverage bool `json:"partial_coverage,omitempty"`
	// UnmonitoredHours is the portion of [PeriodStart, PeriodEnd) covered by
	// a monitoring pause, in hours. Only meaningful when PartialCoverage is
	// true; zero otherwise.
	UnmonitoredHours float64 `json:"unmonitored_hours,omitempty"`
	// CoverageUnknown is true when the pause history could not be read at all,
	// so whether monitoring was paused during this window is unknown. The
	// section still carries every check that was recorded — suppressing it
	// would discard a measured period — but it must not be presented as a
	// complete month either: a failed history read is not evidence that the
	// site was monitored throughout. It is the third state between
	// PartialCoverage (a pause of known size) and a plain, fully-covered
	// section, and the renderers say so in words rather than printing a
	// duration nobody computed.
	CoverageUnknown bool `json:"coverage_unknown,omitempty"`
}

// UptimeDay is one day bucket in the uptime sparkline series.
type UptimeDay struct {
	Day          time.Time `json:"day"`
	UptimePct    float64   `json:"uptime_pct"`
	AvgLatencyMs float64   `json:"avg_latency_ms"`
}

// BackupSection holds backup stats for one site in the report period.
type BackupSection struct {
	CompletedInPeriod int64      `json:"completed_in_period"`
	TotalBytes        int64      `json:"total_bytes"`
	LastCompletedAt   *time.Time `json:"last_completed_at,omitempty"`
}

// UpdateSection holds update task counts for one site in the report period.
type UpdateSection struct {
	Plugins int64 `json:"plugins"`
	Themes  int64 `json:"themes"`
	Core    int64 `json:"core"`
	Failed  int64 `json:"failed"`
	Total   int64 `json:"total"`
}

// PerfSection holds Core Web Vitals p75 for one site (all-devices aggregate).
type PerfSection struct {
	Metrics []PerfMetric `json:"metrics"`
}

// PerfMetric is one CWV metric p75 estimate.
type PerfMetric struct {
	Metric      string  `json:"metric"` // lcp|inp|cls|ttfb|fcp
	P75         float64 `json:"p75"`    // ms (CLS: milli-units, render /1000)
	Rating      string  `json:"rating"` // good|needs_improvement|poor
	SampleCount int64   `json:"sample_count"`
}

// EmailSection holds email delivery counts for one site in the report period.
type EmailSection struct {
	Total   int64 `json:"total"`
	Sent    int64 `json:"sent"`
	Failed  int64 `json:"failed"`
	Bounced int64 `json:"bounced"`
}

// ---------------------------------------------------------------------------
// CWV rating thresholds (LCP ms, INP ms, CLS milli-units, TTFB ms, FCP ms)
// ---------------------------------------------------------------------------

// CWVRating returns the Core Web Vitals rating for a given metric and p75 value
// in milli-units (CLS milli-units = actual CLS * 1000).
func CWVRating(metric string, p75Milli float64) string {
	switch metric {
	case "lcp":
		if p75Milli <= 2500 {
			return "good"
		} else if p75Milli <= 4000 {
			return "needs_improvement"
		}
		return "poor"
	case "inp":
		if p75Milli <= 200 {
			return "good"
		} else if p75Milli <= 500 {
			return "needs_improvement"
		}
		return "poor"
	case "cls":
		// Stored as milli-units (actual * 1000). Thresholds: 0.1 good, 0.25 poor.
		if p75Milli <= 100 {
			return "good"
		} else if p75Milli <= 250 {
			return "needs_improvement"
		}
		return "poor"
	case "ttfb":
		if p75Milli <= 800 {
			return "good"
		} else if p75Milli <= 1800 {
			return "needs_improvement"
		}
		return "poor"
	case "fcp":
		if p75Milli <= 1800 {
			return "good"
		} else if p75Milli <= 3000 {
			return "needs_improvement"
		}
		return "poor"
	default:
		return "good"
	}
}
