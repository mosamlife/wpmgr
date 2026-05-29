// Shell context + hook — extracted from app-shell.tsx so that the component
// file exports only components (react-refresh/only-export-components).
//
// `ShellContext` and `useShellState` are defined here; `AppShell` (in
// app-shell.tsx) imports `ShellContext` to provide the value, and consumers
// import `useShellState` from here (or from @/components/layout/app-shell
// which re-exports it at the same public path for backwards compatibility).

import { createContext, useContext } from "react";

export interface ShellState {
  collapsed: boolean;
  toggleCollapsed: () => void;
  mobileOpen: boolean;
  setMobileOpen: (open: boolean) => void;
}

export const ShellContext = createContext<ShellState | null>(null);

/**
 * Shell state hook. Components inside `<AppShell>` (Sidebar, TopBar, future
 * surfaces that need to know about the collapsed rail) read here. Throws if
 * called outside the provider — that is a programming error, not a runtime
 * one.
 */
export function useShellState(): ShellState {
  const ctx = useContext(ShellContext);
  if (!ctx) {
    throw new Error("useShellState must be used inside <AppShell>");
  }
  return ctx;
}
