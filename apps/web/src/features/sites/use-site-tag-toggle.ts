import { useCallback, useRef } from "react";
import { useQueryClient } from "@tanstack/react-query";
import type { Site } from "@wpmgr/api";

import { useSetSiteTags, getCachedSiteTags } from "@/features/sites/use-sites";
import { toast } from "@/components/toast";

// GH #230 "rich tags" — fixes the single-site tag-picker lost-update race
// (HIGH, adversarial verify): the table/card TagPicker used to compute its
// replace-set toggle from the `site` PROP (the sites LIST cache at the time
// of that render). Two rapid toggles both read the same stale array before
// either PUT resolved, so the second PUT silently overwrote the first
// toggle's effect (last-write-wins on a POST that replaces the whole set).
//
// Two changes fix this together:
//   1. `useSetSiteTags` (use-sites.ts) now optimistically patches every
//      sites-list cache entry (not just the detail cache), so the SECOND
//      toggle's freshest-cache read sees the first toggle's effect as soon
//      as its `onMutate` runs.
//   2. This hook additionally SERIALIZES toggles per site via a promise
//      chain, so correctness never depends on exactly when `onMutate`'s
//      microtask lands relative to a second synchronous click. Each toggle's
//      "current tags" is read only after the PREVIOUS toggle's mutation has
//      fully resolved (not a captured prop, not a synchronous cache read
//      that could itself be racing `onMutate`), so a rapid A-then-B burst is
//      guaranteed to end with ONE final PUT carrying both deltas.
export interface SiteTagToggle {
  /** True while any toggle for this site is in flight. */
  isPending: boolean;
  /** Toggle one tag name on/off. Serialized: see module doc above. */
  toggleTag: (name: string) => void;
}

export function useSiteTagToggle(site: Site): SiteTagToggle {
  const queryClient = useQueryClient();
  const mutation = useSetSiteTags();

  // Per-site toggle queue. Each link resolves to the tag array that toggle
  // actually left the site in (server-confirmed on success; the pre-toggle
  // value on failure, so a subsequent queued toggle in the same burst still
  // has a known-good base instead of replaying a rejected delta).
  const queueRef = useRef<Promise<string[]> | null>(null);

  const toggleTag = useCallback(
    (name: string) => {
      const previous =
        queueRef.current ?? Promise.resolve(getCachedSiteTags(queryClient, site.id, site.tags));

      const next = previous
        // A prior link's rejection should never poison the whole queue —
        // fall back to the freshest cache read and keep going.
        .catch(() => getCachedSiteTags(queryClient, site.id, site.tags))
        .then((current) => {
          const nextTags = current.includes(name)
            ? current.filter((t) => t !== name)
            : [...current, name];
          return mutation.mutateAsync({ siteId: site.id, tags: nextTags }).then(
            () => nextTags,
            (err: unknown) => {
              toast.error(`Could not update tags on ${site.name || site.url}`, {
                description: err instanceof Error ? err.message : undefined,
              });
              return current;
            },
          );
        });

      queueRef.current = next;
    },
    [queryClient, mutation, site.id, site.name, site.url, site.tags],
  );

  return { isPending: mutation.isPending, toggleTag };
}
