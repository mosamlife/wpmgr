import { createContext, useContext } from "react";

// Sprint 3 surface 4.4 — context type + hook for the command palette's
// open/close state. The Provider lives in command-palette-provider.tsx so this
// module is hook-only (keeps Vite's react-refresh happy).

export interface CommandPaletteState {
  open: boolean;
  setOpen: (next: boolean) => void;
  toggle: () => void;
}

export const CommandPaletteContext = createContext<CommandPaletteState | null>(
  null,
);

/** Read or flip the palette from any descendant of `<CommandPaletteProvider>`. */
export function useCommandPalette(): CommandPaletteState {
  const ctx = useContext(CommandPaletteContext);
  if (!ctx) {
    throw new Error(
      "useCommandPalette must be used inside <CommandPaletteProvider>",
    );
  }
  return ctx;
}
