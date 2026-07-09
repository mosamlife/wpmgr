import { useState } from "react";
import { Ban, ChevronDown, ShieldAlert } from "lucide-react";
import type { AuditEntry } from "@wpmgr/api";

import { cn, relativeTime } from "@/lib/utils";

import { ActorChip } from "./actor-chip";
import { AuditEntryDetail, RunDetail } from "./audit-detail";
import type { AuditRun } from "./group-runs";
import { runSummaryLabel } from "./group-runs";
import { actionLabel, classifySeverity, type AuditSeverity } from "./labels";
import {
  commonPathPrefix,
  formatClockTime,
  formatDateTime,
  humanizeTargetType,
  metaString,
} from "./metadata";
import type { SiteMin } from "./types";

// One row of the fleet audit log (redesign points 1, 3, 4, 5, 7, 10).
//
// Aligned tracks, not middot flex-wrap: [actor] [action + severity pill]
// [target: site pill + path] [time], stacked on mobile. Severity reads as a
// left rail (color + intensity) plus, for anything above "read", a small
// pill next to the action label — reads stay unmarked and quiet. Every row
// is a <details> so the full metadata (raw event key, denial reason, ids,
// hashes) is one click away without competing with the collapsed view.

const ROW_GRID =
  "grid grid-cols-1 items-center gap-x-4 gap-y-1 px-4 py-2.5 sm:grid-cols-[168px_minmax(0,1.3fr)_minmax(0,220px)_92px]";

const RAIL: Record<AuditSeverity, string> = {
  denied: "border-l-2 border-destructive bg-destructive-subtle/40",
  sensitive: "border-l-2 border-info",
  write: "border-l-2 border-warning",
  read: "border-l-2 border-transparent",
};

function SeverityPill({ severity }: { severity: AuditSeverity }) {
  if (severity === "denied") {
    return (
      <span className="inline-flex shrink-0 items-center gap-1 rounded bg-destructive-subtle px-1.5 py-0.5 text-[11px] font-medium text-destructive-subtle-fg">
        <Ban aria-hidden="true" className="size-3" />
        Denied
      </span>
    );
  }
  if (severity === "sensitive") {
    return (
      <span className="inline-flex shrink-0 items-center gap-1 rounded bg-info-subtle px-1.5 py-0.5 text-[11px] font-medium text-info-subtle-fg">
        <ShieldAlert aria-hidden="true" className="size-3" />
        Sensitive
      </span>
    );
  }
  if (severity === "write") {
    return (
      <span className="inline-flex shrink-0 items-center rounded bg-warning-subtle px-1.5 py-0.5 text-[11px] font-medium text-warning-subtle-fg">
        Write
      </span>
    );
  }
  return null;
}

function EntryTime({ iso, isToday }: { iso: string; isToday: boolean }) {
  const display = isToday ? (relativeTime(iso) ?? "just now") : formatDateTime(iso);
  return (
    <time
      dateTime={iso}
      title={iso}
      className="justify-self-start text-xs tabular-nums text-muted-foreground sm:justify-self-end"
    >
      {display}
    </time>
  );
}

/**
 * Best-effort site id for a non-"site" target_type, so a backup/restore/
 * update row (target_type "backup_snapshot", "update_task", ...) can still
 * resolve and show the site it happened on instead of just the raw target
 * type (GH #201). The control plane attaches `metadata.site_id` to every
 * site-scoped lifecycle event that isn't itself target_type "site"; the one
 * exception is "backup_schedule", whose events don't carry metadata.site_id
 * because the schedule row's own id IS the site id (backup/handler.go's
 * recordScheduleChange sets TargetID to sched.SiteID). Entries with neither
 * (e.g. update_run, which is scoped to a run, not a single site) return null
 * and fall through to the existing raw target_type display.
 */
function targetSiteId(entry: AuditEntry): string | null {
  if (entry.target_type === "site") return entry.target_id;
  const metaSiteId = metaString(entry.metadata, "site_id");
  if (metaSiteId) return metaSiteId;
  if (entry.target_type === "backup_schedule") return entry.target_id;
  return null;
}

function TargetSlot({ entry, sites }: { entry: AuditEntry; sites: SiteMin[] }) {
  const path = metaString(entry.metadata, "path");
  const isSiteTarget = entry.target_type === "site";
  const siteId = targetSiteId(entry);
  const site = siteId ? sites.find((s) => s.id === siteId) : undefined;

  if (isSiteTarget) {
    return (
      <div className="flex min-w-0 flex-col gap-0.5">
        <span
          className={cn(
            "inline-flex w-fit max-w-full items-center truncate rounded px-1.5 py-0.5 text-xs font-medium",
            site ? "bg-muted text-foreground" : "italic text-muted-foreground",
          )}
        >
          {site ? (site.name || site.url) : "Unknown site"}
        </span>
        {path ? (
          <span
            className="truncate font-mono text-[11px] text-muted-foreground"
            title={path}
          >
            {path}
          </span>
        ) : null}
      </div>
    );
  }

  // Site-scoped non-"site" target (backup/restore/update, ...): show the
  // resolved site name as the primary label, and keep the target-type
  // context (e.g. "Backup snapshot") as a secondary line rather than
  // silently dropping the fact that this was a backup/update on that site.
  if (site) {
    return (
      <div className="flex min-w-0 flex-col gap-0.5">
        <span className="inline-flex w-fit max-w-full items-center truncate rounded bg-muted px-1.5 py-0.5 text-xs font-medium text-foreground">
          {site.name || site.url}
        </span>
        <span
          className="truncate text-[11px] text-muted-foreground"
          title={`${humanizeTargetType(entry.target_type)} · ${entry.target_id}`}
        >
          {humanizeTargetType(entry.target_type)}
        </span>
      </div>
    );
  }

  if (entry.target_id) {
    return (
      <span
        className="inline-flex w-fit max-w-full items-center truncate rounded bg-muted px-1.5 py-0.5 text-[11px] font-medium text-muted-foreground"
        title={`${entry.target_type} · ${entry.target_id}`}
      >
        {entry.target_type}
      </span>
    );
  }

  return <span className="text-xs text-muted-foreground">—</span>;
}

export function AuditEntryRow({
  entry,
  sites,
  isToday,
}: {
  entry: AuditEntry;
  sites: SiteMin[];
  isToday: boolean;
}) {
  const [open, setOpen] = useState(false);
  const severity = classifySeverity(entry.action);
  const label = actionLabel(entry.action);

  return (
    <details
      className="group"
      open={open}
      onToggle={(e) => setOpen(e.currentTarget.open)}
    >
      <summary
        className={cn(
          "grid cursor-pointer list-none",
          ROW_GRID,
          RAIL[severity],
          "hover:bg-muted/30 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-inset",
        )}
        title={entry.action}
      >
        <ActorChip entry={entry} />
        <div className="flex min-w-0 items-center gap-1.5">
          <ChevronDown
            aria-hidden="true"
            className="size-3.5 shrink-0 text-muted-foreground transition-transform group-open:rotate-180"
          />
          <span
            className={cn(
              "truncate text-sm",
              severity === "denied"
                ? "font-medium text-destructive"
                : "font-medium text-foreground",
            )}
          >
            {label}
          </span>
          <SeverityPill severity={severity} />
        </div>
        <TargetSlot entry={entry} sites={sites} />
        <EntryTime iso={entry.created_at} isToday={isToday} />
      </summary>
      {open ? <AuditEntryDetail entry={entry} /> : null}
    </details>
  );
}

export function AuditRunRow({
  run,
  sites,
  isToday,
}: {
  run: AuditRun;
  sites: SiteMin[];
  isToday: boolean;
}) {
  const [open, setOpen] = useState(false);
  const entries = run.entries;
  const first = entries[0];
  if (!first) return null;

  const count = entries.length;
  const label = runSummaryLabel(first.action, count);
  const prefix = commonPathPrefix(entries.map((e) => metaString(e.metadata, "path")));
  const times = entries
    .map((e) => Date.parse(e.created_at))
    .filter((n) => !Number.isNaN(n));
  const startIso = times.length ? new Date(Math.min(...times)).toISOString() : first.created_at;
  const endIso = times.length ? new Date(Math.max(...times)).toISOString() : first.created_at;
  const site =
    first.target_type === "site" ? sites.find((s) => s.id === first.target_id) : null;

  return (
    <details
      className="group"
      open={open}
      onToggle={(e) => setOpen(e.currentTarget.open)}
    >
      <summary
        className={cn(
          "grid cursor-pointer list-none",
          ROW_GRID,
          RAIL.read,
          "hover:bg-muted/30 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-inset",
        )}
      >
        <ActorChip entry={first} />
        <div className="flex min-w-0 items-center gap-1.5">
          <ChevronDown
            aria-hidden="true"
            className="size-3.5 shrink-0 text-muted-foreground transition-transform group-open:rotate-180"
          />
          <span className="truncate text-sm text-foreground">{label}</span>
          <span className="shrink-0 rounded bg-muted px-1.5 py-0.5 font-mono text-[11px] tabular-nums text-muted-foreground">
            x{count}
          </span>
        </div>
        <div className="flex min-w-0 flex-col gap-0.5">
          {first.target_type === "site" ? (
            <span
              className={cn(
                "inline-flex w-fit max-w-full items-center truncate rounded px-1.5 py-0.5 text-xs font-medium",
                site ? "bg-muted text-foreground" : "italic text-muted-foreground",
              )}
            >
              {site ? (site.name || site.url) : "Unknown site"}
            </span>
          ) : null}
          {prefix ? (
            <span
              className="truncate font-mono text-[11px] text-muted-foreground"
              title={prefix}
            >
              {prefix}
            </span>
          ) : null}
        </div>
        <span className="justify-self-start text-xs tabular-nums text-muted-foreground sm:justify-self-end">
          <time dateTime={startIso} title={startIso}>
            {isToday ? formatClockTime(startIso) : formatDateTime(startIso)}
          </time>
          &ndash;
          <time dateTime={endIso} title={endIso}>
            {formatClockTime(endIso)}
          </time>
        </span>
      </summary>
      {open ? <RunDetail entries={entries} isToday={isToday} /> : null}
    </details>
  );
}
