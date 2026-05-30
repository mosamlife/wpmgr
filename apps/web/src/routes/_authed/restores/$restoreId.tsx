import { useEffect, useRef, type ReactNode } from "react";
import { createFileRoute, Link } from "@tanstack/react-router";

import {
  Card,
  CardContent,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { PageError } from "@/components/feedback";
import { PageHeader } from "@/components/shared/page-header";
import { DefinitionList } from "@/components/shared/definition-list";
import { StatusChip } from "@/components/status/status-chip";
import type { StatusTone } from "@/components/status/status-dot";
import { useBackup } from "@/features/backups/use-backups";
import { SnapshotProgressCard } from "@/features/backups/snapshot-progress-card";
import { useBackupStream } from "@/features/backups/use-backup-stream";
import {
  isRestoreActive,
  PHASE_LABEL,
} from "@/features/backups/format-progress";
import {
  useRestoreRun,
  useRestoreEvents,
  isRestoreTerminal,
  type RestoreRun,
  type RestoreStatus,
  type RestoreRunEvent,
} from "@/features/backups/use-restores";
import { useSite } from "@/features/sites/use-sites";
import { relativeTime } from "@/lib/utils";

export const Route = createFileRoute("/_authed/restores/$restoreId")({
  component: RestoreDetailPage,
});

// ---------------------------------------------------------------------------
// Status mappings
// ---------------------------------------------------------------------------

const RESTORE_STATUS_TONE: Record<RestoreStatus, StatusTone> = {
  queued: "muted",
  running: "info",
  completed: "success",
  failed: "destructive",
  rolled_back: "destructive",
};

const RESTORE_STATUS_LABEL: Record<RestoreStatus, string> = {
  queued: "Queued",
  running: "Running",
  completed: "Completed",
  failed: "Failed",
  rolled_back: "Rolled back",
};

// ---------------------------------------------------------------------------
// Page
// ---------------------------------------------------------------------------

function RestoreDetailPage() {
  const { restoreId } = Route.useParams();
  const {
    data: run,
    isPending,
    isError,
    error,
    refetch,
  } = useRestoreRun(restoreId);

  if (isPending) {
    return <RestoreDetailSkeleton />;
  }

  if (isError) {
    return (
      <section className="space-y-4">
        <PageHeader
          title={`Restore ${restoreId.slice(0, 8)}`}
          mono
          backTo={{ to: "/sites", label: "Sites" }}
        />
        <PageError
          what="Could not load restore run."
          why={error.message}
          onRetry={() => void refetch()}
          retryLabel="Reload restore"
        />
      </section>
    );
  }

  return <RestoreDetailView run={run} />;
}

function RestoreDetailSkeleton() {
  return (
    <div className="space-y-6" role="status" aria-label="Loading restore">
      <div className="space-y-2">
        <Skeleton className="h-3.5 w-32" />
        <Skeleton className="h-6 w-64" />
        <Skeleton className="h-4 w-96" />
      </div>
      <Skeleton className="h-48 w-full rounded-xl" />
      <Skeleton className="h-32 w-full rounded-xl" />
      <Skeleton className="h-64 w-full rounded-xl" />
    </div>
  );
}

/** Resolve name > email > short mono id for the triggered_by field. */
function resolveTriggeredBy(run: RestoreRun): ReactNode {
  if (run.triggered_by_name) return run.triggered_by_name;
  if (run.triggered_by_email) return run.triggered_by_email;
  if (run.triggered_by) {
    return (
      <code className="font-mono text-xs text-muted-foreground">
        {run.triggered_by.slice(0, 8)}
      </code>
    );
  }
  return "–";
}

function RestoreDetailView({ run }: { run: RestoreRun }) {
  const running = !isRestoreTerminal(run.status);

  // Resolve the originating site for the back-link and display name.
  const { data: site } = useSite(run.site_id);

  // Load the source snapshot for the live progress card.
  const { data: snapshotDetail } = useBackup(run.snapshot_id);
  const snapshot = snapshotDetail?.snapshot;

  // Open the SSE stream for this snapshot so live updates patch the cache.
  useBackupStream(run.snapshot_id);

  const restoreActiveOnSnapshot =
    snapshot !== undefined && isRestoreActive(snapshot);

  const subline = [
    run.mode ? `Mode: ${run.mode}` : null,
    run.components.length > 0
      ? `Components: ${run.components.join(", ")}`
      : null,
    relativeTime(run.started_at ?? run.created_at)
      ? `Started ${relativeTime(run.started_at ?? run.created_at)}`
      : null,
  ]
    .filter(Boolean)
    .join(" · ");

  return (
    <div className="space-y-6">
      <PageHeader
        title={`Restore ${run.id.slice(0, 8)}`}
        mono
        copyable={run.id}
        badges={
          <StatusChip
            tone={RESTORE_STATUS_TONE[run.status]}
            label={RESTORE_STATUS_LABEL[run.status]}
            pulse={run.status === "running"}
          />
        }
        subline={subline || undefined}
        backTo={
          site
            ? {
                to: "/sites/$siteId/backups",
                params: { siteId: run.site_id },
                label: `Back to ${site.name} backups`,
              }
            : {
                to: "/sites/$siteId/backups",
                params: { siteId: run.site_id },
                label: "Back to site backups",
              }
        }
      />

      {/* Outcome banner for terminal runs */}
      {!running && run.status !== "completed" ? (
        <div
          role="alert"
          className="flex items-center gap-3 rounded-md border border-[var(--color-destructive)]/40 bg-destructive-subtle p-3"
        >
          <StatusChip
            tone={RESTORE_STATUS_TONE[run.status]}
            label={RESTORE_STATUS_LABEL[run.status]}
          />
          {run.error ? (
            <span className="text-sm text-destructive-subtle-fg">
              {run.error}
            </span>
          ) : null}
        </div>
      ) : null}

      {!running && run.status === "completed" ? (
        <div
          role="status"
          className="flex items-center gap-3 rounded-md border border-[var(--color-success)]/40 bg-[var(--color-success)]/10 p-3"
        >
          <StatusChip tone="success" label="Completed" />
          <span className="text-sm text-muted-foreground">
            Restore finished successfully.
          </span>
        </div>
      ) : null}

      {/* Live progress: render SnapshotProgressCard while the source snapshot has an active restore phase */}
      {restoreActiveOnSnapshot && snapshot ? (
        <SnapshotProgressCard snapshot={snapshot} />
      ) : null}

      {/* Summary */}
      <Card>
        <CardHeader>
          <CardTitle>Summary</CardTitle>
        </CardHeader>
        <CardContent>
          <DefinitionList
            rows={[
              { label: "Mode", value: run.mode || "–" },
              {
                label: "Components",
                value:
                  run.components.length > 0
                    ? run.components.join(", ")
                    : "All",
              },
              { label: "Triggered by", value: resolveTriggeredBy(run) },
              { label: "Started", value: relativeTime(run.started_at) ?? "–" },
              {
                label: "Finished",
                value: relativeTime(run.finished_at) ?? "–",
              },
              {
                label: "Source snapshot",
                value: (
                  <Link
                    to="/backups/$snapshotId"
                    params={{ snapshotId: run.snapshot_id }}
                    className="font-mono text-xs underline-offset-2 hover:underline"
                  >
                    {run.snapshot_id.slice(0, 8)}
                  </Link>
                ),
              },
            ]}
          />
        </CardContent>
      </Card>

      {/* Phase log */}
      <PhaseLog restoreId={run.id} running={running} />
    </div>
  );
}

// ---------------------------------------------------------------------------
// Phase log
// ---------------------------------------------------------------------------

function PhaseLog({
  restoreId,
  running,
}: {
  restoreId: string;
  running: boolean;
}) {
  const {
    data: events,
    isError,
    error,
  } = useRestoreEvents(restoreId, { running });

  const scrollRef = useRef<HTMLDivElement>(null);

  // Auto-scroll to bottom while the run is still live.
  useEffect(() => {
    if (!running) return;
    const el = scrollRef.current;
    if (!el) return;
    el.scrollTop = el.scrollHeight;
  }, [events, running]);

  return (
    <Card>
      <CardHeader>
        <CardTitle>Phase log</CardTitle>
      </CardHeader>
      <CardContent>
        {isError ? (
          <PageError
            what="Could not load phase log."
            why={error.message}
          />
        ) : (
          <div
            ref={scrollRef}
            className="max-h-80 overflow-y-auto rounded-md border border-border bg-muted/30 p-3 font-mono text-xs"
            aria-label="Phase log"
            aria-live={running ? "polite" : undefined}
          >
            {!events || events.length === 0 ? (
              <p className="text-muted-foreground">
                {running
                  ? "Waiting for the first phase..."
                  : "No phase log entries recorded."}
              </p>
            ) : (
              <div className="space-y-1">
                {events.map((event) => (
                  <PhaseLogLine key={event.id} event={event} />
                ))}
              </div>
            )}
          </div>
        )}
      </CardContent>
    </Card>
  );
}

function PhaseLogLine({ event }: { event: RestoreRunEvent }) {
  const timeLabel = (() => {
    const d = new Date(event.occurred_at);
    if (Number.isNaN(d.getTime())) return event.occurred_at;
    const hh = String(d.getHours()).padStart(2, "0");
    const mm = String(d.getMinutes()).padStart(2, "0");
    const ss = String(d.getSeconds()).padStart(2, "0");
    return `${hh}:${mm}:${ss}`;
  })();

  const label =
    (PHASE_LABEL as Record<string, string>)[event.phase] ?? event.phase;

  return (
    <div className="flex min-w-0 gap-2">
      <time
        dateTime={event.occurred_at}
        className="shrink-0 tabular-nums text-muted-foreground"
      >
        {timeLabel}
      </time>
      <span className="shrink-0 text-foreground">{label}</span>
      {event.message ? (
        <span className="min-w-0 truncate text-muted-foreground" title={event.message}>
          {event.message}
        </span>
      ) : null}
    </div>
  );
}
