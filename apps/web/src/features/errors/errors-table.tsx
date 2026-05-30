import { useState } from "react";
import { Inbox } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { PageError } from "@/components/feedback";
import {
  Table,
  TableBody,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import type { PhpError } from "@wpmgr/api";

import { ErrorRow } from "./error-row";
import { ErrorDetailDrawer } from "./error-detail-drawer";
import { ErrorConfigPanel } from "./error-config-panel";
import { usePHPErrors, useSilenceError } from "./use-errors";

// The PHP-error monitor table for one site (ADR-037 Batch 4, Impeccable Restyle).
// Columns: Severity / File:line / Message / Count / Last seen / Actions
//
// Loading state:  skeleton rows matching the Activity feed pattern.
// Empty state:    inline Inbox empty (calm headline + subline), no card wrapper.
// Error state:    PageError with verb-first retry label.
// Data state:     bordered table; row click opens the detail drawer.
//
// The toolbar carries a "Show silenced" toggle (default off — silenced rows are
// hidden noise) and a Reload button. The silence mutation invalidates the whole
// errors key family so both silenced/unsilenced views refresh.

export function ErrorsTable({ siteId }: { siteId: string }) {
  const [showSilenced, setShowSilenced] = useState(false);
  const [active, setActive] = useState<PhpError | null>(null);

  const { data, isPending, isError, error, refetch } = usePHPErrors(siteId, {
    showSilenced,
  });
  const silence = useSilenceError(siteId);

  return (
    <section
      aria-labelledby="errors-heading"
      className="space-y-4 px-4 pb-8 pt-6 sm:px-6"
    >
      {/* Toolbar */}
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h2
          id="errors-heading"
          className="text-xs font-medium uppercase tracking-wide text-muted-foreground"
        >
          PHP errors
        </h2>
        <div className="flex items-center gap-2">
          <Button
            type="button"
            size="sm"
            variant={showSilenced ? "outline" : "ghost"}
            onClick={() => setShowSilenced((v) => !v)}
            aria-pressed={showSilenced}
          >
            {showSilenced ? "Hide silenced" : "Show silenced"}
          </Button>
          <Button
            type="button"
            size="sm"
            variant="ghost"
            onClick={() => void refetch()}
          >
            Reload
          </Button>
          <ErrorConfigPanel siteId={siteId} />
        </div>
      </div>

      {/* Content states */}
      {isPending ? (
        <ErrorTableSkeleton />
      ) : isError ? (
        <PageError
          what="Could not load PHP errors."
          why={error instanceof Error ? error.message : "Unknown error"}
          onRetry={() => void refetch()}
          retryLabel="Reload errors"
        />
      ) : (data ?? []).length === 0 ? (
        <EmptyErrors />
      ) : (
        <div className="overflow-hidden rounded-lg border border-border bg-card">
          <div className="w-full overflow-x-auto">
            <Table className="min-w-[700px]">
              <TableHeader>
                <TableRow>
                  <TableHead className="w-[110px]">Severity</TableHead>
                  <TableHead className="w-[240px]">File:line</TableHead>
                  <TableHead>Message</TableHead>
                  <TableHead className="w-[80px] text-right">Count</TableHead>
                  <TableHead className="w-[120px] text-right">Last seen</TableHead>
                  <TableHead className="w-[100px] text-right">Actions</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {(data ?? []).map((e) => (
                  <ErrorRow
                    key={e.id}
                    error={e}
                    onOpen={() => setActive(e)}
                    onSilence={(silenced) =>
                      silence.mutate({ md5: e.md5, silenced })
                    }
                  />
                ))}
              </TableBody>
            </Table>
          </div>
        </div>
      )}

      {/* Detail drawer — always rendered so AnimatePresence can exit cleanly */}
      <ErrorDetailDrawer
        siteId={siteId}
        error={active}
        onClose={() => setActive(null)}
        onSilence={(silenced) => {
          if (active) {
            silence.mutate({ md5: active.md5, silenced });
            setActive(null);
          }
        }}
      />
    </section>
  );
}

// ---------------------------------------------------------------------------
// Loading skeleton — matches the Activity feed table skeleton density
// ---------------------------------------------------------------------------

function ErrorTableSkeleton() {
  return (
    <div
      role="status"
      aria-busy="true"
      className="overflow-hidden rounded-lg border border-border bg-card"
    >
      <span className="sr-only">Loading errors</span>
      {/* Fake header row */}
      <div className="flex items-center gap-4 border-b border-border px-3 py-2.5">
        <Skeleton className="h-3 w-16" />
        <Skeleton className="h-3 w-28" />
        <Skeleton className="h-3 flex-1" />
        <Skeleton className="h-3 w-10 ml-auto" />
        <Skeleton className="h-3 w-16" />
        <Skeleton className="h-3 w-14" />
      </div>
      {Array.from({ length: 6 }).map((_, i) => (
        <div
          key={i}
          className="flex items-center gap-4 border-b border-border px-3 py-3 last:border-0"
        >
          {/* Severity chip skeleton */}
          <Skeleton className="h-5 w-16 rounded" />
          {/* File:line */}
          <Skeleton className="h-3 w-36" />
          {/* Message */}
          <Skeleton className="h-3 flex-1" />
          {/* Count */}
          <Skeleton className="h-3 w-8 ml-auto" />
          {/* Last seen */}
          <Skeleton className="h-3 w-14" />
          {/* Actions */}
          <Skeleton className="h-6 w-16 rounded" />
        </div>
      ))}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Inline empty state
// ---------------------------------------------------------------------------

function EmptyErrors() {
  return (
    <div className="flex flex-col items-center justify-center gap-2 rounded-lg border border-border bg-card px-6 py-12 text-center">
      <Inbox aria-hidden="true" className="size-6 text-muted-foreground" />
      <p className="text-sm font-medium text-foreground">No errors captured yet</p>
      <p className="max-w-sm text-xs text-muted-foreground">
        The agent ships up to 100 newest error fingerprints per heartbeat. Errors
        appear here as they occur.
      </p>
    </div>
  );
}
