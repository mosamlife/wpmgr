// GH #314: resolves ONE site's WPMgr agent freshness for the site Updates
// tab, plus whether the fleet-wide agent update channel is actually reachable
// by the operator looking at it.
//
// WHERE THE "IS THIS SITE'S AGENT BEHIND" DECISION IS MADE: on the control
// plane, not here. GET /api/v1/fleet/agents already classifies every site as
// current | outdated | unknown | ineligible (internal/agentrelease/classify.go),
// against a reference whose provenance it also reports (reference_source), and
// it is the same classification the Sites table's Agent column and the fleet
// summary card on /updates render. This hook only picks this site's row out of
// that rollup. There is deliberately NO client-side version comparison: a
// second implementation of "is 0.61.100 older than 0.61.121" would be a second
// answer that can disagree with the first, and the control plane's version has
// rules this file has no business restating (a malformed version on either
// side is never reported as behind, and the plugin-directory build is settled
// as ineligible before any comparison happens at all).

import { useMemo } from "react";
import type { FleetAgentVersions } from "@wpmgr/api";

import type { AgentStatus } from "@/components/status";
import { canManage, useMe } from "@/features/auth/use-auth";
import { useFleetAgentVersions } from "@/features/fleet/use-fleet-agents";

export interface SiteAgentUpdate {
  /** Control-plane classification for this site (FleetAgentSite.status). */
  status: AgentStatus;
  /**
   * This site's last-reported agent version, verbatim. Empty string when the
   * agent has never reported one, which is distinct from status "unknown"
   * (that also covers a version string too malformed to order).
   */
  version: string;
  /**
   * The single version this site was classified against. Read it together
   * with `referenceSource`: under "fleet" it is the newest agent version this
   * tenant's own fleet has reported, NOT the newest agent that exists, and
   * under "none" it is a placeholder ("unknown") that must never be rendered
   * as though it were a real version.
   */
  latestVersion: string;
  /** Where `latestVersion` came from (FleetAgentVersions.reference_source). */
  referenceSource: FleetAgentVersions["reference_source"];
  /**
   * Whether the fleet-wide agent update channel is available TO THIS
   * OPERATOR right now. Both halves are required, and both are the same
   * conditions that decide whether the "Update WPMgr agent" bulk action is
   * rendered at all on the Sites page (routes/_authed/sites/index.tsx):
   *
   *   1. `self_update_enabled` on the rollup, which is the control plane's
   *      own WPMGR_UPDATE_AGENT_SELF_UPDATE_ENABLED kill switch. Absent
   *      reads as off, matching the channel's ships-dark-by-default contract.
   *   2. canManage(me), owner or admin. The channel is infrastructure, and an
   *      operator without that role cannot see the action even when the
   *      switch is on.
   *
   * Pointing anyone else at that page would be pointing them at an action
   * they cannot see, so the notice tells them what to do instead.
   */
  channelAvailable: boolean;
}

/**
 * This site's agent freshness, or `null` when there is nothing honest to say:
 * the rollup has not loaded, it failed (a site-scoped collaborator gets a 403
 * from GET /api/v1/fleet/agents, exactly as the fleet summary card already
 * handles), or it carries no row for this site. `null` renders no agent line
 * at all, which is the right answer: a guessed line is worse than none.
 */
export function useSiteAgentUpdate(siteId: string): SiteAgentUpdate | null {
  const { data: fleet } = useFleetAgentVersions();
  const { data: me } = useMe();
  const manageAgents = canManage(me);

  return useMemo(() => {
    if (!fleet) return null;
    const entry = fleet.sites.find((s) => s.site_id === siteId);
    if (!entry) return null;
    return {
      status: entry.status,
      version: entry.agent_version,
      latestVersion: fleet.latest_version,
      referenceSource: fleet.reference_source,
      channelAvailable: fleet.self_update_enabled === true && manageAgents,
    };
  }, [fleet, siteId, manageAgents]);
}
