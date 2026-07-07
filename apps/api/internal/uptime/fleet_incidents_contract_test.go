package uptime

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestFleetIncidentItemJSONContract locks the JSON field names of the fleet
// incidents item to the web contract (apps/web/src/features/fleet incidents
// panel, GH #148). Three drift classes previously shipped bad data to the
// dashboard:
//   - a missing `kind` field made every row render as "Degraded", even a
//     down site (the client fell back on `inc.kind === "down" ? ... :
//     "Degraded"` and saw undefined);
//   - `duration_seconds`/`ended_at` used `omitempty`, so an OPEN incident
//     OMITTED the key entirely instead of emitting `null`; the client's
//     `=== null` check missed the omitted (`undefined`) key and rendered
//     "NaNh";
//   - `site_name`/`site_url` JSON tags did not match the `name`/`url` keys
//     the web reads (the same drift class fixed for FleetStatusItem in
//     0.47.1, see TestFleetStatusItemJSONContract).
//
// This test fails fast on any of the three regressing.
func TestFleetIncidentItemJSONContract(t *testing.T) {
	now := time.Now().UTC()
	dur := int64(120)
	totalMs := 842.5

	open := FleetIncidentItem{
		ID:              uuid.New(),
		SiteID:          uuid.New(),
		Kind:            "down",
		SiteName:        "Example Site",
		SiteURL:         "https://example.com",
		StartedAt:       &now,
		EndedAt:         nil,
		DurationSeconds: nil,
		Ongoing:         true,
		LatestTotalMs:   &totalMs,
	}
	ended := now.Add(2 * time.Minute)
	closed := FleetIncidentItem{
		ID:              uuid.New(),
		SiteID:          uuid.New(),
		Kind:            "down",
		SiteName:        "Example Site",
		SiteURL:         "https://example.com",
		StartedAt:       &now,
		EndedAt:         &ended,
		DurationSeconds: &dur,
		Ongoing:         false,
		LatestTotalMs:   &totalMs,
	}

	requiredKeys := []string{
		"id", "site_id", "kind", "name", "url", "started_at", "ended_at",
		"duration_seconds", "ongoing",
	}

	t.Run("open incident", func(t *testing.T) {
		b, err := json.Marshal(open)
		if err != nil {
			t.Fatalf("marshal open FleetIncidentItem: %v", err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		for _, k := range requiredKeys {
			if _, ok := m[k]; !ok {
				t.Errorf("open FleetIncidentItem JSON missing contract key %q (got keys %v)", k, keysOf(m))
			}
		}
		if got := string(m["kind"]); got != `"down"` {
			t.Errorf("open incident kind = %s, want \"down\"", got)
		}
		// The whole point of the fix: for an OPEN incident, ended_at and
		// duration_seconds must be present and explicitly null, NOT omitted.
		if raw, ok := m["ended_at"]; !ok {
			t.Errorf("open FleetIncidentItem: ended_at key omitted entirely (want present + null)")
		} else if string(raw) != "null" {
			t.Errorf("open FleetIncidentItem: ended_at = %s, want null", raw)
		}
		if raw, ok := m["duration_seconds"]; !ok {
			t.Errorf("open FleetIncidentItem: duration_seconds key omitted entirely (want present + null)")
		} else if string(raw) != "null" {
			t.Errorf("open FleetIncidentItem: duration_seconds = %s, want null", raw)
		}
	})

	t.Run("closed incident", func(t *testing.T) {
		b, err := json.Marshal(closed)
		if err != nil {
			t.Fatalf("marshal closed FleetIncidentItem: %v", err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		for _, k := range requiredKeys {
			if _, ok := m[k]; !ok {
				t.Errorf("closed FleetIncidentItem JSON missing contract key %q (got keys %v)", k, keysOf(m))
			}
		}
		if got := string(m["kind"]); got != `"down"` {
			t.Errorf("closed incident kind = %s, want \"down\"", got)
		}
		if string(m["ended_at"]) == "null" {
			t.Errorf("closed FleetIncidentItem: ended_at should not be null")
		}
		if string(m["duration_seconds"]) == "null" {
			t.Errorf("closed FleetIncidentItem: duration_seconds should not be null")
		}
	})
}
