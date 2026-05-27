import { createFileRoute, Link } from "@tanstack/react-router";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { useUpdateRuns } from "@/features/updates/use-updates";
import { RunStatusBadge } from "@/features/updates/update-status";
import { relativeTime } from "@/lib/utils";

export const Route = createFileRoute("/_authed/updates/")({
  component: UpdatesPage,
});

function UpdatesPage() {
  const { data: runs, isPending, isError, error, refetch } = useUpdateRuns();

  return (
    <section aria-labelledby="updates-heading" className="space-y-4">
      <h1 id="updates-heading" className="text-2xl font-semibold">
        Update runs
      </h1>
      <p className="text-sm text-[var(--color-muted-foreground)]">
        Bulk update runs across your sites. Start one from the Sites page by
        selecting sites or filtering by tag.
      </p>

      {isPending ? (
        <p role="status" className="text-[var(--color-muted-foreground)]">
          Loading update runs…
        </p>
      ) : isError ? (
        <div role="alert" className="space-y-3">
          <p className="text-[var(--color-destructive)]">
            Failed to load update runs: {error.message}
          </p>
          <Button variant="outline" size="sm" onClick={() => void refetch()}>
            Retry
          </Button>
        </div>
      ) : runs.length === 0 ? (
        <div className="rounded-xl border border-dashed border-[var(--color-border)] p-8 text-center">
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
                    <RunStatusBadge status={run.status} />
                  </TableCell>
                  <TableCell>
                    {run.dry_run ? (
                      <Badge variant="outline">Dry run</Badge>
                    ) : (
                      <Badge variant="secondary">Live</Badge>
                    )}
                  </TableCell>
                  <TableCell className="text-[var(--color-muted-foreground)]">
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
