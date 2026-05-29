import type { UpdateRun, UpdateTask } from "@wpmgr/api";

import { StatusChip } from "@/components/status/status-chip";
import type { StatusTone } from "@/components/status/status-dot";

type TaskStatus = UpdateTask["status"];
type RunStatus = UpdateRun["status"];

const TASK_TONE: Record<TaskStatus, { tone: StatusTone; label: string; pulse?: boolean }> = {
  succeeded: { tone: "success", label: "Succeeded" },
  failed: { tone: "destructive", label: "Failed" },
  rolled_back: { tone: "warning", label: "Rolled back" },
  running: { tone: "info", label: "Running", pulse: true },
  pending: { tone: "muted", label: "Pending" },
  skipped: { tone: "muted", label: "Skipped" },
};

export function TaskStatusBadge({ status }: { status: TaskStatus }) {
  const cfg = TASK_TONE[status];
  return (
    <StatusChip
      tone={cfg.tone}
      label={cfg.label}
      pulse={cfg.pulse ?? false}
    />
  );
}

const RUN_TONE: Record<RunStatus, { tone: StatusTone; label: string; pulse?: boolean }> = {
  pending: { tone: "muted", label: "Pending" },
  running: { tone: "info", label: "Running", pulse: true },
  completed: { tone: "success", label: "Completed" },
};

export function RunStatusBadge({ status }: { status: RunStatus }) {
  const cfg = RUN_TONE[status];
  return (
    <StatusChip
      tone={cfg.tone}
      label={cfg.label}
      pulse={cfg.pulse ?? false}
    />
  );
}

// Re-export from summarize.ts so existing callers (e.g. $runId.tsx) still work
// without needing an import path change. Surface C agents may update their
// imports to ./summarize directly if preferred.
// eslint-disable-next-line react-refresh/only-export-components -- intentional re-export bridge; callers that own this import will move to ./summarize in Surface C
export { summarizeTasks } from "./summarize";
