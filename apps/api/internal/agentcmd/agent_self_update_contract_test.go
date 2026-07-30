package agentcmd

import (
	"encoding/json"
	"testing"
)

// SHARED GOLDEN VECTOR for the agent self-update beat 1 (ARM then APPLY) wire
// contract.
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
//	"scheduled"          verified; this request's own tail is registered to
//	                     apply it once the response is released. Carries
//	                     to_version and expires_at.
//	"up_to_date"         the verified manifest offers nothing newer than what
//	                     is installed.
//	"not_eligible"       this build cannot self-update (wordpress.org build) or
//	                     the site is not enrolled.
//	"already_scheduled"  another apply for this site already holds the
//	                     upgrader lock. Carries to_version and expires_at.
//	                     NOT a failure.
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
		"expires_at": 1780000000,
		"apply_id": "9f1c2e3a4b5d6e7f"
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
		t.Fatalf("expires_at = %d: dropping this field silently loses the (informational) expiry of the apply lock the agent reported", got.ExpiresAt)
	}
	if got.ApplyID != "9f1c2e3a4b5d6e7f" {
		t.Fatalf("apply_id = %q: dropping this field silently defeats every attribution check downstream", got.ApplyID)
	}

	// An agent that omits cron_mode must decode to the zero value, which the
	// control plane treats as the narrow (honest) window. An agent that
	// predates apply ids (every agent before 0.61.108) must decode ApplyID to
	// "", which is what keeps beat 2 confirming on version movement alone for
	// that agent rather than collapsing every one of its confirmations to
	// unattributed.
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
	if minimal.ApplyID != "" {
		t.Fatalf("an omitted apply_id must decode to the empty string, got %q", minimal.ApplyID)
	}
}
