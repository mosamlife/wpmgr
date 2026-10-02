import { z } from "zod";
import type { AbilityRequest } from "@wpmgr/api";

// Pure logic for the AI page-creation approval card (engine slice E2). Every
// string the AI or the site supplied (title, outline text, site and connection
// names) is carried to the caller as plain data and rendered as a text node;
// nothing here builds markup or an href from one.

// The shape the control plane accepts for wpmgr/page-create
// (apps/api/internal/mcp/ability_write.go validatePageCreateInput): heading
// levels 2 to 4, up to 50 list items, up to 200 nodes.
const outlineNodeSchema = z.discriminatedUnion("type", [
  z.object({ type: z.literal("heading"), level: z.number().int(), text: z.string() }),
  z.object({ type: z.literal("paragraph"), text: z.string() }),
  z.object({ type: z.literal("list"), ordered: z.boolean(), items: z.array(z.string()) }),
]);

const pageInputSchema = z.object({
  post_type: z.string(),
  editor: z.string(),
  title: z.string(),
  outline: z.array(outlineNodeSchema),
});

export type OutlineNode = z.infer<typeof outlineNodeSchema>;
export interface PagePreview {
  readonly title: string;
  readonly outline: readonly OutlineNode[];
}

/** Parses the exact AI input. Null when it is not the shape the card can show in full. */
export function parsePagePreview(inputJson: string): PagePreview | null {
  let raw: unknown;
  try {
    raw = JSON.parse(inputJson);
  } catch {
    return null;
  }
  const parsed = pageInputSchema.safeParse(raw);
  if (!parsed.success) return null;
  return { title: parsed.data.title, outline: parsed.data.outline };
}

export function isPostRequest(r: AbilityRequest): boolean {
  return r.post_type === "post";
}

/** "Create a draft page · Shop". Site name is the site's own label, rendered as text. */
export function abilityCardTitle(r: AbilityRequest): string {
  return `Create a draft ${isPostRequest(r) ? "post" : "page"} · ${r.site_label}`;
}

export function editorName(editor: string | null | undefined): string {
  if (editor === "wordpress_blocks") return "WordPress block editor";
  if (editor === "wordpress_classic") return "Classic editor";
  return "Editor not recorded";
}

export const NOTHING_PUBLISHED = "Nothing is published. Undo moves the draft to the trash.";
export const GAVE_UP_COPY =
  "WPMgr could not confirm whether the draft was created. Check the site's drafts.";
export const UNDO_RETRY_COPY = "The site didn't answer. Try Undo again.";
export const CHANGED_COPY = "This request changed. Ask the AI again.";
export const NOT_SHOWABLE_COPY =
  "WPMgr cannot show this request in full, so it cannot be approved here. Decline it and ask the AI again.";

const NOT_SENT_TEXT: Record<string, string> = {
  grant_inactive: "the connection was revoked or expired",
  assistant_paused: "the AI assistant is paused",
  organisation_deleted: "the organisation is being deleted",
  capability_not_held: "the connection no longer holds this permission",
  site_absent: "the site left the connection's scope",
  forbidden_by_context: "your AI rules forbid it",
  agent_outdated: "the WPMgr plugin on the site is too old",
  dispatch_deadline_passed: "it was not started within 15 minutes of approval",
  transport_pre_send: "WPMgr could not reach the site",
  entry_changed: "the AI tool changed after you approved it",
  entry_disabled: "the AI tool was switched off after you approved it",
  route_changed: "WPMgr's rules for this change were updated after you approved; ask the AI again",
  route_disabled: "this kind of change is turned off",
};

export function notSentText(reason: string | null | undefined): string {
  if (!reason) return "the reason was not recorded";
  return Object.prototype.hasOwnProperty.call(NOT_SENT_TEXT, reason)
    ? NOT_SENT_TEXT[reason]!
    : "WPMgr could not send it";
}

const REFUSAL_ADVICE: Record<string, string> = {
  content_editing_not_enabled: "Turn AI page creation on again from this tab.",
  principal_capabilities_drifted: "Turn AI page creation on again from this tab.",
  principal_missing: "Turn AI page creation on again from this tab.",
  principal_create_failed: "Turn AI page creation on again from this tab.",
  principal_login_taken: "Turn AI page creation on again from this tab.",
  editor_unavailable: "This site's editor isn't available.",
  preview_changed: "The site changed since you approved. Ask the AI to try again.",
  entry_approval_invalid: "The site changed since you approved. Ask the AI to try again.",
  integration_entry_changed: "The site changed since you approved. Ask the AI to try again.",
  sanitiser_changed_new_content:
    "This site changes page text on save in a way WPMgr can't approve. Ask the AI to simplify the text.",
  create_content_invalid: "The AI's page outline wasn't valid. Ask it to try again.",
  bad_input: "The AI's page outline wasn't valid. Ask it to try again.",
  disabled_on_site: "AI page creation is turned off for this site.",
  ability_disabled: "AI page creation is turned off for this site.",
  post_content_would_change:
    "This page contains content the WPMgr user can't save, so WPMgr won't change its title. Edit it in WordPress.",
  post_not_editable: "The WPMgr user is not allowed to edit this page.",
  post_touched: "The page was edited on the site after you approved. Ask the AI again.",
  sanitiser_changed_value:
    "This site changes text on save in a way WPMgr can't approve. Ask the AI to simplify the text.",
  side_effect_detected: "The site did something unexpected while saving, so WPMgr stopped. Check the page in WordPress.",
  route_changed: "WPMgr's rules for this change were updated after you approved. Ask the AI again.",
  route_disabled: "This kind of change is turned off.",
  route_entry_changed: "WPMgr's rules for this change were updated after you approved. Ask the AI again.",
  route_not_reviewed: "This kind of change has not been reviewed for this site yet.",
  route_namespace_refused: "WPMgr does not allow this kind of change.",
  route_key_forbidden: "The AI asked to change a field WPMgr does not allow.",
  route_key_not_allowed: "The AI asked to change a field WPMgr does not allow.",
  route_param_invalid: "The AI's request wasn't valid. Ask it to try again.",
  route_wp_version_unsupported: "This site's WordPress version does not support this change.",
  rest_not_published: "The site did not accept this change.",
  rest_intercepted: "A plugin on the site intercepted the change, so WPMgr stopped.",
  rest_handler_not_core: "A plugin on the site replaced the handler for this change, so WPMgr stopped.",
  rest_error: "The site reported an error. Check the page in WordPress.",
  post_scheduled: "This page is scheduled; change its title in WordPress.",
};

/** Plain advice for a refusal or failure code, or null when the code is not one an operator can act on. */
export function refusalAdvice(code: string | null | undefined): string | null {
  if (!code) return null;
  return Object.prototype.hasOwnProperty.call(REFUSAL_ADVICE, code) ? REFUSAL_ADVICE[code]! : null;
}

export function clockTime(iso: string | null | undefined): string {
  if (!iso) return "an unrecorded time";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "an unreadable time";
  return d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
}

export type AbilityStatusKind =
  | "pending"
  | "approved"
  | "running"
  | "unknown_outcome"
  | "done"
  | "undone"
  | "undo_refused"
  | "undo_failed"
  | "failed"
  | "not_sent"
  | "declined"
  | "withdrawn"
  | "expired"
  | "unrecognised";

export interface AbilityStatus {
  readonly kind: AbilityStatusKind;
  readonly text: string;
  /** A draft may exist on the site: the card offers the edit link. */
  readonly draftMayExist?: boolean;
}

function hasCreatedPost(r: AbilityRequest): boolean {
  return typeof r.created_post_id === "number" && Number.isInteger(r.created_post_id) && r.created_post_id > 0;
}

/**
 * What a recovery undo did, read from undo_state. The server sets only
 * undo_state ("undone", "in_progress", "refused_conflict", "refused_published",
 * "failed") and leaves `trashed` as it was, on done, failed and
 * outcome_unknown rows alike. Null when no undo result applies.
 */
function undoStatus(r: AbilityRequest, trashedCounts: boolean): AbilityStatus | null {
  if (r.undo_state === "undone" || (trashedCounts && r.trashed === true)) {
    return { kind: "undone", text: "Moved to the trash." };
  }
  if (r.undo_state === "in_progress") {
    return { kind: "running", text: "WPMgr is moving the draft to the trash." };
  }
  if (r.undo_state === "refused_published") {
    return {
      kind: "undo_refused",
      text: "The draft has been published since, so WPMgr left it alone. Remove it in WordPress if you do not want it.",
      draftMayExist: true,
    };
  }
  if (r.undo_state === "refused_conflict") {
    return {
      kind: "undo_refused",
      text: "The draft was edited since, so WPMgr left it alone. Remove it in WordPress if you do not want it.",
      draftMayExist: true,
    };
  }
  if (r.undo_state === "failed") {
    return {
      kind: "undo_failed",
      text: "WPMgr could not move the draft to the trash. Remove it in WordPress if you do not want it.",
      draftMayExist: true,
    };
  }
  return null;
}

export function abilityStatus(r: AbilityRequest): AbilityStatus {
  const noun = isPostRequest(r) ? "post" : "page";
  switch (r.state) {
    case "pending":
      return { kind: "pending", text: "Waiting for your decision." };
    case "approved":
      return {
        kind: "approved",
        text: `Approved at ${clockTime(r.decided_at)}. Not started yet. WPMgr sends it to the site shortly.`,
      };
    case "dispatched":
      return { kind: "running", text: `WPMgr is creating the draft ${noun}.` };
    case "outcome_unknown": {
      const undone = undoStatus(r, false);
      if (undone) return undone;
      if (r.resolve_gave_up) {
        return {
          kind: "unknown_outcome",
          text: GAVE_UP_COPY,
          // The edit link is offered when the post id is known (the card
          // still needs a site address to build it).
          draftMayExist: hasCreatedPost(r) && r.trashed !== true,
        };
      }
      if (hasCreatedPost(r) && r.trashed !== true) {
        return {
          kind: "unknown_outcome",
          text: `WPMgr could not confirm the result. A draft ${noun} may exist on the site. Check it, and delete it there if you do not want it.`,
          draftMayExist: true,
        };
      }
      return { kind: "unknown_outcome", text: "WPMgr is checking whether the draft was created." };
    }
    case "done": {
      return undoStatus(r, true) ?? { kind: "done", text: "Draft created." };
    }
    case "failed": {
      const undone = undoStatus(r, false);
      if (undone) return undone;
      if (hasCreatedPost(r)) {
        if (r.trashed === true) {
          return {
            kind: "failed",
            text: `Something went wrong on the site, and a draft ${noun} was created and then moved to the trash. Nothing is left to clean up unless you want to check the trash in WordPress.`,
          };
        }
        const why =
          r.outcome === "verify_mismatch"
            ? `The site's ${noun} did not match what you approved, so WPMgr cannot vouch for it.`
            : `Something went wrong on the site partway through.`;
        return {
          kind: "failed",
          text: r.undo_offered
            ? `${why} A draft ${noun} may exist on the site. Move it to the trash if you do not want it.`
            : `${why} A draft ${noun} may exist on the site. Check it, and delete it there if you do not want it.`,
          draftMayExist: true,
        };
      }
      if (r.outcome === "verify_mismatch") {
        return {
          kind: "failed",
          text: `The site's ${noun} did not match what you approved, so WPMgr cannot vouch for it. Check the site's drafts.`,
        };
      }
      const advice = refusalAdvice(r.outcome_code);
      if (r.outcome === "refused") {
        return {
          kind: "failed",
          text: `The site refused to create the draft ${noun}. Nothing was created.${advice ? ` ${advice}` : ""}`,
        };
      }
      return {
        kind: "failed",
        text: `The draft ${noun} was not created because something went wrong on the site.${advice ? ` ${advice}` : ""}`,
      };
    }
    case "not_sent":
      return {
        kind: "not_sent",
        text: `Nothing was sent: ${notSentText(r.not_sent_reason)}. Nothing was created.`,
      };
    case "declined":
      return { kind: "declined", text: `Declined at ${clockTime(r.decided_at)}. Nothing was created.` };
    case "withdrawn":
      return {
        kind: "withdrawn",
        text: "Withdrawn because its connection was revoked. Nothing was created.",
      };
    case "expired":
      return {
        kind: "expired",
        text: `Closed unanswered at ${clockTime(r.expires_at)}. Nothing was created.`,
      };
    default:
      return { kind: "unrecognised", text: "This request is in a state this page does not know." };
  }
}

/**
 * Link to edit the created draft in WordPress. Null unless the site URL is a
 * plain http(s) URL and the id is a positive integer.
 */
export function editDraftHref(
  siteUrl: string | null | undefined,
  postId: number | null | undefined,
): string | null {
  if (!siteUrl || postId == null || !Number.isInteger(postId) || postId < 1) return null;
  try {
    const base = new URL(siteUrl.endsWith("/") ? siteUrl : `${siteUrl}/`);
    if (base.protocol !== "https:" && base.protocol !== "http:") return null;
    const u = new URL("wp-admin/post.php", base);
    u.searchParams.set("post", String(postId));
    u.searchParams.set("action", "edit");
    return u.toString();
  } catch {
    return null;
  }
}

export function isPending(r: AbilityRequest): boolean {
  return r.state === "pending";
}
