package agentcmd

import (
	"encoding/json"
	"testing"
	"time"
)

// SHARED GOLDEN VECTOR for the agent self-update beat 1 (ARM) wire contract.
//
// This is the Go half. The PHP half is
// apps/agent/tests/AgentSelfUpdateWireContractTest.php and pins the SAME
// literals. The redundancy is the point: the three defects this vector was
// written for were status and cron_mode strings that one side believed in and
// the other never emitted, and every one of them was invisible to both suites,
// because each suite only ever exercised its own half's fiction. A vector each
// side pins INDEPENDENTLY is what turns a one-sided edit into a failing suite
// on the side that made it.
//
// THE CONTRACT, authoritative.
//
// status, exactly five strings:
//
//	"scheduled"          verified and staged, cron event spawned. Carries
//	                     to_version and expires_at.
//	"up_to_date"         the verified manifest offers nothing newer than what
//	                     is installed.
//	"not_eligible"       this build cannot self-update (wordpress.org build) or
//	                     the site is not enrolled.
//	"already_scheduled"  a staged record from a previous arm is still live.
//	                     Carries to_version and expires_at. NOT a failure.
//	"error"              the arm failed. Carries a human-readable detail.
//
// cron_mode, exactly two strings:
//
//	"loopback"   WordPress loopback cron is available.
//	"external"   loopback cron is unavailable (DISABLE_WP_CRON or equivalent);
//	             the site relies on system cron.
//
// "failed", "disabled" and "alternate" are NOT part of this contract. The agent
// has never emitted any of them from beat 1.

// goldenSelfUpdateStatuses is THE status vector, sorted so the comparison below
// reads as equality rather than as a subset test.
var goldenSelfUpdateStatuses = []string{
	"already_scheduled",
	"error",
	"not_eligible",
	"scheduled",
	"up_to_date",
}

// goldenSelfUpdateCronModes is THE cron_mode vector. CronModeUnknown is
// deliberately absent: it is the zero value the control plane sees when the
// field is omitted, never a value the agent sends.
var goldenSelfUpdateCronModes = []string{
	"external",
	"loopback",
}

// neverEmitted are strings a previous version of this contract declared and the
// agent never sent. Pinned so re-introducing one fails loudly instead of
// quietly resurrecting the mismatch.
var neverEmitted = []string{"failed", "disabled", "alternate"}

// TestAgentSelfUpdateWireContractGoldenVector pins the literals. If the
// contract is ever widened or narrowed, this is the assertion that has to be
// changed deliberately, in the same commit as its PHP counterpart.
func TestAgentSelfUpdateWireContractGoldenVector(t *testing.T) {
	declaredStatuses := []string{
		SelfUpdateAlreadyScheduled,
		SelfUpdateError,
		SelfUpdateNotEligible,
		SelfUpdateScheduled,
		SelfUpdateUpToDate,
	}
	if len(declaredStatuses) != len(goldenSelfUpdateStatuses) {
		t.Fatalf("the contract declares %d statuses, the golden vector has %d",
			len(declaredStatuses), len(goldenSelfUpdateStatuses))
	}
	for i, want := range goldenSelfUpdateStatuses {
		if declaredStatuses[i] != want {
			t.Fatalf("status %d = %q, want %q", i, declaredStatuses[i], want)
		}
	}

	declaredCronModes := []string{CronModeExternal, CronModeLoopback}
	if len(declaredCronModes) != len(goldenSelfUpdateCronModes) {
		t.Fatalf("the contract declares %d cron modes, the golden vector has %d",
			len(declaredCronModes), len(goldenSelfUpdateCronModes))
	}
	for i, want := range goldenSelfUpdateCronModes {
		if declaredCronModes[i] != want {
			t.Fatalf("cron mode %d = %q, want %q", i, declaredCronModes[i], want)
		}
	}

	if CronModeUnknown != "" {
		t.Fatalf("CronModeUnknown = %q, want the empty zero value: it is the absent field, not a wire value", CronModeUnknown)
	}

	for _, ghost := range neverEmitted {
		for _, s := range declaredStatuses {
			if s == ghost {
				t.Fatalf("%q is not a beat 1 status: it was removed from the contract because the agent never emitted it", ghost)
			}
		}
		for _, m := range declaredCronModes {
			if m == ghost {
				t.Fatalf("%q is not a beat 1 cron mode: it was removed from the contract because the agent never emitted it", ghost)
			}
		}
	}
}

// TestAgentSelfUpdateResponseDecodesTheWireShape pins the JSON field names
// against a payload written by hand from the agent's own documented shape (see
// the header of apps/agent/includes/commands/class-agent-self-update-command.php).
// A renamed tag here would leave the control plane silently reading zero
// values out of a perfectly good response.
func TestAgentSelfUpdateResponseDecodesTheWireShape(t *testing.T) {
	const payload = `{
		"ok": true,
		"status": "already_scheduled",
		"from_version": "0.61.88",
		"to_version": "0.62.0",
		"detail": "an upgrade to 0.62.0 is already staged",
		"cron_mode": "external",
		"expires_at": 1780000000
	}`

	var got AgentSelfUpdateResponse
	if err := json.Unmarshal([]byte(payload), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Status != SelfUpdateAlreadyScheduled {
		t.Fatalf("status = %q", got.Status)
	}
	if got.FromVersion != "0.61.88" {
		t.Fatalf("from_version = %q", got.FromVersion)
	}
	if got.ToVersion != "0.62.0" {
		t.Fatalf("to_version = %q", got.ToVersion)
	}
	if got.CronMode != CronModeExternal {
		t.Fatalf("cron_mode = %q", got.CronMode)
	}
	if got.Detail == "" {
		t.Fatal("detail was dropped: it is the whole diagnostic value of an error, and the operator-facing note of every other status")
	}
	if got.ExpiresAt != 1780000000 {
		t.Fatalf("expires_at = %d: a staged record whose expiry the control plane cannot read cannot be checked against its own deadline", got.ExpiresAt)
	}

	// An agent that omits cron_mode must decode to the zero value, which the
	// control plane treats as the narrow (honest) window.
	var minimal AgentSelfUpdateResponse
	if err := json.Unmarshal([]byte(`{"status":"up_to_date","from_version":"0.62.0"}`), &minimal); err != nil {
		t.Fatalf("unmarshal minimal: %v", err)
	}
	if minimal.CronMode != CronModeUnknown {
		t.Fatalf("an omitted cron_mode must decode to CronModeUnknown, got %q", minimal.CronMode)
	}
	if minimal.ExpiresAt != 0 {
		t.Fatalf("an omitted expires_at must decode to 0, got %d", minimal.ExpiresAt)
	}
}

// TestStagedTTLFloorExceedsEveryConfirmDeadline is the timing half of the
// contract, stated where the constant lives. The staged record must outlive the
// control plane's patience: a stage that lapses first means beat 2 finds
// nothing to apply, and the site false-fails a build it was never given a
// chance to install. The agent holds a stage for 7200s
// (UpdateChecker::STAGED_TTL_SECONDS); the CP's longest window is 90 minutes
// (update.agentConfirmDeadlineExternalCron), and that side pins the same
// inequality from its own constants.
func TestStagedTTLFloorExceedsEveryConfirmDeadline(t *testing.T) {
	const cpLongestConfirmDeadline = 90 * time.Minute
	if SelfUpdateStagedTTLFloor <= cpLongestConfirmDeadline {
		t.Fatalf("SelfUpdateStagedTTLFloor = %s, must exceed the control plane's longest confirm deadline (%s)",
			SelfUpdateStagedTTLFloor, cpLongestConfirmDeadline)
	}
	if headroom := SelfUpdateStagedTTLFloor - cpLongestConfirmDeadline; headroom < 15*time.Minute {
		t.Fatalf("headroom = %s: raise the agent's staged TTL first, then the control plane window, and keep the margin", headroom)
	}
}
