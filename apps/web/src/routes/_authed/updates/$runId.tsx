import { useState } from "react";
import { createFileRoute, Link } from "@tanstack/react-router";

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
import {
  useUpdateRun,
  useRunEventStream,
  NotFoundError,
  type RunStreamState,
} from "@/features/updates/use-updates";
import {
  RunStatusBadge,
  summarizeTasks,
} from "@/features/updates/update-status";
import {
  UpdateTasksTable,
  siteNameMap,
} from "@/features/updates/update-tasks-table";
import { useSites } from "@/features/sites/use-sites";
import { relativeTime } from "@/lib/utils";
import type { UpdateRun } from "@wpmgr/api";

export const Route = createFileRoute("/_authed/updates/$runId")({
  component: RunDetailPage,
});

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

  return (
    <section aria-labelledby="run-heading" className="space-y-6">
      <div className="flex items-center gap-3">
        <Button asChild variant="outline" size="sm">
          <Link to="/updates">Back to updates</Link>
        </Button>
      </div>

      {isPending ? (
        <p role="status" className="text-[var(--color-muted-foreground)]">
          Loading update run…
        </p>
      ) : isError ? (
        error instanceof NotFoundError ? (
          <div role="alert" className="space-y-2">
            <h1 id="run-heading" className="text-2xl font-semibold">
              Update run not found
            </h1>
            <p className="text-[var(--color-muted-foreground)]">
              No run exists with id <code>{runId}</code>.
            </p>
          </div>
        ) : (
          <div role="alert" className="space-y-3">
            <h1 id="run-heading" className="text-2xl font-semibold">
              Could not load update run
            </h1>
            <p className="text-[var(--color-destructive)]">{error.message}</p>
            <Button variant="outline" size="sm" onClick={() => void refetch()}>
              Retry
            </Button>
          </div>
        )
      ) : (
        <RunDetail run={run} streamState={streamState} />
      )}
    </section>
  );
}

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

  return (
    <div className="space-y-6">
      <div className="space-y-2">
        <div className="flex flex-wrap items-center gap-3">
          <h1 id="run-heading" className="font-mono text-2xl font-semibold">
            Run {run.id.slice(0, 8)}…
          </h1>
          <RunStatusBadge status={run.status} />
          {run.dry_run ? (
            <Badge variant="outline">Dry run</Badge>
          ) : (
            <Badge variant="secondary">Live</Badge>
          )}
          {live ? <LiveIndicator state={streamState} /> : null}
        </div>
        <p className="text-sm text-[var(--color-muted-foreground)]">
          Created {created ?? run.created_at}
          {run.scheduled_at
            ? ` · Scheduled for ${run.scheduled_at}`
            : ""}
        </p>
      </div>

      <Card>
        <CardHeader>
          <CardTitle>Progress</CardTitle>
          <CardDescription>
            {summary.done} of {summary.total} task
            {summary.total === 1 ? "" : "s"} settled.
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-3">
          <Progress
            value={summary.done}
            max={summary.total}
            label="Update progress"
          />
          <dl className="flex flex-wrap gap-4 text-sm">
            <Stat label="Succeeded" value={summary.counts.succeeded} />
            <Stat label="Failed" value={summary.counts.failed} />
            <Stat label="Rolled back" value={summary.counts.rolled_back} />
            <Stat label="Running" value={summary.counts.running} />
            <Stat label="Pending" value={summary.counts.pending} />
            <Stat label="Skipped" value={summary.counts.skipped} />
          </dl>
        </CardContent>
      </Card>

      <div className="space-y-2">
        <h2 className="text-lg font-semibold">Tasks</h2>
        <UpdateTasksTable tasks={tasks} siteNames={siteNameMap(sites)} />
      </div>
    </div>
  );
}

function Stat({ label, value }: { label: string; value: number }) {
  return (
    <div>
      <dt className="text-[var(--color-muted-foreground)]">{label}</dt>
      <dd className="font-medium">{value}</dd>
    </div>
  );
}

// Small live-status pill. "live" = SSE connected; "polling" = SSE failed and we
// fell back to query refetch; otherwise connecting.
function LiveIndicator({ state }: { state: RunStreamState }) {
  if (state === "polling") {
    return (
      <span
        className="inline-flex items-center gap-1 text-xs text-[var(--color-muted-foreground)]"
        role="status"
      >
        <span
          aria-hidden="true"
          className="size-1.5 animate-pulse rounded-full bg-amber-500"
        />
        Polling for updates
      </span>
    );
  }
  if (state === "live") {
    return (
      <span
        className="inline-flex items-center gap-1 text-xs text-[var(--color-muted-foreground)]"
        role="status"
      >
        <span
          aria-hidden="true"
          className="size-1.5 animate-pulse rounded-full bg-green-500"
        />
        Live
      </span>
    );
  }
  return (
    <span
      className="inline-flex items-center gap-1 text-xs text-[var(--color-muted-foreground)]"
      role="status"
    >
      <span
        aria-hidden="true"
        className="size-1.5 rounded-full bg-[var(--color-muted-foreground)]"
      />
      Connecting…
    </span>
  );
}
