import { useCallback, useEffect, useState } from "react";

/**
 * Table row density mode. Compact is the operator default per DESIGN.md.
 * Comfortable = 56px, Compact = 44px, Dense = 36px.
 */
export type SitesDensity = "comfortable" | "compact" | "dense";

const STORAGE_KEY = "wpmgr.sites.density";
const DEFAULT_DENSITY: SitesDensity = "compact";

const ALLOWED: ReadonlySet<SitesDensity> = new Set<SitesDensity>([
  "comfortable",
  "compact",
  "dense",
]);

function readStoredDensity(): SitesDensity {
  if (typeof window === "undefined") return DEFAULT_DENSITY;
  try {
    const raw = window.localStorage.getItem(STORAGE_KEY);
    if (raw && ALLOWED.has(raw as SitesDensity)) return raw as SitesDensity;
  } catch {
    // localStorage may throw in privacy modes; fall back silently.
  }
  return DEFAULT_DENSITY;
}

/**
 * Density mode hook, synced to localStorage. SSR-safe (defaults to "compact"
 * during hydration; reconciles from storage on mount).
 */
export function useSitesDensity(
  override?: SitesDensity,
): [SitesDensity, (next: SitesDensity) => void] {
  const [density, setDensityState] = useState<SitesDensity>(
    override ?? DEFAULT_DENSITY,
  );

  // Hydrate from localStorage once, unless caller forces an override.
  useEffect(() => {
    if (override) {
      setDensityState(override);
      return;
    }
    setDensityState(readStoredDensity());
  }, [override]);

  const setDensity = useCallback(
    (next: SitesDensity) => {
      setDensityState(next);
      if (typeof window === "undefined") return;
      try {
        window.localStorage.setItem(STORAGE_KEY, next);
      } catch {
        // Best-effort persistence; non-fatal.
      }
    },
    [],
  );

  return [density, setDensity];
}

/** Row height in pixels for a density mode. Header row is fixed at 44px. */
export function rowHeightFor(density: SitesDensity): number {
  switch (density) {
    case "comfortable":
      return 56;
    case "compact":
      return 44;
    case "dense":
      return 36;
  }
}
