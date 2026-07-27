// Shared status -> icon/label/color-class maps for uptime status kinds
// (up/degraded/down/unknown). Extracted from the /uptime route so both the
// page itself and the incident detail dialog render the exact same
// triple-encoded (colour + label + icon) status language (GH #148) for
// accessibility, without the dialog importing across the router's
// auto-code-split route boundary (route files are their own chunk; a
// feature module importing from one would create an import cycle since the
// route also imports the feature to mount it).

import {
  CheckCircle2,
  AlertTriangle,
  XCircle,
  HelpCircle,
} from "lucide-react";

import type { UptimeStatusKind } from "./fleet-types";

export const STATUS_ICON: Record<UptimeStatusKind, typeof CheckCircle2> = {
  up: CheckCircle2,
  degraded: AlertTriangle,
  down: XCircle,
  unknown: HelpCircle,
};

export const STATUS_LABEL: Record<UptimeStatusKind, string> = {
  up: "Up",
  degraded: "Degraded",
  down: "Down",
  unknown: "Unknown",
};

export const STATUS_COLOR_CLASS: Record<UptimeStatusKind, string> = {
  up: "text-[var(--color-success-subtle-fg)]",
  degraded: "text-[var(--color-warning-subtle-fg)]",
  down: "text-[var(--color-destructive-subtle-fg)]",
  unknown: "text-[var(--color-muted-foreground)]",
};

// ---------------------------------------------------------------------------
// status_reason (GH #291) — a page-cached site whose PHP backend is
// completely dead used to render a clean green "Up"; the control plane now
// correctly derives "degraded" for it AND sends a reason code explaining
// why. A bare "Degraded" chip is not actionable on its own, so every reason
// code we know about maps to a short, calm, plain-language explanation.
//
// Deliberately a plain Record (not keyed by a union of the reason codes):
// the control plane can add a new code at any time, and a code missing from
// this map must fall back to "no explanation" rather than a type error or a
// broken render — see `statusReasonCopy` below.
// ---------------------------------------------------------------------------

export const STATUS_REASON_COPY: Record<string, string> = {
  agent_unreachable:
    "Serving visitors, but the site agent is not responding. Cached pages may be masking a broken backend.",
  agent_degraded: "Serving visitors, but the site agent is late checking in.",
  // GH #291 — the reason the explanation feature exists: a page cache can
  // keep serving a healthy-looking 200 while WordPress itself is dead
  // behind it. Honest in both directions: visitors are NOT seeing an
  // outage (so this must not read as "down"), but WordPress is not
  // responding (so it must not read as "fine" either), and it points an
  // operator at what is actually broken.
  app_down:
    "Serving visitors from cache, but WordPress is not responding. Logins, forms and the admin area are likely already broken.",
  slow_response: "Responding slowly.",
};

/**
 * Resolve a `FleetStatusItem.status_reason` code to its plain-language copy.
 * Returns `null` for an absent OR unrecognised reason so callers render the
 * status chip exactly as they always have, with no extra text — a future
 * control-plane reason code this map does not yet know about can never
 * break the UI.
 */
export function statusReasonCopy(reason: string | null | undefined): string | null {
  if (!reason) return null;
  return STATUS_REASON_COPY[reason] ?? null;
}
