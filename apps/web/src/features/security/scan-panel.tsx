import { ScanLine, ShieldCheck, ShieldX } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { PageError } from "@/components/feedback";
import { LiveIndicator } from "@/components/shared/live-indicator";
import { toast } from "@/components/toast/use-toast-helpers";
import { relativeTime } from "@/lib/utils";

import {
  useScanRuns,
  useScanRun,
  useStartScan,
  isLiveScan,
  type ScanRun,
  type ScanStatus,
} from "./use-scan";
import { ScanFindingsTable } from "./scan-findings-table";

// S3 — Integrity scan panel.
//
// Layout (inside the security tab "Integrity scan" section):
//   [header toolbar]
//     "Run scan" button + description blurb
//   [latest run status row]  ← only when a run exists
//   [findings table]         ← only when a done run exists
//
// Design rules applied:
//   - Verb-first button labels ("Run scan").
//   - No card wrapper (DESIGN "borders over shadows; never nest cards").
//   - font-mono on paths/hashes; tabular-nums on file counts.
//   - LiveIndicator while queued/scanning/diffing.
//   - StatusChip-style status label beside the indicator.

// ---------------------------------------------------------------------------
// Public entry point
// ---------------------------------------------------------------------------

export function ScanPanel({ siteId }: { siteId: string }) {
  const runs = useScanRuns(siteId);

  if (runs.isPending) {
    return <ScanPanelSkeleton />;
  }

  if (runs.isError) {
    return (
      <PageError
        what="Could not load scan history."
        why={runs.error instanceof Error ? runs.error.message : "Unknown error"}
        onRetry={() => void runs.refetch()}
        retryLabel="Reload scans"
      />
    );
  }

  const latestRun = runs.data[0] ?? null;

  return (
    <div className="space-y-6">
      {/* ── Header row: description + action ── */}
      <ScanHeaderRow siteId={siteId} />

      {/* ── Latest run status ── */}
      {latestRun ? (
        <LatestRunStatus siteId={siteId} run={latestRun} />
      ) : (
        <PreScanEmpty />
      )}

      {/* ── Findings table — rendered once we have a completed run ── */}
      {latestRun && latestRun.status === "done" ? (
        <ScanFindingsTable siteId={siteId} runId={latestRun.id} />
      ) : null}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Header row: description + "Run scan" button
// ---------------------------------------------------------------------------

function ScanHeaderRow({ siteId }: { siteId: string }) {
  const runs = useScanRuns(siteId);
  const start = useStartScan(siteId);

  // Disable while a run is in progress.
  const isInProgress = (runs.data ?? []).some((r) => isLiveScan(r.status));

  function handleStartScan() {
    start.mutate(undefined, {
      onSuccess: () => {
        toast.success("Scan started.", {
          description: "Comparing WordPress core files against official checksums.",
        });
      },
      onError: (err: Error) => {
        toast.error("Could not start scan.", { description: err.message });
      },
    });
  }

  return (
    <div className="flex flex-col gap-3 sm:flex-row sm:items-start sm:justify-between">
      <div className="space-y-1">
        <p className="text-sm text-[var(--color-foreground)]">
          Compares your WordPress core files against the official WordPress.org
          checksums. Detects modified, missing, or unknown injected files.
        </p>
      </div>
      <Button
        type="button"
        size="sm"
        className="shrink-0"
        disabled={start.isPending || isInProgress}
        aria-busy={start.isPending || isInProgress}
        onClick={handleStartScan}
      >
        <ScanLine aria-hidden="true" className="size-3.5" />
        {start.isPending ? "Starting..." : "Run scan"}
      </Button>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Latest run status row — polls via useScanRun while in-progress
// ---------------------------------------------------------------------------

function LatestRunStatus({
  siteId,
  run: listRun,
}: {
  siteId: string;
  run: ScanRun;
}) {
  // Subscribe to the live-polling run detail so status advances in real time.
  const { data: run } = useScanRun(siteId, listRun.id);
  const current = run ?? listRun;

  const isLive = isLiveScan(current.status);

  return (
    <div
      className="flex flex-wrap items-center gap-x-6 gap-y-2 rounded-lg border border-[var(--color-border)] px-4 py-3"
      aria-live="polite"
      aria-label="Latest scan status"
    >
      {/* Status indicator */}
      <div className="flex items-center gap-2">
        {isLive ? (
          <LiveIndicator
            state="connecting"
            label={statusLabel(current.status)}
          />
        ) : (
          <StatusIndicator status={current.status} />
        )}
      </div>

      {/* Files scanned — tabular-nums */}
      {current.files_scanned != null ? (
        <span className="text-xs text-[var(--color-muted-foreground)] tabular-nums">
          {current.files_scanned.toLocaleString()} files scanned
        </span>
      ) : null}

      {/* WordPress version */}
      {current.wp_version ? (
        <span className="text-xs text-[var(--color-muted-foreground)]">
          WordPress{" "}
          <span className="font-mono">{current.wp_version}</span>
        </span>
      ) : null}

      {/* Finding counts (only when done) */}
      {current.status === "done" ? (
        <FindingCountsSummary counts={current.finding_counts} />
      ) : null}

      {/* Error message */}
      {current.status === "failed" && current.error ? (
        <span className="text-xs text-[var(--color-destructive)] truncate max-w-xs">
          {current.error}
        </span>
      ) : null}

      {/* Timestamp */}
      <span className="ml-auto text-xs text-[var(--color-muted-foreground)] tabular-nums">
        {relativeTime(current.created_at) ?? ""}
      </span>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Finding counts summary chips (inline, not full table)
// ---------------------------------------------------------------------------

function FindingCountsSummary({
  counts,
}: {
  // The API can send this as null (a nil Go map marshals to JSON null) for a
  // run with no findings or one still in flight; guard before Object.values.
  counts: Record<string, number> | null | undefined;
}) {
  const total = Object.values(counts ?? {}).reduce((sum, n) => sum + n, 0);

  if (total === 0) {
    return (
      <span className="inline-flex items-center gap-1.5 text-xs text-[var(--color-success,_oklch(0.62_0.18_145))]">
        <ShieldCheck aria-hidden="true" className="size-3.5 shrink-0" />
        No issues found
      </span>
    );
  }

  return (
    <span className="inline-flex items-center gap-1.5 text-xs text-[var(--color-destructive)]">
      <ShieldX aria-hidden="true" className="size-3.5 shrink-0" />
      <span className="tabular-nums font-mono">{total}</span> issue{total !== 1 ? "s" : ""} found
    </span>
  );
}

// ---------------------------------------------------------------------------
// Status indicator (terminal states — no pulse)
// ---------------------------------------------------------------------------

const STATUS_LABEL: Record<ScanStatus, string> = {
  queued: "Queued",
  scanning: "Scanning",
  diffing: "Comparing checksums",
  done: "Complete",
  failed: "Failed",
};

function statusLabel(status: ScanStatus): string {
  return STATUS_LABEL[status];
}

const STATUS_DOT: Record<ScanStatus, string> = {
  queued: "bg-[var(--color-muted-foreground)]",
  scanning: "bg-[var(--color-warning,_oklch(0.8_0.17_85))]",
  diffing: "bg-[var(--color-warning,_oklch(0.8_0.17_85))]",
  done: "bg-[var(--color-success,_oklch(0.62_0.18_145))]",
  failed: "bg-[var(--color-destructive)]",
};

const STATUS_TEXT: Record<ScanStatus, string> = {
  queued: "text-[var(--color-muted-foreground)]",
  scanning: "text-[var(--color-warning,_oklch(0.8_0.17_85))]",
  diffing: "text-[var(--color-warning,_oklch(0.8_0.17_85))]",
  done: "text-[var(--color-foreground)]",
  failed: "text-[var(--color-destructive)]",
};

function StatusIndicator({ status }: { status: ScanStatus }) {
  return (
    <span
      className={`inline-flex items-center gap-1.5 text-xs font-medium ${STATUS_TEXT[status]}`}
    >
      <span
        aria-hidden="true"
        className={`size-1.5 shrink-0 rounded-full ${STATUS_DOT[status]}`}
      />
      {statusLabel(status)}
    </span>
  );
}

// ---------------------------------------------------------------------------
// Pre-scan empty state (before any run exists)
// ---------------------------------------------------------------------------

function PreScanEmpty() {
  return (
    <p className="text-sm text-[var(--color-muted-foreground)]">
      Run a scan to check core file integrity.
    </p>
  );
}

// ---------------------------------------------------------------------------
// Loading skeleton
// ---------------------------------------------------------------------------

function ScanPanelSkeleton() {
  return (
    <div
      role="status"
      aria-busy="true"
      aria-label="Loading scan history"
      className="space-y-4"
    >
      <span className="sr-only">Loading scan history</span>
      <div className="flex items-start justify-between gap-4">
        <div className="space-y-2 flex-1">
          <Skeleton className="h-3 w-full max-w-md" />
          <Skeleton className="h-3 w-2/3 max-w-xs" />
        </div>
        <Skeleton className="h-8 w-24 shrink-0" />
      </div>
      <Skeleton className="h-12 w-full rounded-lg" />
    </div>
  );
}
