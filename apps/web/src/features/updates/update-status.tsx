import type { UpdateRun, UpdateTask } from "@wpmgr/api";

import { StatusChip } from "@/components/status/status-chip";
import type { StatusTone } from "@/components/status/status-dot";
import {
  isAgentNotEligible,
  isSiteDownRecovery,
  SITE_DOWN_RECOVERY_LABEL,
} from "./summarize";

type TaskStatus = UpdateTask["status"];
type RunStatus = UpdateRun["status"];

const TASK_TONE: Record<TaskStatus, { tone: StatusTone; label: string; pulse?: boolean }> = {
  succeeded: { tone: "success", label: "Succeeded" },
  failed: { tone: "destructive", label: "Failed" },
  rolled_back: { tone: "warning", label: "Rolled back" },
  running: { tone: "info", label: "Running", pulse: true },
  pending: { tone: "muted", label: "Pending" },
  skipped: { tone: "muted", label: "Skipped" },
  // GH #255 Phase 2: never dispatched because its run halted first.
  cancelled: { tone: "muted", label: "Cancelled" },
};

export function TaskStatusBadge({
  task,
}: {
  task: Pick<UpdateTask, "status" | "detail" | "error" | "target_type">;
}) {
  // GH #210 — the site-wide-fatal + undeliverable-rollback + auto-filesystem-
  // recovery condition is distinct and more severe than an ordinary failure
  // or rollback. The backend reports it through detail/error text on the
  // existing failed/rolled_back statuses, so surface it as its own chip
  // instead of the generic "Failed"/"Rolled back" label.
  if (isSiteDownRecovery(task.status, task.detail, task.error)) {
    return <StatusChip tone="destructive" label={SITE_DOWN_RECOVERY_LABEL} />;
  }
  // GH #255 Phase 2: the agent self-update channel's own vocabulary. A
  // `running` agent task has already been armed (beat 1 acknowledged) and is
  // waiting on the site's own cron to apply it and phone home (beat 3); the
  // generic "Running" label would suggest the control plane is doing
  // something right now, when really nothing further happens here until the
  // site reports back. "Not eligible" reads as informational, never as an
  // error: the site updates through its own channel.
  if (task.target_type === "agent") {
    if (task.status === "running") {
      return <StatusChip tone="info" label="Awaiting confirmation" pulse />;
    }
    if (isAgentNotEligible(task.target_type, task.status, task.detail)) {
      return <StatusChip tone="muted" label="Not eligible" />;
    }
  }
  const cfg = TASK_TONE[task.status];
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
  // GH #255 Phase 2: a wave failed to prove itself; the run stopped early.
  // Destructive tone on purpose: this is the signal that a bad agent build
  // was caught and needs the operator's attention.
  halted: { tone: "destructive", label: "Halted" },
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
