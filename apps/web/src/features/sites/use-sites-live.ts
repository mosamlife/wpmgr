import { useCallback } from "react";
import { useQueryClient } from "@tanstack/react-query";
import type { Site } from "@wpmgr/api";

import { sitesKeys } from "./use-sites";
import {
  useSiteEvents,
  parseStateChanged,
  type SiteEvent,
} from "./use-site-events";
import { asConnectedSite } from "./connection-state";

// Phase 5.3 — keep the Sites list + detail caches live over SSE, with NO
// polling. Two classes of event:
//
//   • CARDINALITY changes (a row appears or leaves) → invalidate the list so
//     react-query refetches the authoritative set:
//       site.created   — new pending row
//       site.enrolled  — (a created site that just got an agent; the row's
//                         identity/visibility may change)
//       site.archived  — row drops out of the default (non-archived) list
//       site.restored  — row re-enters the default list
//
//   • IN-PLACE changes (same rows, new field values) → patch the cache directly
//     with setQueryData, NO refetch:
//       site.state_changed — carries the full post-transition site
//       site.heartbeat     — bumps last_seen_at (keeps "last seen {t}" fresh)
//
// Patching (rather than invalidating) the in-place class is what makes the
// status dot animate a single one-shot pulse instead of the whole table
// flickering through a refetch. We patch EVERY cached list query (there can be
// several: all-sites, per-tag, archived) by walking the query cache.

/** The shape of a `site.heartbeat` event's data (best-effort). */
function heartbeatLastSeen(ev: SiteEvent): string {
  if (
    typeof ev.data === "object" &&
    ev.data !== null &&
    "last_seen_at" in ev.data &&
    typeof ev.data.last_seen_at === "string"
  ) {
    return ev.data.last_seen_at;
  }
  // Fall back to the envelope timestamp — the heartbeat *is* the last-seen.
  return ev.ts;
}

export function useSitesLiveSync(): void {
  const queryClient = useQueryClient();

  const handle = useCallback(
    (ev: SiteEvent) => {
      switch (ev.type) {
        case "site.created":
        case "site.enrolled":
        case "site.archived":
        case "site.restored":
          // Cardinality change — let react-query refetch the canonical list.
          void queryClient.invalidateQueries({ queryKey: sitesKeys.lists() });
          return;

        case "site.state_changed": {
          const changed = parseStateChanged(ev);
          if (!changed) return;
          // The SSE `site` payload is a PARTIAL summary (no tags/components/
          // versions), so MERGE it into the cached row — replacing outright would
          // drop fields like `tags` and crash consumers that iterate them.
          const summary = changed.site;
          queryClient.setQueryData<Site>(sitesKeys.detail(ev.site_id), (prev) =>
            prev ? { ...prev, ...summary } : summary,
          );
          patchListsWith(queryClient, ev.site_id, (prev) => ({
            ...prev,
            ...summary,
          }));
          // If the transition moved the site into/out of the archived bucket,
          // the default list's membership changed → invalidate to reconcile.
          if (changed.to === "archived" || changed.from === "archived") {
            void queryClient.invalidateQueries({ queryKey: sitesKeys.lists() });
          }
          return;
        }

        case "site.heartbeat": {
          const lastSeen = heartbeatLastSeen(ev);
          // Bump last_seen_at on the detail + every list in place.
          queryClient.setQueryData<Site>(
            sitesKeys.detail(ev.site_id),
            (prev) => (prev ? { ...prev, last_seen_at: lastSeen } : prev),
          );
          patchListsWith(queryClient, ev.site_id, (prev) => ({
            ...prev,
            last_seen_at: lastSeen,
          }));
          return;
        }

        // Disconnected/revoked also arrive as their own named events, but the
        // authoritative state lives in the paired site.state_changed frame.
        // We invalidate the detail so a focused detail page reconciles.
        case "site.disconnected":
        case "site.revoked":
          void queryClient.invalidateQueries({
            queryKey: sitesKeys.detail(ev.site_id),
          });
          return;
      }
    },
    [queryClient],
  );

  useSiteEvents(handle);
}

/**
 * Patch the single matching row inside every cached `["sites","list", …]`
 * query. `update` receives the current row and returns its replacement. Rows
 * not present in a given list are left untouched (the row may simply not match
 * that list's filter — we don't synthesize membership here; cardinality is the
 * invalidate path's job).
 */
function patchListsWith(
  queryClient: ReturnType<typeof useQueryClient>,
  siteId: string,
  update: (prev: Site) => Site,
): void {
  const queries = queryClient.getQueryCache().findAll({
    queryKey: sitesKeys.lists(),
  });
  for (const query of queries) {
    const data = query.state.data;
    if (!Array.isArray(data)) continue;
    const list = data as Site[];
    let changed = false;
    const next = list.map((site) => {
      if (site.id !== siteId) return site;
      changed = true;
      // Merge so connection fields on the incoming row survive the narrowing.
      return asConnectedSite(update(site));
    });
    if (changed) {
      queryClient.setQueryData(query.queryKey, next);
    }
  }
}
