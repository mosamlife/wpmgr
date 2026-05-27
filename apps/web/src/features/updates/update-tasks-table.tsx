import type { Site, UpdateTask } from "@wpmgr/api";

import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { TaskStatusBadge } from "@/features/updates/update-status";

// Live table of per-(site, target) update tasks. Rows reflect whatever is in
// the run-detail query cache, which the SSE stream patches in place.
export function UpdateTasksTable({
  tasks,
  siteNames,
}: {
  tasks: UpdateTask[];
  // Optional lookup of site id -> friendly name (from the sites cache).
  siteNames?: Map<string, string>;
}) {
  if (tasks.length === 0) {
    return (
      <p className="text-sm text-[var(--color-muted-foreground)]">
        No tasks yet. They appear as the run is scheduled.
      </p>
    );
  }

  return (
    <div className="rounded-xl border border-[var(--color-border)]">
      <Table>
        <caption className="sr-only">Update tasks</caption>
        <TableHeader>
          <TableRow>
            <TableHead>Site</TableHead>
            <TableHead>Target</TableHead>
            <TableHead>Version</TableHead>
            <TableHead>Status</TableHead>
            <TableHead>Detail</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {tasks.map((task) => (
            <TableRow key={task.id} data-testid="update-task-row">
              <TableCell className="font-medium">
                {siteNames?.get(task.site_id) ?? short(task.site_id)}
              </TableCell>
              <TableCell>
                <span className="capitalize text-[var(--color-muted-foreground)]">
                  {task.target_type}
                </span>{" "}
                {task.target_type !== "core" ? (
                  <span className="font-medium">{task.target_slug}</span>
                ) : null}
              </TableCell>
              <TableCell>
                <VersionDiff from={task.from_version} to={task.to_version} />
              </TableCell>
              <TableCell>
                <TaskStatusBadge status={task.status} />
              </TableCell>
              <TableCell className="text-[var(--color-muted-foreground)]">
                {task.error ?? task.detail ?? "—"}
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </div>
  );
}

function VersionDiff({ from, to }: { from?: string; to?: string }) {
  if (!from && !to) {
    return <span className="text-[var(--color-muted-foreground)]">—</span>;
  }
  return (
    <span className="font-mono text-xs">
      <span className="text-[var(--color-muted-foreground)]">
        {from ?? "?"}
      </span>
      <span aria-hidden="true"> → </span>
      <span className="font-medium">{to ?? "latest"}</span>
    </span>
  );
}

function short(id: string): string {
  return id.length > 8 ? `${id.slice(0, 8)}…` : id;
}

/** Build a site id -> name lookup from the sites list cache. */
export function siteNameMap(sites: Site[] | undefined): Map<string, string> {
  const map = new Map<string, string>();
  for (const site of sites ?? []) map.set(site.id, site.name);
  return map;
}
