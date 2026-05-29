import type { CSSProperties } from "react";

import { Skeleton } from "@/components/ui/skeleton";
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

// Keep these column widths in lockstep with sites-table.tsx (COL_*_PX). If
// the real table's column geometry shifts, this constant moves with it.
const COLUMNS = [
  { id: "select", width: 40 },
  { id: "url", width: 320 },
  { id: "tags", width: 160 },
  { id: "wp", width: 90 },
  { id: "php", width: 90 },
  { id: "updates", width: 130 },
  { id: "backup", width: 180 },
  { id: "uptime", width: 80 },
  { id: "actions", width: 80 },
] as const;

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
      className={cn("flex w-full flex-col bg-background", className)}
    >
      <span className="sr-only">Loading sites…</span>

      <table
        className="w-full border-collapse"
        style={{ tableLayout: "fixed" }}
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
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function CellSkeleton({ column }: { column: (typeof COLUMNS)[number]["id"] }) {
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
    case "tags":
      // Single chip-shaped block (real cell may show up to 3 + overflow).
      return <Skeleton className="h-5 w-12 rounded-md" />;
    case "wp":
    case "php":
      // Mono version placeholder.
      return <Skeleton className="h-3 w-10" />;
    case "updates":
      return <Skeleton className="h-5 w-20 rounded-md" />;
    case "backup":
      return <Skeleton className="h-5 w-28 rounded-md" />;
    case "uptime":
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
  }
}
