import { useCallback, useEffect, useSyncExternalStore } from "react";

// Sprint 3 surface 4.4 — recently-viewed sites tracker for the command palette.
//
// When an operator presses ⌘K and types nothing, the "Navigate" group surfaces
// their last few viewed sites as "Go to {hostname}" verb items. Persistence is
// localStorage; the store is a small singleton so the recording call (from the
// site-detail route) and the reading call (from the palette) share state.
//
// We cap at 5 because the palette panel is finite — beyond that, fuzzy search
// over the full site list is the better mental model.

const STORAGE_KEY = "wpmgr.recent-sites";
const MAX_RECENT = 5;

export interface RecentSite {
  id: string;
  hostname: string;
  /** Epoch ms; used to sort most-recent-first and prune duplicates. */
  visitedAt: number;
}

// ── Module-level singleton ──────────────────────────────────────────────────

let cache: readonly RecentSite[] = readInitial();
const listeners = new Set<() => void>();

function readInitial(): readonly RecentSite[] {
  if (typeof window === "undefined") return [];
  try {
    const raw = window.localStorage.getItem(STORAGE_KEY);
    if (!raw) return [];
    const parsed = JSON.parse(raw) as unknown;
    if (!Array.isArray(parsed)) return [];
    // Defensive shape check — drop entries that look malformed rather than
    // throwing in the palette's hot path.
    return parsed
      .filter(
        (entry): entry is RecentSite =>
          typeof entry === "object" &&
          entry !== null &&
          typeof (entry as RecentSite).id === "string" &&
          typeof (entry as RecentSite).hostname === "string" &&
          typeof (entry as RecentSite).visitedAt === "number",
      )
      .slice(0, MAX_RECENT);
  } catch {
    return [];
  }
}

function persist(next: readonly RecentSite[]): void {
  try {
    window.localStorage.setItem(STORAGE_KEY, JSON.stringify(next));
  } catch {
    // Quota / private mode — fall through, the in-memory cache still works.
  }
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

function getSnapshot(): readonly RecentSite[] {
  return cache;
}

function emit(next: readonly RecentSite[]): void {
  cache = next;
  persist(next);
  listeners.forEach((l) => l());
}

/**
 * Derive a friendly hostname from a site `url`. Stripped of protocol, www, and
 * any trailing slash so the palette renders "blog.example.com" rather than
 * "https://www.blog.example.com/".
 */
export function hostnameFromUrl(url: string): string {
  try {
    const parsed = new URL(url);
    return parsed.hostname.replace(/^www\./, "");
  } catch {
    return url.replace(/^https?:\/\//, "").replace(/^www\./, "").replace(/\/$/, "");
  }
}

/** Read the current recent-sites list (most-recent-first). */
export function useRecentSites(): readonly RecentSite[] {
  return useSyncExternalStore(subscribe, getSnapshot, getSnapshot);
}

/**
 * Imperative recorder. Call from a route effect when the operator visits a
 * site detail page; the next ⌘K surface will show it at the top of "Navigate".
 */
export function recordRecentSite(input: {
  id: string;
  name?: string;
  url?: string;
}): void {
  const hostname =
    (input.url ? hostnameFromUrl(input.url) : "") || input.name || input.id;
  const next: RecentSite = {
    id: input.id,
    hostname,
    visitedAt: Date.now(),
  };
  const deduped = cache.filter((entry) => entry.id !== input.id);
  emit([next, ...deduped].slice(0, MAX_RECENT));
}

/**
 * Hook variant for routes: pass the current site (or undefined while loading)
 * and the recorder fires once per id.
 */
export function useRecordRecentSite(
  site: { id: string; name?: string; url?: string } | undefined | null,
): void {
  const record = useCallback(
    (s: { id: string; name?: string; url?: string }) => recordRecentSite(s),
    [],
  );
  useEffect(() => {
    if (!site?.id) return;
    record({ id: site.id, name: site.name, url: site.url });
  }, [site?.id, site?.name, site?.url, record]);
}
