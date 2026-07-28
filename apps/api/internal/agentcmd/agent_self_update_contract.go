package agentcmd

import "time"

// This file is the AUTHORITATIVE CP->agent command contract for the agent's
// own self-update channel (Phase 2). The wp-agent-engineer mirrors these
// shapes in the agent's command handler. Field names are JSON wire names; do
// not rename without updating both sides.
//
// Transport, agent_self_update:
//   POST {site_url}/wp-json/wpmgr/v1/command/agent_self_update
//   Header: Authorization: Bearer <minted EdDSA JWT>
//           cmd="agent_self_update", aud=<siteId>
//   Body:   application/json, AgentSelfUpdateRequest below.
//   Response: 200 with AgentSelfUpdateResponse; a non-200 is a transport-level
//             failure the CP records as a failed task.
//
// WHY THIS IS NOT AN `update` COMMAND WITH type="plugin"
// -----------------------------------------------------
// Applying a normal plugin update to the agent means the plugin overwrites its
// own files while its code is the code performing the update, inside the very
// request that has to report the outcome. If it dies partway there is no
// working code left to report it, and the snapshot + automatic rollback that
// protects every other plugin update deliberately does NOT arm for the agent's
// own directory (whatever performs the rollback is what is being replaced).
// Recovery would need per-site filesystem access, which across a fleet is not
// recovery at all. The control plane therefore refuses the agent as an update
// target outright (see internal/agentplugin) and ships agent upgrades over
// this separate three-beat protocol instead.
//
// THE THREE-BEAT PROTOCOL
// -----------------------
// BEAT 1 ARM, this command. NOTHING on disk moves. The agent refuses when it
//   has no self-updater at all (the wordpress.org build, which ships without
//   it) or the site is not enrolled, with status "not_eligible"; otherwise it
//   fetches and FULLY verifies a release manifest through its existing
//   verification chain (signature, cmd/slug/aud, iat/exp/jti replay,
//   monotonic-iat, downgrade guard, https + host allowlist + size cap) before
//   anything is staged. Nothing newer than what is installed yields
//   "up_to_date". A staged record from an earlier arm that is still live yields
//   "already_scheduled". Otherwise the agent writes a small staged record with
//   an expiry, schedules a single cron event, spawns cron, and answers
//   "scheduled". An arm that could not complete answers "error".
// BEAT 2 APPLY, a SEPARATE WordPress bootstrap (the cron request) runs the
//   upgrade. No CP response rides on that request, so a mid-copy death takes
//   nothing down with it. There is no new download/verify/unzip code: the
//   agent's existing upgrader hooks engage unchanged.
// BEAT 3 CONFIRM, the ONLY trustworthy success signal is the NEW code phoning
//   home: a signed metadata push carrying the new agent_version, which the CP
//   observes on sites.agent_version. A "scheduled" acknowledgement is NEVER
//   success (see update.AgentConfirmWorker). A site whose loopback cron is
//   broken simply never reaches beat 2, which is SAFE, nothing was touched, //   and must surface as unconfirmed, never as a silent success.

// AgentSelfUpdateRequest is the POST body for the `agent_self_update` command
// (beat 1, ARM). It carries no parameters on purpose: the agent resolves and
// verifies the target build from its own release manifest, and the control
// plane never names a version. A CP-supplied version would be a second,
// unverified source of truth for the one upgrade whose integrity chain must
// not have one, and the agent's downgrade guard would refuse an older build
// anyway. The struct is reserved for future flags.
type AgentSelfUpdateRequest struct{}

// Agent self-update ARM outcomes (agent -> CP). These five strings are the
// WHOLE set: the agent emits nothing else, and the control plane declares
// nothing else. A status the CP declares but the agent never sends is worse
// than useless, it is a branch that looks handled and is dead, while the
// status the agent DOES send falls through to a default that discards its
// detail. Both halves pin these literals in a golden-vector test
// (TestAgentSelfUpdateWireContractGoldenVector here,
// AgentSelfUpdateWireContractTest on the agent side) so an edit to either half
// fails its own suite.
const (
	// SelfUpdateScheduled: the manifest verified, a newer build is staged, the
	// cron event is scheduled and cron was spawned. This is an ACKNOWLEDGEMENT,
	// not a success: nothing has been applied yet and beat 2 may never run.
	// Carries ToVersion and ExpiresAt.
	SelfUpdateScheduled = "scheduled"
	// SelfUpdateAlreadyScheduled: a staged record from a PREVIOUS arm is still
	// live, so this arm did not need to stage anything. It carries the same
	// ToVersion and ExpiresAt as the arm that staged it, and means the earlier
	// arm succeeded and its upgrade is still pending. It is NOT a failure and
	// must never halt a wave: treated exactly like SelfUpdateScheduled.
	SelfUpdateAlreadyScheduled = "already_scheduled"
	// SelfUpdateUpToDate: the agent's verified manifest offers nothing newer
	// than what is installed. Nothing was staged. See the control plane's
	// handling (update.Worker.upToDate): this is NEVER an unqualified success,
	// because the CP only creates a task for a site it already classified as
	// outdated, so this answer means the two disagree.
	SelfUpdateUpToDate = "up_to_date"
	// SelfUpdateNotEligible: this build cannot self-update (the wordpress.org
	// build ships without a self-updater and is upgraded by the directory) or
	// the site is not enrolled. The channel does not apply to this site at all.
	SelfUpdateNotEligible = "not_eligible"
	// SelfUpdateError: the arm failed, manifest fetch/verification failed, the
	// staged record could not be written, or cron could not be scheduled.
	// Nothing was applied. Carries a human-readable Detail, which the control
	// plane must preserve on the task rather than recording the bare status.
	SelfUpdateError = "error"
)

// WP-Cron modes the agent reports alongside an ARM acknowledgement. Exactly
// two strings ride the wire. The value tells the control plane how long to wait
// for beat 3: a site without loopback cron depends on an external scheduler
// whose period the CP cannot see, so it is given a much wider confirmation
// deadline rather than being declared unconfirmed while its upgrade is still
// legitimately queued.
const (
	// CronModeLoopback: WordPress loopback cron is available, and the agent
	// spawned it in beat 1. The cron request normally lands within seconds.
	CronModeLoopback = "loopback"
	// CronModeExternal: loopback cron is unavailable (DISABLE_WP_CRON or
	// equivalent), so nothing the agent does can make the cron request happen;
	// the site relies on a system scheduler running wp-cron.php on its own
	// period, commonly every 15 minutes but sometimes hourly.
	CronModeExternal = "external"
	// CronModeUnknown is NOT a wire value: it is the zero value the CP sees
	// when an agent omits the field. Treated exactly like CronModeLoopback for
	// the deadline, the narrow window is the honest default when the CP has no
	// evidence the site needs a wider one.
	CronModeUnknown = ""
)

// SelfUpdateStagedTTLFloor is the timing contract between the two halves, and
// it is load-bearing: the agent's staged record MUST outlive the control
// plane's patience. If the record expires first, a slow site expires its own
// stage, beat 2 finds nothing to apply, and the canary false-fails, halting a
// rollout that was fine. So the agent's staged TTL has to comfortably exceed
// the CP's LONGEST confirmation deadline (the external-cron window), and the CP
// may never widen a deadline past this floor. A staged TTL shorter than the CP
// deadline is a defect on the agent side; a CP deadline longer than this floor
// is a defect on this side. TestStagedTTLFloorExceedsEveryConfirmDeadline and
// update.TestConfirmDeadlinesFitTheStagedTTLFloor pin the two halves of that
// inequality.
const SelfUpdateStagedTTLFloor = 2 * time.Hour

// AgentSelfUpdateResponse is the agent's response to the `agent_self_update`
// command (beat 1, ARM).
//
//	status        one of the SelfUpdate* constants above.
//	from_version  the agent version installed right now.
//	to_version    the version the verified manifest points at. Set when status
//	              is "scheduled" or "already_scheduled" (what beat 2 will
//	              install); empty otherwise.
//	cron_mode     one of the CronMode* constants; how beat 2 will be reached.
//	detail        short human-readable note, surfaced verbatim to the operator
//	              on the update task. Carries the refusal reason for
//	              "not_eligible" and the failure reason for "error".
//	expires_at    Unix seconds at which the staged record lapses. Set when
//	              status is "scheduled" or "already_scheduled"; 0 otherwise.
//	              See SelfUpdateStagedTTLFloor: it must sit beyond the
//	              confirmation deadline the CP is about to set.
type AgentSelfUpdateResponse struct {
	Status      string `json:"status"`
	FromVersion string `json:"from_version,omitempty"`
	ToVersion   string `json:"to_version,omitempty"`
	CronMode    string `json:"cron_mode,omitempty"`
	Detail      string `json:"detail,omitempty"`
	ExpiresAt   int64  `json:"expires_at,omitempty"`
}
