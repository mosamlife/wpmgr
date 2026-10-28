// Package report implements the white-label client-report domain (m64):
// schedule management, on-demand generation, HTML + PDF rendering, and email
// delivery. Every generated report is stored in object storage and its
// aggregated numbers are snapshotted into generated_reports.data_snapshot.
package report

import (
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/report/reportdata"
)

// ErrNotFound is returned when a schedule or report row does not exist, or
// when a ClaimAdvanceSchedule races and the row was already claimed.
var ErrNotFound = errors.New("report: not found")

// Report status constants mirror the generated_reports.status CHECK constraint.
const (
	StatusPending    = "pending"
	StatusGenerating = "generating"
	StatusCompleted  = "completed"
	StatusFailed     = "failed"
)

// ---------------------------------------------------------------------------
// Re-export render types from reportdata so callers of this package can use
// report.ReportData, report.SectionFlags, etc. without a secondary import.
// ---------------------------------------------------------------------------

// ReportData is the complete aggregated report for one client in one period.
type ReportData = reportdata.ReportData

// SectionFlags controls which sections appear in the report.
type SectionFlags = reportdata.SectionFlags

// DefaultSectionFlags returns all sections enabled.
var DefaultSectionFlags = reportdata.DefaultSectionFlags

// ReportTotals is the fleet-wide rollup across all client sites.
type ReportTotals = reportdata.ReportTotals

// SiteReport is the per-site section of the report.
type SiteReport = reportdata.SiteReport

// UptimeSection holds uptime metrics for one site in the report period.
type UptimeSection = reportdata.UptimeSection

// UptimeDay is one day bucket in the uptime sparkline series.
type UptimeDay = reportdata.UptimeDay

// BackupSection holds backup stats for one site in the report period.
type BackupSection = reportdata.BackupSection

// UpdateSection holds update task counts for one site in the report period.
type UpdateSection = reportdata.UpdateSection

// UpdateItem is one row of the Updates section's version table.
type UpdateItem = reportdata.UpdateItem

// PerfSection holds Core Web Vitals p75 for one site (all-devices aggregate).
type PerfSection = reportdata.PerfSection

// PerfMetric is one CWV metric p75 estimate.
type PerfMetric = reportdata.PerfMetric

// EmailSection holds email delivery counts for one site in the report period.
type EmailSection = reportdata.EmailSection

// CWVRating returns the Core Web Vitals rating for a given metric and p75 value.
var CWVRating = reportdata.CWVRating

// ---------------------------------------------------------------------------
// Domain types (schedule + report rows — NOT re-exported from reportdata)
// ---------------------------------------------------------------------------

// Schedule is the domain model for one report_schedules row.
type Schedule struct {
	ID               uuid.UUID
	TenantID         uuid.UUID
	ClientID         uuid.UUID
	Enabled          bool
	Cadence          string // "weekly" | "monthly"
	SendDay          int    // weekly: 0=Sunday..6=Saturday; monthly: 1-28
	SendHour         int    // 0-23
	Recipients       []string
	Sections         SectionFlags
	IntroText        string
	ClosingText      string
	PoweredByRemoved bool
	NextRunAt        *time.Time
	LastRunAt        *time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
	// Timezone is the IANA timezone on the client's record, which SendHour is
	// in. It is read from the client when a schedule is returned, not stored
	// on the schedule.
	Timezone string
}

// GeneratedReport is the domain model for one generated_reports row.
type GeneratedReport struct {
	ID          uuid.UUID
	TenantID    uuid.UUID
	ClientID    uuid.UUID
	ScheduleID  *uuid.UUID
	PeriodStart time.Time
	PeriodEnd   time.Time
	Status      string
	HTMLBlobKey string
	PDFBlobKey  string
	Error       string
	CreatedAt   time.Time
	// UpdatedAt is when anything last touched the row. A job touches it each
	// time an attempt starts. Zero when the source did not carry it.
	UpdatedAt   time.Time
	CompletedAt *time.Time
}

// DueSchedule is the view returned by ListDueSchedules, which JOINs the
// client row to provide timezone and notification metadata.
type DueSchedule struct {
	Schedule
	ClientName         string
	ClientContactEmail *string
	ClientTimezone     string
}

// ---------------------------------------------------------------------------
// Input types
// ---------------------------------------------------------------------------

// SectionFlagsPatch is a partial SectionFlags as a request sends it: a nil
// field was not sent and keeps the value it is applied over, an explicit false
// turns that section off (GH #972).
type SectionFlagsPatch struct {
	Overview    *bool `json:"overview"`
	Uptime      *bool `json:"uptime"`
	Backups     *bool `json:"backups"`
	Updates     *bool `json:"updates"`
	Performance *bool `json:"performance"`
	Email       *bool `json:"email"`
}

// Apply returns base with every field the patch carries replaced.
func (p SectionFlagsPatch) Apply(base SectionFlags) SectionFlags {
	set := func(dst *bool, v *bool) {
		if v != nil {
			*dst = *v
		}
	}
	out := base
	set(&out.Overview, p.Overview)
	set(&out.Uptime, p.Uptime)
	set(&out.Backups, p.Backups)
	set(&out.Updates, p.Updates)
	set(&out.Performance, p.Performance)
	set(&out.Email, p.Email)
	return out
}

// UpsertScheduleInput is the validated input for PUT /report-schedule.
type UpsertScheduleInput struct {
	TenantID   uuid.UUID
	ClientID   uuid.UUID
	Enabled    bool
	Cadence    string
	SendDay    int
	SendHour   int
	Recipients []string
	// SectionsPatch is the sections the request sent. The service applies it
	// over the stored sections (or the defaults when there is no row yet), so
	// a section the request left out keeps its value.
	SectionsPatch SectionFlagsPatch
	// Sections is the resolved value written to the row. It is computed by
	// the service from SectionsPatch; callers do not supply it directly.
	Sections         SectionFlags
	IntroText        string
	ClosingText      string
	PoweredByRemoved bool
	// NextRunAt is computed by the service; callers do not supply it directly.
	NextRunAt *time.Time
}

// CreateReportInput is the input for creating a new pending report row.
type CreateReportInput struct {
	TenantID    uuid.UUID
	ClientID    uuid.UUID
	ScheduleID  *uuid.UUID
	PeriodStart time.Time
	PeriodEnd   time.Time
}

// ListReportsInput is the keyset-cursor pagination input.
type ListReportsInput struct {
	TenantID        uuid.UUID
	ClientID        uuid.UUID
	CursorCreatedAt *time.Time // nil = first page
	CursorID        *uuid.UUID
	Limit           int32
}

// ListReportsResult is the paged list output.
type ListReportsResult struct {
	Items      []GeneratedReport
	NextCursor string // empty = no further pages
}
