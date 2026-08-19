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

type ToneCfg = { tone: StatusTone; label: string; pulse?: boolean };

// Defence in depth against version skew: a self-hosted control plane can be
// newer than the browser bundle it serves and emit a status literal this
// build has never heard of. `tsc` already refuses to compile TASK_TONE/
// RUN_TONE below if a status is added to the generated union and this file
// is not updated to match — that is the exhaustiveness check, and it is real
// coverage. This fallback is the OTHER case: the type says every key is
// covered, but the value actually on the wire is not one `tsc` ever checked,
// so the lookup below is deliberately re-typed as partial for the read.
const UNKNOWN_TONE: ToneCfg = { tone: "muted", label: "Unknown" };

function toneFor<S extends string>(
  map: Record<S, ToneCfg>,
  status: S,
): ToneCfg {
  return (map as Partial<Record<S, ToneCfg>>)[status] ?? UNKNOWN_TONE;
}

const TASK_TONE: Record<TaskStatus, ToneCfg> = {
  succeeded: { tone: "success", label: "Succeeded" },
  failed: { tone: "destructive", label: "Failed" },
  rolled_back: { tone: "warning", label: "Rolled back" },
  running: { tone: "info", label: "Running", pulse: true },
  pending: { tone: "muted", label: "Pending" },
  skipped: { tone: "muted", label: "Skipped" },
  // GH #255 Phase 2: never dispatched because its run halted first. GH #336
  // relabels it: nobody cancelled this, the control plane withheld it, and
  // nothing was ever sent to the site.
  cancelled: { tone: "muted", label: "Not attempted" },
  // GH #463: the parent run has not reached its scheduled_at yet. Nothing
  // has happened. Same neutral tone as "Pending"; the distinct "Scheduled"
  // label (paired with the run's scheduled_at shown elsewhere) is what
  // signals "waiting for a future time" rather than "queued now".
  scheduled: { tone: "muted", label: "Scheduled" },
  // GH #463: the parent run expired before dispatching, so this task was
  // never attempted either — same story as `cancelled`, a different cause.
  // Kept muted (never attempted is not a failure) but with its own label so
  // it reads distinctly from "Not attempted" (an operator's choice) rather
  // than collapsing the two together.
  expired: { tone: "muted", label: "Expired" },
};

export function TaskStatusBadge({
  task,
}: {
  task: Pick<UpdateTask, "status" | "detail" | "error" | "target_type">;
}) {
  // GH #210: the site-wide-fatal + undeliverable-rollback + auto-filesystem-
  // recovery condition is distinct and more severe than an ordinary failure
  // or rollback. The backend reports it through detail/error text on the
  // existing failed/rolled_back statuses, so surface it as its own chip
  // instead of the generic "Failed"/"Rolled back" label. This is a LABEL
  // choice only; retryability is a server field, never this predicate.
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
  const cfg = toneFor(TASK_TONE, task.status);
  return (
    <StatusChip
      tone={cfg.tone}
      label={cfg.label}
      pulse={cfg.pulse ?? false}
    />
  );
}

const RUN_TONE: Record<RunStatus, ToneCfg> = {
  pending: { tone: "muted", label: "Pending" },
  running: { tone: "info", label: "Running", pulse: true },
  completed: { tone: "success", label: "Completed" },
  // GH #255 Phase 2: a wave failed to prove itself; the run stopped early.
  // Destructive tone on purpose: this is the signal that a bad agent build
  // was caught and needs the operator's attention.
  halted: { tone: "destructive", label: "Halted" },
  // GH #463: waiting for scheduled_at. No site has been contacted yet.
  // Neutral, not an error — mirrors the task-level "Scheduled" tone.
  scheduled: { tone: "muted", label: "Scheduled" },
  // GH #463: transient, held only while the dispatcher enqueues the run's
  // tasks. An operator will rarely see this; `info` + pulse mirrors
  // "Running" because something genuinely is happening right now.
  dispatching: { tone: "info", label: "Dispatching", pulse: true },
  // GH #463: terminal — the run came due more than the grace window ago
  // while the control plane was unavailable, so it was never dispatched.
  // No site was contacted. This is a MISSED SCHEDULE, not a failed update,
  // so it deliberately does not use "destructive" (that would read as "an
  // update broke a site", which is false here). `warning` keeps it visible
  // enough that an operator notices the schedule didn't fire, without the
  // implication that anything on a site went wrong.
  expired: { tone: "warning", label: "Expired" },
};

export function RunStatusBadge({ status }: { status: RunStatus }) {
  const cfg = toneFor(RUN_TONE, status);
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
