import { CheckCircle2, Clock, HardDrive, Images } from "lucide-react";
import type { LucideIcon } from "lucide-react";

import { cn, formatBytes } from "@/lib/utils";

import type { MediaSummary } from "./types";

// MediaOverview — the 4 dashboard tiles: Total / Optimized / Pending / Bytes
// saved. Sourced from the listAssets summary rollup (summaryDTO). Every numeric
// is tabular-nums so the tiles read as a stable, scannable row. Borders over
// shadows per DESIGN; tiles are a single bordered strip, not nested cards.

interface Tile {
  key: keyof MediaSummary | "bytes";
  label: string;
  icon: LucideIcon;
  value: string;
  iconClass: string;
}

export interface MediaOverviewProps {
  summary: MediaSummary;
  className?: string;
}

export function MediaOverview({ summary, className }: MediaOverviewProps) {
  const tiles: Tile[] = [
    {
      key: "total",
      label: "Total assets",
      icon: Images,
      value: summary.total.toLocaleString(),
      iconClass: "text-[var(--color-muted-foreground)]",
    },
    {
      key: "optimized",
      label: "Optimized",
      icon: CheckCircle2,
      value: summary.optimized.toLocaleString(),
      iconClass: "text-[var(--color-success)]",
    },
    {
      key: "pending",
      label: "Pending",
      icon: Clock,
      value: summary.pending.toLocaleString(),
      iconClass: "text-[var(--color-info)]",
    },
    {
      key: "bytes",
      label: "Bytes saved",
      icon: HardDrive,
      value: formatBytes(summary.bytes_saved),
      iconClass: "text-[var(--color-primary)]",
    },
  ];

  return (
    <dl
      aria-label="Media optimization summary"
      className={cn(
        "grid grid-cols-2 gap-px overflow-hidden rounded-lg border border-[var(--color-border)] bg-[var(--color-border)] sm:grid-cols-4",
        className,
      )}
    >
      {tiles.map((t) => {
        const Icon = t.icon;
        return (
          <div
            key={t.key}
            className="flex flex-col gap-1 bg-[var(--color-card)] p-4"
          >
            <dt className="flex items-center gap-1.5 text-xs font-medium uppercase tracking-[0.02em] text-[var(--color-muted-foreground)]">
              <Icon aria-hidden="true" className={cn("size-3.5", t.iconClass)} />
              {t.label}
            </dt>
            <dd className="text-2xl font-semibold tabular-nums text-[var(--color-foreground)]">
              {t.value}
            </dd>
          </div>
        );
      })}
    </dl>
  );
}
