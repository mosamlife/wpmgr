import type { AbilityRequest } from "@wpmgr/api";

import {
  OUTCOME_UNKNOWN_LINE,
  ranAutomatically,
  settingNotSentLine,
} from "@/features/ai-trust/ai-trust-copy";

import { ELEMENTOR_EDITOR, type PageBuilderFacts } from "./outline-model";

// Pure logic for the AI page-creation approval card (engine slice E2). Every
// string the AI or the site supplied (title, outline text, site and connection
// names) is carried to the caller as plain data and rendered as a text node;
// nothing here builds markup or an href from one. The outline itself is parsed
// by outline-model.ts.

export function isPostRequest(r: AbilityRequest): boolean {
  return r.post_type === "post";
}

/** A page Elementor builds: the editor the request recorded is builder:elementor. */
export function isElementorRequest(r: AbilityRequest): boolean {
  return r.editor === ELEMENTOR_EDITOR;
}

/**
 * "Create a draft page · Shop", or "Create a draft page in Elementor · Shop".
 * Site name is the site's own label, rendered as text.
 */
export function abilityCardTitle(r: AbilityRequest): string {
  const noun = isPostRequest(r) ? "post" : "page";
  return `Create a draft ${noun}${isElementorRequest(r) ? " in Elementor" : ""} · ${r.site_label}`;
}

export function editorName(editor: string | null | undefined): string {
  if (editor === "wordpress_blocks") return "WordPress block editor";
  if (editor === "wordpress_classic") return "Classic editor";
  if (editor === ELEMENTOR_EDITOR) return "Elementor";
  return "Editor not recorded";
}

/** "Elementor 3.35.9 · classic widgets, containers", or "Elementor 4.3.4 · Atomic editor". The version is the site's. */
export function elementorEditorLine(b: PageBuilderFacts): string {
  const how = b.format === "atomic" ? "Atomic editor" : `classic widgets, ${b.layout}`;
  return `Elementor ${b.version} · ${how}`;
}

export interface CardRow {
  readonly label: string;
  readonly lines: readonly string[];
}

/**
 * The rows under the outline of a page Elementor builds. The first names what
 * Elementor builds the page from, so a page laid out in sections says so.
 */
export function elementorCardRows(b: PageBuilderFacts): readonly CardRow[] {
  const parts =
    b.format === "classic" && b.layout === "sections" ? "sections, columns and widgets" : "containers and widgets";
  return [
    {
      label: "Elementor",
      lines: [
        `Built from Elementor's own ${parts}, styled by this site's Elementor settings.`,
        "WPMgr does not change Elementor's site-wide styles, templates, header or footer.",
      ],
    },
    {
      label: "Checks",
      lines: [
        "WPMgr checks afterwards that Elementor saved exactly this layout and that nothing outside the page changed. If not, it moves the draft to the trash.",
      ],
    },
    { label: "Also happens", lines: ["Elementor rebuilds this page's style file the first time it is viewed."] },
    { label: "Effect", lines: ["Draft only. Nobody sees it until a person publishes it."] },
    { label: "Undo", lines: ["Undo moves the draft to the trash."] },
  ];
}

export const NOTHING_PUBLISHED = "Nothing is published. Undo moves the draft to the trash.";
/** A draft's undo refused because someone edited it in WordPress after WPMgr's own changes. */
export const CHAIN_TOUCHED_COPY =
  "WPMgr won't move this draft to the trash: someone edited it in WordPress after WPMgr's changes.";
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
  preview_changed: "The site or one of the chosen images changed since you approved. Ask the AI to try again.",
  entry_approval_invalid: "The site changed since you approved. Ask the AI to try again.",
  integration_entry_changed: "The site changed since you approved. Ask the AI to try again.",
  sanitiser_changed_new_content:
    "This site changes page content when saving it, often because of a plugin, in a way WPMgr can't approve. Ask the AI to simplify the page.",
  create_content_invalid: "The AI's page outline wasn't valid. Ask it to try again.",
  bad_input: "The AI's page outline wasn't valid. Ask it to try again.",
  // Page layouts: the codes the page-create builder and the ability_run command
  // refuse with (class-page-create-builder.php, class-ability-run-command.php).
  layout_invalid: "The AI's page layout wasn't valid. Ask it to try again.",
  link_invalid: "The AI's page layout wasn't valid. Ask it to try again.",
  image_not_available:
    "An image the AI chose is no longer in the media library, or WPMgr may not use it. Ask the AI to pick another image.",
  image_url_unusable:
    "WordPress gave an unusual address for one of the images, so WPMgr stopped. Ask the AI to pick another image.",
  layout_needs_block_editor:
    "This site uses the classic editor, which has no columns, sections, buttons or spacing. Ask the AI for a simpler page.",
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

/**
 * An approved row that has not been sent yet. A row the site's setting
 * approved was not decided by any person, so it never says "Approved at".
 */
export function approvedNotStartedText(r: AbilityRequest): string {
  const lead = ranAutomatically(r.approval)
    ? `Allowed by this site's setting at ${clockTime(r.decided_at)}.`
    : `Approved at ${clockTime(r.decided_at)}.`;
  return `${lead} Not started yet. WPMgr sends it to the site shortly.`;
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
function undoStatus(r: AbilityRequest, trashedCounts: boolean, laterEdits: number): AbilityStatus | null {
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
      text:
        laterEdits > 0
          ? CHAIN_TOUCHED_COPY
          : "The draft was edited since, so WPMgr left it alone. Remove it in WordPress if you do not want it.",
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

/**
 * A failure whose end state the record settles: the site refused before it
 * changed anything, or what it changed was put back or moved to the trash.
 * Those keep their own words. Every other failure leaves the result open.
 */
function failureEndStateKnown(r: AbilityRequest): boolean {
  return r.outcome === "refused" || r.trashed === true || r.restored === true;
}

/**
 * A change the site's setting approved was decided by no person, so a status
 * that leans on "what you approved" cannot stand. Where WPMgr cannot vouch for
 * the result, because it is unresolved or the failure left the end state open,
 * the card says so in one plain sentence and names where to read the answer
 * (design §8.9). The kind is unchanged, so a failure keeps its red border and
 * its place under "Failed or result unknown". A person's own approval is
 * untouched, and so is a failure whose end state is known.
 */
export function automaticStatus(r: AbilityRequest, status: AbilityStatus): AbilityStatus {
  if (!ranAutomatically(r.approval)) return status;
  const open = status.kind === "unknown_outcome" || (status.kind === "failed" && !failureEndStateKnown(r));
  return open ? { ...status, text: OUTCOME_UNKNOWN_LINE } : status;
}

export function abilityStatus(r: AbilityRequest, laterEdits = 0): AbilityStatus {
  return automaticStatus(r, baseAbilityStatus(r, laterEdits));
}

/** The status as a person's own approval reads it; `abilityStatus` adjusts it for an automatic one. */
function baseAbilityStatus(r: AbilityRequest, laterEdits: number): AbilityStatus {
  const noun = isPostRequest(r) ? "post" : "page";
  switch (r.state) {
    case "pending":
      return { kind: "pending", text: "Waiting for your decision." };
    case "approved":
      return { kind: "approved", text: approvedNotStartedText(r) };
    case "dispatched":
      return { kind: "running", text: `WPMgr is creating the draft ${noun}.` };
    case "outcome_unknown": {
      const undone = undoStatus(r, false, laterEdits);
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
      return (
        undoStatus(r, true, laterEdits) ?? {
          kind: "done",
          text: isElementorRequest(r) ? "Draft created in Elementor." : "Draft created.",
        }
      );
    }
    case "failed": {
      const undone = undoStatus(r, false, laterEdits);
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
        text:
          settingNotSentLine(r.not_sent_reason) ??
          `Nothing was sent: ${notSentText(r.not_sent_reason)}. Nothing was created.`,
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

// Links to a post on the site are built from the site's own address and an
// integer id only, never from the AI's or the site's words.

/** The site's address as a base for its links, or null unless it is a plain http(s) URL. */
function siteBase(siteUrl: string | null | undefined): URL | null {
  if (!siteUrl) return null;
  try {
    const base = new URL(siteUrl.endsWith("/") ? siteUrl : `${siteUrl}/`);
    return base.protocol === "https:" || base.protocol === "http:" ? base : null;
  } catch {
    return null;
  }
}

function isPostId(postId: number | null | undefined): postId is number {
  return postId != null && Number.isInteger(postId) && postId >= 1;
}

function adminPostHref(
  siteUrl: string | null | undefined,
  postId: number | null | undefined,
  action: "edit" | "elementor",
): string | null {
  const base = siteBase(siteUrl);
  if (base === null || !isPostId(postId)) return null;
  const u = new URL("wp-admin/post.php", base);
  u.searchParams.set("post", String(postId));
  u.searchParams.set("action", action);
  return u.toString();
}

/**
 * Link to edit the created draft in WordPress. Null unless the site URL is a
 * plain http(s) URL and the id is a positive integer.
 */
export function editDraftHref(
  siteUrl: string | null | undefined,
  postId: number | null | undefined,
): string | null {
  return adminPostHref(siteUrl, postId, "edit");
}

/** "Open in Elementor": {site}/wp-admin/post.php?post={id}&action=elementor, or null as editDraftHref. */
export function elementorEditHref(
  siteUrl: string | null | undefined,
  postId: number | null | undefined,
): string | null {
  return adminPostHref(siteUrl, postId, "elementor");
}

/** "Preview": {site}/?page_id={id}&preview=true, or ?p={id} for a post; null as editDraftHref. */
export function draftPreviewHref(
  siteUrl: string | null | undefined,
  postId: number | null | undefined,
  isPost: boolean,
): string | null {
  const base = siteBase(siteUrl);
  if (base === null || !isPostId(postId)) return null;
  const u = new URL("./", base);
  u.searchParams.set(isPost ? "p" : "page_id", String(postId));
  u.searchParams.set("preview", "true");
  return u.toString();
}

export interface DraftLink {
  readonly label: string;
  readonly href: string;
}

/**
 * The links under a request's status: "Open in Elementor" and "Preview" once
 * a page Elementor builds is created, otherwise "Edit the draft in WordPress"
 * while a draft may exist. Empty without the site's address or a post id.
 */
export function draftLinks(r: AbilityRequest, status: AbilityStatus, siteUrl: string | null | undefined): DraftLink[] {
  const id = r.created_post_id;
  const links: Array<{ label: string; href: string | null }> =
    status.kind === "done" && isElementorRequest(r)
      ? [
          { label: "Open in Elementor", href: elementorEditHref(siteUrl, id) },
          { label: "Preview", href: draftPreviewHref(siteUrl, id, isPostRequest(r)) },
        ]
      : status.kind === "done" || status.draftMayExist === true
        ? [{ label: "Edit the draft in WordPress", href: editDraftHref(siteUrl, id) }]
        : [];
  return links.flatMap((l) => (l.href === null ? [] : [{ label: l.label, href: l.href }]));
}

export function isPending(r: AbilityRequest): boolean {
  return r.state === "pending";
}

/**
 * True when a done change's Undo period has passed with nothing undone: the
 * card then says so rather than silently dropping the button.
 */
export function undoWindowOver(request: AbilityRequest, statusKind: string, canUndo: boolean): boolean {
  if (statusKind !== "done" || canUndo || !request.undo_available_until) return false;
  const end = Date.parse(request.undo_available_until);
  return Number.isFinite(end) && end <= Date.now();
}
