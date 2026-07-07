// Pure helpers for rendering a FleetIncident's ongoing/duration labels
// (GH #148). Extracted from the /uptime IncidentList so the "is this
// incident still open" decision and the duration formatting can be unit
// tested without mounting the route.

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
