package site

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The integration package is excluded from CI (see
// .claude/rules/ci-and-build-logic.md), so gh414_sites_list_pause_fields_test.go
// proves the endpoints end-to-end but proves it nowhere CI can see. These two
// run in `go test ./internal/...`: they pin the exact wire names and the
// absent-means-active contract that both GET /sites and GET /sites/{id} serve,
// since both map through toAPI.

// TestToAPICarriesMonitoringPauseAndHealthAsOf asserts the five phase-4a fields
// reach the wire under the names the web client reads. Marshalling the DTO
// rather than reading its Go fields is deliberate: a rename in openapi.yaml
// that was never regenerated changes the JSON tag, not the Go field.
func TestToAPICarriesMonitoringPauseAndHealthAsOf(t *testing.T) {
	pausedAt := time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC)
	resumeAt := time.Date(2026, 8, 16, 9, 0, 0, 0, time.UTC)
	// An hour before the pause: the last probe that actually ran. Nothing will
	// move this again while the pause holds, which is why health_status needs
	// it alongside.
	checkedAt := time.Date(2026, 8, 15, 8, 0, 0, 0, time.UTC)
	actor := uuid.New()

	s := Site{
		ID:                     uuid.New(),
		TenantID:               uuid.New(),
		URL:                    "https://example.com",
		Name:                   "Example",
		Status:                 "active",
		HealthStatus:           "healthy",
		ConnectionState:        StateConnected,
		Tags:                   []string{},
		CreatedAt:              pausedAt,
		UpdatedAt:              pausedAt,
		MonitoringPausedAt:     &pausedAt,
		MonitoringPausedBy:     &actor,
		MonitoringPausedReason: "database migration window",
		MonitoringResumeAt:     &resumeAt,
		HealthCheckedAt:        &checkedAt,
	}

	out := toAPI(s)
	b, err := json.Marshal(&out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	for key, want := range map[string]string{
		"monitoring_paused_at":     pausedAt.Format(time.RFC3339),
		"monitoring_resume_at":     resumeAt.Format(time.RFC3339),
		"monitoring_paused_by":     actor.String(),
		"monitoring_paused_reason": "database migration window",
		"health_checked_at":        checkedAt.Format(time.RFC3339),
	} {
		raw, ok := got[key]
		if !ok {
			t.Fatalf("%s absent from the wire payload: %s", key, string(b))
		}
		str, ok := raw.(string)
		if !ok {
			t.Fatalf("%s is %T, want string", key, raw)
		}
		if key == "monitoring_paused_reason" || key == "monitoring_paused_by" {
			if str != want {
				t.Fatalf("%s = %q, want %q", key, str, want)
			}
			continue
		}
		parsed, perr := time.Parse(time.RFC3339Nano, str)
		if perr != nil {
			t.Fatalf("%s = %q does not parse as RFC 3339: %v", key, str, perr)
		}
		wantT, _ := time.Parse(time.RFC3339, want)
		if !parsed.Equal(wantT) {
			t.Fatalf("%s = %v, want %v", key, parsed, wantT)
		}
	}

	// health_status must still be served. The fix for a frozen badge is to date
	// it, never to withhold it: an operator who paused a site still wants to
	// see the last known verdict.
	if got["health_status"] != "healthy" {
		t.Fatalf("health_status = %v, want healthy", got["health_status"])
	}
}

// TestToAPIOmitsMonitoringPauseWhenActive pins the other half of the contract:
// absent monitoring_paused_at IS "monitoring is active". There is no separate
// boolean, so nothing can disagree with the timestamp — and an unprobed site
// omits health_checked_at rather than defaulting to a zero time the UI would
// render as 0001-01-01.
func TestToAPIOmitsMonitoringPauseWhenActive(t *testing.T) {
	now := time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC)
	s := Site{
		ID:              uuid.New(),
		TenantID:        uuid.New(),
		URL:             "https://example.com",
		Name:            "Example",
		Status:          "active",
		HealthStatus:    "healthy",
		ConnectionState: StateConnected,
		Tags:            []string{},
		CreatedAt:       now,
		UpdatedAt:       now,
	}

	out := toAPI(s)
	b, err := json.Marshal(&out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	for _, key := range []string{
		"monitoring_paused_at",
		"monitoring_paused_by",
		"monitoring_paused_reason",
		"monitoring_resume_at",
		"health_checked_at",
	} {
		if v, ok := got[key]; ok {
			t.Fatalf("%s should be absent for an active, never-probed site, got %v", key, v)
		}
	}
}
