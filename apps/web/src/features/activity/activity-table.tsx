import { useMemo, useRef, useState } from "react";
import { motion } from "motion/react";
import { ShieldCheck, ShieldAlert, Search, Inbox } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Skeleton } from "@/components/ui/skeleton";
import { PageError } from "@/components/feedback";
import { cn } from "@/lib/utils";
import { fadeUp } from "@/lib/motion-presets";
import type { SiteActivityEvent } from "@wpmgr/api";

import { ActivityRow } from "./activity-row";
import { ActivityDetailDrawer } from "./activity-detail-drawer";
import {
  useActivity,
  useActivityVerify,
  type ActivityFilters,
  type SeverityFilter,
} from "./use-activity";

// The WordPress activity log for one site (ADR-037, redesigned).
//
// The log renders as a day-grouped FEED of two-line rows (see activity-row),
// not a column grid: the human `summary` sentence leads, and actor + IP are
// always shown on line 2 instead of being squeezed into narrow columns.
//
// Toolbar: a severity chip group (with live per-severity counts), an
// object-type chip group derived from the loaded rows, an actor search input,
// the server-side integrity badge, and a reload control. Verb-first voice, no
// em-dashes, tabular-nums, font-mono for hashes / IPs / object ids.

const SEVERITIES: SeverityFilter[] = ["all", "high", "medium", "low"];

const SEVERITY_LABEL: Record<SeverityFilter, string> = {
  all: "All",
  high: "High",
  medium: "Medium",
  low: "Low",
};

export function ActivityTable({ siteId }: { siteId: string }) {
  const [severity, setSeverity] = useState<SeverityFilter>("all");
  const [objectType, setObjectType] = useState("");
  const [actorLogin, setActorLogin] = useState("");
  const [active, setActive] = useState<SiteActivityEvent | null>(null);

  const filters: ActivityFilters = { severity, objectType, actorLogin };
  const { data, isPending, isError, error, refetch } = useActivity(
    siteId,
    filters,
  );
  const verify = useActivityVerify(siteId);

  const rows = data ?? [];

  // Derive the object_type chip set from whatever the current page returned, so
  // the filter reflects the site's real event surface without a second query.
  const objectTypes = useMemo(() => {
    const set = new Set<string>();
    for (const e of rows) set.add(e.object_type);
    return Array.from(set).sort();
  }, [rows]);

  // Per-severity counts for the chip group. Computed from the unfiltered "all"
  // shape is not available client-side, so we count the rows we have. When a
  // severity filter is active the other counts read 0, which is honest: the
  // chips reflect what is currently loaded.
  const severityCounts = useMemo(() => {
    const counts = { all: rows.length, high: 0, medium: 0, low: 0 };
    for (const e of rows) counts[e.severity] += 1;
    return counts;
  }, [rows]);

  const groups = useMemo(() => groupByDay(rows), [rows]);

  return (
    <section
      aria-labelledby="activity-heading"
      className="space-y-4 px-6 pb-8 pt-6"
    >
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h2
          id="activity-heading"
          className="text-xs font-medium uppercase tracking-wide text-muted-foreground"
        >
          Activity log
        </h2>
        <div className="flex items-center gap-2">
          <IntegrityBadge
            valid={verify.data?.valid ?? true}
            breakAtSeq={verify.data?.break_at_seq ?? null}
            pending={verify.isPending}
          />
          <Button
            type="button"
            size="sm"
            variant="ghost"
            onClick={() => {
              void refetch();
              void verify.refetch();
            }}
          >
            Reload
          </Button>
        </div>
      </div>

      <div className="flex flex-wrap items-center gap-3">
        <div
          className="inline-flex items-center gap-1 rounded-md border border-border bg-card p-0.5"
          role="group"
          aria-label="Filter by severity"
        >
          {SEVERITIES.map((s) => {
            const selected = severity === s;
            const count = severityCounts[s];
            return (
              <button
                key={s}
                type="button"
                aria-pressed={selected}
                onClick={() => setSeverity(s)}
                className={cn(
                  "inline-flex items-center gap-1.5 rounded px-2.5 py-1 text-xs font-medium transition-colors",
                  "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-1",
                  selected
                    ? "bg-muted text-foreground"
                    : "text-muted-foreground hover:text-foreground",
                )}
              >
                {SEVERITY_LABEL[s]}
                <span className="font-mono tabular-nums opacity-70">
                  {count}
                </span>
              </button>
            );
          })}
        </div>

        {objectTypes.length > 0 ? (
          <div
            className="inline-flex flex-wrap gap-1"
            role="group"
            aria-label="Filter by object type"
          >
            <ObjectChip
              label="All objects"
              selected={objectType === ""}
              onClick={() => setObjectType("")}
            />
            {objectTypes.map((t) => (
              <ObjectChip
                key={t}
                label={t}
                selected={objectType === t}
                onClick={() => setObjectType(objectType === t ? "" : t)}
              />
            ))}
          </div>
        ) : null}

        <div className="relative ml-auto">
          <Search
            aria-hidden="true"
            className="pointer-events-none absolute left-2.5 top-1/2 size-4 -translate-y-1/2 text-muted-foreground"
          />
          <Input
            type="search"
            inputMode="text"
            value={actorLogin}
            onChange={(e) => setActorLogin(e.target.value)}
            placeholder="Search by actor"
            aria-label="Search by actor login"
            className="h-9 w-[260px] pl-8"
          />
        </div>
      </div>

      {isPending ? (
        <ActivityFeedSkeleton />
      ) : isError ? (
        <PageError
          what="Could not load activity."
          why={error instanceof Error ? error.message : "Unknown error"}
          onRetry={() => void refetch()}
          retryLabel="Reload activity"
        />
      ) : rows.length === 0 ? (
        <EmptyActivity />
      ) : (
        <div className="overflow-hidden rounded-lg border border-border bg-card">
          {groups.map((group) => (
            <DayGroup
              key={group.key}
              label={group.label}
              events={group.events}
              onOpen={setActive}
            />
          ))}
        </div>
      )}

      <ActivityDetailDrawer event={active} onClose={() => setActive(null)} />
    </section>
  );
}

function ObjectChip({
  label,
  selected,
  onClick,
}: {
  label: string;
  selected: boolean;
  onClick: () => void;
}) {
  return (
    <button
      type="button"
      aria-pressed={selected}
      onClick={onClick}
      className={cn(
        "rounded px-2 py-1 text-xs font-medium transition-colors",
        "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-1",
        selected
          ? "border border-border bg-muted text-foreground"
          : "border border-transparent text-muted-foreground hover:text-foreground",
      )}
    >
      {label}
    </button>
  );
}

function DayGroup({
  label,
  events,
  onOpen,
}: {
  label: string;
  events: SiteActivityEvent[];
  onOpen: (event: SiteActivityEvent) => void;
}) {
  // First-mount fadeUp only — re-fetches (30s poll) must not re-fire the
  // entrance, which would read as flicker to an operator scanning the feed.
  const hasMounted = useRef(false);
  const firstMount = !hasMounted.current;
  hasMounted.current = true;

  return (
    <div>
      <div className="sticky top-0 z-10 border-b border-border bg-card/95 px-4 py-1.5 text-xs font-medium uppercase tracking-wide text-muted-foreground backdrop-blur supports-[backdrop-filter]:bg-card/80">
        {label}
      </div>
      <motion.ul
        variants={fadeUp}
        initial={firstMount ? "initial" : false}
        animate="animate"
        className="divide-y divide-border"
      >
        {events.map((e) => (
          <li key={e.id}>
            <ActivityRow event={e} onOpen={() => onOpen(e)} />
          </li>
        ))}
      </motion.ul>
    </div>
  );
}

function ActivityFeedSkeleton() {
  return (
    <div
      role="status"
      aria-busy="true"
      className="overflow-hidden rounded-lg border border-border bg-card"
    >
      <span className="sr-only">Loading activity</span>
      <div className="border-b border-border px-4 py-1.5">
        <Skeleton className="h-3 w-16" />
      </div>
      <ul className="divide-y divide-border">
        {Array.from({ length: 6 }).map((_, i) => (
          <li key={i} className="flex items-start gap-3 px-4 py-3">
            <Skeleton className="mt-2 size-1.5 rounded-full" />
            <Skeleton className="mt-0.5 size-7 rounded-full" />
            <div className="flex min-w-0 flex-1 flex-col gap-2">
              <div className="flex items-center justify-between gap-3">
                <Skeleton className="h-3.5 w-2/3" />
                <Skeleton className="h-3 w-12 shrink-0" />
              </div>
              <Skeleton className="h-3 w-1/2" />
            </div>
          </li>
        ))}
      </ul>
    </div>
  );
}

function EmptyActivity() {
  return (
    <div className="flex flex-col items-center justify-center gap-2 rounded-lg border border-border bg-card px-6 py-12 text-center">
      <Inbox aria-hidden="true" className="size-6 text-muted-foreground" />
      <p className="text-sm font-medium text-foreground">No activity yet</p>
      <p className="max-w-sm text-xs text-muted-foreground">
        The agent ships hash-chained events as they occur, newest first.
      </p>
    </div>
  );
}

function IntegrityBadge({
  valid,
  breakAtSeq,
  pending,
}: {
  valid: boolean;
  breakAtSeq: number | null;
  pending: boolean;
}) {
  if (pending) {
    return (
      <span className="inline-flex items-center rounded border border-border bg-muted px-2 py-0.5 text-xs font-medium text-muted-foreground">
        Verifying integrity
      </span>
    );
  }
  if (valid) {
    return (
      <span className="inline-flex items-center gap-1.5 rounded bg-success-subtle px-2 py-0.5 text-xs font-medium text-success-subtle-fg">
        <ShieldCheck aria-hidden="true" className="size-3.5" />
        Integrity verified
      </span>
    );
  }
  return (
    <span className="inline-flex items-center gap-1.5 rounded bg-destructive-subtle px-2 py-0.5 text-xs font-medium text-destructive-subtle-fg">
      <ShieldAlert aria-hidden="true" className="size-3.5" />
      Chain break at seq{" "}
      <span className="tabular-nums">{breakAtSeq ?? "?"}</span>
    </span>
  );
}

// ---------------------------------------------------------------------------
// Day grouping
// ---------------------------------------------------------------------------

interface DayGroupData {
  key: string;
  label: string;
  events: SiteActivityEvent[];
}

/**
 * Group events into calendar-day buckets (UTC timestamp rendered in the
 * operator's local zone). The input is already newest-first from the query, so
 * the resulting group order is preserved. Labels read "Today" / "Yesterday" /
 * "May 28, 2026" to keep a long log navigable.
 */
function groupByDay(events: SiteActivityEvent[]): DayGroupData[] {
  const groups: DayGroupData[] = [];
  const byKey = new Map<string, DayGroupData>();
  const now = new Date();
  const todayKey = dayKey(now);
  const yesterday = new Date(now);
  yesterday.setDate(now.getDate() - 1);
  const yesterdayKey = dayKey(yesterday);

  for (const event of events) {
    const date = new Date(event.occurred_at);
    const key = Number.isNaN(date.getTime()) ? "unknown" : dayKey(date);
    let group = byKey.get(key);
    if (!group) {
      group = { key, label: dayLabel(key, date, todayKey, yesterdayKey), events: [] };
      byKey.set(key, group);
      groups.push(group);
    }
    group.events.push(event);
  }
  return groups;
}

/** Local-zone YYYY-MM-DD key for a date. */
function dayKey(date: Date): string {
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
