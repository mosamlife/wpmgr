import { useState } from "react";
import { createFileRoute } from "@tanstack/react-router";

import { Badge } from "@/components/ui/badge";
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
import { summarizeTasks, siteNameMap } from "@/features/updates/summarize";
import { useSites } from "@/features/sites/use-sites";
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
  // Disabled once the run is completed (no more deltas to receive).
  useRunEventStream(runId, {
    enabled: run?.status !== "completed",
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
};

const RUN_STATUS_LABEL: Record<RunStatus, string> = {
  pending: "Pending",
  running: "Running",
  completed: "Completed",
};

function RunDetail({
  run,
  streamState,
}: {
  run: UpdateRun;
  streamState: RunStreamState;
}) {
  const { data: sites } = useSites();
  const tasks = run.tasks ?? [];
  const summary = summarizeTasks(tasks);
  const created = relativeTime(run.created_at);
  const live = run.status !== "completed";

  const liveState = toLiveState(streamState);
  const liveLabel = streamState === "polling" ? "Polling" : undefined;

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
      />

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
            value={summary.done}
            max={summary.total}
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
            ]}
          />
        </CardContent>
      </Card>

      <div className="space-y-2">
        <h2 className="text-lg font-semibold">Tasks</h2>
        <UpdateTasksTable tasks={tasks} siteNames={siteNameMap(sites)} />
      </div>
    </section>
  );
}

