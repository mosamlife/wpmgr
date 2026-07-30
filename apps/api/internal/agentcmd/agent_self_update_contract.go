package agentcmd

// This file is the AUTHORITATIVE CP->agent command contract for the agent's
// own self-update channel. The wp-agent-engineer mirrors these shapes in the
// agent's command handler. Field names are JSON wire names; do not rename
// without updating both sides.
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
// request that has to report the outcome. The control plane's own pre-update
// snapshot + automatic rollback that protects every other plugin update
// deliberately does NOT arm for the agent's own directory (whatever performs
// the rollback is what is being replaced), and per-site filesystem access to
// recover by hand is not recovery at all across a fleet. The control plane
// therefore refuses the agent as an update target outright (see
// internal/agentplugin) and ships agent upgrades over this separate protocol
// instead.
//
// THE PROTOCOL, TWO BEATS
// ------------------------
// BEAT 1 ARM THEN APPLY, both inside this one command request. The agent
//   refuses when it has no self-updater at all (the wordpress.org build,
//   which ships without it) or the site is not enrolled, with status
//   "not_eligible"; otherwise it fetches and FULLY verifies a release
//   manifest through its existing verification chain (signature,
//   cmd/slug/aud, iat/exp/jti replay, monotonic-iat, downgrade guard, https +
//   host allowlist + size cap). Nothing newer than what is installed yields
//   "up_to_date". Otherwise the agent takes WordPress core's own
//   cross-request upgrader lock (WP_Upgrader::create_lock); if another apply
//   already holds it, this arm answers "already_scheduled" naming that
//   apply's own id and expiry rather than starting a second one. Otherwise it
//   mints an apply id, registers itself on core's own rest_pre_serve_request
//   filter, and answers "scheduled". NOTHING on disk has moved when either
//   acknowledgement is returned.
//
//   The response is then written and the connection released (the same
//   fastcgi_finish_request / litespeed_finish_request detach the agent's
//   other long-running jobs use) BEFORE anything is touched. Only once the
//   connection is confirmed detached does this SAME request continue: it
//   forces wp_doing_cron() true for the duration, which is what lets core's
//   unmodified Plugin_Upgrader run the swap without deactivating the plugin
//   and with its own maintenance-mode window covering the destructive part,
//   then runs the upgrade through core's ordinary upgrader machinery. A SAPI
//   that cannot detach the connection (neither of the two functions above)
//   refuses to run the upgrade at all rather than run it on a connection
//   something else can time out mid-swap; nothing on disk moves on that path,
//   and the site is told to update itself from its dashboard instead. If the
//   acknowledgement itself cannot be written, the apply is abandoned before
//   anything is touched, and an un-upgraded site simply re-arms on its next
//   command.
//
//   This position, the tail of the SAME request that answered, is what makes
//   a failed swap survivable. WP_Upgrader::run() registers core's own
//   restore_temp_backup on the 'shutdown' hook at priority 10, and
//   delete_temp_backup at 100, the moment install_package() reports failure.
//   Running the swap from here means those registrations land in an ordinary,
//   not-yet-started dispatch of 'shutdown' and actually fire. An apply that
//   instead runs from a callback already registered on 'shutdown' sits past
//   that point and past WP_Hook's own reentrancy guard, so neither
//   registration is ever reached and a failed swap has no rollback at all.
//   Beat 1 registers its own restorer, gated on core's backup still being
//   present and the plugin directory genuinely missing, as a second line of
//   defence for the one failure shape that skips even that hook: a fatal
//   raised inside install_package() itself, before core's shutdown callbacks
//   are registered at all.
//
// BEAT 2 CONFIRM, the ONLY trustworthy success signal is the NEW code phoning
//   home: a signed metadata push carrying the new agent_version, which the CP
//   observes on sites.agent_version. The new build's first boot pushes this
//   immediately (bound on plugins_loaded, deferred to 'shutdown' so that one
//   request never pays the control plane's latency); a periodic metadata
//   heartbeat is the backstop for a site that gets no organic traffic before
//   the control plane's own confirmation window runs out. A "scheduled" (or
//   "already_scheduled") acknowledgement from beat 1 is NEVER success (see
//   update.AgentConfirmWorker): the swap that follows it can still fail, and
//   when it does, the failure reaches the control plane only through the
//   agent's own apply-outcome record, replayed on the next metadata push (see
//   AgentApplyResult in the update package), never through this response.

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
	// SelfUpdateScheduled: the manifest verified and this request's own tail
	// is registered to run the apply once the acknowledgement is released.
	// This is an ACKNOWLEDGEMENT, not a success: nothing has moved yet, the
	// connection may fail to detach, and the swap itself may still fail; none
	// of those shapes rides on this response (see the protocol notes above).
	// Carries ToVersion and ExpiresAt.
	SelfUpdateScheduled = "scheduled"
	// SelfUpdateAlreadyScheduled: another apply for this site currently holds
	// core's own cross-request upgrader lock, most likely the tail of an
	// earlier arm's request still running its swap in the background. It
	// carries the same ToVersion and ExpiresAt that lock holder reported, and
	// means the earlier arm is the one whose outcome will land. It is NOT a
	// failure and must never halt a wave: treated exactly like
	// SelfUpdateScheduled.
	SelfUpdateAlreadyScheduled = "already_scheduled"
	// SelfUpdateUpToDate: the agent's verified manifest offers nothing newer
	// than what is installed. Nothing was applied. See the control plane's
	// handling (update.Worker.upToDate): this is NEVER an unqualified success,
	// because the CP only creates a task for a site it already classified as
	// outdated, so this answer means the two disagree.
	SelfUpdateUpToDate = "up_to_date"
	// SelfUpdateNotEligible: this build cannot self-update. Three reasons reach
	// it: the wordpress.org build ships without a self-updater and is upgraded
	// by the directory; the site is not enrolled; or the site's PHP setup cannot
	// release the response before doing further work, so the upgrade would run
	// against a connection the caller is still timing. The last one is decided
	// at arm time on purpose, so such a site answers immediately instead of
	// consuming a whole confirmation window and then failing its wave. Carries a
	// Detail naming which. The channel does not apply to this site at all.
	SelfUpdateNotEligible = "not_eligible"
	// SelfUpdateError: the arm failed, manifest fetch/verification failed, or
	// the site could not take the upgrade seam (the WordPress filter API was
	// unavailable, or the upgrader lock could not be honoured). Nothing was
	// applied. Carries a human-readable Detail, which the control plane must
	// preserve on the task rather than recording the bare status.
	SelfUpdateError = "error"
)

// WP-Cron modes the agent reports alongside an ARM acknowledgement. Exactly
// two strings ride the wire. This no longer says how beat 1's apply is
// dispatched: it isn't, the apply runs inline, in the tail of the same
// request that answered, whatever this value is. What it still says is how
// reliably beat 2's own periodic metadata heartbeat reaches the control plane
// on a site where no ordinary page load boots the new code first, which is
// why the control plane still widens its confirmation window for the value
// below that means that heartbeat is less certain.
const (
	// CronModeLoopback: WordPress loopback cron is available, so the periodic
	// metadata heartbeat that can confirm an otherwise-quiet site runs on
	// WordPress's own schedule.
	CronModeLoopback = "loopback"
	// CronModeExternal: loopback cron is unavailable (DISABLE_WP_CRON or
	// equivalent), so that periodic heartbeat depends on an external system
	// scheduler hitting wp-cron.php on its own period, commonly every 15
	// minutes but sometimes hourly, which the control plane cannot see.
	CronModeExternal = "external"
	// CronModeUnknown is NOT a wire value: it is the zero value the CP sees
	// when an agent omits the field. Treated exactly like CronModeLoopback for
	// the deadline, the narrow window is the honest default when the CP has no
	// evidence the site needs a wider one.
	CronModeUnknown = ""
)

// AgentSelfUpdateResponse is the agent's response to the `agent_self_update`
// command (beat 1, ARM).
//
//	status        one of the SelfUpdate* constants above.
//	from_version  the agent version installed right now.
//	to_version    the version the verified manifest points at. Set when status
//	              is "scheduled" or "already_scheduled" (what this apply will
//	              install, or is already installing); empty otherwise.
//	cron_mode     one of the CronMode* constants; see the const block above,
//	              it now speaks to beat 2's confirmation reliability, not to
//	              whether or how beat 1's apply runs.
//	detail        short human-readable note, surfaced verbatim to the operator
//	              on the update task. Carries the refusal reason for
//	              "not_eligible" and the failure reason for "error".
//	expires_at    Unix seconds at which core's own cross-request upgrader lock
//	              (held for the duration of the apply) self-expires. Set when
//	              status is "scheduled" or "already_scheduled"; 0 otherwise.
//	              Purely informational: nothing on the control plane reads or
//	              checks it, because by the time this response is on the wire
//	              the apply it describes is already under way, inline, in the
//	              same request.
//	apply_id      opaque per-apply identifier the agent mints when it takes
//	              the upgrader lock and stamps into the outcome record it
//	              stores for that apply. Additive: empty from an agent that
//	              predates this field. Carried unmodified onto
//	              AgentConfirmArgs so beat 2 can tell "this run's own apply
//	              installed what the site is now running" apart from "the
//	              site's version moved for some other reason". Compared
//	              whole, never parsed; never used as a version or a time. See
//	              update.agentApplyAttributed.
type AgentSelfUpdateResponse struct {
	Status      string `json:"status"`
	FromVersion string `json:"from_version,omitempty"`
	ToVersion   string `json:"to_version,omitempty"`
	CronMode    string `json:"cron_mode,omitempty"`
	Detail      string `json:"detail,omitempty"`
	ExpiresAt   int64  `json:"expires_at,omitempty"`
	ApplyID     string `json:"apply_id,omitempty"`
}
