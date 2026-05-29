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
import { useBackups, useCreateBackup } from "@/features/backups/use-backups";
import { StatusBadge, KindBadge } from "@/features/backups/backup-badges";
import { InlineSnapshotProgress } from "@/features/backups/inline-snapshot-progress";
import { BackupScheduleEditor } from "@/features/backups/backup-schedule-editor";
import { formatBytes, relativeTime } from "@/lib/utils";
import type { BackupCreate } from "@wpmgr/api";

// The "Backups" section rendered on the site detail page. It shows the
// snapshot list, a "Back up now" control (operator+), and the schedule editor
// (operator+). Viewers see the list only.

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
        <CardHeader>
          <CardTitle>Backups</CardTitle>
          <CardDescription>
            Encrypted snapshots of this site. Chunks are encrypted on the agent;
            the control plane cannot read your data.
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-6">
          {canOperate ? <BackupNowControl siteId={siteId} /> : null}
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
    <div className="space-y-2 rounded-md border border-[var(--color-border)] p-3">
      <div className="flex flex-wrap items-end gap-3">
        <div className="space-y-1">
          <Label htmlFor="backup-kind">What to back up</Label>
          <select
            id="backup-kind"
            value={kind}
            onChange={(e) =>
              setKind(e.target.value as NonNullable<BackupCreate["kind"]>)
            }
            className="h-9 rounded-md border border-[var(--color-input)] bg-transparent px-3 text-sm focus-visible:ring-2 focus-visible:ring-[var(--color-ring)] focus-visible:outline-none"
          >
            {KINDS.map((k) => (
              <option key={k.value} value={k.value}>
                {k.label}
              </option>
            ))}
          </select>
        </div>
        <Button onClick={onBackup} disabled={create.isPending}>
          {create.isPending ? "Starting…" : "Back up now"}
        </Button>
      </div>
      {create.isError ? (
        <p role="alert" className="text-sm text-[var(--color-destructive)]">
          {create.error.message}
        </p>
      ) : null}
      {create.isSuccess ? (
        <p role="status" className="text-sm text-[var(--color-muted-foreground)]">
          Backup started — it appears below as it progresses.
        </p>
      ) : null}
    </div>
  );
}

function SnapshotList({ siteId }: { siteId: string }) {
  const { data, isPending, isError, error, refetch } = useBackups(siteId);

  if (isPending) {
    return (
      <p role="status" className="text-sm text-[var(--color-muted-foreground)]">
        Loading backups…
      </p>
    );
  }

  if (isError) {
    return (
      <div role="alert" className="space-y-2">
        <p className="text-sm text-[var(--color-destructive)]">
          {error.message}
        </p>
        <Button variant="outline" size="sm" onClick={() => void refetch()}>
          Retry
        </Button>
      </div>
    );
  }

  if (data.length === 0) {
    return (
      <p className="text-sm text-[var(--color-muted-foreground)]">
        No backups yet.
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
                {snap.status === "running" || snap.status === "pending" ? (
                  <InlineSnapshotProgress snapshot={snap} />
                ) : null}
              </div>
            </TableCell>
            <TableCell>{formatBytes(snap.total_size)}</TableCell>
            <TableCell>{snap.chunk_count ?? "—"}</TableCell>
            <TableCell title={snap.created_at}>
              {relativeTime(snap.created_at) ?? "—"}
            </TableCell>
            <TableCell title={snap.finished_at ?? undefined}>
              {relativeTime(snap.finished_at) ?? "—"}
            </TableCell>
            <TableCell className="text-right">
              <Button asChild variant="outline" size="sm">
                <Link
                  to="/backups/$snapshotId"
                  params={{ snapshotId: snap.id }}
                >
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
