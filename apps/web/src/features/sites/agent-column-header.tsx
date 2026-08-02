import { AlertTriangle, Info } from "lucide-react";
import type { AgentMirrorStatus, FleetAgentVersions } from "@wpmgr/api";

import {
  Popover,
  PopoverContent,
  PopoverTrigger,
} from "@/components/ui/popover";
import { cn } from "@/lib/utils";

import { describeReferenceCheck } from "./agent-reference-check";

// GH #255 follow-up / GH #261.
//
// The Sites table's Agent column used to append "in fleet" to every single
// "Current" row when the freshness comparison had no published release
// manifest to work from. The qualifier was correct (a self-hosted fleet's
// "newest agent seen anywhere in this tenant" is not the newest agent that
// exists) but it is a property of the COMPARISON, not of each site, so
// stating it 22 times per screen wrapped the cell onto two lines, doubled
// the row height and pushed the version pill into the Updates badge.
//
// It is stated once here instead, on the column header, and only when the
// reference really is fleet-derived. Deliberately a Popover rather than a
// title/hover tooltip: it must be reachable by keyboard and by touch, not
// just by a mouse pointer that happens to rest on it.

export const AGENT_FLEET_NOTE_TITLE = "Compared against this fleet";

export const AGENT_FLEET_NOTE_BODY =
  "No published release manifest was available, so each site is compared against the newest agent version this fleet has reported. That is the normal state on a self-hosted install. A site shown as current is current within this fleet and may still be behind a newer published build.";

export const AGENT_FLEET_NOTE_LABEL = "About the Agent column comparison";

// GH #322: the fleet Agent column previously had no signal at all for
// whether the REFERENCE it classifies every site against might itself be
// stale (on a self-hosted install, that reference is only as fresh as the
// last successful run of the upstream release mirror). This popover is
// where that signal lives: see agent-reference-check.ts for the exact copy
// per state.
//
// This is an INFORMATION surface only. Triggering an immediate mirror check
// is a superadmin, install-level operation (POST /api/v1/admin/agent-mirror
// /check), and lives in the admin console
// (routes/_authed/admin/agent-mirror.tsx), not here: a superadmin cannot
// open the tenant-scoped Sites page at all (see routes/_authed.tsx's
// isSuperadminAllowedPath guard), so a trigger on this page would be
// reachable by no one who could ever use it.
//
// Deliberately does NOT change the per-site "Current" badge. That badge's
// claim ("this site matches the reference") stays true regardless of the
// reference's age; what is uncertain is the reference itself, which is one
// fact shared by every row, not a per-site fact. Escalating the shared
// header affordance (icon + tint) rather than tinting every row is the
// same reasoning that already moved the fleet-derived caveat here in the
// first place.

export interface AgentColumnFleetNoteProps {
  /**
   * Where the freshness comparison's reference version came from (see
   * FleetAgentVersions.reference_source). Renders the fleet-derived note
   * unless this is "fleet"; independent of `referenceCheck` below (both may
   * render at once, since a fleet-derived reference and a stale mirror are
   * different facts that can both be true).
   */
  referenceSource?: FleetAgentVersions["reference_source"];
  /**
   * Freshness of the upstream agent-release mirror (GH #322;
   * FleetAgentVersions.agent_mirror). Omit while the rollup has not loaded:
   * the popover then says nothing about mirror freshness at all, never a
   * guessed or empty value (see describeReferenceCheck's own doc for the
   * full state table, including why a hosted/direct reference renders
   * nothing here).
   */
  referenceCheck?: AgentMirrorStatus;
  className?: string;
}

/** Info affordance for the Sites table's Agent column header. */
export function AgentColumnFleetNote({
  referenceSource,
  referenceCheck,
  className,
}: AgentColumnFleetNoteProps) {
  const showFleetNote = referenceSource === "fleet";
  const checkMessage = describeReferenceCheck(referenceCheck, referenceSource);
  if (!showFleetNote && !checkMessage) return null;

  const warn = checkMessage?.tone === "warn";
  const Icon = warn ? AlertTriangle : Info;
  // The aria-label carries the headline of a warn-tier state too, so a
  // screen-reader user gets the escalation even though the icon swap alone
  // is a visual-only signal.
  const ariaLabel =
    warn && checkMessage
      ? `${AGENT_FLEET_NOTE_LABEL}. ${checkMessage.title}.`
      : AGENT_FLEET_NOTE_LABEL;

  return (
    <Popover>
      <PopoverTrigger asChild>
        <button
          type="button"
          aria-label={ariaLabel}
          onClick={(e) => e.stopPropagation()}
          className={cn(
            "inline-flex size-5 shrink-0 items-center justify-center rounded-sm transition-colors",
            warn
              ? "text-[var(--color-warning)] hover:bg-[var(--color-warning-subtle)]"
              : "text-[var(--color-muted-foreground)] hover:bg-[var(--color-accent)] hover:text-[var(--color-accent-foreground)]",
            "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)] focus-visible:ring-offset-1",
            className,
          )}
        >
          <Icon aria-hidden="true" className="size-3.5" />
        </button>
      </PopoverTrigger>
      <PopoverContent
        align="start"
        className="w-80 space-y-3 p-3 text-xs normal-case tracking-normal"
      >
        {showFleetNote ? (
          <div className="space-y-1">
            <p className="font-medium text-[var(--color-foreground)]">
              {AGENT_FLEET_NOTE_TITLE}
            </p>
            <p className="leading-relaxed text-[var(--color-muted-foreground)]">
              {AGENT_FLEET_NOTE_BODY}
            </p>
          </div>
        ) : null}

        {checkMessage ? (
          <div
            className={cn(
              "space-y-1",
              showFleetNote && "border-t border-[var(--color-border)] pt-3",
            )}
          >
            {checkMessage.lead ? (
              <p className="leading-relaxed text-[var(--color-muted-foreground)]">
                {checkMessage.lead}
              </p>
            ) : null}
            <p
              className={cn(
                "font-medium",
                warn
                  ? "text-[var(--color-warning-foreground)]"
                  : "text-[var(--color-foreground)]",
              )}
            >
              {checkMessage.title}
            </p>
            <p className="leading-relaxed text-[var(--color-muted-foreground)]">
              {checkMessage.body}
            </p>
          </div>
        ) : null}
      </PopoverContent>
    </Popover>
  );
}
