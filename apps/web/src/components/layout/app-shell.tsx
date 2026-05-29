import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from "react";

import { Sidebar } from "@/components/layout/sidebar";
import { TopBar } from "@/components/layout/top-bar";

// Phase 4 / Sprint 1 surfaces 4.1 + 4.2 + 4.3: AppShell, Sidebar, TopBar.
//
// Layout grid (per DESIGN.md "App shell" spec - 240px sidebar, 48px topbar,
// 24/32 content padding):
//
//   ┌────────────┬──────────────────────────────┐
//   │            │ TopBar (48px)                │
//   │ Sidebar    ├──────────────────────────────┤
//   │ (240 / 64) │ Main (overflow-y-auto)       │
//   │            │                              │
//   └────────────┴──────────────────────────────┘
//
// Sidebar spans both rows; TopBar + Main share the right column. Borders, not
// shadows, carry the separation (DESIGN.md "Elevation & Depth").
//
// State:
//   - `collapsed` - desktop sidebar 240↔64. Persisted to
//     localStorage["wpmgr.sidebar.collapsed"]. The toggle never animates
//     width - the grid template column swaps discretely (DESIGN.md "Don't
//     animate width, height, padding, margin, top, or left").
//   - `mobileOpen` - narrow viewports (<768px) hide the sidebar by default;
//     the TopBar exposes a menu button that slides it in via transform.
//
// State is local React state + localStorage. No new global store - the
// project keeps server state in TanStack Query and ephemeral shell state in
// component state per the ADRs.

const COLLAPSED_KEY = "wpmgr.sidebar.collapsed";

interface ShellState {
  collapsed: boolean;
  toggleCollapsed: () => void;
  mobileOpen: boolean;
  setMobileOpen: (open: boolean) => void;
}

const ShellContext = createContext<ShellState | null>(null);

/**
 * Shell state hook. Components inside `<AppShell>` (Sidebar, TopBar, future
 * surfaces that need to know about the collapsed rail) read here. Throws if
 * called outside the provider - that's a programming error, not a runtime
 * one.
 */
export function useShellState(): ShellState {
  const ctx = useContext(ShellContext);
  if (!ctx) {
    throw new Error("useShellState must be used inside <AppShell>");
  }
  return ctx;
}

function readCollapsed(): boolean {
  if (typeof window === "undefined") return false;
  try {
    return window.localStorage.getItem(COLLAPSED_KEY) === "1";
  } catch {
    // Private mode / quota denied - fall back to expanded.
    return false;
  }
}

export function AppShell({ children }: { children: ReactNode }) {
  const [collapsed, setCollapsed] = useState<boolean>(readCollapsed);
  const [mobileOpen, setMobileOpen] = useState<boolean>(false);

  const toggleCollapsed = useCallback(() => {
    setCollapsed((prev) => {
      const next = !prev;
      try {
        window.localStorage.setItem(COLLAPSED_KEY, next ? "1" : "0");
      } catch {
        // Ignore storage failures; the toggle still works for the session.
      }
      return next;
    });
  }, []);

  // Lock body scroll while the mobile drawer is open so the underlying main
  // pane doesn't scroll behind it.
  useEffect(() => {
    if (!mobileOpen) return;
    const prev = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    return () => {
      document.body.style.overflow = prev;
    };
  }, [mobileOpen]);

  const shellState = useMemo<ShellState>(
    () => ({ collapsed, toggleCollapsed, mobileOpen, setMobileOpen }),
    [collapsed, toggleCollapsed, mobileOpen],
  );

  // Two static template strings so Tailwind extracts both at build time.
  const desktopColsClass = collapsed
    ? "md:grid-cols-[64px_1fr]"
    : "md:grid-cols-[240px_1fr]";

  return (
    <ShellContext.Provider value={shellState}>
      <div
        className={`grid h-dvh grid-cols-[1fr] grid-rows-[48px_1fr] bg-background text-foreground ${desktopColsClass}`}
      >
        <Sidebar />
        <TopBar />
        <main
          id="main-content"
          className="col-start-1 row-start-2 overflow-y-auto bg-background px-8 py-6 md:col-start-2"
        >
          {children}
        </main>
      </div>
    </ShellContext.Provider>
  );
}
