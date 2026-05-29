import { Skeleton } from "@/components/ui/skeleton";
import { cn } from "@/lib/utils";

// Surface 4.13 — Site detail skeleton.
//
// Tracks the Sprint 3 site detail page layout:
//   1. Sticky header strip: hostname + status chip
//   2. Sub-nav: six tab labels in a row
//   3. 4-tile health grid: one card containing four tiles (no nested cards).
//      Each tile = title + value + chart placeholder.
//
// Container intentionally uses `bg-card` with one border so we stay
// borders-over-shadows and never nest cards (DESIGN.md rule).

export interface SiteDetailSkeletonProps {
  className?: string;
}

export function SiteDetailSkeleton({ className }: SiteDetailSkeletonProps) {
  return (
    <div
      role="status"
      aria-label="Loading site detail"
      aria-busy="true"
      className={cn("flex w-full flex-col gap-6 bg-background", className)}
    >
      <span className="sr-only">Loading site detail…</span>

      {/* 1. Sticky header strip — hostname + status chip */}
      <div className="sticky top-0 z-10 flex items-center gap-3 border-b border-border bg-background py-3">
        <Skeleton className="h-4 w-48" />
        <Skeleton className="h-5 w-24 rounded-md" />
      </div>

      {/* 2. Sub-nav — six tab labels */}
      <div className="flex items-center gap-6 border-b border-border pb-3">
        {Array.from({ length: 6 }, (_, i) => (
          <Skeleton key={i} className="h-3 w-16" />
        ))}
      </div>

      {/* 3. 4-tile health grid — one card, four tiles, no nested cards */}
      <div className="rounded-lg border border-border bg-card p-6">
        <div className="grid grid-cols-1 gap-6 sm:grid-cols-2 lg:grid-cols-4">
          {Array.from({ length: 4 }, (_, i) => (
            <div key={i} className="flex flex-col gap-2">
              <Skeleton className="h-3 w-16" />
              <Skeleton className="h-6 w-20" />
              <Skeleton className="h-12 w-full" />
            </div>
          ))}
        </div>
      </div>
    </div>
  );
}
