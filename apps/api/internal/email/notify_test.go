package email

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// ---------------------------------------------------------------------------
// issue #123 — Notifications settings page couldn't be saved:
//   1. digest_cadence="daily" was rejected (UI only ever collects a "Daily
//      digest" toggle + a single send-at time, never a cadence/day picker).
//   2. A digest validation error blocked the unrelated per-failure-alerts
//      section (recipients/throttle) from saving too.
// ---------------------------------------------------------------------------

func TestPutNotifySettings_DailyDigest_NoDigestDayRequired(t *testing.T) {
	tenantID := uuid.New()
	repo := newFakeRepo()
	svc := NewService(&Repo{}, &fakeEncryptor{}, nil)
	svc.repo = repo

	in := NotifySettingsUpsertInput{
		TenantID:             tenantID,
		Enabled:              true,
		Recipients:           []string{"ops@example.com"},
		AlertOnFailure:       true,
		AlertThrottleMinutes: 60,
		DigestEnabled:        true,
		DigestCadence:        "daily",
		DigestDay:            0, // not collected by the UI for daily cadence
		DigestHour:           8,
		Timezone:             "UTC",
	}

	saved, err := svc.PutNotifySettings(context.Background(), in)
	if err != nil {
		t.Fatalf("PutNotifySettings with daily cadence: unexpected error: %v", err)
	}
	if saved.DigestCadence != "daily" {
		t.Errorf("expected DigestCadence 'daily', got %q", saved.DigestCadence)
	}
	if saved.NextDigestAt == nil {
		t.Error("expected next_digest_at to be computed for an enabled daily digest")
	}
}

func TestPutNotifySettings_DigestDisabled_AlertsOnlySaveSucceeds(t *testing.T) {
	tenantID := uuid.New()
	repo := newFakeRepo()
	svc := NewService(&Repo{}, &fakeEncryptor{}, nil)
	svc.repo = repo

	// Digest is off and its fields are left zero-valued/invalid — exactly what
	// a form only rendering the "Per-failure alerts" section would send. This
	// must succeed and must not require digest_cadence/digest_day at all.
	in := NotifySettingsUpsertInput{
		TenantID:             tenantID,
		Enabled:              true,
		Recipients:           []string{"alerts@example.com"},
		AlertOnFailure:       true,
		AlertThrottleMinutes: 120,
		DigestEnabled:        false,
		DigestCadence:        "",
		DigestDay:            0,
		DigestHour:           0,
		Timezone:             "",
	}

	saved, err := svc.PutNotifySettings(context.Background(), in)
	if err != nil {
		t.Fatalf("PutNotifySettings with digest disabled: unexpected error: %v", err)
	}
	if saved.AlertThrottleMinutes != 120 {
		t.Errorf("expected AlertThrottleMinutes 120, got %d", saved.AlertThrottleMinutes)
	}
	if saved.NextDigestAt != nil {
		t.Error("expected next_digest_at to stay nil when digest is disabled")
	}
	// Digest fields must be normalized to valid column-default values (the DB
	// CHECK constraints apply to every row regardless of digest_enabled), not
	// persisted verbatim from the unvalidated input.
	if saved.DigestCadence != "weekly" {
		t.Errorf("expected normalized DigestCadence 'weekly', got %q", saved.DigestCadence)
	}
	if saved.Timezone != "UTC" {
		t.Errorf("expected normalized Timezone 'UTC', got %q", saved.Timezone)
	}
}

func TestValidateNotifySettings_InvalidDigestCadence_RejectedOnlyWhenEnabled(t *testing.T) {
	base := NotifySettingsUpsertInput{
		Recipients:           []string{"a@example.com"},
		AlertThrottleMinutes: 60,
	}

	// Digest enabled with a bogus cadence — must be rejected.
	enabled := base
	enabled.DigestEnabled = true
	enabled.DigestCadence = "hourly"
	enabled.DigestHour = 8
	enabled.Timezone = "UTC"
	if err := validateNotifySettings(enabled); err == nil {
		t.Error("expected error for invalid digest_cadence when digest is enabled")
	}

	// Digest disabled with the same bogus/empty cadence — must be accepted.
	disabled := base
	disabled.DigestEnabled = false
	disabled.DigestCadence = "hourly"
	if err := validateNotifySettings(disabled); err != nil {
		t.Errorf("expected no error for digest fields when digest is disabled, got: %v", err)
	}
}

func TestValidateNotifySettings_DailyCadenceAccepted(t *testing.T) {
	in := NotifySettingsUpsertInput{
		Recipients:           []string{"a@example.com"},
		AlertThrottleMinutes: 60,
		DigestEnabled:        true,
		DigestCadence:        "daily",
		DigestHour:           8,
		Timezone:             "UTC",
	}
	if err := validateNotifySettings(in); err != nil {
		t.Errorf("expected daily cadence to be accepted without digest_day, got: %v", err)
	}
}

func TestNextDigestAt_Daily(t *testing.T) {
	next := nextDigestAt("daily", 0, 8, "UTC")
	if next == nil {
		t.Fatal("expected a non-nil next digest time for daily cadence")
	}
	if next.Hour() != 8 {
		t.Errorf("expected hour 8, got %d", next.Hour())
	}
}

// ---------------------------------------------------------------------------
// m103 (GH #247) — vulnerability digest section
// ---------------------------------------------------------------------------

// fakeVulnDigestSource is a test double for VulnDigestSource.
type fakeVulnDigestSource struct {
	summary  VulnDigestSummary
	included bool
	err      error
}

func (f *fakeVulnDigestSource) GetVulnDigestSummary(_ context.Context, _ uuid.UUID) (VulnDigestSummary, bool, error) {
	return f.summary, f.included, f.err
}

// TestBuildDigestData_VulnOnly_StillSends is the widened-skip-send
// regression: a tenant with ZERO email activity but open vulnerabilities
// (and vuln_include_in_digest on) must still receive a digest instead of
// being silently skipped — the pre-m103 rule skipped whenever the email
// Total was 0, which would have suppressed a vuln-only digest entirely.
func TestBuildDigestData_VulnOnly_StillSends(t *testing.T) {
	tenantID := uuid.New()
	svc := NewService(&Repo{}, &fakeEncryptor{}, nil)
	svc.repo = newFakeRepo()
	svc.vulnDigest = &fakeVulnDigestSource{
		included: true,
		summary: VulnDigestSummary{
			OpenCount:         3,
			CriticalHighCount: 1,
			Top: []VulnDigestItem{
				{SiteName: "example.com", Component: "Plugin: Foo", Severity: "Critical", CVE: "CVE-2024-1"},
			},
		},
	}
	settings := defaultNotifySettings(tenantID)
	from, to := time.Now().AddDate(0, 0, -7), time.Now()

	data, err := svc.buildDigestData(context.Background(), tenantID, settings, from, to)
	if err != nil {
		t.Fatalf("buildDigestData: %v", err)
	}
	if data == nil {
		t.Fatal("expected a non-nil digest data map for a vuln-only period (widened skip-send)")
	}
	if data["Total"] != int64(0) {
		t.Errorf("Total should still reflect zero email activity, got %v", data["Total"])
	}
	if data["OpenVulnCount"] != 3 {
		t.Errorf("OpenVulnCount = %v, want 3", data["OpenVulnCount"])
	}
	if data["CriticalHighCount"] != 1 {
		t.Errorf("CriticalHighCount = %v, want 1", data["CriticalHighCount"])
	}
	topVulns, ok := data["TopVulns"].([]map[string]any)
	if !ok || len(topVulns) != 1 {
		t.Fatalf("expected 1 TopVulns entry, got %v", data["TopVulns"])
	}
	if data["VulnDashboardURL"] == "" {
		t.Error("expected a non-empty VulnDashboardURL")
	}
}

// TestBuildDigestData_FlagOff_OmitsSection_PreservesZeroSkip proves the
// vuln_include_in_digest gate: when the flag is off (included=false, even
// though the source has open findings), the vuln section is entirely absent
// from the data, AND the original Total==0 skip-send rule still applies when
// there is also no email activity.
func TestBuildDigestData_FlagOff_OmitsSection_PreservesZeroSkip(t *testing.T) {
	tenantID := uuid.New()
	svc := NewService(&Repo{}, &fakeEncryptor{}, nil)
	svc.repo = newFakeRepo()
	svc.vulnDigest = &fakeVulnDigestSource{
		included: false, // flag off
		summary:  VulnDigestSummary{OpenCount: 5},
	}
	settings := defaultNotifySettings(tenantID)
	from, to := time.Now().AddDate(0, 0, -7), time.Now()

	data, err := svc.buildDigestData(context.Background(), tenantID, settings, from, to)
	if err != nil {
		t.Fatalf("buildDigestData: %v", err)
	}
	if data != nil {
		t.Fatalf("expected skip-send (nil) when the flag is off and there is no email activity, got %v", data)
	}
}

// ---------------------------------------------------------------------------
// GH #381 phase 2 — failure-detection coverage on GET notify-settings
// ---------------------------------------------------------------------------

// failureDetectionWire and notifySettingsWire mirror the wire JSON shape
// (dto.go's notifySettingsDTO / failureDetectionDTO) so these tests decode the
// handler's actual SERIALIZED response rather than inspecting Go structs the
// handler never sends.
type failureDetectionWire struct {
	SitesTotal      int    `json:"sites_total"`
	SitesCovered    int    `json:"sites_covered"`
	MinAgentVersion string `json:"min_agent_version"`
}

type notifySettingsWire struct {
	FailureDetection failureDetectionWire `json:"failure_detection"`
}

// notifySettingsGetCtx builds a request+principal context for GET
// /email/notify-settings — an org-level route with no path params.
func notifySettingsGetCtx(t *testing.T, p domain.Principal) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil).WithContext(domain.WithPrincipal(context.Background(), p))
	return c, rec
}

func decodeNotifySettingsWire(t *testing.T, rec *httptest.ResponseRecorder) notifySettingsWire {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body %s", rec.Code, rec.Body.String())
	}
	var wire notifySettingsWire
	if err := json.Unmarshal(rec.Body.Bytes(), &wire); err != nil {
		t.Fatalf("failed to decode response: %v, body: %s", err, rec.Body.String())
	}
	return wire
}

// TestGetNotifySettings_ReportsFailureDetectionCoverage: RED today because
// the failure_detection field does not exist yet. 3 connected sites, 2 at or
// above the gate, must serialize as sites_total=3, sites_covered=2.
func TestGetNotifySettings_ReportsFailureDetectionCoverage(t *testing.T) {
	tenantID := uuid.New()
	repo := newFakeRepo()
	repo.connectedAgentVersions = map[uuid.UUID][]string{
		tenantID: {"999.5.0", MinAgentVersionForFailureDetection, "0.61.138"},
	}
	svc := NewService(&Repo{}, &fakeEncryptor{}, nil)
	svc.repo = repo
	h := &Handler{svc: svc}

	c, rec := notifySettingsGetCtx(t, domain.Principal{
		Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: tenantID,
		Role: "admin", Scope: domain.ScopeOrg,
	})
	h.getNotifySettings(c)

	wire := decodeNotifySettingsWire(t, rec)
	if wire.FailureDetection.SitesTotal != 3 {
		t.Errorf("sites_total = %d, want 3", wire.FailureDetection.SitesTotal)
	}
	if wire.FailureDetection.SitesCovered != 2 {
		t.Errorf("sites_covered = %d, want 2", wire.FailureDetection.SitesCovered)
	}
	if wire.FailureDetection.MinAgentVersion != MinAgentVersionForFailureDetection {
		t.Errorf("min_agent_version = %q, want %q", wire.FailureDetection.MinAgentVersion, MinAgentVersionForFailureDetection)
	}
}

// TestGetNotifySettings_CoverageIsTenantScoped: the coverage count for tenant
// A must never include tenant B's fleet, however much larger it is. This is
// the one that matters most — get it wrong and the endpoint leaks another
// tenant's fleet size.
func TestGetNotifySettings_CoverageIsTenantScoped(t *testing.T) {
	tenantA, tenantB := uuid.New(), uuid.New()
	repo := newFakeRepo()
	repo.connectedAgentVersions = map[uuid.UUID][]string{
		tenantA: {"999.0.0"},
		tenantB: {"999.0.0", "999.0.0", "999.0.0", "999.0.0"},
	}
	svc := NewService(&Repo{}, &fakeEncryptor{}, nil)
	svc.repo = repo
	h := &Handler{svc: svc}

	c, rec := notifySettingsGetCtx(t, domain.Principal{
		Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: tenantA,
		Role: "admin", Scope: domain.ScopeOrg,
	})
	h.getNotifySettings(c)

	wire := decodeNotifySettingsWire(t, rec)
	if wire.FailureDetection.SitesTotal != 1 {
		t.Errorf("sites_total = %d, want 1 — tenant B's 4 sites leaked into tenant A's count", wire.FailureDetection.SitesTotal)
	}
	if wire.FailureDetection.SitesCovered != 1 {
		t.Errorf("sites_covered = %d, want 1", wire.FailureDetection.SitesCovered)
	}
}

// TestGetNotifySettings_FailureDetectionCoverage_NoOverfire is the
// over-fire control: a tenant with zero connected sites, and a tenant where
// every connected site is covered, must both produce sane values rather than
// a divide-by-zero or an omitted field.
func TestGetNotifySettings_FailureDetectionCoverage_NoOverfire(t *testing.T) {
	t.Run("zero_sites", func(t *testing.T) {
		tenantID := uuid.New()
		repo := newFakeRepo() // no entry for tenantID — zero connected sites
		svc := NewService(&Repo{}, &fakeEncryptor{}, nil)
		svc.repo = repo
		h := &Handler{svc: svc}

		c, rec := notifySettingsGetCtx(t, domain.Principal{
			Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: tenantID,
			Role: "admin", Scope: domain.ScopeOrg,
		})
		h.getNotifySettings(c)

		wire := decodeNotifySettingsWire(t, rec)
		if wire.FailureDetection.SitesTotal != 0 || wire.FailureDetection.SitesCovered != 0 {
			t.Errorf("expected 0/0 for a tenant with no connected sites, got %+v", wire.FailureDetection)
		}
		if !strings.Contains(rec.Body.String(), `"failure_detection"`) {
			t.Error("failure_detection must be present even for a zero-site tenant, never omitted")
		}
	})

	t.Run("all_covered", func(t *testing.T) {
		tenantID := uuid.New()
		repo := newFakeRepo()
		repo.connectedAgentVersions = map[uuid.UUID][]string{
			tenantID: {"999.0.0", "999.1.0", "1000.0.0"},
		}
		svc := NewService(&Repo{}, &fakeEncryptor{}, nil)
		svc.repo = repo
		h := &Handler{svc: svc}

		c, rec := notifySettingsGetCtx(t, domain.Principal{
			Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: tenantID,
			Role: "admin", Scope: domain.ScopeOrg,
		})
		h.getNotifySettings(c)

		wire := decodeNotifySettingsWire(t, rec)
		if wire.FailureDetection.SitesTotal != 3 || wire.FailureDetection.SitesCovered != 3 {
			t.Errorf("expected 3/3 when every connected site is covered, got %+v", wire.FailureDetection)
		}
	})
}

// ---------------------------------------------------------------------------
// GH #381 phase 5 — maybeAlertFailures observability: every exit path logs a
// reason, the detached goroutine is bounded and panic-safe, and recipient
// addresses never reach a log record.
// ---------------------------------------------------------------------------

// capturedRecord is a slog.Record flattened for assertions.
type capturedRecord struct {
	Level slog.Level
	Msg   string
	Attrs map[string]string
}

// capturingHandler is a minimal slog.Handler that records everything it is
// given, regardless of level, so a test can assert on Debug records too.
type capturingHandler struct {
	mu      sync.Mutex
	records []capturedRecord
}

func newCapturingHandler() *capturingHandler { return &capturingHandler{} }

func (h *capturingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *capturingHandler) Handle(_ context.Context, r slog.Record) error {
	attrs := make(map[string]string, r.NumAttrs())
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.String()
		return true
	})
	h.mu.Lock()
	h.records = append(h.records, capturedRecord{Level: r.Level, Msg: r.Message, Attrs: attrs})
	h.mu.Unlock()
	return nil
}

func (h *capturingHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *capturingHandler) WithGroup(_ string) slog.Handler      { return h }

func (h *capturingHandler) Records() []capturedRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]capturedRecord, len(h.records))
	copy(out, h.records)
	return out
}

// String dumps every captured record's message and attribute values, one per
// line, so a test can grep the WHOLE emitted surface for a value that must
// never appear (e.g. a recipient address) rather than checking one known key.
func (h *capturingHandler) String() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var sb strings.Builder
	for _, r := range h.records {
		sb.WriteString(r.Msg)
		sb.WriteString(" ")
		for k, v := range r.Attrs {
			sb.WriteString(k)
			sb.WriteString("=")
			sb.WriteString(v)
			sb.WriteString(" ")
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// fakeMailer is a MailerEnqueuer test double that counts calls and can
// optionally signal a channel per successful call, so a test can wait
// (bounded) for the detached alert goroutine to finish instead of sleeping.
type fakeMailer struct {
	mu             sync.Mutex
	enqueueErr     error
	calls          int
	lastRecipients []string
	notify         chan struct{}
}

func (f *fakeMailer) Enqueue(_ context.Context, _ uuid.UUID, recipients []string, _ string, _ map[string]any) error {
	f.mu.Lock()
	if f.enqueueErr != nil {
		f.mu.Unlock()
		return f.enqueueErr
	}
	f.calls++
	f.lastRecipients = recipients
	notify := f.notify
	f.mu.Unlock()
	if notify != nil {
		notify <- struct{}{}
	}
	return nil
}

func (f *fakeMailer) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// panicRepo wraps fakeRepo and panics from AccumulateAlertFailures, standing
// in for a real driver-level panic so TestMaybeAlertFailures_RecoversFromPanic
// has something genuine for the recover() in maybeAlertFailuresAsync to catch.
type panicRepo struct {
	*fakeRepo
}

func (r *panicRepo) AccumulateAlertFailures(context.Context, uuid.UUID, uuid.UUID, int64) error {
	panic("simulated repo panic — GH #381 phase 5 regression test")
}

// TestMaybeAlertFailures_EveryExitPathLogsAReason is table-driven over every
// documented exit of maybeAlertFailures (notify.go). RED before phase 5: five
// of these paths (nil_mailer, no_settings_row, disabled, alert_on_failure_off,
// no_recipients, throttled — six, in fact) emitted nothing at all.
func TestMaybeAlertFailures_EveryExitPathLogsAReason(t *testing.T) {
	tenantID, siteID := uuid.New(), uuid.New()
	fullSettings := NotifySettings{
		Enabled:              true,
		AlertOnFailure:       true,
		Recipients:           []string{"ops@example.com"},
		AlertThrottleMinutes: 60,
	}
	claimedState := &AlertState{TenantID: tenantID, SiteID: siteID, FailuresSinceAlert: 3}
	siteRef := &SiteRef{ID: siteID, Name: "example.com", URL: "https://example.com"}

	cases := []struct {
		name       string
		mailer     bool // false leaves svc.mailer nil
		mailerErr  error
		repo       func(r *fakeRepo)
		wantLevel  slog.Level
		wantReason string
	}{
		{
			name:       "nil_mailer",
			mailer:     false,
			wantLevel:  slog.LevelWarn,
			wantReason: "nil_mailer",
		},
		{
			name:   "accumulate_failed",
			mailer: true,
			repo: func(r *fakeRepo) {
				r.alertAccumulateErr = errors.New("db unavailable")
			},
			wantLevel:  slog.LevelWarn,
			wantReason: "accumulate_failed",
		},
		{
			name:       "no_settings_row",
			mailer:     true,
			wantLevel:  slog.LevelDebug,
			wantReason: "no_settings_row",
		},
		{
			name:   "settings_lookup_error",
			mailer: true,
			repo: func(r *fakeRepo) {
				r.alertSettingsErr = errors.New("connection reset")
			},
			wantLevel:  slog.LevelWarn,
			wantReason: "settings_lookup_error",
		},
		{
			name:   "disabled",
			mailer: true,
			repo: func(r *fakeRepo) {
				s := fullSettings
				s.Enabled = false
				r.alertSettings = &s
			},
			wantLevel:  slog.LevelDebug,
			wantReason: "disabled",
		},
		{
			name:   "alert_on_failure_off",
			mailer: true,
			repo: func(r *fakeRepo) {
				s := fullSettings
				s.AlertOnFailure = false
				r.alertSettings = &s
			},
			wantLevel:  slog.LevelDebug,
			wantReason: "alert_on_failure_off",
		},
		{
			name:   "no_recipients",
			mailer: true,
			repo: func(r *fakeRepo) {
				s := fullSettings
				s.Recipients = nil
				r.alertSettings = &s
			},
			wantLevel:  slog.LevelDebug,
			wantReason: "no_recipients",
		},
		{
			name:   "throttled",
			mailer: true,
			repo: func(r *fakeRepo) {
				s := fullSettings
				r.alertSettings = &s
				// alertClaimState/alertClaimErr left nil/nil == throttled.
			},
			wantLevel:  slog.LevelDebug,
			wantReason: "throttled",
		},
		{
			name:   "claim_alert_slot_error",
			mailer: true,
			repo: func(r *fakeRepo) {
				s := fullSettings
				r.alertSettings = &s
				r.alertClaimErr = errors.New("advisory lock timeout")
			},
			wantLevel:  slog.LevelWarn,
			wantReason: "claim_alert_slot_error",
		},
		{
			name:   "site_ref_error",
			mailer: true,
			repo: func(r *fakeRepo) {
				s := fullSettings
				r.alertSettings = &s
				r.alertClaimState = claimedState
				// alertSiteRef/alertSiteRefErr left at default == ErrNotFound.
			},
			wantLevel:  slog.LevelWarn,
			wantReason: "site_ref_error",
		},
		{
			name:   "enqueue_failed",
			mailer: true,
			repo: func(r *fakeRepo) {
				s := fullSettings
				r.alertSettings = &s
				r.alertClaimState = claimedState
				r.alertSiteRef = siteRef
			},
			mailerErr:  errors.New("smtp timeout"),
			wantLevel:  slog.LevelWarn,
			wantReason: "enqueue_failed",
		},
		{
			name:   "alert_sent",
			mailer: true,
			repo: func(r *fakeRepo) {
				s := fullSettings
				r.alertSettings = &s
				r.alertClaimState = claimedState
				r.alertSiteRef = siteRef
			},
			wantLevel:  slog.LevelInfo,
			wantReason: "alert_sent",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler := newCapturingHandler()
			logger := slog.New(handler)

			repo := newFakeRepo()
			if tc.repo != nil {
				tc.repo(repo)
			}

			svc := NewService(&Repo{}, &fakeEncryptor{}, logger)
			svc.repo = repo
			if tc.mailer {
				svc.mailer = &fakeMailer{enqueueErr: tc.mailerErr}
			}

			svc.maybeAlertFailures(context.Background(), tenantID, siteID, 7)

			var matches []capturedRecord
			for _, r := range handler.Records() {
				if r.Msg == alertDecisionEvent {
					matches = append(matches, r)
				}
			}
			if len(matches) != 1 {
				t.Fatalf("expected exactly 1 %q record, got %d: %+v", alertDecisionEvent, len(matches), handler.Records())
			}
			got := matches[0]
			if got.Level != tc.wantLevel {
				t.Errorf("level = %v, want %v", got.Level, tc.wantLevel)
			}
			if got.Attrs["reason"] != tc.wantReason {
				t.Errorf("reason = %q, want %q", got.Attrs["reason"], tc.wantReason)
			}
			if got.Attrs["tenant_id"] != tenantID.String() {
				t.Errorf("tenant_id = %q, want %q", got.Attrs["tenant_id"], tenantID.String())
			}
			if got.Attrs["site_id"] != siteID.String() {
				t.Errorf("site_id = %q, want %q", got.Attrs["site_id"], siteID.String())
			}
			if got.Attrs["failure_count"] != "7" {
				t.Errorf("failure_count = %q, want %q", got.Attrs["failure_count"], "7")
			}
		})
	}
}

// TestMaybeAlertFailures_RecoversFromPanic proves the detached goroutine
// survives a panic anywhere in maybeAlertFailures. RED before phase 5: there
// was no recover() at all, so this crashed the whole test binary — which is
// itself the proof a panic here would have taken down the live API process.
func TestMaybeAlertFailures_RecoversFromPanic(t *testing.T) {
	tenantID, siteID := uuid.New(), uuid.New()
	handler := newCapturingHandler()
	logger := slog.New(handler)

	svc := NewService(&Repo{}, &fakeEncryptor{}, logger)
	svc.repo = &panicRepo{fakeRepo: newFakeRepo()}
	svc.mailer = &fakeMailer{}

	done := make(chan struct{})
	go func() {
		svc.maybeAlertFailuresAsync(tenantID, siteID, 3)
		close(done)
	}()

	select {
	case <-done:
		// survived — half the proof.
	case <-time.After(5 * time.Second):
		t.Fatal("maybeAlertFailuresAsync did not return within 5s — recover() missing or goroutine hung")
	}

	var found bool
	for _, r := range handler.Records() {
		if r.Msg == alertDecisionEvent && r.Level == slog.LevelError && r.Attrs["reason"] == "panic" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected an Error record with reason=panic, got %+v", handler.Records())
	}
}

// TestMaybeAlertFailures_NeverLogsRecipientAddress is the assertion that
// matters most: no recipient address may appear in ANY emitted log record,
// on the success path or the enqueue-failure path. The fixture recipient is
// a distinctive string so this test would fail the moment someone logs the
// recipient list instead of just its count.
func TestMaybeAlertFailures_NeverLogsRecipientAddress(t *testing.T) {
	const distinctiveRecipient = "definitely-not-logged-9f3ac21b@example.com"
	tenantID, siteID := uuid.New(), uuid.New()
	claimedState := &AlertState{TenantID: tenantID, SiteID: siteID, FailuresSinceAlert: 2}
	siteRef := &SiteRef{ID: siteID, Name: "example.com", URL: "https://example.com"}

	for _, mailerErr := range []error{nil, errors.New("smtp rejected")} {
		handler := newCapturingHandler()
		logger := slog.New(handler)

		repo := newFakeRepo()
		repo.alertSettings = &NotifySettings{
			Enabled:              true,
			AlertOnFailure:       true,
			Recipients:           []string{distinctiveRecipient},
			AlertThrottleMinutes: 60,
		}
		repo.alertClaimState = claimedState
		repo.alertSiteRef = siteRef

		svc := NewService(&Repo{}, &fakeEncryptor{}, logger)
		svc.repo = repo
		svc.mailer = &fakeMailer{enqueueErr: mailerErr}

		svc.maybeAlertFailures(context.Background(), tenantID, siteID, 4)

		if dump := handler.String(); strings.Contains(dump, distinctiveRecipient) {
			t.Fatalf("recipient address leaked into a log record (mailerErr=%v):\n%s", mailerErr, dump)
		}
	}
}

// TestIngestLogBatch_FailureAlert_SendsExactlyOnce_IngestStaysAsync is the
// over-fire control: a batch that SHOULD trigger an alert still sends exactly
// one, unchanged from pre-phase-5 behaviour, and IngestLogBatch itself
// returns without waiting on the alert goroutine — phase 5 is logging and
// robustness only, never a synchronous write on the ingest hot path.
func TestIngestLogBatch_FailureAlert_SendsExactlyOnce_IngestStaysAsync(t *testing.T) {
	tenantID, siteID := uuid.New(), uuid.New()
	repo := newFakeRepo()
	repo.alertSettings = &NotifySettings{
		Enabled:              true,
		AlertOnFailure:       true,
		Recipients:           []string{"ops@example.com"},
		AlertThrottleMinutes: 60,
	}
	repo.alertClaimState = &AlertState{TenantID: tenantID, SiteID: siteID, FailuresSinceAlert: 1}
	repo.alertSiteRef = &SiteRef{ID: siteID, Name: "example.com", URL: "https://example.com"}

	mailer := &fakeMailer{notify: make(chan struct{}, 4)}
	svc := NewService(&Repo{}, &fakeEncryptor{}, slog.New(newCapturingHandler()))
	svc.repo = repo
	svc.mailer = mailer

	start := time.Now()
	result, err := svc.IngestLogBatch(context.Background(), tenantID, siteID, []IngestEntry{
		{AgentSeq: 1, Status: "failed", Subject: "test failure"},
	})
	ingestElapsed := time.Since(start)
	if err != nil {
		t.Fatalf("IngestLogBatch: unexpected error: %v", err)
	}
	if result.AckedThrough != 1 {
		t.Errorf("AckedThrough = %d, want 1", result.AckedThrough)
	}
	// The alert dispatch must not be on the ingest path.
	if ingestElapsed > time.Second {
		t.Errorf("IngestLogBatch took %v — the alert path must stay off the ingest hot path", ingestElapsed)
	}

	select {
	case <-mailer.notify:
		// alert dispatched, asynchronously, exactly as before phase 5.
	case <-time.After(5 * time.Second):
		t.Fatal("mailer.Enqueue was never called within 5s of a should-alert batch")
	}
	// Give an (incorrect) second dispatch a moment to also land before we count.
	time.Sleep(50 * time.Millisecond)

	if got := mailer.Calls(); got != 1 {
		t.Errorf("mailer.Enqueue called %d times, want exactly 1", got)
	}
}
