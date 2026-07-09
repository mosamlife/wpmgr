import { useState, useMemo, type ReactNode } from "react";
import { Link } from "@tanstack/react-router";
import {
  Lock,
  LockOpen,
  Info,
  ChevronDown,
  ChevronRight,
  Loader2,
  MoreHorizontal,
} from "lucide-react";

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
import { Checkbox } from "@/components/ui/checkbox";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { PageError } from "@/components/feedback";
import { StatusChip } from "@/components/status/status-chip";
import type { StatusTone } from "@/components/status/status-dot";
import { DestructiveConfirm } from "@/components/dialogs/destructive-confirm";
import { BackupDestinationRequiredPrompt } from "@/components/dialogs/backup-destination-required-prompt";
import {
  Dialog,
  DialogBody,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { ByoDestinationRequiredError } from "@/lib/api";
import { useMe, activeRole } from "@/features/auth/use-auth";
import {
  useBackups,
  useCreateBackup,
  useDeleteBackup,
  useCancelBackup,
  useLockBackup,
  useUnlockBackup,
  useBackupSettingsContents,
  useBackupSchedule,
  isTerminal,
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
import {
  useBackupsSelection,
  type BackupsSelection,
} from "@/features/backups/use-backups-selection";
import {
  chainDependents,
  terminalChainDependents,
  memberCheckState,
  chainCheckState,
  countAutoIncludedDependents,
  reclaimableBytes,
} from "@/features/backups/backups-chain-selection";
import {
  useBulkDeleteBackups,
  computeBulkDeleteToken,
} from "@/features/backups/use-bulk-delete-backups";
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

  // 402 byo_destination_required (M16 Phase B backup-destinations Phase 2):
  // swap to the shared BackupDestinationRequiredPrompt instead of rendering
  // it as a generic inline error below — the two are never shown stacked.
  const { data: me } = useMe();
  const [destinationRequired, setDestinationRequired] =
    useState<ByoDestinationRequiredError | null>(null);

  function onBackup() {
    create.mutate(
      {},
      {
        onError: (err) => {
          if (err instanceof ByoDestinationRequiredError) {
            setDestinationRequired(err);
          }
        },
      },
    );
  }

  const hasComponents =
    contents?.backup_components !== null &&
    (contents?.backup_components?.length ?? 0) > 0;
  const contentsNote = hasComponents
    ? `Uses saved contents settings (${contents!.backup_components!.join(", ")}).`
    : "Uses your saved backup contents settings (full backup by default).";

  const genericError =
    create.isError && !(create.error instanceof ByoDestinationRequiredError)
      ? create.error.message
      : null;

  return (
    <div className="flex shrink-0 flex-col items-end gap-1.5">
      <Button size="sm" onClick={onBackup} disabled={create.isPending}>
        {create.isPending ? "Starting…" : "Run backup"}
      </Button>
      <p className="flex items-center gap-1 text-xs text-muted-foreground">
        <Info aria-hidden className="size-3 shrink-0" />
        {contentsNote}
      </p>
      {genericError ? (
        <p role="alert" className="text-xs text-destructive-subtle-fg">
          {genericError}
        </p>
      ) : null}
      {create.isSuccess ? (
        <p role="status" className="text-xs text-muted-foreground">
          Backup started. It appears below as it progresses.
        </p>
      ) : null}
      <BackupDestinationRequiredPrompt
        open={destinationRequired !== null}
        onClose={() => setDestinationRequired(null)}
        plan={destinationRequired?.plan ?? "free"}
        canUpgrade={Boolean(me?.hosted) && activeRole(me) === "owner"}
      />
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
  const byId = useMemo(
    () => new Map((data ?? []).map((s) => [s.id, s] as const)),
    [data],
  );
  const allIds = useMemo(() => (data ?? []).map((s) => s.id), [data]);
  const deletableIds = useMemo(
    () => (data ?? []).filter((s) => isTerminal(s.status)).map((s) => s.id),
    [data],
  );
  const failedIds = useMemo(
    () => (data ?? []).filter((s) => s.status === "failed").map((s) => s.id),
    [data],
  );
  const zeroByteIds = useMemo(
    () =>
      (data ?? [])
        .filter((s) => isTerminal(s.status) && (s.total_size ?? 0) === 0)
        .map((s) => s.id),
    [data],
  );

  // Issue #115 — bulk-delete selection state. Local to this list (not a
  // module singleton — see use-backups-selection.ts), pruned against the
  // live id set on every poll so a row that disappears never lingers
  // "selected" in the toolbar count or a submitted batch.
  const selection = useBackupsSelection(allIds);
  const bulkDelete = useBulkDeleteBackups(siteId, selection.clear);
  const [pendingBatch, setPendingBatch] = useState<BackupSnapshot[] | null>(
    null,
  );

  // The raw selection already contains auto-included dependents (see
  // ChainMemberRow's toggle handler), so mapping it straight through `byId`
  // gives the full effective batch — no separate "expansion" pass needed.
  const effectiveBatch = useMemo(
    () =>
      Array.from(selection.selected)
        .map((id) => byId.get(id))
        .filter((s): s is BackupSnapshot => s !== undefined),
    [selection.selected, byId],
  );
  const selectedBytes = useMemo(
    () => effectiveBatch.reduce((sum, s) => sum + (s.total_size ?? 0), 0),
    [effectiveBatch],
  );

  function openConfirm(batch: BackupSnapshot[]) {
    if (batch.length === 0) return;
    setPendingBatch(batch);
  }

  function closeConfirm() {
    if (bulkDelete.isPending) return;
    setPendingBatch(null);
    bulkDelete.reset();
  }

  /**
   * "Delete entire chain..." (the chain parent's overflow menu) is a single
   * action: mark the chain's deletable members selected (so the checkboxes
   * reflect it) AND open the confirm directly against exactly those members —
   * it does not wait for a second "Delete selected" click.
   */
  function handleDeleteChain(group: SnapshotGroup) {
    const eligible = group.members.filter((m) => isTerminal(m.status));
    if (eligible.length === 0) return;
    selection.setMany(eligible.map((m) => m.id), true);
    openConfirm(eligible);
  }

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
    <div className="space-y-3">
      {canOperate ? (
        <QuickSelectChips
          failedIds={failedIds}
          zeroByteIds={zeroByteIds}
          onSelect={(ids) => selection.setMany(ids, true)}
        />
      ) : null}

      {canOperate && selection.count > 0 ? (
        <SelectionToolbar
          count={selection.count}
          reclaimableBytesTotal={selectedBytes}
          onClear={selection.clear}
          onDelete={() => openConfirm(effectiveBatch)}
        />
      ) : null}

      <Table>
        <TableHeader>
          <TableRow>
            {canOperate ? (
              <TableHead className="w-10">
                <SelectAllHeaderCheckbox
                  deletableIds={deletableIds}
                  selection={selection}
                />
              </TableHead>
            ) : null}
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
                selection={selection}
              />
            ) : (
              <ChainGroupRows
                key={group.key}
                group={group}
                siteId={siteId}
                canOperate={canOperate}
                selection={selection}
                onDeleteChain={handleDeleteChain}
              />
            ),
          )}
        </TableBody>
      </Table>

      {canOperate ? (
        <BulkDeleteConfirmDialogs
          batch={pendingBatch ?? []}
          open={pendingBatch !== null}
          isPending={bulkDelete.isPending}
          errorMessage={bulkDelete.isError ? bulkDelete.error.message : null}
          onClose={closeConfirm}
          onConfirm={() => {
            const ids = (pendingBatch ?? [])
              .filter((s) => !s.locked)
              .map((s) => s.id);
            if (ids.length === 0) return;
            bulkDelete.mutate(ids, {
              onSuccess: () => setPendingBatch(null),
            });
          }}
        />
      ) : null}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Bulk-delete selection UI (issue #115)
// ---------------------------------------------------------------------------

/** Header checkbox — select-all-deletable, tri-state via the indeterminate ref trick. */
function SelectAllHeaderCheckbox({
  deletableIds,
  selection,
}: {
  deletableIds: readonly string[];
  selection: BackupsSelection;
}) {
  const allSelected =
    deletableIds.length > 0 &&
    deletableIds.every((id) => selection.selected.has(id));
  const someSelected =
    deletableIds.some((id) => selection.selected.has(id)) && !allSelected;

  return (
    <Checkbox
      aria-label={
        allSelected ? "Deselect all snapshots" : "Select all deletable snapshots"
      }
      checked={allSelected}
      ref={(el) => {
        if (el) el.indeterminate = someSelected;
      }}
      disabled={deletableIds.length === 0}
      onChange={() => selection.setMany(deletableIds, !allSelected)}
    />
  );
}

const QUICK_SELECT_CHIP_CLASSES =
  "inline-flex items-center gap-1.5 rounded-full border border-border px-2.5 py-1 text-xs font-medium text-muted-foreground transition-colors hover:border-primary/50 hover:bg-primary/5 hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2";

/** "All failed (K)" / "All 0 B (K)" quick-select pills — the junk-clearing fast path. */
function QuickSelectChips({
  failedIds,
  zeroByteIds,
  onSelect,
}: {
  failedIds: readonly string[];
  zeroByteIds: readonly string[];
  onSelect: (ids: readonly string[]) => void;
}) {
  if (failedIds.length === 0 && zeroByteIds.length === 0) return null;

  return (
    <div className="flex flex-wrap items-center gap-2">
      <span className="text-xs font-medium uppercase tracking-wide text-muted-foreground">
        Quick select
      </span>
      {failedIds.length > 0 ? (
        <button
          type="button"
          className={QUICK_SELECT_CHIP_CLASSES}
          onClick={() => onSelect(failedIds)}
        >
          All failed ({failedIds.length})
        </button>
      ) : null}
      {zeroByteIds.length > 0 ? (
        <button
          type="button"
          className={QUICK_SELECT_CHIP_CLASSES}
          onClick={() => onSelect(zeroByteIds)}
        >
          All 0 B ({zeroByteIds.length})
        </button>
      ) : null}
    </div>
  );
}

/**
 * Slim selection bar rendered between the card header and the table once a
 * snapshot is selected (SitesToolbar ActionMode style, scaled down for a
 * single card rather than a full page toolbar).
 */
function SelectionToolbar({
  count,
  reclaimableBytesTotal,
  onClear,
  onDelete,
}: {
  count: number;
  reclaimableBytesTotal: number;
  onClear: () => void;
  onDelete: () => void;
}) {
  const noun = count === 1 ? "snapshot" : "snapshots";
  return (
    <div
      role="toolbar"
      aria-label="Bulk snapshot actions"
      className="flex flex-wrap items-center justify-between gap-3 rounded-md border border-border bg-muted/30 px-3 py-2"
    >
      <span
        aria-live="polite"
        className="flex flex-wrap items-center gap-2 text-sm text-foreground"
      >
        <span className="font-mono font-medium tabular-nums">{count}</span>
        <span className="text-muted-foreground">{noun} selected</span>
        <span aria-hidden="true" className="text-muted-foreground">
          ·
        </span>
        <button
          type="button"
          onClick={onClear}
          className="text-sm font-medium text-primary underline-offset-4 hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2"
        >
          Clear selection
        </button>
      </span>
      <div className="flex items-center gap-3">
        <span className="text-xs text-muted-foreground">
          ~{formatBytes(reclaimableBytesTotal)} reclaimable
        </span>
        <Button
          type="button"
          variant="outline"
          size="sm"
          className="gap-1.5 text-destructive-subtle-fg"
          onClick={onDelete}
        >
          Delete selected ({count})
        </Button>
      </div>
    </div>
  );
}

/** The chain parent's overflow menu: "Select chain (M)" + "Delete entire chain...". */
function ChainOverflowMenu({
  eligibleCount,
  onSelectChain,
  onDeleteChain,
}: {
  eligibleCount: number;
  onSelectChain: () => void;
  onDeleteChain: () => void;
}) {
  if (eligibleCount === 0) return null;
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button
          type="button"
          variant="outline"
          size="sm"
          aria-label="More chain actions"
          className="px-2"
        >
          <MoreHorizontal aria-hidden="true" className="size-3.5" />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end">
        <DropdownMenuItem onSelect={onSelectChain}>
          Select chain ({eligibleCount})
        </DropdownMenuItem>
        <DropdownMenuItem
          onSelect={onDeleteChain}
          className="text-destructive focus:bg-destructive/10 focus:text-destructive"
        >
          Delete entire chain...
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

/** Shared breakdown block rendered inside BOTH confirm tiers. */
function BulkDeleteSummaryBody({
  submittable,
  lockedCount,
  dependentsCount,
  reclaimable,
}: {
  submittable: BackupSnapshot[];
  lockedCount: number;
  dependentsCount: number;
  reclaimable: number;
}) {
  const failedCount = submittable.filter((s) => s.status === "failed").length;
  const zeroByteCount = submittable.filter(
    (s) => (s.total_size ?? 0) === 0,
  ).length;
  const completedCount = submittable.filter(
    (s) => s.status === "completed",
  ).length;

  return (
    <div className="space-y-2 text-sm">
      {submittable.length > 0 ? (
        <p className="text-muted-foreground">
          {failedCount} failed, {zeroByteCount} 0 B, {completedCount} completed
        </p>
      ) : null}
      {dependentsCount > 0 ? (
        <p className="text-muted-foreground">
          +{dependentsCount} dependent{dependentsCount === 1 ? "" : "s"}{" "}
          auto-included. Later generations depend on the ones you selected.
        </p>
      ) : null}
      {lockedCount > 0 ? (
        <p className="text-warning-subtle-fg">
          {lockedCount} locked excluded. Unlock{" "}
          {lockedCount === 1 ? "it" : "them"} first (the Lock button on the
          row) to include {lockedCount === 1 ? "it" : "them"} in a future
          batch.
        </p>
      ) : null}
      {submittable.length > 0 ? (
        <p className="text-muted-foreground">
          Reclaims ~{formatBytes(reclaimable)}.
        </p>
      ) : null}
    </div>
  );
}

/**
 * Two confirm tiers (issue #115's core win):
 *   LIGHT  — plain AlertDialog, no typing, when every submitted snapshot is
 *            failed or 0 B (the junk-clearing fast path).
 *   STRONG — type-to-confirm the whole-batch token "DELETE N SNAPSHOTS" when
 *            the batch contains any completed, non-zero-byte snapshot.
 * Locked snapshots are excluded from `submittable` (never auto-unlocked) —
 * both tiers surface a "K locked excluded" line when that applies.
 */
function BulkDeleteConfirmDialogs({
  batch,
  open,
  isPending,
  errorMessage,
  onClose,
  onConfirm,
}: {
  batch: BackupSnapshot[];
  open: boolean;
  isPending: boolean;
  errorMessage: string | null;
  onClose: () => void;
  onConfirm: () => void;
}) {
  const submittable = useMemo(() => batch.filter((s) => !s.locked), [batch]);
  const lockedCount = batch.length - submittable.length;
  const dependentsCount = useMemo(
    () => countAutoIncludedDependents(submittable),
    [submittable],
  );
  const reclaimable = useMemo(
    () => reclaimableBytes(submittable),
    [submittable],
  );
  // Any completed snapshot escalates to type-to-confirm, regardless of size:
  // the light path is reserved for failed/0-byte junk only.
  const strong = submittable.some((s) => s.status === "completed");
  const n = submittable.length;
  const title = `Delete ${n} snapshot${n === 1 ? "" : "s"}`;

  if (!open) return null;

  if (strong) {
    return (
      <DestructiveConfirm
        open={open}
        onClose={onClose}
        onConfirm={onConfirm}
        title={title}
        consequencesBody={
          <>
            <p>
              This permanently deletes {n} snapshot{n === 1 ? "" : "s"} and
              reclaims their storage. This cannot be undone.
            </p>
            <BulkDeleteSummaryBody
              submittable={submittable}
              lockedCount={lockedCount}
              dependentsCount={dependentsCount}
              reclaimable={reclaimable}
            />
          </>
        }
        resourceName={computeBulkDeleteToken(n)}
        confirmLabel="Delete snapshots"
        cancelLabel="Keep snapshots"
        isPending={isPending}
        errorMessage={errorMessage}
      />
    );
  }

  return (
    <AlertDialog open={open} onOpenChange={(o) => !o && onClose()}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>{title}</AlertDialogTitle>
          <AlertDialogDescription>
            {n === 0
              ? "Every selected snapshot is locked. Unlock them first to delete."
              : "These are all failed or empty snapshots. This permanently deletes them and reclaims their storage."}
          </AlertDialogDescription>
        </AlertDialogHeader>
        <BulkDeleteSummaryBody
          submittable={submittable}
          lockedCount={lockedCount}
          dependentsCount={dependentsCount}
          reclaimable={reclaimable}
        />
        {errorMessage ? (
          <p role="alert" className="text-sm text-destructive-subtle-fg">
            {errorMessage}
          </p>
        ) : null}
        <AlertDialogFooter>
          <AlertDialogCancel onClick={onClose} disabled={isPending} />
          <AlertDialogAction
            variant="destructive"
            disabled={isPending || n === 0}
            onClick={onConfirm}
          >
            {isPending ? (
              <Loader2 aria-hidden="true" className="size-4 animate-spin" />
            ) : null}
            Delete
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}

/** One flat row for a full/legacy snapshot — zero visual regression vs before. */
function SingletonRow({
  snap,
  siteId,
  canOperate,
  selection,
}: {
  snap: BackupSnapshot;
  siteId: string;
  canOperate: boolean;
  selection: BackupsSelection;
}) {
  const deletable = isTerminal(snap.status);
  return (
    <TableRow data-testid="backup-row">
      {canOperate ? (
        <TableCell onClick={(e) => e.stopPropagation()}>
          {deletable ? (
            <div className="flex items-center gap-1">
              <Checkbox
                aria-label={`Select snapshot ${snap.id.slice(0, 8)}`}
                checked={selection.selected.has(snap.id)}
                onChange={() => selection.toggle(snap.id)}
              />
              {snap.locked ? (
                <span title="Locked, excluded from bulk delete until unlocked">
                  <Lock aria-hidden="true" className="size-3 text-muted-foreground" />
                </span>
              ) : null}
            </div>
          ) : (
            <span
              title={`Selectable once this ${snap.status} snapshot finishes`}
              className="inline-flex size-4 items-center justify-center text-xs text-muted-foreground/40"
            >
              <span className="sr-only">Not selectable while {snap.status}</span>
              –
            </span>
          )}
        </TableCell>
      ) : null}
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
              to="/sites/$siteId/backups/$snapshotId"
              params={{ siteId, snapshotId: snap.id }}
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
  selection,
  onDeleteChain,
}: {
  group: SnapshotGroup;
  siteId: string;
  canOperate: boolean;
  selection: BackupsSelection;
  onDeleteChain: (group: SnapshotGroup) => void;
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

  // Issue #115 — the chain parent's own tri-state "select chain" checkbox,
  // computed over the terminal (deletable) members only.
  const eligibleIds = useMemo(
    () => group.members.filter((m) => isTerminal(m.status)).map((m) => m.id),
    [group.members],
  );
  const chainState = chainCheckState(selection.selected, group.members);

  return (
    <>
      {/* Parent row — the TIP */}
      <TableRow data-testid="backup-row" data-chain-id={group.key}>
        {canOperate ? <TableCell aria-hidden="true" /> : null}
        <TableCell>
          <div className="flex flex-col items-start gap-1">
            <div className="flex items-center gap-1">
              {canOperate ? (
                <Checkbox
                  aria-label={
                    chainState === "checked"
                      ? "Deselect all snapshots in this chain"
                      : `Select all ${eligibleIds.length} deletable snapshots in this chain`
                  }
                  checked={chainState === "checked"}
                  ref={(el) => {
                    if (el) el.indeterminate = chainState === "indeterminate";
                  }}
                  disabled={eligibleIds.length === 0}
                  onChange={() =>
                    selection.setMany(eligibleIds, chainState !== "checked")
                  }
                  onClick={(e) => e.stopPropagation()}
                />
              ) : null}
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
                to="/sites/$siteId/backups/$snapshotId"
                params={{ siteId, snapshotId: tip.id }}
              >
                View
              </Link>
            </Button>
            {canOperate ? (
              <ChainOverflowMenu
                eligibleCount={eligibleIds.length}
                onSelectChain={() => selection.setMany(eligibleIds, true)}
                onDeleteChain={() => onDeleteChain(group)}
              />
            ) : null}
          </div>
        </TableCell>
      </TableRow>

      {/* Expanded child rows — one per member, generation ASC */}
      {expanded
        ? group.members.map((member) => (
            <ChainMemberRow
              key={member.id}
              member={member}
              members={group.members}
              siteId={siteId}
              canOperate={canOperate}
              selection={selection}
            />
          ))
        : null}

      {/* Restore dialog seeded with the full chain (version-picker) */}
      {restoreOpen ? (
        <tr aria-hidden>
          <td colSpan={canOperate ? 8 : 7} className="p-0">
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
  members,
  siteId,
  canOperate,
  selection,
}: {
  member: BackupSnapshot;
  /** The full chain (all generations, ASC) — used to derive dependents. */
  members: BackupSnapshot[];
  siteId: string;
  canOperate: boolean;
  selection: BackupsSelection;
}) {
  const gen = member.generation ?? 0;
  const genLabel = gen === 0 ? "base" : `gen ${gen}`;
  const deletable = isTerminal(member.status);
  const checkState = memberCheckState(selection.selected, members, member);

  // Issue #115 auto-expand: checking a member auto-checks every same-chain
  // dependent (higher generation) so the operator always sees the full
  // effective set. Unchecking only ever removes the clicked id itself — an
  // ancestor that becomes newly-invalid (its dependent got unchecked) is
  // reflected automatically via memberCheckState's "indeterminate" branch,
  // no explicit cascade needed.
  function onToggle() {
    if (selection.selected.has(member.id)) {
      selection.toggle(member.id);
      return;
    }
    const depIds = terminalChainDependents(members, member).map((d) => d.id);
    selection.setMany([member.id, ...depIds], true);
  }

  return (
    <TableRow
      data-testid="backup-chain-member"
      className="bg-muted/30 hover:bg-muted/50"
    >
      {canOperate ? (
        <TableCell onClick={(e) => e.stopPropagation()}>
          {deletable ? (
            <div className="flex items-center gap-1">
              <Checkbox
                aria-label={`Select ${genLabel} snapshot`}
                checked={checkState === "checked"}
                ref={(el) => {
                  if (el) el.indeterminate = checkState === "indeterminate";
                }}
                onChange={onToggle}
              />
              {member.locked ? (
                <span title="Locked, excluded from bulk delete until unlocked">
                  <Lock aria-hidden="true" className="size-3 text-muted-foreground" />
                </span>
              ) : null}
            </div>
          ) : (
            <span
              title={`Selectable once this ${member.status} snapshot finishes`}
              className="inline-flex size-4 items-center justify-center text-xs text-muted-foreground/40"
            >
              <span className="sr-only">Not selectable while {member.status}</span>
              –
            </span>
          )}
        </TableCell>
      ) : null}
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
              to="/sites/$siteId/backups/$snapshotId"
              params={{ siteId, snapshotId: member.id }}
            >
              View
            </Link>
          </Button>
          {canOperate ? (
            <BackupRowActions
              snapshot={member}
              siteId={siteId}
              chainMembers={members}
            />
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
 *   - Delete shows only for terminal snapshots (completed/failed). The server
 *     is chain-safe and refuses to delete a base/mid-chain increment that
 *     still has dependents (422 chain_has_dependents). When `chainMembers` is
 *     passed (issue #115 — a member of a 2+ chain) this row already knows its
 *     dependent count locally and surfaces it up front — both on the button
 *     label ("Delete + N dependents") and in the confirm body — instead of
 *     dead-ending the operator at a server error after the fact.
 *   - A LOCKED snapshot cannot be deleted (the server returns "snapshot_locked").
 *     Clicking Delete on it opens a warning that explains the lock and offers to
 *     unlock first, rather than dead-ending the operator at a server error.
 */
function BackupRowActions({
  snapshot,
  siteId,
  chainMembers,
}: {
  snapshot: BackupSnapshot;
  siteId: string;
  /** The full chain this snapshot belongs to (all generations), if any. */
  chainMembers?: BackupSnapshot[];
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
  const dependentsCount = chainMembers
    ? chainDependents(chainMembers, snapshot).length
    : 0;

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
          {dependentsCount > 0
            ? `Delete + ${dependentsCount} dependents`
            : "Delete"}
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
          <>
            <p>
              This permanently deletes the snapshot and reclaims its storage.
              Unique chunks are removed; chunks still used by other snapshots are
              kept. This cannot be undone.
            </p>
            {dependentsCount > 0 ? (
              <p className="text-warning-subtle-fg">
                This snapshot has {dependentsCount} later increment
                {dependentsCount === 1 ? "" : "s"} in its chain that depend on
                it. Deleting it alone will be rejected. Delete the dependents
                first, or use the checkboxes to bulk-delete the whole chain at
                once.
              </p>
            ) : null}
          </>
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
                to="/sites/$siteId/backups/$snapshotId"
                params={{ siteId: run.site_id, snapshotId: run.snapshot_id }}
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
