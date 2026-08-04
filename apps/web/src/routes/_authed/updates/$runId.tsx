import { useMemo, useState } from "react";
import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { RotateCcw } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Progress } from "@/components/ui/progress";
import { PageError } from "@/components/feedback/page-error";
import { PageHeader } from "@/components/shared/page-header";
import { LiveIndicator, type LiveState } from "@/components/shared/live-indicator";
import { DefinitionList } from "@/components/shared/definition-list";
import { StatusChip } from "@/components/status/status-chip";
import type { StatusTone } from "@/components/status/status-dot";
import {
  useUpdateRun,
  useRunEventStream,
  NotFoundError,
  type RunStreamState,
} from "@/features/updates/use-updates";
import { UpdateTasksTable } from "@/features/updates/update-tasks-table";
import {
  summarizeTasks,
  siteNameMap,
  haltReason,
  isTerminalRunStatus,
} from "@/features/updates/summarize";
import {
  retryActionLabel,
  retryAvailability,
  sharedTargetType,
  type RetryTask,
} from "@/features/updates/retry-contract";
import { useRetrySelection } from "@/features/updates/use-retry-selection";
import { RetryActionBar } from "@/features/updates/retry-action-bar";
import { RetryRunDialog } from "@/features/updates/retry-dialog";
import { useSites } from "@/features/sites/use-sites";
import { useMe, canManage, canOperate } from "@/features/auth/use-auth";
import { relativeTime } from "@/lib/utils";
import type { UpdateRun } from "@wpmgr/api";

export const Route = createFileRoute("/_authed/updates/$runId")({
  component: RunDetailPage,
});

/** Map SSE transport state to the shared LiveIndicator's LiveState. */
function toLiveState(s: RunStreamState): LiveState {
  if (s === "live") return "live";
  if (s === "connecting") return "connecting";
  if (s === "polling") return "connecting";
  return "idle";
}

function RunDetailPage() {
  const { runId } = Route.useParams();

  // Transport state drives the live indicator and the polling fallback: when
  // the SSE EventSource errors we flip to "polling" and enable query refetch.
  const [streamState, setStreamState] = useState<RunStreamState>("connecting");
  const poll = streamState === "polling";

  const { data: run, isPending, isError, error, refetch } = useUpdateRun(
    runId,
    { poll },
  );

  // Subscribe to the SSE stream; it patches the run-detail cache directly.
  // Disabled once the run reaches a terminal state (no more deltas to
  // receive). `halted` (GH #255 Phase 2) is terminal exactly like
  // `completed`; see isTerminalRunStatus.
  useRunEventStream(runId, {
    enabled: !isTerminalRunStatus(run?.status),
    onState: setStreamState,
  });

  if (isPending) {
    return (
      <p role="status" className="text-[var(--color-muted-foreground)]">
        Loading update run…
      </p>
    );
  }

  if (isError) {
    if (error instanceof NotFoundError) {
      return (
        <section className="space-y-4">
          <PageHeader
            title={`Run ${runId.slice(0, 8)}…`}
            mono
            backTo={{ to: "/updates", label: "Update runs" }}
          />
          <PageError
            what="Update run not found"
            why={`No run exists with id ${runId}.`}
          />
        </section>
      );
    }
    return (
      <section className="space-y-4">
        <PageHeader
          title="Update run"
          backTo={{ to: "/updates", label: "Update runs" }}
        />
        <PageError
          what="Could not load update run"
          why={error.message}
          onRetry={() => void refetch()}
        />
      </section>
    );
  }

  return <RunDetail run={run} streamState={streamState} />;
}

type RunStatus = UpdateRun["status"];

const RUN_STATUS_TONE: Record<RunStatus, StatusTone> = {
  pending: "muted",
  running: "info",
  completed: "success",
  // GH #255 Phase 2: a wave failed to prove itself; destructive tone flags
  // it for immediate attention, matching the prominent banner below.
  halted: "destructive",
};

const RUN_STATUS_LABEL: Record<RunStatus, string> = {
  pending: "Pending",
  running: "Running",
  completed: "Completed",
  halted: "Halted",
};

function RunDetail({
  run,
  streamState,
}: {
  run: UpdateRun;
  streamState: RunStreamState;
}) {
  const { data: sites } = useSites();
  const { data: me } = useMe();
  const navigate = useNavigate();
  // Stable identity so the retry selection's memos (defaults, selectable set)
  // are not rebuilt on every render of a live run.
  const tasks: RetryTask[] = useMemo(() => run.tasks ?? [], [run.tasks]);
  const summary = summarizeTasks(tasks);
  const created = relativeTime(run.created_at);
  // `halted` (GH #255 Phase 2) is terminal exactly like `completed`; see
  // isTerminalRunStatus.
  const live = !isTerminalRunStatus(run.status);
  const halted = run.status === "halted";

  const liveState = toLiveState(streamState);
  const liveLabel = streamState === "polling" ? "Polling" : undefined;

  // ── GH #336 retry ───────────────────────────────────────────────────────
  //
  // Every gate here is a SERVER fact or a role, never a reading of task prose
  // and never the sites cache. The gating rules themselves live in
  // `retryAvailability` so they are pinned by a test rather than by JSX.
  const selection = useRetrySelection(tasks);
  const [retryOpen, setRetryOpen] = useState(false);

  const { available: retryAvailable, note: retryNote } = retryAvailability({
    tasks,
    selectableCount: selection.selectableTasks.length,
    runStatus: run.status,
    canOperate: canOperate(me),
    canManageAgents: canManage(me),
  });

  const selectedTarget = sharedTargetType(selection.selectedTasks);
  const retryLabel = retryActionLabel({
    count: selection.count,
    target: selectedTarget,
    dryRun: run.dry_run,
  });

  const openRun = (nextRunId: string) => {
    void navigate({ to: "/updates/$runId", params: { runId: nextRunId } });
  };

  return (
    <section className="space-y-6">
      <PageHeader
        title={`Run ${run.id.slice(0, 8)}…`}
        mono
        copyable={run.id}
        backTo={{ to: "/updates", label: "Update runs" }}
        badges={
          <>
            <StatusChip
              tone={RUN_STATUS_TONE[run.status]}
              label={RUN_STATUS_LABEL[run.status]}
              pulse={run.status === "running"}
            />
            {run.dry_run ? (
              <Badge variant="outline">Dry run</Badge>
            ) : (
              <Badge variant="secondary">Live</Badge>
            )}
            {live ? (
              <LiveIndicator state={liveState} label={liveLabel} />
            ) : null}
          </>
        }
        subline={
          <>
            Created {created ?? run.created_at}
            {run.scheduled_at
              ? ` · Scheduled for ${run.scheduled_at}`
              : ""}
          </>
        }
        actions={
          retryAvailable ? (
            <Button
              type="button"
              size="sm"
              onClick={() => setRetryOpen(true)}
              disabled={selection.count === 0}
            >
              <RotateCcw aria-hidden="true" className="size-4" />
              {retryLabel}
            </Button>
          ) : undefined
        }
      />

      {halted ? (
        // GH #255 Phase 2: a halt means a bad agent build was caught before
        // it could reach the rest of the fleet, which the operator needs to
        // see immediately, with the reason. Reuses PageError (no retry: the
        // run is terminal, not a failed fetch) so it reads with the same
        // weight as every other "this needs your attention" surface.
        <PageError
          what="This rollout was halted"
          why={haltReason(run) ?? undefined}
        />
      ) : null}

      <Card>
        <CardHeader>
          <CardTitle>Progress</CardTitle>
          <CardDescription>
            {summary.done} of {summary.total} task
            {summary.total === 1 ? "" : "s"} settled.
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          <Progress
            value={
              summary.total === 0
                ? null
                : Math.round((summary.done / summary.total) * 100)
            }
            label="Update progress"
          />
          <DefinitionList
            rows={[
              { label: "Succeeded", value: summary.counts.succeeded, tabular: true },
              { label: "Failed", value: summary.counts.failed, tabular: true },
              { label: "Rolled back", value: summary.counts.rolled_back, tabular: true },
              { label: "Running", value: summary.counts.running, tabular: true },
              { label: "Pending", value: summary.counts.pending, tabular: true },
              { label: "Skipped", value: summary.counts.skipped, tabular: true },
              // GH #336: nobody cancelled these. The run stopped and the
              // control plane withheld them, so nothing was ever sent to
              // those sites. "Not attempted" is what happened.
              {
                label: "Not attempted",
                value: summary.counts.cancelled,
                tabular: true,
              },
            ]}
          />
        </CardContent>
      </Card>

      <div className="space-y-2">
        <h2 className="text-lg font-semibold">Tasks</h2>
        {retryNote ? (
          <p className="text-sm text-muted-foreground">{retryNote}</p>
        ) : null}
        <UpdateTasksTable
          tasks={tasks}
          siteNames={siteNameMap(sites)}
          selection={retryAvailable ? selection : undefined}
        />
      </div>

      {retryAvailable ? (
        <>
          <RetryActionBar
            selectedTasks={selection.selectedTasks}
            target={selectedTarget}
            dryRun={run.dry_run}
            onClear={selection.clear}
            onRetry={() => setRetryOpen(true)}
          />
          <RetryRunDialog
            open={retryOpen}
            onClose={() => setRetryOpen(false)}
            runId={run.id}
            dryRun={run.dry_run}
            selectedTasks={selection.selectedTasks}
            allTasks={tasks}
            siteNames={siteNameMap(sites)}
            haltReason={haltReason(run)}
            onOpenRun={openRun}
          />
        </>
      ) : null}
    </section>
  );
}

