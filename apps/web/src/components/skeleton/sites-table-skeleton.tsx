import type { CSSProperties } from "react";

import { Skeleton } from "@/components/ui/skeleton";
import {
  SITES_COLUMN_TRACKS,
  SITES_TABLE_MIN_WIDTH_PX,
} from "@/features/sites/sites-table-geometry";
import { cn } from "@/lib/utils";

// Surface 4.13 — Sites table skeleton.
//
// Mirrors the column geometry of features/sites/sites-table.tsx so the
// first 500ms of page load reads as "your sites are arriving" rather than
// "the page is broken". The column widths, header strip, and per-row
// element shapes track the real surface 1:1 — only the content is replaced
// with muted blocks.
//
// Row height respects density:
//   Comfortable 56px → h-14
//   Compact     44px → h-11
//   Dense       36px → h-9
//
// Animation: each block pulses via the `.wpmgr-skeleton-pulse` utility
// (opacity 0.4 → 0.7 → 0.4, 1.4s linear). When the real table mounts the
// route owner crossfades over 500ms via `useCrossfade`.

export type SitesTableSkeletonDensity = "comfortable" | "compact" | "dense";

export interface SitesTableSkeletonProps {
  /** Number of skeleton rows to render. Defaults to 8. */
  rowCount?: number;
  /** Row height mode. Mirrors the real table's density. Defaults to compact. */
  density?: SitesTableSkeletonDensity;
  className?: string;
}

// Column geometry is READ from the real table's track table rather than
// duplicated here (GH #255 / GH #261). The hand-copied list this replaced had
// silently drifted: it was missing the Client and Agent columns entirely and
// every width was stale, so the crossfade from skeleton to table jumped every
// column sideways.
const COLUMNS = SITES_COLUMN_TRACKS.map((t) => ({ id: t.id, width: t.base }));

const HEADER_HEIGHT_CLASS = "h-11";

function rowHeightClass(density: SitesTableSkeletonDensity): string {
  switch (density) {
    case "comfortable":
      return "h-14";
    case "compact":
      return "h-11";
    case "dense":
      return "h-9";
  }
}

function colStyle(width: number): CSSProperties {
  return { width, minWidth: width };
}

export function SitesTableSkeleton({
  rowCount = 8,
  density = "compact",
  className,
}: SitesTableSkeletonProps) {
  const rowClass = rowHeightClass(density);
  const rows = Array.from({ length: rowCount }, (_, i) => i);

  return (
    <div
      role="status"
      aria-label="Loading sites"
      aria-busy="true"
      className={cn(
        "flex w-full flex-col overflow-x-auto bg-background",
        className,
      )}
    >
      <span className="sr-only">Loading sites…</span>

      <table
        className="w-full border-collapse"
        // Same floor as the real table: narrow viewports scroll rather than
        // crushing the columns, so the crossfade does not reflow.
        style={{ tableLayout: "fixed", minWidth: SITES_TABLE_MIN_WIDTH_PX }}
      >
        <thead className="sticky top-0 z-10 bg-background">
          <tr className={cn(HEADER_HEIGHT_CLASS, "border-b border-border")}>
            {COLUMNS.map((col, idx) => {
              const isFirst = idx === 0;
              const isLast = idx === COLUMNS.length - 1;
              return (
                <th
                  key={col.id}
                  scope="col"
                  style={colStyle(col.width)}
                  className={cn(
                    "px-3 text-left align-middle",
                    isFirst && "pl-4 pr-2",
                    isLast && "pr-4",
                  )}
                >
                  {col.id === "select" ? (
                    <Skeleton className="size-4 rounded" />
                  ) : col.id === "actions" ? null : (
                    <Skeleton className="h-3 w-16" />
                  )}
                </th>
              );
            })}
            {/* Trailing spacer track, mirroring the real table so an
                ultrawide viewport does not stretch the columns. */}
            <td aria-hidden="true" className="p-0" />
          </tr>
        </thead>
        <tbody>
          {rows.map((i) => (
            <tr key={i} className={cn(rowClass, "border-b border-border")}>
              {COLUMNS.map((col, idx) => {
                const isFirst = idx === 0;
                const isLast = idx === COLUMNS.length - 1;
                return (
                  <td
                    key={col.id}
                    style={colStyle(col.width)}
                    className={cn(
                      "px-3 align-middle",
                      isFirst && "pl-4 pr-2",
                      isLast && "pr-4 text-right",
                    )}
                  >
                    <CellSkeleton column={col.id} />
                  </td>
                );
              })}
              <td aria-hidden="true" className="p-0" />
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

// Column ids are the real table's track ids (see sites-table-geometry.ts).
function CellSkeleton({ column }: { column: string }) {
  switch (column) {
    case "select":
      return <Skeleton className="size-4 rounded" />;
    case "url":
      // Mimics the hostname + status chip pair stacked in the real cell.
      return (
        <div className="flex min-w-0 flex-col gap-1">
          <Skeleton className="h-3 w-32" />
          <Skeleton className="h-2 w-20" />
        </div>
      );
    case "client":
      return <Skeleton className="h-3 w-16" />;
    case "tags":
      // Single chip-shaped block (real cell may show up to 3 + overflow).
      return <Skeleton className="h-5 w-12 rounded-md" />;
    case "wp_version":
    case "php_version":
      // Mono version placeholder.
      return <Skeleton className="h-3 w-10" />;
    case "agent_version":
      // Status icon + mono version.
      return <Skeleton className="h-3 w-14" />;
    case "updates_count":
      return <Skeleton className="h-5 w-20 rounded-md" />;
    case "backup_status":
      return <Skeleton className="h-5 w-16 rounded-md" />;
    case "uptime_sparkline":
      // Sparkline placeholder — keep it short, the real chart is small too.
      return <Skeleton className="h-3 w-12" />;
    case "actions":
      // Two action icon buttons (Log in + More) right-aligned.
      return (
        <div className="flex items-center justify-end gap-1">
          <Skeleton className="size-7 rounded" />
          <Skeleton className="size-7 rounded" />
        </div>
      );
    default:
      return null;
  }
}
