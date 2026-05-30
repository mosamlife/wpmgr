import { useState } from "react";

import { Skeleton } from "@/components/ui/skeleton";
import { formatBytes } from "@/lib/utils";
import type { SqlInspection } from "@wpmgr/api";

import { useSqlInspection } from "./use-sql-inspection";

// "Snapshot contents" preview card inside the restore dialog.
//
// Reads the SQL inspection report for the snapshot (agent-generated when
// available, CP-legacy fallback otherwise) and renders a short, scannable
// summary the operator can sanity-check before pulling the trigger:
//   - Site URL / Home URL / Table prefix / Charset (mono)
//   - Top 10 tables by bytes_estimate with an expandable "+ N more" tail
//   - Calm chips for not-WordPress and parser_warnings
//
// The card never blocks restore. If the inspection report is unavailable
// (legacy snapshot, no agent JSON, CP fallback unwired), it shows a muted
// note and gets out of the way — the operator can still type the host into
// destructive-confirm and ship the restore.

const TOP_TABLES = 10;
const STILL_ANALYZING_THRESHOLD_MS = 30_000;

interface SqlInspectionCardProps {
  snapshotId: string;
  /** Controls polling. The dialog passes `false` for "Files only" mode so
   *  we don't burn CP cycles parsing a dump the operator chose to skip. */
  enabled?: boolean;
  /** When false, render bare (no bordered surface) so the card can sit inside
   *  a parent Card without nesting cards (ADR-037 Batch 2 snapshot detail). */
  bordered?: boolean;
}

export function SqlInspectionCard({
  snapshotId,
  enabled = true,
  bordered = true,
}: SqlInspectionCardProps) {
  const query = useSqlInspection(snapshotId, enabled);

  if (!enabled) return null;

  return (
    <section
      aria-label="Snapshot contents"
      className={
        bordered
          ? "rounded-lg border border-[var(--color-border)] bg-[var(--color-card)] p-4"
          : ""
      }
    >
      <Header query={query} />

      {query.data?.state.phase === "ready" ? (
        <ReadyBody report={query.data.state.report} />
      ) : query.data?.state.phase === "analyzing" ? (
        <AnalyzingBody elapsedMs={query.data.state.elapsedMs} />
      ) : query.data?.state.phase === "unwired" ? (
        <UnwiredBody />
      ) : query.data?.state.phase === "error" ? (
        <ErrorBody message={query.data.state.message} />
      ) : (
        <LoadingBody />
      )}
    </section>
  );
}

function Header({
  query,
}: {
  query: ReturnType<typeof useSqlInspection>;
}) {
  const state = query.data?.state;

  let subtitle: string | null = null;
  if (state?.phase === "ready") {
    const r = state.report;
    const flavor = r.is_wordpress ? "WordPress" : "Non-WordPress DB";
    const tableCount = r.tables.length;
    const totalBytes = r.tables.reduce(
      (sum, t) => sum + (t.bytes_estimate ?? 0),
      0,
    );
    subtitle = `${flavor} · ${tableCount.toLocaleString()} table${tableCount === 1 ? "" : "s"} · ${formatBytes(totalBytes)}`;
  }

  return (
    <header className="mb-3 flex items-baseline justify-between gap-3">
      <h3 className="text-sm font-semibold text-[var(--color-foreground)]">
        Snapshot contents
      </h3>
      {subtitle ? (
        <span className="text-xs text-[var(--color-muted-foreground)] tabular-nums">
          {subtitle}
        </span>
      ) : null}
    </header>
  );
}

function ReadyBody({ report }: { report: SqlInspection }) {
  return (
    <div className="space-y-3">
      <MetadataGrid report={report} />
      <TablesList tables={report.tables} />
      <Warnings report={report} />
    </div>
  );
}

function MetadataGrid({ report }: { report: SqlInspection }) {
  const rows: Array<{ label: string; value: string | undefined }> = [
    { label: "Site URL", value: report.siteurl },
    { label: "Home URL", value: report.home },
    { label: "Table prefix", value: report.table_prefix },
    { label: "Charset", value: report.charset },
  ];

  return (
    <dl className="grid min-w-0 grid-cols-[max-content_1fr] gap-x-3 gap-y-1 text-xs">
      {rows.map((row) => (
        <Row key={row.label} label={row.label} value={row.value} />
      ))}
    </dl>
  );
}

function Row({ label, value }: { label: string; value: string | undefined }) {
  return (
    <>
      <dt className="text-[var(--color-muted-foreground)]">{label}</dt>
      <dd className="min-w-0 truncate font-mono text-[var(--color-foreground)]">
        {value && value.length > 0 ? value : "–"}
      </dd>
    </>
  );
}

function TablesList({ tables }: { tables: SqlInspection["tables"] }) {
  const [expanded, setExpanded] = useState(false);

  if (tables.length === 0) {
    return (
      <p className="text-xs text-[var(--color-muted-foreground)]">
        No tables parsed from this dump.
      </p>
    );
  }

  // Stable sort by bytes_estimate descending; ties broken by row count then
  // name so the displayed order doesn't shuffle between polls.
  const sorted = [...tables].sort((a, b) => {
    const ab = a.bytes_estimate ?? 0;
    const bb = b.bytes_estimate ?? 0;
    if (ab !== bb) return bb - ab;
    if (a.rows_estimate !== b.rows_estimate)
      return b.rows_estimate - a.rows_estimate;
    return a.name.localeCompare(b.name);
  });

  const visible = expanded ? sorted : sorted.slice(0, TOP_TABLES);
  const hiddenCount = sorted.length - visible.length;

  return (
    <div className="space-y-1 border-t border-[var(--color-border)] pt-3">
      <p className="mb-1 text-[10px] font-medium tracking-[0.02em] text-[var(--color-muted-foreground)] uppercase">
        Top tables
      </p>
      <ul className="space-y-0.5 text-xs">
        {visible.map((t) => (
          <li
            key={t.name}
            className="flex items-baseline justify-between gap-3 font-mono"
          >
            <span className="truncate text-[var(--color-foreground)]">
              {t.name}
            </span>
            <span className="shrink-0 tabular-nums text-[var(--color-muted-foreground)]">
              {t.rows_estimate.toLocaleString()} row
              {t.rows_estimate === 1 ? "" : "s"} ·{" "}
              {formatBytes(t.bytes_estimate)}
            </span>
          </li>
        ))}
      </ul>
      {hiddenCount > 0 ? (
        <button
          type="button"
          onClick={() => setExpanded(true)}
          className="mt-1 text-xs font-medium text-[var(--color-primary)] hover:underline focus-visible:ring-2 focus-visible:ring-[var(--color-ring)] focus-visible:outline-none"
        >
          Show {hiddenCount.toLocaleString()} more table
          {hiddenCount === 1 ? "" : "s"}
        </button>
      ) : expanded && sorted.length > TOP_TABLES ? (
        <button
          type="button"
          onClick={() => setExpanded(false)}
          className="mt-1 text-xs font-medium text-[var(--color-primary)] hover:underline focus-visible:ring-2 focus-visible:ring-[var(--color-ring)] focus-visible:outline-none"
        >
          Show fewer
        </button>
      ) : null}
    </div>
  );
}

function Warnings({ report }: { report: SqlInspection }) {
  const hasNonWp = !report.is_wordpress;
  const hasTruncated = report.truncated === true;
  const hasParserWarnings =
    report.parser_warnings && report.parser_warnings.length > 0;

  if (!hasNonWp && !hasTruncated && !hasParserWarnings) return null;

  return (
    <div className="space-y-2 border-t border-[var(--color-border)] pt-3">
      {hasNonWp ? (
        <p className="rounded-md border border-[var(--color-border)] bg-[var(--color-muted)]/40 px-2 py-1.5 text-xs text-[var(--color-foreground)]">
          This snapshot is not a WordPress database. Restore will proceed but
          URL rewrites will not run.
        </p>
      ) : null}
      {hasTruncated ? (
        <p className="text-xs text-[var(--color-muted-foreground)]">
          Parser hit its budget; the table list above may be incomplete.
        </p>
      ) : null}
      {hasParserWarnings
        ? report.parser_warnings!.map((w, i) => (
            <p
              key={`${i}-${w}`}
              className="text-xs text-[var(--color-muted-foreground)]"
            >
              {w}
            </p>
          ))
        : null}
    </div>
  );
}

function LoadingBody() {
  return (
    <div className="space-y-2" aria-busy="true">
      <Skeleton className="h-4 w-3/4" />
      <Skeleton className="h-4 w-2/3" />
      <Skeleton className="h-4 w-1/2" />
    </div>
  );
}

function AnalyzingBody({ elapsedMs }: { elapsedMs: number }) {
  const stillAnalyzing = elapsedMs >= STILL_ANALYZING_THRESHOLD_MS;
  return (
    <div
      className="space-y-2"
      aria-busy="true"
      aria-live="polite"
    >
      <p className="text-xs text-[var(--color-muted-foreground)]">
        {stillAnalyzing
          ? "Still analyzing – large snapshot. We'll keep checking."
          : "Analyzing snapshot…"}
      </p>
      <Skeleton className="h-4 w-3/4" />
      <Skeleton className="h-4 w-2/3" />
      <Skeleton className="h-4 w-1/2" />
    </div>
  );
}

function UnwiredBody() {
  return (
    <p className="text-xs text-[var(--color-muted-foreground)]">
      Snapshot was created by an older agent. Database preview is unavailable
      for this snapshot but the restore will work.
    </p>
  );
}

function ErrorBody({ message }: { message: string }) {
  return (
    <p
      role="alert"
      className="text-xs text-[var(--color-muted-foreground)]"
    >
      Could not load snapshot contents. {message}
    </p>
  );
}
