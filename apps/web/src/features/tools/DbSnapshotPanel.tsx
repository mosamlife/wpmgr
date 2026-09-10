import { useId, useState } from "react";
import {
  AlertTriangle,
  Camera,
  Loader2,
  RotateCcw,
  Trash2,
  XCircle,
} from "lucide-react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Dialog,
  DialogBody,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { toast } from "@/components/toast";
import { cn } from "@/lib/utils";

import {
  useDbSnapshotList,
  useCreateSnapshot,
  useRevertSnapshot,
  useDeleteSnapshot,
  type DbSnapshotEntry,
} from "./use-db-snapshots";

// DbSnapshotPanel — local database snapshot tool (#189).
//
// UX FLOW:
//   1. List view shows existing snapshots sorted newest-first (label, age, size).
//   2. "Take snapshot" button captures the current DB state with an optional label.
//   3. Per-row "Revert" opens a destructive-confirm dialog:
//       - Shows a prominent warning that this REPLACES the entire database.
//       - The operator must type the snapshot label (or "REVERT" if no label) to
//         enable the Revert button.
//       - Mentions the auto-safety snapshot that will be taken before import.
//   4. Per-row trash icon deletes the snapshot (simple confirm popover).
//
// PERMISSIONS: parent route gates on canOperate (PermSiteWrite / operator+).
// This component disables all mutation buttons when canOperate=false.
// PermSiteRead (viewer) can still see the list; write actions are hidden.

interface Props {
  siteId: string;
  canOperate: boolean;
}

export function DbSnapshotPanel({ siteId, canOperate }: Props) {
  const { data, isPending, isError, error, refetch } = useDbSnapshotList(siteId);
  const createMut = useCreateSnapshot(siteId);
  const revertMut = useRevertSnapshot(siteId);
  const deleteMut = useDeleteSnapshot(siteId);

  const [labelInput, setLabelInput] = useState("");
  const [revertTarget, setRevertTarget] = useState<DbSnapshotEntry | null>(null);
  const [deleteTarget, setDeleteTarget] = useState<DbSnapshotEntry | null>(null);

  async function handleCreate() {
    if (createMut.isPending) return;
    try {
      await createMut.mutateAsync({ label: labelInput.trim() || undefined });
      setLabelInput("");
      toast.success("Snapshot captured.");
    } catch (err) {
      toast.error("Snapshot failed", {
        description: err instanceof Error ? err.message : "Unknown error",
      });
    }
  }

  async function handleRevert() {
    if (!revertTarget || revertMut.isPending) return;
    const target = revertTarget;
    setRevertTarget(null);
    try {
      const result = await revertMut.mutateAsync({
        snapshotId: target.id,
        confirm: "REVERT",
      });
      toast.success("Database reverted.", {
        description: result.safety_id
          ? `Safety snapshot saved as ${result.safety_id}.`
          : undefined,
      });
    } catch (err) {
      toast.error("Revert failed", {
        description: err instanceof Error ? err.message : "Unknown error",
      });
    }
  }

  async function handleDelete(snapshot: DbSnapshotEntry) {
    if (deleteMut.isPending) return;
    setDeleteTarget(null);
    try {
      await deleteMut.mutateAsync(snapshot.id);
      toast.success("Snapshot deleted.");
    } catch (err) {
      toast.error("Delete failed", {
        description: err instanceof Error ? err.message : "Unknown error",
      });
    }
  }

  const snapshots = data?.snapshots ?? [];

  return (
    <div className="space-y-6">
      {/* Header */}
      <div>
        <h2 className="text-base font-semibold">Database Snapshots</h2>
        <p className="mt-1 text-sm text-muted-foreground">
          Capture the database before a risky change, such as a plugin update,
          search-replace, or bulk edit. Revert in one click if something breaks.
          Snapshots are stored locally on the WP server; they are separate from
          this site's off-site backups.
        </p>
      </div>

      {/* Take-snapshot form */}
      {canOperate && (
        <div className="flex flex-wrap items-end gap-2">
          <div className="flex-1 space-y-1.5 min-w-[200px]">
            <Label htmlFor="snap-label">Label (optional)</Label>
            <Input
              id="snap-label"
              value={labelInput}
              onChange={(e) => setLabelInput(e.target.value)}
              placeholder="e.g. Before WooCommerce update"
              disabled={createMut.isPending}
            />
          </div>
          <Button
            onClick={() => void handleCreate()}
            disabled={createMut.isPending}
            className="gap-1.5 shrink-0"
          >
            {createMut.isPending ? (
              <Loader2 aria-hidden="true" className="size-4 animate-spin" />
            ) : (
              <Camera aria-hidden="true" className="size-4" />
            )}
            Take snapshot
          </Button>
        </div>
      )}

      {/* State: loading */}
      {isPending && (
        <div className="space-y-2" aria-label="Loading snapshots">
          <Skeleton className="h-12 w-full" />
          <Skeleton className="h-12 w-full" />
          <Skeleton className="h-12 w-full" />
        </div>
      )}

      {/* State: error */}
      {isError && (
        <div
          role="alert"
          className="flex items-start gap-2 rounded-md border border-destructive/30 bg-destructive/10 p-3 text-sm text-destructive"
        >
          <XCircle aria-hidden="true" className="mt-0.5 size-4 shrink-0" />
          <div className="space-y-1">
            <p>Could not load snapshots.</p>
            {error instanceof Error && (
              <p className="text-xs">{error.message}</p>
            )}
            <Button
              variant="ghost"
              size="sm"
              onClick={() => void refetch()}
              className="h-auto px-0 text-xs text-destructive"
            >
              Retry
            </Button>
          </div>
        </div>
      )}

      {/* State: empty */}
      {!isPending && !isError && snapshots.length === 0 && (
        <div className="flex items-center gap-2 rounded-md border border-dashed p-6 text-sm text-muted-foreground">
          <Camera aria-hidden="true" className="size-4 shrink-0" />
          No snapshots yet. Take one before a risky change.
        </div>
      )}

      {/* State: list */}
      {!isPending && snapshots.length > 0 && (
        <div className="rounded-md border">
          <ul role="list" className="divide-y divide-border">
            {snapshots.map((snap) => (
              <SnapshotRow
                key={snap.id}
                snapshot={snap}
                canOperate={canOperate}
                isReverting={
                  revertMut.isPending && revertTarget?.id === snap.id
                }
                isDeleting={deleteMut.isPending && deleteTarget?.id === snap.id}
                onRevert={() => setRevertTarget(snap)}
                onDelete={() => setDeleteTarget(snap)}
              />
            ))}
          </ul>
        </div>
      )}

      {/* Revert confirm dialog */}
      {revertTarget !== null && (
        <RevertConfirmDialog
          snapshot={revertTarget}
          isPending={revertMut.isPending}
          errorMessage={revertMut.isError ? revertMut.error.message : null}
          onConfirm={() => void handleRevert()}
          onClose={() => setRevertTarget(null)}
        />
      )}

      {/* Delete confirm dialog */}
      {deleteTarget !== null && (
        <DeleteConfirmDialog
          snapshot={deleteTarget}
          isPending={deleteMut.isPending}
          onConfirm={() => void handleDelete(deleteTarget)}
          onClose={() => setDeleteTarget(null)}
        />
      )}
    </div>
  );
}

// ── SnapshotRow ──────────────────────────────────────────────────────────────

interface RowProps {
  snapshot: DbSnapshotEntry;
  canOperate: boolean;
  isReverting: boolean;
  isDeleting: boolean;
  onRevert: () => void;
  onDelete: () => void;
}

function SnapshotRow({
  snapshot,
  canOperate,
  isReverting,
  isDeleting,
  onRevert,
  onDelete,
}: RowProps) {
  const label = snapshot.label || snapshot.id.slice(0, 16) + "…";
  const ago = relativeTime(snapshot.created_at);
  const size = formatBytes(snapshot.size);

  return (
    <li className="flex items-center gap-3 px-4 py-3">
      <Camera aria-hidden="true" className="size-4 shrink-0 text-muted-foreground" />

      <div className="min-w-0 flex-1">
        <p
          className="truncate text-sm font-medium"
          title={snapshot.label || undefined}
        >
          {label}
        </p>
        <p className="text-xs text-muted-foreground">
          {ago} &middot; {size} &middot; {snapshot.table_count} tables
        </p>
      </div>

      {canOperate && (
        <div className="flex items-center gap-1 shrink-0">
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="gap-1.5"
            onClick={onRevert}
            disabled={isReverting || isDeleting}
            aria-label={`Revert database to snapshot: ${label}`}
          >
            {isReverting ? (
              <Loader2 aria-hidden="true" className="size-3.5 animate-spin" />
            ) : (
              <RotateCcw aria-hidden="true" className="size-3.5" />
            )}
            Revert
          </Button>
          <Button
            type="button"
            variant="ghost"
            size="icon"
            className="size-8 text-muted-foreground hover:text-destructive"
            onClick={onDelete}
            disabled={isReverting || isDeleting}
            aria-label={`Delete snapshot: ${label}`}
          >
            {isDeleting ? (
              <Loader2 aria-hidden="true" className="size-3.5 animate-spin" />
            ) : (
              <Trash2 aria-hidden="true" className="size-3.5" />
            )}
          </Button>
        </div>
      )}
    </li>
  );
}

// ── RevertConfirmDialog ──────────────────────────────────────────────────────

interface RevertDialogProps {
  snapshot: DbSnapshotEntry;
  isPending: boolean;
  errorMessage: string | null;
  onConfirm: () => void;
  onClose: () => void;
}

function RevertConfirmDialog({
  snapshot,
  isPending,
  errorMessage,
  onConfirm,
  onClose,
}: RevertDialogProps) {
  const titleId = useId();
  const descId = useId();
  // The operator must type the snapshot label (or the ID prefix if no label) to confirm.
  const requiredText = snapshot.label.trim() || "REVERT";
  const [typed, setTyped] = useState("");
  const canConfirm = typed.trim() === requiredText && !isPending;

  return (
    <Dialog open onClose={onClose}>
      <DialogContent ariaLabelledBy={titleId} ariaDescribedBy={descId}>
        <DialogHeader>
          <DialogTitle id={titleId}>Revert database?</DialogTitle>
        </DialogHeader>

        <DialogBody>
          <div
            id={descId}
            role="alert"
            className="flex gap-2 rounded-md border border-destructive/40 bg-destructive/10 p-3 text-sm text-destructive"
          >
            <AlertTriangle
              aria-hidden="true"
              className="mt-0.5 size-4 shrink-0"
            />
            <div className="space-y-1">
              <p>
                <strong>This replaces your entire database.</strong> All data
                written since this snapshot was taken will be lost.
              </p>
              <p>
                A safety snapshot of the current state will be taken first, so
                you can revert the revert if needed.
              </p>
            </div>
          </div>

          <div className="mt-4 space-y-2">
            <dl className="grid grid-cols-2 gap-x-4 gap-y-1 text-sm">
              <div>
                <dt className="text-muted-foreground">Snapshot</dt>
                <dd className="font-medium truncate" title={snapshot.label || snapshot.id}>
                  {snapshot.label || snapshot.id.slice(0, 20)}
                </dd>
              </div>
              <div>
                <dt className="text-muted-foreground">Taken</dt>
                <dd className="font-medium">{relativeTime(snapshot.created_at)}</dd>
              </div>
            </dl>
          </div>

          {errorMessage && (
            <div className="mt-3 flex items-start gap-2 rounded-md border border-destructive/30 bg-destructive/10 p-2.5 text-sm text-destructive">
              <XCircle aria-hidden="true" className="mt-0.5 size-4 shrink-0" />
              <span>{errorMessage}</span>
            </div>
          )}

          <div className="mt-4 space-y-1.5">
            <Label htmlFor="revert-confirm-input">
              Type{" "}
              <code className="rounded bg-muted px-1 font-mono text-xs">
                {requiredText}
              </code>{" "}
              to confirm
            </Label>
            <Input
              id="revert-confirm-input"
              value={typed}
              onChange={(e) => setTyped(e.target.value)}
              disabled={isPending}
              autoComplete="off"
              aria-describedby="revert-confirm-hint"
            />
            <p
              id="revert-confirm-hint"
              className={cn(
                "text-xs",
                typed && !canConfirm && !isPending
                  ? "text-destructive"
                  : "text-muted-foreground",
              )}
            >
              {typed && !canConfirm && !isPending
                ? "Text doesn't match."
                : "Case-sensitive."}
            </p>
          </div>
        </DialogBody>

        <DialogFooter>
          <Button
            type="button"
            variant="outline"
            onClick={onClose}
            disabled={isPending}
          >
            Keep current data
          </Button>
          <Button
            type="button"
            variant="destructive"
            onClick={onConfirm}
            disabled={!canConfirm}
          >
            {isPending ? (
              <>
                <Loader2 aria-hidden="true" className="mr-1.5 size-4 animate-spin" />
                Reverting…
              </>
            ) : (
              "Revert database"
            )}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

// ── DeleteConfirmDialog ──────────────────────────────────────────────────────

interface DeleteDialogProps {
  snapshot: DbSnapshotEntry;
  isPending: boolean;
  onConfirm: () => void;
  onClose: () => void;
}

function DeleteConfirmDialog({
  snapshot,
  isPending,
  onConfirm,
  onClose,
}: DeleteDialogProps) {
  const titleId = useId();
  const label = snapshot.label || snapshot.id.slice(0, 20);

  return (
    <Dialog open onClose={onClose}>
      <DialogContent ariaLabelledBy={titleId}>
        <DialogHeader>
          <DialogTitle id={titleId}>Delete snapshot?</DialogTitle>
        </DialogHeader>
        <DialogBody>
          <p className="text-sm text-muted-foreground">
            The snapshot{" "}
            <strong className="text-foreground">{label}</strong>{" "}
            will be permanently removed from the server. This cannot be undone.
          </p>
        </DialogBody>
        <DialogFooter>
          <Button type="button" variant="outline" onClick={onClose} disabled={isPending}>
            Keep snapshot
          </Button>
          <Button
            type="button"
            variant="destructive"
            onClick={onConfirm}
            disabled={isPending}
          >
            {isPending ? (
              <>
                <Loader2 aria-hidden="true" className="mr-1.5 size-4 animate-spin" />
                Deleting…
              </>
            ) : (
              "Delete snapshot"
            )}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

// ── Helpers ──────────────────────────────────────────────────────────────────

function relativeTime(unixSeconds: number): string {
  if (!unixSeconds) return "unknown";
  const diffMs = Date.now() - unixSeconds * 1000;
  const diffMin = Math.floor(diffMs / 60_000);
  if (diffMin < 1) return "just now";
  if (diffMin < 60) return `${diffMin}m ago`;
  const diffHr = Math.floor(diffMin / 60);
  if (diffHr < 24) return `${diffHr}h ago`;
  const diffDays = Math.floor(diffHr / 24);
  if (diffDays < 7) return `${diffDays}d ago`;
  return new Date(unixSeconds * 1000).toLocaleDateString(undefined, {
    month: "short",
    day: "numeric",
  });
}

function formatBytes(bytes: number): string {
  if (!bytes) return "0 B";
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  if (bytes < 1024 * 1024 * 1024) return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
  return `${(bytes / (1024 * 1024 * 1024)).toFixed(1)} GB`;
}
