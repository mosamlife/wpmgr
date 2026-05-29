import { useEffect, useState } from "react";
import { WifiOff } from "lucide-react";

import { cn } from "@/lib/utils";

// Phase 6 (harden) — global "you're offline" banner.
//
// Why: when the operator loses network in the middle of a fleet update or
// while staring at the sites table, the dashboard goes silent (no toasts —
// SSE/poll just stops landing). Without a banner, the only signal is stale
// data, which is the opposite of "calm, clinical, operator-grade".
//
// What it does:
//   • Reads `navigator.onLine` and listens for the global online/offline
//     events. SSR-safe — the initial state defaults to "online" so the banner
//     never flashes on first paint.
//   • Renders fixed top-0, full-width, above the AppShell (z-50). The TopBar
//     is z-30 in the shell so the banner sits on top.
//   • Tone: `warning-subtle` (yellow-ish, low chroma) — never destructive.
//     Being offline is recoverable; the banner is a status, not an alarm.
//   • Copy: what (you're offline) · how (we'll resume when you're back). No
//     em dashes (DESIGN.md UI-string ban), no exclamation.
//
// Hardening contract:
//   • The banner has no shadow (DESIGN.md "borders over shadows"); the divider
//     to the page below is the bottom border.
//   • The banner content is wrapped in `truncate min-w-0` so a future
//     translation of the message that runs long doesn't break the row.
//   • `aria-live="polite"` so screen readers announce the transition without
//     interrupting whatever the operator is doing.
//   • The hook auto-removes its listeners on unmount.

export interface OfflineBannerProps {
  /** Optional copy override for the headline. Verb-first stays the same in any locale. */
  message?: string;
  /** Optional copy override for the secondary explanation. */
  resumeHint?: string;
}

/**
 * useNetworkOnline — subscribes to navigator online/offline events.
 *
 * SSR-safe: the lazy initializer reads navigator.onLine at first render.
 * When window/navigator are absent (SSR, some embedded webviews) it
 * defaults to true so the banner never paints on first paint.
 */
function useNetworkOnline(): boolean {
  // Lazy initializer — runs once, synchronously, before the first paint.
  // Defensive: some embedded webviews leave navigator.onLine undefined.
  const [online, setOnline] = useState<boolean>(() => {
    if (typeof navigator !== "undefined" && typeof navigator.onLine === "boolean") {
      return navigator.onLine;
    }
    return true;
  });

  useEffect(() => {
    function handleOnline() {
      setOnline(true);
    }
    function handleOffline() {
      setOnline(false);
    }
    window.addEventListener("online", handleOnline);
    window.addEventListener("offline", handleOffline);
    return () => {
      window.removeEventListener("online", handleOnline);
      window.removeEventListener("offline", handleOffline);
    };
  }, []);

  return online;
}

export function OfflineBanner({
  message = "You're offline.",
  resumeHint = "We'll resume when you're back.",
}: OfflineBannerProps = {}) {
  const online = useNetworkOnline();
  if (online) return null;

  return (
    <div
      role="status"
      aria-live="polite"
      className={cn(
        // Fixed top, full width, above the TopBar (z-50 wins over the shell).
        "fixed inset-x-0 top-0 z-50",
        // No shadow; a 1px bottom border carries the separation.
        "border-b border-[var(--color-border)] bg-[var(--color-warning-subtle)] text-[var(--color-warning-subtle-fg)]",
      )}
    >
      <div className="mx-auto flex min-w-0 max-w-[1400px] items-center gap-2 px-4 py-1.5 text-xs font-medium">
        <WifiOff aria-hidden="true" className="size-3.5 shrink-0" />
        <span className="min-w-0 truncate" title={`${message} ${resumeHint}`}>
          <span>{message}</span>{" "}
          <span className="font-normal opacity-80">{resumeHint}</span>
        </span>
      </div>
    </div>
  );
}
