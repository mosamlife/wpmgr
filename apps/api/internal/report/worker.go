package report

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"time"

	// tzdata embeds the full IANA timezone database (mirrors email/worker.go:13).
	_ "time/tzdata"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/mosamlife/wpmgr/apps/api/internal/httpclient"
	"github.com/mosamlife/wpmgr/apps/api/internal/report/render/html"
	"github.com/mosamlife/wpmgr/apps/api/internal/report/render/pdf"
)

// ---------------------------------------------------------------------------
// GenerateWorker — renders one report (pending → completed/failed)
// ---------------------------------------------------------------------------

// GenerateArgs is the River job payload for a single report generation.
type GenerateArgs struct {
	TenantID    uuid.UUID  `json:"tenant_id"`
	ClientID    uuid.UUID  `json:"client_id"`
	ReportID    uuid.UUID  `json:"report_id"`
	ScheduleID  *uuid.UUID `json:"schedule_id,omitempty"`
	PeriodStart time.Time  `json:"period_start"`
	PeriodEnd   time.Time  `json:"period_end"`
	Notify      bool       `json:"notify"`
	// SectionsOverride is an on-demand request's override of the client's
	// stored sections (GH #971). Nil, which every scheduled run and the
	// dashboard's "Generate now" send, uses the stored sections unchanged.
	SectionsOverride *SectionFlagsPatch `json:"sections_override,omitempty"`
}

// Kind implements river.JobArgs.
func (GenerateArgs) Kind() string { return "report_generate" }

const (
	// generateJobTimeout is how long one attempt of a report job may run.
	generateJobTimeout = 2 * time.Minute
	// generateMaxAttempts is how many times River runs a report job.
	generateMaxAttempts = 3
	// staleReportAfter is how long a pending or generating report row may go
	// untouched before its job is suspected dead: every attempt has had its
	// whole timeout. Each attempt touches the row when it starts, so a job that
	// has been running is never idle for this long; a job still waiting in the
	// queue is, and GenerateNow asks the queue before it fails the row.
	staleReportAfter = generateJobTimeout * generateMaxAttempts
	// failReportTimeout bounds the update that marks a report failed, which
	// runs after the attempt's own context may be done.
	failReportTimeout = 15 * time.Second
)

// InsertOpts sets MaxAttempts=generateMaxAttempts on the default queue.
func (GenerateArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: generateMaxAttempts}
}

// GenerateWorker renders a single report to HTML + PDF, stores blobs, and
// optionally sends a notification email.
type GenerateWorker struct {
	river.WorkerDefaults[GenerateArgs]
	repo         Repo
	svc          *Service
	sources      Sources
	htmlRenderer *html.Renderer
	pdfRenderer  pdf.Renderer
	// ssrfClient is the SSRF-hardened HTTP client used to fetch logo images.
	// When nil, logo fetching is skipped and reports render without a logo.
	ssrfClient *httpclient.Client
	logger     *slog.Logger
}

// Timeout is the per-attempt River worker timeout.
func (w *GenerateWorker) Timeout(_ *river.Job[GenerateArgs]) time.Duration {
	return generateJobTimeout
}

// NewGenerateWorker constructs the generate worker.
func NewGenerateWorker(
	repo Repo,
	svc *Service,
	sources Sources,
	htmlRenderer *html.Renderer,
	pdfRenderer pdf.Renderer,
	ssrfClient *httpclient.Client,
	logger *slog.Logger,
) *GenerateWorker {
	if logger == nil {
		logger = slog.Default()
	}
	return &GenerateWorker{
		repo:         repo,
		svc:          svc,
		sources:      sources,
		htmlRenderer: htmlRenderer,
		pdfRenderer:  pdfRenderer,
		ssrfClient:   ssrfClient,
		logger:       logger,
	}
}

// Work generates the report. Whatever stops the final attempt from completing
// the report, an error or a panic, it ends with the row marked failed: a row
// left 'generating' would make GenerateNow refuse every later request for the
// client. An earlier attempt that fails or panics just returns the error, and
// River runs the next attempt.
func (w *GenerateWorker) Work(ctx context.Context, job *river.Job[GenerateArgs]) (err error) {
	args := job.Args
	isFinal := job.Attempt >= job.MaxAttempts

	defer func() {
		if p := recover(); p != nil {
			w.logger.Error("report generate: panic",
				slog.String("report_id", args.ReportID.String()),
				slog.Any("panic", p),
				slog.String("stack", string(debug.Stack())))
			err = fmt.Errorf("report generate: panic: %v", p)
		}
		if err != nil && isFinal {
			w.failReport(ctx, args, err)
		}
	}()

	return w.generate(ctx, args)
}

// failReport marks the report failed and tells the dashboard. It runs when an
// attempt's own context may already be done (the job timed out), so it works
// on a context of its own: with the finished one the update would fail and the
// row would stay 'generating'.
func (w *GenerateWorker) failReport(ctx context.Context, args GenerateArgs, cause error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), failReportTimeout)
	defer cancel()
	if _, err := w.repo.FailReport(ctx, args.TenantID, args.ReportID, cause.Error()); err != nil {
		w.logger.Error("report generate: mark report failed failed",
			slog.String("report_id", args.ReportID.String()),
			slog.Any("error", err))
	}
	publishReportCompleted(ctx, w.svc.publisher, args.TenantID, args.ClientID, args.ReportID, StatusFailed)
}

// generate runs one attempt. Work decides what a failure does to the row.
func (w *GenerateWorker) generate(ctx context.Context, args GenerateArgs) error {
	// A job can start after its row was ended by something else: failed and
	// replaced by a new report, or finished by an earlier attempt of the same
	// job. It must not bring that row back. Whatever is not waiting for a job or being run by one
	// has nothing to do, and the job ends clean so River neither retries it nor
	// marks the report failed. A row that cannot be read fails the attempt like
	// any other error.
	row, err := w.repo.GetReport(ctx, args.TenantID, args.ClientID, args.ReportID)
	if err != nil {
		w.logger.Error("report generate: read report failed",
			slog.String("report_id", args.ReportID.String()),
			slog.Any("error", err))
		return err
	}
	if row.Status != StatusPending && row.Status != StatusGenerating {
		w.logger.Info("report generate: report is no longer in flight, nothing to do",
			slog.String("report_id", args.ReportID.String()),
			slog.String("client_id", args.ClientID.String()),
			slog.String("status", row.Status))
		return nil
	}

	// Mark as generating.
	if _, err := w.repo.MarkGenerating(ctx, args.TenantID, args.ReportID); err != nil {
		w.logger.Error("report generate: mark generating failed",
			slog.String("report_id", args.ReportID.String()),
			slog.Any("error", err))
		return err
	}

	// The client's report settings: sections, intro and closing text, and the
	// "Prepared with WPMgr" switch. Read for EVERY run, scheduled or on demand
	// (GH #971), and a failed read fails the attempt rather than falling back
	// to the defaults, which would print the branding the agency removed.
	sched, err := reportSettingsFor(ctx, w.repo, args)
	if err != nil {
		w.logger.Error("report generate: read report settings failed",
			slog.String("report_id", args.ReportID.String()),
			slog.Any("error", err))
		return err
	}

	// Get client info + agency name.
	agencyName, _ := w.repo.GetTenantName(ctx, args.TenantID)
	// FIX-1: fetch client row under InAgentTx to populate branding fields.
	clientInfo, err := w.repo.GetClientInfo(ctx, args.TenantID, args.ClientID)
	if err != nil {
		w.logger.Warn("report generate: client info unavailable, rendering with empty branding",
			slog.String("client_id", args.ClientID.String()),
			slog.Any("error", err))
		clientInfo = ClientInfo{}
	}

	// Build report data.
	buildIn := BuildInput{
		TenantID:    args.TenantID,
		ClientID:    args.ClientID,
		Client:      clientInfo,
		AgencyName:  agencyName,
		Schedule:    sched,
		PeriodStart: args.PeriodStart,
		PeriodEnd:   args.PeriodEnd,
	}
	rd, err := BuildReportData(ctx, w.sources, buildIn)
	if err != nil {
		return err
	}

	// Snapshot.
	snapshot, err := json.Marshal(rd)
	if err != nil {
		return fmt.Errorf("report generate: marshal snapshot: %w", err)
	}

	// FIX-1: fetch and validate the logo exactly ONCE using the SSRF-safe client,
	// then pass the validated bytes to BOTH renderers. On any failure logoBytes
	// remains nil and both renderers render without a logo — never emit a remote
	// URL in either the HTML blob or the PDF.
	var logoBytes []byte
	if rd.LogoURL != "" && w.ssrfClient != nil {
		var fetchErr error
		logoBytes, fetchErr = pdf.FetchAndValidateLogo(ctx, w.ssrfClient, rd.LogoURL)
		if fetchErr != nil {
			w.logger.Info("report generate: logo fetch/validation failed, rendering without logo",
				slog.String("report_id", args.ReportID.String()),
				slog.Any("error", fetchErr))
			logoBytes = nil
		}
	}
	// Clear LogoURL in the ReportData snapshot so neither the HTML blob nor the
	// stored JSON snapshot contains a raw remote URL. The HTML renderer replaces
	// LogoURL with a data: URI when logoBytes are present.
	rd.LogoURL = ""

	// Render HTML.
	htmlBytes, err := w.htmlRenderer.Render(rd, logoBytes)
	if err != nil {
		return fmt.Errorf("report generate: render html: %w", err)
	}

	// Render PDF.
	pdfBytes, err := w.pdfRenderer.Render(rd, logoBytes)
	if err != nil {
		return fmt.Errorf("report generate: render pdf: %w", err)
	}

	// Upload blobs.
	htmlKey := blobKey(args.TenantID, args.ClientID, args.ReportID, "html")
	pdfKey := blobKey(args.TenantID, args.ClientID, args.ReportID, "pdf")

	if w.svc.blob != nil {
		if err := w.svc.blob.Put(ctx, htmlKey, bytes.NewReader(htmlBytes), int64(len(htmlBytes))); err != nil {
			return fmt.Errorf("report generate: upload html: %w", err)
		}
		if err := w.svc.blob.Put(ctx, pdfKey, bytes.NewReader(pdfBytes), int64(len(pdfBytes))); err != nil {
			return fmt.Errorf("report generate: upload pdf: %w", err)
		}
	}

	// Complete the report row.
	if _, err := w.repo.CompleteReport(ctx, args.TenantID, args.ReportID, htmlKey, pdfKey, snapshot); err != nil {
		return err
	}

	publishReportCompleted(ctx, w.svc.publisher, args.TenantID, args.ClientID, args.ReportID, StatusCompleted)

	// Notification email.
	if args.Notify {
		w.sendNotification(ctx, args, rd, htmlKey, pdfKey)
	}

	w.logger.Info("report generate: completed",
		slog.String("report_id", args.ReportID.String()),
		slog.String("client_id", args.ClientID.String()))
	return nil
}

// reportSettingsFor returns the report settings a run for this client uses:
// the client's report_schedules row, which holds them all (sections, intro and
// closing text, PoweredByRemoved). There is at most one row per client (UNIQUE
// (client_id)), so a scheduled run and an on-demand run read the same
// settings. An on-demand request's SectionsOverride is applied over the stored
// sections. Nil, nil means the client has no settings saved and the request
// overrides nothing: the report uses the defaults.
func reportSettingsFor(ctx context.Context, repo Repo, args GenerateArgs) (*Schedule, error) {
	s, found, err := repo.GetSchedule(ctx, args.TenantID, args.ClientID)
	if err != nil {
		return nil, fmt.Errorf("report generate: get report settings: %w", err)
	}
	if !found {
		if args.SectionsOverride == nil {
			return nil, nil
		}
		s = defaultSchedule(args.ClientID, args.TenantID)
	}
	if args.SectionsOverride != nil {
		s.Sections = args.SectionsOverride.Apply(s.Sections)
	}
	return &s, nil
}

func (w *GenerateWorker) sendNotification(ctx context.Context, args GenerateArgs, rd ReportData, htmlKey, pdfKey string) {
	// Degrade silently when mailer is not configured.
	if w.svc.mailer == nil || !w.svc.MailerConfigured(ctx) {
		w.logger.Info("report generate: mailer not configured, skipping email notification",
			slog.String("report_id", args.ReportID.String()))
		return
	}

	// Resolve recipients from schedule.
	recipients := resolveRecipients(ctx, w.repo, args.TenantID, args.ClientID)
	if len(recipients) == 0 {
		return
	}

	// Mint 7-day presigned URLs for the email.
	var htmlURL, pdfURL string
	if w.svc.blob != nil {
		htmlURL, _ = w.svc.blob.PresignGet(ctx, htmlKey, presignTTL)
		pdfURL, _ = w.svc.blob.PresignGet(ctx, pdfKey, presignTTL)
	}

	data := map[string]any{
		"agency_name":     rd.AgencyName,
		"client_name":     rd.ClientName,
		"period_label":    rd.PeriodLabel,
		"site_count":      rd.Totals.SiteCount,
		"avg_uptime_pct":  fmt.Sprintf("%.1f", rd.Totals.AvgUptimePct),
		"backups_count":   rd.Totals.BackupsCount,
		"updates_applied": rd.Totals.UpdatesApplied,
		"emails_sent":     rd.Totals.EmailsSent,
		"emails_failed":   rd.Totals.EmailsFailed,
		"intro_text":      rd.IntroText,
		"closing_text":    rd.ClosingText,
		"html_url":        htmlURL,
		"pdf_url":         pdfURL,
		"show_powered_by": rd.ShowPoweredBy,
	}

	if err := w.svc.mailer.Enqueue(ctx, args.TenantID, recipients, "report_ready", data); err != nil {
		w.logger.Warn("report generate: notification email enqueue failed (best-effort)",
			slog.String("report_id", args.ReportID.String()),
			slog.Any("error", err))
	}
}

// resolveRecipients returns recipients from the schedule, falling back to
// clients.contact_email when the schedule has none.
func resolveRecipients(ctx context.Context, repo Repo, tenantID, clientID uuid.UUID) []string {
	sched, found, err := repo.GetSchedule(ctx, tenantID, clientID)
	if err == nil && found && len(sched.Recipients) > 0 {
		// Re-trim to the hard cap (already enforced on PUT; defensive).
		if len(sched.Recipients) > maxRecipients {
			return sched.Recipients[:maxRecipients]
		}
		return sched.Recipients
	}
	return nil
}

// blobKey returns the storage key: tenants/<tenantID>/reports/<clientID>/<reportID>.<ext>
// Note: plural "tenants" is the pinned convention for reports (different from
// backup's singular "tenant/" scheme — do not "fix" this).
func blobKey(tenantID, clientID, reportID uuid.UUID, ext string) string {
	return fmt.Sprintf("tenants/%s/reports/%s/%s.%s",
		tenantID, clientID, reportID, ext)
}

// ---------------------------------------------------------------------------
// ScheduleScanWorker — scans for due schedules and enqueues generate jobs
// ---------------------------------------------------------------------------

// ScheduleScanArgs is the River job payload for the periodic schedule scanner.
type ScheduleScanArgs struct{}

// Kind implements river.JobArgs.
func (ScheduleScanArgs) Kind() string { return "report_scheduler" }

// ScheduleScanWorker scans for due report_schedules rows and enqueues
// GenerateArgs for each, advancing next_run_at atomically (mirrors
// email.DigestWorker and backup.ScheduleWorker).
type ScheduleScanWorker struct {
	river.WorkerDefaults[ScheduleScanArgs]
	repo        Repo
	riverClient *river.Client[pgx.Tx]
	logger      *slog.Logger
}

// NewScheduleScanWorker builds the scanner worker. riverClient may be nil at
// construction time and set later via SetRiverClient (like backup.Service.SetEnqueuer).
func NewScheduleScanWorker(repo Repo, logger *slog.Logger) *ScheduleScanWorker {
	if logger == nil {
		logger = slog.Default()
	}
	return &ScheduleScanWorker{repo: repo, logger: logger}
}

// SetRiverClient injects the River client after it has been started. Must be
// called before the worker's first Work execution (i.e. before the first
// ScheduleScanArgs job fires).
func (w *ScheduleScanWorker) SetRiverClient(client *river.Client[pgx.Tx]) {
	w.riverClient = client
}

// Work scans due schedules and enqueues generate jobs.
func (w *ScheduleScanWorker) Work(ctx context.Context, _ *river.Job[ScheduleScanArgs]) error {
	due, err := w.repo.ListDueSchedules(ctx, 100)
	if err != nil {
		w.logger.Error("report scheduler: list due failed", slog.Any("error", err))
		return err
	}

	for _, d := range due {
		if !d.Enabled {
			continue
		}

		now := time.Now().UTC()

		// Compute period: end=now, start = last_run_at if set, else end - period.
		var periodStart time.Time
		if d.LastRunAt != nil {
			periodStart = *d.LastRunAt
		} else {
			switch d.Cadence {
			case "monthly":
				periodStart = now.AddDate(0, -1, 0)
			default:
				periodStart = now.Add(-7 * 24 * time.Hour)
			}
		}
		periodEnd := now

		// FIX-2: clamp the scheduler-derived period to maxPeriodDays (same cap as
		// GenerateNow). A stale last_run_at (e.g. after a long outage) would
		// otherwise produce a multi-year window that exhausts every data source
		// and inflates job payload size well beyond the intended cap.
		if periodEnd.Sub(periodStart) > time.Duration(maxPeriodDays)*24*time.Hour {
			periodStart = periodEnd.Add(-time.Duration(maxPeriodDays) * 24 * time.Hour)
		}

		// Compute next run time.
		tz := d.ClientTimezone
		if tz == "" {
			tz = "UTC"
		}
		newNextAt := nextRunAt(d.Cadence, d.SendDay, d.SendHour, tz)
		if newNextAt == nil {
			continue
		}

		// Claim atomically. Race ⇒ skip.
		claimed, claimErr := w.repo.ClaimAdvanceSchedule(ctx, d.TenantID, d.ID, *newNextAt)
		if claimErr != nil {
			if strings.Contains(claimErr.Error(), "not found") || claimErr == ErrNotFound {
				continue // raced — another worker claimed it
			}
			w.logger.Warn("report scheduler: claim failed",
				slog.String("schedule_id", d.ID.String()),
				slog.Any("error", claimErr))
			continue
		}

		// Create pending report row.
		rpt, rptErr := w.repo.CreateReportAgent(ctx, CreateReportInput{
			TenantID:    d.TenantID,
			ClientID:    d.ClientID,
			ScheduleID:  &claimed.ID,
			PeriodStart: periodStart,
			PeriodEnd:   periodEnd,
		})
		if rptErr != nil {
			w.logger.Warn("report scheduler: create report row failed",
				slog.String("schedule_id", d.ID.String()),
				slog.Any("error", rptErr))
			continue
		}

		if w.riverClient == nil {
			w.logger.Warn("report scheduler: river client not wired, skipping enqueue",
				slog.String("schedule_id", d.ID.String()))
			continue
		}
		// Enqueue generate job.
		schedID := claimed.ID
		_, insertErr := w.riverClient.Insert(ctx, GenerateArgs{
			TenantID:    d.TenantID,
			ClientID:    d.ClientID,
			ReportID:    rpt.ID,
			ScheduleID:  &schedID,
			PeriodStart: periodStart,
			PeriodEnd:   periodEnd,
			Notify:      true,
		}, nil)
		if insertErr != nil {
			w.logger.Warn("report scheduler: enqueue generate job failed",
				slog.String("report_id", rpt.ID.String()),
				slog.Any("error", insertErr))
		} else {
			w.logger.Info("report scheduler: enqueued generate job",
				slog.String("report_id", rpt.ID.String()),
				slog.String("client_id", d.ClientID.String()))
		}
	}
	return nil
}
