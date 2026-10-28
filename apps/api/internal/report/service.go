package report

import (
	"context"
	"fmt"
	"io"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"

	// tzdata embeds the IANA timezone database so nextRunAt can call
	// time.LoadLocation on any host (mirrors email/worker.go:11-13).
	_ "time/tzdata"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// Enqueuer schedules report generation jobs (River) and says whether a report
// still has one. Declared as an interface so the service is unit-testable with a
// fake (mirrors backup.Enqueuer pattern). Wired after the River client starts
// via SetEnqueuer.
type Enqueuer interface {
	EnqueueGenerate(ctx context.Context, args GenerateArgs) error
	// HasActiveGenerateJob reports whether a generate job for the report is still
	// queued, waiting for a retry or running: a job that can still complete or
	// fail the report's row. It says nothing about the row itself.
	HasActiveGenerateJob(ctx context.Context, tenantID, reportID uuid.UUID) (bool, error)
}

// MailerEnabled is the subset of mailer.Service needed to check whether the
// instance mailer is configured. Injected from main.go to avoid a direct
// import cycle (mirrors email/notify.go:297-300).
type MailerEnabled interface {
	Enabled(ctx context.Context) bool
}

// MailerEnqueuer enqueues durable transactional emails. The concrete type is
// *mailer.Enqueuer. Optional: when nil, email delivery is silently skipped.
type MailerEnqueuer interface {
	Enqueue(ctx context.Context, tenantID uuid.UUID, recipients []string, template string, data map[string]any) error
}

// BlobStorer is the subset of blobstore.Store used by the report service.
type BlobStorer interface {
	Put(ctx context.Context, key string, body io.Reader, size int64) error
	Delete(ctx context.Context, key string) error
	PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error)
}

const (
	maxRecipients      = 10
	maxIntroClosingLen = 2000
	presignTTL         = 7 * 24 * time.Hour // 604800s = SigV4 max
	maxPeriodDays      = 92
)

// Service holds the report business logic.
type Service struct {
	repo         Repo
	mailerStatus MailerEnabled
	mailer       MailerEnqueuer
	blob         BlobStorer
	publisher    EventPublisher
	enqueuer     Enqueuer
	aggBuilder   func(ctx context.Context, in BuildInput) (ReportData, error)
}

// NewService builds a report Service.
func NewService(repo Repo, blob BlobStorer) *Service {
	return &Service{repo: repo, blob: blob}
}

// SetEnqueuer wires the River enqueuer after the River client is started
// (resolving the client ← enqueuer ← service ← worker construction cycle,
// mirroring backup.Service.SetEnqueuer).
func (s *Service) SetEnqueuer(e Enqueuer) { s.enqueuer = e }

// SetMailer injects the mailer enqueuer (called after River starts, like backup).
func (s *Service) SetMailer(e MailerEnqueuer) { s.mailer = e }

// SetMailerStatus injects the mailer-configured check (from main.go).
func (s *Service) SetMailerStatus(ms MailerEnabled) { s.mailerStatus = ms }

// SetPublisher injects the SSE event publisher.
func (s *Service) SetPublisher(p EventPublisher) { s.publisher = p }

// SetAggregatorBuilder injects the aggregator build function (set after wiring).
func (s *Service) SetAggregatorBuilder(fn func(ctx context.Context, in BuildInput) (ReportData, error)) {
	s.aggBuilder = fn
}

// ---------------------------------------------------------------------------
// Schedule
// ---------------------------------------------------------------------------

// GetSchedule returns the schedule for a client, or defaults if none exists.
// NEVER returns a 404 — mirrors the email notify-settings pattern.
func (s *Service) GetSchedule(ctx context.Context, tenantID, clientID uuid.UUID) (Schedule, bool, error) {
	sched, found, err := s.repo.GetSchedule(ctx, tenantID, clientID)
	if err != nil {
		return Schedule{}, false, err
	}
	if !found {
		// Return defaults without creating a row.
		sched = defaultSchedule(clientID, tenantID)
	}
	tz, err := s.repo.GetClientTimezone(ctx, tenantID, clientID)
	if err != nil {
		return Schedule{}, false, err
	}
	sched.Timezone = tz
	return sched, found, nil
}

// UpsertSchedule validates and upserts a report schedule.
func (s *Service) UpsertSchedule(ctx context.Context, in UpsertScheduleInput) (Schedule, error) {
	cleaned, err := validateScheduleInput(in)
	if err != nil {
		return Schedule{}, err
	}
	// FIX-3: use the cleaned (deduped, canonicalized) recipients, not the raw input.
	in.Recipients = cleaned
	// The send hour is in the client's timezone, the one the scanner computes
	// every later run in, so the first run is computed in it too.
	tz, err := s.repo.GetClientTimezone(ctx, in.TenantID, in.ClientID)
	if err != nil {
		return Schedule{}, err
	}
	if in.Enabled {
		in.NextRunAt = nextRunAt(in.Cadence, in.SendDay, in.SendHour, tz)
	} else {
		in.NextRunAt = nil
	}
	// GH #972: a section the request did not send keeps its stored value. The
	// settings form has no Overview control, so replacing the stored flags
	// with the request's turned Overview off on every save.
	//
	// The stored sections are read, merged over and written back in one
	// transaction, one save of the client at a time. Read apart from the write,
	// two saves that each change a different section undo each other.
	saved, err := s.repo.UpsertSchedule(ctx, in.TenantID, in.ClientID, func(stored Schedule, found bool) UpsertScheduleInput {
		base := DefaultSectionFlags()
		if found {
			base = stored.Sections
		}
		in.Sections = in.SectionsPatch.Apply(base)
		return in
	})
	if err != nil {
		return Schedule{}, err
	}
	saved.Timezone = tz
	return saved, nil
}

// ---------------------------------------------------------------------------
// On-demand generation
// ---------------------------------------------------------------------------

// GenerateNow creates a pending report row, enqueues the generate job, and
// returns 202 + the pending report. Returns domain code
// "object_storage_required" when the blob store is nil (503).
//
// Abuse guard: if a pending or generating report already exists for this client
// the existing report is returned (same 202 body) rather than creating a
// duplicate job. This prevents queue flooding from repeated calls. A report
// untouched for staleReportAfter is replaced only when no job for it is still
// queued or running; one whose job is only waiting in the queue is returned like
// any other report in flight.
//
// If the River enqueue fails after row creation, the row is marked failed so
// it doesn't remain in a forever-pending state.
func (s *Service) GenerateNow(ctx context.Context, tenantID, clientID uuid.UUID, in GenerateNowInput) (GeneratedReport, error) {
	if s.blob == nil {
		return GeneratedReport{}, domain.ServiceUnavailable("object_storage_required", "object storage must be configured to generate reports")
	}

	now := time.Now().UTC()
	end := in.PeriodEnd
	if end.IsZero() || end.After(now) {
		end = now
	}
	start := in.PeriodStart
	if start.IsZero() {
		start = end.AddDate(0, 0, -30)
	}
	if !start.Before(end) {
		return GeneratedReport{}, domain.Validation("invalid_period", "period_start must be before period_end")
	}
	if end.Sub(start) > time.Duration(maxPeriodDays)*24*time.Hour {
		return GeneratedReport{}, domain.Validation("period_too_long", fmt.Sprintf("report period may not exceed %d days", maxPeriodDays))
	}

	// FIX-4 abuse guard: return the existing in-flight report rather than
	// creating a new one. This bounds per-client queue depth to 1 on-demand job.
	//
	// A row nothing has touched for staleReportAfter may belong to a job that
	// died without finishing it. Then it is marked failed and the request goes
	// through; returning it would refuse "Generate now" for this client for ever.
	//
	// Untouched is not dead. A job still waiting in a busy queue has touched
	// nothing either, and failing the row of a scheduled report leaves the client
	// without that period's email, because the replacement does not notify. So
	// the row is failed only when no job for it is queued or running, and a
	// report that still has one is returned like any other in flight.
	existing, found, err := s.repo.GetActiveReport(ctx, tenantID, clientID)
	if err == nil && found {
		if !reportIsStale(existing, now) {
			return existing, nil
		}
		live, liveErr := s.reportHasLiveJob(ctx, tenantID, existing.ID)
		if liveErr != nil {
			// Not knowing is not the same as the job being gone: the row is left as
			// it is and the request fails, so a retry can decide.
			return GeneratedReport{}, domain.Internal("report_job_check_failed", "failed to check whether the report's job is still queued").WithCause(liveErr)
		}
		if live {
			return existing, nil
		}
		_, _ = s.repo.FailReport(ctx, tenantID, existing.ID, "report generation did not finish")
	}

	rpt, err := s.repo.CreateReport(ctx, CreateReportInput{
		TenantID:    tenantID,
		ClientID:    clientID,
		PeriodStart: start,
		PeriodEnd:   end,
	})
	if err != nil {
		return GeneratedReport{}, err
	}

	// FIX-4: enqueue the River generate job. On failure, mark the row failed
	// immediately so it doesn't remain forever-pending.
	if s.enqueuer != nil {
		if enqErr := s.enqueuer.EnqueueGenerate(ctx, GenerateArgs{
			TenantID:         tenantID,
			ClientID:         clientID,
			ReportID:         rpt.ID,
			PeriodStart:      start,
			PeriodEnd:        end,
			Notify:           in.Notify,
			SectionsOverride: in.Sections,
		}); enqErr != nil {
			// Best-effort fail the row so callers can see it errored.
			_, _ = s.repo.FailReport(ctx, tenantID, rpt.ID, "enqueue failed: "+enqErr.Error())
			return GeneratedReport{}, domain.Internal("report_enqueue_failed", "failed to enqueue report generation job").WithCause(enqErr)
		}
	}
	return rpt, nil
}

// reportIsStale reports whether an in-flight report row has gone untouched for
// staleReportAfter, which makes its job suspect: it died, or it is still waiting
// in the queue. reportHasLiveJob tells the two apart. A row without an update
// time proves nothing and is not stale.
func reportIsStale(r GeneratedReport, now time.Time) bool {
	return !r.UpdatedAt.IsZero() && now.Sub(r.UpdatedAt) > staleReportAfter
}

// reportHasLiveJob reports whether a generate job for the report is still queued
// or running. A service with no queue wired has queued nothing, so nothing is
// waiting.
func (s *Service) reportHasLiveJob(ctx context.Context, tenantID, reportID uuid.UUID) (bool, error) {
	if s.enqueuer == nil {
		return false, nil
	}
	return s.enqueuer.HasActiveGenerateJob(ctx, tenantID, reportID)
}

// ---------------------------------------------------------------------------
// Report detail + delete
// ---------------------------------------------------------------------------

// GetReportWithURLs returns a report and, when completed, mints presigned URLs.
func (s *Service) GetReportWithURLs(ctx context.Context, tenantID, clientID, reportID uuid.UUID) (GeneratedReport, string, string, error) {
	rpt, err := s.repo.GetReport(ctx, tenantID, clientID, reportID)
	if err != nil {
		return GeneratedReport{}, "", "", err
	}
	htmlURL, pdfURL := s.PresignReportURLs(ctx, rpt)
	return rpt, htmlURL, pdfURL, nil
}

// PresignReportURLs mints presigned download URLs for a completed report.
// Returns empty strings when the report is not completed or object storage is
// not configured. Presigning is local SigV4 computation (no round trip to the
// storage backend), so calling this per list item is cheap.
func (s *Service) PresignReportURLs(ctx context.Context, rpt GeneratedReport) (string, string) {
	if rpt.Status != StatusCompleted || s.blob == nil {
		return "", ""
	}
	var htmlURL, pdfURL string
	if rpt.HTMLBlobKey != "" {
		htmlURL, _ = s.blob.PresignGet(ctx, rpt.HTMLBlobKey, presignTTL)
	}
	if rpt.PDFBlobKey != "" {
		pdfURL, _ = s.blob.PresignGet(ctx, rpt.PDFBlobKey, presignTTL)
	}
	return htmlURL, pdfURL
}

// DeleteReport deletes the DB row and best-effort deletes blob objects.
func (s *Service) DeleteReport(ctx context.Context, tenantID, clientID, reportID uuid.UUID) error {
	rpt, err := s.repo.GetReport(ctx, tenantID, clientID, reportID)
	if err != nil {
		return err
	}
	if err := s.repo.DeleteReport(ctx, tenantID, clientID, reportID); err != nil {
		return err
	}
	// Best-effort blob cleanup — failures are logged but do not fail the call.
	if s.blob != nil {
		if rpt.HTMLBlobKey != "" {
			_ = s.blob.Delete(ctx, rpt.HTMLBlobKey)
		}
		if rpt.PDFBlobKey != "" {
			_ = s.blob.Delete(ctx, rpt.PDFBlobKey)
		}
	}
	return nil
}

// ListReports returns a paginated list of generated reports.
func (s *Service) ListReports(ctx context.Context, in ListReportsInput) (ListReportsResult, error) {
	if in.Limit <= 0 || in.Limit > 100 {
		in.Limit = 20
	}
	return s.repo.ListReports(ctx, in)
}

// MailerConfigured reports whether the instance mailer is ready to send.
func (s *Service) MailerConfigured(ctx context.Context) bool {
	if s.mailerStatus == nil {
		return false
	}
	return s.mailerStatus.Enabled(ctx)
}

// ---------------------------------------------------------------------------
// nextRunAt — ported verbatim from email/notify.go:257-295
// ---------------------------------------------------------------------------

// nextRunAt computes the next report send time. UTC fallback on bad timezone.
func nextRunAt(cadence string, day, hour int, tz string) *time.Time {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		loc = time.UTC
	}
	now := time.Now().In(loc)

	var next time.Time
	switch cadence {
	case "monthly":
		d := day
		if d < 1 {
			d = 1
		}
		if d > 28 {
			d = 28
		}
		candidate := time.Date(now.Year(), now.Month(), d, hour, 0, 0, 0, loc)
		if !candidate.After(now) {
			candidate = time.Date(now.Year(), now.Month()+1, d, hour, 0, 0, 0, loc)
		}
		next = candidate
	default: // weekly
		targetWD := time.Weekday(day % 7)
		daysUntil := int(targetWD) - int(now.Weekday())
		if daysUntil < 0 {
			daysUntil += 7
		}
		candidate := time.Date(now.Year(), now.Month(), now.Day()+daysUntil, hour, 0, 0, 0, loc)
		if !candidate.After(now) {
			candidate = candidate.AddDate(0, 0, 7)
		}
		next = candidate
	}
	t := next.UTC()
	return &t
}

// ---------------------------------------------------------------------------
// Validation helpers
// ---------------------------------------------------------------------------

// validateScheduleInput validates all schedule fields and returns the cleaned
// (deduped, canonicalized) recipient slice. The caller MUST use the returned
// slice; assigning to the local copy here would silently discard the cleaning.
func validateScheduleInput(in UpsertScheduleInput) ([]string, error) {
	// Validate cadence.
	if in.Cadence != "weekly" && in.Cadence != "monthly" {
		return nil, domain.Validation("invalid_cadence", "cadence must be 'weekly' or 'monthly'")
	}
	// Validate send_day range per cadence.
	if in.Cadence == "weekly" {
		if in.SendDay < 0 || in.SendDay > 6 {
			return nil, domain.Validation("invalid_send_day", "send_day must be 0-6 for weekly cadence (0=Sunday)")
		}
	} else {
		if in.SendDay < 1 || in.SendDay > 28 {
			return nil, domain.Validation("invalid_send_day", "send_day must be 1-28 for monthly cadence")
		}
	}
	if in.SendHour < 0 || in.SendHour > 23 {
		return nil, domain.Validation("invalid_send_hour", "send_hour must be 0-23")
	}
	// Characters, not bytes: the limit is the spec's maxLength and the message
	// below says characters, and a byte count would turn away text in any
	// script that takes more than one byte a character.
	if utf8.RuneCountInString(in.IntroText) > maxIntroClosingLen {
		return nil, domain.Validation("intro_text_too_long", fmt.Sprintf("intro_text must be %d characters or fewer", maxIntroClosingLen))
	}
	if utf8.RuneCountInString(in.ClosingText) > maxIntroClosingLen {
		return nil, domain.Validation("closing_text_too_long", fmt.Sprintf("closing_text must be %d characters or fewer", maxIntroClosingLen))
	}
	// Validate and de-duplicate recipients; return cleaned slice to the caller.
	cleaned, err := validateRecipients(in.Recipients)
	if err != nil {
		return nil, err
	}
	return cleaned, nil
}

// validateRecipients parses, validates, case-insensitive-dedupes, and caps the
// list. Returns the cleaned slice.
func validateRecipients(raw []string) ([]string, error) {
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		addr, err := mail.ParseAddress(r)
		if err != nil {
			return nil, domain.Validation("invalid_recipient", fmt.Sprintf("%q is not a valid email address", r))
		}
		key := strings.ToLower(addr.Address)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		// Persist the canonical (lowercased) bare address for consistency.
		out = append(out, key)
	}
	if len(out) > maxRecipients {
		return nil, domain.Validation("too_many_recipients", fmt.Sprintf("recipients may not exceed %d addresses", maxRecipients))
	}
	return out, nil
}

// validateTimezone returns nil if the IANA timezone string is valid or empty.
func validateTimezone(tz string) error {
	if tz == "" || tz == "UTC" {
		return nil
	}
	if _, err := time.LoadLocation(tz); err != nil {
		return domain.Validation("invalid_timezone", fmt.Sprintf("%q is not a valid IANA timezone", tz))
	}
	return nil
}

// defaultSchedule returns the read-only defaults used when no row exists.
// ALWAYS 200, mirrors email handler's defaultNotifySettings pattern.
func defaultSchedule(clientID, tenantID uuid.UUID) Schedule {
	return Schedule{
		ClientID:   clientID,
		TenantID:   tenantID,
		Cadence:    "monthly",
		SendDay:    1,
		SendHour:   8,
		Recipients: []string{},
		Sections:   DefaultSectionFlags(),
	}
}

// GenerateNowInput is the parsed+validated body for POST /reports.
type GenerateNowInput struct {
	PeriodStart time.Time
	PeriodEnd   time.Time
	// Sections is the request's optional override of the client's stored
	// report sections (GH #971); nil uses the stored sections unchanged.
	Sections *SectionFlagsPatch
	Notify   bool
}
