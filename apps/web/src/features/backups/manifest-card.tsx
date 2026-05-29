import { useMemo, useState } from "react";
import { Virtuoso } from "react-virtuoso";

import { cn, formatBytes } from "@/lib/utils";
import type { BackupManifestEntry, SqlInspection } from "@wpmgr/api";

// ManifestCard — the snapshot's contents, grouped by component (ADR-037 Batch
// 2). Replaces the old single flat Entries table. We derive two component
// groups from the manifest entries:
//
//   • Files     — every file entry, summarized then listed (virtualized > 100).
//   • Database  — every db entry (one per table), merged with the SQL
//                 inspection per-table inventory (rows / bytes / charset) when
//                 a report is available; both describe the same tables.
//
// Each group leads with a summary row ("N entries · X · C chunks") and then a
// collapsible detail list. Tokens only; mono for paths/table names;
// tabular-nums for column-bound numbers; no em-dashes; verb-first toggles.

// DESIGN.md: virtualize lists past 100 rows. Below that we render plainly so
// short manifests don't pay for a scroll container.
const VIRTUALIZE_THRESHOLD = 100;

type SqlTable = SqlInspection["tables"][number];

interface ManifestCardProps {
  entries: BackupManifestEntry[];
  /** SQL inspection tables, merged into the database list when present. */
  sqlTables?: SqlTable[];
}

export function ManifestCard({ entries, sqlTables }: ManifestCardProps) {
  const { files, dbTables } = useMemo(() => groupEntries(entries), [entries]);

  if (entries.length === 0) {
    return (
      <p className="text-sm text-muted-foreground">
        No manifest entries recorded for this snapshot.
      </p>
    );
  }

  return (
    <div className="space-y-4">
      <FilesGroup entries={files} />
      <DatabaseGroup entries={dbTables} sqlTables={sqlTables} />
    </div>
  );
}

interface Grouped {
  files: BackupManifestEntry[];
  dbTables: BackupManifestEntry[];
}

function groupEntries(entries: BackupManifestEntry[]): Grouped {
  const files: BackupManifestEntry[] = [];
  const dbTables: BackupManifestEntry[] = [];
  for (const e of entries) {
    if (e.entry_kind === "db") dbTables.push(e);
    else files.push(e);
  }
  return { files, dbTables };
}

function sumSize(entries: BackupManifestEntry[]): number {
  return entries.reduce((acc, e) => acc + (e.size ?? 0), 0);
}

function sumChunks(entries: BackupManifestEntry[]): number {
  return entries.reduce((acc, e) => acc + (e.chunk_count ?? 0), 0);
}

/** Group summary header — the always-visible "Files: N · X · C chunks" row. */
function GroupSummary({
  label,
  count,
  noun,
  size,
  chunks,
  open,
  onToggle,
  disabled,
}: {
  label: string;
  count: number;
  noun: string;
  size: number;
  chunks: number;
  open: boolean;
  onToggle: () => void;
  disabled: boolean;
}) {
  const facts = `${count.toLocaleString()} ${count === 1 ? noun : `${noun}s`} · ${formatBytes(size)} · ${chunks.toLocaleString()} ${chunks === 1 ? "chunk" : "chunks"}`;
  return (
    <button
      type="button"
      onClick={onToggle}
      disabled={disabled}
      aria-expanded={open}
      className={cn(
        "flex w-full items-baseline justify-between gap-3 rounded-md px-2 py-1.5 text-left text-sm transition-colors",
        disabled
          ? "cursor-default"
          : "hover:bg-muted/60 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
      )}
    >
      <span className="font-medium text-foreground">{label}</span>
      <span className="flex items-center gap-2 text-muted-foreground tabular-nums">
        {facts}
        {!disabled ? (
          <span aria-hidden="true" className="text-xs">
            {open ? "Hide" : "Show"}
          </span>
        ) : null}
      </span>
    </button>
  );
}

function FilesGroup({ entries }: { entries: BackupManifestEntry[] }) {
  const [open, setOpen] = useState(false);
  const size = useMemo(() => sumSize(entries), [entries]);
  const chunks = useMemo(() => sumChunks(entries), [entries]);
  const sorted = useMemo(
    () => [...entries].sort((a, b) => (b.size ?? 0) - (a.size ?? 0)),
    [entries],
  );

  return (
    <section aria-label="Files" className="space-y-2">
      <GroupSummary
        label="Files"
        count={entries.length}
        noun="entry"
        size={size}
        chunks={chunks}
        open={open}
        onToggle={() => setOpen((v) => !v)}
        disabled={entries.length === 0}
      />
      {open && entries.length > 0 ? (
        sorted.length > VIRTUALIZE_THRESHOLD ? (
          <Virtuoso
            className="h-80 rounded-md border border-border"
            data={sorted}
            itemContent={(_index, entry) => <FileRow entry={entry} />}
          />
        ) : (
          <ul className="divide-y divide-border rounded-md border border-border">
            {sorted.map((entry) => (
              <li key={`${entry.entry_kind}:${entry.path}`}>
                <FileRow entry={entry} />
              </li>
            ))}
          </ul>
        )
      ) : null}
    </section>
  );
}

function FileRow({ entry }: { entry: BackupManifestEntry }) {
  return (
    <div
      data-testid="manifest-entry-row"
      className="flex items-baseline justify-between gap-3 px-3 py-1.5 text-xs"
    >
      <span className="min-w-0 break-all font-mono text-foreground" title={entry.path}>
        {entry.path}
      </span>
      <span className="shrink-0 text-muted-foreground tabular-nums">
        {formatBytes(entry.size)} · {entry.chunk_count.toLocaleString()}{" "}
        {entry.chunk_count === 1 ? "chunk" : "chunks"}
      </span>
    </div>
  );
}

interface MergedTable {
  name: string;
  entry: BackupManifestEntry | undefined;
  sql: SqlTable | undefined;
}

function DatabaseGroup({
  entries,
  sqlTables,
}: {
  entries: BackupManifestEntry[];
  sqlTables?: SqlTable[];
}) {
  const [open, setOpen] = useState(false);
  const size = useMemo(() => sumSize(entries), [entries]);
  const chunks = useMemo(() => sumChunks(entries), [entries]);

  // Merge manifest db entries with the SQL inspection inventory by table name.
  // Both describe the same tables; the manifest knows chunk/size on disk, the
  // inspection knows rows/bytes/charset. Union the two so a table present in
  // only one source still shows.
  const merged = useMemo<MergedTable[]>(() => {
    const byName = new Map<string, MergedTable>();
    for (const e of entries) {
      const name = e.table_name ?? e.path;
      byName.set(name, { name, entry: e, sql: undefined });
    }
    for (const t of sqlTables ?? []) {
      const existing = byName.get(t.name);
      if (existing) existing.sql = t;
      else byName.set(t.name, { name: t.name, entry: undefined, sql: t });
    }
    return [...byName.values()].sort((a, b) => {
      const ab = a.sql?.bytes_estimate ?? a.entry?.size ?? 0;
      const bb = b.sql?.bytes_estimate ?? b.entry?.size ?? 0;
      if (ab !== bb) return bb - ab;
      return a.name.localeCompare(b.name);
    });
  }, [entries, sqlTables]);

  const tableCount = merged.length;

  return (
    <section aria-label="Database" className="space-y-2 border-t border-border pt-4">
      <GroupSummary
        label="Database"
        count={tableCount}
        noun="table"
        size={size}
        chunks={chunks}
        open={open}
        onToggle={() => setOpen((v) => !v)}
        disabled={tableCount === 0}
      />
      {open && tableCount > 0 ? (
        merged.length > VIRTUALIZE_THRESHOLD ? (
          <Virtuoso
            className="h-80 rounded-md border border-border"
            data={merged}
            itemContent={(_index, t) => <DbRow table={t} />}
          />
        ) : (
          <ul className="divide-y divide-border rounded-md border border-border">
            {merged.map((t) => (
              <li key={t.name}>
                <DbRow table={t} />
              </li>
            ))}
          </ul>
        )
      ) : null}
    </section>
  );
}

function DbRow({ table }: { table: MergedTable }) {
  const { entry, sql } = table;
  // Prefer SQL inspection bytes (logical), fall back to the manifest size on
  // disk. Rows + charset come only from the inspection report.
  const bytes = sql?.bytes_estimate ?? entry?.size ?? null;
  return (
    <div
      data-testid="manifest-entry-row"
      className="flex items-baseline justify-between gap-3 px-3 py-1.5 text-xs"
    >
      <span className="min-w-0 break-all font-mono text-foreground" title={table.name}>
        {table.name}
      </span>
      <span className="flex shrink-0 items-baseline gap-2 text-muted-foreground tabular-nums">
        {sql ? (
          <span>
            {sql.rows_estimate.toLocaleString()}{" "}
            {sql.rows_estimate === 1 ? "row" : "rows"}
          </span>
        ) : null}
        <span>{formatBytes(bytes)}</span>
        {sql?.charset ? (
          <span className="font-mono text-[10px]">{sql.charset}</span>
        ) : null}
        {entry ? (
          <span>
            {entry.chunk_count.toLocaleString()}{" "}
            {entry.chunk_count === 1 ? "chunk" : "chunks"}
          </span>
        ) : null}
      </span>
    </div>
  );
}
