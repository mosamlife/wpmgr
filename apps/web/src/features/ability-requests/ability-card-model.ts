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
};

export function notSentText(reason: string | null | undefined): string {
  if (!reason) return "the reason was not recorded";
  return Object.prototype.hasOwnProperty.call(NOT_SENT_TEXT, reason)
    ? NOT_SENT_TEXT[reason]!
    : "WPMgr could not send it";
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
  /** Site-supplied text, shown after "The site said:" as a text node. */
  readonly siteSaid?: string;
}

/** The undo window is open: done, not yet undone or tried, and still in time. */
export function undoOpen(r: AbilityRequest, now: Date): boolean {
  if (r.state !== "done" || r.outcome !== "created") return false;
  if (r.undo_state !== "available" || r.trashed === true) return false;
  if (!r.undo_available_until) return false;
  const until = new Date(r.undo_available_until).getTime();
  return !Number.isNaN(until) && until > now.getTime();
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
    case "outcome_unknown":
      return { kind: "unknown_outcome", text: "WPMgr is checking whether the draft was created." };
    case "done": {
      if (r.undo_state === "undone" || r.trashed === true) {
        return { kind: "undone", text: "Moved to the trash." };
      }
      if (r.undo_state === "in_progress") {
        return { kind: "running", text: "WPMgr is moving the draft to the trash." };
      }
      if (r.undo_state === "refused_published") {
        return {
          kind: "undo_refused",
          text: "The draft has been published since, so WPMgr left it alone. Remove it in WordPress if you do not want it.",
        };
      }
      if (r.undo_state === "refused_conflict") {
        return {
          kind: "undo_refused",
          text: "The draft was edited since, so WPMgr left it alone. Remove it in WordPress if you do not want it.",
        };
      }
      if (r.undo_state === "failed") {
        return {
          kind: "undo_failed",
          text: "WPMgr could not move the draft to the trash. Remove it in WordPress if you do not want it.",
        };
      }
      return { kind: "done", text: "Draft created." };
    }
    case "failed": {
      const said = r.outcome_code ?? undefined;
      const extra = said !== undefined && said.trim() !== "" ? { siteSaid: said } : {};
      if (r.outcome === "verify_mismatch") {
        return {
          kind: "failed",
          text: `The site's ${noun} did not match what you approved, so WPMgr cannot vouch for it. Check the site's drafts.`,
          ...extra,
        };
      }
      if (r.outcome === "refused") {
        return {
          kind: "failed",
          text: `The site refused to create the draft ${noun}. Nothing was created.`,
          ...extra,
        };
      }
      return {
        kind: "failed",
        text: `The draft ${noun} was not created because something went wrong on the site.`,
        ...extra,
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
