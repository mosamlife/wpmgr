import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from "react";

// Sprint 3 surface 4.4 — command palette open/close state.
//
// Lifted into a tiny Context (no Zustand) per the Sprint 3 brief: it's a single
// boolean plus open/close/toggle helpers, shared between the TopBar trigger and
// the palette dialog itself. The keyboard shortcut (⌘K on Mac, Ctrl-K
// elsewhere) is wired here so it works no matter which surface is focused.

interface CommandPaletteState {
  open: boolean;
  setOpen: (next: boolean) => void;
  toggle: () => void;
}

const CommandPaletteContext = createContext<CommandPaletteState | null>(null);

/** Hook for any descendant of `<CommandPaletteProvider>` to read or flip the palette. */
export function useCommandPalette(): CommandPaletteState {
  const ctx = useContext(CommandPaletteContext);
  if (!ctx) {
    throw new Error(
      "useCommandPalette must be used inside <CommandPaletteProvider>",
    );
  }
  return ctx;
}

interface ProviderProps {
  children: ReactNode;
}

export function CommandPaletteProvider({ children }: ProviderProps) {
  const [open, setOpen] = useState<boolean>(false);

  const toggle = useCallback(() => setOpen((prev) => !prev), []);

  // Global keyboard shortcut. ⌘K (Mac) / Ctrl-K (Win/Linux). We intentionally
  // do NOT swallow keys when an editable element is focused — cmdk's own input
  // is editable, and the operator may want to toggle the palette while focused
  // elsewhere too. We DO skip when modifier keys for browser shortcuts (alt,
  // shift) are involved.
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
