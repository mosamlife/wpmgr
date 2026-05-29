import { useEffect, useState } from "react";
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
import { SnapshotProgressCard } from "@/features/backups/snapshot-progress-card";
import { useBackupStream } from "@/features/backups/use-backup-stream";
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
  const [restoreRequestedAt, setRestoreRequestedAt] = useState<number | null>(
    null,
  );
  const inFlight =
    snapshot.status === "pending" || snapshot.status === "running";

  // ALWAYS open the SSE stream on this page so the first restore event lands
  // and triggers the progress card to render — otherwise chicken-and-egg: the
  // card waits for phase to be a restore phase, but phase only updates when
  // SSE delivers an event, but SSE only opens when the card mounts. Returned
  // state is not used here (the card consumes it via its own hook call) — the
  // call is for the side effect of opening the EventSource.
  useBackupStream(snapshot.id);

  // Bridge the perceptual gap between "I clicked Restore" and the first SSE
  // phase event landing (preflight, typically a few seconds away while the
  // agent polls its task queue). Show a small banner for ~8s after the
  // confirmed POST, OR until a restore phase event lands and the progress
  // card renders for real — whichever comes first.
  const hasRestorePhase =
    typeof snapshot.progress === "object" &&
    snapshot.progress !== null &&
    (() => {
      const p = (snapshot.progress as { phase?: unknown }).phase;
      return typeof p === "string" && [
        "preflight", "download_artifacts", "verify_artifacts",
        "maintenance_on", "stage_files", "swap_files", "restore_db",
        "migrate_db", "swap_db", "post_hooks", "maintenance_off",
        "cleanup", "rolled_back",
      ].includes(p);
    })();
  const showRestoreBanner =
    restoreRequestedAt !== null && !hasRestorePhase;
  useEffect(() => {
    if (restoreRequestedAt === null) return;
    const id = window.setTimeout(() => setRestoreRequestedAt(null), 8000);
    return () => window.clearTimeout(id);
  }, [restoreRequestedAt]);
  // Auto-dismiss as soon as a real restore phase event lands.
  useEffect(() => {
    if (hasRestorePhase && restoreRequestedAt !== null) {
      setRestoreRequestedAt(null);
    }
  }, [hasRestorePhase, restoreRequestedAt]);

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

      {showRestoreBanner ? (
        <p
          role="status"
          aria-live="polite"
          className="flex items-center gap-2 rounded-md border border-[var(--color-border)] bg-[var(--color-accent)] p-3 text-sm"
        >
          <span
            aria-hidden="true"
            className="size-1.5 animate-pulse rounded-full bg-amber-500"
          />
          Restore requested — waiting for the agent to acknowledge…
        </p>
      ) : null}

      {/* M5.6 / ADR-033 + ADR-034: live progress.
          Show during a backup (snapshot.status === "running") OR during a
          restore overlay (snapshot is completed but progress.phase is a
          restore phase — the restore runner emits events on the same SSE
          channel and patches snapshot.progress without flipping the snapshot
          status away from "completed"). */}
      {snapshot.status === "running" ||
      (typeof snapshot.progress === "object" &&
        snapshot.progress !== null &&
        (() => {
          const p = (snapshot.progress as { phase?: unknown }).phase;
          return typeof p === "string" && [
            "preflight", "download_artifacts", "verify_artifacts",
            "maintenance_on", "stage_files", "swap_files", "restore_db",
            "migrate_db", "swap_db", "post_hooks", "maintenance_off",
            "cleanup", "rolled_back",
          ].includes(p);
        })()) ? (
        <SnapshotProgressCard snapshot={snapshot} />
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
          onRequested={() => setRestoreRequestedAt(Date.now())}
          snapshotId={snapshot.id}
          entries={entries}
          // TODO(forms-architect): snapshot-detail should fetch the site and
          // pass site.host once Sprint 4 wires site lookups; for now the
          // destructive-confirm falls back to the snapshot's short id.
          snapshotTakenAt={snapshot.created_at}
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
