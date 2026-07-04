import { useCallback, useMemo, useState } from "react";
import { createFileRoute, useNavigate, Link } from "@tanstack/react-router";
import {
  ShieldCheck,
  ShieldAlert,
  ShieldQuestion,
  ChevronLeft,
  ChevronRight,
  Inbox,
  Search,
  RefreshCw,
} from "lucide-react";
import { z } from "zod";
import type { AuditEntry } from "@wpmgr/api";

import { PageHeader } from "@/components/shared/page-header";
import { PageError } from "@/components/feedback";
import { Button } from "@/components/ui/button";
import { Select } from "@/components/ui/select";
import { Input } from "@/components/ui/input";
import { Skeleton } from "@/components/ui/skeleton";
import { cn } from "@/lib/utils";
import { useSites } from "@/features/sites/use-sites";
import { useAudit, useAuditVerify } from "@/features/audit/use-audit";
import { AuditIntegrityReport } from "@/features/audit/audit-integrity-report";
import { AuditEntryRow, AuditRunRow } from "@/features/audit/audit-row";
import { groupRuns } from "@/features/audit/group-runs";
import { actionLabel, classifySeverity, type AuditSeverity } from "@/features/audit/labels";
import { resolveActor } from "@/features/audit/actor";
import { metaString } from "@/features/audit/metadata";
import type { SiteMin } from "@/features/audit/types";

// Fleet audit log — redesigned for real operators (see the audit-log UX
// investigation this implements). The page owns: the two server-side filters
// (action-prefix preset, site), the client-side page filters (outcome,
// free-text search — cheap over one page of up to 50 entries), day-grouped
// rendering with read-burst collapsing, and the integrity badge/dialog. Row
// rendering, labeling, severity, and actor resolution all live in
// features/audit/* so this file stays about page composition.

// ---------------------------------------------------------------------------
// Route definition + search params
// ---------------------------------------------------------------------------

const searchSchema = z.object({
  // Prefix-match on the action field; "" means all. Server-side filter.
  action: z.string().optional(),
  // UUID of a specific site; "" means all sites. Server-side filter.
  site_id: z.string().optional(),
  // Pagination offset.
  offset: z.number().int().nonnegative().optional(),
});

type AuditSearch = z.infer<typeof searchSchema>;

export const Route = createFileRoute("/_authed/audit")({
  validateSearch: searchSchema,
  component: AuditPage,
});

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

const PAGE_LIMIT = 50;

// Quick-filter presets for the action field. Prefixes match the real
// emitted keys (apps/api/internal/audit/audit.go + each domain's Record call
// sites) — the previous "cache."/"settings."/"security." chips silently
// missed site.cache.*, smtp.settings.*, and site_security_*/auth.2fa.*.
const ACTION_PRESETS: { label: string; value: string }[] = [
  { label: "All events", value: "" },
  { label: "File manager", value: "site.files." },
  { label: "Backups", value: "backup." },
  { label: "Restores", value: "restore." },
  { label: "Updates", value: "update." },
  { label: "Security", value: "site_security" },
  { label: "2FA", value: "auth.2fa." },
  { label: "SMTP", value: "smtp.settings." },
  { label: "Cache", value: "site.cache." },
];

type OutcomeFilter = "all" | AuditSeverity;

const OUTCOME_FILTERS: { value: OutcomeFilter; label: string }[] = [
  { value: "all", label: "All" },
  { value: "denied", label: "Denied" },
  { value: "write", label: "Writes" },
  { value: "sensitive", label: "Sensitive" },
  { value: "read", label: "Reads" },
];

// ---------------------------------------------------------------------------
// Page
// ---------------------------------------------------------------------------

function AuditPage() {
  const navigate = useNavigate({ from: Route.fullPath });
  const search = Route.useSearch();

  const action = search.action ?? "";
  const siteId = search.site_id ?? "";
  const offset = search.offset ?? 0;

  const activePreset = ACTION_PRESETS.find((p) => p.value === action)?.value ?? action;

  const setFilter = useCallback(
    (patch: Partial<AuditSearch>) => {
      void navigate({
        search: (prev: AuditSearch) => ({
          ...prev,
          ...patch,
          // Reset to page 0 whenever any server-side filter changes.
          offset: "offset" in patch ? patch.offset : 0,
        }),
        replace: true,
      });
    },
    [navigate],
  );

  // Client-side page filters: outcome + free-text search operate over the
  // currently loaded page only (cheap at 50 rows; no new endpoint needed).
  const [outcome, setOutcome] = useState<OutcomeFilter>("all");
  const [query, setQuery] = useState("");

  // Sites list for the site dropdown + target-site resolution.
  const { data: sites = [] } = useSites();

  // Audit data.
  const { items, isPending, isError, error, isFetching, refetch } = useAudit({
    action,
    siteId,
    limit: PAGE_LIMIT,
    offset,
  });

  // Integrity check.
  const verify = useAuditVerify();
  const [integrityOpen, setIntegrityOpen] = useState(false);
  // Captured at the moment the badge is clicked, independent of `verify`'s
  // live state, so a successful re-baseline (which flips verify.data.ok to
  // true while the dialog is still open) doesn't yank `broken_at` out from
  // under the dialog and unmount its content mid-confirmation.
  const [integrityBrokenAt, setIntegrityBrokenAt] = useState<string | null>(null);

  const openIntegrityReport = useCallback(() => {
    setIntegrityBrokenAt(verify.data?.broken_at ?? null);
    setIntegrityOpen(true);
  }, [verify.data]);

  // Reload re-checks both the event list and chain integrity, and visibly
  // shows it's working (spinning icon + disabled button); previously it
  // only refetched the list with no feedback, so a click looked like a no-op.
  const isReloading = isFetching || verify.isFetching;
  const reload = () => {
    void refetch();
    void verify.refetch();
  };

  const severityCounts = useMemo(() => {
    const counts: Record<OutcomeFilter, number> = {
      all: items.length,
      denied: 0,
      write: 0,
      sensitive: 0,
      read: 0,
    };
    for (const entry of items) counts[classifySeverity(entry.action)] += 1;
    return counts;
  }, [items]);

  const visibleItems = useMemo(
    () => items.filter((e) => matchesFilters(e, outcome, query, sites)),
    [items, outcome, query, sites],
  );

  const groups = useMemo(() => groupByDay(visibleItems), [visibleItems]);

  const totalOnPage = items.length;
  const hasPrev = offset > 0;
  const hasNext = totalOnPage === PAGE_LIMIT;
  const isFiltered = visibleItems.length !== items.length;

  const clearPageFilters = () => {
    setOutcome("all");
    setQuery("");
  };

  return (
    <div className="space-y-6 px-4 pb-10 pt-6 sm:px-6">
      <PageHeader
        title="Audit log"
        subline="Fleet-wide operator event stream, newest first."
        actions={
          <div className="flex items-center gap-2">
            <IntegrityBadge verify={verify} onOpenReport={openIntegrityReport} />
            <Button
              type="button"
              size="sm"
              variant="ghost"
              onClick={reload}
              disabled={isReloading}
              aria-busy={isReloading}
              className="gap-1.5"
            >
              <RefreshCw
                aria-hidden="true"
                className={cn(
                  "size-3.5",
                  isReloading && "motion-safe:animate-spin motion-reduce:animate-none",
                )}
              />
              Reload
            </Button>
          </div>
        }
      />

      {/* Server-side filters: action category + site */}
      <div className="flex flex-wrap items-center gap-3">
        <div
          className="inline-flex flex-wrap items-center gap-1 rounded-md border border-border bg-card p-0.5"
          role="group"
          aria-label="Filter by action category"
        >
          {ACTION_PRESETS.map((preset) => {
            const selected = activePreset === preset.value;
            return (
              <button
                key={preset.value}
                type="button"
                aria-pressed={selected}
                onClick={() => setFilter({ action: preset.value || undefined })}
                className={cn(
                  "rounded px-2.5 py-1 text-xs font-medium transition-colors",
                  "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-1",
                  selected
                    ? "bg-muted text-foreground"
                    : "text-muted-foreground hover:text-foreground",
                )}
              >
                {preset.label}
              </button>
            );
          })}
        </div>

        {sites.length > 0 ? (
          <div className="w-[220px]">
            <label htmlFor="audit-site-filter" className="sr-only">
              Filter by site
            </label>
            <Select
              id="audit-site-filter"
              value={siteId}
              onChange={(e) => setFilter({ site_id: e.target.value || undefined })}
              aria-label="Filter by site"
            >
              <option value="">All sites</option>
              {sites.map((site) => (
                <option key={site.id} value={site.id}>
                  {site.name ?? site.url}
                </option>
              ))}
            </Select>
          </div>
        ) : null}
      </div>

      {/* Client-side filters: outcome + free-text search over this page */}
      <div className="flex flex-wrap items-center gap-3">
        <div
          className="inline-flex items-center gap-1 rounded-md border border-border bg-card p-0.5"
          role="group"
          aria-label="Filter by outcome"
        >
          {OUTCOME_FILTERS.map((f) => {
            const selected = outcome === f.value;
            const count = severityCounts[f.value];
            return (
              <button
                key={f.value}
                type="button"
                aria-pressed={selected}
                onClick={() => setOutcome(f.value)}
                className={cn(
                  "inline-flex items-center gap-1.5 rounded px-2.5 py-1 text-xs font-medium transition-colors",
                  "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-1",
                  selected
                    ? "bg-muted text-foreground"
                    : "text-muted-foreground hover:text-foreground",
                )}
              >
                {f.label}
                <span className="font-mono tabular-nums opacity-70">{count}</span>
              </button>
            );
          })}
        </div>

        <div className="relative w-full sm:ml-auto sm:w-[280px]">
          <Search
            aria-hidden="true"
            className="pointer-events-none absolute left-2.5 top-1/2 size-4 -translate-y-1/2 text-muted-foreground"
          />
          <Input
            type="search"
            inputMode="text"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            placeholder="Search this page: operator, action, path…"
            aria-label="Search the loaded events"
            className="h-9 w-full pl-8"
          />
        </div>
      </div>

      {/* Content */}
      {isPending ? (
        <AuditSkeleton />
      ) : isError ? (
        <PageError
          what="Could not load the audit log."
          why={error instanceof Error ? error.message : "Unknown error"}
          onRetry={() => void refetch()}
          retryLabel="Reload audit log"
        />
      ) : items.length === 0 ? (
        <EmptyAudit action={action} siteId={siteId} sites={sites} />
      ) : visibleItems.length === 0 ? (
        <EmptyFilteredAudit outcome={outcome} query={query} onClear={clearPageFilters} />
      ) : (
        <div className="overflow-hidden rounded-lg border border-border bg-card">
          {groups.map((group) => (
            <DayGroup
              key={group.key}
              label={group.label}
              isToday={group.isToday}
              entries={group.entries}
              sites={sites}
            />
          ))}
        </div>
      )}

      {/* Pagination */}
      {!isPending && !isError && items.length > 0 ? (
        <div className="flex items-center justify-between gap-3 px-1">
          <span className="text-xs tabular-nums text-muted-foreground">
            {isFiltered
              ? `${visibleItems.length} of ${totalOnPage} shown (page filtered)`
              : `${offset + 1}–${offset + totalOnPage} events`}
          </span>
          <div className="flex items-center gap-1">
            <Button
              type="button"
              variant="outline"
              size="sm"
              disabled={!hasPrev}
              onClick={() => setFilter({ offset: Math.max(0, offset - PAGE_LIMIT) })}
              className="gap-1"
              aria-label="Previous page"
            >
              <ChevronLeft aria-hidden="true" className="size-4" />
              Prev
            </Button>
            <Button
              type="button"
              variant="outline"
              size="sm"
              disabled={!hasNext}
              onClick={() => setFilter({ offset: offset + PAGE_LIMIT })}
              className="gap-1"
              aria-label="Next page"
            >
              Next
              <ChevronRight aria-hidden="true" className="size-4" />
            </Button>
          </div>
        </div>
      ) : null}

      {integrityOpen && integrityBrokenAt ? (
        <AuditIntegrityReport
          open={integrityOpen}
          onClose={() => setIntegrityOpen(false)}
          brokenAt={integrityBrokenAt}
          entries={items}
          verify={verify}
        />
      ) : null}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Client-side filter matching
// ---------------------------------------------------------------------------

function matchesFilters(
  entry: AuditEntry,
  outcome: OutcomeFilter,
  query: string,
  sites: SiteMin[],
): boolean {
  if (outcome !== "all" && classifySeverity(entry.action) !== outcome) return false;
  if (!query.trim()) return true;

  const q = query.trim().toLowerCase();
  const actor = resolveActor(entry);
  const path = metaString(entry.metadata, "path") ?? "";
  const site =
    entry.target_type === "site"
      ? (sites.find((s) => s.id === entry.target_id)?.name ?? "")
      : "";

  const haystack = [actionLabel(entry.action), entry.action, actor.name, actor.detail ?? "", path, site]
    .join(" ")
    .toLowerCase();
  return haystack.includes(q);
}

// ---------------------------------------------------------------------------
// Integrity badge
// ---------------------------------------------------------------------------

function IntegrityBadge({
  verify,
  onOpenReport,
}: {
  verify: ReturnType<typeof useAuditVerify>;
  onOpenReport: () => void;
}) {
  if (verify.isPending) {
    return (
      <span className="inline-flex items-center rounded border border-border bg-muted px-2 py-0.5 text-xs font-medium text-muted-foreground">
        Verifying
      </span>
    );
  }
  if (verify.isError || !verify.data) {
    return (
      <span className="inline-flex items-center gap-1.5 rounded border border-border bg-muted px-2 py-0.5 text-xs font-medium text-muted-foreground">
        <ShieldQuestion aria-hidden="true" className="size-3.5" />
        Integrity: not checked
      </span>
    );
  }
  if (verify.data.ok) {
    return (
      <span className="inline-flex items-center gap-1.5 rounded bg-success-subtle px-2 py-0.5 text-xs font-medium text-success-subtle-fg">
        <ShieldCheck aria-hidden="true" className="size-3.5" />
        Chain verified
      </span>
    );
  }
  return (
    <button
      type="button"
      aria-haspopup="dialog"
      onClick={onOpenReport}
      className={cn(
        "inline-flex cursor-pointer items-center gap-1.5 rounded bg-destructive-subtle px-2 py-0.5 text-xs font-medium text-destructive-subtle-fg",
        "underline-offset-2 hover:underline",
        "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-1",
      )}
    >
      <ShieldAlert aria-hidden="true" className="size-3.5" />
      Chain break
    </button>
  );
}

// ---------------------------------------------------------------------------
// Day grouping
// ---------------------------------------------------------------------------

interface DayGroupData {
  key: string;
  label: string;
  isToday: boolean;
  entries: AuditEntry[];
}

function groupByDay(entries: AuditEntry[]): DayGroupData[] {
  const groups: DayGroupData[] = [];
  const byKey = new Map<string, DayGroupData>();
  const now = new Date();
  const todayKey = localDayKey(now);
  const yesterday = new Date(now);
  yesterday.setDate(now.getDate() - 1);
  const yesterdayKey = localDayKey(yesterday);

  for (const entry of entries) {
    const date = new Date(entry.created_at);
    const key = Number.isNaN(date.getTime()) ? "unknown" : localDayKey(date);
    let group = byKey.get(key);
    if (!group) {
      group = {
        key,
        label: dayLabel(key, date, todayKey, yesterdayKey),
        isToday: key === todayKey,
        entries: [],
      };
      byKey.set(key, group);
      groups.push(group);
    }
    group.entries.push(entry);
  }
  return groups;
}

function localDayKey(date: Date): string {
  const y = date.getFullYear();
  const m = String(date.getMonth() + 1).padStart(2, "0");
  const d = String(date.getDate()).padStart(2, "0");
  return `${y}-${m}-${d}`;
}

function dayLabel(
  key: string,
  date: Date,
  todayKey: string,
  yesterdayKey: string,
): string {
  if (key === "unknown") return "Unknown date";
  if (key === todayKey) return "Today";
  if (key === yesterdayKey) return "Yesterday";
  return date.toLocaleDateString(undefined, {
    month: "short",
    day: "numeric",
    year: "numeric",
  });
}

// ---------------------------------------------------------------------------
// DayGroup — renders each day's entries as collapsed runs + standalone rows
// ---------------------------------------------------------------------------

function DayGroup({
  label,
  isToday,
  entries,
  sites,
}: {
  label: string;
  isToday: boolean;
  entries: AuditEntry[];
  sites: SiteMin[];
}) {
  const runs = useMemo(() => groupRuns(entries), [entries]);

  return (
    <div>
      <div className="sticky top-0 z-10 border-b border-border bg-card/95 px-4 py-1.5 text-xs font-medium uppercase tracking-wide text-muted-foreground backdrop-blur supports-[backdrop-filter]:bg-card/80">
        {label}
      </div>
      <ul className="divide-y divide-border">
        {runs.map((run, index) => {
          // Every run always has at least one entry (see groupRuns); its first
          // entry's id is a stable, pure key. The index fallback only matters
          // for a hypothetical empty run, which groupRuns never produces.
          const key = run.entries[0]?.id ?? `run-${index}`;
          return (
            <li key={key}>
              {run.kind === "run" ? (
                <AuditRunRow run={run} sites={sites} isToday={isToday} />
              ) : (
                <AuditEntryRow entry={run.entries[0]!} sites={sites} isToday={isToday} />
              )}
            </li>
          );
        })}
      </ul>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Empty + skeleton states
// ---------------------------------------------------------------------------

function EmptyAudit({
  action,
  siteId,
  sites,
}: {
  action: string;
  siteId: string;
  sites: SiteMin[];
}) {
  const hasFilter = !!action || !!siteId;
  const siteName = siteId
    ? (sites.find((s) => s.id === siteId)?.name ?? siteId.slice(0, 8))
    : null;

  return (
    <div className="flex flex-col items-center justify-center gap-3 rounded-lg border border-border bg-card px-6 py-14 text-center">
      <Inbox aria-hidden="true" className="size-6 text-muted-foreground" />
      {hasFilter ? (
        <>
          <p className="text-sm font-medium text-foreground">No matching events</p>
          <p className="max-w-xs text-xs text-muted-foreground">
            {action && siteName
              ? `No ${action} events for ${siteName}.`
              : action
                ? `No events with prefix "${action}".`
                : `No events for ${siteName ?? "this site"}.`}
          </p>
        </>
      ) : (
        <>
          <p className="text-sm font-medium text-foreground">No audit events yet</p>
          <p className="max-w-xs text-xs text-muted-foreground">
            Operator events are recorded here as they occur across all sites in
            your account.
          </p>
        </>
      )}
    </div>
  );
}

function EmptyFilteredAudit({
  outcome,
  query,
  onClear,
}: {
  outcome: OutcomeFilter;
  query: string;
  onClear: () => void;
}) {
  const outcomeLabel = OUTCOME_FILTERS.find((f) => f.value === outcome)?.label ?? "matching";

  return (
    <div className="flex flex-col items-center justify-center gap-3 rounded-lg border border-border bg-card px-6 py-14 text-center">
      <Inbox aria-hidden="true" className="size-6 text-muted-foreground" />
      <p className="text-sm font-medium text-foreground">
        {outcome === "all"
          ? "No events match your search"
          : `No ${outcomeLabel.toLowerCase()} events in this range`}
      </p>
      <p className="max-w-xs text-xs text-muted-foreground">
        {query
          ? `Nothing on this page matches "${query}".`
          : "Try a different outcome filter, or page back to see older events."}
      </p>
      <Button type="button" variant="outline" size="sm" onClick={onClear}>
        Clear page filters
      </Button>
    </div>
  );
}

function AuditSkeleton() {
  return (
    <div
      role="status"
      aria-busy="true"
      className="overflow-hidden rounded-lg border border-border bg-card"
    >
      <span className="sr-only">Loading audit log</span>
      <div className="border-b border-border px-4 py-1.5">
        <Skeleton className="h-3 w-12" />
      </div>
      <ul className="divide-y divide-border">
        {Array.from({ length: 8 }).map((_, i) => (
          <li key={i} className="flex items-center gap-3 px-4 py-3">
            <Skeleton className="size-5 shrink-0 rounded-full" />
            <div className="flex min-w-0 flex-1 items-center justify-between gap-3">
              <Skeleton className="h-3.5 w-1/3" />
              <Skeleton className="h-3 w-1/4" />
              <Skeleton className="h-3 w-10 shrink-0" />
            </div>
          </li>
        ))}
      </ul>
    </div>
  );
}

// Exported so FileBrowser can import it for the "View activity" deep-link.
// This avoids the caller having to hard-code the URL shape.
export { Link };
