import React from "react";
import { ShieldAlert } from "lucide-react";

import { cn, relativeTime } from "@/lib/utils";
import type { SiteActivityEvent } from "@wpmgr/api";

import { objectTypeIcon } from "./object-type-icon";

// Stable wrapper so the React Compiler sees a fixed component reference
// rather than a component created during ActivityRow's render. We use
// React.createElement directly to avoid the JSX-component-creation path
// that `react-hooks/static-components` flags; LucideIcon values from
// objectTypeIcon are always stable module-level components.
function ObjectIcon({
  type,
  className,
}: {
  type: string;
  className: string;
}) {
  return React.createElement(objectTypeIcon(type), { className });
}

// One row of the activity feed (ADR-037 redesign).
//
// This is a LOG, not a data table: rows vary in importance and read better as a
// two-line stream than as a cramped column grid. The old layout hid the
// ready-made `summary` sentence and squeezed actor/IP into narrow columns;
// here the summary is the visual lead and actor + IP always live on line 2.
//
//   • Line 1: object-type icon in a severity-tinted circle, the summary
//     sentence, and the relative time (absolute UTC in title).
//   • Line 2 (muted): event_type pill · actor · IP · object label, each in the
//     right typographic register (mono for event_type / IP / object).
//   • Severity reads as a colored dot at the left edge.
//   • A broken chain link (chain_valid=false) keeps a loud destructive left
//     accent + a ShieldAlert so a tampered row is impossible to miss.
//
// The whole row is a <button> so it is keyboard-operable and opens the drawer.

export interface ActivityRowProps {
  event: SiteActivityEvent;
  onOpen: () => void;
}

type Severity = SiteActivityEvent["severity"];

// Severity -> token classes. `dot` paints the left-edge status dot; `iconWrap`
// tints the object-type icon circle. All values are design tokens (no off-token
// hex / tailwind palette colors), so light + dark + AA all hold.
const SEVERITY_DOT: Record<Severity, string> = {
  high: "bg-destructive",
  medium: "bg-warning",
  low: "bg-muted-foreground",
};

const SEVERITY_ICON_WRAP: Record<Severity, string> = {
  high: "bg-destructive-subtle text-destructive-subtle-fg",
  medium: "bg-warning-subtle text-warning-subtle-fg",
  low: "bg-muted text-muted-foreground",
};

export function ActivityRow({ event, onOpen }: ActivityRowProps) {
  const tampered = !event.chain_valid;
  const hasActor = event.actor_login !== "" && event.actor_user_id !== 0;
  const objectText =
    event.object_label !== "" ? event.object_label : event.object_id;
  const rel = relativeTime(event.occurred_at);

  return (
    <button
      type="button"
      onClick={onOpen}
      data-chain-invalid={tampered || undefined}
      className={cn(
        "group relative flex w-full items-start gap-3 px-4 py-3 text-left",
        "transition-colors hover:bg-muted/40",
        "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-inset",
        tampered &&
          "bg-destructive-subtle/40 before:absolute before:inset-y-0 before:left-0 before:w-0.5 before:bg-destructive before:content-['']",
      )}
    >
      {/* Severity dot pinned to the left gutter. */}
      <span
        aria-label={`Severity: ${event.severity}`}
        role="img"
        className={cn(
          "mt-2 size-1.5 shrink-0 rounded-full",
          SEVERITY_DOT[event.severity],
        )}
      />

      {/* Object-type icon in a severity-tinted circle. */}
      <span
        aria-hidden="true"
        className={cn(
          "mt-0.5 flex size-7 shrink-0 items-center justify-center rounded-full",
          SEVERITY_ICON_WRAP[event.severity],
        )}
      >
        <ObjectIcon type={event.object_type} className="size-3.5" />
      </span>

      <span className="flex min-w-0 flex-1 flex-col gap-1">
        {/* Line 1: summary headline + time. */}
        <span className="flex items-start justify-between gap-3">
          <span className="flex min-w-0 items-center gap-1.5">
            {tampered ? (
              <ShieldAlert
                aria-label="Chain integrity broken"
                className="size-3.5 shrink-0 text-destructive"
              />
            ) : null}
            <span
              className="truncate text-sm text-foreground"
              title={event.summary}
            >
              {event.summary}
            </span>
          </span>
          <time
            dateTime={event.occurred_at}
            title={event.occurred_at}
            className="shrink-0 text-xs tabular-nums text-muted-foreground"
          >
            {rel ?? "just now"}
          </time>
        </span>

        {/* Line 2: metadata, separated by middots. Nothing is hidden. */}
        <span className="flex flex-wrap items-center gap-x-1.5 gap-y-1 text-xs text-muted-foreground">
          <span className="rounded-sm bg-muted px-1.5 py-0.5 font-mono text-[11px] text-muted-foreground">
            {event.event_type}
          </span>
          <span aria-hidden="true">·</span>
          {hasActor ? (
            <span className="truncate">{event.actor_login}</span>
          ) : (
            <span className="italic">system</span>
          )}
          {event.actor_ip !== "" ? (
            <>
              <span aria-hidden="true">·</span>
              <span className="font-mono tabular-nums">{event.actor_ip}</span>
            </>
          ) : null}
          {objectText !== "" ? (
            <>
              <span aria-hidden="true">·</span>
              <span className="max-w-[260px] truncate font-mono" title={objectText}>
                {objectText}
              </span>
            </>
          ) : null}
        </span>
      </span>
    </button>
  );
}
