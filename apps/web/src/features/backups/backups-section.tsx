import { useState, type ReactNode } from "react";
import { Link } from "@tanstack/react-router";

import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Select } from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import { Label } from "@/components/ui/label";
import { PageError } from "@/components/feedback";
import { StatusChip } from "@/components/status/status-chip";
import type { StatusTone } from "@/components/status/status-dot";
import { useBackups, useCreateBackup } from "@/features/backups/use-backups";
import { StatusBadge, KindBadge } from "@/features/backups/backup-badges";
import { InlineSnapshotProgress } from "@/features/backups/inline-snapshot-progress";
import { isRestoreActive, PHASE_LABEL } from "@/features/backups/format-progress";
import {
  useRestoreRuns,
  type RestoreRun,
  type RestoreStatus,
} from "@/features/backups/use-restores";
import {
  useScheduleRuns,
  type ScheduleRun,
  type ScheduleRunStatus,
} from "@/features/backups/use-schedule-runs";
import { BackupScheduleEditor } from "@/features/backups/backup-schedule-editor";
import { formatBytes, relativeTime } from "@/lib/utils";
import type { BackupCreate } from "@wpmgr/api";

// The "Backups" section rendered on the site detail page. One card holds the
// snapshot list; "Back up now" lives as a header control (not an inset
// bordered box) so the surface is flat (ADR-037 Batch 2 — never card-in-card).
// Viewers see the list only; the schedule editor (operator+) is its own card.

const KINDS: { value: NonNullable<BackupCreate["kind"]>; label: string }[] = [
  { value: "full", label: "Full (files + database)" },
  { value: "files", label: "Files only" },
  { value: "db", label: "Database only" },
];

export function BackupsSection({
  siteId,
  canOperate,
}: {
  siteId: string;
  canOperate: boolean;
}) {
  return (
    <div className="space-y-6">
      <Card>
        <CardHeader className="flex flex-row items-start justify-between gap-4">
          <div className="space-y-1.5">
            <CardTitle>Backups</CardTitle>
            <CardDescription>
              Encrypted snapshots of this site. Chunks are encrypted on the
              agent; the control plane cannot read your data.
            </CardDescription>
          </div>
          {canOperate ? <BackupNowControl siteId={siteId} /> : null}
        </CardHeader>
        <CardContent>
          <SnapshotList siteId={siteId} />
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>Restore history</CardTitle>
          <CardDescription>
            Restores initiated from any snapshot of this site, newest first.
          </CardDescription>
        </CardHeader>
        <CardContent>
          <RestoreHistory siteId={siteId} />
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>Backup schedule runs</CardTitle>
          <CardDescription>
            Upcoming scheduled backups and past run history for this site.
          </CardDescription>
        </CardHeader>
        <CardContent>
          <ScheduleRunsSection siteId={siteId} />
        </CardContent>
      </Card>

      {canOperate ? <BackupScheduleEditor siteId={siteId} /> : null}
    </div>
  );
}

function BackupNowControl({ siteId }: { siteId: string }) {
  const [kind, setKind] = useState<NonNullable<BackupCreate["kind"]>>("full");
  const create = useCreateBackup(siteId);

  function onBackup() {
    create.mutate({ kind }, { onError: () => {} });
  }

  return (
    <div className="flex shrink-0 flex-col items-end gap-1.5">
      <div className="flex items-end gap-2">
        <div className="space-y-1">
          <Label htmlFor="backup-kind" className="sr-only">
            What to back up
          </Label>
          <Select
            id="backup-kind"
            value={kind}
            onChange={(e) =>
              setKind(e.target.value as NonNullable<BackupCreate["kind"]>)
            }
            className="h-8 px-2 text-xs"
          >
            {KINDS.map((k) => (
              <option key={k.value} value={k.value}>
                {k.label}
              </option>
            ))}
          </Select>
        </div>
        <Button size="sm" onClick={onBackup} disabled={create.isPending}>
          {create.isPending ? "Starting…" : "Run backup"}
        </Button>
      </div>
      {create.isError ? (
        <p role="alert" className="text-xs text-destructive-subtle-fg">
          {create.error.message}
        </p>
      ) : null}
      {create.isSuccess ? (
        <p role="status" className="text-xs text-muted-foreground">
          Backup started. It appears below as it progresses.
        </p>
      ) : null}
    </div>
  );
}

function SnapshotList({ siteId }: { siteId: string }) {
  const { data, isPending, isError, error, refetch } = useBackups(siteId);

  if (isPending) {
    return (
      <p role="status" className="text-sm text-muted-foreground">
        Loading backups…
      </p>
    );
  }

  if (isError) {
    return (
      <PageError
        what="Could not load backups."
        why={error.message}
        onRetry={() => void refetch()}
      />
    );
  }

  if (data.length === 0) {
    return (
      <p className="text-sm text-muted-foreground">
        No backups yet. Run one to capture this site.
      </p>
    );
  }

  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead>Kind</TableHead>
          <TableHead>Status</TableHead>
          <TableHead>Size</TableHead>
          <TableHead>Chunks</TableHead>
          <TableHead>Created</TableHead>
          <TableHead>Finished</TableHead>
          <TableHead>
            <span className="sr-only">Actions</span>
          </TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {data.map((snap) => (
          <TableRow key={snap.id} data-testid="backup-row">
            <TableCell>
              <KindBadge kind={snap.kind} />
            </TableCell>
            <TableCell>
              <div className="flex flex-col gap-1">
                <StatusBadge status={snap.status} />
                {snap.status === "running" ||
                snap.status === "pending" ||
                isRestoreActive(snap) ? (
                  <InlineSnapshotProgress snapshot={snap} />
                ) : null}
                {snap.status === "failed" && snap.error ? (
                  <span
                    role="alert"
                    className="text-xs text-destructive-subtle-fg"
                  >
                    {snap.error}
                  </span>
                ) : null}
              </div>
            </TableCell>
            <TableCell className="tabular-nums">
              {formatBytes(snap.total_size)}
            </TableCell>
            <TableCell className="tabular-nums">
              {snap.chunk_count ?? "–"}
            </TableCell>
            <TableCell className="tabular-nums" title={snap.created_at}>
              {relativeTime(snap.created_at) ?? "–"}
            </TableCell>
            <TableCell
              className="tabular-nums"
              title={snap.finished_at ?? undefined}
            >
              {relativeTime(snap.finished_at) ?? "–"}
            </TableCell>
            <TableCell className="text-right">
              <Button asChild variant="outline" size="sm">
                <Link to="/backups/$snapshotId" params={{ snapshotId: snap.id }}>
                  View
                </Link>
              </Button>
            </TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
}

// ---------------------------------------------------------------------------
// Restore history
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

function phaseLabel(phase: string | null): string {
  if (!phase) return "–";
  return (PHASE_LABEL as Record<string, string>)[phase] ?? phase;
}

function RestoreHistory({ siteId }: { siteId: string }) {
  const { data, isPending, isError, error, refetch } = useRestoreRuns(siteId);

  if (isPending) {
    return (
      <div
        role="status"
        aria-label="Loading restore history"
        className="space-y-2"
      >
        {Array.from({ length: 3 }, (_, i) => (
          <Skeleton key={i} className="h-9 w-full" />
        ))}
      </div>
    );
  }

  if (isError) {
    return (
      <PageError
        what="Could not load restore history."
        why={error.message}
        onRetry={() => void refetch()}
      />
    );
  }

  if (data.length === 0) {
    return (
      <p className="text-sm text-muted-foreground">
        No restores yet. Restores initiated from a snapshot will appear here.
      </p>
    );
  }

  return (
    <div className="overflow-x-auto">
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Status</TableHead>
            <TableHead>Phase</TableHead>
            <TableHead>Snapshot</TableHead>
            <TableHead>Started</TableHead>
            <TableHead>Triggered by</TableHead>
            <TableHead>
              <span className="sr-only">Actions</span>
            </TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {data.map((run) => (
            <RestoreRow key={run.id} run={run} />
          ))}
        </TableBody>
      </Table>
    </div>
  );
}

/**
 * Resolve a human label for who triggered a restore.
 * Prefers name, falls back to email, then falls back to the first 8 chars of
 * the UUID (monospaced) so we never surface a raw UUID as readable text.
 */
function triggeredByLabel(run: RestoreRun): ReactNode {
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

function RestoreRow({ run }: { run: RestoreRun }) {
  const isRunning = run.status === "running";
  const timeLabel =
    relativeTime(run.started_at ?? run.created_at) ?? "–";

  return (
    <TableRow>
      <TableCell>
        <StatusChip
          tone={RESTORE_STATUS_TONE[run.status]}
          label={RESTORE_STATUS_LABEL[run.status]}
          pulse={isRunning}
        />
      </TableCell>
      <TableCell className="text-sm">
        {phaseLabel(run.current_phase)}
      </TableCell>
      <TableCell>
        <code className="font-mono text-xs text-muted-foreground">
          {run.snapshot_id.slice(0, 8)}
        </code>
      </TableCell>
      <TableCell className="tabular-nums text-sm" title={run.started_at ?? run.created_at}>
        <time dateTime={run.started_at ?? run.created_at}>{timeLabel}</time>
      </TableCell>
      <TableCell className="text-sm text-muted-foreground">
        {triggeredByLabel(run)}
      </TableCell>
      <TableCell className="text-right">
        <Button asChild variant="outline" size="sm">
          <Link to="/restores/$restoreId" params={{ restoreId: run.id }}>
            View
          </Link>
        </Button>
      </TableCell>
    </TableRow>
  );
}

// ---------------------------------------------------------------------------
// Backup schedule runs section
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

function ScheduleRunsSection({ siteId }: { siteId: string }) {
  const { data, isPending, isError, error, refetch } = useScheduleRuns(siteId);

  if (isPending) {
    return (
      <div role="status" aria-label="Loading schedule runs" className="space-y-2">
        {Array.from({ length: 3 }, (_, i) => (
          <Skeleton key={i} className="h-9 w-full" />
        ))}
      </div>
    );
  }

  if (isError) {
    return (
      <PageError
        what="Could not load schedule run history."
        why={error.message}
        onRetry={() => void refetch()}
      />
    );
  }

  const { upcoming, past } = data;

  return (
    <div className="space-y-6">
      {/* Upcoming */}
      <div>
        <h3 className="mb-2 text-sm font-semibold text-foreground">Upcoming</h3>
        {upcoming.length === 0 ? (
          <p className="text-sm text-muted-foreground">
            No upcoming runs. Enable the backup schedule to queue runs.
          </p>
        ) : (
          <div className="overflow-x-auto">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Status</TableHead>
                  <TableHead>Scheduled for</TableHead>
                  <TableHead>Kind</TableHead>
                  <TableHead>
                    <span className="sr-only">Actions</span>
                  </TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {upcoming.map((run) => (
                  <ScheduleRunRow key={run.id} run={run} />
                ))}
              </TableBody>
            </Table>
          </div>
        )}
      </div>

      {/* Past */}
      <div>
        <h3 className="mb-2 text-sm font-semibold text-foreground">Past</h3>
        {past.length === 0 ? (
          <p className="text-sm text-muted-foreground">
            No past runs yet.
          </p>
        ) : (
          <div className="overflow-x-auto">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Status</TableHead>
                  <TableHead>Scheduled for</TableHead>
                  <TableHead>Kind</TableHead>
                  <TableHead>Snapshot</TableHead>
                  <TableHead>
                    <span className="sr-only">Actions</span>
                  </TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {past.map((run) => (
                  <ScheduleRunRow key={run.id} run={run} showSnapshot />
                ))}
              </TableBody>
            </Table>
          </div>
        )}
      </div>
    </div>
  );
}

function ScheduleRunRow({
  run,
  showSnapshot = false,
}: {
  run: ScheduleRun;
  showSnapshot?: boolean;
}) {
  const isRunning = run.status === "running";
  const scheduledLabel = relativeTime(run.scheduled_for) ?? "–";

  return (
    <TableRow>
      <TableCell>
        <StatusChip
          tone={SCHEDULE_STATUS_TONE[run.status]}
          label={SCHEDULE_STATUS_LABEL[run.status]}
          pulse={isRunning}
        />
        {run.status === "failed" && run.error ? (
          <span
            role="alert"
            className="mt-1 block text-xs text-destructive-subtle-fg"
          >
            {run.error}
          </span>
        ) : null}
      </TableCell>
      <TableCell className="tabular-nums text-sm" title={run.scheduled_for}>
        <time dateTime={run.scheduled_for}>{scheduledLabel}</time>
      </TableCell>
      <TableCell className="text-sm">{run.kind}</TableCell>
      {showSnapshot ? (
        <TableCell>
          {run.snapshot_id ? (
            <Button asChild variant="link" size="sm" className="h-auto p-0">
              <Link
                to="/backups/$snapshotId"
                params={{ snapshotId: run.snapshot_id }}
              >
                <code className="font-mono text-xs">
                  {run.snapshot_id.slice(0, 8)}
                </code>
              </Link>
            </Button>
          ) : (
            <span className="text-xs text-muted-foreground">–</span>
          )}
        </TableCell>
      ) : null}
      <TableCell className="text-right">
        <Button asChild variant="outline" size="sm">
          <Link
            to="/schedule-runs/$runId"
            params={{ runId: run.id }}
          >
            View
          </Link>
        </Button>
      </TableCell>
    </TableRow>
  );
}
