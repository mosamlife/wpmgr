import type {
  AbilityRequestApproval,
  AiMode,
  AiModeOption,
  AiModeSource,
} from "@wpmgr/api";

// Every sentence the AI-trust screens show, as pure functions of server data.
// A person's name from the server is carried through as plain text for a text
// node; nothing here builds markup from one. Kind names ("edits to published
// pages") come from the server, never from a literal table here.

export const MODE_NAME: Readonly<Record<AiMode, string>> = {
  ask: "Ask every time",
  ai_drafts: "Auto for AI drafts",
  full: "Full auto on this site",
};

/** The one line under "AI editing" while it is on, per mode. */
export function modeStateLine(mode: AiMode): string {
  switch (mode) {
    case "ask":
      return "AI editing is on. Every change the AI asks for waits for your approval.";
    case "ai_drafts":
      return "AI editing is on. Drafts the AI makes run at once. Everything else waits for you.";
    case "full":
      return "Full auto is on. Published pages can change without asking. Everything is listed in AI activity.";
  }
}

export const MODE_DESCRIPTION: Readonly<Record<AiMode, string>> = {
  ask: "Every change the AI wants waits for you to approve it.",
  ai_drafts:
    "Drafts the AI makes, and changes to them, run at once. Each is saved with a copy, listed in AI activity, and has an Undo button. Published pages, your own drafts and publishing still wait for you.",
  full: "Not available yet.",
};

export const MODE_HEADING = "How much the AI may do on this site";
export const MODE_SUBHEADING = "You decide this. The AI cannot change it.";
export const FULL_AUTO_LATER = "Coming later";

/** The Ask list, by name (design §2.4). */
export const ALWAYS_WAITS_LINE =
  "Always waits for you: permanent deletes, users and roles, the site address, installing or removing plugins and themes, WordPress updates, and changes WPMgr could not undo exactly.";

/** Shown above the enable button while AI editing is off. */
export const ENABLE_NOTICE =
  "When you turn this on, drafts the AI makes here run without asking. Each is saved with a copy and can be undone. Anything else waits for you. You can change this afterwards.";

/** Shown while the mode's source is `launch_default`. */
export const MIGRATED_NOTICE =
  "New: drafts the AI makes on this site now run without asking. Each is saved with a copy, listed in AI activity, and can be undone. Published pages and everything else still wait for you.";
export const KEEP_ASKING = "Keep asking every time";
export const KEEP_AUTO = "Keep Auto for AI drafts";
export const NOT_OPERATOR_LINE = "An operator, admin or owner can change this.";

export const SETTER_INVALID_LINE =
  "The person who chose this setting no longer has the access it needs, so every change waits for you. Choose the setting again to start it.";

export const PAUSED_LINE =
  "AI is paused for your organisation. Nothing runs, automatic or not, until an owner resumes it.";

export const SAVE_FAILED_LINE = "WPMgr could not save this. The setting is unchanged. Try again.";

export function staleVersionLine(mode: AiMode | null): string {
  const now = mode ? ` It now reads ${MODE_NAME[mode]}.` : "";
  return `This setting was changed a moment ago.${now} Choose again if you still want to change it.`;
}

export function savedToast(mode: AiMode): string {
  return `Saved. This site is set to ${MODE_NAME[mode]}.`;
}

/** "Oct 9". An unreadable timestamp is said to be unreadable, never shown as today. */
export function shortDate(iso: string | null | undefined): string {
  if (!iso) return "an unrecorded date";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "an unreadable date";
  return d.toLocaleDateString([], { month: "short", day: "numeric" });
}

interface SetterFacts {
  readonly set_by_user_id?: string | null;
  readonly set_by_name?: string | null;
  readonly set_by_account_deleted: boolean;
  readonly set_at?: string | null;
}

/** "you" is decided by user id, never by name. */
function setterName(s: SetterFacts, currentUserId: string | null | undefined): string | null {
  if (s.set_by_account_deleted) return null;
  if (currentUserId != null && s.set_by_user_id === currentUserId) return "you";
  return s.set_by_name ?? null;
}

/** The footer of the setting: who chose the mode now in force, and when. Null when nobody did. */
export function setByLine(
  s: SetterFacts & { readonly source: AiModeSource; readonly mode: AiMode },
  currentUserId: string | null | undefined,
): string | null {
  const date = shortDate(s.set_at);
  const name = setterName(s, currentUserId);
  switch (s.source) {
    case "unset":
    case "migration":
      return null;
    case "tightened":
      return `Lowered to Ask every time on ${date}.`;
    case "launch_default":
      return `The default since ${date}.`;
    case "enable_default":
      if (s.set_by_account_deleted) {
        return `Chosen on ${date} when AI editing was turned on, by a person whose account was later deleted.`;
      }
      return `Chosen when ${name ?? "someone"} turned on AI editing on ${date}.`;
    case "person":
      if (s.set_by_account_deleted) return `Set by a person whose account was later deleted, on ${date}.`;
      return `Set by ${name ?? "someone"} on ${date}.`;
    default:
      return null;
  }
}

/**
 * The "Allowed by" line of a change a setting approved, built only from the
 * values copied onto the request when it was approved (never re-read from the
 * site, so it stays true after the setting changes).
 */
export function allowedByLine(
  approval: AbilityRequestApproval | null | undefined,
  currentUserId: string | null | undefined,
): string | null {
  if (!approval || approval.source !== "policy") return null;
  const s = approval.setting;
  if (!s) return "This site's setting at the time.";
  const setting = MODE_NAME[s.mode];
  const date = shortDate(s.set_at);
  if (s.set_by_account_deleted) return `${setting}, set by a person whose account was later deleted`;
  const name = setterName(s, currentUserId) ?? "someone";
  switch (s.source) {
    case "launch_default":
      return `${setting}, the default since ${date}`;
    case "enable_default":
      return `${setting}, chosen when ${name} turned on AI editing on ${date}`;
    case "person":
      return `${setting}, set by ${name} on ${date}`;
    default:
      return `${setting}, set on ${date}`;
  }
}

/** True when a setting, not a person, approved the request. */
export function ranAutomatically(approval: AbilityRequestApproval | null | undefined): boolean {
  return approval?.source === "policy";
}

/**
 * The "Waiting because" line of a card that waits for a person, from the
 * server's `ask_reason`. Null when the server recorded no reason. A reason
 * this build does not know still reads as waiting for a person.
 */
export function waitingBecauseLine(
  askReason: string | null | undefined,
  kindName: string | null | undefined,
): string | null {
  if (!askReason) return null;
  switch (askReason) {
    case "kind_always_asks":
      return "This kind of change always waits for you.";
    case "unknown_target_state":
      return "WPMgr could not tell whether visitors would see this change.";
    case "site_mode_ask":
      return "This site is set to ask you every time.";
    case "kind_not_in_mode":
      return kindName
        ? `This site runs ${kindName} only when you allow them.`
        : "This site's setting does not run this change on its own.";
    case "setter_lacks_permission":
      return "The person who chose this site's setting no longer has the access that setting needs.";
    case "over_change_budget":
      return "This connection used its automatic changes for now.";
    case "over_site_cap":
      return "This connection reached its limit on sites changed this hour.";
    case "connection_never_auto":
      return "This connection is set to always ask.";
    case "connection_setter_invalid":
      return "The person who allowed this connection to run changes on its own can no longer manage AI connections. An owner or admin can allow it again on the connection.";
    case "not_checked":
      return "WPMgr could not check this against the site's setting in time, so it waits for you.";
    default:
      return "This change waits for you to approve it.";
  }
}

/** Why one mode cannot be chosen now, from the option's server `reason`. Null when it can. */
export function optionReasonLine(option: AiModeOption, minAgentVersion: string): string | null {
  if (option.choosable) return null;
  switch (option.reason) {
    case "role_required":
      return NOT_OPERATOR_LINE;
    case "org_scope_required":
      return "Only a full member of the organisation can choose this.";
    case "session_required":
      return "Only a person signed in to WPMgr can choose this.";
    case "paused":
      return PAUSED_LINE;
    case "agent_outdated":
      return `Update the WPMgr plugin on this site to ${minAgentVersion} or later, then choose this.`;
    default:
      return "This cannot be chosen right now.";
  }
}

/** Not-sent reasons that belong to the setting that approved a change (design §8.9). */
export function settingNotSentLine(reason: string | null | undefined): string | null {
  if (reason === "setting_changed") {
    return "Not started: this site's setting changed before it ran. Nothing was changed.";
  }
  if (reason === "class_changed") {
    return "Not started: WPMgr changed how this kind of change is handled before it ran. Nothing was changed.";
  }
  return null;
}

export const RAN_AUTOMATICALLY = "Ran automatically";

export const UNDO_WINDOW_OVER_LINE = "Undo is no longer available. Its period has ended.";
