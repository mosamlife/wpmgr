import type { UpdateTask } from "@wpmgr/api";

import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { VersionArrow } from "@/components/shared/version-arrow";
import { TaskStatusBadge } from "@/features/updates/update-status";

// Re-export siteNameMap so existing callers (e.g. $runId.tsx) don't need an
// import path change. Surface C agents may update their imports to ./summarize.
// eslint-disable-next-line react-refresh/only-export-components -- intentional re-export bridge; callers that own this import will move to ./summarize in Surface C
export { siteNameMap } from "./summarize";

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
      <p className="text-sm text-muted-foreground">
        No tasks yet. They appear as the run is scheduled.
      </p>
    );
  }

  return (
    <div className="overflow-hidden rounded-xl border border-border">
      <div className="w-full overflow-x-auto">
        <Table className="min-w-[560px]">
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
                  <span className="font-mono text-xs text-muted-foreground capitalize">
                    {task.target_type}
                  </span>
                  {task.target_type !== "core" && task.target_slug ? (
                    <>
                      {" "}
                      <span className="font-mono text-xs font-medium">
                        {task.target_slug}
                      </span>
                    </>
                  ) : null}
                </TableCell>
                <TableCell>
                  {task.from_version && task.to_version ? (
                    <VersionArrow
                      from={task.from_version}
                      to={task.to_version}
                    />
                  ) : (
                    <span className="text-muted-foreground text-xs">{"–"}</span>
                  )}
                </TableCell>
                <TableCell>
                  <TaskStatusBadge status={task.status} />
                </TableCell>
                <TableCell className="max-w-[200px] truncate font-mono text-xs text-muted-foreground">
                  {task.error ?? task.detail ?? (
                    <span aria-hidden="true">{"–"}</span>
                  )}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </div>
    </div>
  );
}

function short(id: string): string {
  return id.length > 8 ? `${id.slice(0, 8)}…` : id;
}
