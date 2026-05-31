import { useMemo } from "react";

import { useNow } from "@/lib/use-now";
import { cn, relativeTime } from "@/lib/utils";

import { StatusDot, type StatusTone } from "./status-dot";

// Phase 5.5 — the single source of truth for rendering a site's connection
// lifecycle state. Reads `connection_state` (plus last-seen / expiry context)
// and renders dot + label + relative time. Used on the sites table and the
// site-detail header so the vocabulary stays identical everywhere.
//
//   pending_enrollment → ◐ Awaiting agent · {N}m left   (info)
//   connected          → ● Connected · last seen {t}     (success)
//   degraded           → ● Degraded · last seen {t}      (warning)
//   disconnected       → ● Disconnected · {t}            (destructive)
//   revoked            → ● Revoked                        (destructive)
//   archived           → ◯ Archived                       (muted)
//
// Relative time auto-updates via `useNow()` — a 1s tick while connected or
// degraded (operators watch these closely), 30s otherwise (cheap; the others
// move slowly). The dot one-shot-pulses on tone change (`pulseOnChange`) so a
// pending→connected or connected→degraded flip registers without becoming a
// perpetual attention magnet.

import type { ConnectionState } from "@/features/sites/connection-state";

export interface ConnectionStateBadgeProps {
  state: ConnectionState;
  /** ISO last-heartbeat timestamp; drives the "last seen {t}" tail. */
  lastSeenAt?: string | null;
  /** ISO enrollment-code expiry; drives the "{N}m left" pending tail. */
  expiresAt?: string | null;
  /** Pulse the dot once when the state changes. Defaults to true. */
  pulseOnChange?: boolean;
  className?: string;
}

interface BadgeShape {
  tone: StatusTone;
  /** ◐ / ● / ◯ glyph carried by the dot shape — we render via StatusDot. */
  glyph: "half" | "filled" | "hollow";
  label: string;
}

const SHAPE: Record<ConnectionState, BadgeShape> = {
  pending_enrollment: { tone: "info", glyph: "half", label: "Awaiting agent" },
  connected: { tone: "success", glyph: "filled", label: "Connected" },
  degraded: { tone: "warning", glyph: "filled", label: "Degraded" },
  disconnected: {
    tone: "destructive",
    glyph: "filled",
    label: "Disconnected",
  },
  revoked: { tone: "destructive", glyph: "filled", label: "Revoked" },
  archived: { tone: "muted", glyph: "hollow", label: "Archived" },
};

/** States that benefit from a 1s clock (everything else updates slowly). */
function tickFor(state: ConnectionState): number {
  return state === "connected" || state === "degraded" ? 1000 : 30000;
}

export function ConnectionStateBadge({
  state,
  lastSeenAt,
  expiresAt,
  pulseOnChange = true,
  className,
}: ConnectionStateBadgeProps) {
  const now = useNow(tickFor(state));
  const shape = SHAPE[state];

  const tail = useMemo(
    () => computeTail(state, now, lastSeenAt, expiresAt),
    [state, now, lastSeenAt, expiresAt],
  );

  const a11yLabel = tail.text
    ? `${shape.label}, ${tail.aria ?? tail.text}`
    : shape.label;

  return (
    <span
      className={cn(
        "inline-flex items-center gap-1.5 text-xs font-medium",
        className,
      )}
      aria-label={a11yLabel}
    >
      {/* The dot is decorative here; the wrapper span carries the a11y label
          (and an aria-live region announces pending countdowns, see below). */}
      <ConnectionDot
        tone={shape.tone}
        glyph={shape.glyph}
        pulseOnChange={pulseOnChange}
      />
      <span aria-hidden="true">{shape.label}</span>
      {tail.text ? (
        <>
          <span aria-hidden="true" className="text-muted-foreground">
            {"·"}
          </span>
          <span
            aria-hidden="true"
            className="font-mono tabular-nums text-muted-foreground"
            // Pending countdowns are announced politely so a screen-reader user
            // hears "2m left" tick toward expiry without a barrage.
            {...(state === "pending_enrollment"
              ? { "aria-live": "polite" as const }
              : {})}
          >
            {tail.text}
          </span>
        </>
      ) : null}
    </span>
  );
}

/**
 * StatusDot renders a filled circle; we adapt its glyph by overlaying a ring
 * (half / hollow). For `filled` we use the dot as-is. For `half` (◐ awaiting)
 * and `hollow` (◯ archived) we render a ring + partial/empty fill via border
 * utilities so the three lifecycle "shapes" read distinctly at a glance.
 */
function ConnectionDot({
  tone,
  glyph,
  pulseOnChange,
}: {
  tone: StatusTone;
  glyph: BadgeShape["glyph"];
  pulseOnChange: boolean;
}) {
  if (glyph === "filled") {
    return <StatusDot tone={tone} pulseOnChange={pulseOnChange} />;
  }
  if (glyph === "hollow") {
    // ◯ — a hollow ring in the muted tone.
    return (
      <span
        aria-hidden="true"
        className="inline-block size-2 shrink-0 rounded-full border-2 border-muted-foreground"
      />
    );
  }
  // ◐ — half-filled: a ringed dot with a half-fill via a clipped inner.
  return (
    <span
      aria-hidden="true"
      className="relative inline-flex size-2 shrink-0 items-center justify-center rounded-full border-2 border-info"
    >
      <span className="absolute inset-y-0 left-0 w-1/2 rounded-l-full bg-info" />
    </span>
  );
}

interface Tail {
  text: string | null;
  /** Optional fuller phrasing for the accessible label. */
  aria?: string;
}

function computeTail(
  state: ConnectionState,
  now: number,
  lastSeenAt?: string | null,
  expiresAt?: string | null,
): Tail {
  switch (state) {
    case "pending_enrollment": {
      const left = minutesLeft(expiresAt, now);
      if (left === null) return { text: null };
      if (left <= 0)
        return { text: "code expired", aria: "enrollment code expired" };
      return { text: `${left}m left`, aria: `${left} minutes left to enroll` };
    }
    case "connected":
    case "degraded": {
      const t = shortRelative(lastSeenAt);
      if (!t) return { text: null };
      return { text: `last seen ${t}`, aria: `last seen ${t}` };
    }
    case "disconnected": {
      const t = shortRelative(lastSeenAt);
      if (!t) return { text: null };
      return { text: t, aria: `since ${t}` };
    }
    case "revoked":
    case "archived":
      return { text: null };
  }
}

function minutesLeft(expiresAt: string | null | undefined, now: number): number | null {
  if (!expiresAt) return null;
  const expiry = Date.parse(expiresAt);
  if (Number.isNaN(expiry)) return null;
  return Math.max(0, Math.ceil((expiry - now) / 60000));
}

/** "4m" / "2h" — chip-format relative time with the trailing " ago" stripped. */
function shortRelative(iso: string | null | undefined): string | null {
  const full = relativeTime(iso);
  if (!full) return null;
  if (full === "just now") return "now";
  return full.replace(/ ago$/, "");
}
