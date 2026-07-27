import { useCallback, useEffect, useRef, useState } from "react";

// GH #291 Phase 3 — one-time dismissal state for the "application health
// alerting is available" upgrade prompt (`app-health-alert-prompt.tsx`).
//
// Reuses the SAME mechanism as `components/empty/use-onboarding-state.ts`:
// per-browser localStorage, never round-tripped through the API. That file's
// own reasoning applies here too — this is a UX nicety ("don't nag the same
// operator about the same one-time notice") rather than a tenant-bound
// state machine, so there is no need to invent a settings endpoint just to
// remember a dismissal. An operator on a new device sees the prompt again,
// which is fine: it is calm and dismissible, not a blocker.
//
// Scoped per tenant (the key includes tenantId) so switching tenants in the
// same browser does not carry a dismissal across organisations that have
// nothing to do with each other.
//
// Storage layout:
//   localStorage["wpmgr.app-health-alert-prompt.dismissed.<tenantId>"] = "true" | absent

function storageKey(tenantId: string): string {
  return `wpmgr.app-health-alert-prompt.dismissed.${tenantId}`;
}

function readDismissed(tenantId: string | null): boolean {
  if (!tenantId) return false;
  if (typeof window === "undefined") return false;
  try {
    return window.localStorage.getItem(storageKey(tenantId)) === "true";
  } catch {
    // Storage-denied environments (e.g. Safari private mode) fall through to
    // "not dismissed" — the prompt shows every time, which is the better
    // failure mode than silently and permanently hiding it.
    return false;
  }
}

export interface AppHealthPromptDismissalState {
  /** True once this tenant has dismissed the prompt in this browser. */
  isDismissed: boolean;
  /** Persist the dismissal so the prompt does not show again. */
  dismiss: () => void;
}

/**
 * @param tenantId The active tenant id, or `null`/`undefined` when there is
 *   no resolved tenant yet (e.g. `me` still loading). Dismissal is treated
 *   as unknown (not dismissed, but also not shown by the caller — see the
 *   prompt component's own gating) until a tenantId is available.
 */
export function useAppHealthPromptDismissed(
  tenantId: string | null | undefined,
): AppHealthPromptDismissalState {
  const key = tenantId ?? null;
  const [dismissed, setDismissed] = useState<boolean>(() => readDismissed(key));

  // Re-read when the tenant changes (e.g. switching orgs). This is the
  // React-documented "adjust state during render" pattern (comparing
  // against a ref of the previous key) rather than a setState call inside
  // useEffect, which would cascade an extra render on every mount.
  const lastKeyRef = useRef(key);
  if (lastKeyRef.current !== key) {
    lastKeyRef.current = key;
    const next = readDismissed(key);
    if (next !== dismissed) setDismissed(next);
  }

  // Listen for cross-tab changes so dismissing in one tab is reflected in
  // another already-open tab.
  useEffect(() => {
    function onStorage(e: StorageEvent) {
      if (!key || e.key !== storageKey(key)) return;
      setDismissed(e.newValue === "true");
    }
    window.addEventListener("storage", onStorage);
    return () => window.removeEventListener("storage", onStorage);
  }, [key]);

  const dismiss = useCallback(() => {
    if (!key) return;
    try {
      window.localStorage.setItem(storageKey(key), "true");
    } catch {
      // Storage denied — fall back to in-memory state so the prompt still
      // disappears for the remainder of the session.
    }
    setDismissed(true);
  }, [key]);

  return { isDismissed: dismissed, dismiss };
}
