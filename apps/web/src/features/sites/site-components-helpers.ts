// site-components helpers — extracted from site-components-table.tsx so that
// the component file exports only components (react-refresh/only-export-components).

import type { SiteComponent } from "@wpmgr/api";

// Forward-compatible shape: Track B's OpenAPI regen adds `available_update` to
// SiteComponent. Until the generated types catch up, treat the field as an
// optional opaque marker — its presence (not its value) is what we filter on.
type ComponentWithMaybeUpdate = SiteComponent & {
  available_update?: unknown;
};

/** Count of plugins+themes that have NO outstanding update. */
export function countUpToDate(
  plugins: SiteComponent[] = [],
  themes: SiteComponent[] = [],
): number {
  const hasNoUpdate = (c: SiteComponent): boolean => {
    const ext = c as ComponentWithMaybeUpdate;
    return ext.available_update == null;
  };
  return plugins.filter(hasNoUpdate).length + themes.filter(hasNoUpdate).length;
}
