import type { AuditEntry } from "@wpmgr/api";

// Resolves an audit entry's actor into a displayable person/key/system
// identity. The accountability anchor for the redesigned row: never a bare
// account UUID (see actor-chip.tsx for the rendering half of this contract).

export type ActorKind = "user" | "api_key" | "system";

export interface ActorInfo {
  kind: ActorKind;
  /** Primary display text. */
  name: string;
  /** Secondary text for the tooltip/detail panel (email, or the full id). */
  detail?: string;
  /** Raw actor id, always available for the detail panel / copy. */
  id: string;
  /**
   * True when `name` is a real resolved identity (a person's name, a key's
   * label, or the fixed "System" label) rather than an id-shaped fallback.
   */
  resolved: boolean;
}

function shortId(id: string): string {
  if (!id) return "";
  return id.length > 10 ? `${id.slice(0, 8)}…` : id;
}

export function resolveActor(entry: AuditEntry): ActorInfo {
  if (entry.actor_type === "system" || !entry.actor_id) {
    return { kind: "system", name: "System", id: entry.actor_id ?? "", resolved: true };
  }

  if (entry.actor_type === "api_key") {
    const label = entry.actor_name?.trim();
    if (label) {
      return { kind: "api_key", name: label, detail: entry.actor_id, id: entry.actor_id, resolved: true };
    }
    return {
      kind: "api_key",
      name: `API key ${shortId(entry.actor_id)}`,
      detail: entry.actor_id,
      id: entry.actor_id,
      resolved: false,
    };
  }

  // actor_type === "user" (default treatment for any other/future actor_type
  // too — it still gets the person-shaped chip, never a bare id left bare).
  const name = entry.actor_name?.trim();
  if (name) {
    return {
      kind: "user",
      name,
      detail: entry.actor_email ?? entry.actor_id,
      id: entry.actor_id,
      resolved: true,
    };
  }
  return {
    kind: "user",
    name: shortId(entry.actor_id),
    detail: entry.actor_email ?? "Account details unavailable",
    id: entry.actor_id,
    resolved: false,
  };
}

/** Two-letter initials for an avatar chip, from a resolved display name. */
export function initials(name: string): string {
  const parts = name.trim().split(/\s+/).filter(Boolean);
  if (parts.length === 0) return "?";
  if (parts.length === 1) {
    const word = parts[0] ?? "";
    return word.slice(0, 2).toUpperCase() || "?";
  }
  const first = parts[0]?.charAt(0) ?? "";
  const last = parts[parts.length - 1]?.charAt(0) ?? "";
  return `${first}${last}`.toUpperCase();
}
