// Pure helpers for rendering a FleetIncident's ongoing/duration labels, and
// (GH #148 detail dialog) a fatal-reason-to-human-copy map. Extracted from
// the /uptime IncidentList so the "is this incident still open" decision,
// the duration formatting, and the reason humanizer can be unit tested
// without mounting the route or the detail dialog.

import type { FleetIncident } from "./fleet-types";

/**
 * Decide whether an incident should render as "ongoing".
 *
 * Trusts the reliable `ongoing` boolean from the pinned API contract first.
 * Falls back to treating a non-finite `duration_seconds` (null, undefined,
 * or NaN) as ongoing too — this is the hardening GH #148 asked for: never
 * derive "ongoing" from `duration_seconds === null` alone, so a missing or
 * malformed duration can never render as a completed incident (and can
 * never produce "NaNh").
 */
export function isIncidentOngoing(
  inc: Pick<FleetIncident, "ongoing" | "duration_seconds">,
): boolean {
  if (inc.ongoing) return true;
  return !Number.isFinite(inc.duration_seconds);
}

/**
 * Format a resolved incident's duration as a compact "Xs" / "Xm" / "X.Xh"
 * label (no "for" prefix — callers add that so the sentence composes
 * naturally). Returns null for an ongoing incident, or when the duration is
 * not a finite number — defensive so this can never render "NaNh"/"NaNm".
 */
export function formatIncidentDuration(
  durationSeconds: number | null | undefined,
  ongoing: boolean,
): string | null {
  if (ongoing) return null;
  if (!Number.isFinite(durationSeconds)) return null;
  const seconds = durationSeconds as number;
  if (seconds < 60) return `${Math.round(seconds)}s`;
  if (seconds < 3600) return `${Math.round(seconds / 60)}m`;
  return `${(seconds / 3600).toFixed(1)}h`;
}

/**
 * Map a raw incident `reason` string (`IncidentDetail.reason`, GH #148 detail
 * dialog) to a short, human-readable phrase.
 *
 * The uptime prober emits a mix of shapes in this field: two fixed codes
 * ("wp_fatal_error", "wp_db_error" — see apps/api/internal/uptime/probe.go
 * scanFatal), a formatted string ("http status 500"), an "ssrf_blocked: ..."
 * prefix, and otherwise the raw Go transport error text (timeouts, DNS
 * failures, connection refused, TLS errors). This never throws and always
 * returns something renderable — falling back to the raw reason for
 * anything it does not recognize, so a new/unmapped reason is still shown
 * rather than silently dropped.
 */
export function humanizeIncidentReason(
  reason: string | null | undefined,
): string {
  const raw = (reason ?? "").trim();
  if (!raw) return "No specific cause recorded";

  const lower = raw.toLowerCase();

  if (lower === "wp_fatal_error") return "WordPress fatal error page";
  if (lower === "wp_db_error") return "Database connection error page";

  const httpStatusMatch = /^http status (\d{3})$/i.exec(raw);
  if (httpStatusMatch) return `Site returned HTTP ${httpStatusMatch[1]}`;

  if (lower.startsWith("ssrf_blocked"))
    return "Blocked by outbound security policy";
  if (lower.includes("deadline exceeded") || lower.includes("timeout"))
    return "Connection timed out";
  if (lower.includes("connection refused")) return "Connection refused";
  if (lower.includes("no such host") || lower.includes("lookup"))
    return "DNS lookup failed";
  if (
    lower.includes("certificate") ||
    lower.includes("tls") ||
    lower.includes("x509")
  )
    return "TLS certificate error";

  return raw;
}
