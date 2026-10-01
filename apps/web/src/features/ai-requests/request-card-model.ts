import type { AssistantRequest } from "@wpmgr/api";

// Pure, render-agnostic logic for the AI cache-clear request card
// (tracka-cache-purge-design-v7 §2.6, the W2 slice). Kept apart from
// request-card.tsx so every branch — one per row in the design's "Card
// states" table — is a function call a test can hit directly, without
// mounting a component for each of the states below.
//
// EVERY MODEL-CHOSEN OR SITE-REPORTED VALUE IS TREATED AS UNTRUSTED TEXT.
// `site_label`, `site_host`, `grant_label`, `url` and `site_reported_text`
// pass straight through unmodified: this module never wraps one in markup,
// never builds an href out of one, and never interpolates one into a
// sentence this component authored (ADR-061 Decision 3). The values are
// simply carried to the caller, which renders them as text nodes.

/** The maximum length of the rendered "Page address" slot before "…". */
export const PAGE_ADDRESS_DISPLAY_CAP = 120;

export interface TruncatedAddress {
  readonly display: string;
  readonly truncated: boolean;
}

/**
 * Caps a stored page address at `max` characters for the fixed slot.
 * Percent-encoding is never decoded here — the caller renders `display`
 * (and, once expanded, the untouched `value`) exactly as stored, so an
 * encoded control character or encoded prose stays visibly encoded rather
 * than being turned back into the bytes it was refused for at creation.
 */
export function truncateAddress(
  value: string,
  max: number = PAGE_ADDRESS_DISPLAY_CAP,
): TruncatedAddress {
  if (value.length <= max) return { display: value, truncated: false };
  return { display: `${value.slice(0, max)}…`, truncated: true };
}

/** "Clear one page on Shop" / "Clear the whole cache on Shop" (§2.6). */
export function cardTitle(request: AssistantRequest): string {
  return request.scope === "url"
    ? `Clear one page on ${request.site_label}`
    : `Clear the whole cache on ${request.site_label}`;
}

export interface SetUpForLine {
  readonly primary: string;
  readonly caption?: string;
}

// The N3 caption, verbatim from the design, is only ever shown next to the
// browser-sign-in line -- it is a fact about THAT setup path, not a general
// disclaimer to attach to every connection name.
const BROWSER_SIGN_IN_CAPTION =
  "On browser sign-in this name starts as the name the client gave itself, so it identifies the connection, not who is asking.";

/**
 * "Set up for" (§2.6): `setup_client` when the connection named one, the
 * dedicated browser-sign-in line when it did not, and the raw `grant_via`
 * as a last resort so the row is never blank.
 */
export function setUpForLine(request: {
  grant_via: string;
  setup_client?: string | null;
}): SetUpForLine {
  if (request.grant_via === "browser_sign_in") {
    return { primary: "Set up by browser sign-in", caption: BROWSER_SIGN_IN_CAPTION };
  }
  return { primary: request.setup_client ?? request.grant_via };
}

/** The "If you approve" paragraph, which never differs by anything but scope. */
export function ifApproveCopy(scope: AssistantRequest["scope"]): string {
  return scope === "url"
    ? "WPMgr deletes its own cached files for this page address. It skips every hosting cache, because none is yet confirmed to clear only this site. If this site uses a CDN set up in WPMgr, it also removes this address from that CDN. Pages load slower until the cache refills."
    : "WPMgr deletes its own cached files for this site. It skips every hosting cache, because none is yet confirmed to clear only this site. Pages load slower until the cache refills.";
}

/** An unparsable timestamp renders as unparsable, never as "now" or blank. */
export function formatTime(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "an unreadable time";
  return d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
}

const WAITING_REASON_TEXT: Record<string, string> = {
  site_unreachable: "site agent not connected",
  site_cooldown: "this site's cache was cleared recently; this clear will run once the cooldown ends",
  site_hourly_cap: "this site reached its limit of AI cache clears for the hour",
  site_busy: "another AI cache clear on this site is still running",
  org_busy: "an organisation change is in progress",
  context_unavailable: "we could not read your AI rules",
  write_tools_disabled: "AI cache clears are switched off on this server",
};

/** The `last_attempt_code` sentence for an `approved_undispatched` row. */
export function waitingReasonText(code: string | null): string | null {
  if (code === null) return null;
  return WAITING_REASON_TEXT[code] ?? code;
}

const NOT_SENT_REASON_TEXT: Record<string, string> = {
  grant_inactive: "connection revoked or expired",
  assistant_paused: "AI assistant paused",
  organisation_deleted: "organisation is being deleted",
  capability_not_held: "connection no longer holds this capability",
  site_absent: "site left the connection's scope",
  forbidden_by_context: "AI rules forbid it",
  agent_outdated: "site agent too old",
  dispatch_deadline_passed: "not started within an hour of approval",
  transport_pre_send: "could not reach the site",
};

/** The `not_sent_reason` sentence for the "done: not sent" outcome. */
export function notSentReasonText(reason: string | null): string {
  if (reason === null) return "the reason was not recorded";
  return NOT_SENT_REASON_TEXT[reason] ?? reason;
}

export interface DecidedByLine {
  /** "Approved" or "Declined". */
  readonly verb: "Approved" | "Declined";
  readonly text: string;
}

/**
 * "Approved by you at 09:43." / "Declined by Sam at 09:50." / the n3
 * deleted-account variant (§2.6). `currentUserId` is compared against
 * `decided_by_user_id`, never against `decided_by_name` — a name is
 * site-adjacent free text an operator could have renamed themselves into,
 * an id is the actual foreign key the server compared.
 */
export function decidedByLine(
  request: AssistantRequest,
  verb: "Approved" | "Declined",
  currentUserId: string | null | undefined,
): DecidedByLine {
  const time = request.decided_at ? formatTime(request.decided_at) : "an unrecorded time";
  if (request.decided_by_account_deleted) {
    return { verb, text: `${verb} by a person whose account was later deleted at ${time}.` };
  }
  if (
    verb === "Approved" &&
    currentUserId != null &&
    request.decided_by_user_id === currentUserId
  ) {
    return { verb, text: `${verb} by you at ${time}.` };
  }
  return { verb, text: `${verb} by ${request.decided_by_name ?? "someone"} at ${time}.` };
}

/**
 * The site's own failure text, carried as plain data for the caller to render
 * as a text node. Empty or absent is null.
 */
function siteSaid(request: AssistantRequest): string | null {
  const t = request.site_reported_text;
  if (typeof t !== "string" || t.trim() === "") return null;
  return `The site said: ${t}`;
}

/** One rendered card-state line, everything after the always-shown facts. */
export interface RequestStatusLine {
  readonly kind:
    | "pending"
    | "approved_waiting"
    | "running"
    | "done_purged"
    | "done_site_reported_failure"
    | "done_agent_failed"
    | "done_outcome_unknown"
    | "done_not_sent"
    | "declined"
    | "withdrawn"
    | "expired"
    | "unknown";
  readonly text: string;
  readonly detail?: string;
}

/**
 * Maps one `AssistantRequest` to the line the card shows, following §2.6's
 * "Card states" table row by row. `state === "dispatched"` alone is
 * ambiguous between "running" and "done" (model.go never gives it its own
 * wire value): the outcome column is what tells them apart, exactly as the
 * worker records it.
 */
export function requestStatusLine(
  request: AssistantRequest,
  currentUserId: string | null | undefined,
): RequestStatusLine {
  switch (request.state) {
    case "pending":
      return { kind: "pending", text: "Waiting for your decision." };

    case "approved_undispatched": {
      const approved = decidedByLine(request, "Approved", currentUserId);
      const reason = waitingReasonText(request.last_attempt_code);
      if (reason === null || request.last_attempt_at === null) {
        return { kind: "approved_waiting", text: `${approved.text} Not started yet.` };
      }
      const attemptTime = formatTime(request.last_attempt_at);
      return {
        kind: "approved_waiting",
        text: `${approved.text} Not started yet.`,
        detail: `Last attempt ${attemptTime}: ${reason}.`,
      };
    }

    case "dispatched": {
      if (request.outcome === null) {
        return { kind: "running", text: "Clearing now." };
      }
      const at = request.outcome_at ? formatTime(request.outcome_at) : "an unrecorded time";
      switch (request.outcome) {
        case "purged": {
          const cleared = request.hosting_caches_cleared ?? [];
          const skipped = request.hosting_caches_skipped ?? [];
          const clearedText = cleared.length > 0 ? cleared.join(", ") : "none";
          let text = `Cleared at ${at}. Hosting caches cleared: ${clearedText}.`;
          if (skipped.length > 0) {
            text += ` Skipped because WPMgr has not confirmed they clear only this site: ${skipped.join(", ")}.`;
          }
          if (request.origin_only_confirmed === false) {
            text +=
              " The site did not confirm that it skipped hosting caches WPMgr has not confirmed.";
          }
          if (request.wpmgr_cdn === "failed") {
            text +=
              " Removing it from your CDN failed, so visitors may see the old page until the CDN copy expires.";
          }
          return { kind: "done_purged", text };
        }
        case "site_reported_failure": {
          const said = siteSaid(request);
          return {
            kind: "done_site_reported_failure",
            text: "The site said clearing did not complete. Part of its cache may have been cleared.",
            ...(said !== null ? { detail: said } : {}),
          };
        }
        case "agent_failed": {
          const said = siteSaid(request);
          return {
            kind: "done_agent_failed",
            text: "The site's agent failed while clearing. Part of its cache may have been cleared.",
            ...(said !== null ? { detail: said } : {}),
          };
        }
        case "outcome_unknown":
          return {
            kind: "done_outcome_unknown",
            text: "We could not confirm the result. The site may or may not have cleared its cache.",
          };
        case "not_sent": {
          const reason = notSentReasonText(request.not_sent_reason);
          const line: RequestStatusLine = {
            kind: "done_not_sent",
            text: `Nothing was sent: ${reason}. Nothing ran.`,
          };
          if (request.not_sent_reason === "dispatch_deadline_passed") {
            const last = waitingReasonText(request.last_attempt_code);
            if (last !== null) return { ...line, detail: `Last attempt: ${last}.` };
          }
          return line;
        }
        default:
          return { kind: "unknown", text: "This request's outcome is not one this page knows." };
      }
    }

    case "rejected": {
      const declined = decidedByLine(request, "Declined", currentUserId);
      return { kind: "declined", text: declined.text };
    }

    case "withdrawn": {
      const at = request.withdrawn_at ? formatTime(request.withdrawn_at) : "an unrecorded time";
      return {
        kind: "withdrawn",
        text: `Withdrawn at ${at}: its connection was revoked. Nothing ran.`,
      };
    }

    case "expired":
      return {
        kind: "expired",
        text: `Closed unanswered at ${formatTime(request.expires_at)}. Nothing ran.`,
      };

    default:
      // A fifth wire state would land here rather than being silently
      // coerced into one of the above — the same "an unknown value is a
      // failure, not a guess" rule the connections list follows.
      return { kind: "unknown", text: "This request is in a state this page does not know." };
  }
}

/** Only a `pending` row shows Approve/Decline — every other state is read-only. */
export function isActionable(request: AssistantRequest): boolean {
  return request.state === "pending";
}
