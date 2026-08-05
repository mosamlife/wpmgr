import { AlertTriangle, Info, RefreshCw } from "lucide-react";
import type { AgentMirrorStatus, FleetAgentVersions } from "@wpmgr/api";

import { Button } from "@/components/ui/button";
import {
  Popover,
  PopoverContent,
  PopoverTrigger,
} from "@/components/ui/popover";
// The manual trigger's mutation, reused verbatim from the admin console page
// that already renders it (routes/_authed/admin/agent-mirror.tsx). It lives in
// features/admin because the endpoint it calls is POST /api/v1/admin/agent-
// mirror/check, and it is shared rather than reimplemented so that the two
// surfaces cannot drift into two vocabularies for the same three outcomes: 202
// queued is a success toast, 409 already running and 429 rate limited are INFO
// (being skipped by the 30 minute spacing is the system working as designed,
// not a failure), and everything else is a real error.
import { useCheckAgentMirrorNow } from "@/features/admin/use-admin-agent-mirror";
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
// It also carries the manual "Check now" trigger, but only for a viewer the
// control plane says may actually use it (agent_mirror.can_check_now).
//
// This popover used to be an information surface only, and the reason given
// here was that triggering a check is a superadmin operation while a
// superadmin cannot open the tenant-scoped Sites page at all (see
// routes/_authed.tsx's isSuperadminAllowedPath guard), so a button here would
// have been reachable by nobody who could use it. THAT REASONING NO LONGER
// HOLDS for the case it now serves. The endpoint admits the owner of the only
// live organisation on an install as well as a superadmin, and an ordinary
// owner is never redirected off this page. The reporter asked for it exactly
// here, and they are right: reading "may be stale, last confirmed 14h ago" is
// the moment an operator wants to act, so the action sits with the
// information rather than a navigation away from it.
//
// Two things that did NOT change. A superadmin still cannot see this button,
// because they still cannot open this page, so the admin console remains
// their route to the same action. And on any install with more than one live
// organisation can_check_now is false for every non-superadmin, so nothing
// appears here on the hosted service. The visibility rule is never guessed
// from a role in the browser: it is the server's own answer, computed by the
// same code that gates the endpoint, so this button cannot be shown to
// someone who would receive a 403.
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
   *
   * Its `can_check_now` is what reveals the "Check now" action below. That
   * boolean is the control plane's own answer for THIS viewer, so it is read
   * verbatim and never combined with a locally guessed role.
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
  // === true, not a truthiness check: the generated type is a plain boolean,
  // but an older control plane that predates the field sends nothing at all,
  // and "absent" must read as "not permitted" rather than reveal a button
  // whose endpoint would refuse it.
  const canCheckNow = referenceCheck?.can_check_now === true;
  // canCheckNow is part of the condition rather than assumed to imply
  // checkMessage. The server only sets it while the mirror is enabled, which
  // is also the condition under which describeReferenceCheck always returns a
  // message, so the two agree today; keeping it explicit means a future change
  // to either one cannot silently hide the action.
  if (!showFleetNote && !checkMessage && !canCheckNow) return null;

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

        {checkMessage || canCheckNow ? (
          <div
            className={cn(
              "space-y-1",
              showFleetNote && "border-t border-[var(--color-border)] pt-3",
            )}
          >
            {checkMessage?.lead ? (
              <p className="leading-relaxed text-[var(--color-muted-foreground)]">
                {checkMessage.lead}
              </p>
            ) : null}
            {/*
              warning-SUBTLE-FG, not warning-foreground (GH #322).

              --warning-foreground is the text colour for content sitting ON a
              --warning background, so it is near-black in BOTH themes and its
              dark override is darker still (L 20% light, L 15% dark). Used
              here, on the popover's own surface, it rendered this heading
              almost invisible in dark mode while the body text below it was
              fine.

              --warning-subtle-fg is the token for warning-tinted text on an
              ordinary surface, and it inverts properly (L 38% light, L 86%
              dark). It is what the Core Web Vitals threshold labels already
              use for the same job.
            */}
            {checkMessage ? (
              <>
                <p
                  className={cn(
                    "font-medium",
                    warn
                      ? "text-[var(--color-warning-subtle-fg)]"
                      : "text-[var(--color-foreground)]",
                  )}
                >
                  {checkMessage.title}
                </p>
                <p className="leading-relaxed text-[var(--color-muted-foreground)]">
                  {checkMessage.body}
                </p>
              </>
            ) : null}
            {canCheckNow ? <CheckNowAction /> : null}
          </div>
        ) : null}
      </PopoverContent>
    </Popover>
  );
}

/**
 * The manual trigger, rendered directly under the freshness text so the action
 * sits where the information that prompts it already is.
 *
 * Its own component only so the mutation hook is not instantiated on every
 * render of a popover that will never show it: the vast majority of viewers,
 * on every hosted install and on every self-hosted install with more than one
 * organisation, have can_check_now false.
 *
 * All colours come from Button's own variant tokens, which already carry
 * light and dark values; nothing here introduces a new colour.
 */
function CheckNowAction() {
  const check = useCheckAgentMirrorNow();

  return (
    <div className="pt-1">
      <Button
        type="button"
        variant="outline"
        size="sm"
        disabled={check.isPending}
        // The popover lives inside the Sites table header, whose cells have
        // their own click handling; the trigger icon stops propagation for the
        // same reason.
        onClick={(e) => {
          e.stopPropagation();
          check.mutate();
        }}
        // Starts with the visible label on purpose. An accessible name that
        // does not contain the words on the button breaks voice control ("click
        // Check now") and WCAG 2.5.3; the rest is the context the two words
        // alone do not carry, and it stays put while the label swaps to
        // "Checking..." so the control keeps one stable name.
        aria-label="Check now for a newer agent release upstream"
      >
        <RefreshCw
          aria-hidden="true"
          className={cn("mr-1.5 size-3.5", check.isPending && "animate-spin")}
        />
        {check.isPending ? "Checking..." : "Check now"}
      </Button>
      {/*
        Deliberately not a promise of a result. The endpoint answers 202
        "queued", the run has not happened yet, and the outcome lands in this
        very popover only once the fleet rollup refetches. Saying so here stops
        the button from reading as "confirmed just now", which is the exact
        class of overclaim this whole feature exists to remove.
      */}
      <p className="pt-1.5 leading-relaxed text-[var(--color-muted-foreground)]">
        Queues one immediate check instead of waiting for the next scheduled
        run. The result appears here once this view refreshes.
      </p>
    </div>
  );
}
