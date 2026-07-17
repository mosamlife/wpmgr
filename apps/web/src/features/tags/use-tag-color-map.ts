import { useMemo } from "react";

import { useTags } from "@/features/tags/use-tags";

// GH #230 "rich tags" — adversarial-verify MEDIUM: custom tag colors only
// rendered correctly in Settings (which already has the full `SiteTag`
// registry objects, `.color` included). Every OTHER surface renders a tag
// from just its NAME (a site's `tags: string[]`), so it had no way to know
// a registry color was set at all and silently fell back to auto everywhere
// else. This hook is the name -> color lookup every such call site threads
// into `resolveTagStyle`/`resolveTagDot`.

/**
 * Memoized name -> color ("" or "#rrggbb") map derived from the tenant's tag
 * registry. Returns an EMPTY Map while the registry hasn't loaded yet (or
 * has no tags) — every lookup then misses and callers fall through to the
 * auto (name-derived) style, exactly like an explicit `color: ""`. Never
 * "flash unstyled": the auto path always resolves to a deterministic class
 * recipe regardless of whether the registry has loaded.
 */
export function useTagColorMap(): ReadonlyMap<string, string> {
  const { data: tags } = useTags();
  return useMemo(() => {
    const map = new Map<string, string>();
    for (const tag of tags ?? []) map.set(tag.name, tag.color);
    return map;
  }, [tags]);
}
