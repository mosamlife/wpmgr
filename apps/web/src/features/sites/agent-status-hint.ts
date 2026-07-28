import { AGENT_STATUS_LABEL } from "@/components/status";
import type { FleetAgentVersions } from "@wpmgr/api";

/**
 * GH #255: an axis that has collapsed to the single "Unknown" bucket toggles
 * a checkbox that matches every visible site, which reads as a dead filter
 * ("doesn't seem to trigger or activate properly"). That is not a second bug
 * on top of the reporter's other complaint: it is the same root cause (no
 * reference agent version to classify against) wearing a different hat.
 * Rather than leave the operator to guess, name it.
 *
 * Returns `undefined` for every other shape of the options list. A single
 * non-"Unknown" value, or several values, is ordinary and needs no callout.
 */
export function agentStatusFilterHint(
  agentStatusOptions: readonly string[],
  referenceSource: FleetAgentVersions["reference_source"] | undefined,
): string | undefined {
  if (
    agentStatusOptions.length !== 1 ||
    agentStatusOptions[0] !== AGENT_STATUS_LABEL.unknown
  ) {
    return undefined;
  }
  return referenceSource === "none"
    ? "Every site here is Unknown because WPMgr has no reference agent version to compare against, so this filter cannot narrow the list."
    : "Every site here is Unknown, so this filter cannot narrow the list.";
}
