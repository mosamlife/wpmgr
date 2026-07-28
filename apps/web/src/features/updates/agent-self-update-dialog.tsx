import { useMemo } from "react";
import { useNavigate } from "@tanstack/react-router";

import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogBody,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { agentStatusDisplayLabel, type AgentStatus } from "@/components/status";
import { useCreateUpdateRun } from "@/features/updates/use-updates";
import type { FleetAgentVersions, Site } from "@wpmgr/api";

// GH #255 Phase 2: the confirmation dialog for the "Update WPMgr agent" bulk
// action (sites toolbar > More). Reuses the fleet agent-version rollup
// (Phase 1's useFleetAgentVersions, already loaded on the Sites page for the
// status chip/filter) to tell the operator the truth up front: which of the
// selected sites will actually be targeted, and which are excluded and why.
//
// Deliberately its own dialog rather than <UpdateWizard>: the agent target
// takes no slug/version, has no dry run, and is a fundamentally different
// operation (a staged, wave-gated rollout of the manager itself, not a
// plugin/theme/core bump), so mixing it into the wizard's component picker
// would misrepresent both.

export function AgentSelfUpdateDialog({
  open,
  onClose,
  sites,
  agentStatusById,
  agentReferenceSource,
}: {
  open: boolean;
  onClose: () => void;
  /** The full current selection (may include sites outside the loaded view). */
  sites: Site[];
  /** Per-site agent-freshness classification from the fleet rollup. */
  agentStatusById: Map<string, AgentStatus> | undefined;
  /** Where the classification's reference version came from; see FleetAgentVersions.reference_source. */
  agentReferenceSource?: FleetAgentVersions["reference_source"];
}) {
  return (
    <Dialog open={open} onClose={onClose}>
      {/* Remount on open so a previous attempt's error/pending state never
          leaks into the next time the dialog is opened for a new selection. */}
      {open ? (
        <AgentSelfUpdateDialogBody
          key={sites.map((s) => s.id).join(",")}
          sites={sites}
          agentStatusById={agentStatusById}
          agentReferenceSource={agentReferenceSource}
          onClose={onClose}
        />
      ) : null}
    </Dialog>
  );
}

function AgentSelfUpdateDialogBody({
  sites,
  agentStatusById,
  agentReferenceSource,
  onClose,
}: {
  sites: Site[];
  agentStatusById: Map<string, AgentStatus> | undefined;
  agentReferenceSource?: FleetAgentVersions["reference_source"];
  onClose: () => void;
}) {
  const navigate = useNavigate();
  const create = useCreateUpdateRun();

  // Known once the fleet rollup has loaded. Only "outdated" sites are ever
  // targeted (planAgentTasks on the control plane silently excludes every
  // other status), so the dialog must say that up front rather than let the
  // operator believe every selected site will be touched.
  const knownEligibility = agentStatusById !== undefined;
  const eligible = useMemo(
    () =>
      sites.filter((s) => agentStatusById?.get(s.id) === "outdated"),
    [sites, agentStatusById],
  );
  const excludedCounts = useMemo(() => {
    const counts: Partial<Record<AgentStatus, number>> = {};
    if (!knownEligibility) return counts;
    for (const site of sites) {
      const status = agentStatusById?.get(site.id) ?? "unknown";
      if (status === "outdated") continue;
      counts[status] = (counts[status] ?? 0) + 1;
    }
    return counts;
  }, [sites, agentStatusById, knownEligibility]);

  const targetCount = knownEligibility ? eligible.length : sites.length;
  const sitesNoun = sites.length === 1 ? "site" : "sites";
  const targetNoun = targetCount === 1 ? "site" : "sites";

  async function handleConfirm() {
    if (targetCount === 0) return;
    const targetIds = knownEligibility
      ? eligible.map((s) => s.id)
      : sites.map((s) => s.id);
    try {
      const run = await create.mutateAsync({
        site_ids: targetIds,
        items: [{ type: "agent" }],
        dry_run: false,
      });
      onClose();
      void navigate({ to: "/updates/$runId", params: { runId: run.id } });
    } catch {
      // create.isError renders below; nothing further to do here.
    }
  }

  return (
    <DialogContent
      ariaLabelledBy="agent-self-update-title"
      ariaDescribedBy="agent-self-update-description"
      className="max-w-[520px]"
    >
      <DialogHeader>
        <DialogTitle id="agent-self-update-title">
          Update WPMgr agent
        </DialogTitle>
        <DialogDescription id="agent-self-update-description">
          {knownEligibility
            ? `${targetCount} of ${sites.length} selected ${sitesNoun} will be targeted.`
            : `Targeting ${sites.length} selected ${sitesNoun}.`}
        </DialogDescription>
      </DialogHeader>

      <DialogBody className="space-y-3 text-sm text-foreground">
        <p>
          The agent update is applied in staged waves, starting with a single
          site. Each wave has to prove itself before the next one is allowed
          to start, so a bad build is caught early rather than reaching the
          whole fleet.
        </p>
        <p>
          A site only counts as updated once its agent reports the new
          version back on its own. Scheduling the update is not success by
          itself.
        </p>
        <p>
          A site WPMgr cannot reach, or that never confirms, is left
          untouched rather than marked failed. This can take a while: the
          upgrade runs on each site's own cron, which is not instant.
        </p>

        {knownEligibility && Object.keys(excludedCounts).length > 0 ? (
          <p className="text-muted-foreground">
            Not targeted:{" "}
            {(Object.entries(excludedCounts) as [AgentStatus, number][])
              .map(
                ([status, n]) =>
                  `${n} ${agentStatusDisplayLabel(status, agentReferenceSource).toLowerCase()}`,
              )
              .join(", ")}
            .
          </p>
        ) : null}

        {knownEligibility && targetCount === 0 ? (
          <p role="status" className="text-muted-foreground">
            None of the selected sites need an agent update right now.
          </p>
        ) : null}

        {create.isError ? (
          <p role="alert" className="text-destructive">
            {create.error.message}
          </p>
        ) : null}
      </DialogBody>

      <DialogFooter className="pt-2">
        <Button
          type="button"
          variant="outline"
          onClick={onClose}
          disabled={create.isPending}
        >
          Cancel
        </Button>
        <Button
          type="button"
          onClick={() => void handleConfirm()}
          disabled={create.isPending || targetCount === 0}
        >
          {create.isPending
            ? "Starting..."
            : `Update ${targetCount} ${targetNoun}`}
        </Button>
      </DialogFooter>
    </DialogContent>
  );
}
