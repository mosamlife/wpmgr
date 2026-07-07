package uptime

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestIncidentDetailJSONContract locks the JSON field names + null-vs-omitted
// discipline of the incident-detail response (GET
// /api/v1/fleet/incidents/:incidentId, M94/GH #148 part 1) to the frontend
// contract (apps/web/src/features/fleet/fleet-types.ts IncidentDetail). An
// OPEN incident must marshal ended_at/duration_seconds as explicit `null`,
// mirroring the FleetIncidentItem fix (see TestFleetIncidentItemJSONContract)
// — the same omitted-vs-null bug class would otherwise recur here — and
// probes must always be `[]`, never `null`, even when the metrics store
// returned no data for the window (graceful degradation: an empty timeline
// must not become an omitted/null key that breaks a `.map()` on the client).
func TestIncidentDetailJSONContract(t *testing.T) {
	started := time.Now().UTC().Add(-10 * time.Minute)

	requiredKeys := []string{
		"id", "site_id", "name", "url", "started_at", "ended_at",
		"duration_seconds", "ongoing", "peak_status", "last_http_status",
		"reason", "incident_count_30d", "probes", "probes_truncated",
	}

	t.Run("open incident", func(t *testing.T) {
		open := IncidentDetail{
			ID:               uuid.New(),
			SiteID:           uuid.New(),
			Name:             "Example Site",
			URL:              "https://example.com",
			StartedAt:        started,
			EndedAt:          nil,
			DurationSeconds:  nil,
			Ongoing:          true,
			PeakStatus:       "down",
			LastHTTPStatus:   500,
			Reason:           "http status 500",
			IncidentCount30d: 3,
			Probes:           []IncidentProbe{},
			ProbesTruncated:  false,
		}

		b, err := json.Marshal(open)
		if err != nil {
			t.Fatalf("marshal open IncidentDetail: %v", err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		for _, k := range requiredKeys {
			if _, ok := m[k]; !ok {
				t.Errorf("open IncidentDetail JSON missing contract key %q (got keys %v)", k, keysOf(m))
			}
		}
		if raw, ok := m["ended_at"]; !ok {
			t.Errorf("open IncidentDetail: ended_at key omitted entirely (want present + null)")
		} else if string(raw) != "null" {
			t.Errorf("open IncidentDetail: ended_at = %s, want null", raw)
		}
		if raw, ok := m["duration_seconds"]; !ok {
			t.Errorf("open IncidentDetail: duration_seconds key omitted entirely (want present + null)")
		} else if string(raw) != "null" {
			t.Errorf("open IncidentDetail: duration_seconds = %s, want null", raw)
		}
		if string(m["probes"]) != "[]" {
			t.Errorf("open IncidentDetail: probes = %s, want [] (never null, even with an empty metrics window)", m["probes"])
		}
		if got := string(m["ongoing"]); got != "true" {
			t.Errorf("open IncidentDetail: ongoing = %s, want true", got)
		}
	})

	t.Run("closed incident", func(t *testing.T) {
		ended := started.Add(5 * time.Minute)
		dur := int64(300)
		closed := IncidentDetail{
			ID:               uuid.New(),
			SiteID:           uuid.New(),
			Name:             "Example Site",
			URL:              "https://example.com",
			StartedAt:        started,
			EndedAt:          &ended,
			DurationSeconds:  &dur,
			Ongoing:          false,
			PeakStatus:       "down",
			LastHTTPStatus:   200,
			Reason:           "",
			IncidentCount30d: 1,
			Probes: []IncidentProbe{
				{ProbedAt: started, Up: false, HTTPStatus: 500, TotalMs: 120.5, Error: "http status 500"},
				{ProbedAt: ended, Up: true, HTTPStatus: 200, TotalMs: 80.0},
			},
			ProbesTruncated: false,
		}

		b, err := json.Marshal(closed)
		if err != nil {
			t.Fatalf("marshal closed IncidentDetail: %v", err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		for _, k := range requiredKeys {
			if _, ok := m[k]; !ok {
				t.Errorf("closed IncidentDetail JSON missing contract key %q (got keys %v)", k, keysOf(m))
			}
		}
		if string(m["ended_at"]) == "null" {
			t.Errorf("closed IncidentDetail: ended_at should not be null")
		}
		if string(m["duration_seconds"]) == "null" {
			t.Errorf("closed IncidentDetail: duration_seconds should not be null")
		}

		var probes []map[string]json.RawMessage
		if err := json.Unmarshal(m["probes"], &probes); err != nil {
			t.Fatalf("unmarshal probes: %v", err)
		}
		if len(probes) != 2 {
			t.Fatalf("expected 2 probes, got %d", len(probes))
		}
		if _, ok := probes[0]["error"]; !ok {
			t.Errorf("a down probe's non-empty error must be present, not omitted")
		}
		if _, ok := probes[1]["error"]; ok {
			t.Errorf("a healthy probe's empty error must be omitted entirely, got %s", probes[1]["error"])
		}
	})
}
