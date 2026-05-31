// Phase 5 — connection lifecycle types + helpers for the Sites domain.
//
// The Phase 3/4 backend added a `connection_state` machine to every Site plus a
// tenant-level SSE stream (`GET /api/v1/sites/events`). The generated @wpmgr/api
// `Site` type has NOT been regenerated to declare these fields yet (the JSON
// carries them at runtime). Per the codebase's existing pattern of extending
// generated types locally (see Member.email / SiteShare.email), we model the
// connection surface here as a local extension rather than churning the
// generated client.

import type { Site } from "@wpmgr/api";

/** The connection lifecycle state machine, mirrored from the CP enum. */
export type ConnectionState =
  | "pending_enrollment"
  | "connected"
  | "degraded"
  | "disconnected"
  | "revoked"
  | "archived";

/**
 * Runtime-present connection fields the generated `Site` type does not yet
 * declare. Intersected onto `Site` so call sites read `site.connection_state`
 * with full type safety once they go through `withConnection()`.
 */
export interface SiteConnectionFields {
  connection_state?: ConnectionState;
  connection_generation?: number;
  disconnected_reason?: string;
}

/** A `Site` widened with the connection-lifecycle fields. */
export type ConnectedSite = Site & SiteConnectionFields;

/**
 * Narrow an arbitrary `Site` (which at runtime carries the connection fields)
 * into a `ConnectedSite`. This is the single, explicit narrowing point — we do
 * NOT sprinkle `as` casts across the codebase.
 */
export function asConnectedSite(site: Site): ConnectedSite {
  // `SiteConnectionFields` are all optional, so a plain `Site` already widens
  // to `ConnectedSite` structurally — the runtime values come from the JSON.
  return site;
}

/**
 * Read a site's connection state with a sensible fallback. Older rows (or the
 * brief window before the first SSE patch) may not carry an explicit
 * `connection_state`; we derive a best-effort value from the legacy
 * `enrolled` / `status` flags so the badge never renders blank.
 */
export function connectionStateOf(site: Site): ConnectionState {
  const s = asConnectedSite(site);
  if (s.connection_state) return s.connection_state;
  // Legacy fallback: map the pre-Phase-5 fields onto the new vocabulary.
  if (site.status === "disabled") return "archived";
  if (!site.enrolled) return "pending_enrollment";
  if (site.health_status === "unreachable") return "disconnected";
  return "connected";
}

/** Connection states that mean "the agent is gone / paused" — re-connectable. */
export function isReconnectable(state: ConnectionState): boolean {
  return (
    state === "revoked" || state === "disconnected" || state === "archived"
  );
}

/** Whether a state should be hidden from the default (non-archived) list. */
export function isArchivedState(state: ConnectionState): boolean {
  return state === "archived";
}
