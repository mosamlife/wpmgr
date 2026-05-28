import { useState } from "react";
import { createFileRoute, Link } from "@tanstack/react-router";

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
import { useBackup, NotFoundError } from "@/features/backups/use-backups";
import { StatusBadge, KindBadge } from "@/features/backups/backup-badges";
import { RestoreDialog } from "@/features/backups/restore-dialog";
import { useMe, canOperate } from "@/features/auth/use-auth";
import { formatBytes, relativeTime } from "@/lib/utils";
import type { BackupSnapshotDetail } from "@wpmgr/api";
import type { ReactNode } from "react";

export const Route = createFileRoute("/_authed/backups/$snapshotId")({
  component: SnapshotDetailPage,
});

function SnapshotDetailPage() {
  const { snapshotId } = Route.useParams();
  const { data, isPending, isError, error, refetch } = useBackup(snapshotId);
  const { data: me } = useMe();
  const operate = canOperate(me);

  return (
    <section aria-labelledby="snapshot-heading" className="space-y-6">
      <div className="flex items-center gap-3">
        <Button asChild variant="outline" size="sm">
          <Link to="/sites">Back to sites</Link>
        </Button>
      </div>

      {isPending ? (
        <p role="status" className="text-[var(--color-muted-foreground)]">
          Loading snapshot…
        </p>
      ) : isError ? (
        error instanceof NotFoundError ? (
          <div role="alert" className="space-y-2">
            <h1 id="snapshot-heading" className="text-2xl font-semibold">
              Snapshot not found
            </h1>
            <p className="text-[var(--color-muted-foreground)]">
              No backup snapshot exists with id <code>{snapshotId}</code>.
            </p>
          </div>
        ) : (
          <div role="alert" className="space-y-3">
            <h1 id="snapshot-heading" className="text-2xl font-semibold">
              Could not load snapshot
            </h1>
            <p className="text-[var(--color-destructive)]">{error.message}</p>
            <Button variant="outline" size="sm" onClick={() => void refetch()}>
              Retry
            </Button>
          </div>
        )
      ) : (
        <SnapshotDetailView detail={data} canRestore={operate} />
      )}
    </section>
  );
}

function SnapshotDetailView({
  detail,
  canRestore,
}: {
  detail: BackupSnapshotDetail;
  canRestore: boolean;
}) {
  const { snapshot, entries } = detail;
  const [restoreOpen, setRestoreOpen] = useState(false);
  const inFlight =
    snapshot.status === "pending" || snapshot.status === "running";

  return (
    <div className="space-y-6">
      <div className="space-y-2">
        <div className="flex flex-wrap items-center gap-3">
          <h1 id="snapshot-heading" className="font-mono text-2xl font-semibold">
            Snapshot {snapshot.id.slice(0, 8)}…
          </h1>
          <KindBadge kind={snapshot.kind} />
          <StatusBadge status={snapshot.status} />
          {inFlight ? (
            <span
              role="status"
              className="inline-flex items-center gap-1 text-xs text-[var(--color-muted-foreground)]"
            >
              <span
                aria-hidden="true"
                className="size-1.5 animate-pulse rounded-full bg-amber-500"
              />
              In progress — polling
            </span>
          ) : null}
        </div>
        <p className="text-sm text-[var(--color-muted-foreground)]">
          Created {relativeTime(snapshot.created_at) ?? snapshot.created_at}
        </p>
      </div>

      {snapshot.status === "failed" && snapshot.error ? (
        <p
          role="alert"
          className="rounded-md border border-[var(--color-destructive)]/40 bg-[var(--color-destructive)]/10 p-3 text-sm text-[var(--color-destructive)]"
        >
          {snapshot.error}
        </p>
      ) : null}

      <Card>
        <CardHeader className="flex flex-row items-start justify-between gap-3">
          <div>
            <CardTitle>Manifest summary</CardTitle>
            <CardDescription>
              What this snapshot captured and how it is stored.
            </CardDescription>
          </div>
          {canRestore ? (
            <Button
              variant="destructive"
              onClick={() => setRestoreOpen(true)}
              disabled={snapshot.status !== "completed"}
            >
              Restore…
            </Button>
          ) : null}
        </CardHeader>
        <CardContent>
          <dl className="grid grid-cols-1 gap-x-6 gap-y-3 text-sm sm:grid-cols-2">
            <Detail label="Kind" value={snapshot.kind} />
            <Detail label="Status" value={snapshot.status} />
            <Detail label="Total size" value={formatBytes(snapshot.total_size)} />
            <Detail label="Chunks" value={snapshot.chunk_count ?? "—"} />
            <Detail label="Entries" value={entries.length} />
            <Detail
              label="Archived"
              value={snapshot.archived ? "Yes" : "No"}
            />
            <Detail
              label="Started"
              value={relativeTime(snapshot.started_at) ?? "—"}
            />
            <Detail
              label="Finished"
              value={relativeTime(snapshot.finished_at) ?? "—"}
            />
            <Detail
              label="age recipient"
              value={snapshot.age_recipient ?? "—"}
            />
          </dl>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>Entries ({entries.length})</CardTitle>
          <CardDescription>
            Files and database tables in this snapshot.
          </CardDescription>
        </CardHeader>
        <CardContent>
          {entries.length === 0 ? (
            <p className="text-sm text-[var(--color-muted-foreground)]">
              No entry details available.
            </p>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Path</TableHead>
                  <TableHead>Type</TableHead>
                  <TableHead>Table</TableHead>
                  <TableHead>Size</TableHead>
                  <TableHead>Chunks</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {entries.map((entry) => (
                  <TableRow
                    key={`${entry.entry_kind}:${entry.path}`}
                    data-testid="manifest-entry-row"
                  >
                    <TableCell className="font-mono text-xs break-all">
                      {entry.path}
                    </TableCell>
                    <TableCell className="capitalize">
                      {entry.entry_kind}
                    </TableCell>
                    <TableCell className="font-mono text-xs">
                      {entry.table_name ?? "—"}
                    </TableCell>
                    <TableCell>{formatBytes(entry.size)}</TableCell>
                    <TableCell>{entry.chunk_count}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>

      {canRestore ? (
        <RestoreDialog
          open={restoreOpen}
          onClose={() => setRestoreOpen(false)}
          snapshotId={snapshot.id}
          entries={entries}
        />
      ) : null}
    </div>
  );
}

function Detail({ label, value }: { label: string; value: ReactNode }) {
  return (
    <div>
      <dt className="text-[var(--color-muted-foreground)] capitalize">
        {label}
      </dt>
      <dd className="font-medium break-all">{value}</dd>
    </div>
  );
}
