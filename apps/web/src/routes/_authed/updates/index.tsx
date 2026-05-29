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
import { relativeTime } from "@/lib/utils";
import type { UpdateRun } from "@wpmgr/api";

export const Route = createFileRoute("/_authed/updates/")({
  component: UpdatesPage,
});

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

function UpdatesPage() {
  const { data: runs, isPending, isError, error, refetch } = useUpdateRuns();

  return (
    <section className="space-y-6">
      <PageHeader
        title="Update runs"
        subline="Start a run from the Sites page by selecting sites or filtering by tag."
      />

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
                    <Link
                      to="/updates/$runId"
                      params={{ runId: run.id }}
                      className="font-mono text-sm underline-offset-4 hover:underline"
                    >
                      {run.id.slice(0, 8)}…
                    </Link>
                  </TableCell>
                  <TableCell>
                    <StatusChip
                      tone={RUN_STATUS_TONE[run.status]}
                      label={RUN_STATUS_LABEL[run.status]}
                      pulse={run.status === "running"}
                    />
                  </TableCell>
                  <TableCell>
                    {run.dry_run ? (
                      <Badge variant="outline">Dry run</Badge>
                    ) : (
                      <Badge variant="secondary">Live</Badge>
                    )}
                  </TableCell>
                  <TableCell className="tabular-nums text-[var(--color-muted-foreground)]">
                    {run.tasks?.length ?? 0}
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
