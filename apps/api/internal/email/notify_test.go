package email

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
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
