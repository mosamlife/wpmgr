import { useState } from "react";

import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
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
import { SqlInspectionCard } from "@/features/backups/sql-inspection-card";
import type { BackupManifestEntry, RestoreCreate } from "@wpmgr/api";

// Destructive restore dialog (operator+). Two-step flow:
//   1. Modal A: choose WHAT to restore (everything / database only / files
//      only), then optionally narrow by paths or tables, then optionally
//      flip advanced toggles. The snapshot-contents card sits in this modal
//      and lets the operator sanity-check the dump before they pull the
//      trigger.
//   2. Modal B: <DestructiveConfirm> — operator types the site host (or
//      snapshot id if no host is available) to enable the destructive button.
// On submit we POST /restores and close both; the snapshot-detail page polls
// the job to completion via SSE.

// The top-level mode the operator picks first. We model it as a single
// enum (rather than a checkbox pair) because the API wants ONE of three
// things: every component, db-only, or files-only. The downstream API
// `components` field uses the array `["db"]` / `["files"]` shape; we
// translate when building the submit body.
type Mode = "everything" | "db" | "files";

// Narrowing within a mode. "Everything" implies full=true; the other two
// optionally accept a path list or a table list to scope further.
type Narrow = "full" | "paths" | "tables";

// File-component kinds the operator can pick when mode === "files".
// These are the four Track 5 sub-components that wp-content decomposes
// into on the CP side. Selecting all 4 collapses to the broad "files"
// alias at submit time so the CP takes the fast path.
type FileComponent = "plugin" | "theme" | "upload" | "wp-content";

const ALL_FILE_COMPONENTS: FileComponent[] = [
  "plugin",
  "theme",
  "upload",
  "wp-content",
];

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

function fileComponentLabel(kind: FileComponent): string {
  switch (kind) {
    case "plugin":
      return "plugins";
    case "theme":
      return "themes";
    case "upload":
      return "uploads";
    case "wp-content":
      return "wp-content (others)";
  }
}

export function RestoreDialog({
  open,
  onClose,
  onRequested,
  onRestoreRunId,
  snapshotId,
  entries,
  siteHost,
  snapshotTakenAt,
  sourceSiteUrl,
  targetSiteUrl,
}: {
  open: boolean;
  onClose: () => void;
  /** Fired after the restore POST resolves successfully, just before the
      dialog closes. Lets the parent page surface an immediate "Restore
      requested — waiting for agent" banner that bridges the perceptual gap
      between click and the first SSE phase event landing. */
  onRequested?: () => void;
  /** Called with the restore_run_id from the 202 body so the caller can
      navigate to /restores/{id} for the live restore log. */
  onRestoreRunId?: (restoreRunId: string) => void;
  snapshotId: string;
  entries: BackupManifestEntry[];
  /** Hostname for the typed-confirmation step. Falls back to the snapshot
      short id (first 8 chars) when the page hasn't resolved a site host. */
  siteHost?: string;
  /** Snapshot creation timestamp, used in the destructive-confirm title. */
  snapshotTakenAt?: string;
  /**
   * P0 URL rewriter (ADR-036) — when the snapshot was taken under a
   * different site URL than the current restore target, surface a warning
   * chip before the typed-confirmation step so the operator knows the
   * agent will rewrite siteurl/home references across every DB table.
   * Both fields are optional; the warning renders only when both exist
   * and differ. Set by the snapshot-detail page from `snapshot.source_site_url`
   * (recorded at backup time) and `site.url` (the current target).
   */
  sourceSiteUrl?: string;
  targetSiteUrl?: string;
}) {
  return (
    <Dialog open={open} onClose={onClose}>
      {open ? (
        <RestoreForm
          snapshotId={snapshotId}
          entries={entries}
          onClose={onClose}
          onRequested={onRequested}
          onRestoreRunId={onRestoreRunId}
          siteHost={siteHost}
          snapshotTakenAt={snapshotTakenAt}
          sourceSiteUrl={sourceSiteUrl}
          targetSiteUrl={targetSiteUrl}
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
  onRestoreRunId,
  siteHost,
  snapshotTakenAt,
  sourceSiteUrl,
  targetSiteUrl,
}: {
  snapshotId: string;
  entries: BackupManifestEntry[];
  onClose: () => void;
  onRequested?: () => void;
  onRestoreRunId?: (restoreRunId: string) => void;
  siteHost?: string;
  snapshotTakenAt?: string;
  sourceSiteUrl?: string;
  targetSiteUrl?: string;
}) {
  const restore = useCreateRestore(snapshotId);
  const [mode, setMode] = useState<Mode>("everything");
  const [narrow, setNarrow] = useState<Narrow>("full");
  const [paths, setPaths] = useState("");
  const [tables, setTables] = useState("");
  const [keepOldFiles, setKeepOldFiles] = useState(false);
  const [confirmOpen, setConfirmOpen] = useState(false);
  // Per-component file selection (only meaningful when mode === "files").
  // Default to all four; switchMode() resets back to this whenever the
  // operator flips modes so stale state never leaks across submissions.
  const [selectedFileComponents, setSelectedFileComponents] = useState<
    FileComponent[]
  >(ALL_FILE_COMPONENTS);

  const knownTables = Array.from(
    new Set(
      entries
        .filter((e) => e.entry_kind === "db" && e.table_name)
        .map((e) => e.table_name as string),
    ),
  ).sort();

  const selectedPaths = narrow === "paths" ? splitList(paths) : [];
  const selectedTables = narrow === "tables" ? splitList(tables) : [];

  const showInspection = mode !== "files";
  const showPathsNarrowing = mode !== "db";
  const showTablesNarrowing = mode !== "files";

  // When mode === "files", the operator must keep at least one of the four
  // file components checked. We surface this as inline validation copy
  // under the sub-fieldset; here it also gates the Apply button so the
  // operator can't dispatch an empty-files restore.
  const fileComponentsValid =
    mode !== "files" || selectedFileComponents.length > 0;

  // Validity rules are the cartesian product of mode + narrow, plus the
  // file-components rule above:
  //   everything + full        · ok
  //   db        + full         · ok (everything in the db component)
  //   db        + tables       · ok iff at least one table typed
  //   files     + full         · ok iff at least one file component checked
  //   files     + paths        · ok iff at least one path typed AND at
  //                              least one file component checked
  // The narrow "paths" while mode=db, or "tables" while mode=files, are
  // hidden in the UI so they never reach this check, but we still guard.
  const valid = (() => {
    if (!fileComponentsValid) return false;
    if (narrow === "full") return true;
    if (narrow === "paths") return showPathsNarrowing && selectedPaths.length > 0;
    if (narrow === "tables")
      return showTablesNarrowing && selectedTables.length > 0;
    return false;
  })();

  // The string the operator must type. Prefer a hostname when the parent
  // resolves one; otherwise the short snapshot id keeps the friction in place
  // without inventing data we don't have. TODO: snapshot-detail page should
  // pass the site host once site fetch is wired.
  const resourceName = siteHost ?? snapshotId.slice(0, 8);

  // P0 URL rewriter (ADR-036): a cross-environment restore (snapshot taken
  // under a different siteurl than the live target) will rewrite siteurl /
  // home references in EVERY DB table. Surface that consequence before the
  // typed-confirmation gate so the operator can't accidentally do it without
  // realising. The trailing slashes are normalised so an off-by-one slash
  // doesn't trip the warning falsely.
  const normalizedSource = sourceSiteUrl?.replace(/\/+$/, "");
  const normalizedTarget = targetSiteUrl?.replace(/\/+$/, "");
  const urlsDiffer =
    !!normalizedSource &&
    !!normalizedTarget &&
    normalizedSource !== normalizedTarget;
  const takenAtLabel = snapshotTakenAt
    ? new Date(snapshotTakenAt).toISOString().replace("T", " ").slice(0, 16)
    : `snapshot ${snapshotId.slice(0, 8)}`;

  function switchMode(next: Mode) {
    setMode(next);
    // When the operator flips between modes, reset narrowing to "full" so
    // a stale path/table list doesn't follow them into a mode where it
    // doesn't apply. The body builder also discards mismatched fields, but
    // resetting state keeps the UI honest.
    setNarrow("full");
    // Same reasoning for the file-component checkboxes: every entry into
    // Files mode starts from the safe default (all four checked). Doing
    // this unconditionally also makes the off-mode state predictable.
    setSelectedFileComponents(ALL_FILE_COMPONENTS);
  }

  function toggleFileComponent(kind: FileComponent, checked: boolean) {
    setSelectedFileComponents((prev) => {
      if (checked) {
        if (prev.includes(kind)) return prev;
        // Preserve the canonical order so "all 4" comparisons stay stable.
        return ALL_FILE_COMPONENTS.filter(
          (k) => prev.includes(k) || k === kind,
        );
      }
      return prev.filter((k) => k !== kind);
    });
  }

  async function performRestore() {
    if (!valid) return;

    // Four branches:
    //   everything       → undefined (CP restores all components in snapshot)
    //   db               → ["db"]
    //   files (all 4)    → ["files"] shorthand (CP expands the alias)
    //   files (partial)  → the explicit subset of plugin/theme/upload/wp-content
    const components: RestoreCreate["components"] =
      mode === "everything"
        ? undefined
        : mode === "db"
          ? ["db"]
          : selectedFileComponents.length === ALL_FILE_COMPONENTS.length
            ? ["files"]
            : selectedFileComponents;

    const body: RestoreCreate = {
      // `full` carries the meaning "no narrowing"; the API treats it as the
      // safe default. We send it explicitly so the CP doesn't have to infer.
      ...(narrow === "full" ? { full: true } : null),
      ...(narrow === "paths" && showPathsNarrowing
        ? { paths: selectedPaths }
        : null),
      ...(narrow === "tables" && showTablesNarrowing
        ? { db_tables: selectedTables }
        : null),
      ...(components !== undefined ? { components } : null),
      ...(keepOldFiles ? { keep_old_files: true } : null),
      // P0 URL rewriter (ADR-036): forward the destination site URL so the
      // CP threads it into the agent's RestoreRequest. The CP authoritatively
      // derives the target URL from Site.URL on the destination, so this is
      // CP-derived; we just pass it for transparency.
      ...(targetSiteUrl ? { target_site_url: targetSiteUrl } : null),
    };

    try {
      const result = await restore.mutateAsync(body);
      onRequested?.();
      if (result.restore_run_id) {
        onRestoreRunId?.(result.restore_run_id);
      }
      setConfirmOpen(false);
      onClose();
    } catch {
      // Error surfaces via restore.isError below; keep both dialogs open so
      // the operator can retry without losing their scope selection.
    }
  }

  const scopeSummary = (() => {
    const modeLabel =
      mode === "everything"
        ? "database and files"
        : mode === "db"
          ? "database only"
          : "files only";
    if (narrow === "paths") {
      const n = selectedPaths.length;
      return `${n} file path${n === 1 ? "" : "s"}`;
    }
    if (narrow === "tables") {
      const n = selectedTables.length;
      return `${n} database table${n === 1 ? "" : "s"}`;
    }
    // Files mode with a partial component selection: spell out which
    // kinds so the operator confirms exactly what they picked rather
    // than the generic "every file" copy.
    if (
      mode === "files" &&
      selectedFileComponents.length > 0 &&
      selectedFileComponents.length < ALL_FILE_COMPONENTS.length
    ) {
      const labels = selectedFileComponents.map(fileComponentLabel);
      return `${labels.join(" + ")} only`;
    }
    return `every ${modeLabel === "files only" ? "file" : modeLabel === "database only" ? "database table" : "file and database table"} in this snapshot`;
  })();

  return (
    <>
      <DialogContent ariaLabelledBy="restore-title">
        <DialogHeader>
          <DialogTitle id="restore-title">Restore from snapshot</DialogTitle>
          <DialogDescription>
            Pick what to restore; the next step asks you to type the host to
            confirm.
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

          {/* Step 1 — top-level mode selector. Three radios in a single
              fieldset; the descriptions sit immediately under each label so
              the operator never has to hover for a tooltip. */}
          <fieldset className="space-y-2">
            <legend className="text-sm font-medium">What to restore</legend>

            <ModeRadio
              value="everything"
              checked={mode === "everything"}
              onChange={() => switchMode("everything")}
              title="Everything"
              description="Database and every file. (default)"
            />
            <ModeRadio
              value="db"
              checked={mode === "db"}
              onChange={() => switchMode("db")}
              title="Database only"
              description="Restore the DB; leave files untouched."
            />
            <ModeRadio
              value="files"
              checked={mode === "files"}
              onChange={() => switchMode("files")}
              title="Files only"
              description="Restore wp-content files; leave the DB live."
            />

            {/* Sub-fieldset: per-component file picker. Only rendered when
                Files mode is active. Defaults to all four checked; if the
                operator clears every box, inline copy explains why Apply is
                disabled. Sits nested under the Files radio (pl-6 aligns the
                checkboxes with the radio's label text) so the visual
                hierarchy reads as "Files only · which kinds". */}
            {mode === "files" ? (
              <fieldset className="space-y-2 pl-6">
                <legend className="sr-only">
                  Which file components to restore
                </legend>
                <FileComponentCheckbox
                  kind="plugin"
                  checked={selectedFileComponents.includes("plugin")}
                  onChange={(c) => toggleFileComponent("plugin", c)}
                  title="Plugins"
                  description="Restore plugins/ subtree."
                />
                <FileComponentCheckbox
                  kind="theme"
                  checked={selectedFileComponents.includes("theme")}
                  onChange={(c) => toggleFileComponent("theme", c)}
                  title="Themes"
                  description="Restore themes/ subtree."
                />
                <FileComponentCheckbox
                  kind="upload"
                  checked={selectedFileComponents.includes("upload")}
                  onChange={(c) => toggleFileComponent("upload", c)}
                  title="Uploads"
                  description="Restore uploads/ subtree (typically the biggest)."
                />
                <FileComponentCheckbox
                  kind="wp-content"
                  checked={selectedFileComponents.includes("wp-content")}
                  onChange={(c) => toggleFileComponent("wp-content", c)}
                  title="wp-content (others)"
                  description="Mu-plugins, languages, drop-ins, custom dirs."
                />
                {!fileComponentsValid ? (
                  <p
                    role="alert"
                    className="text-xs text-[var(--color-destructive)]"
                  >
                    Pick at least one file component to restore.
                  </p>
                ) : null}
              </fieldset>
            ) : null}
          </fieldset>

          {/* Step 2 — narrowing. Hidden behind a disclosure so the default
              path (Everything → full) stays uncluttered for the 90% case. */}
          {showPathsNarrowing || showTablesNarrowing ? (
            <details className="rounded-md border border-[var(--color-border)] bg-transparent">
              <summary className="cursor-pointer list-none px-3 py-2 text-sm font-medium text-[var(--color-foreground)] [&::-webkit-details-marker]:hidden">
                Narrow the scope (optional)
              </summary>
              <div className="space-y-3 border-t border-[var(--color-border)] px-3 py-3">
                <NarrowRadio
                  value="full"
                  checked={narrow === "full"}
                  onChange={() => setNarrow("full")}
                  label="Restore every selected component in full"
                />

                {showPathsNarrowing ? (
                  <>
                    <NarrowRadio
                      value="paths"
                      checked={narrow === "paths"}
                      onChange={() => setNarrow("paths")}
                      label="Restore only these file paths"
                    />
                    {narrow === "paths" ? (
                      <div className="space-y-1 pl-6">
                        <Label htmlFor="restore-paths">File paths</Label>
                        <textarea
                          id="restore-paths"
                          value={paths}
                          onChange={(e) => setPaths(e.target.value)}
                          rows={3}
                          placeholder={
                            "wp-content/uploads/2026/05/logo.png\nwp-config.php"
                          }
                          className="w-full rounded-md border border-[var(--color-input)] bg-transparent p-2 font-mono text-xs focus-visible:ring-2 focus-visible:ring-[var(--color-ring)] focus-visible:outline-none"
                        />
                        <p className="text-xs text-[var(--color-muted-foreground)] tabular-nums">
                          One path per line (or comma-separated).{" "}
                          {selectedPaths.length} selected.
                        </p>
                      </div>
                    ) : null}
                  </>
                ) : null}

                {showTablesNarrowing ? (
                  <>
                    <NarrowRadio
                      value="tables"
                      checked={narrow === "tables"}
                      onChange={() => setNarrow("tables")}
                      label="Restore only these database tables"
                    />
                    {narrow === "tables" ? (
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
                            In this snapshot:{" "}
                            <span className="font-mono">
                              {knownTables.join(", ")}
                            </span>
                          </p>
                        ) : null}
                        <p className="text-xs text-[var(--color-muted-foreground)] tabular-nums">
                          One table per line (or comma-separated).{" "}
                          {selectedTables.length} selected.
                        </p>
                      </div>
                    ) : null}
                  </>
                ) : null}
              </div>
            </details>
          ) : null}

          {/* Step 3 — snapshot contents preview. Hidden when mode is
              "Files only" because there's no DB to inspect. */}
          {showInspection ? (
            <SqlInspectionCard
              snapshotId={snapshotId}
              enabled={showInspection}
            />
          ) : null}

          {/* Step 4 — advanced toggles. Collapsed by default per spec. */}
          <details className="rounded-md border border-[var(--color-border)] bg-transparent">
            <summary className="cursor-pointer list-none px-3 py-2 text-sm font-medium text-[var(--color-foreground)] [&::-webkit-details-marker]:hidden">
              Advanced
            </summary>
            <div className="space-y-3 border-t border-[var(--color-border)] px-3 py-3">
              <label className="flex items-start gap-2 text-sm">
                <Checkbox
                  checked={keepOldFiles}
                  onChange={(e) => setKeepOldFiles(e.target.checked)}
                  className="mt-0.5"
                />
                <span>
                  <span className="font-medium">
                    Keep the pre-restore wp-content for 24h
                  </span>
                  <span className="block text-xs text-[var(--color-muted-foreground)]">
                    Useful if you want to manually inspect or roll back.
                  </span>
                </span>
              </label>
            </div>
          </details>
        </DialogBody>

        <DialogFooter className="pt-2">
          <Button
            type="button"
            variant="outline"
            onClick={onClose}
            disabled={restore.isPending}
          >
            Discard restore
          </Button>
          <Button
            type="button"
            variant="destructive"
            onClick={() => setConfirmOpen(true)}
            disabled={!valid || restore.isPending}
          >
            Apply restore
          </Button>
        </DialogFooter>
      </DialogContent>

      <DestructiveConfirm
        open={confirmOpen}
        onClose={() => setConfirmOpen(false)}
        onConfirm={performRestore}
        title={`Apply restore for ${siteHost ?? "site"} from backup taken ${takenAtLabel}`}
        consequencesBody={
          <div className="space-y-2">
            <p>
              The agent will overwrite the live site with the contents of this
              snapshot. The site will run in maintenance mode briefly while
              files and the database are swapped. There is no undo.
            </p>
            {/* P0 URL rewriter (ADR-036): cross-environment restore notice.
                Calm warning chip, not destructive-styled — the user is
                already past the destructive header and we don't want to
                add more red than necessary. */}
            {urlsDiffer ? (
              <p
                role="note"
                className="rounded-md border border-[var(--color-border)] bg-[var(--color-muted)]/40 p-2 text-xs text-[var(--color-foreground)]"
              >
                This restore will rewrite{" "}
                <span className="font-mono">{normalizedSource}</span>{" "}
                <span aria-hidden>→</span>{" "}
                <span className="font-mono">{normalizedTarget}</span> across
                every database table.
              </p>
            ) : null}
            <p className="text-[var(--color-muted-foreground)]">
              Scope:{" "}
              <strong className="text-[var(--color-foreground)]">
                {scopeSummary}
              </strong>
              {keepOldFiles ? (
                <>
                  {" "}
                  The agent will keep the pre-restore wp-content tree for 24
                  hours.
                </>
              ) : null}
            </p>
          </div>
        }
        resourceName={resourceName}
        confirmLabel="Apply restore"
        cancelLabel="Discard restore"
        isPending={restore.isPending}
        errorMessage={restore.isError ? restore.error.message : null}
      />
    </>
  );
}

function ModeRadio({
  value,
  checked,
  onChange,
  title,
  description,
}: {
  value: string;
  checked: boolean;
  onChange: () => void;
  title: string;
  description: string;
}) {
  return (
    <label className="flex items-start gap-2 text-sm">
      <input
        type="radio"
        name="restore-mode"
        value={value}
        checked={checked}
        onChange={onChange}
        className="mt-1 accent-[var(--color-primary)]"
      />
      <span>
        <span className="font-medium">{title}</span>
        <span className="block text-xs text-[var(--color-muted-foreground)]">
          {description}
        </span>
      </span>
    </label>
  );
}

function NarrowRadio({
  value,
  checked,
  onChange,
  label,
}: {
  value: string;
  checked: boolean;
  onChange: () => void;
  label: string;
}) {
  return (
    <label className="flex items-start gap-2 text-sm">
      <input
        type="radio"
        name="restore-narrow"
        value={value}
        checked={checked}
        onChange={onChange}
        className="mt-1 accent-[var(--color-primary)]"
      />
      <span className="font-medium">{label}</span>
    </label>
  );
}

function FileComponentCheckbox({
  kind,
  checked,
  onChange,
  title,
  description,
}: {
  kind: FileComponent;
  checked: boolean;
  onChange: (checked: boolean) => void;
  title: string;
  description: string;
}) {
  const id = `restore-file-component-${kind}`;
  return (
    <label htmlFor={id} className="flex items-start gap-2 text-sm">
      <Checkbox
        id={id}
        checked={checked}
        onChange={(e) => onChange(e.target.checked)}
        className="mt-0.5"
      />
      <span>
        <span className="font-medium">{title}</span>
        <span className="block text-xs text-[var(--color-muted-foreground)]">
          {description}
        </span>
      </span>
    </label>
  );
}
