package uptime

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/metrics"
)

// TestGetFleetStatus_NoMeasurementSerialisesNull is the GH #460 regression
// test for the user-visible half of the bug: a site with no uptime
// measurement must put `"uptime_pct_7d": null` on the wire, never `0`.
//
// Before this fix, FleetStatusItem.UptimePct7d was a bare float64 and
// GetFleetStatus discarded the store's nil, so a never-probed site serialised
// 0. The dashboard reads 0 as "0% available" and painted the site as a 90-day
// solid-red outage strip — a claim about someone's site we had no measurement
// to support, on a surface agencies are told they can lift into a client
// report. Proven red against the pre-fix source (the assertion below reported
// `uptime_pct_7d as 0, want null`) and green after.
//
// The two ways "no measurement" reaches the service come from different places
// in the store contract, so both are covered:
//
//   - absent from the map entirely: no site_uptime_status row at all, or its
//     last probe has aged past probeRetention — pgStore.QueryFleetUptime skips
//     any row whose latest_up is NULL.
//   - present with UptimePct7d == nil: there IS a recent latest-probe
//     snapshot, but the windowed counters summed to total_checks = 0.
//
// A future refactor that "helpfully" defaults either one to 0 reintroduces
// exactly the shipped defect.
func TestGetFleetStatus_NoMeasurementSerialisesNull(t *testing.T) {
	tenantID := uuid.New()
	neverProbed := uuid.New()
	probedNoCounters := uuid.New()
	measured := uuid.New()

	up := true
	probedAt := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	pct := 99.5

	repo := &stubRepo{infos: []FleetSiteInfo{
		{SiteID: neverProbed, Name: "never-probed", URL: "https://never.example",
			ConnectionState: "connected", HealthStatus: "unknown"},
		{SiteID: probedNoCounters, Name: "no-counters", URL: "https://fresh.example",
			ConnectionState: "connected", HealthStatus: "healthy"},
		{SiteID: measured, Name: "measured", URL: "https://measured.example",
			ConnectionState: "connected", HealthStatus: "healthy"},
	}}
	store := &stubStore{uptimeMap: map[uuid.UUID]metrics.FleetUptimeRow{
		// neverProbed: deliberately ABSENT — that is the store's "no data".
		probedNoCounters: {Up: &up, LastProbeAt: &probedAt, UptimePct7d: nil},
		measured:         {Up: &up, LastProbeAt: &probedAt, UptimePct7d: &pct},
	}}

	svc := NewService(repo, store, nil)
	resp, err := svc.GetFleetStatus(context.Background(), tenantID,
		[]uuid.UUID{neverProbed, probedNoCounters, measured})
	if err != nil {
		t.Fatalf("GetFleetStatus: %v", err)
	}

	byID := make(map[uuid.UUID]FleetStatusItem, len(resp.Items))
	for _, it := range resp.Items {
		byID[it.SiteID] = it
	}
	if len(byID) != 3 {
		t.Fatalf("got %d items, want 3", len(byID))
	}

	// Domain level: nil, not a zero float.
	for _, id := range []uuid.UUID{neverProbed, probedNoCounters} {
		if got := byID[id].UptimePct7d; got != nil {
			t.Errorf("site %s UptimePct7d = %v, want nil — a site with no measurement must not report a percentage",
				byID[id].Name, *got)
		}
	}
	if got := byID[measured].UptimePct7d; got == nil || *got != pct {
		t.Errorf("measured site UptimePct7d = %v, want %v — the fix must not blank real data", got, pct)
	}

	// Wire level: this is what the dashboard actually parses, so assert the
	// encoded bytes rather than trusting the struct.
	//
	// Items decode into map[string]json.RawMessage on purpose: unmarshalling a
	// JSON `null` into any pointer field sets that pointer to nil, which makes
	// "key present, value null" indistinguishable from "key absent" — and the
	// difference between those two IS the contract here (fleet-types.ts
	// declares the key required and nullable). A raw map keeps the literal
	// bytes.
	var wire struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal FleetStatusResponse: %v", err)
	}
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(wire.Items) != 3 {
		t.Fatalf("wire has %d items, want 3", len(wire.Items))
	}
	want := map[string]string{
		"never-probed": "null",
		"no-counters":  "null",
		"measured":     "99.5",
	}
	seen := 0
	for _, it := range wire.Items {
		var name string
		if err := json.Unmarshal(it["name"], &name); err != nil {
			t.Fatalf("decode name: %v", err)
		}
		wantRaw, known := want[name]
		if !known {
			t.Fatalf("unexpected site %q in response", name)
		}
		raw, present := it["uptime_pct_7d"]
		if !present {
			t.Errorf("site %s: uptime_pct_7d key missing from JSON — the web contract requires the key to be present", name)
			continue
		}
		if got := string(raw); got != wantRaw {
			t.Errorf("site %s: uptime_pct_7d serialised as %s, want %s", name, got, wantRaw)
		}
		seen++
	}
	if seen != len(want) {
		t.Errorf("asserted %d of %d sites — the loop skipped one, so a silent pass is possible", seen, len(want))
	}
}
