import { createFileRoute, Link } from "@tanstack/react-router";

import { Badge } from "@/components/ui/badge";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { PageError } from "@/components/feedback/page-error";
import { PageHeader } from "@/components/shared/page-header";
import { StatusChip } from "@/components/status/status-chip";
import type { StatusTone } from "@/components/status/status-dot";
import { useUpdateRuns } from "@/features/updates/use-updates";
import { ScheduleCountdown } from "@/features/updates/schedule-notices";
import { AgentFleetSummaryCard } from "@/features/fleet/AgentFleetSummaryCard";
import { relativeTime } from "@/lib/utils";
import type { UpdateRun } from "@wpmgr/api";

export const Route = createFileRoute("/_authed/updates/")({
  component: UpdatesPage,
});

type RunStatus = UpdateRun["status"];

// Defence in depth against version skew, mirroring
// features/updates/update-status.tsx: `tsc` enforces that every RunStatus is
// covered below, but a self-hosted control plane can still send a status
// literal this bundle predates, so the dereferences below re-type the lookup
// as partial rather than trusting the exhaustive type at runtime.
const UNKNOWN_RUN_TONE: StatusTone = "muted";
const UNKNOWN_RUN_LABEL = "Unknown";

const RUN_STATUS_TONE: Record<RunStatus, StatusTone> = {
  pending: "muted",
  running: "info",
  completed: "success",
  // GH #255 Phase 2: a wave failed to prove itself; destructive tone flags
  // it for immediate attention among an otherwise calm list of runs.
  halted: "destructive",
  // GH #463: waiting for scheduled_at, nothing has happened yet.
  scheduled: "muted",
  // GH #463: transient, the dispatcher is enqueueing this run's tasks right
  // now — mirrors "Running".
  dispatching: "info",
  // GH #463: terminal, no site was contacted — a missed schedule, not a
  // failed update, so this deliberately avoids "destructive" (see
  // features/updates/update-status.tsx for the full reasoning).
  expired: "warning",
};

const RUN_STATUS_LABEL: Record<RunStatus, string> = {
  pending: "Pending",
  running: "Running",
  completed: "Completed",
  halted: "Halted",
  scheduled: "Scheduled",
  dispatching: "Dispatching",
  expired: "Expired",
};

function UpdatesPage() {
  const { data: runs, isPending, isError, error, refetch } = useUpdateRuns();

  return (
    <section className="space-y-6">
      <PageHeader
        title="Update runs"
        subline="Start a run from the Sites page by selecting sites or filtering by tag."
      />

      <AgentFleetSummaryCard />

      {isPending ? (
        <p role="status" className="text-[var(--color-muted-foreground)]">
          Loading update runs…
        </p>
      ) : isError ? (
        <PageError
          what="Could not load update runs"
          why={error.message}
          onRetry={() => void refetch()}
        />
      ) : runs.length === 0 ? (
        <div className="rounded-xl border border-[var(--color-border)] p-8 text-center">
          <p className="text-[var(--color-muted-foreground)]">
            No update runs yet.
          </p>
        </div>
      ) : (
        <div className="rounded-xl border border-[var(--color-border)]">
          <Table>
            <caption className="sr-only">Recent update runs</caption>
            <TableHeader>
              <TableRow>
                <TableHead>Run</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Mode</TableHead>
                <TableHead>Tasks</TableHead>
                <TableHead>Created</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {runs.map((run) => (
                <TableRow key={run.id} data-testid="update-run-row">
                  <TableCell className="font-medium">
                    <div className="flex flex-col gap-0.5">
                      <Link
                        to="/updates/$runId"
                        params={{ runId: run.id }}
                        className="font-mono text-sm underline-offset-4 hover:underline"
                      >
                        {run.id.slice(0, 8)}…
                      </Link>
                      {/* GH #463: a scheduled run is the one row in this list
                          where "when" is the whole point, so the countdown and
                          the zone-labelled absolute time sit directly under the
                          id rather than in the Created column, which means
                          something else. */}
                      {run.status === "scheduled" && run.scheduled_at ? (
                        <ScheduleCountdown
                          scheduledAt={run.scheduled_at}
                          className="text-xs font-normal"
                        />
                      ) : null}
                    </div>
                  </TableCell>
                  <TableCell>
                    <StatusChip
                      tone={
                        (
                          RUN_STATUS_TONE as Partial<
                            Record<RunStatus, StatusTone>
                          >
                        )[run.status] ?? UNKNOWN_RUN_TONE
                      }
                      label={
                        (
                          RUN_STATUS_LABEL as Partial<Record<RunStatus, string>>
                        )[run.status] ?? UNKNOWN_RUN_LABEL
                      }
                      pulse={
                        run.status === "running" ||
                        run.status === "dispatching"
                      }
                    />
                  </TableCell>
                  <TableCell>
                    {run.dry_run ? (
                      <Badge variant="outline">Dry run</Badge>
                    ) : (
                      <Badge variant="secondary">Live</Badge>
                    )}
                  </TableCell>
                  <TableCell>
                    <div className="flex flex-wrap items-center gap-1.5 tabular-nums">
                      <span className="text-[var(--color-muted-foreground)]">
                        {run.task_count ?? 0}
                      </span>
                      {(run.failed_count ?? 0) > 0 ? (
                        <Badge
                          variant="destructive"
                          className="rounded-sm px-1.5 py-0 text-xs tabular-nums"
                        >
                          {run.failed_count} failed
                        </Badge>
                      ) : null}
                      {(run.site_count ?? 0) > 0 ? (
                        <Badge
                          variant="muted"
                          className="rounded-sm px-1.5 py-0 text-xs tabular-nums"
                        >
                          {run.site_count} site{run.site_count === 1 ? "" : "s"}
                        </Badge>
                      ) : null}
                    </div>
                  </TableCell>
                  <TableCell className="text-[var(--color-muted-foreground)]">
                    <time dateTime={run.created_at}>
                      {relativeTime(run.created_at) ?? run.created_at}
                    </time>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
      )}
    </section>
  );
}
