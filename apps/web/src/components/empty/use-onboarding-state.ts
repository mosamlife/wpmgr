import { useCallback, useEffect, useState } from "react";

// Surface 4.12 — onboarding state, persisted in localStorage so the wizard
// shows only on the tenant's first zero-sites encounter and stays gone after
// they complete (or skip) it. Per-browser, not per-server: we deliberately
// avoid round-tripping this through the API because the wizard is a UX nicety,
// not a tenant-bound state machine. Operators on a new device get the wizard
// again, which is fine — they'll dismiss it once.
//
// Storage layout:
//   localStorage["wpmgr.onboarding.completed"] = "true" | absent
//
// SSR / no-window safety: readCompleted() guards against a missing window, so
// the lazy useState initializer is safe in both SSR and browser environments.
// Tenants who have already completed onboarding get the correct state from the
// very first render; no reconcile pass is needed.

const STORAGE_KEY = "wpmgr.onboarding.completed";

function readCompleted(): boolean {
  if (typeof window === "undefined") return false;
  try {
    return window.localStorage.getItem(STORAGE_KEY) === "true";
  } catch {
    // Safari private mode and other storage-denied environments fall through
    // to "not completed" — the wizard will show, which is the better failure
    // mode than silently hiding the only path to enrollment.
    return false;
  }
}

export interface OnboardingState {
  /** True when the wizard should be shown in place of NoSitesEmpty. */
  isOnboarding: boolean;
  /** Persist completion and hide the wizard. */
  complete: () => void;
  /** Clear completion so the wizard reappears (debug / settings affordance). */
  reset: () => void;
}

export function useOnboardingState(): OnboardingState {
  const [completed, setCompleted] = useState<boolean>(readCompleted);

  // Listen for cross-tab changes so a "Reset onboarding" action in another
  // tab takes effect here without a refresh. The initial value is already
  // read directly in the useState lazy initializer above, so no synchronous
  // setState is needed on mount.
  useEffect(() => {
    function onStorage(e: StorageEvent) {
      if (e.key !== STORAGE_KEY) return;
      setCompleted(e.newValue === "true");
    }
    window.addEventListener("storage", onStorage);
    return () => window.removeEventListener("storage", onStorage);
  }, []);

  const complete = useCallback(() => {
    try {
      window.localStorage.setItem(STORAGE_KEY, "true");
    } catch {
      // Storage denied — fall back to in-memory state so the wizard still
      // disappears for the remainder of the session.
    }
    setCompleted(true);
  }, []);

  const reset = useCallback(() => {
    try {
      window.localStorage.removeItem(STORAGE_KEY);
    } catch {
      // ignore
    }
    setCompleted(false);
  }, []);

  return { isOnboarding: !completed, complete, reset };
}
