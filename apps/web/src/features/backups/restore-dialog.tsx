import { useEffect, useRef, useState } from "react";

import { Button } from "@/components/ui/button";
import { Label } from "@/components/ui/label";
import { useCreateRestore } from "@/features/backups/use-backups";
import type { BackupManifestEntry, RestoreCreate } from "@wpmgr/api";

// Destructive restore dialog (operator+). Offers three mutually-exclusive
// modes: a full restore, a partial restore by file path, or a partial restore
// by database table. The user must explicitly acknowledge the overwrite before
// the submit button enables. On submit we POST the restore and close; the
// snapshot-detail page then polls the job to completion.

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
  snapshotId,
  entries,
}: {
  open: boolean;
  onClose: () => void;
  snapshotId: string;
  entries: BackupManifestEntry[];
}) {
  const dialogRef = useRef<HTMLDialogElement>(null);

  useEffect(() => {
    const el = dialogRef.current;
    if (!el) return;
    if (open && !el.open) el.showModal();
    if (!open && el.open) el.close();
  }, [open]);

  return (
    <dialog
      ref={dialogRef}
      onClose={onClose}
      aria-labelledby="restore-title"
      className="m-auto w-full max-w-lg rounded-xl border border-[var(--color-border)] bg-[var(--color-background)] p-6 text-[var(--color-foreground)] backdrop:bg-black/50"
    >
      {open ? (
        <RestoreForm
          snapshotId={snapshotId}
          entries={entries}
          onClose={onClose}
        />
      ) : null}
    </dialog>
  );
}

function RestoreForm({
  snapshotId,
  entries,
  onClose,
}: {
  snapshotId: string;
  entries: BackupManifestEntry[];
  onClose: () => void;
}) {
  const restore = useCreateRestore(snapshotId);
  const [mode, setMode] = useState<Mode>("full");
  const [paths, setPaths] = useState("");
  const [tables, setTables] = useState("");
  const [acknowledged, setAcknowledged] = useState(false);

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
    acknowledged &&
    (mode === "full" ||
      (mode === "paths" && selectedPaths.length > 0) ||
      (mode === "tables" && selectedTables.length > 0));

  async function onSubmit(e: React.FormEvent) {
    e.preventDefault();
    if (!valid) return;

    const body: RestoreCreate =
      mode === "full"
        ? { full: true }
        : mode === "paths"
          ? { paths: selectedPaths }
          : { db_tables: selectedTables };

    await restore.mutateAsync(body, { onError: () => {} });
    onClose();
  }

  return (
    <form onSubmit={(e) => void onSubmit(e)} noValidate className="space-y-5">
      <div className="space-y-1">
        <h2 id="restore-title" className="text-lg font-semibold">
          Restore from snapshot
        </h2>
        <p
          role="alert"
          className="rounded-md border border-[var(--color-destructive)]/40 bg-[var(--color-destructive)]/10 p-2 text-sm text-[var(--color-destructive)]"
        >
          This is destructive: restoring overwrites the site&apos;s files and/or
          database with the contents of this snapshot. There is no undo.
        </p>
      </div>

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
              One table per line (or comma-separated). {selectedTables.length}{" "}
              selected.
            </p>
          </div>
        ) : null}
      </fieldset>

      <label className="flex items-center gap-2 text-sm">
        <input
          type="checkbox"
          checked={acknowledged}
          onChange={(e) => setAcknowledged(e.target.checked)}
          className="size-4 accent-[var(--color-primary)]"
        />
        I understand this overwrites the live site and cannot be undone.
      </label>

      {restore.isError ? (
        <p role="alert" className="text-sm text-[var(--color-destructive)]">
          {restore.error.message}
        </p>
      ) : null}

      <div className="flex justify-end gap-2">
        <Button
          type="button"
          variant="outline"
          onClick={onClose}
          disabled={restore.isPending}
        >
          Cancel
        </Button>
        <Button
          type="submit"
          variant="destructive"
          disabled={!valid || restore.isPending}
        >
          {restore.isPending ? "Starting restore…" : "Restore"}
        </Button>
      </div>
    </form>
  );
}
