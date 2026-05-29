import {
  useCallback,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from "react";

import {
  CommandPaletteContext,
  type CommandPaletteState,
} from "@/features/command/use-command-palette";

// Sprint 3 surface 4.4 — Provider for the command palette.
//
// Owns the open boolean plus the global keyboard shortcut. Kept in its own
// file so the hook module remains hook-only (avoids the
// `react-refresh/only-export-components` HMR warning).

interface ProviderProps {
  children: ReactNode;
}

export function CommandPaletteProvider({ children }: ProviderProps) {
  const [open, setOpen] = useState<boolean>(false);

  const toggle = useCallback(() => setOpen((prev) => !prev), []);

  // Global keyboard shortcut. ⌘K (Mac) / Ctrl-K (Win/Linux). We intentionally
  // do NOT swallow keys when an editable element is focused — cmdk's own input
  // is editable, and the operator may want to toggle the palette while focused
  // elsewhere too. We DO skip when extra modifiers (alt/shift) would conflict
  // with native browser shortcuts.
  useEffect(() => {
    function handler(e: KeyboardEvent) {
      if (e.key !== "k" && e.key !== "K") return;
      if (!(e.metaKey || e.ctrlKey)) return;
      if (e.altKey || e.shiftKey) return;
      e.preventDefault();
      setOpen((prev) => !prev);
    }
    window.addEventListener("keydown", handler);
    return () => window.removeEventListener("keydown", handler);
  }, []);

  const value = useMemo<CommandPaletteState>(
    () => ({ open, setOpen, toggle }),
    [open, toggle],
  );

  return (
    <CommandPaletteContext.Provider value={value}>
      {children}
    </CommandPaletteContext.Provider>
  );
}
