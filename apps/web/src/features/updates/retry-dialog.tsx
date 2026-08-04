import { useMemo, useState } from "react";

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
import { toast } from "@/components/toast";

import {
  countByRetryClass,
  distinctSiteCount,
  exclusionReasonLabel,
  formatRetryBreakdown,
  groupExclusions,
  retryActionLabel,
  retryNeedsReview,
  sharedTargetType,
  sitesNoun,
  taskSiteLabel,
  taskTargetLabel,
  updatesNoun,
  type RetryResult,
  type RetryTask,
} from "./retry-contract";
import { useRetryRun } from "./use-retry-run";

// GH #336 confirmation for retrying a set of tasks from an existing run.
//
// Two things this dialog exists to do, and it must never stop doing either:
//
//   1. INFORM, do not gate. A rolled_back task gets a plain statement of what
//      a rollback means and what a retry may reproduce. It gets NO typed
//      token and no acknowledgement checkbox: the same operator can already
//      retry the same rolled_back update with one unconfirmed click from the
//      site's own updates card, so heavy friction here only pushes people to
//      the easier, worse informed door.
//   2. ACCOUNT FOR EVERY REQUESTED TASK. The response says how many were
//      requested, how many were created, and why the rest were not. When
//      those numbers differ the dialog switches to a result step and shows
//      the difference with reasons. There is no success-only path that
//      navigates away from a partial commit.

export function RetryRunDialog({
  open,
  onClose,
  runId,
  dryRun,
  selectedTasks,
  allTasks,
  siteNames,
  haltReason,
  onOpenRun,
}: {
  open: boolean;
  onClose: () => void;
  /** The run being retried. A retry never mutates it. */
  runId: string;
  /** True when the run being retried was a dry run. */
  dryRun: boolean;
  /** The effective selection: tasks the server still marks retryable. */
  selectedTasks: RetryTask[];
  /** Every task in the run, used to name an excluded task in the result. */
  allTasks: RetryTask[];
  siteNames?: Map<string, string>;
  /** The reason the source run halted, when it did. */
  haltReason?: string | null;
  /** Navigate to a run id. Owned by the page so this stays router free. */
  onOpenRun: (runId: string) => void;
}) {
  return (
    <Dialog open={open} onClose={onClose}>
      {/*
        DELIBERATELY NOT KEYED on the selection.

        A previous attempt's result must not leak into the next open, and it
        cannot: the body is conditionally rendered, so closing the dialog
        unmounts it and reopening mounts a fresh one. The key added nothing to
        that and cost something real.

        Keying on selectedTasks remounted the body WHILE THE DIALOG WAS OPEN.
        On a still-running run the default selection grows as tasks settle, so
        the body remounted under the operator: the heading changed from "Retry
        12 updates" to "Retry 18 updates" mid-decision, and the partial-commit
        accounting, the exclusion reasons and the server warning were all
        destroyed seconds after arriving, because that state lives in this
        component. The confirm button then came back live with the same
        selection, so the next click created a SECOND retry run.

        The body freezes its own copy of the selection at mount for the same
        reason: what the operator confirms must be what gets submitted.
      */}
      {open ? (
        <RetryRunDialogBody
          runId={runId}
          dryRun={dryRun}
          selectedTasks={selectedTasks}
          allTasks={allTasks}
          siteNames={siteNames}
          haltReason={haltReason}
          onClose={onClose}
          onOpenRun={onOpenRun}
        />
      ) : null}
    </Dialog>
  );
}

function RetryRunDialogBody({
  runId,
  dryRun,
  selectedTasks: liveSelection,
  allTasks,
  siteNames,
  haltReason,
  onClose,
  onOpenRun,
}: {
  runId: string;
  dryRun: boolean;
  selectedTasks: RetryTask[];
  allTasks: RetryTask[];
  siteNames?: Map<string, string>;
  haltReason?: string | null;
  onClose: () => void;
  onOpenRun: (runId: string) => void;
}) {
  const retry = useRetryRun(runId);
  const [result, setResult] = useState<RetryResult | null>(null);

  // FROZEN AT MOUNT, which is the moment the dialog opened. useState's
  // initializer runs exactly once.
  //
  // The selection this dialog was opened with is a decision about a specific
  // set of updates. On a still-running run the live default set grows as tasks
  // settle, and letting that through would change the number under the cursor
  // of someone who has already decided to click, and submit a set they never
  // confirmed. What is counted here, what the confirm button says, and what is
  // sent are now all the same list.
  const [selectedTasks] = useState<RetryTask[]>(() => liveSelection);

  const count = selectedTasks.length;
  const target = sharedTargetType(selectedTasks);
  const siteCount = distinctSiteCount(selectedTasks);
  const counts = countByRetryClass(selectedTasks);
  const breakdown = formatRetryBreakdown(counts);
  const label = retryActionLabel({ count, target, dryRun });
  const isAgent = target === "agent";
  const revertedCount = counts.reverted ?? 0;
  const skippedCount = counts.skipped ?? 0;
  const neverRanCount = counts.never_ran ?? 0;

  async function handleConfirm() {
    if (count === 0) return;
    try {
      const response = await retry.mutateAsync({
        taskIds: selectedTasks.map((t) => t.id),
      });
      const newRunId = response.run_id;
      if (!retryNeedsReview(response) && newRunId) {
        // Everything requested was created and the server flagged nothing.
        // Navigation is the feedback, matching every other run-creating
        // surface in this app.
        onClose();
        onOpenRun(newRunId);
        return;
      }
      // A shortfall or a server warning: hold the dialog open on a result
      // step so it is read before anything else happens, and leave a toast
      // behind so the numbers survive the navigation that follows.
      setResult(response);
      const shortfall = response.requested - response.created;
      toast.warning(
        `Started ${response.created} of ${updatesNoun(response.requested)}`,
        {
          description:
            shortfall > 0
              ? `${shortfall} did not start. Open the run to see what did.`
              : "The control plane flagged something about this run.",
          action: {
            label: "Open run",
            onClick: () => {
              if (newRunId) onOpenRun(newRunId);
            },
          },
        },
      );
    } catch {
      // retry.isError renders inline below; nothing further to do here.
    }
  }

  if (result) {
    return (
      <RetryResultPanel
        result={result}
        allTasks={allTasks}
        siteNames={siteNames}
        onClose={onClose}
        onOpenRun={onOpenRun}
      />
    );
  }

  return (
    <DialogContent
      ariaLabelledBy="retry-run-title"
      ariaDescribedBy="retry-run-description"
      className="max-w-[560px]"
    >
      <DialogHeader>
        <DialogTitle id="retry-run-title">{label}</DialogTitle>
        <DialogDescription id="retry-run-description">
          {`${updatesNoun(count)} on ${sitesNoun(siteCount)} will be requested. This creates a new run and leaves this one exactly as it is.`}
        </DialogDescription>
      </DialogHeader>

      <DialogBody className="space-y-3 text-sm text-foreground">
        {dryRun ? (
          <p
            role="status"
            className="rounded-md border border-[var(--color-warning)]/40 bg-warning-subtle px-3 py-2 text-sm text-warning-subtle-fg"
          >
            Dry run. Nothing will be applied to any site. This run reports what
            would change.
          </p>
        ) : null}

        {breakdown ? (
          <p className="text-muted-foreground">Selected: {breakdown}.</p>
        ) : null}

        {isAgent ? (
          <>
            <p>
              This retry starts a fresh staged rollout: one site first, then a
              pilot group, then the rest. Each wave has to prove itself before
              the next one is allowed to start, so a bad build is caught early
              rather than reaching the whole fleet. Previously cancelled sites
              are not sent all at once.
            </p>
            <p>
              A site only counts as updated once its agent reports the new
              version back on its own, and the retry installs whichever agent
              version is published right now, which may not be the version this
              run set out to install.
            </p>
            {count === 1 ? (
              <p className="rounded-md border border-[var(--color-warning)]/40 bg-warning-subtle px-3 py-2 text-warning-subtle-fg">
                This retry has only one site, so there is no canary ahead of it.
                Nothing will prove the build before it is applied.
              </p>
            ) : null}
          </>
        ) : (
          <p>
            Each site is checked again before anything is applied, and an update
            is rolled back automatically if the site fails its health check
            afterwards.
          </p>
        )}

        {neverRanCount > 0 ? (
          <p className="text-muted-foreground">
            {`${updatesNoun(neverRanCount)} in this selection ${neverRanCount === 1 ? "was" : "were"} never attempted: the run stopped before ${neverRanCount === 1 ? "it was" : "they were"} sent, so nothing was applied to ${neverRanCount === 1 ? "that site" : "those sites"}.`}
          </p>
        ) : null}

        {revertedCount > 0 ? (
          <p className="rounded-md border border-[var(--color-warning)]/40 bg-warning-subtle px-3 py-2 text-warning-subtle-fg">
            {`${updatesNoun(revertedCount)} in this selection ${revertedCount === 1 ? "was" : "were"} rolled back. The update applied, the site then failed its health check, and the change was reverted automatically. Retrying runs the same update down the same path and can reproduce the same break. The health check and the automatic rollback still apply.`}
          </p>
        ) : null}

        {skippedCount > 0 ? (
          <p className="text-muted-foreground">
            {`${updatesNoun(skippedCount)} in this selection ${skippedCount === 1 ? "was" : "were"} skipped. A skip usually means the site reported it was already up to date, so a retry can simply skip again.`}
          </p>
        ) : null}

        {haltReason ? (
          <p className="text-muted-foreground">
            This run stopped because: {haltReason}
          </p>
        ) : null}

        <p className="text-muted-foreground">
          Each site is re-checked when the retry is created. A site that has
          since disconnected, or an update another run is already applying, is
          left out and reported back with a reason.
        </p>

        {retry.isError ? (
          <p role="alert" className="text-destructive">
            {retry.error.message}
          </p>
        ) : null}
      </DialogBody>

      <DialogFooter className="pt-2">
        <Button
          type="button"
          variant="outline"
          onClick={onClose}
          disabled={retry.isPending}
        >
          Keep this run as it is
        </Button>
        <Button
          type="button"
          onClick={() => void handleConfirm()}
          disabled={retry.isPending || count === 0}
        >
          {retry.isPending ? "Starting..." : label}
        </Button>
      </DialogFooter>
    </DialogContent>
  );
}

/**
 * THE PARTIAL COMMIT SURFACE. Reached whenever `created` is not `requested`,
 * including `created === 0`. It states both numbers, then every exclusion with
 * its reason and the (site, target) it belongs to. An unrecognised reason code
 * is printed verbatim rather than dropped: the operator's question is "why did
 * this one not run", and a reason this client has not seen before is still the
 * answer.
 */
function RetryResultPanel({
  result,
  allTasks,
  siteNames,
  onClose,
  onOpenRun,
}: {
  result: RetryResult;
  allTasks: RetryTask[];
  siteNames?: Map<string, string>;
  onClose: () => void;
  onOpenRun: (runId: string) => void;
}) {
  const byId = useMemo(() => {
    const map = new Map<string, RetryTask>();
    for (const task of allTasks) map.set(task.id, task);
    return map;
  }, [allTasks]);

  const groups = groupExclusions(result.excluded);
  const shortfall = result.requested - result.created;
  const started = result.created > 0;
  const newRunId = result.run_id;
  const title = started
    ? `Started ${result.created} of ${updatesNoun(result.requested)}`
    : "No updates were started";

  return (
    <DialogContent
      ariaLabelledBy="retry-result-title"
      ariaDescribedBy="retry-result-description"
      className="max-w-[560px]"
    >
      <DialogHeader>
        <DialogTitle id="retry-result-title">{title}</DialogTitle>
        <DialogDescription id="retry-result-description">
          {started
            ? `${updatesNoun(result.created)} were created in a new run. ${shortfall} of the ${result.requested} requested did not start.`
            : `None of the ${updatesNoun(result.requested)} requested could be started. This run is unchanged.`}
        </DialogDescription>
      </DialogHeader>

      <DialogBody className="space-y-3 text-sm text-foreground">
        <div data-testid="retry-partial-result" className="space-y-3">
          {shortfall > 0 ? (
            <p role="alert" className="font-medium">
              {`${shortfall} of ${updatesNoun(result.requested)} did not start.`}
            </p>
          ) : null}

          {result.warning ? (
            <p
              role="alert"
              className="rounded-md border border-[var(--color-warning)]/40 bg-warning-subtle px-3 py-2 text-warning-subtle-fg"
            >
              {result.warning}
            </p>
          ) : null}

          {groups.length > 0 ? (
            <ul className="space-y-3">
              {groups.map((group) => (
                <li key={group.reason} className="space-y-1">
                  <p>
                    <span className="tabular-nums font-medium">
                      {group.items.length}
                    </span>{" "}
                    {exclusionReasonLabel(group.reason)}
                  </p>
                  <ul className="space-y-1 pl-4 text-xs text-muted-foreground">
                    {group.items.map((item) => {
                      const task = byId.get(item.task_id);
                      return (
                        <li key={item.task_id}>
                          <span className="font-mono">
                            {task
                              ? `${taskSiteLabel(task, siteNames)} (${taskTargetLabel(task)})`
                              : item.task_id}
                          </span>
                          {/* The server authors this sentence with the
                            specifics; it is rendered as it arrives. */}
                          {item.message ? <span>: {item.message}</span> : null}
                        </li>
                      );
                    })}
                  </ul>
                </li>
              ))}
            </ul>
          ) : shortfall > 0 ? (
            <p className="text-muted-foreground">
              The control plane did not say which updates were left out. Open
              the new run to see exactly what it contains.
            </p>
          ) : null}
        </div>
      </DialogBody>

      <DialogFooter className="pt-2">
        <Button type="button" variant="outline" onClick={onClose}>
          Stay on this run
        </Button>
        {started && newRunId ? (
          <Button
            type="button"
            onClick={() => {
              onClose();
              onOpenRun(newRunId);
            }}
          >
            {`Open run ${newRunId.slice(0, 8)}`}
          </Button>
        ) : null}
      </DialogFooter>
    </DialogContent>
  );
}
