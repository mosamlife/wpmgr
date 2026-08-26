import { useState } from "react";
import { Download, Loader2, Search, RotateCcw, Trash2, Paperclip } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Select } from "@/components/ui/select";
import { Checkbox } from "@/components/ui/checkbox";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Skeleton } from "@/components/ui/skeleton";
import { Badge } from "@/components/ui/badge";
import { PageError } from "@/components/feedback";
import {
  AlertDialog,
  AlertDialogContent,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogAction,
  AlertDialogCancel,
} from "@/components/ui/alert-dialog";
import { TooltipProvider, Tooltip } from "@/components/ui/tooltip";
import { relativeTime } from "@/lib/utils";
import type { SiteEmailLogEntry } from "@wpmgr/api";
import {
  useEmailLog,
  useBulkResendEmail,
  useBulkDeleteEmail,
  type EmailLogFilters,
} from "./use-email";
import { EmailStatusBadge } from "./email-status-badge";
import { EmailLogDetailDialog } from "./email-log-detail-dialog";

// ---------------------------------------------------------------------------
// Export helper
// ---------------------------------------------------------------------------

function buildExportUrl(
  siteId: string,
  filters: EmailLogFilters,
  format: "csv" | "json",
): string {
  const params = new URLSearchParams();
  params.set("format", format);
  if (filters.status) params.set("status", filters.status);
  if (filters.from) params.set("from", filters.from);
  if (filters.to) params.set("to", filters.to);
  if (filters.q) params.set("q", filters.q);
  return `/api/v1/sites/${siteId}/email/log/export?${params.toString()}`;
}

// ---------------------------------------------------------------------------
// Bulk delete confirm dialog
// ---------------------------------------------------------------------------

interface BulkDeleteConfirmProps {
  count: number;
  open: boolean;
  isPending: boolean;
  onConfirm: () => void;
  onCancel: () => void;
}

function BulkDeleteConfirm({
  count,
  open,
  isPending,
  onConfirm,
  onCancel,
}: BulkDeleteConfirmProps) {
  return (
    <AlertDialog open={open} onOpenChange={(o) => !o && onCancel()}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>Delete {count} log {count === 1 ? "entry" : "entries"}</AlertDialogTitle>
          <AlertDialogDescription>
            This will permanently delete {count} email log{" "}
            {count === 1 ? "entry" : "entries"}. This action cannot be undone.
          </AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel onClick={onCancel} disabled={isPending} />
          <AlertDialogAction
            variant="destructive"
            disabled={isPending}
            onClick={onConfirm}
          >
            {isPending ? (
              <Loader2 aria-hidden="true" className="size-4 animate-spin" />
            ) : null}
            Delete
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}

// ---------------------------------------------------------------------------
// Bulk action bar
// ---------------------------------------------------------------------------

interface BulkActionBarProps {
  selectedIds: Set<string>;
  entries: SiteEmailLogEntry[];
  onClearSelection: () => void;
  onBulkResend: () => void;
  onBulkDelete: () => void;
  isBulkResendPending: boolean;
  isBulkDeletePending: boolean;
}

function BulkActionBar({
  selectedIds,
  entries,
  onClearSelection,
  onBulkResend,
  onBulkDelete,
  isBulkResendPending,
  isBulkDeletePending,
}: BulkActionBarProps) {
  const count = selectedIds.size;
  if (count === 0) return null;

  // Only stored-body rows can be resent
  const storedBodyIds = new Set(
    entries.filter((e) => e.body_stored && selectedIds.has(e.id)).map((e) => e.id),
  );
  const resendableCount = storedBodyIds.size;
  const canResend = resendableCount > 0;

  return (
    <div
      role="toolbar"
      aria-label="Bulk actions"
      className="flex flex-wrap items-center gap-2 rounded-lg border border-[var(--color-primary)] bg-[var(--color-primary)]/10 px-3 py-2"
    >
      <span className="text-sm font-medium text-[var(--color-foreground)]">
        {count} selected
      </span>
      <div className="ml-auto flex items-center gap-2">
        <Tooltip
          content={
            canResend
              ? `Resend ${resendableCount} of ${count} (${count - resendableCount} without stored body skipped)`
              : "None of the selected emails have a stored body"
          }
          disabled={canResend}
        >
          <Button
            type="button"
            variant="outline"
            size="sm"
            disabled={!canResend || isBulkResendPending}
            onClick={onBulkResend}
            className="gap-1.5"
          >
            {isBulkResendPending ? (
              <Loader2 aria-hidden="true" className="size-3.5 animate-spin" />
            ) : (
              <RotateCcw aria-hidden="true" className="size-3.5" />
            )}
            Resend ({resendableCount})
          </Button>
        </Tooltip>
        <Button
          type="button"
          variant="destructive"
          size="sm"
          disabled={isBulkDeletePending}
          onClick={onBulkDelete}
          className="gap-1.5"
        >
          {isBulkDeletePending ? (
            <Loader2 aria-hidden="true" className="size-3.5 animate-spin" />
          ) : (
            <Trash2 aria-hidden="true" className="size-3.5" />
          )}
          Delete ({count})
        </Button>
        <button
          type="button"
          onClick={onClearSelection}
          className="text-sm text-[var(--color-muted-foreground)] hover:text-[var(--color-foreground)] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)] rounded px-1"
          aria-label="Clear selection"
        >
          Clear
        </button>
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Email log table (per-site)
// ---------------------------------------------------------------------------

export interface EmailLogTableProps {
  siteId: string;
  /** When true a "Site" column is added (used in the fleet view) */
  showSiteColumn?: boolean;
}

export function EmailLogTable({
  siteId,
  showSiteColumn = false,
}: EmailLogTableProps) {
  const [filters, setFilters] = useState<EmailLogFilters>({
    status: "",
    q: "",
    from: "",
    to: "",
  });
  const [selectedLogId, setSelectedLogId] = useState<string | null>(null);
  const [selectedIds, setSelectedIds] = useState<Set<string>>(new Set());
  const [bulkDeleteOpen, setBulkDeleteOpen] = useState(false);

  const { entries, fetchNextPage, hasNextPage, isFetchingNextPage, isPending, isError, error, refetch } =
    useEmailLog(siteId, filters);

  const bulkResend = useBulkResendEmail(siteId);
  const bulkDelete = useBulkDeleteEmail(siteId);

  function setFilter<K extends keyof EmailLogFilters>(
    key: K,
    value: EmailLogFilters[K],
  ) {
    setFilters((prev) => ({ ...prev, [key]: value }));
    setSelectedIds(new Set()); // clear selection on filter change
  }

  function toggleRow(id: string) {
    setSelectedIds((prev) => {
      const next = new Set(prev);
      if (next.has(id)) {
        next.delete(id);
      } else {
        next.add(id);
      }
      return next;
    });
  }

  function toggleAll() {
    if (selectedIds.size === entries.length) {
      setSelectedIds(new Set());
    } else {
      setSelectedIds(new Set(entries.map((e) => e.id)));
    }
  }

  function handleBulkResend() {
    // Only resend stored-body rows
    const ids = entries
      .filter((e) => e.body_stored && selectedIds.has(e.id))
      .map((e) => e.id);
    if (ids.length === 0) return;
    bulkResend.mutate(ids, {
      // GH #528: when any of the successful resends could not be confirmed
      // as the exact message selected, narrow the selection down to just
      // those log_ids instead of clearing it — this is the operator's way
      // to see which ones the toast is talking about (highlighted rows,
      // still available in the bulk action bar). A fully-verified batch
      // clears the selection exactly as before.
      onSuccess: (result) => {
        const unverifiedIds = new Set(
          result.results
            .filter((r) => r.ok && r.verified === false)
            .map((r) => r.log_id),
        );
        setSelectedIds(unverifiedIds);
      },
    });
  }

  function handleBulkDelete() {
    const ids = Array.from(selectedIds);
    bulkDelete.mutate(ids, {
      onSuccess: () => {
        setSelectedIds(new Set());
        setBulkDeleteOpen(false);
      },
    });
  }

  if (isPending) {
    return <EmailLogSkeleton />;
  }

  if (isError) {
    return (
      <PageError
        what="Could not load email log."
        why={error?.message}
        onRetry={() => void refetch()}
        retryLabel="Reload log"
      />
    );
  }

  const allSelected = entries.length > 0 && selectedIds.size === entries.length;
  const someSelected = selectedIds.size > 0 && selectedIds.size < entries.length;

  return (
    <TooltipProvider>
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
              setFilter("status", e.target.value as EmailLogFilters["status"])
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
        <div className="ml-auto flex gap-2">
          <a
            href={buildExportUrl(siteId, filters, "csv")}
            download
            className="inline-flex items-center gap-1.5 rounded-md border border-[var(--color-border)] bg-[var(--color-background)] px-3 py-1.5 text-sm font-medium text-[var(--color-foreground)] hover:bg-[var(--color-accent)] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)]"
            aria-label="Export as CSV"
          >
            <Download aria-hidden="true" className="size-3.5" />
            CSV
          </a>
          <a
            href={buildExportUrl(siteId, filters, "json")}
            download
            className="inline-flex items-center gap-1.5 rounded-md border border-[var(--color-border)] bg-[var(--color-background)] px-3 py-1.5 text-sm font-medium text-[var(--color-foreground)] hover:bg-[var(--color-accent)] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)]"
            aria-label="Export as JSON"
          >
            <Download aria-hidden="true" className="size-3.5" />
            JSON
          </a>
        </div>
      </div>

      {/* Bulk action bar */}
      <BulkActionBar
        selectedIds={selectedIds}
        entries={entries}
        onClearSelection={() => setSelectedIds(new Set())}
        onBulkResend={handleBulkResend}
        onBulkDelete={() => setBulkDeleteOpen(true)}
        isBulkResendPending={bulkResend.isPending}
        isBulkDeletePending={bulkDelete.isPending}
      />

      {/* Table */}
      {entries.length === 0 ? (
        <div className="rounded-lg border border-[var(--color-border)] px-5 py-10 text-center">
          <p className="text-sm text-[var(--color-muted-foreground)]">
            No emails match these filters.
          </p>
        </div>
      ) : (
        <div className="rounded-md border border-[var(--color-border)] mt-2">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead className="w-10">
                  <Checkbox
                    checked={allSelected}
                    ref={(el) => {
                      if (el) el.indeterminate = someSelected;
                    }}
                    onChange={toggleAll}
                    aria-label={allSelected ? "Deselect all" : "Select all"}
                  />
                </TableHead>
                <TableHead>Date</TableHead>
                {showSiteColumn ? <TableHead>Site</TableHead> : null}
                <TableHead>To</TableHead>
                <TableHead>Subject</TableHead>
                <TableHead>Provider</TableHead>
                <TableHead>Status</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {entries.map((entry) => (
                <EmailLogRow
                  key={entry.id}
                  entry={entry}
                  showSiteColumn={showSiteColumn}
                  selected={selectedIds.has(entry.id)}
                  onSelect={() => toggleRow(entry.id)}
                  onClick={() => setSelectedLogId(entry.id)}
                />
              ))}
            </TableBody>
          </Table>
        </div>
      )}

      {/* Load more */}
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

      {/* Detail dialog */}
      <EmailLogDetailDialog
        siteId={siteId}
        logId={selectedLogId}
        onClose={() => setSelectedLogId(null)}
        onNavigate={(id) => setSelectedLogId(id)}
      />

      {/* Bulk delete confirm */}
      <BulkDeleteConfirm
        count={selectedIds.size}
        open={bulkDeleteOpen}
        isPending={bulkDelete.isPending}
        onConfirm={handleBulkDelete}
        onCancel={() => setBulkDeleteOpen(false)}
      />
    </TooltipProvider>
  );
}

// ---------------------------------------------------------------------------
// Row
// ---------------------------------------------------------------------------

interface EmailLogRowProps {
  entry: SiteEmailLogEntry;
  showSiteColumn: boolean;
  selected: boolean;
  onSelect: () => void;
  onClick: () => void;
}

function EmailLogRow({ entry, showSiteColumn, selected, onSelect, onClick }: EmailLogRowProps) {
  const toLabel = entry.to_addresses.slice(0, 2).join(", ");
  const hasMore = entry.to_addresses.length > 2;

  return (
    <TableRow
      data-selected={selected}
      className="data-[selected=true]:bg-[var(--color-accent)]"
    >
      <TableCell onClick={(e) => e.stopPropagation()}>
        <Checkbox
          checked={selected}
          onChange={onSelect}
          aria-label={`Select email to ${toLabel}`}
        />
      </TableCell>
      <TableCell
        onClick={onClick}
        className="cursor-pointer whitespace-nowrap text-sm text-[var(--color-muted-foreground)] hover:text-[var(--color-foreground)]"
      >
        {relativeTime(entry.created_at)}
      </TableCell>
      {showSiteColumn ? (
        <TableCell
          onClick={onClick}
          className="cursor-pointer max-w-[100px] truncate text-sm"
        >
          {entry.site_id}
        </TableCell>
      ) : null}
      <TableCell
        onClick={onClick}
        className="cursor-pointer max-w-[160px] truncate text-sm"
      >
        {toLabel}
        {hasMore ? (
          <Badge variant="muted" className="ml-1">
            +{entry.to_addresses.length - 2}
          </Badge>
        ) : null}
      </TableCell>
      <TableCell
        onClick={onClick}
        className="cursor-pointer max-w-[200px] text-sm"
      >
        <div className="flex items-center gap-1.5">
          <span className="truncate">
            {entry.subject || <span className="italic text-[var(--color-muted-foreground)]">(no subject)</span>}
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
      <TableCell onClick={onClick} className="cursor-pointer">
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
      <TableCell onClick={onClick} className="cursor-pointer">
        <EmailStatusBadge status={entry.status} />
      </TableCell>
    </TableRow>
  );
}

// ---------------------------------------------------------------------------
// Loading skeleton
// ---------------------------------------------------------------------------

function EmailLogSkeleton() {
  return (
    <div className="space-y-2">
      <div className="flex gap-2">
        <Skeleton className="h-9 flex-1" />
        <Skeleton className="h-9 w-32" />
        <Skeleton className="h-9 w-28" />
      </div>
      {Array.from({ length: 6 }).map((_, i) => (
        <Skeleton key={i} className="h-10 w-full" />
      ))}
    </div>
  );
}
