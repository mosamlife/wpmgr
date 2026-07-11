import { useState } from "react";
import { AlertTriangle, Check, ChevronDown, ChevronRight, Copy } from "lucide-react";
import type { UpdateTask } from "@wpmgr/api";

import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Button } from "@/components/ui/button";
import { VersionArrow } from "@/components/shared/version-arrow";
import { TaskStatusBadge } from "@/features/updates/update-status";
import {
  isSiteDownRecovery,
  SITE_DOWN_RECOVERY_FALLBACK_DETAIL,
} from "@/features/updates/summarize";

// Re-export siteNameMap so existing callers (e.g. $runId.tsx) don't need an
// import path change. Surface C agents may update their imports to ./summarize.
// eslint-disable-next-line react-refresh/only-export-components -- intentional re-export bridge; callers that own this import will move to ./summarize in Surface C
export { siteNameMap } from "./summarize";

const TASK_COLUMN_COUNT = 5;

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
              <UpdateTaskRow
                key={task.id}
                task={task}
                siteName={siteNames?.get(task.site_id)}
              />
            ))}
          </TableBody>
        </Table>
      </div>
    </div>
  );
}

// One task row plus, when the agent reported a non-empty error/log, a
// disclosure toggle that reveals a second full-width row with the full text.
// `task.error` carries the agent's complete diagnostic (e.g. why a rollback
// was triggered), while `task.detail` stays the short generic status string
// — both are surfaced, but the full log is one click away instead of only
// ever being available truncated in the Detail cell.
function UpdateTaskRow({
  task,
  siteName,
}: {
  task: UpdateTask;
  siteName?: string;
}) {
  const [open, setOpen] = useState(false);
  const hasLog = Boolean(task.error && task.error.trim().length > 0);
  const logId = `update-task-log-${task.id}`;
  const siteDown = isSiteDownRecovery(task.status, task.detail, task.error);

  return (
    <>
      <TableRow data-testid="update-task-row">
        <TableCell className="font-medium">
          {siteName ?? short(task.site_id)}
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
            <VersionArrow from={task.from_version} to={task.to_version} />
          ) : (
            <span className="text-muted-foreground text-xs">{"–"}</span>
          )}
        </TableCell>
        <TableCell>
          <TaskStatusBadge task={task} />
        </TableCell>
        <TableCell className="max-w-[220px] text-xs text-muted-foreground">
          <div className="flex flex-col items-start gap-1">
            {siteDown ? (
              // GH #210 — never truncate this: it's the worst-case rollback
              // failure (site-wide fatal, undeliverable rollback, automatic
              // filesystem recovery attempted) and needs to read as its own
              // actionable condition, not a generic status string.
              <span
                role="alert"
                className="flex items-start gap-1.5 whitespace-normal text-destructive-subtle-fg"
              >
                <AlertTriangle
                  aria-hidden="true"
                  className="mt-0.5 size-3.5 shrink-0"
                />
                <span>{task.detail ?? SITE_DOWN_RECOVERY_FALLBACK_DETAIL}</span>
              </span>
            ) : (
              <span
                className="max-w-full min-w-0 truncate font-mono"
                title={task.detail}
              >
                {task.detail ??
                  (hasLog ? "See log for details" : <span aria-hidden="true">{"–"}</span>)}
              </span>
            )}
            {hasLog ? (
              <Button
                type="button"
                variant="ghost"
                size="sm"
                onClick={() => setOpen((v) => !v)}
                aria-expanded={open}
                aria-controls={logId}
                className="h-auto px-1.5 py-0.5 font-sans text-xs"
              >
                {open ? (
                  <ChevronDown aria-hidden="true" className="size-3.5" />
                ) : (
                  <ChevronRight aria-hidden="true" className="size-3.5" />
                )}
                {open ? "Hide log" : "View log"}
              </Button>
            ) : null}
          </div>
        </TableCell>
      </TableRow>
      {hasLog && open ? (
        <TableRow data-testid="update-task-log-row">
          <TableCell colSpan={TASK_COLUMN_COUNT} className="bg-muted/20 p-0">
            {/* task.error is a defined non-empty string here; hasLog narrows it. */}
            <TaskLogPanel id={logId} error={task.error ?? ""} />
          </TableCell>
        </TableRow>
      ) : null}
    </>
  );
}

// The full agent log for one task: monospace, newline-preserving, scrollable
// past a reasonable height, and copyable in one click.
function TaskLogPanel({ id, error }: { id: string; error: string }) {
  const [copied, setCopied] = useState(false);

  const onCopy = () => {
    if (typeof navigator === "undefined" || !navigator.clipboard) return;
    void navigator.clipboard.writeText(error).then(() => {
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1500);
    });
  };

  return (
    <div id={id} className="space-y-2 border-t border-border p-3">
      <div className="flex items-center justify-between gap-2">
        <h3 className="text-xs font-medium uppercase tracking-wide text-muted-foreground">
          Agent log
        </h3>
        <Button
          type="button"
          variant="ghost"
          size="sm"
          onClick={onCopy}
          aria-label="Copy agent log"
        >
          {copied ? (
            <>
              <Check aria-hidden="true" className="size-3.5" />
              Copied
            </>
          ) : (
            <>
              <Copy aria-hidden="true" className="size-3.5" />
              Copy
            </>
          )}
        </Button>
      </div>
      <pre className="max-h-64 overflow-auto rounded-md border border-border bg-muted/50 p-3 font-mono text-xs whitespace-pre-wrap break-words text-foreground">
        {error}
      </pre>
    </div>
  );
}

function short(id: string): string {
  return id.length > 8 ? `${id.slice(0, 8)}…` : id;
}
