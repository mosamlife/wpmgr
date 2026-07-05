import { useCallback, useMemo, useState } from "react";

// Selection state for the per-site snapshot table (issue #115 bulk-delete).
//
// Unlike `useSitesSelection` (features/sites/use-sites-selection.ts) this is
// deliberately LOCAL React state, not a module-level singleton: the Sites
// selection is shared with the command palette across the whole app, but a
// snapshot selection only ever makes sense scoped to the one Backups card the
// operator is looking at. Two open site-detail tabs (or a route remount)
// must never share or leak a snapshot selection.
//
// The interface mirrors SitesSelection (selected / count / toggle / setMany /
// clear) so callers familiar with the Sites bulk-select surface recognize the
// shape immediately.

export interface BackupsSelection {
  /** Underlying set of selected snapshot ids. Read-only from the consumer side. */
  readonly selected: ReadonlySet<string>;
  /** Number of selected ids; equivalent to `selected.size`. */
  readonly count: number;
  /** Flip the membership of one id (used by per-row checkboxes). */
  toggle: (id: string) => void;
  /** Force a list of ids to a specific selected state (header / chain / quick-select). */
  setMany: (ids: readonly string[], next: boolean) => void;
  /** Drop every selection (the "Clear selection" affordance). */
  clear: () => void;
}

/**
 * Remove any id from `selected` that is no longer present in `validIds`.
 * Exported as a pure function so the prune behaviour is unit-testable without
 * mounting a component. Returns the SAME set reference when nothing changed
 * so callers can skip a re-render (`useState` bails out on identical refs).
 */
export function pruneSelection(
  selected: ReadonlySet<string>,
  validIds: readonly string[],
): ReadonlySet<string> {
  if (selected.size === 0) return selected;
  const validSet = new Set(validIds);
  let changed = false;
  const next = new Set<string>();
  for (const id of selected) {
    if (validSet.has(id)) {
      next.add(id);
    } else {
      changed = true;
    }
  }
  return changed ? next : selected;
}

/**
 * Local multi-select state for a site's snapshot list. `validIds` should be
 * the full set of snapshot ids currently in the list (not just the eligible
 * ones) — the list polls every 3s while any snapshot is non-terminal, and a
 * row can disappear (deleted elsewhere, GC'd) between polls. Whenever
 * `validIds` changes we drop any selected id that vanished so the toolbar
 * count and the eventual bulk-delete payload never reference a stale id.
 */
export function useBackupsSelection(
  validIds: readonly string[],
): BackupsSelection {
  const [selected, setSelected] = useState<ReadonlySet<string>>(
    () => new Set(),
  );

  // Prune during render (the React-endorsed "adjust state on prop change"
  // pattern) rather than in a useEffect: an effect would commit the stale
  // selection for one extra frame before the prune runs, and re-running
  // setState synchronously inside an effect body is flagged as a footgun by
  // the react-hooks lint rule. `prevValidIds` lets us detect the list
  // reference change and prune synchronously within the same render pass;
  // React re-renders immediately when state changes during render, so the
  // component never paints with a stale selection.
  const [prevValidIds, setPrevValidIds] = useState(validIds);
  if (prevValidIds !== validIds) {
    setPrevValidIds(validIds);
    const pruned = pruneSelection(selected, validIds);
    if (pruned !== selected) {
      setSelected(pruned);
    }
  }

  const toggle = useCallback((id: string) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }, []);

  const setMany = useCallback((ids: readonly string[], nextState: boolean) => {
    if (ids.length === 0) return;
    setSelected((prev) => {
      const next = new Set(prev);
      for (const id of ids) {
        if (nextState) next.add(id);
        else next.delete(id);
      }
      return next;
    });
  }, []);

  const clear = useCallback(() => {
    setSelected((prev) => (prev.size === 0 ? prev : new Set()));
  }, []);

  return useMemo<BackupsSelection>(
    () => ({ selected, count: selected.size, toggle, setMany, clear }),
    [selected, toggle, setMany, clear],
  );
}
