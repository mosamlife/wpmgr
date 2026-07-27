// StatusMatrix — dense grid of small rounded-square cells, one per site.
// Fill colour = status colour. Hover tooltip shows site name + metric value.
// Click selects / drills the site. Datadog host-map idiom, squares not hexagons.
//
// Accessibility: each cell has a role="gridcell" with aria-label. Status is
// communicated by both colour AND label text (not colour alone).

import {
  TooltipProvider,
  TooltipRoot,
  TooltipTrigger,
  TooltipContent,
} from "@/components/ui/tooltip";
import { cn } from "@/lib/utils";
import { Skeleton } from "@/components/ui/skeleton";
import { statusReasonCopy } from "./uptime-status";
import type { UptimeStatusKind } from "./fleet-types";

// ---------------------------------------------------------------------------
// Status colour mapping — semantic tokens only
// ---------------------------------------------------------------------------

const STATUS_BG: Record<UptimeStatusKind, string> = {
  up: "bg-[var(--color-success)]",
  degraded: "bg-[var(--color-warning)]",
  down: "bg-[var(--color-destructive)]",
  unknown: "bg-[var(--color-muted-foreground)]/40",
};

const STATUS_LABEL: Record<UptimeStatusKind, string> = {
  up: "Up",
  degraded: "Degraded",
  down: "Down",
  unknown: "Unknown",
};

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

export interface MatrixCell {
  siteId: string;
  name: string;
  url: string;
  status: UptimeStatusKind;
  /** Optional secondary metric shown in the tooltip (e.g. "24 ms"). */
  metricLabel?: string;
  /**
   * `FleetStatusItem.status_reason` (GH #291) — only surfaced when `status`
   * is `degraded` and the code is one this UI recognises; otherwise the
   * cell renders exactly as it always has.
   */
  statusReason?: string | null;
}

export interface StatusMatrixProps {
  cells: MatrixCell[];
  selectedSiteId?: string | null;
  onSelect?: (siteId: string) => void;
  loading?: boolean;
  /** Target cell size in px. Defaults to 12. */
  cellSize?: number;
}

export function StatusMatrix({
  cells,
  selectedSiteId,
  onSelect,
  loading = false,
  cellSize = 12,
}: StatusMatrixProps) {
  if (loading) {
    return (
      <div
        role="status"
        aria-label="Loading site status matrix"
        className="flex flex-wrap gap-1"
      >
        <span className="sr-only">Loading</span>
        {Array.from({ length: 40 }).map((_, i) => (
          <Skeleton
            key={i}
            className="rounded-sm"
            style={{ width: cellSize, height: cellSize }}
          />
        ))}
      </div>
    );
  }

  if (cells.length === 0) {
    return (
      <p className="text-sm text-[var(--color-muted-foreground)]">
        No sites to display.
      </p>
    );
  }

  return (
    <TooltipProvider>
      <div
        role="grid"
        aria-label="Site status matrix"
        className="flex flex-wrap gap-1 p-1"
      >
        {cells.map((cell) => {
          const isSelected = cell.siteId === selectedSiteId;
          const note =
            cell.status === "degraded"
              ? statusReasonCopy(cell.statusReason)
              : null;
          const tooltipContent = (
            <span className="space-y-0.5">
              <span className="block font-medium">{cell.name}</span>
              <span className="block text-[10px] opacity-80">{cell.url}</span>
              <span className="block">
                {STATUS_LABEL[cell.status]}
                {cell.metricLabel ? ` · ${cell.metricLabel}` : ""}
              </span>
              {note && (
                <span className="block max-w-[220px] text-[10px] opacity-80">
                  {note}
                </span>
              )}
            </span>
          );
          return (
            <TooltipRoot key={cell.siteId}>
              <TooltipTrigger asChild>
                <button
                  type="button"
                  role="gridcell"
                  aria-label={`${cell.name}: ${STATUS_LABEL[cell.status]}${cell.metricLabel ? ", " + cell.metricLabel : ""}${note ? ". " + note : ""}`}
                  aria-pressed={isSelected}
                  onClick={() => onSelect?.(cell.siteId)}
                  style={{ width: cellSize, height: cellSize }}
                  className={cn(
                    "rounded-sm transition-all duration-150 ease-out",
                    "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)] focus-visible:ring-offset-1",
                    STATUS_BG[cell.status],
                    isSelected && "ring-2 ring-[var(--color-foreground)] ring-offset-1",
                    !isSelected && "hover:brightness-110 hover:opacity-90",
                    onSelect && "cursor-pointer",
                    !onSelect && "cursor-default",
                  )}
                />
              </TooltipTrigger>
              <TooltipContent side="top">{tooltipContent}</TooltipContent>
            </TooltipRoot>
          );
        })}
      </div>
    </TooltipProvider>
  );
}
