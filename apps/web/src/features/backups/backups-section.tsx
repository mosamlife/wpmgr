import { useState, useMemo, type ReactNode } from "react";
import { Link } from "@tanstack/react-router";
import { Lock, LockOpen, Info, ChevronDown, ChevronRight } from "lucide-react";

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
import { Skeleton } from "@/components/ui/skeleton";
import { PageError } from "@/components/feedback";
import { StatusChip } from "@/components/status/status-chip";
import type { StatusTone } from "@/components/status/status-dot";
import { DestructiveConfirm } from "@/components/dialogs/destructive-confirm";
import {
  Dialog,
  DialogBody,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import {
  useBackups,
  useCreateBackup,
  useDeleteBackup,
  useCancelBackup,
  useLockBackup,
  useUnlockBackup,
  useBackupSettingsContents,
  useBackupSchedule,
} from "@/features/backups/use-backups";
import {
  StatusBadge,
  KindBadge,
  IncrementalBadge,
} from "@/features/backups/backup-badges";
import { InlineSnapshotProgress } from "@/features/backups/inline-snapshot-progress";
import { isRestoreActive, PHASE_LABEL } from "@/features/backups/format-progress";
import { RestoreDialog } from "@/features/backups/restore-dialog";
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
import {
  BackupScheduleEditor,
  NextRunLine,
} from "@/features/backups/backup-schedule-editor";
import { formatBytes, relativeTime } from "@/lib/utils";
import type { BackupSnapshot } from "@wpmgr/api";

// The "Backups" section rendered on the site detail page. One card holds the
// snapshot list; "Back up now" lives as a header control (not an inset
// bordered box) so the surface is flat (ADR-037 Batch 2 — never card-in-card).
// Viewers see the list only; the schedule editor (operator+) is its own card.

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
          <SnapshotList siteId={siteId} canOperate={canOperate} />
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

/**
 * Run-backup control (operator+). Scope is always resolved server-side from
 * site_backup_settings at worker dispatch time — no per-run override dialog.
 */
function BackupNowControl({ siteId }: { siteId: string }) {
  const create = useCreateBackup(siteId);
  // Read saved contents settings so the note below the button reflects what
  // the worker will use when it dispatches. We never pass these in the body —
  // they are resolved server-side from site_backup_settings.
  const { data: contents } = useBackupSettingsContents(siteId);

  function onBackup() {
    create.mutate({}, { onError: () => {} });
  }

  const hasComponents =
    contents?.backup_components !== null &&
    (contents?.backup_components?.length ?? 0) > 0;
  const contentsNote = hasComponents
    ? `Uses saved contents settings (${contents!.backup_components!.join(", ")}).`
    : "Uses your saved backup contents settings (full backup by default).";

  return (
    <div className="flex shrink-0 flex-col items-end gap-1.5">
      <Button size="sm" onClick={onBackup} disabled={create.isPending}>
        {create.isPending ? "Starting…" : "Run backup"}
      </Button>
      <p className="flex items-center gap-1 text-xs text-muted-foreground">
        <Info aria-hidden className="size-3 shrink-0" />
        {contentsNote}
      </p>
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

// ---------------------------------------------------------------------------
// Chain grouping helpers (issue #177 — grouped chain rows)
// ---------------------------------------------------------------------------

/**
 * A snapshot group. A SINGLETON has one member (a full/legacy snapshot, or a
 * chain with only its base). A CHAIN group has 2+ members sorted by generation
 * ASC; the TIP (last element) is the highest generation.
 */
interface SnapshotGroup {
  /** The chain_id that binds this group, or the snapshot id for singletons. */
  key: string;
  /** All members, sorted by generation ASC (gen 0 first, tip last). */
  members: BackupSnapshot[];
}

/**
 * Group a flat snapshot list (API returns newest-created first) into chain
 * groups ordered by the TIP's created_at DESC (same visual order the flat list
 * had). Within each group members are sorted by generation ASC.
 *
 * Grouping key: chain_id when present, snapshot id otherwise. A group with a
 * single member is a SINGLETON and renders exactly as the old flat row.
 */
function groupSnapshots(snaps: BackupSnapshot[]): SnapshotGroup[] {
  const map = new Map<string, BackupSnapshot[]>();

  for (const snap of snaps) {
    const key = snap.chain_id ?? snap.id;
    const bucket = map.get(key);
    if (bucket) {
      bucket.push(snap);
    } else {
      map.set(key, [snap]);
    }
  }

  const groups: SnapshotGroup[] = Array.from(map.entries()).map(
    ([key, members]) => ({
      key,
      members: [...members].sort((a, b) => (a.generation ?? 0) - (b.generation ?? 0)),
    }),
  );

  // Keep the same visual order as the flat list: sort by the TIP's created_at
  // descending (newest chain/singleton first). The non-null assertions are
  // safe: every group has at least one member by construction above.
  groups.sort((a, b) => {
    const tipA = a.members[a.members.length - 1]!;
    const tipB = b.members[b.members.length - 1]!;
    return tipB.created_at.localeCompare(tipA.created_at);
  });

  return groups;
}

function SnapshotList({
  siteId,
  canOperate,
}: {
  siteId: string;
  canOperate: boolean;
}) {
  const { data, isPending, isError, error, refetch } = useBackups(siteId);

  const groups = useMemo(
    () => (data ? groupSnapshots(data) : []),
    [data],
  );

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
        {groups.map((group) =>
          group.members.length === 1 ? (
            // Safe: length===1 guarantees members[0] exists.
            <SingletonRow
              key={group.key}
              snap={group.members[0]!}
              siteId={siteId}
              canOperate={canOperate}
            />
          ) : (
            <ChainGroupRows
              key={group.key}
              group={group}
              siteId={siteId}
              canOperate={canOperate}
            />
          ),
        )}
      </TableBody>
    </Table>
  );
}

/** One flat row for a full/legacy snapshot — zero visual regression vs before. */
function SingletonRow({
  snap,
  siteId,
  canOperate,
}: {
  snap: BackupSnapshot;
  siteId: string;
  canOperate: boolean;
}) {
  return (
    <TableRow data-testid="backup-row">
      <TableCell>
        <div className="flex flex-col items-start gap-1">
          <KindBadge kind={snap.kind} />
          <IncrementalBadge
            isIncremental={snap.is_incremental}
            generation={snap.generation}
          />
        </div>
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
        <div className="flex items-center justify-end gap-2">
          {canOperate ? (
            <SnapshotLockToggle snapshot={snap} siteId={siteId} />
          ) : null}
          <Button asChild variant="outline" size="sm">
            <Link
              to="/backups/$snapshotId"
              params={{ snapshotId: snap.id }}
            >
              View
            </Link>
          </Button>
          {canOperate ? (
            <BackupRowActions snapshot={snap} siteId={siteId} />
          ) : null}
        </div>
      </TableCell>
    </TableRow>
  );
}

/**
 * An expandable parent row for an incremental chain (2+ members).
 * The parent shows the TIP (highest generation); a chevron toggle expands
 * all members sorted generation ASC as indented child rows.
 */
function ChainGroupRows({
  group,
  siteId,
  canOperate,
}: {
  group: SnapshotGroup;
  siteId: string;
  canOperate: boolean;
}) {
  const [expanded, setExpanded] = useState(false);

  // Non-null assertion: ChainGroupRows is only rendered when members.length >= 2
  // (guarded at the call site in SnapshotList). The tip is always the last
  // element in the generation-ASC sorted array.
  const tip = group.members[group.members.length - 1]!;
  const baseCount = 1; // gen-0 member
  const incrCount = group.members.length - baseCount;

  // Aggregate totals across all members for the parent row display.
  const totalSize = useMemo(
    () => group.members.reduce((acc, s) => acc + (s.total_size ?? 0), 0),
    [group.members],
  );
  const totalChunks = useMemo(
    () => group.members.reduce((acc, s) => acc + (s.chunk_count ?? 0), 0),
    [group.members],
  );

  // If any member is in-flight, show live progress on the parent.
  const inFlightMember = group.members.find(
    (s) => s.status === "running" || s.status === "pending" || isRestoreActive(s),
  );

  // Restore dialog state: open on the chain, version-picker defaults to tip.
  const [restoreOpen, setRestoreOpen] = useState(false);

  const subLabel = `base + ${incrCount} increment${incrCount === 1 ? "" : "s"}`;

  return (
    <>
      {/* Parent row — the TIP */}
      <TableRow data-testid="backup-row" data-chain-id={group.key}>
        <TableCell>
          <div className="flex flex-col items-start gap-1">
            <div className="flex items-center gap-1">
              <button
                type="button"
                aria-label={expanded ? "Collapse chain" : "Expand chain members"}
                aria-expanded={expanded}
                onClick={() => setExpanded((v) => !v)}
                className="flex items-center gap-1 rounded p-0.5 text-muted-foreground hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
              >
                {expanded ? (
                  <ChevronDown aria-hidden className="size-3.5 shrink-0" />
                ) : (
                  <ChevronRight aria-hidden className="size-3.5 shrink-0" />
                )}
              </button>
              <KindBadge kind={tip.kind} />
            </div>
            <IncrementalBadge
              isIncremental={tip.is_incremental}
              generation={tip.generation}
            />
            <span className="text-xs text-muted-foreground">{subLabel}</span>
          </div>
        </TableCell>
        <TableCell>
          <div className="flex flex-col gap-1">
            <StatusBadge status={tip.status} />
            {inFlightMember ? (
              <InlineSnapshotProgress snapshot={inFlightMember} />
            ) : null}
            {tip.status === "failed" && tip.error ? (
              <span
                role="alert"
                className="text-xs text-destructive-subtle-fg"
              >
                {tip.error}
              </span>
            ) : null}
          </div>
        </TableCell>
        <TableCell className="tabular-nums">
          {formatBytes(totalSize)}
        </TableCell>
        <TableCell className="tabular-nums">
          {totalChunks > 0 ? totalChunks : "–"}
        </TableCell>
        <TableCell className="tabular-nums" title={tip.created_at}>
          {relativeTime(tip.created_at) ?? "–"}
        </TableCell>
        <TableCell
          className="tabular-nums"
          title={tip.finished_at ?? undefined}
        >
          {relativeTime(tip.finished_at) ?? "–"}
        </TableCell>
        <TableCell className="text-right">
          <div className="flex items-center justify-end gap-2">
            {canOperate && tip.status === "completed" ? (
              <Button
                variant="outline"
                size="sm"
                onClick={() => setRestoreOpen(true)}
              >
                Restore
              </Button>
            ) : null}
            <Button asChild variant="outline" size="sm">
              <Link
                to="/backups/$snapshotId"
                params={{ snapshotId: tip.id }}
              >
                View
              </Link>
            </Button>
          </div>
        </TableCell>
      </TableRow>

      {/* Expanded child rows — one per member, generation ASC */}
      {expanded
        ? group.members.map((member) => (
            <ChainMemberRow
              key={member.id}
              member={member}
              siteId={siteId}
              canOperate={canOperate}
            />
          ))
        : null}

      {/* Restore dialog seeded with the full chain (version-picker) */}
      {restoreOpen ? (
        <tr aria-hidden>
          <td colSpan={7} className="p-0">
            <RestoreDialog
              open={restoreOpen}
              onClose={() => setRestoreOpen(false)}
              snapshotId={tip.id}
              entries={[]}
              chainSnapshots={group.members}
            />
          </td>
        </tr>
      ) : null}
    </>
  );
}

/** Indented child row for one chain member (gen N). */
function ChainMemberRow({
  member,
  siteId,
  canOperate,
}: {
  member: BackupSnapshot;
  siteId: string;
  canOperate: boolean;
}) {
  const gen = member.generation ?? 0;
  const genLabel = gen === 0 ? "base" : `gen ${gen}`;

  return (
    <TableRow
      data-testid="backup-chain-member"
      className="bg-muted/30 hover:bg-muted/50"
    >
      <TableCell>
        <div className="flex flex-col items-start gap-1 pl-6">
          <div className="flex items-center gap-1.5">
            <span className="font-mono text-xs font-semibold text-foreground">
              {genLabel}
            </span>
            <span aria-hidden className="text-muted-foreground">·</span>
            <KindBadge kind={member.kind} />
            <IncrementalBadge
              isIncremental={member.is_incremental}
              generation={member.generation}
            />
          </div>
        </div>
      </TableCell>
      <TableCell>
        <div className="flex flex-col gap-1">
          <StatusBadge status={member.status} />
          {member.status === "running" ||
          member.status === "pending" ||
          isRestoreActive(member) ? (
            <InlineSnapshotProgress snapshot={member} />
          ) : null}
          {member.status === "failed" && member.error ? (
            <span role="alert" className="text-xs text-destructive-subtle-fg">
              {member.error}
            </span>
          ) : null}
        </div>
      </TableCell>
      <TableCell className="tabular-nums">
        {formatBytes(member.total_size)}
      </TableCell>
      <TableCell className="tabular-nums">
        {member.chunk_count ?? "–"}
      </TableCell>
      <TableCell className="tabular-nums" title={member.created_at}>
        {relativeTime(member.created_at) ?? "–"}
      </TableCell>
      <TableCell
        className="tabular-nums"
        title={member.finished_at ?? undefined}
      >
        {relativeTime(member.finished_at) ?? "–"}
      </TableCell>
      <TableCell className="text-right">
        <div className="flex items-center justify-end gap-2">
          {canOperate ? (
            <SnapshotLockToggle snapshot={member} siteId={siteId} />
          ) : null}
          <Button asChild variant="outline" size="sm">
            <Link
              to="/backups/$snapshotId"
              params={{ snapshotId: member.id }}
            >
              View
            </Link>
          </Button>
          {canOperate ? (
            <BackupRowActions snapshot={member} siteId={siteId} />
          ) : null}
        </div>
      </TableCell>
    </TableRow>
  );
}

/**
 * Track C (m49) — lock/unlock toggle for a completed snapshot (operator+).
 *
 * A locked snapshot is exempt from retention GC. Only completed snapshots can
 * be locked (the server returns 409 for pending/running; the button is hidden
 * for non-terminal states). Pending/running rows show nothing.
 */
function SnapshotLockToggle({
  snapshot,
  siteId,
}: {
  snapshot: BackupSnapshot;
  siteId: string;
}) {
  const isCompleted = snapshot.status === "completed";
  const isLocked = snapshot.locked === true;

  const lock = useLockBackup(snapshot.id, siteId);
  const unlock = useUnlockBackup(snapshot.id, siteId);

  if (!isCompleted) return null;

  const isPending = lock.isPending || unlock.isPending;

  if (isLocked) {
    return (
      <div className="flex flex-col items-end gap-0.5">
        <Button
          variant="outline"
          size="sm"
          onClick={() => unlock.mutate(undefined, { onError: () => {} })}
          disabled={isPending}
          aria-label="Unlock snapshot — allow GC to prune"
          title="Locked: GC will not prune this snapshot. Click to unlock."
          className="gap-1.5 text-xs"
        >
          <Lock aria-hidden className="size-3.5 shrink-0" />
          {isPending ? "Unlocking…" : "Locked"}
        </Button>
        {unlock.isError ? (
          <span role="alert" className="text-xs text-destructive-subtle-fg">
            {unlock.error.message}
          </span>
        ) : null}
      </div>
    );
  }

  return (
    <div className="flex flex-col items-end gap-0.5">
      <Button
        variant="ghost"
        size="sm"
        onClick={() => lock.mutate(undefined, { onError: () => {} })}
        disabled={isPending}
        aria-label="Lock snapshot — protect from GC pruning"
        title="Unlocked: retention GC may prune. Click to lock."
        className="gap-1.5 text-xs text-muted-foreground hover:text-foreground"
      >
        <LockOpen aria-hidden className="size-3.5 shrink-0" />
        {isPending ? "Locking…" : "Lock"}
      </Button>
      {lock.isError ? (
        <span role="alert" className="text-xs text-destructive-subtle-fg">
          {lock.error.message}
        </span>
      ) : null}
    </div>
  );
}

/**
 * Per-row Cancel/Delete actions for a snapshot (operator+).
 *
 * Gating:
 *   - Cancel shows only for running/pending snapshots and stops the in-flight
 *     run (server marks it failed — there is no "cancelled" status).
 *   - Delete shows only for terminal snapshots (completed/failed). The server is
 *     chain-safe and refuses to delete a base/mid-chain increment that still has
 *     dependents, surfacing that as an inline error in the confirm dialog (we
 *     don't have the chain tip locally to pre-disable it — task #180).
 *   - A LOCKED snapshot cannot be deleted (the server returns "snapshot_locked").
 *     Clicking Delete on it opens a warning that explains the lock and offers to
 *     unlock first, rather than dead-ending the operator at a server error.
 */
function BackupRowActions({
  snapshot,
  siteId,
}: {
  snapshot: BackupSnapshot;
  siteId: string;
}) {
  const [confirm, setConfirm] = useState<null | "cancel" | "delete" | "locked">(
    null,
  );
  const del = useDeleteBackup(snapshot.id, siteId);
  const cancel = useCancelBackup(snapshot.id, siteId);
  const unlock = useUnlockBackup(snapshot.id, siteId);

  const isInFlight =
    snapshot.status === "running" || snapshot.status === "pending";
  const isLocked = snapshot.locked === true;
  const shortId = snapshot.id.slice(0, 8);

  function close() {
    setConfirm(null);
    del.reset();
    cancel.reset();
    unlock.reset();
  }

  return (
    <>
      {isInFlight ? (
        <Button
          variant="outline"
          size="sm"
          onClick={() => setConfirm("cancel")}
        >
          Cancel
        </Button>
      ) : (
        <Button
          variant="outline"
          size="sm"
          className="text-destructive-subtle-fg"
          onClick={() => setConfirm(isLocked ? "locked" : "delete")}
        >
          Delete
        </Button>
      )}

      <DestructiveConfirm
        open={confirm === "cancel"}
        onClose={close}
        onConfirm={() =>
          cancel.mutate(undefined, { onSuccess: () => setConfirm(null) })
        }
        title="Cancel backup"
        consequencesBody={
          <p>
            This stops the in-progress backup. The snapshot is marked failed and
            no data is kept from this run. You can run a new backup at any time.
          </p>
        }
        resourceName={shortId}
        confirmLabel="Cancel backup"
        cancelLabel="Keep running"
        isPending={cancel.isPending}
        errorMessage={cancel.isError ? cancel.error.message : null}
      />

      <DestructiveConfirm
        open={confirm === "delete"}
        onClose={close}
        onConfirm={() =>
          del.mutate(undefined, { onSuccess: () => setConfirm(null) })
        }
        title="Delete backup"
        consequencesBody={
          <p>
            This permanently deletes the snapshot and reclaims its storage.
            Unique chunks are removed; chunks still used by other snapshots are
            kept. This cannot be undone.
          </p>
        }
        resourceName={shortId}
        confirmLabel="Delete backup"
        cancelLabel="Keep backup"
        isPending={del.isPending}
        errorMessage={del.isError ? del.error.message : null}
      />

      <Dialog
        open={confirm === "locked"}
        onClose={unlock.isPending ? () => {} : close}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>This backup is locked</DialogTitle>
          </DialogHeader>
          <DialogBody>
            <p className="text-sm text-foreground">
              A locked backup is protected: retention never prunes it and it
              cannot be deleted. To remove it, unlock it first, then delete.
            </p>
            {unlock.isError ? (
              <p
                role="alert"
                className="rounded-md border border-[var(--color-destructive)]/40 bg-[var(--color-destructive)]/10 p-2 text-sm text-[var(--color-destructive)]"
              >
                {unlock.error.message}
              </p>
            ) : null}
          </DialogBody>
          <DialogFooter className="pt-2">
            <Button
              type="button"
              variant="outline"
              onClick={close}
              disabled={unlock.isPending}
            >
              Keep locked
            </Button>
            <Button
              type="button"
              variant="outline"
              className="text-destructive-subtle-fg"
              onClick={() =>
                unlock.mutate(undefined, {
                  onSuccess: () => setConfirm("delete"),
                })
              }
              disabled={unlock.isPending}
            >
              {unlock.isPending ? "Unlocking…" : "Unlock to delete"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
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
  // Re-use the already-fetched schedule (same Query cache key as BackupScheduleEditor
  // below — no extra network request). We need it to drive the upcoming empty-state:
  // the scheduler only materialises a `backup_schedule_runs` row shortly before a
  // run fires, so `upcoming` is usually empty even when the schedule is enabled.
  const { data: schedule } = useBackupSchedule(siteId);

  const browserTz = Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";

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

  // Projected next runs from the schedule (server-computed, always consistent
  // with the "Next run" and "Next 3 runs" strip in the Backup schedule card
  // below). Only populated when a schedule exists and is enabled — the
  // scheduler materialises `backup_schedule_runs` rows only shortly before
  // each fire, so `upcoming` is usually empty even with an active schedule.
  const projectedRuns: string[] = (() => {
    if (!schedule?.enabled) return [];
    if (schedule.next_runs.length > 0) return schedule.next_runs;
    if (schedule.next_run_at) return [schedule.next_run_at];
    return [];
  })();

  return (
    <div className="space-y-6">
      {/* Upcoming */}
      <div>
        <h3 className="mb-2 text-sm font-semibold text-foreground">Upcoming</h3>
        {upcoming.length > 0 ? (
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
        ) : projectedRuns.length > 0 ? (
          // No materialised rows yet, but the schedule is enabled. Show the
          // server-computed projection so the panel agrees with the "Next run"
          // and "Next 3 runs" strip in the Backup schedule card below.
          <div className="space-y-1.5">
            <p className="text-xs text-muted-foreground">
              Runs are queued shortly before they are due. Projected next{" "}
              {projectedRuns.length === 1 ? "run" : "runs"}:
            </p>
            <ol className="space-y-0.5">
              {projectedRuns.map((iso) => (
                <li key={iso}>
                  <NextRunLine label="" iso={iso} timezone={browserTz} compact />
                </li>
              ))}
            </ol>
          </div>
        ) : (
          // Schedule is disabled (or not yet created). Prompt the user to
          // enable it — only show this when it is actually applicable.
          <p className="text-sm text-muted-foreground">
            No upcoming runs. Enable the backup schedule to queue runs.
          </p>
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
