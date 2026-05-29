import { Skeleton } from "@/components/ui/skeleton";
import { cn } from "@/lib/utils";

// Surface 4.13 — Backup snapshot list skeleton.
//
// Each row is: snapshot id (mono-ish), size, and a status chip. Renders
// four rows — typical snapshot lists are short, so eight would feel
// disproportionate while the page is loading.

export interface SnapshotListSkeletonProps {
  /** Number of skeleton rows. Defaults to 4. */
  rowCount?: number;
  className?: string;
}

export function SnapshotListSkeleton({
  rowCount = 4,
  className,
}: SnapshotListSkeletonProps) {
  return (
    <ul
      role="status"
      aria-label="Loading snapshots"
      aria-busy="true"
      className={cn("flex w-full flex-col bg-background", className)}
    >
      <span className="sr-only">Loading snapshots…</span>
      {Array.from({ length: rowCount }, (_, i) => (
        <li
          key={i}
          className="flex h-11 items-center justify-between gap-4 border-b border-border px-4"
        >
          <div className="flex items-center gap-4">
            <Skeleton className="h-3 w-32" />
            <Skeleton className="h-3 w-16" />
          </div>
          <Skeleton className="h-5 w-24 rounded-md" />
        </li>
      ))}
    </ul>
  );
}
