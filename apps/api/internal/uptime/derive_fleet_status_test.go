package uptime

import "testing"

// TestDeriveFleetStatus_ConnectionStateMatrix is the GH #291 Task 1 unit test:
// it pins deriveFleetStatus's behavior for every connection_state value
// against an up=true (cached 200) probe result, proving:
//
//   - "disconnected" now derives Degraded when disconnected_reason names the
//     sweeper's own active-verify failure (the core fix: previously fell
//     through to a clean Up even though the connection sweeper's signed,
//     uncacheable active-verify had already failed against the agent).
//   - "degraded" continues to derive Degraded (pre-existing behavior).
//   - "connected"/"pending_enrollment" continue to derive Up.
//   - "revoked"/"archived" ALSO derive Up: the operator deliberately stopped
//     managing the site, so no alarming Degraded is raised (see GH #282 for
//     the identical precedent in backup scheduling).
//   - Task 2's status_reason is populated exactly when the status is
//     Degraded, and matches the specific cause.
func TestDeriveFleetStatus_ConnectionStateMatrix(t *testing.T) {
	up := true
	fast := 100.0

	cases := []struct {
		name               string
		connectionState    string
		disconnectedReason string
		wantStatus         FleetSiteStatus
		wantReason         string
	}{
		{"connected stays up", "connected", "", FleetStatusUp, ""},
		{"pending_enrollment stays up", "pending_enrollment", "", FleetStatusUp, ""},
		{"degraded derives degraded", "degraded", "", FleetStatusDegraded, FleetReasonAgentDegraded},
		{"disconnected+agent_unreachable derives degraded (GH #291 core fix)", "disconnected", "agent_unreachable", FleetStatusDegraded, FleetReasonAgentUnreachable},
		{"disconnected+heartbeat_timeout derives degraded (GH #291 core fix)", "disconnected", "heartbeat_timeout", FleetStatusDegraded, FleetReasonAgentUnreachable},
		{"revoked stays up (operator deliberately unmanaged, GH #282 precedent)", "revoked", "", FleetStatusUp, ""},
		{"archived stays up (operator deliberately unmanaged, GH #282 precedent)", "archived", "", FleetStatusUp, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotStatus, gotReason := deriveFleetStatus(&up, &fast, tc.connectionState, tc.disconnectedReason)
			if gotStatus != tc.wantStatus {
				t.Errorf("status = %q, want %q", gotStatus, tc.wantStatus)
			}
			if gotReason != tc.wantReason {
				t.Errorf("reason = %q, want %q", gotReason, tc.wantReason)
			}
		})
	}
}

// TestDeriveFleetStatus_DisconnectedReasonDisambiguation is the GH #291
// follow-up regression test: a "disconnected" site is reached by two
// completely different, equally legitimate paths (internal/site/sweeper.go
// Sweep vs connService.RecordLastWillTenant), and only the sweeper's own
// paths may raise the alarming Degraded chip. A clean operator-initiated
// last-will disconnect (deactivate/uninstall, or the handler's
// "user_initiated" default) must render exactly like a healthy "connected"
// site — no false alarm on a site that was cleanly taken offline on purpose.
func TestDeriveFleetStatus_DisconnectedReasonDisambiguation(t *testing.T) {
	up := true
	fast := 100.0

	cases := []struct {
		name               string
		disconnectedReason string
		wantStatus         FleetSiteStatus
		wantReason         string
	}{
		// Sweeper-authored reasons (internal/site/sweeper.go Sweep): real,
		// CP-observed unreachability. Must alarm.
		{"sweeper active-verify failure", "agent_unreachable", FleetStatusDegraded, FleetReasonAgentUnreachable},
		{"sweeper passive heartbeat timeout", "heartbeat_timeout", FleetStatusDegraded, FleetReasonAgentUnreachable},
		// Signed agent last-will (ADR-040): the operator chose to stop the
		// agent. The site is healthy and must NOT alarm.
		{"last-will deactivated", "deactivated", FleetStatusUp, ""},
		{"last-will uninstalled", "uninstalled", FleetStatusUp, ""},
		{"last-will default reason", "user_initiated", FleetStatusUp, ""},
		// Conservative fallback: any value that is not positively attributable
		// to the sweeper (unrecognized text, or a legacy/empty row that
		// predates the disconnected_reason column) must NOT alarm either — the
		// data cannot prove the site is unhealthy, so it must not claim it is.
		{"unrecognized reason string", "something_unexpected", FleetStatusUp, ""},
		{"empty/legacy reason", "", FleetStatusUp, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotStatus, gotReason := deriveFleetStatus(&up, &fast, "disconnected", tc.disconnectedReason)
			if gotStatus != tc.wantStatus {
				t.Errorf("disconnected_reason=%q: status = %q, want %q", tc.disconnectedReason, gotStatus, tc.wantStatus)
			}
			if gotReason != tc.wantReason {
				t.Errorf("disconnected_reason=%q: reason = %q, want %q", tc.disconnectedReason, gotReason, tc.wantReason)
			}
		})
	}
}

// TestDeriveFleetStatus_DownAndUnknownIgnoreConnectionState proves that
// up=false and up=nil are decided purely by the probe result, regardless of
// connection_state or disconnected_reason, and never carry a status_reason
// (down/unknown are self-explanatory).
func TestDeriveFleetStatus_DownAndUnknownIgnoreConnectionState(t *testing.T) {
	down := false
	fast := 100.0

	for _, cs := range []string{"connected", "degraded", "disconnected", "revoked", "archived", ""} {
		status, reason := deriveFleetStatus(&down, &fast, cs, "agent_unreachable")
		if status != FleetStatusDown {
			t.Errorf("connection_state=%q: status = %q, want down", cs, status)
		}
		if reason != "" {
			t.Errorf("connection_state=%q: reason = %q, want empty", cs, reason)
		}
	}

	for _, cs := range []string{"connected", "degraded", "disconnected", "revoked", "archived", ""} {
		status, reason := deriveFleetStatus(nil, nil, cs, "agent_unreachable")
		if status != FleetStatusUnknown {
			t.Errorf("connection_state=%q: status = %q, want unknown", cs, status)
		}
		if reason != "" {
			t.Errorf("connection_state=%q: reason = %q, want empty", cs, reason)
		}
	}
}

// TestDeriveFleetStatus_SlowResponseReason proves the pre-existing latency
// threshold path still derives Degraded, now with a distinct status_reason
// from the connection-state-driven cases, when connection_state is healthy.
func TestDeriveFleetStatus_SlowResponseReason(t *testing.T) {
	up := true
	slow := slowThresholdMs + 1

	status, reason := deriveFleetStatus(&up, &slow, "connected", "")
	if status != FleetStatusDegraded {
		t.Fatalf("status = %q, want degraded", status)
	}
	if reason != FleetReasonSlowResponse {
		t.Fatalf("reason = %q, want %q", reason, FleetReasonSlowResponse)
	}
}
