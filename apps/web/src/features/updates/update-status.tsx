import type { UpdateRun, UpdateTask } from "@wpmgr/api";

import { Badge } from "@/components/ui/badge";

type TaskStatus = UpdateTask["status"];
type RunStatus = UpdateRun["status"];

// Color-coding per the M3 contract:
//   succeeded = green, failed = red, rolled_back = amber, running = blue/pulse,
//   skipped/pending = muted.
const TASK_BADGE: Record<
  TaskStatus,
  { label: string; className: string; pulse?: boolean }
> = {
  succeeded: {
    label: "Succeeded",
    className:
      "border-transparent bg-green-100 text-green-800 dark:bg-green-950 dark:text-green-300",
  },
  failed: {
    label: "Failed",
    className:
      "border-transparent bg-red-100 text-red-800 dark:bg-red-950 dark:text-red-300",
  },
  rolled_back: {
    label: "Rolled back",
    className:
      "border-transparent bg-amber-100 text-amber-800 dark:bg-amber-950 dark:text-amber-300",
  },
  running: {
    label: "Running",
    className:
      "border-transparent bg-blue-100 text-blue-800 dark:bg-blue-950 dark:text-blue-300",
    pulse: true,
  },
  pending: {
    label: "Pending",
    className:
      "border-transparent bg-[var(--color-muted)] text-[var(--color-muted-foreground)]",
  },
  skipped: {
    label: "Skipped",
    className:
      "border-transparent bg-[var(--color-muted)] text-[var(--color-muted-foreground)]",
  },
};

export function TaskStatusBadge({ status }: { status: TaskStatus }) {
  const cfg = TASK_BADGE[status];
  return (
    <Badge
      className={cfg.className}
      aria-label={`Status: ${cfg.label}`}
    >
      {cfg.pulse ? (
        <span
          aria-hidden="true"
          className="size-1.5 animate-pulse rounded-full bg-current"
        />
      ) : null}
      {cfg.label}
    </Badge>
  );
}

const RUN_BADGE: Record<RunStatus, { label: string; variant: "default" | "secondary" | "success" }> = {
  pending: { label: "Pending", variant: "secondary" },
  running: { label: "Running", variant: "default" },
  completed: { label: "Completed", variant: "success" },
};

export function RunStatusBadge({ status }: { status: RunStatus }) {
  const cfg = RUN_BADGE[status];
  return <Badge variant={cfg.variant}>{cfg.label}</Badge>;
}

/** Count task statuses and derive a 0..total "settled" progress figure. */
export function summarizeTasks(tasks: UpdateTask[]): {
  total: number;
  done: number;
  counts: Record<TaskStatus, number>;
} {
  const counts: Record<TaskStatus, number> = {
    pending: 0,
    running: 0,
    succeeded: 0,
    failed: 0,
    rolled_back: 0,
    skipped: 0,
  };
  for (const task of tasks) counts[task.status] += 1;
  const done =
    counts.succeeded + counts.failed + counts.rolled_back + counts.skipped;
  return { total: tasks.length, done, counts };
}
