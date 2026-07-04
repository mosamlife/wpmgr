import { useState } from "react";
import { Ban, ChevronDown, ShieldAlert } from "lucide-react";
import type { AuditEntry } from "@wpmgr/api";

import { cn, relativeTime } from "@/lib/utils";

import { ActorChip } from "./actor-chip";
import { AuditEntryDetail, RunDetail } from "./audit-detail";
import type { AuditRun } from "./group-runs";
import { runSummaryLabel } from "./group-runs";
import { actionLabel, classifySeverity, type AuditSeverity } from "./labels";
import { commonPathPrefix, formatClockTime, metaString } from "./metadata";
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
  const display = isToday ? (relativeTime(iso) ?? "just now") : formatClockTime(iso);
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

function TargetSlot({ entry, sites }: { entry: AuditEntry; sites: SiteMin[] }) {
  const path = metaString(entry.metadata, "path");

  if (entry.target_type === "site") {
    const site = sites.find((s) => s.id === entry.target_id);
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
          {formatClockTime(startIso)}&ndash;{formatClockTime(endIso)}
        </span>
      </summary>
      {open ? <RunDetail entries={entries} isToday={isToday} /> : null}
    </details>
  );
}
