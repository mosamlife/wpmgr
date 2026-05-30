import { useNavigate } from "@tanstack/react-router";
// `Command` is the namespace export from cmdk; it exposes Dialog, List, Group,
// Item, Input, Empty as static properties. Importing the namespace lets us read
// like `Command.Dialog` which matches the upstream docs verbatim.
import { Command } from "cmdk";
import {
  ArrowDownUp,
  Database,
  Globe,
  Key,
  LogOut,
  RefreshCw,
  Search,
  Settings as SettingsIcon,
} from "lucide-react";
import { type ReactNode, useMemo } from "react";

import { useCommandPalette } from "@/features/command/use-command-palette";
import { useRecentSites } from "@/features/command/use-recent-sites";
import { useLogout } from "@/features/auth/use-auth";
import { useSitesSelection } from "@/features/sites/use-sites-selection";
import { cn } from "@/lib/utils";

// Sprint 3 surface 4.4 — Command palette.
//
// cmdk (Radix Dialog + a headless command-menu primitive) powers the
// keyboard-first omnibar. The dialog handles the scrim, focus trap, and Escape
// for us; we layer the WPMgr visual language on top via Tailwind utilities and
// tokens declared in styles/globals.css.
//
// Items are verbs ("Update plugins on 47 sites", "Sign out") per PRODUCT.md.
// Group headings are nouns ("Navigate", "Run on selected", "Run on all",
// "Settings"). Empty selection silently omits the "Run on selected" group —
// cmdk handles all-disabled groups poorly, and the operator gets no signal
// from a disabled stub anyway.
//
// Visual:
//   • Overlay  → fixed inset-0, bg-[var(--scrim)] (Sprint 1 token)
//   • Panel    → popover surface, 12px radius (modal radius per DESIGN.md),
//                border, shadow-lg, max-w-2xl. Centred horizontally, 8rem
//                from the top so it doesn't feel anchored to the title.
//   • Input    → 48px tall, leading Search icon, border-b separator.
//   • Items    → 6px radius, aria-selected highlight via cmdk.
//   • Footer   → quiet keyboard hint at the bottom.
//
// Motion:
//   • 180ms scale 0.96→1 + fade (matches --duration-fast and the
//     "fade-in-0 zoom-in-95" idiom already used by the dropdown menu).
//   • Reduced-motion fallback inherits the global @media query in
//     globals.css; we add no extra opt-out.

interface CommandPaletteProps {
  /** When false, returns null — keeps the cmdk subtree out of the DOM. */
  open: boolean;
  /** Fired when the dialog requests close (Escape, scrim click, selection). */
  onClose: () => void;
}

export function CommandPalette({ open, onClose }: CommandPaletteProps) {
  const navigate = useNavigate();
  const recentSites = useRecentSites();
  const selection = useSitesSelection();
  const selectedCount = selection.count;
  const logout = useLogout();

  // Wrap navigate-and-close in one callback per command so onSelect stays
  // declarative below.
  const go = useMemo(
    () =>
      function go(to: string): () => void {
        return () => {
          onClose();
          // eslint-disable-next-line @typescript-eslint/no-explicit-any
          void navigate({ to: to as any });
        };
      },
    [navigate, onClose],
  );

  function handleSignOut(): void {
    onClose();
    logout.mutate(undefined, {
      onSettled: () => {
        // eslint-disable-next-line @typescript-eslint/no-explicit-any
        void navigate({ to: "/login" as any });
      },
    });
  }

  // The Sprint 2 wizard ("Update X selected sites") is driven from the Sites
  // page route. The palette can't open it directly without lifting wizard
  // state; for now, the verb routes the operator there and the toolbar
  // pre-selection drives the wizard. Same logic for the "Run on all" group —
  // we navigate, the destination surface confirms.
  function runOnSelected(): void {
    onClose();
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    void navigate({ to: "/sites" as any });
  }

  return (
    <Command.Dialog
      open={open}
      onOpenChange={(next) => {
        if (!next) onClose();
      }}
      label="Command palette"
      // cmdk forwards `overlayClassName` and `contentClassName` to its inner
      // Radix Dialog. `Command.Dialog` internally renders the cmdk Command root
      // inside Dialog.Content — so we DON'T add a second `<Command>` wrapper
      // (that would create two contexts and items would register against the
      // wrong one). All styling lands on `contentClassName`.
      overlayClassName={cn(
        // `wpmgr-cmdk-overlay` is the keyframe hook (see styles/globals.css).
        // cmdk sets data-state="open"|"closed" on this element which drives the
        // 180ms fade. The class also pins the scrim full-viewport.
        "wpmgr-cmdk-overlay fixed inset-0 z-50 bg-[var(--scrim)]",
      )}
      contentClassName={cn(
        // Outer positioner. Centred horizontally, 8rem from the top — feels
        // like an omnibar rather than a modal. The px-4 on this wrapper plus
        // the panel itself filling w-full keeps it inside 100vw on mobile.
        "wpmgr-cmdk-panel fixed left-1/2 top-32 z-50 w-[calc(100vw-2rem)] max-w-2xl -translate-x-1/2",
        "focus-visible:outline-none",
      )}
    >
      <div
        // The visible floating surface: popover background, 12px radius (modal
        // shape per DESIGN.md), 1px border, shadow-lg. Nested inside the cmdk
        // Command root that `Command.Dialog` already provides.
        className={cn(
          "flex w-full flex-col overflow-hidden rounded-xl border border-border",
          "bg-popover text-popover-foreground shadow-lg",
        )}
      >
        {/* INPUT row. 48px tall, leading icon, border-b separator. */}
        <div className="flex items-center gap-2 border-b border-border px-4">
          <Search
            aria-hidden="true"
            className="size-4 shrink-0 text-muted-foreground"
          />
          <Command.Input
            placeholder="Search sites, runs, snapshots"
            className={cn(
              "h-12 flex-1 bg-transparent text-base outline-none",
              "placeholder:text-muted-foreground",
            )}
          />
        </div>

        <Command.List
          className={cn(
            "max-h-[24rem] overflow-y-auto p-2",
            // Internal padding around groups, not in the group component, so
            // empty state aligns visually with the input.
          )}
        >
          <Command.Empty
            className="px-3 py-6 text-center text-sm text-muted-foreground"
          >
            No matches.
          </Command.Empty>

          {/* ── Navigate ────────────────────────────────────────────────── */}
          <PaletteGroup heading="Navigate">
            <PaletteItem onSelect={go("/sites")} icon={<Globe className="size-4" aria-hidden="true" />}>
              Go to Sites
            </PaletteItem>
            <PaletteItem
              onSelect={go("/updates")}
              icon={<RefreshCw className="size-4" aria-hidden="true" />}
            >
              Go to Updates
            </PaletteItem>
            <PaletteItem
              onSelect={go("/backups")}
              icon={<Database className="size-4" aria-hidden="true" />}
            >
              Go to Backups
            </PaletteItem>
            {recentSites.map((site) => (
              <PaletteItem
                key={`recent-${site.id}`}
                onSelect={go(`/sites/${site.id}`)}
                icon={<Globe className="size-4" aria-hidden="true" />}
                // The hostname is the user-facing identifier — keep it mono so
                // it reads as a fixed-width URL fragment per DESIGN.md ("use
                // mono for every hostname").
                trailing={
                  <span className="font-mono text-xs text-muted-foreground">
                    {site.hostname}
                  </span>
                }
              >
                Go to {site.hostname}
              </PaletteItem>
            ))}
          </PaletteGroup>

          {/* ── Run on selected (only when there IS a selection) ───────── */}
          {selectedCount > 0 ? (
            <PaletteGroup heading={`Run on selected (${selectedCount})`}>
              <PaletteItem
                onSelect={runOnSelected}
                icon={<RefreshCw className="size-4" aria-hidden="true" />}
              >
                Update plugins on {selectedCount} sites
              </PaletteItem>
              <PaletteItem
                onSelect={runOnSelected}
                icon={<Database className="size-4" aria-hidden="true" />}
              >
                Run backup on {selectedCount} sites
              </PaletteItem>
            </PaletteGroup>
          ) : null}

          {/* ── Run on all ─────────────────────────────────────────────── */}
          <PaletteGroup heading="Run on all">
            <PaletteItem
              onSelect={go("/backups")}
              icon={<Database className="size-4" aria-hidden="true" />}
            >
              Run backup on all sites
            </PaletteItem>
            <PaletteItem
              onSelect={go("/sites")}
              icon={<ArrowDownUp className="size-4" aria-hidden="true" />}
            >
              Sync metadata on all sites
            </PaletteItem>
          </PaletteGroup>

          {/* ── Settings ───────────────────────────────────────────────── */}
          <PaletteGroup heading="Settings">
            <PaletteItem
              onSelect={go("/settings/api-keys")}
              icon={<Key className="size-4" aria-hidden="true" />}
            >
              Manage API keys
            </PaletteItem>
            <PaletteItem
              onSelect={go("/settings/alerts")}
              icon={<SettingsIcon className="size-4" aria-hidden="true" />}
            >
              Configure alerts
            </PaletteItem>
            <PaletteItem
              onSelect={handleSignOut}
              icon={<LogOut className="size-4" aria-hidden="true" />}
            >
              Sign out
            </PaletteItem>
          </PaletteGroup>
        </Command.List>

        {/* FOOTER hint. Quiet, mono kbd glyphs so the operator learns the
            keyboard model without the panel feeling decorative. */}
        <div
          className={cn(
            "flex items-center justify-between gap-3 border-t border-border px-3 py-2",
            "text-xs text-muted-foreground",
          )}
        >
          <span className="flex items-center gap-3">
            <span className="flex items-center gap-1">
              <kbd className="rounded border border-border bg-background px-1.5 font-mono">
                ↑↓
              </kbd>
              navigate
            </span>
            <span className="flex items-center gap-1">
              <kbd className="rounded border border-border bg-background px-1.5 font-mono">
                ↵
              </kbd>
              select
            </span>
            <span className="flex items-center gap-1">
              <kbd className="rounded border border-border bg-background px-1.5 font-mono">
                esc
              </kbd>
              close
            </span>
          </span>
          <span className="font-mono">⌘K</span>
        </div>
      </div>
    </Command.Dialog>
  );
}

/**
 * Mounting shim: the TopBar trigger needs to render the palette regardless of
 * its open state (so the close animation runs). We do it here, reading state
 * from the provider so the consumer is just `<MountedCommandPalette />`.
 */
export function MountedCommandPalette() {
  const { open, setOpen } = useCommandPalette();
  return <CommandPalette open={open} onClose={() => setOpen(false)} />;
}

// ── Internal building blocks ────────────────────────────────────────────────

interface PaletteGroupProps {
  heading: string;
  children: ReactNode;
}

function PaletteGroup({ heading, children }: PaletteGroupProps) {
  return (
    <Command.Group
      heading={heading}
      // The heading slot is wrapped by cmdk in a `[cmdk-group-heading]` div.
      // We style that via a child selector so the visual is the spec-stated
      // "uppercase, tracking-wide, muted" caption — no extra wrapper.
      className={cn(
        "mb-1 [&_[cmdk-group-heading]]:px-3 [&_[cmdk-group-heading]]:py-2",
        "[&_[cmdk-group-heading]]:text-xs [&_[cmdk-group-heading]]:font-medium",
        "[&_[cmdk-group-heading]]:uppercase [&_[cmdk-group-heading]]:tracking-wide",
        "[&_[cmdk-group-heading]]:text-muted-foreground",
      )}
    >
      {children}
    </Command.Group>
  );
}

interface PaletteItemProps {
  onSelect: () => void;
  icon?: ReactNode;
  trailing?: ReactNode;
  children: ReactNode;
}

function PaletteItem({ onSelect, icon, trailing, children }: PaletteItemProps) {
  return (
    <Command.Item
      onSelect={onSelect}
      className={cn(
        // 6px radius, 12/8 padding, body-sm sizing — matches DESIGN.md item
        // shape ("rectangles with 6px radius", "8/12 padding").
        "flex cursor-pointer items-center gap-2 rounded-md px-3 py-2 text-sm",
        "text-foreground outline-none",
        // cmdk sets aria-selected on the active item; that's our hover/focus
        // tone too. Keeps mouse + keyboard visuals consistent.
        "aria-selected:bg-accent aria-selected:text-accent-foreground",
        "transition-colors duration-100",
      )}
    >
      {icon ? (
        <span className="grid size-5 shrink-0 place-items-center text-muted-foreground aria-selected:text-accent-foreground">
          {icon}
        </span>
      ) : null}
      <span className="min-w-0 flex-1 truncate">{children}</span>
      {trailing ? <span className="ml-auto shrink-0">{trailing}</span> : null}
    </Command.Item>
  );
}
