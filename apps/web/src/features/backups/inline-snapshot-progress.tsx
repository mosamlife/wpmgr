/**
 * InlineSnapshotProgress — compact live progress for the site detail page
 * BackupsSection table cell. Constrained to ~280 px max width.
 *
 * Layout (per UX research dossier):
 *   ● ● ● ⊙ ○   Archiving · 1,847/3,201   57%
 *               ▓▓▓▓▓▓▓▓▓▓▓░░░░░░░░░░░░░░
 *               wp-content/uploads/.../header.jpg
 *
 * Uses the same `useBackupStream` + `formatProgress` as the full card, so
 * cache patches fan out to both surfaces with no extra coordination.
 */
import NumberFlow from "@number-flow/react";
import { useMemo } from "react";

import type { BackupSnapshot } from "@wpmgr/api";
import { Progress } from "@/components/ui/progress";

import { buildStepperPhases, formatProgress, isRestoreActive } from "./format-progress";
import { PhaseStepper } from "./phase-stepper";
import { useBackupStream } from "./use-backup-stream";

export function InlineSnapshotProgress({ snapshot }: { snapshot: BackupSnapshot }) {
  const stream = useBackupStream(snapshot.id);
  const fp = useMemo(() => formatProgress(snapshot), [snapshot]);

  // Render when:
  //   - Snapshot is actively backing up (status running/pending), OR
  //   - Snapshot is in a restore phase (status stays "completed" since restore
  //     is an overlay on a completed backup — but progress.phase is one of the
  //     restore phase values)
  const isRunningBackup = snapshot.status === "running" || snapshot.status === "pending";
  // Gate restore on the shared phase-based helper, NOT fp.isTerminal — a restore
  // overlays a "completed" snapshot, so isTerminal is always true during one.
  const isRunningRestore = isRestoreActive(snapshot);
  if (!isRunningBackup && !isRunningRestore) return null;

  const stepperPhases = buildStepperPhases(fp.phase, snapshot.status);

  const counterDone = fp.filesDone ?? fp.chunksDone ?? null;
  const counterTotal = fp.filesTotal ?? fp.chunksTotal ?? null;
  const counterUnit = fp.phase === "archiving_files" ? "files" : "chunks";

  return (
    <div
      data-build="sse-inline-v2"
      data-snapshot-id={snapshot.id}
      className="flex flex-col gap-1.5"
      role="status"
      aria-live="polite"
    >
      {/* Top line: compact phase dot-strip + active label + counter */}
      <PhaseStepper
        phases={stepperPhases}
        compact
        compactActiveLabel={fp.label}
      />

      {/* Counter + percent inline */}
      {counterTotal && counterTotal > 0 ? (
        <div className="flex items-baseline justify-between text-xs">
          <span className="font-mono text-[var(--color-muted-foreground)]">
            <NumberFlow value={counterDone ?? 0} /> /{" "}
            <NumberFlow value={counterTotal} /> {counterUnit}
          </span>
          {fp.percent !== null ? (
            <span className="font-mono text-[var(--color-muted-foreground)]">
              {fp.percent}%
            </span>
          ) : null}
        </div>
      ) : null}

      {/* Compact bar — same Radix Progress, just constrained width */}
      <Progress
        value={fp.percent}
        className="h-1 max-w-[260px]"
        label={`${fp.label} progress`}
      />

      {/* Current file or artifact — truncated */}
      {(fp.currentFile || fp.artifact) ? (
        <span
          className="block max-w-[260px] truncate font-mono text-[10px] text-[var(--color-muted-foreground)]"
          title={fp.currentFile ?? fp.artifact ?? ""}
        >
          {fp.currentFile ?? fp.artifact}
        </span>
      ) : null}

      {/* Tiny live indicator at the right of the row (top-line dots already give phase status; this confirms transport) */}
      {!stream.isLive && stream.failureCount > 0 ? (
        <span className="text-[10px] text-amber-600" title="SSE failed — using polling fallback">
          Polling (×{stream.failureCount})
        </span>
      ) : null}
    </div>
  );
}
