// Small metadata helpers shared by the row, the run-collapsing summary, and
// the per-entry detail panel. `AuditEntry.metadata` is a loosely-typed JSON
// blob (`{[key: string]: unknown}`); these narrow it defensively instead of
// widening with `any`.

export type AuditMetadata = Record<string, unknown> | undefined;

export function metaString(meta: AuditMetadata, key: string): string | null {
  const v = meta?.[key];
  return typeof v === "string" && v.length > 0 ? v : null;
}

export function metaNumber(meta: AuditMetadata, key: string): number | null {
  const v = meta?.[key];
  return typeof v === "number" && Number.isFinite(v) ? v : null;
}

export function metaBoolean(meta: AuditMetadata, key: string): boolean | null {
  const v = meta?.[key];
  return typeof v === "boolean" ? v : null;
}

/** Turn "version_id" / "confirm_sensitive_missing" into readable words. */
export function humanizeKey(key: string): string {
  const words = key.replace(/_/g, " ").trim();
  if (!words) return key;
  return words.charAt(0).toUpperCase() + words.slice(1);
}

const REASON_LABELS: Record<string, string> = {
  confirm_sensitive_missing: "Sensitive-file confirmation was not provided",
  insufficient_permission: "The actor's role does not have permission for this action",
  confirm_token_missing: "The required confirmation token was not provided",

  // AI cache-clear requests (tracka-cache-purge design v7, S2.7 / S3.3).
  // `mcp.tool.denied`'s reason, or `assistant.request.not_sent`'s.
  capability_not_held: "This connection does not hold the cache-clear capability",
  scope_empty: "This connection has no sites in scope",
  site_absent: "The site is outside this connection's scope",
  // G2: the site's own stored address has no host form WPMgr can dial.
  site_address_unusable: "WPMgr cannot use this site's stored address",
  invalid_arguments: "The AI tool call's arguments were invalid",
  url_not_on_site: "The page address is not on this site",
  url_other_site: "The page address belongs to a different site in scope",
  url_port_ambiguous: "The page address's port did not match a site in scope",
  url_same_address: "More than one site in scope shares this exact address",
  site_unreachable: "This site's agent was not connected",
  agent_outdated: "This site's agent is too old for this request",
  forbidden_by_context: "An operator AI rule forbids this tool on this site",
  context_unavailable: "This site's AI rules could not be read",
  pending_request_for_site: "This connection already has a request waiting for this site",
  pending_cap: "This connection has reached its limit of waiting requests",
  grant_daily_cap: "This connection reached its daily request limit",
  site_hourly_cap: "This site reached its hourly limit of AI cache clears",
  // assistant.request.withdrawn's reason, and not_sent's closed_by.
  connection_revoked: "The connection was revoked",
  // assistant.request.not_sent's reason.
  grant_inactive: "The connection was revoked or had expired",
  assistant_paused: "The organisation's AI assistant was paused",
  organisation_deleted: "The organisation is being deleted",
  write_tools_disabled: "AI cache clears were switched off on this server",
  dispatch_deadline_passed: "The approved clear did not start within an hour of approval",
  entry_changed: "The AI tool changed after it was approved",
  entry_disabled: "The AI tool was switched off after it was approved",
  could_not_reach_site: "WPMgr could not reach this site",
  // assistant.request.failed's outcome.
  outcome_unknown: "WPMgr could not confirm whether the clear ran",
};

/** Humanize a `metadata.reason` code; passes through free text unchanged. */
export function humanizeReason(reason: string): string {
  const known = REASON_LABELS[reason];
  if (known) return known;
  if (reason.includes(" ")) return reason;
  return humanizeKey(reason);
}

/** Readable label for a non-site target_type ("update_task" -> "Update task"). */
export function humanizeTargetType(targetType: string): string {
  if (targetType === "user") return "Account";
  if (targetType === "assistant_ability_request") return "AI change request";
  return humanizeKey(targetType.replace(/_/g, " "));
}

// ---------------------------------------------------------------------------
// Delivery status (uptime.alert.sent's email_status/webhook_status metadata)
// ---------------------------------------------------------------------------
//
// New rows carry an honest email_status/webhook_status ("sent"/"skipped"/
// "failed") plus a reason code when the outcome isn't "sent" — replacing the
// old bare "emailed"/"webhooked" booleans that always read "Yes" whether or
// not delivery actually happened. Historical rows still have the old
// booleans and keep rendering through the generic formatMetaValue path in
// audit-detail.tsx; this section only covers the new keys.

const DELIVERY_STATUS_WORDS: Record<string, string> = {
  sent: "Sent",
  skipped: "Skipped",
  failed: "Failed",
};

/** Human label for an email_status/webhook_status code; unknown codes fall
 * back to sentence case rather than a raw lowercase token. */
export function humanizeDeliveryStatus(status: string): string {
  return DELIVERY_STATUS_WORDS[status] ?? humanizeKey(status);
}

const DELIVERY_REASON_LABELS: Record<string, string> = {
  smtp_not_configured: "SMTP not configured",
  no_recipients: "No recipients",
  no_recipients_configured: "No recipients",
  resolve_smtp: "SMTP lookup failed",
};

/** Human label for an email_reason/webhook_reason code; an unknown code
 * passes through unchanged (the control plane already scrubs raw transport
 * errors before they reach audit metadata). */
export function humanizeDeliveryReason(reason: string): string {
  return DELIVERY_REASON_LABELS[reason] ?? reason;
}

/**
 * Full plain-text status line for a delivery outcome, e.g. "Sent",
 * "Skipped (SMTP not configured)", "Failed (SMTP lookup failed)". Used as
 * the rendered row's title/tooltip; the on-screen value additionally colors
 * the leading word with a status pill (see audit-detail.tsx).
 */
export function formatDeliveryStatus(status: string, reason: string | null): string {
  const word = humanizeDeliveryStatus(status);
  if (status === "sent" || !reason) return word;
  return `${word} (${humanizeDeliveryReason(reason)})`;
}

/** Absolute local clock time (HH:MM:SS) for a past-day timestamp (point 7:
 * only "today" uses a relative label; anything in an already-labeled day
 * bucket shows an unambiguous absolute time instead). */
export function formatClockTime(iso: string): string {
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return "—";
  return date.toLocaleTimeString(undefined, {
    hour12: false,
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });
}

/**
 * Compact date + time for a non-today row ("Jul 3, 16:43:03", or
 * "Jul 3 2025, 16:43:03" when the entry's year differs from the current
 * year). Pairs with `formatClockTime`: a row that scrolls out from under its
 * sticky day-group header still carries its own date, rather than reading as
 * a bare time with no context.
 */
export function formatDateTime(iso: string): string {
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return "—";
  const monthDay = date.toLocaleDateString(undefined, {
    month: "short",
    day: "numeric",
  });
  const sameYear = date.getFullYear() === new Date().getFullYear();
  const datePart = sameYear ? monthDay : `${monthDay} ${date.getFullYear()}`;
  return `${datePart}, ${formatClockTime(iso)}`;
}

/**
 * Longest common "/"-separated directory prefix across a set of file paths,
 * used to summarize a collapsed read-burst ("wp-content/uploads/…"). Returns
 * null when there's nothing meaningful to show (no paths, or no shared
 * segment at all).
 */
export function commonPathPrefix(paths: Array<string | null>): string | null {
  const valid = paths.filter((p): p is string => !!p);
  if (valid.length === 0) return null;
  if (valid.length === 1) return valid[0] ?? null;

  const split = valid.map((p) => p.split("/"));
  const minLen = Math.min(...split.map((s) => s.length));
  const common: string[] = [];
  for (let i = 0; i < minLen; i++) {
    const seg = split[0]?.[i];
    if (seg !== undefined && split.every((s) => s[i] === seg)) {
      common.push(seg);
    } else {
      break;
    }
  }
  if (common.length === 0) return null;

  const prefix = common.join("/");
  const allSame = valid.every((p) => p === valid[0]);
  return allSame ? prefix : `${prefix}/…`;
}
