// AgentFleetSummaryCard: compact fleet-wide WPMgr agent freshness summary
// for the Updates page. Visibility only: this phase never offers an "update
// agent" action (see update-wizard.tsx, which explicitly excludes the
// agent's own plugin key from selectable update targets); the card's only
// affordance is a link into the Sites page's agent-status filter.

import { Bot } from "lucide-react";
import { Link } from "@tanstack/react-router";

import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { AGENT_STATUS_LABEL } from "@/components/status";
import { useFleetAgentVersions } from "./use-fleet-agents";

export function AgentFleetSummaryCardSkeleton() {
  return (
    <Card>
      <CardHeader>
        <Skeleton className="h-5 w-36" />
      </CardHeader>
      <CardContent>
        <Skeleton className="h-4 w-64" />
      </CardContent>
    </Card>
  );
}

/**
 * "N of M sites on <latest version>, K outdated" when a reference version
 * exists, or a plain-language explanation when it does not (GH #255: a
 * self-hosted install with nothing in its fleet to compare against used to
 * render the literal word "unknown" in place of a version, e.g. "0 of 24
 * sites on unknown, 24 unknown". A version placeholder must never be
 * interpolated into the sentence as if it were a real version).
 *
 * `reference_source` says where `latest_version` came from:
 *   - "published": the release channel manifest. "N of M on <version>" means
 *     current with the newest agent that exists.
 *   - "fleet": no manifest was readable (the normal steady state for a
 *     self-hosted install), so the comparison falls back to the newest
 *     agent_version already reported by a site in this tenant's own fleet.
 *     Rendered with a short qualifier so "current" is never misread as
 *     "up to date with the latest release": it only means "not behind the
 *     newest agent this fleet has seen".
 *   - "none": nothing to compare against, so every site is unknown. Two
 *     causes, deliberately not distinguished on the wire: an install that
 *     has never read a manifest and has no well-formed version anywhere in
 *     its fleet, or an install whose manifest was readable but has been
 *     unreachable past the staleness bound. The second does NOT fall back
 *     to "fleet", so the copy below must not imply this install has no
 *     channel; it says only that no reference version is available.
 *
 * Best-effort: a site-scoped collaborator with no org-level access gets a
 * 403 from GET /api/v1/fleet/agents, which this card treats the same as
 * "nothing to show" (return null) rather than a page-level error on a page
 * whose primary content is update runs, not agent health.
 */
export function AgentFleetSummaryCard() {
  const { data, isPending, isError } = useFleetAgentVersions();

  if (isPending) return <AgentFleetSummaryCardSkeleton />;
  if (isError || !data) return null;

  const { latest_version, reference_source, counts } = data;
  const total = counts.current + counts.outdated + counts.unknown + counts.ineligible;
  if (total === 0) return null;

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-base">
          <Bot
            aria-hidden="true"
            className="size-4 text-[var(--color-muted-foreground)]"
          />
          Agent version
        </CardTitle>
      </CardHeader>
      <CardContent className="flex flex-wrap items-center justify-between gap-3">
        {reference_source === "none" ? (
          <p className="text-sm text-[var(--color-foreground)]">
            WPMgr has no reference agent version for this install, so it
            cannot tell which of your{" "}
            <span className="font-mono tabular-nums">{total}</span> sites are
            behind.
            {counts.ineligible > 0 ? (
              <>
                {" "}
                <span className="font-mono tabular-nums">
                  {counts.ineligible}
                </span>{" "}
                {counts.ineligible === 1 ? "runs" : "run"} a build that cannot
                self-update regardless.
              </>
            ) : null}
          </p>
        ) : (
          <p className="text-sm text-[var(--color-foreground)]">
            <span className="font-mono tabular-nums">{counts.current}</span> of{" "}
            <span className="font-mono tabular-nums">{total}</span> sites on{" "}
            <span className="font-mono">{latest_version}</span>
            {reference_source === "fleet" ? (
              <span className="text-[var(--color-muted-foreground)]">
                {" "}
                (newest seen in this fleet, not a published release)
              </span>
            ) : null}
            {counts.outdated > 0 ? (
              <>
                {", "}
                <span className="font-mono tabular-nums">{counts.outdated}</span>{" "}
                outdated
              </>
            ) : null}
            {/* Name every bucket that the denominator counts. Sites on the
                plugin-directory build can never self-update and sites that
                have not reported a version yet are not outdated, so folding
                them silently into the total reads as a fleet permanently
                short of itself with no explanation. */}
            {counts.ineligible > 0 ? (
              <>
                {", "}
                <span className="font-mono tabular-nums">
                  {counts.ineligible}
                </span>{" "}
                not self-updating
              </>
            ) : null}
            {counts.unknown > 0 ? (
              <>
                {", "}
                <span className="font-mono tabular-nums">{counts.unknown}</span>{" "}
                unknown
              </>
            ) : null}
          </p>
        )}
        {counts.outdated > 0 ? (
          <Link
            to="/sites"
            search={{ agentStatus: [AGENT_STATUS_LABEL.outdated] }}
            className="text-sm font-medium text-primary underline-offset-4 hover:underline"
          >
            View outdated sites
          </Link>
        ) : null}
      </CardContent>
    </Card>
  );
}
