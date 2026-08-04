import { Bot } from "lucide-react";
import { Link } from "@tanstack/react-router";

import { AGENT_STATUS_LABEL, AgentStatusChip } from "@/components/status";
import { VersionArrow } from "@/components/shared/version-arrow";
import { AGENT_FLEET_NOTE_BODY } from "@/features/sites/agent-column-header";

import type { SiteAgentUpdate } from "./use-site-agent-update";

// AgentUpdateNotice (GH #314) - the site Updates tab's line for the one
// component WPMgr does NOT update for you.
//
// Reported against 0.61.102: the tab said "All up to date" with a green check
// while the same site's wp-admin was offering a WPMgr agent update. Both were
// telling the truth about different things. The agent is stripped from the
// managed update list on purpose (shipped 0.61.97), and wp-admin offering it
// is the agent's own self-update channel doing its job. What was missing was
// anything on this tab admitting the agent exists.
//
// HARD RULE, do not weaken: this line is presentational only. It renders no
// checkbox, is not part of any selection set, and carries no control that can
// enqueue an update run. An agent update applied through the ordinary plugin
// path is the plugin overwriting its own running files inside the request that
// has to report the outcome, and the snapshot plus rollback that protects
// every other update deliberately does not arm for the agent's own directory,
// because whatever would perform the rollback is what is being replaced. The
// only affordance here is a LINK to the dedicated channel (see #255), never a
// trigger.
//
// HONESTY RULE: never imply the agent is current from the fact that the
// managed components are. Every branch below states only what the control
// plane's own classification actually established, and the fleet-derived
// caveat is reused verbatim from the Sites page (AGENT_FLEET_NOTE_BODY)
// rather than reworded into a second phrasing of the same warning.

/**
 * Why the agent can never join the list above. Stated on every branch,
 * including the ones where the agent is perfectly current, because the point
 * of this line is the boundary, not the version.
 */
const NOT_UPDATED_HERE =
  "The agent is never part of an update run started here. It would be overwriting its own running files, and the snapshot and rollback that protect every other update deliberately do not cover its own directory.";

/** What to do when the fleet-wide channel is not available to this operator. */
const WP_ADMIN_FALLBACK =
  "Update it from this site's own Plugins screen in WordPress admin, where the agent offers its own update.";

/** How to reach the dedicated channel, which lives on the Sites page. */
const CHANNEL_STEPS =
  "Select this site on the Sites page, then use Update WPMgr agent.";

export interface AgentUpdateNoticeProps {
  /**
   * This site's agent freshness (see useSiteAgentUpdate). `null` renders
   * nothing at all: the rollup has not loaded, was refused, or has no row for
   * this site, and a line asserting anything in that state would be a guess.
   */
  agent: SiteAgentUpdate | null;
}

export function AgentUpdateNotice({ agent }: AgentUpdateNoticeProps) {
  if (!agent) return null;

  const { status, version, latestVersion, referenceSource, channelAvailable } =
    agent;
  const outdated = status === "outdated";
  // The channel is offered for exactly one case. "ineligible" is the
  // wordpress.org build, which ships with no self-updater at all, so sending
  // that operator to a channel their build cannot consume would be advice
  // that silently does nothing.
  const offerChannel = outdated && channelAvailable;

  return (
    <section
      data-testid="agent-update-notice"
      aria-labelledby="agent-update-notice-heading"
      className="space-y-2 rounded-md border border-dashed border-[var(--color-border)] p-3"
    >
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div className="flex min-w-0 flex-wrap items-center gap-2">
          <Bot
            aria-hidden="true"
            className="size-4 shrink-0 text-[var(--color-muted-foreground)]"
          />
          <h4
            id="agent-update-notice-heading"
            className="text-sm font-medium text-[var(--color-foreground)]"
          >
            WPMgr agent
          </h4>
          <AgentStatusChip status={status} referenceSource={referenceSource} />
          {outdated ? (
            <VersionArrow from={version} to={latestVersion} />
          ) : version ? (
            <span className="font-mono text-xs tabular-nums text-[var(--color-muted-foreground)]">
              {version}
            </span>
          ) : null}
        </div>
        {offerChannel ? (
          <Link
            to="/sites"
            search={{ agentStatus: [AGENT_STATUS_LABEL.outdated] }}
            className="text-sm font-medium text-primary underline-offset-4 hover:underline"
          >
            Open agent updates
          </Link>
        ) : null}
      </div>

      <div className="space-y-1 text-xs leading-relaxed text-[var(--color-muted-foreground)]">
        <StatusLine
          status={status}
          latestVersion={latestVersion}
          referenceSource={referenceSource}
        />
        <p>{NOT_UPDATED_HERE}</p>
        {outdated ? (
          <p>{channelAvailable ? CHANNEL_STEPS : WP_ADMIN_FALLBACK}</p>
        ) : null}
      </div>
    </section>
  );
}

/**
 * What the control plane's classification actually established, and nothing
 * more.
 *
 * The two "unknown" wordings are distinguished by `referenceSource`, not by a
 * second field: when the source is "published" or "fleet" the reference
 * version is well-formed by construction, so the unreadable side must be this
 * site's own reported version. When it is "none" there was no reference at
 * all. Both say the same thing about currency, which is that WPMgr does not
 * know, and neither one guesses at "behind": a false "behind" on a site that
 * is fine is worse than saying nothing, and staying silent while the heading
 * above says everything is up to date is what caused this defect in the first
 * place.
 */
function StatusLine({
  status,
  latestVersion,
  referenceSource,
}: {
  status: SiteAgentUpdate["status"];
  latestVersion: string;
  referenceSource: SiteAgentUpdate["referenceSource"];
}) {
  switch (status) {
    case "outdated":
      return referenceSource === "fleet" ? (
        <p>
          Another site in this fleet already reports agent version{" "}
          <span className="font-mono tabular-nums">{latestVersion}</span>, so
          this one is behind.
        </p>
      ) : (
        <p>
          A newer published agent version is available for this site.
        </p>
      );
    case "current":
      return referenceSource === "fleet" ? (
        <>
          <p>
            This site runs the newest agent version this fleet has reported.
          </p>
          {/* Reused verbatim from the Sites table's Agent column header so
              the caveat has one wording, not two that can drift apart. */}
          <p>{AGENT_FLEET_NOTE_BODY}</p>
        </>
      ) : (
        <p>This site runs the published agent version.</p>
      );
    case "ineligible":
      return (
        <p>
          This site runs the WordPress plugin directory build of the agent,
          which ships without a self-updater. It updates from the site's own
          Plugins screen, through the plugin directory.
        </p>
      );
    case "unknown":
      return referenceSource === "none" ? (
        <p>
          WPMgr has no reference agent version for this install, so it cannot
          tell whether this agent is behind.
        </p>
      ) : (
        <p>
          This site has not reported a readable agent version, so WPMgr cannot
          tell whether it is behind.
        </p>
      );
  }
}
