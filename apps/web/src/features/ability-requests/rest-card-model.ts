import { z } from "zod";
import type { AbilityRequest } from "@wpmgr/api";

import { clockTime, notSentText, refusalAdvice, type AbilityStatus } from "./ability-card-model";

// Pure logic for the structured "AI changes a site field" card (engine slice
// E3). Every string below is plain data for a text node: the AI's requested
// value (`after`) and the site's own words (`from_the_site.*`) are never turned
// into markup, an href or a title attribute. The server cleans and caps them.

export const REST_WRITE_ABILITY = "wpmgr/rest-write";

const factsSchema = z.object({
  route_id: z.string(),
  effect_copy: z.string(),
  route_title: z.string(),
  method: z.string(),
  target: z.object({
    id: z.number(),
    post_type: z.string(),
    from_the_site: z.object({ status: z.string(), title_before: z.string() }),
  }),
  changes: z.array(
    z.object({
      key: z.string(),
      label: z.string(),
      after: z.string(),
      from_the_site: z.object({ before: z.string() }),
    }),
  ),
  live: z.boolean(),
  effect_label: z.string(),
  undo: z.string(),
  undo_exact: z.boolean(),
  undo_note: z.string().nullish(),
});

export type RestCardFacts = z.infer<typeof factsSchema>;

export function isRestWrite(r: AbilityRequest): boolean {
  return r.ability_name === REST_WRITE_ABILITY;
}

/** Validates the row's card_facts as defence; null when it is not the shape the card can show in full. */
export function parseRestCardFacts(r: AbilityRequest): RestCardFacts | null {
  const parsed = factsSchema.safeParse(r.card_facts);
  return parsed.success ? parsed.data : null;
}

/** The effect line is a warning only for the live label the server wrote. */
export function isPublishedImmediately(f: RestCardFacts): boolean {
  return f.live;
}

export const REST_NOT_SHOWABLE_COPY =
  "WPMgr cannot show this request in full, so it cannot be approved here. Decline it and ask the AI again.";

export function restCardTitle(f: RestCardFacts | null, siteLabel: string): string {
  return f ? `${f.route_title} · ${siteLabel}` : `Change a site field · ${siteLabel}`;
}

function undoStatusText(r: AbilityRequest): AbilityStatus | null {
  switch (r.undo_state) {
    case "undone":
      return { kind: "undone", text: "Put back the way it was." };
    case "in_progress":
      return { kind: "running", text: "WPMgr is putting the old text back." };
    case "refused_conflict":
    case "refused_published":
      return {
        kind: "undo_refused",
        text: "The page was edited since, so WPMgr left it alone. Edit it in WordPress.",
      };
    case "failed":
      return { kind: "undo_failed", text: "WPMgr could not put the old text back. Edit it in WordPress." };
    default:
      return null;
  }
}

export function restWriteStatus(r: AbilityRequest): AbilityStatus {
  switch (r.state) {
    case "pending":
      return { kind: "pending", text: "Waiting for your decision." };
    case "approved":
      return {
        kind: "approved",
        text: `Approved at ${clockTime(r.decided_at)}. Not started yet. WPMgr sends it to the site shortly.`,
      };
    case "dispatched":
      return { kind: "running", text: "WPMgr is making the change on the site." };
    case "outcome_unknown":
      return (
        undoStatusText(r) ?? {
          kind: "unknown_outcome",
          text: "WPMgr could not confirm the result. Check the page in WordPress.",
        }
      );
    case "done":
      return undoStatusText(r) ?? { kind: "done", text: "Changed." };
    case "failed": {
      const undone = undoStatusText(r);
      if (undone) return undone;
      const advice = refusalAdvice(r.outcome_code);
      const lead =
        r.outcome === "refused"
          ? "The site refused the change. Nothing was changed."
          : "The change was not made because something went wrong on the site.";
      return { kind: "failed", text: advice ? `${lead} ${advice}` : lead };
    }
    case "not_sent":
      return {
        kind: "not_sent",
        text: `Nothing was sent: ${notSentText(r.not_sent_reason)}. Nothing was changed.`,
      };
    case "declined":
      return { kind: "declined", text: `Declined at ${clockTime(r.decided_at)}. Nothing was changed.` };
    case "withdrawn":
      return { kind: "withdrawn", text: "Withdrawn because its connection was revoked. Nothing was changed." };
    case "expired":
      return { kind: "expired", text: `Closed unanswered at ${clockTime(r.expires_at)}. Nothing was changed.` };
    default:
      return { kind: "unrecognised", text: "This request is in a state this page does not know." };
  }
}
