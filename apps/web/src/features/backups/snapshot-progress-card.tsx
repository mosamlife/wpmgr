/**
 * SnapshotProgressCard — M5.6 v2 progress UI.
 *
 * Design (per docs/research/backup-progress-{ux-patterns,implementation}.md):
 *   - Vertical PhaseStepper on the left showing all 5 pipeline phases
 *   - Active-phase detail on the right: shadcn Progress (with smooth
 *     translateX-based fill, or shimmer for indeterminate), live counters
 *     animated with NumberFlow, ETA, current file/artifact
 *   - Live/Polling indicator dot in the card header
 *   - On failed: bar freezes, active step turns destructive, surface
 *     phase_detail.message
 *
 * Both this card AND `InlineSnapshotProgress` (for the site detail table
 * row) consume the same `useBackupStream` SSE patches and `formatProgress`
 * helper — no Context, no shared state, just a shared canonical view of
 * the snapshot.
 */
import NumberFlow from "@number-flow/react";
import { useEffect, useMemo } from "react";

import type { BackupSnapshot } from "@wpmgr/api";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Progress } from "@/components/ui/progress";
import { formatBytes, relativeTime } from "@/lib/utils";
import { useNow } from "@/lib/use-now";

import { LiveIndicator } from "@/components/shared/live-indicator";

import { buildStepperPhases, formatProgress, isRestorePhase } from "./format-progress";
import { PhaseStepper } from "./phase-stepper";
import { formatElapsed, useEta, useEtaSamples } from "./use-eta";
import { useBackupStream } from "./use-backup-stream";

export function SnapshotProgressCard({ snapshot }: { snapshot: BackupSnapshot }) {
  // now ticks every second; the elapsed display reads state, not the live
  // clock, keeping this render pure (react-hooks/purity).
  const now = useNow(1000);

  const stream = useBackupStream(snapshot.id);
  const fp = useMemo(() => formatProgress(snapshot), [snapshot]);
  const isRestore = isRestorePhase(fp.phase);

  // Sliding-window ETA samples. Push the current percent on every render
  // (which fires whenever the SSE event lands and patches the cache).
  const samplesRef = useEtaSamples();
  const samples = useMemo(() => {
    if (fp.percent === null) return [];
    return samplesRef.push(fp.percent);
    // We intentionally do NOT memoize on samplesRef — the ref is stable; the
    // dependency is `fp.percent` so we sample only when % actually changes.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [fp.percent]);
  const etaLabel = useEta(samples);

  // Elapsed (seconds since created_at) ticks via `now` (~1/sec for live
  // backups). Terminal snapshots have finished_at set so the value is stable
  // regardless of `now` advancing.
  const elapsedLabel = useMemo(() => {
    const createdMs = Date.parse(snapshot.created_at);
    if (!Number.isFinite(createdMs)) return null;
    const finishedMs = snapshot.finished_at
      ? Date.parse(snapshot.finished_at)
      : now;
    return formatElapsed((finishedMs - createdMs) / 1000);
  }, [snapshot.created_at, snapshot.finished_at, now]);

  // For terminal snapshots reset the ETA buffer once so the next backup
  // starts clean.
  useEffect(() => {
    if (fp.isTerminal) samplesRef.reset();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [fp.isTerminal]);

  const stepperPhases = buildStepperPhases(fp.phase, snapshot.status);

  const indicatorLabel = stream.isLive
    ? "Live (SSE)"
    : stream.failureCount > 0
      ? `Polling (SSE failed ×${stream.failureCount})`
      : "Polling";

  const counterDone = fp.filesDone ?? fp.chunksDone ?? 0;
  const counterTotal = fp.filesTotal ?? fp.chunksTotal ?? 0;
  const counterUnit = fp.phase === "archiving_files" ? "files" : "chunks";

  return (
    <Card data-build="sse-card-v2" data-snapshot-id={snapshot.id}>
      <CardHeader>
        <div className="flex items-start justify-between gap-3">
          <div>
            <CardTitle>
              {isRestore ? "Restore in progress" : "Live progress"}
            </CardTitle>
            <CardDescription>
              {fp.label}
              {fp.artifact ? ` — ${fp.artifact}` : ""}
            </CardDescription>
          </div>
          <span role="status" aria-label={indicatorLabel} title={indicatorLabel}>
            <LiveIndicator
              state={stream.isLive ? "live" : "connecting"}
              label={stream.isLive ? "Live" : "Polling"}
            />
          </span>
        </div>
      </CardHeader>
      <CardContent className="grid gap-6 md:grid-cols-[1fr_2fr]">
        <PhaseStepper phases={stepperPhases} />

        <div className="space-y-4">
          {/* The bar — determinate for archiving/upload, indeterminate (shimmer) for opaque phases. */}
          <Progress
            value={fp.isTerminal && snapshot.status === "completed" ? 100 : fp.percent}
            label={`${fp.label} progress`}
            className={
              snapshot.status === "failed"
                ? "[&>div]:!bg-[var(--color-destructive)]"
                : ""
            }
          />

          {/* Counter line: animated numbers with byte total. Only shows if we have a meaningful counter. */}
          {counterTotal > 0 ? (
            <div className="flex items-baseline justify-between text-sm">
              <span className="font-mono">
                <NumberFlow value={counterDone} /> /{" "}
                <NumberFlow value={counterTotal} /> {counterUnit}
              </span>
              <span className="font-mono text-[var(--color-muted-foreground)]">
                {fp.percent !== null ? `${fp.percent}%` : ""}
              </span>
            </div>
          ) : null}

          {/* Bytes line — shown whenever we have one. */}
          {fp.bytesDone !== null && fp.bytesDone > 0 ? (
            <div className="text-sm text-[var(--color-muted-foreground)]">
              {formatBytes(fp.bytesDone)} processed
            </div>
          ) : null}

          {/* ETA — only when running with a measurable rate. */}
          {!fp.isTerminal && fp.percent !== null ? (
            <div className="text-sm text-[var(--color-muted-foreground)]">
              {etaLabel}
            </div>
          ) : null}

          {/* Elapsed time — always shown. */}
          {elapsedLabel ? (
            <div className="text-xs text-[var(--color-muted-foreground)]">
              Elapsed {elapsedLabel}
              {!fp.isTerminal ? " (live)" : ""}
            </div>
          ) : null}

          {/* Current file (during archiving) or current artifact (during encrypt/upload). */}
          {fp.currentFile ? (
            <p
              className="truncate font-mono text-xs text-[var(--color-muted-foreground)]"
              title={fp.currentFile}
            >
              {fp.currentFile}
            </p>
          ) : null}

          {/* Failure surface. */}
          {snapshot.status === "failed" && (fp.errorMessage || snapshot.error) ? (
            <p
              role="alert"
              className="text-sm text-[var(--color-destructive)]"
            >
              {fp.errorMessage ?? snapshot.error}
            </p>
          ) : null}

          {/* Last-update timestamp (small, secondary). */}
          <p className="text-[10px] uppercase tracking-wide text-[var(--color-muted-foreground)]">
            Updated{" "}
            {relativeTime(snapshot.progress_updated_at ?? snapshot.updated_at) ?? "—"}
          </p>
        </div>
      </CardContent>
    </Card>
  );
}
