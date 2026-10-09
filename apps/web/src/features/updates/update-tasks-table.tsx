import { useState } from "react";
import { AlertTriangle, ChevronDown, ChevronRight } from "lucide-react";

import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { VersionArrow } from "@/components/shared/version-arrow";
import { TaskStatusBadge } from "@/features/updates/update-status";
import {
  isRedirectFailure,
  siteDownFallbackDetail,
  siteDownKind,
} from "@/features/updates/summarize";
import { TaskLogPanel } from "@/features/updates/task-log-panel";
import {
  isRetrySelectable,
  notRetryableReason,
  taskSelectLabel,
  taskSiteLabel,
  type RetryTask,
} from "@/features/updates/retry-contract";

// Re-export siteNameMap so existing callers (e.g. $runId.tsx) don't need an
// import path change. Surface C agents may update their imports to ./summarize.
// eslint-disable-next-line react-refresh/only-export-components -- intentional re-export bridge; callers that own this import will move to ./summarize in Surface C
export { siteNameMap } from "./summarize";

const BASE_COLUMN_COUNT = 5;

/**
 * GH #336: the retry selection this table renders, or undefined for a purely
 * read-only table (which is exactly how it renders when the control plane does
 * not speak the retry contract, or the operator may not start runs).
 */
export interface TaskTableSelection {
  isSelected: (taskId: string) => boolean;
  toggle: (taskId: string) => void;
  setAllSelectable: (next: boolean) => void;
  allSelectableSelected: boolean;
  someSelectableSelected: boolean;
}

// Live table of per-(site, target) update tasks. Rows reflect whatever is in
// the run-detail query cache, which the SSE stream patches in place.
export function UpdateTasksTable({
  tasks,
  siteNames,
  selection,
}: {
  tasks: RetryTask[];
  // Legacy lookup of site id -> friendly name, used only when the control
  // plane does not send `site_name` on the task row. It is built from the
  // sites list, which the control plane caps at 50 rows, so it must never be
  // the primary source of site identity and never drives selection.
  siteNames?: Map<string, string>;
  selection?: TaskTableSelection;
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
              {selection ? (
                <TableHead className="w-10">
                  <Checkbox
                    aria-label={
                      selection.allSelectableSelected
                        ? "Clear selection"
                        : "Select all retryable updates"
                    }
                    checked={selection.allSelectableSelected}
                    ref={(el) => {
                      if (el) el.indeterminate = selection.someSelectableSelected;
                    }}
                    onChange={(e) =>
                      selection.setAllSelectable(e.currentTarget.checked)
                    }
                  />
                </TableHead>
              ) : null}
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
                siteNames={siteNames}
                selection={selection}
              />
            ))}
          </TableBody>
        </Table>
      </div>
    </div>
  );
}

// One task row plus, when the task has a non-empty error/log, a disclosure
// toggle that reveals a second full-width row with the full text.
// `task.detail` is the sentence the control plane wrote for the operator and is
// always shown in full; `task.error` carries the raw diagnostic behind it (the
// agent's log, or the reply the site sent), which is one click away.
function UpdateTaskRow({
  task,
  siteNames,
  selection,
}: {
  task: RetryTask;
  siteNames?: Map<string, string>;
  selection?: TaskTableSelection;
}) {
  const [open, setOpen] = useState(false);
  const hasLog = Boolean(task.error && task.error.trim().length > 0);
  const logId = `update-task-log-${task.id}`;
  const columnCount = BASE_COLUMN_COUNT + (selection ? 1 : 0);
  const selectable = isRetrySelectable(task);
  const checked = selection?.isSelected(task.id) ?? false;
  // GH #210 display treatment only. This reads the agent's own prose and is
  // NEVER a safety or selection authority: whether a task may be retried is
  // the server's `retryable` field and nothing else. The target type is part
  // of the question: WordPress core has no automatic recovery (GH #415).
  const siteDown = siteDownKind(task);
  // GH #755 slice 1: a redirect failure's task.detail is the full operator
  // message (names the target + remedy). It keeps its own warning treatment.
  const redirectFailure =
    !siteDown && isRedirectFailure(task.status, task.detail, task.error);
  // GH #255 Phase 2: an armed agent task has no detail text until beat 3
  // resolves it (nothing has happened on the site yet beyond scheduling the
  // cron event that will apply the upgrade), so the generic empty-cell
  // fallback would read as a stall rather than the expected wait.
  const awaitingConfirmation =
    task.target_type === "agent" && task.status === "running";

  return (
    <>
      <TableRow
        data-testid="update-task-row"
        data-state={selection && checked ? "selected" : undefined}
      >
        {selection ? (
          <TableCell className="w-10">
            {selectable ? (
              <Checkbox
                aria-label={taskSelectLabel(task, siteNames)}
                checked={checked}
                onChange={() => selection.toggle(task.id)}
                onClick={(e) => e.stopPropagation()}
              />
            ) : (
              // No dead control: a disabled checkbox with no stated cause is
              // worse than none, and some screen-reader modes skip disabled
              // controls entirely. The visible cause is in the Status column;
              // this states it for assistive technology in place of the
              // control that is deliberately absent.
              <span className="sr-only">{notRetryableReason(task)}</span>
            )}
          </TableCell>
        ) : null}
        <TableCell className="font-medium">
          {taskSiteLabel(task, siteNames)}
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
            // No version pair to show yet. An aria-hidden placeholder keeps the
            // cell from collapsing without reading anything out.
            <span aria-hidden="true" className="text-muted-foreground text-xs">
              {"..."}
            </span>
          )}
        </TableCell>
        <TableCell>
          <TaskStatusBadge task={task} />
        </TableCell>
        <TableCell className="max-w-sm text-xs text-muted-foreground">
          <div className="flex flex-col items-start gap-1">
            {siteDown ? (
              // GH #210: never truncate this. It is the worst-case rollback
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
                <span>{task.detail ?? siteDownFallbackDetail(siteDown)}</span>
              </span>
            ) : redirectFailure ? (
              // GH #755 slice 1: an actionable config mismatch, not a
              // destructive condition — its own non-truncating treatment,
              // distinct from the site-down alert above.
              <span role="alert" className="whitespace-normal text-warning-subtle-fg">
                {task.detail}
              </span>
            ) : (
              // GH #679: the detail is the sentence the control plane wrote
              // for the operator (the raw reply is in `error`, behind the log
              // toggle below), so it wraps and is never cut to one line.
              <span className="max-w-full min-w-0 whitespace-normal break-words">
                {task.detail ??
                  (awaitingConfirmation
                    ? "Waiting for the upgraded agent to report back"
                    : hasLog
                      ? "See log for details"
                      : null)}
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
          <TableCell colSpan={columnCount} className="bg-muted/20 p-0">
            {/* task.error is a defined non-empty string here; hasLog narrows it. */}
            <TaskLogPanel id={logId} error={task.error ?? ""} />
          </TableCell>
        </TableRow>
      ) : null}
    </>
  );
}
