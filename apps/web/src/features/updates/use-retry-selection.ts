import { useCallback, useMemo, useState } from "react";

import {
  isDefaultRetrySelected,
  isRetrySelectable,
  type RetryTask,
} from "./retry-contract";

// GH #336 selection state for the run detail task table.
//
// PAGE LOCAL ON PURPOSE. `useSitesSelection` is a module-level singleton keyed
// by SITE id, deliberately shared with the command palette, so putting TASK
// ids in it would corrupt the Sites page selection. This mirrors the idiom
// (a plain Set, a header select-all, per-row toggle) without sharing its store,
// exactly like the media page's own page-local selection.
//
// Two invariants:
//
//   1. UNTOUCHED SELECTION IS DERIVED. Before the operator touches anything,
//      the selection IS the server's default set (`retry_class` of `failed` or
//      `never_ran`), recomputed from whatever the SSE stream has patched into
//      the cache. A task that fails while the operator reads the page joins
//      the default selection instead of being silently left out.
//   2. TOUCHED SELECTION IS EXPLICIT AND NEVER RECONCILED against what is on
//      screen. Ticking one row must not re-derive the rest.
//
// The effective set is always re-filtered through `isRetrySelectable` at read
// time: a task selected while it was terminal can be superseded by a later
// frame, and the count on the button must be the count of things that will
// actually be requested.
//
// SCALE, stated rather than assumed. GET /api/v1/updates/{runId} does not
// paginate, so the task array is always complete and a select-all is sound at
// any run size:
//
//   21 tasks    a Set of 21; the header checkbox is the whole story.
//   300 tasks   a Set of 300; still one pass per render, no per-row request.
//               The table is not virtualised (it never was), so this is a
//               long page, which is why the action bar is sticky.
//   2000 tasks  a Set of 2000; selection stays O(n) with no server call, and
//               the request body is 2000 uuids, about 74 KB, under the
//               contract's 5000 id ceiling. Above 5000 the control plane
//               answers 422 `too_many_tasks` and its own sentence is what the
//               dialog shows; this client does not invent a second limit.
//
// Screen readers: the action bar owns one polite live region, debounced, so a
// select-all announces the settled count once instead of reading every
// intermediate number.

export interface RetrySelection {
  /** Raw selected task ids, including any that have since become unselectable. */
  selected: ReadonlySet<string>;
  /** Selected tasks that are still server-selectable right now. */
  selectedTasks: RetryTask[];
  /** Effective count. THE UNIT IS TASKS. */
  count: number;
  /** Every task the server says may be retried. */
  selectableTasks: RetryTask[];
  allSelectableSelected: boolean;
  someSelectableSelected: boolean;
  isSelected: (id: string) => boolean;
  toggle: (id: string) => void;
  /** Select or clear every selectable task in one action. */
  setAllSelectable: (next: boolean) => void;
  clear: () => void;
}

export function useRetrySelection(tasks: readonly RetryTask[]): RetrySelection {
  const [explicit, setExplicit] = useState<ReadonlySet<string> | null>(null);

  const selectableTasks = useMemo(
    () => tasks.filter((task) => isRetrySelectable(task)),
    [tasks],
  );

  const defaults = useMemo(
    () =>
      new Set(
        tasks.filter((task) => isDefaultRetrySelected(task)).map((t) => t.id),
      ),
    [tasks],
  );

  const selected = explicit ?? defaults;

  const selectedTasks = useMemo(
    () => selectableTasks.filter((task) => selected.has(task.id)),
    [selectableTasks, selected],
  );

  const toggle = useCallback(
    (id: string) => {
      setExplicit((prev) => {
        const next = new Set(prev ?? defaults);
        if (next.has(id)) {
          next.delete(id);
        } else {
          next.add(id);
        }
        return next;
      });
    },
    [defaults],
  );

  const setAllSelectable = useCallback(
    (next: boolean) => {
      setExplicit(
        next ? new Set(selectableTasks.map((task) => task.id)) : new Set(),
      );
    },
    [selectableTasks],
  );

  const clear = useCallback(() => {
    setExplicit(new Set());
  }, []);

  const isSelected = useCallback((id: string) => selected.has(id), [selected]);

  const allSelectableSelected =
    selectableTasks.length > 0 &&
    selectedTasks.length === selectableTasks.length;
  const someSelectableSelected =
    selectedTasks.length > 0 && !allSelectableSelected;

  return {
    selected,
    selectedTasks,
    count: selectedTasks.length,
    selectableTasks,
    allSelectableSelected,
    someSelectableSelected,
    isSelected,
    toggle,
    setAllSelectable,
    clear,
  };
}
