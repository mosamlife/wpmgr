import { useState } from "react";

import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogBody,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Label } from "@/components/ui/label";
import { DestructiveConfirm } from "@/components/dialogs/destructive-confirm";
import { useCreateRestore } from "@/features/backups/use-backups";
import type { BackupManifestEntry, RestoreCreate } from "@wpmgr/api";

// Destructive restore dialog (operator+). Two-step flow:
//   1. Modal A: choose scope (full / paths / tables). This is the form the
//      operator interacts with day-to-day. Sprint 4 forms-architect owns the
//      inputs; Sprint 3 owns the chrome + the destructive-confirm step.
//   2. Modal B: <DestructiveConfirm> — operator types the site host (or
//      snapshot id if no host is available) to enable the destructive button.
// On submit we POST /restores and close both; the snapshot-detail page polls
// the job to completion via SSE.

type Mode = "full" | "paths" | "tables";

function splitList(input: string): string[] {
  return Array.from(
    new Set(
      input
        .split(/[\n,]+/)
        .map((s) => s.trim())
        .filter((s) => s.length > 0),
    ),
  );
}

export function RestoreDialog({
  open,
  onClose,
  onRequested,
  snapshotId,
  entries,
  siteHost,
  snapshotTakenAt,
}: {
  open: boolean;
  onClose: () => void;
  /** Fired after the restore POST resolves successfully, just before the
      dialog closes. Lets the parent page surface an immediate "Restore
      requested — waiting for agent" banner that bridges the perceptual gap
      between click and the first SSE phase event landing. */
  onRequested?: () => void;
  snapshotId: string;
  entries: BackupManifestEntry[];
  /** Hostname for the typed-confirmation step. Falls back to the snapshot
      short id (first 8 chars) when the page hasn't resolved a site host. */
  siteHost?: string;
  /** Snapshot creation timestamp, used in the destructive-confirm title. */
  snapshotTakenAt?: string;
}) {
  return (
    <Dialog open={open} onClose={onClose}>
      {open ? (
        <RestoreForm
          snapshotId={snapshotId}
          entries={entries}
          onClose={onClose}
          onRequested={onRequested}
          siteHost={siteHost}
          snapshotTakenAt={snapshotTakenAt}
        />
      ) : null}
    </Dialog>
  );
}

function RestoreForm({
  snapshotId,
  entries,
  onClose,
  onRequested,
  siteHost,
  snapshotTakenAt,
}: {
  snapshotId: string;
  entries: BackupManifestEntry[];
  onClose: () => void;
  onRequested?: () => void;
  siteHost?: string;
  snapshotTakenAt?: string;
}) {
  const restore = useCreateRestore(snapshotId);
  const [mode, setMode] = useState<Mode>("full");
  const [paths, setPaths] = useState("");
  const [tables, setTables] = useState("");
  const [confirmOpen, setConfirmOpen] = useState(false);

  const knownTables = Array.from(
    new Set(
      entries
        .filter((e) => e.entry_kind === "db" && e.table_name)
        .map((e) => e.table_name as string),
    ),
  ).sort();

  const selectedPaths = mode === "paths" ? splitList(paths) : [];
  const selectedTables = mode === "tables" ? splitList(tables) : [];

  const valid =
    mode === "full" ||
    (mode === "paths" && selectedPaths.length > 0) ||
    (mode === "tables" && selectedTables.length > 0);

  // The string the operator must type. Prefer a hostname when the parent
  // resolves one; otherwise the short snapshot id keeps the friction in place
  // without inventing data we don't have. TODO: snapshot-detail page should
  // pass the site host once site fetch is wired.
  const resourceName = siteHost ?? snapshotId.slice(0, 8);
  const takenAtLabel = snapshotTakenAt
    ? new Date(snapshotTakenAt).toISOString().replace("T", " ").slice(0, 16)
    : `snapshot ${snapshotId.slice(0, 8)}`;

  async function performRestore() {
    if (!valid) return;

    const body: RestoreCreate =
      mode === "full"
        ? { full: true }
        : mode === "paths"
          ? { paths: selectedPaths }
          : { db_tables: selectedTables };

    try {
      await restore.mutateAsync(body);
      onRequested?.();
      setConfirmOpen(false);
      onClose();
    } catch {
      // Error surfaces via restore.isError below; keep both dialogs open so
      // the operator can retry without losing their scope selection.
    }
  }

  return (
    <>
      <DialogContent ariaLabelledBy="restore-title">
        <DialogHeader>
          <DialogTitle id="restore-title">Restore from snapshot</DialogTitle>
          <DialogDescription>
            Pick a scope; the next step asks you to type the host to confirm.
          </DialogDescription>
        </DialogHeader>

        <DialogBody>
          <p
            role="alert"
            className="rounded-md border border-[var(--color-destructive)]/40 bg-[var(--color-destructive)]/10 p-2 text-sm text-[var(--color-destructive)]"
          >
            This is destructive: restoring overwrites the site&apos;s files
            and/or database with the contents of this snapshot. There is no
            undo.
          </p>

          <fieldset className="space-y-3">
            <legend className="text-sm font-medium">What to restore</legend>

            <label className="flex items-start gap-2 text-sm">
              <input
                type="radio"
                name="restore-mode"
                value="full"
                checked={mode === "full"}
                onChange={() => setMode("full")}
                className="mt-1 accent-[var(--color-primary)]"
              />
              <span>
                <span className="font-medium">Full restore</span>
                <span className="block text-xs text-[var(--color-muted-foreground)]">
                  Restore every file and database table in this snapshot.
                </span>
              </span>
            </label>

            <label className="flex items-start gap-2 text-sm">
              <input
                type="radio"
                name="restore-mode"
                value="paths"
                checked={mode === "paths"}
                onChange={() => setMode("paths")}
                className="mt-1 accent-[var(--color-primary)]"
              />
              <span className="font-medium">Selected files (by path)</span>
            </label>
            {mode === "paths" ? (
              <div className="space-y-1 pl-6">
                <Label htmlFor="restore-paths">File paths</Label>
                <textarea
                  id="restore-paths"
                  value={paths}
                  onChange={(e) => setPaths(e.target.value)}
                  rows={3}
                  placeholder={"wp-content/uploads/2026/05/logo.png\nwp-config.php"}
                  className="w-full rounded-md border border-[var(--color-input)] bg-transparent p-2 font-mono text-xs focus-visible:ring-2 focus-visible:ring-[var(--color-ring)] focus-visible:outline-none"
                />
                <p className="text-xs text-[var(--color-muted-foreground)]">
                  One path per line (or comma-separated). {selectedPaths.length}{" "}
                  selected.
                </p>
              </div>
            ) : null}

            <label className="flex items-start gap-2 text-sm">
              <input
                type="radio"
                name="restore-mode"
                value="tables"
                checked={mode === "tables"}
                onChange={() => setMode("tables")}
                className="mt-1 accent-[var(--color-primary)]"
              />
              <span className="font-medium">Selected database tables</span>
            </label>
            {mode === "tables" ? (
              <div className="space-y-1 pl-6">
                <Label htmlFor="restore-tables">Table names</Label>
                <textarea
                  id="restore-tables"
                  value={tables}
                  onChange={(e) => setTables(e.target.value)}
                  rows={3}
                  placeholder={"wp_posts\nwp_options"}
                  className="w-full rounded-md border border-[var(--color-input)] bg-transparent p-2 font-mono text-xs focus-visible:ring-2 focus-visible:ring-[var(--color-ring)] focus-visible:outline-none"
                />
                {knownTables.length > 0 ? (
                  <p className="text-xs text-[var(--color-muted-foreground)]">
                    In this snapshot: {knownTables.join(", ")}
                  </p>
                ) : null}
                <p className="text-xs text-[var(--color-muted-foreground)]">
                  One table per line (or comma-separated).{" "}
                  {selectedTables.length} selected.
                </p>
              </div>
            ) : null}
          </fieldset>
        </DialogBody>

        <DialogFooter className="pt-2">
          <Button
            type="button"
            variant="outline"
            onClick={onClose}
            disabled={restore.isPending}
          >
            Keep current state
          </Button>
          <Button
            type="button"
            variant="destructive"
            onClick={() => setConfirmOpen(true)}
            disabled={!valid || restore.isPending}
          >
            Restore site
          </Button>
        </DialogFooter>
      </DialogContent>

      <DestructiveConfirm
        open={confirmOpen}
        onClose={() => setConfirmOpen(false)}
        onConfirm={performRestore}
        title={`Restore ${siteHost ?? "site"} from backup taken ${takenAtLabel}`}
        consequencesBody={
          <div className="space-y-2">
            <p>
              The agent will overwrite the live site with the contents of this
              snapshot. The site will run with maintenance-mode briefly while
              files and the database are swapped. There is no undo.
            </p>
            <p className="text-[var(--color-muted-foreground)]">
              Scope:{" "}
              <strong className="text-[var(--color-foreground)]">
                {mode === "full"
                  ? "every file and database table in this snapshot"
                  : mode === "paths"
                    ? `${selectedPaths.length} file path${selectedPaths.length === 1 ? "" : "s"}`
                    : `${selectedTables.length} database table${selectedTables.length === 1 ? "" : "s"}`}
              </strong>
              .
            </p>
          </div>
        }
        resourceName={resourceName}
        confirmLabel="Restore site"
        cancelLabel="Keep current state"
        isPending={restore.isPending}
        errorMessage={restore.isError ? restore.error.message : null}
      />
    </>
  );
}
