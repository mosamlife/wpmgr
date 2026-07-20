import { useMemo, useState } from "react";
import { Download, Loader2, Search, Paperclip } from "lucide-react";
import { Link } from "@tanstack/react-router";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Select } from "@/components/ui/select";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Badge } from "@/components/ui/badge";
import { Skeleton } from "@/components/ui/skeleton";
import { PageError } from "@/components/feedback";
import { relativeTime } from "@/lib/utils";
import { useSites } from "@/features/sites/use-sites";
import type { SiteEmailLogEntry } from "@wpmgr/api";
import {
  useFleetEmailLog,
  type FleetEmailLogFilters,
} from "./use-email";
import { EmailStatusBadge } from "./email-status-badge";

// ---------------------------------------------------------------------------
// Fleet email log table (cross-site, org-scope)
//
// Mirrors the per-site log table but adds a "Site" column and links each
// site_id to the per-site Email tab for drill-in.
// ---------------------------------------------------------------------------

export function FleetEmailLogTable() {
  const [filters, setFilters] = useState<FleetEmailLogFilters>({
    status: "",
    q: "",
    from: "",
    to: "",
  });

  const {
    entries,
    fetchNextPage,
    hasNextPage,
    isFetchingNextPage,
    isPending,
    isError,
    error,
    refetch,
  } = useFleetEmailLog(filters);

  // Bug #251: the log entries only carry `site_id`; resolve it to a name via
  // the shared sites list (same cache the Sites list and capability dots
  // read, so this is no extra network cost). Loading and archived/removed
  // sites simply miss the map, so FleetEmailRow falls back to the truncated
  // id.
  const { data: sites } = useSites();
  const siteNameById = useMemo(() => {
    const map = new Map<string, string>();
    for (const s of sites ?? []) {
      map.set(s.id, s.name || s.url);
    }
    return map;
  }, [sites]);

  function setFilter<K extends keyof FleetEmailLogFilters>(
    key: K,
    value: FleetEmailLogFilters[K],
  ) {
    setFilters((prev) => ({ ...prev, [key]: value }));
  }

  if (isPending) {
    return <FleetLogSkeleton />;
  }

  if (isError) {
    return (
      <PageError
        what="Could not load fleet email log."
        why={error?.message}
        onRetry={() => void refetch()}
        retryLabel="Reload log"
      />
    );
  }

  return (
    <>
      {/* Filter bar */}
      <div className="mb-4 flex flex-wrap items-center gap-2">
        <div className="relative flex-1 min-w-[180px]">
          <Search
            aria-hidden="true"
            className="absolute left-2.5 top-1/2 size-4 -translate-y-1/2 text-[var(--color-muted-foreground)]"
          />
          <Input
            type="search"
            placeholder="Search subject, from, to…"
            value={filters.q ?? ""}
            onChange={(e) => setFilter("q", e.target.value)}
            className="pl-8"
            aria-label="Search email log"
          />
        </div>
        <div className="w-36">
          <Select
            value={filters.status ?? ""}
            onChange={(e) =>
              setFilter("status", e.target.value as FleetEmailLogFilters["status"])
            }
            aria-label="Filter by status"
          >
            <option value="">All statuses</option>
            <option value="sent">Sent</option>
            <option value="failed">Failed</option>
          </Select>
        </div>
        <Input
          type="date"
          value={filters.from ?? ""}
          onChange={(e) => setFilter("from", e.target.value)}
          aria-label="From date"
          className="w-36"
        />
        <Input
          type="date"
          value={filters.to ?? ""}
          onChange={(e) => setFilter("to", e.target.value)}
          aria-label="To date"
          className="w-36"
        />
        <a
          href={buildFleetExportUrl(filters, "csv")}
          download
          className="ml-auto inline-flex items-center gap-1.5 rounded-md border border-[var(--color-border)] bg-[var(--color-background)] px-3 py-1.5 text-sm font-medium text-[var(--color-foreground)] hover:bg-[var(--color-accent)] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)]"
          aria-label="Export fleet log as CSV"
        >
          <Download aria-hidden="true" className="size-3.5" />
          Export CSV
        </a>
      </div>

      {entries.length === 0 ? (
        <div className="rounded-lg border border-[var(--color-border)] px-5 py-10 text-center">
          <p className="text-sm text-[var(--color-muted-foreground)]">
            No emails match these filters.
          </p>
        </div>
      ) : (
        <div className="rounded-md border border-[var(--color-border)]">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Date</TableHead>
                <TableHead>Site</TableHead>
                <TableHead>To</TableHead>
                <TableHead>Subject</TableHead>
                <TableHead>Provider</TableHead>
                <TableHead>Status</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {entries.map((entry) => (
                <FleetEmailRow
                  key={entry.id}
                  entry={entry}
                  siteName={siteNameById.get(entry.site_id)}
                />
              ))}
            </TableBody>
          </Table>
        </div>
      )}

      {hasNextPage ? (
        <div className="mt-4 flex justify-center">
          <Button
            type="button"
            variant="outline"
            size="sm"
            disabled={isFetchingNextPage}
            onClick={() => void fetchNextPage()}
            className="gap-2"
          >
            {isFetchingNextPage ? (
              <Loader2 aria-hidden="true" className="size-4 animate-spin" />
            ) : null}
            {isFetchingNextPage ? "Loading…" : "Load more"}
          </Button>
        </div>
      ) : null}
    </>
  );
}

// ---------------------------------------------------------------------------
// Row with site drill-in link
// ---------------------------------------------------------------------------

function FleetEmailRow({
  entry,
  siteName,
}: {
  entry: SiteEmailLogEntry;
  /** Resolved from the tenant's sites list; undefined while loading or when
   *  the site is archived/removed and no longer in the list. Either way we
   *  fall back to the truncated id so the row never breaks. */
  siteName?: string;
}) {
  const toLabel = entry.to_addresses.slice(0, 2).join(", ");
  const hasMore = entry.to_addresses.length > 2;

  return (
    <TableRow>
      <TableCell className="whitespace-nowrap text-sm text-[var(--color-muted-foreground)]">
        {relativeTime(entry.created_at)}
      </TableCell>
      <TableCell>
        {/* Link to the per-site Email > Log tab */}
        <Link
          to="/sites/$siteId/email"
          params={{ siteId: entry.site_id }}
          className="block max-w-[160px] truncate text-sm text-[var(--color-primary)] hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)]"
        >
          {siteName || `${entry.site_id.slice(0, 8)}…`}
        </Link>
      </TableCell>
      <TableCell className="max-w-[140px] truncate text-sm">
        {toLabel}
        {hasMore ? (
          <Badge variant="muted" className="ml-1">
            +{entry.to_addresses.length - 2}
          </Badge>
        ) : null}
      </TableCell>
      <TableCell className="max-w-[200px] text-sm">
        <div className="flex items-center gap-1.5">
          <span className="truncate">
            {entry.subject || (
              <span className="italic text-[var(--color-muted-foreground)]">
                (no subject)
              </span>
            )}
          </span>
          {(entry.attachment_count ?? 0) > 0 ? (
            <span
              className="inline-flex shrink-0 items-center gap-0.5 rounded bg-[var(--color-muted)] px-1 py-0.5 text-xs text-[var(--color-muted-foreground)]"
              aria-label={`${entry.attachment_count} attachment${(entry.attachment_count ?? 0) !== 1 ? "s" : ""}`}
            >
              <Paperclip aria-hidden="true" className="size-2.5" />
              {entry.attachment_count}
            </span>
          ) : null}
        </div>
      </TableCell>
      <TableCell>
        <div className="flex flex-col gap-0.5">
          <Badge variant="outline" className="text-xs w-fit">
            {entry.provider}
          </Badge>
          {entry.connection_key ? (
            <code className="text-xs text-[var(--color-muted-foreground)]">
              via {entry.connection_key}
            </code>
          ) : null}
        </div>
      </TableCell>
      <TableCell>
        <EmailStatusBadge status={entry.status} />
      </TableCell>
    </TableRow>
  );
}

// ---------------------------------------------------------------------------
// Export URL helper (fleet log export — same route, no siteId path segment)
// ---------------------------------------------------------------------------

function buildFleetExportUrl(
  filters: FleetEmailLogFilters,
  format: "csv" | "json",
): string {
  const params = new URLSearchParams();
  params.set("format", format);
  if (filters.status) params.set("status", filters.status);
  if (filters.from) params.set("from", filters.from);
  if (filters.to) params.set("to", filters.to);
  if (filters.q) params.set("q", filters.q);
  // Note: fleet log export endpoint would be /api/v1/email/log/export — same
  // path shape. The CP must support this endpoint for fleet export.
  return `/api/v1/email/log/export?${params.toString()}`;
}

// ---------------------------------------------------------------------------
// Skeleton
// ---------------------------------------------------------------------------

function FleetLogSkeleton() {
  return (
    <div className="space-y-2">
      <div className="flex gap-2">
        <Skeleton className="h-9 flex-1" />
        <Skeleton className="h-9 w-32" />
      </div>
      {Array.from({ length: 8 }).map((_, i) => (
        <Skeleton key={i} className="h-10 w-full" />
      ))}
    </div>
  );
}
