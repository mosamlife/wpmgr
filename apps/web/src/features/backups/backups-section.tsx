import { useState } from "react";
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
import { Label } from "@/components/ui/label";
import { PageError } from "@/components/feedback";
import { useBackups, useCreateBackup } from "@/features/backups/use-backups";
import { StatusBadge, KindBadge } from "@/features/backups/backup-badges";
import { InlineSnapshotProgress } from "@/features/backups/inline-snapshot-progress";
import { isRestoreActive } from "@/features/backups/format-progress";
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
          <select
            id="backup-kind"
            value={kind}
            onChange={(e) =>
              setKind(e.target.value as NonNullable<BackupCreate["kind"]>)
            }
            className="h-8 rounded-md border border-[var(--color-input)] bg-transparent px-2 text-xs focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)]"
          >
            {KINDS.map((k) => (
              <option key={k.value} value={k.value}>
                {k.label}
              </option>
            ))}
          </select>
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
