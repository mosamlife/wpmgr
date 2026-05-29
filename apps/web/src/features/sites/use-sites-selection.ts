import { useCallback, useEffect, useMemo, useSyncExternalStore } from "react";

/**
 * Selection state hook for the Sites table. The selection lives as a Set of
 * site_id strings so it persists across pagination, filtering, and sort
 * changes — exactly per Sprint 2 brief ("47 selected across all filters").
 *
 * Callers consume `selected.size` for the counter, `selected.has(id)` for the
 * row checkbox, and the toggle helpers for row + header interactions.
 *
 * Implementation note (Sprint 3): the underlying Set is a module-level
 * singleton wired through `useSyncExternalStore`. That keeps the public API
 * identical (consumers still call `useSitesSelection()`) while letting the
 * command palette (a separate subtree from the Sites page) read the SAME
 * selection — necessary for "Run on selected" command items. Previously each
 * call site produced an independent Set; making it a singleton also fixes a
 * latent bug where SitesTable's internal fallback selection diverged from the
 * one its parent route lifted.
 */
export interface SitesSelection {
  /** Underlying set of selected site_ids. Read-only from the consumer side. */
  readonly selected: ReadonlySet<string>;
  /** Number of selected ids; equivalent to `selected.size`. */
  readonly count: number;
  /** Flip the membership of one id (used by per-row checkboxes). */
  toggle: (id: string) => void;
  /** Force a list of ids to a specific selected state (header "select all"). */
  setMany: (ids: readonly string[], next: boolean) => void;
  /** Drop every selection (the "Clear selection" affordance). */
  clear: () => void;
  /** Replace the entire selection wholesale (rare; useful for restore from URL). */
  replace: (ids: readonly string[]) => void;
}

// ── Module-level singleton store ────────────────────────────────────────────

let selectedRef: ReadonlySet<string> = new Set<string>();
const listeners = new Set<() => void>();

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

function getSnapshot(): ReadonlySet<string> {
  return selectedRef;
}

function emit(next: ReadonlySet<string>): void {
  selectedRef = next;
  listeners.forEach((l) => l());
}

function toggleId(id: string): void {
  const next = new Set(selectedRef);
  if (next.has(id)) next.delete(id);
  else next.add(id);
  emit(next);
}

function setManyIds(ids: readonly string[], nextState: boolean): void {
  const next = new Set(selectedRef);
  for (const id of ids) {
    if (nextState) next.add(id);
    else next.delete(id);
  }
  emit(next);
}

function clearAll(): void {
  if (selectedRef.size === 0) return;
  emit(new Set());
}

function replaceAll(ids: readonly string[]): void {
  emit(new Set(ids));
}

/**
 * Persistent multi-select state. Selection survives changes to the filtered
 * rows because we never reconcile against `sites` — selecting a row and then
 * filtering it out keeps the id in the set. Backed by a singleton so the
 * command palette in TopBar reads the same selection as the Sites page.
 *
 * The optional `initial` arg seeds the store ONCE on first mount; subsequent
 * mounts ignore it (the store is global, not per-instance).
 */
export function useSitesSelection(initial?: readonly string[]): SitesSelection {
  const selected = useSyncExternalStore(subscribe, getSnapshot, getSnapshot);

  // First-time-only seeding from `initial`. We don't react to changes in the
  // arg — that would let any consumer clobber the global selection on every
  // render — but seeding once supports the "restore from URL" use case.
  useEffect(() => {
    if (initial && initial.length > 0 && selectedRef.size === 0) {
      emit(new Set(initial));
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const toggle = useCallback((id: string) => toggleId(id), []);
  const setMany = useCallback(
    (ids: readonly string[], nextState: boolean) =>
      setManyIds(ids, nextState),
    [],
  );
  const clear = useCallback(() => clearAll(), []);
  const replace = useCallback((ids: readonly string[]) => replaceAll(ids), []);

  return useMemo<SitesSelection>(
    () => ({
      selected,
      count: selected.size,
      toggle,
      setMany,
      clear,
      replace,
    }),
    [selected, toggle, setMany, clear, replace],
  );
}
