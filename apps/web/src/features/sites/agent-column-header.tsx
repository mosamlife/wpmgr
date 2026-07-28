import { Info } from "lucide-react";
import type { FleetAgentVersions } from "@wpmgr/api";

import {
  Popover,
  PopoverContent,
  PopoverTrigger,
} from "@/components/ui/popover";
import { cn } from "@/lib/utils";

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

export interface AgentColumnFleetNoteProps {
  /**
   * Where the freshness comparison's reference version came from (see
   * FleetAgentVersions.reference_source). Renders nothing unless this is
   * "fleet": a published reference needs no caveat, and an absent one means
   * the rollup has not loaded so there is nothing to explain yet.
   */
  referenceSource?: FleetAgentVersions["reference_source"];
  className?: string;
}

/** Info affordance for the Sites table's Agent column header. */
export function AgentColumnFleetNote({
  referenceSource,
  className,
}: AgentColumnFleetNoteProps) {
  if (referenceSource !== "fleet") return null;
  return (
    <Popover>
      <PopoverTrigger asChild>
        <button
          type="button"
          aria-label={AGENT_FLEET_NOTE_LABEL}
          onClick={(e) => e.stopPropagation()}
          className={cn(
            "inline-flex size-5 shrink-0 items-center justify-center rounded-sm",
            "text-[var(--color-muted-foreground)] transition-colors",
            "hover:bg-[var(--color-accent)] hover:text-[var(--color-accent-foreground)]",
            "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)] focus-visible:ring-offset-1",
            className,
          )}
        >
          <Info aria-hidden="true" className="size-3.5" />
        </button>
      </PopoverTrigger>
      <PopoverContent
        align="start"
        className="w-72 space-y-1 p-3 text-xs normal-case tracking-normal"
      >
        <p className="font-medium text-[var(--color-foreground)]">
          {AGENT_FLEET_NOTE_TITLE}
        </p>
        <p className="leading-relaxed text-[var(--color-muted-foreground)]">
          {AGENT_FLEET_NOTE_BODY}
        </p>
      </PopoverContent>
    </Popover>
  );
}
