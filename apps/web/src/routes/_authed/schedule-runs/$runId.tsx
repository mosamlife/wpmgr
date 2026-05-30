import { type ReactNode } from "react";
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
import { useSite } from "@/features/sites/use-sites";
import {
  useScheduleRun,
  isScheduleRunTerminal,
  type ScheduleRun,
  type ScheduleRunStatus,
} from "@/features/backups/use-schedule-runs";
import { relativeTime } from "@/lib/utils";

export const Route = createFileRoute("/_authed/schedule-runs/$runId")({
  component: ScheduleRunDetailPage,
});

// ---------------------------------------------------------------------------
// Status mappings
// ---------------------------------------------------------------------------

const SCHEDULE_STATUS_TONE: Record<ScheduleRunStatus, StatusTone> = {
  scheduled: "muted",
  queued: "muted",
  running: "info",
  completed: "success",
  failed: "destructive",
  skipped: "warning",
  canceled: "muted",
};

const SCHEDULE_STATUS_LABEL: Record<ScheduleRunStatus, string> = {
  scheduled: "Scheduled",
  queued: "Queued",
  running: "Running",
  completed: "Completed",
  failed: "Failed",
  skipped: "Skipped",
  canceled: "Canceled",
};

// ---------------------------------------------------------------------------
// Page
// ---------------------------------------------------------------------------

function ScheduleRunDetailPage() {
  const { runId } = Route.useParams();
  const {
    data: run,
    isPending,
    isError,
    error,
    refetch,
  } = useScheduleRun(runId);

  if (isPending) {
    return <ScheduleRunDetailSkeleton />;
  }

  if (isError) {
    return (
      <section className="space-y-4">
        <PageHeader
          title={`Schedule run ${runId.slice(0, 8)}`}
          mono
          backTo={{ to: "/sites", label: "Sites" }}
        />
        <PageError
          what="Could not load schedule run."
          why={error.message}
          onRetry={() => void refetch()}
          retryLabel="Reload run"
        />
      </section>
    );
  }

  return <ScheduleRunDetailView run={run} />;
}

function ScheduleRunDetailSkeleton() {
  return (
    <div className="space-y-6" role="status" aria-label="Loading schedule run">
      <div className="space-y-2">
        <Skeleton className="h-3.5 w-32" />
        <Skeleton className="h-6 w-64" />
        <Skeleton className="h-4 w-96" />
      </div>
      <Skeleton className="h-48 w-full rounded-xl" />
      <Skeleton className="h-32 w-full rounded-xl" />
    </div>
  );
}

/** Resolve name > email > short mono id for the triggered_by field. */
function resolveTriggeredBy(run: ScheduleRun): ReactNode {
  if (run.triggered_by_name) return run.triggered_by_name;
  if (run.triggered_by_email) return run.triggered_by_email;
  if (run.triggered_by) {
    return (
      <code className="font-mono text-xs text-muted-foreground">
        {run.triggered_by.slice(0, 8)}
      </code>
    );
  }
  return "schedule";
}

function ScheduleRunDetailView({ run }: { run: ScheduleRun }) {
  const terminal = isScheduleRunTerminal(run.status);

  // Resolve the originating site for the back-link.
  const { data: site } = useSite(run.site_id);

  const subline = [
    `Kind: ${run.kind}`,
    relativeTime(run.started_at ?? run.scheduled_for)
      ? `Scheduled ${relativeTime(run.scheduled_for)}`
      : null,
  ]
    .filter(Boolean)
    .join(" · ");

  return (
    <div className="space-y-6">
      <PageHeader
        title={`Schedule run ${run.id.slice(0, 8)}`}
        mono
        copyable={run.id}
        badges={
          <StatusChip
            tone={SCHEDULE_STATUS_TONE[run.status]}
            label={SCHEDULE_STATUS_LABEL[run.status]}
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

      {/* Outcome banner for terminal failure runs */}
      {terminal && run.status !== "completed" && run.status !== "skipped" ? (
        <div
          role="alert"
          className="flex items-center gap-3 rounded-md border border-[var(--color-destructive)]/40 bg-destructive-subtle p-3"
        >
          <StatusChip
            tone={SCHEDULE_STATUS_TONE[run.status]}
            label={SCHEDULE_STATUS_LABEL[run.status]}
          />
          {run.error ? (
            <span className="text-sm text-destructive-subtle-fg">
              {run.error}
            </span>
          ) : null}
        </div>
      ) : null}

      {run.status === "skipped" ? (
        <div
          role="status"
          className="flex items-center gap-3 rounded-md border border-border bg-muted/30 p-3"
        >
          <StatusChip tone="muted" label="Skipped" />
          {run.error ? (
            <span className="text-sm text-muted-foreground">{run.error}</span>
          ) : (
            <span className="text-sm text-muted-foreground">
              This run was skipped (site may have been unreachable or
              unenrollable).
            </span>
          )}
        </div>
      ) : null}

      {terminal && run.status === "completed" ? (
        <div
          role="status"
          className="flex items-center gap-3 rounded-md border border-[var(--color-success)]/40 bg-[var(--color-success)]/10 p-3"
        >
          <StatusChip tone="success" label="Completed" />
          <span className="text-sm text-muted-foreground">
            Backup finished successfully.
          </span>
        </div>
      ) : null}

      {/* Summary */}
      <Card>
        <CardHeader>
          <CardTitle>Summary</CardTitle>
        </CardHeader>
        <CardContent>
          <DefinitionList
            rows={[
              { label: "Kind", value: run.kind },
              { label: "Status", value: SCHEDULE_STATUS_LABEL[run.status] },
              { label: "Triggered by", value: resolveTriggeredBy(run) },
              {
                label: "Scheduled for",
                value: (
                  <time dateTime={run.scheduled_for} title={run.scheduled_for}>
                    {relativeTime(run.scheduled_for) ?? run.scheduled_for}
                  </time>
                ),
              },
              {
                label: "Started",
                value: relativeTime(run.started_at) ?? "–",
              },
              {
                label: "Finished",
                value: relativeTime(run.finished_at) ?? "–",
              },
              {
                label: "Snapshot",
                value: run.snapshot_id ? (
                  <Link
                    to="/backups/$snapshotId"
                    params={{ snapshotId: run.snapshot_id }}
                    className="font-mono text-xs underline-offset-2 hover:underline"
                  >
                    {run.snapshot_id.slice(0, 8)}
                  </Link>
                ) : (
                  "–"
                ),
              },
              {
                label: "Error",
                value: run.error ?? "–",
              },
            ]}
          />
        </CardContent>
      </Card>
    </div>
  );
}
