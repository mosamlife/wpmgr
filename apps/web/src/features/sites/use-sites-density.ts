import { useCallback, useState } from "react";

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
 * when localStorage is unavailable or the stored value is absent).
 *
 * Precedence: explicit override > localStorage > DEFAULT_DENSITY.
 *
 * When override changes between renders we apply it via the stored-prev-in-
 * state pattern (React docs pattern for derived state). Calling setDensityState
 * in the render body is allowed — the lint rule only bans synchronous setState
 * calls inside effect bodies.
 */
export function useSitesDensity(
  override?: SitesDensity,
): [SitesDensity, (next: SitesDensity) => void] {
  // Lazy initializer runs once, reading the correct value before first paint.
  const [density, setDensityState] = useState<SitesDensity>(
    () => override ?? readStoredDensity(),
  );

  // Store the previous override value in state so we can detect changes during
  // the render pass (stored-prev-in-state pattern). No ref — the lint rule
  // react-hooks/refs bans reading/writing refs during render.
  const [prevOverride, setPrevOverride] = useState<SitesDensity | undefined>(
    override,
  );
  if (prevOverride !== override) {
    // Override changed — synchronously update both the tracked value and the
    // active density during this render pass. React will flush these together.
    setPrevOverride(override);
    setDensityState(override ?? readStoredDensity());
  }

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
