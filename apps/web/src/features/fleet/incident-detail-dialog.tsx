import { useId, useMemo } from "react";
import { Link } from "@tanstack/react-router";
import { useQueryClient } from "@tanstack/react-query";
import {
  X,
  RefreshCw,
  DatabaseBackup,
  Bug,
  Activity as ActivityIcon,
  Stethoscope,
} from "lucide-react";

import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogBody,
} from "@/components/ui/dialog";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { PageError } from "@/components/feedback";
import { TooltipProvider, Tooltip } from "@/components/ui/tooltip";
import { DefinitionList } from "@/components/shared/definition-list";
import { toast } from "@/components/toast";
import { cn, relativeTime } from "@/lib/utils";

import { AutoLoginButton } from "@/features/sites/auto-login-button";
import {
  useRecheckConnection,
  AgentUnreachableError,
} from "@/features/sites/use-site-connection";
import { useRefreshDiagnostics } from "@/features/health/use-diagnostics";
import { useSiteUptime, monitoringKeys } from "@/features/monitoring/use-uptime";
import { useUpdateRuns } from "@/features/updates/use-updates";
import { useBackups } from "@/features/backups/use-backups";
import { useActivity } from "@/features/activity/use-activity";
import { usePHPErrors } from "@/features/errors/use-errors";

import { useIncidentDetail } from "./use-fleet-uptime";
import type { FleetIncident, IncidentDetail, UptimeStatusKind } from "./fleet-types";
import { STATUS_ICON, STATUS_LABEL, STATUS_COLOR_CLASS } from "./uptime-status";
import {
  isIncidentOngoing,
  formatIncidentDuration,
  humanizeIncidentReason,
} from "./incident-format";
import {
  updateTasksToTimeline,
  backupsToTimeline,
  activityToTimeline,
  phpErrorsToTimeline,
  mergeIncidentTimeline,
  type TimelineItem,
} from "./incident-timeline";

// Incident detail dialog (GH #148) — click-a-row drill-in on the fleet
// Uptime page's incidents panel. Mirrors the EmailLogDetailDialog structure
// (Dialog + DialogHeader/Title/Body, capped max-w, close X). The dialog only
// mounts its data hooks (uptime windows, update/backup/activity/error
// timelines, and the two action mutations) while an incident is selected —
// `IncidentDetailBody` is only rendered when `incident !== null`, so closing
// the dialog tears every one of those queries back down.

export interface IncidentDetailDialogProps {
  incident: FleetIncident | null;
  onClose: () => void;
}

export function IncidentDetailDialog({
  incident,
  onClose,
}: IncidentDetailDialogProps) {
  const titleId = useId();

  return (
    <TooltipProvider>
      <Dialog open={incident !== null} onClose={onClose}>
        <DialogContent
          ariaLabelledBy={titleId}
          className="max-w-[min(720px,calc(100vw-2rem))]"
        >
          <DialogHeader>
            <div className="flex items-center justify-between gap-2">
              <DialogTitle id={titleId}>Incident detail</DialogTitle>
              <button
                type="button"
                aria-label="Close detail"
                onClick={onClose}
                className="rounded text-[var(--color-muted-foreground)] hover:text-[var(--color-foreground)] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)]"
              >
                <X aria-hidden="true" className="size-4" />
              </button>
            </div>
          </DialogHeader>

          <DialogBody>
            {/* Keyed by incident.id so all internal query/mutation state
                resets automatically when the operator switches incidents
                without closing the dialog. */}
            {incident ? (
              <IncidentDetailBody key={incident.id} incident={incident} />
            ) : null}
          </DialogBody>
        </DialogContent>
      </Dialog>
    </TooltipProvider>
  );
}

// ---------------------------------------------------------------------------
// Body — all data hooks live here so they only run while the dialog is open
// ---------------------------------------------------------------------------

const TIMELINE_ICON: Record<TimelineItem["source"], typeof RefreshCw> = {
  update: RefreshCw,
  backup: DatabaseBackup,
  activity: ActivityIcon,
  php_error: Bug,
};

function IncidentDetailBody({ incident }: { incident: FleetIncident }) {
  const siteId = incident.site_id;
  const queryClient = useQueryClient();

  const detail = useIncidentDetail(incident.id);

  // Context: 7d + 30d uptime windows are distinct cached query keys.
  const uptime7d = useSiteUptime(siteId, "7d");
  const uptime30d = useSiteUptime(siteId, "30d");

  // Events timeline sources — existing per-site hooks, reused as-is.
  const updateRuns = useUpdateRuns();
  const backups = useBackups(siteId);
  const activity = useActivity(siteId, {
    severity: "all",
    objectType: "",
    actorLogin: "",
  });
  const phpErrors = usePHPErrors(siteId, { showSilenced: false, limit: 50 });

  // Footer actions.
  const recheck = useRecheckConnection();
  const refreshDiagnostics = useRefreshDiagnostics(siteId);

  const updateItems = useMemo(
    () =>
      updateTasksToTimeline(
        (updateRuns.data ?? []).flatMap((run) =>
          (run.tasks ?? []).filter((task) => task.site_id === siteId),
        ),
      ),
    [updateRuns.data, siteId],
  );
  const backupItems = useMemo(
    () => backupsToTimeline(backups.data ?? []),
    [backups.data],
  );
  const activityItems = useMemo(
    () => activityToTimeline(activity.items),
    [activity.items],
  );
  const phpErrorItems = useMemo(
    () => phpErrorsToTimeline(phpErrors.items),
    [phpErrors.items],
  );

  const timeline = useMemo(() => {
    if (!detail.data) return [];
    return mergeIncidentTimeline(
      [updateItems, backupItems, activityItems, phpErrorItems],
      { start: detail.data.started_at, end: detail.data.ended_at },
    );
  }, [updateItems, backupItems, activityItems, phpErrorItems, detail.data]);

  if (detail.isPending) {
    return (
      <div className="space-y-3">
        <Skeleton className="h-5 w-2/3" />
        <Skeleton className="h-4 w-1/2" />
        <Skeleton className="h-24 w-full" />
        <Skeleton className="h-16 w-full" />
      </div>
    );
  }

  if (detail.isError) {
    return (
      <PageError
        what="Could not load incident detail."
        why={detail.error?.message}
        onRetry={() => void detail.refetch()}
      />
    );
  }

  const d: IncidentDetail | undefined = detail.data;
  if (!d) return null;

  return (
    <IncidentDetailContent
      incident={incident}
      detail={d}
      timeline={timeline}
      uptime7dPct={uptime7d.data?.uptime_pct ?? null}
      uptime30dPct={uptime30d.data?.uptime_pct ?? null}
      lastCheck={uptime7d.data?.last_check ?? uptime30d.data?.last_check ?? null}
      onRecheck={() => {
        recheck.mutate(
          { siteId },
          {
            onSuccess: () => {
              toast.success("Connection refreshed");
              void queryClient.invalidateQueries({
                queryKey: monitoringKeys.uptime(siteId, "7d"),
              });
              void queryClient.invalidateQueries({
                queryKey: monitoringKeys.uptime(siteId, "30d"),
              });
            },
            onError: (err) => {
              if (err instanceof AgentUnreachableError) {
                toast.info(err.message);
              } else {
                toast.error("Re-check failed", { description: err.message });
              }
            },
          },
        );
      }}
      recheckPending={recheck.isPending}
      onRunDiagnostics={() => {
        refreshDiagnostics.mutate(undefined, {
          onSuccess: () => toast.success("Diagnostics refresh requested"),
          onError: (err) => toast.info(err.message),
        });
      }}
      diagnosticsPending={refreshDiagnostics.isPending}
    />
  );
}

// ---------------------------------------------------------------------------
// Content — pure presentational split so the loading/error gates above stay
// readable
// ---------------------------------------------------------------------------

interface IncidentDetailContentProps {
  incident: FleetIncident;
  detail: IncidentDetail;
  timeline: TimelineItem[];
  uptime7dPct: number | null;
  uptime30dPct: number | null;
  lastCheck: string | null;
  onRecheck: () => void;
  recheckPending: boolean;
  onRunDiagnostics: () => void;
  diagnosticsPending: boolean;
}

function IncidentDetailContent({
  incident,
  detail: d,
  timeline,
  uptime7dPct,
  uptime30dPct,
  lastCheck,
  onRecheck,
  recheckPending,
  onRunDiagnostics,
  diagnosticsPending,
}: IncidentDetailContentProps) {
  const siteId = d.site_id;
  const ongoing = isIncidentOngoing(d);
  const durationLabel = formatIncidentDuration(d.duration_seconds, ongoing);

  // Prefer the freshest signal (peak_status from the detail DTO) when it's
  // one of the two incident kinds; fall back to the row's kind (the detail
  // contract has no `kind` field of its own).
  const kind: "down" | "degraded" =
    d.peak_status === "down" || d.peak_status === "degraded"
      ? d.peak_status
      : incident.kind;
  const statusKind: UptimeStatusKind = ongoing ? kind : "up";
  const StatusIcon = STATUS_ICON[statusKind];

  return (
    <div className="space-y-5">
      {/* Header: identity + status */}
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0">
          <Link
            to="/sites/$siteId"
            params={{ siteId }}
            className="font-medium text-[var(--color-foreground)] hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)]"
          >
            {d.name || d.url}
          </Link>
          <p className="truncate text-xs text-[var(--color-muted-foreground)]">
            {d.url}
          </p>
        </div>
        <Badge
          variant="outline"
          className={cn("shrink-0 gap-1", STATUS_COLOR_CLASS[statusKind])}
        >
          <StatusIcon aria-hidden="true" className="size-3.5" />
          {ongoing ? STATUS_LABEL[statusKind] : "Recovered"}
        </Badge>
      </div>

      {/* Incident timeline */}
      <section aria-labelledby="incident-timeline-heading" className="space-y-2">
        <h3
          id="incident-timeline-heading"
          className="text-xs font-medium uppercase tracking-wide text-[var(--color-muted-foreground)]"
        >
          Timeline
        </h3>
        <DefinitionList
          rows={[
            { label: "Started", value: relativeTime(d.started_at) ?? d.started_at },
            {
              label: "Ended",
              value: ongoing
                ? "Ongoing"
                : d.ended_at
                  ? (relativeTime(d.ended_at) ?? d.ended_at)
                  : undefined,
            },
            {
              label: "Duration",
              value: ongoing ? "Ongoing" : (durationLabel ?? undefined),
            },
            { label: "Peak status", value: capitalize(d.peak_status) },
            {
              label: "Last HTTP code",
              value:
                d.last_http_status > 0 ? String(d.last_http_status) : "No response",
              tabular: true,
            },
            { label: "Cause", value: humanizeIncidentReason(d.reason) },
          ]}
        />

        {/* Probe window — newest-first compact chips. */}
        <div className="space-y-1.5">
          <p className="text-xs font-medium text-[var(--color-muted-foreground)]">
            Probe window
          </p>
          {d.probes.length === 0 ? (
            <p className="text-sm text-[var(--color-muted-foreground)]">
              Probe detail unavailable (retention).
            </p>
          ) : (
            <div className="flex gap-1.5 overflow-x-auto pb-1" role="list" aria-label="Probe results, newest first">
              {d.probes.map((probe, idx) => (
                <Tooltip
                  key={`${probe.probed_at}-${idx}`}
                  content={
                    <div className="space-y-0.5">
                      <p>{relativeTime(probe.probed_at) ?? probe.probed_at}</p>
                      <p>{probe.total_ms} ms</p>
                      {probe.error ? <p>{probe.error}</p> : null}
                    </div>
                  }
                >
                  <span
                    role="listitem"
                    className={cn(
                      "inline-flex shrink-0 items-center rounded px-1.5 py-0.5 text-[10px] font-mono tabular-nums",
                      probe.up
                        ? "bg-[var(--color-success-subtle,oklch(95%_0.05_145))] text-[var(--color-success,oklch(50%_0.15_145))]"
                        : "bg-[var(--color-destructive-subtle,oklch(95%_0.05_25))] text-[var(--color-destructive,oklch(50%_0.18_25))]",
                    )}
                  >
                    {probe.http_status > 0 ? probe.http_status : probe.up ? "OK" : "ERR"}
                  </span>
                </Tooltip>
              ))}
            </div>
          )}
          {d.probes_truncated ? (
            <p className="text-xs text-[var(--color-muted-foreground)]">
              Showing the most recent probes only.
            </p>
          ) : null}
        </div>
      </section>

      {/* Events timeline — client-composed from existing per-site hooks */}
      <section aria-labelledby="incident-events-heading" className="space-y-2">
        <h3
          id="incident-events-heading"
          className="text-xs font-medium uppercase tracking-wide text-[var(--color-muted-foreground)]"
        >
          What happened around this incident
        </h3>
        {timeline.length === 0 ? (
          <p className="text-sm text-[var(--color-muted-foreground)]">
            No update, backup, or error activity recorded near this window.
          </p>
        ) : (
          <div
            role="list"
            aria-label="Events around this incident"
            className="divide-y divide-[var(--color-border)] rounded-lg border border-[var(--color-border)]"
          >
            {timeline.map((item) => (
              <TimelineRow key={`${item.source}-${item.id}`} item={item} />
            ))}
          </div>
        )}
      </section>

      {/* Context */}
      <section aria-labelledby="incident-context-heading" className="space-y-2">
        <h3
          id="incident-context-heading"
          className="text-xs font-medium uppercase tracking-wide text-[var(--color-muted-foreground)]"
        >
          Context
        </h3>
        <DefinitionList
          rows={[
            {
              label: "Uptime (7d)",
              value: uptime7dPct !== null ? `${uptime7dPct.toFixed(2)}%` : undefined,
              tabular: true,
            },
            {
              label: "Uptime (30d)",
              value: uptime30dPct !== null ? `${uptime30dPct.toFixed(2)}%` : undefined,
              tabular: true,
            },
            {
              label: "Last check",
              value: lastCheck ? relativeTime(lastCheck) : undefined,
            },
            {
              label: "Flapping (30d)",
              value: `${d.incident_count_30d} incident${d.incident_count_30d === 1 ? "" : "s"}`,
            },
          ]}
        />
      </section>

      {/* Footer actions */}
      <div className="flex flex-wrap items-center gap-2 border-t border-[var(--color-border)] pt-3">
        <AutoLoginButton siteId={siteId} siteName={d.name || d.url} size="sm" />

        <Button
          type="button"
          variant="outline"
          size="sm"
          disabled={recheckPending}
          onClick={onRecheck}
          className="gap-1.5"
        >
          <RefreshCw
            aria-hidden="true"
            className={cn("size-3.5", recheckPending && "animate-spin")}
          />
          Re-check connection
        </Button>

        <Button
          type="button"
          variant="outline"
          size="sm"
          disabled={diagnosticsPending}
          onClick={onRunDiagnostics}
          className="gap-1.5"
        >
          <Stethoscope aria-hidden="true" className="size-3.5" />
          Run diagnostics
        </Button>

        <div className="ml-auto flex flex-wrap items-center gap-1">
          <Button asChild variant="ghost" size="sm">
            <Link to="/sites/$siteId/health" params={{ siteId }}>
              Health
            </Link>
          </Button>
          <Button asChild variant="ghost" size="sm">
            <Link
              to="/sites/$siteId/backups"
              params={{ siteId }}
              aria-label="Open backups — restore from a snapshot here"
            >
              Backups
            </Link>
          </Button>
          <Button asChild variant="ghost" size="sm">
            <Link to="/sites/$siteId/updates" params={{ siteId }}>
              Updates
            </Link>
          </Button>
          <Button asChild variant="ghost" size="sm">
            <Link to="/sites/$siteId/activity" params={{ siteId }}>
              Activity
            </Link>
          </Button>
          <Button asChild variant="ghost" size="sm">
            <Link to="/sites/$siteId/errors" params={{ siteId }}>
              Errors
            </Link>
          </Button>
        </div>
      </div>
    </div>
  );
}

function TimelineRow({ item }: { item: TimelineItem }) {
  const Icon = TIMELINE_ICON[item.source];
  return (
    <div role="listitem" className="flex items-start gap-2.5 px-3 py-2 text-xs">
      <Icon
        aria-hidden="true"
        className="mt-0.5 size-3.5 shrink-0 text-[var(--color-muted-foreground)]"
      />
      <div className="min-w-0 flex-1">
        <p className="truncate text-[var(--color-foreground)]">{item.label}</p>
        {item.detail ? (
          <p className="truncate text-[var(--color-muted-foreground)]">
            {item.detail}
          </p>
        ) : null}
      </div>
      <span className="shrink-0 text-[var(--color-muted-foreground)]">
        {relativeTime(item.timestamp) ?? ""}
      </span>
    </div>
  );
}

function capitalize(value: string): string {
  if (!value) return value;
  return value.charAt(0).toUpperCase() + value.slice(1);
}
