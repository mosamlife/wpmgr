import { useState } from "react";
import { Inbox } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { PageError } from "@/components/feedback";
import { StatusChip } from "@/components/status/status-chip";
import { toast } from "@/components/toast/use-toast-helpers";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { relativeTime } from "@/lib/utils";
import type { StatusTone } from "@/components/status/status-dot";

import { useLoginEvents, useUnblockIp } from "./use-security";

// S2 — Login events table for the Security tab.
//
// Columns: IP | Status | Category | Username | When | Actions
//
// Status numeric codes:
//   1 = failure  → warning tone
//   2 = success  → success tone
//   3 = blocked  → destructive tone
//
// "Unblock" action is only shown on status=3 (blocked) rows.
//
// Loading: skeleton rows.
// Empty: inline Inbox empty state.
// Error: PageError with retry.

const STATUS_META: Record<
  1 | 2 | 3,
  { label: string; tone: StatusTone }
> = {
  1: { label: "Failure", tone: "warning" },
  2: { label: "Success", tone: "success" },
  3: { label: "Blocked", tone: "destructive" },
};

export function LoginEventsTable({ siteId }: { siteId: string }) {
  const [statusFilter, setStatusFilter] = useState<
    "all" | "1" | "2" | "3"
  >("all");

  const parsedStatus = statusFilter === "all"
    ? undefined
    : (parseInt(statusFilter, 10) as 1 | 2 | 3);

  const { data, isPending, isError, error, refetch } = useLoginEvents(
    siteId,
    { status: parsedStatus },
  );
  const unblock = useUnblockIp(siteId);

  function handleUnblock(ip: string) {
    unblock.mutate(ip, {
      onSuccess: (result) => {
        if (result.ok) {
          toast.success(`Unblocked ${ip}.`, {
            description: result.detail || "The IP block has been removed.",
          });
        } else {
          toast.error(`Could not unblock ${ip}.`, {
            description: result.detail || "The agent rejected the unblock.",
          });
        }
      },
      onError: (err: Error) => {
        toast.error(`Unblock failed for ${ip}.`, {
          description: err.message,
        });
      },
    });
  }

  return (
    <section aria-labelledby="login-events-heading" className="space-y-4">
      {/* Toolbar */}
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h3
          id="login-events-heading"
          className="text-xs font-medium uppercase tracking-wide text-muted-foreground"
        >
          Recent login events
        </h3>

        <div className="flex items-center gap-2">
          {/* Status filter */}
          <div className="flex items-center gap-1">
            {(
              [
                { value: "all", label: "All" },
                { value: "1", label: "Failures" },
                { value: "2", label: "Successes" },
                { value: "3", label: "Blocked" },
              ] as const
            ).map((opt) => (
              <Button
                key={opt.value}
                type="button"
                size="sm"
                variant={statusFilter === opt.value ? "outline" : "ghost"}
                onClick={() => setStatusFilter(opt.value)}
                aria-pressed={statusFilter === opt.value}
              >
                {opt.label}
              </Button>
            ))}
          </div>

          <Button
            type="button"
            size="sm"
            variant="ghost"
            onClick={() => void refetch()}
          >
            Reload
          </Button>
        </div>
      </div>

      {/* Content states */}
      {isPending ? (
        <EventsTableSkeleton />
      ) : isError ? (
        <PageError
          what="Could not load login events."
          why={error instanceof Error ? error.message : "Unknown error"}
          onRetry={() => void refetch()}
          retryLabel="Reload events"
        />
      ) : (data ?? []).length === 0 ? (
        <EmptyEvents />
      ) : (
        <div className="overflow-hidden rounded-lg border border-[var(--color-border)] bg-[var(--color-card)]">
          <div className="w-full overflow-x-auto">
            <Table className="min-w-[640px]">
              <TableHeader>
                <TableRow>
                  <TableHead className="w-[160px]">IP</TableHead>
                  <TableHead className="w-[110px]">Status</TableHead>
                  <TableHead className="w-[140px]">Category</TableHead>
                  <TableHead>Username</TableHead>
                  <TableHead className="w-[140px] text-right">When</TableHead>
                  <TableHead className="w-[100px] text-right">Actions</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
              {(data ?? []).map((event) => {
                const statusMeta = STATUS_META[event.status];
                const rel = relativeTime(event.occurred_at);
                return (
                  <TableRow key={event.id}>
                    {/* IP — mono */}
                    <TableCell>
                      <span className="font-mono text-xs text-[var(--color-foreground)]">
                        {event.ip}
                      </span>
                    </TableCell>

                    {/* Status chip */}
                    <TableCell>
                      {statusMeta ? (
                        <StatusChip
                          tone={statusMeta.tone}
                          label={statusMeta.label}
                        />
                      ) : (
                        <span className="text-xs text-[var(--color-muted-foreground)]">
                          Unknown
                        </span>
                      )}
                    </TableCell>

                    {/* Category */}
                    <TableCell>
                      <span className="text-xs text-[var(--color-foreground)]">
                        {event.category}
                      </span>
                    </TableCell>

                    {/* Username */}
                    <TableCell>
                      <span className="font-mono text-xs text-[var(--color-foreground)]">
                        {event.username}
                      </span>
                    </TableCell>

                    {/* When */}
                    <TableCell className="text-right">
                      <time
                        dateTime={event.occurred_at}
                        title={new Date(event.occurred_at).toLocaleString()}
                        className="tabular-nums text-xs text-[var(--color-muted-foreground)]"
                      >
                        {rel ?? "just now"}
                      </time>
                    </TableCell>

                    {/* Actions — Unblock for blocked rows only */}
                    <TableCell className="text-right">
                      {event.status === 3 ? (
                        <Button
                          type="button"
                          variant="outline"
                          size="sm"
                          onClick={() => handleUnblock(event.ip)}
                          disabled={unblock.isPending}
                          aria-label={`Unblock IP ${event.ip}`}
                        >
                          Unblock
                        </Button>
                      ) : (
                        <span className="text-xs text-[var(--color-muted-foreground)]">
                          —
                        </span>
                      )}
                    </TableCell>
                  </TableRow>
                );
              })}
            </TableBody>
          </Table>
          </div>
        </div>
      )}
    </section>
  );
}

// ---------------------------------------------------------------------------
// Skeleton
// ---------------------------------------------------------------------------

function EventsTableSkeleton() {
  return (
    <div
      role="status"
      aria-busy="true"
      className="overflow-hidden rounded-lg border border-[var(--color-border)] bg-[var(--color-card)]"
    >
      <span className="sr-only">Loading login events</span>
      <div className="flex items-center gap-4 border-b border-[var(--color-border)] px-3 py-2.5">
        <Skeleton className="h-3 w-28" />
        <Skeleton className="h-3 w-16" />
        <Skeleton className="h-3 w-24" />
        <Skeleton className="h-3 flex-1" />
        <Skeleton className="h-3 w-20 ml-auto" />
        <Skeleton className="h-3 w-14" />
      </div>
      {Array.from({ length: 5 }).map((_, i) => (
        <div
          key={i}
          className="flex items-center gap-4 border-b border-[var(--color-border)] px-3 py-3 last:border-0"
        >
          <Skeleton className="h-3 w-32" />
          <Skeleton className="h-5 w-20 rounded" />
          <Skeleton className="h-3 w-24" />
          <Skeleton className="h-3 flex-1" />
          <Skeleton className="h-3 w-16 ml-auto" />
          <Skeleton className="h-7 w-16 rounded" />
        </div>
      ))}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Empty state
// ---------------------------------------------------------------------------

function EmptyEvents() {
  return (
    <div className="flex flex-col items-center justify-center gap-2 rounded-lg border border-[var(--color-border)] bg-[var(--color-card)] px-6 py-12 text-center">
      <Inbox
        aria-hidden="true"
        className="size-6 text-[var(--color-muted-foreground)]"
      />
      <p className="text-sm font-medium text-[var(--color-foreground)]">
        No login events yet
      </p>
      <p className="max-w-sm text-xs text-[var(--color-muted-foreground)]">
        Login events appear here as the agent ingests them. If you just enabled
        login protection, events will show up on the next login attempt.
      </p>
    </div>
  );
}
