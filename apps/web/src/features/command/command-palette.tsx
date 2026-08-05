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
  PackageCheck,
  RefreshCw,
  Search,
  Settings as SettingsIcon,
} from "lucide-react";
import { type ReactNode, useCallback, useMemo, useState } from "react";

import { useCommandPalette } from "@/features/command/use-command-palette";
import { hostnameFromUrl, useRecentSites } from "@/features/command/use-recent-sites";
import { canManage, useLogout, useMe } from "@/features/auth/use-auth";
import { useFleetAgentVersions } from "@/features/fleet/use-fleet-agents";
import { useSitesSelection } from "@/features/sites/use-sites-selection";
import { useSites } from "@/features/sites/use-sites";
import { useBulkBackup } from "@/features/backups/use-bulk-backup";
import { toast } from "@/components/toast";
import { useDebouncedValue } from "@/lib/use-debounced-value";
import { cn } from "@/lib/utils";
import type { Site } from "@wpmgr/api";

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

/**
 * GH #349. How many site results one search shows. The panel is 24rem tall;
 * past a screenful the operator refines the term rather than scrolls, and the
 * whole point of the server search is that refining now reaches every site in
 * the organisation instead of a preloaded page.
 */
const PALETTE_SEARCH_LIMIT = 20;

export function CommandPalette({ open, onClose }: CommandPaletteProps) {
  const navigate = useNavigate();
  const recentSites = useRecentSites();
  const selection = useSitesSelection();
  const selectedCount = selection.count;
  const logout = useLogout();
  const bulkBackup = useBulkBackup();
  // The sites query is already loaded by the app shell; this hits the TanStack
  // Query cache (no extra network call) and gives us the enrolled site IDs for
  // "Run backup on all sites".
  const { data: allSites } = useSites();

  // ── Site search (GH #349) ─────────────────────────────────────────────────
  //
  // The palette used to offer ONLY `recentSites`, a localStorage list of sites
  // this browser had already opened. An agency with 24 enrolled sites and none
  // visited got "No matches." for every one of them, which was the honest
  // answer to the wrong question.
  //
  // The term now goes to the server, so a search reaches every site in the
  // organisation. Recents stay as the ZERO-QUERY default: with an empty box
  // the useful thing to show is where the operator just was.
  const [term, setTerm] = useState("");
  const trimmedTerm = term.trim();
  const hasTerm = trimmedTerm.length > 0;
  const searchTerm = useDebouncedValue(trimmedTerm, 200);
  const siteSearch = useSites({
    q: searchTerm,
    limit: PALETTE_SEARCH_LIMIT,
    enabled: searchTerm.length > 0,
  });
  const results = hasTerm ? (siteSearch.data ?? []) : [];
  // A disabled query reports `isPending` forever, so `isFetching` is what
  // actually means "a request is in flight". The window between the keystroke
  // and the debounce firing counts as searching too: that is exactly the
  // moment the old palette would have said "No matches."
  const isSearching =
    hasTerm && (siteSearch.isFetching || trimmedTerm !== searchTerm);
  const searchFailed = hasTerm && !isSearching && siteSearch.isError;

  // GH #322: is the fleet agent rollout actually usable by THIS viewer?
  //
  // Both halves, matching the gate on the bulk action itself
  // (routes/_authed/sites/index.tsx:907). self_update_enabled is the control
  // plane's kill switch, and an absent value reads as off rather than on.
  // canManage is owner or admin: the channel is infrastructure, so a viewer
  // who cannot run it must not be offered a shortcut to it.
  const { data: me } = useMe();
  const { data: fleetAgents } = useFleetAgentVersions();
  const agentRolloutAvailable =
    fleetAgents?.self_update_enabled === true && canManage(me);

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
  // pre-selection drives the wizard.
  function runOnSelected(): void {
    onClose();
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    void navigate({ to: "/sites" as any });
  }

  /** Enqueue backups for the currently selected sites, then navigate to /backups. */
  const runBackupOnSelected = useCallback(async () => {
    onClose();
    const ids = Array.from(selection.selected);
    if (ids.length === 0) return;
    const { enqueued, skipped, failed } = await bulkBackup(ids);
    const parts: string[] = [];
    if (enqueued.length > 0) parts.push(`${enqueued.length} queued`);
    if (skipped.length > 0) parts.push(`${skipped.length} already running`);
    if (failed.length > 0) parts.push(`${failed.length} failed`);
    if (failed.length > 0 && enqueued.length === 0 && skipped.length === 0) {
      toast.error(`Backup failed for all ${failed.length} sites`, {
        description: "Check site connections and try again.",
      });
    } else {
      toast.success(`Backup started for ${enqueued.length + skipped.length} of ${ids.length} sites`, {
        description: parts.join(", "),
      });
    }
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    void navigate({ to: "/backups" as any });
  }, [onClose, selection, bulkBackup, navigate]);

  /** Enqueue backups for ALL enrolled active sites, then navigate to /backups. */
  const runBackupOnAll = useCallback(async () => {
    onClose();
    const ids = (allSites ?? []).map((s) => s.id);
    if (ids.length === 0) {
      toast.info("No enrolled sites found");
      return;
    }
    const { enqueued, skipped, failed } = await bulkBackup(ids);
    const parts: string[] = [];
    if (enqueued.length > 0) parts.push(`${enqueued.length} queued`);
    if (skipped.length > 0) parts.push(`${skipped.length} already running`);
    if (failed.length > 0) parts.push(`${failed.length} failed`);
    if (failed.length > 0 && enqueued.length === 0 && skipped.length === 0) {
      toast.error(`Backup failed for all ${failed.length} sites`, {
        description: "Check site connections and try again.",
      });
    } else {
      toast.success(`Backup started for ${enqueued.length + skipped.length} of ${ids.length} sites`, {
        description: parts.join(", "),
      });
    }
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    void navigate({ to: "/backups" as any });
  }, [onClose, allSites, bulkBackup, navigate]);

  return (
    <Command.Dialog
      open={open}
      onOpenChange={(next) => {
        if (!next) {
          // Reset the term on close so the next ⌘K opens on recents rather
          // than on a stale search. Done in the event handler, never an
          // effect.
          setTerm("");
          onClose();
        }
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
            value={term}
            onValueChange={setTerm}
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
          {/*
            With a term typed, the Sites section below owns the whole story
            for that term (searching / failed / nothing found), so cmdk's own
            "No matches." must not also render: two verdicts on one screen,
            one of them written before the request came back, is the bug this
            issue reported.
          */}
          {hasTerm ? null : (
            <Command.Empty className="px-3 py-6 text-center text-sm text-muted-foreground">
              No matches.
            </Command.Empty>
          )}

          {/* ── Sites (server search, GH #349) ──────────────────────────── */}
          {hasTerm ? (
            <SiteSearchSection
              term={trimmedTerm}
              results={results}
              isSearching={isSearching}
              hasFailed={searchFailed}
              onRetry={() => void siteSearch.refetch()}
              onOpen={(siteId) => go(`/sites/${siteId}`)()}
            />
          ) : null}

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
            {/*
              Recents are the ZERO-QUERY default only. Once something is
              typed, the Sites section above answers from the server across the
              whole organisation, and a second list of the same sites drawn
              from this browser's history would just compete with it.
            */}
            {hasTerm
              ? null
              : recentSites.map((site) => (
                  <PaletteItem
                    key={`recent-${site.id}`}
                    onSelect={go(`/sites/${site.id}`)}
                    icon={<Globe className="size-4" aria-hidden="true" />}
                    // The hostname is the user-facing identifier, so keep it mono
                    // so it reads as a fixed-width URL fragment per DESIGN.md
                    // ("use mono for every hostname").
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
                onSelect={() => { void runBackupOnSelected(); }}
                icon={<Database className="size-4" aria-hidden="true" />}
              >
                Run backup on {selectedCount} sites
              </PaletteItem>
            </PaletteGroup>
          ) : null}

          {/* ── Run on all ─────────────────────────────────────────────── */}
          <PaletteGroup heading="Run on all">
            <PaletteItem
              onSelect={() => { void runBackupOnAll(); }}
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
            {/*
              GH #322. The fleet agent rollout is the third action with this
              exact shape and audience, and it was the only one that still
              needed selecting sites plus opening a dropdown to reach.

              It NAVIGATES rather than firing, unlike "Run backup on all
              sites". A wave-gated rollout that touches every agent in the
              fleet is not something to start from a fuzzy match on a
              keystroke, and the Sites page is where the canary and wave
              structure are explained before it runs. That is the same choice
              "Sync metadata on all sites" already makes.

              Rendered only when the channel is actually usable, on the same
              pair the bulk action itself is gated on
              (routes/_authed/sites/index.tsx:907): the control plane's
              self-update switch AND owner or admin. Offering it otherwise
              would point at an action that is not there when you arrive.
            */}
            {agentRolloutAvailable ? (
              <PaletteItem
                onSelect={go("/sites?agentStatus=Outdated")}
                icon={<PackageCheck className="size-4" aria-hidden="true" />}
              >
                Update agent on all sites
              </PaletteItem>
            ) : null}
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
              onSelect={go("/alerts")}
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

// ── Site search results (GH #349) ───────────────────────────────────────────

interface SiteSearchSectionProps {
  /** The trimmed term, echoed back in the no-results line. */
  term: string;
  results: readonly Site[];
  isSearching: boolean;
  hasFailed: boolean;
  onRetry: () => void;
  onOpen: (siteId: string) => void;
}

/**
 * The three states a search can be in, told apart on screen.
 *
 * The reported bug was a palette that said "No matches." whatever was
 * happening, so the operator could not tell "your organisation has no site
 * called that" from "we have not asked yet" or "the request failed". Each
 * gets its own line here, and only the settled, empty, successful case is
 * allowed to say nothing was found.
 *
 * The status and error lines are plain elements rather than Command.Items:
 * cmdk filters items against the typed term, and a status line that vanished
 * because it did not fuzzy-match "iacop" would be its own small lie. Site
 * rows use `forceMount` for the same reason. The SERVER decided these sites
 * match; the client must not quietly overrule it with a second, weaker filter,
 * which is the shape of the defect this whole change is undoing.
 */
function SiteSearchSection({
  term,
  results,
  isSearching,
  hasFailed,
  onRetry,
  onOpen,
}: SiteSearchSectionProps) {
  if (hasFailed) {
    return (
      <div
        role="alert"
        className="flex flex-wrap items-center justify-center gap-2 px-3 py-6 text-center text-sm text-muted-foreground"
      >
        <span>Could not search sites.</span>
        <button
          type="button"
          onClick={onRetry}
          className={cn(
            "rounded-sm font-medium text-primary underline-offset-4 hover:underline",
            "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
          )}
        >
          Try again
        </button>
      </div>
    );
  }

  if (results.length === 0) {
    return (
      <div
        role="status"
        aria-live="polite"
        className="px-3 py-6 text-center text-sm text-muted-foreground"
      >
        {isSearching ? "Searching sites..." : `No sites match "${term}".`}
      </div>
    );
  }

  return (
    <>
      <PaletteGroup heading="Sites" forceMount>
        {results.map((site) => {
          const hostname = hostnameFromUrl(site.url);
          return (
            <PaletteItem
              key={`site-${site.id}`}
              forceMount
              // Unique per site, and stable across renders, so cmdk's
              // selection does not jump as results come and go.
              value={`site-${site.id}`}
              // The same three fields the server matched on. cmdk scores
              // against these to order the results sensibly; `forceMount`
              // above is what guarantees they are SHOWN either way, so a
              // scoring disagreement can never hide a real match.
              keywords={[site.name, site.url, ...site.tags]}
              onSelect={() => onOpen(site.id)}
              icon={<Globe className="size-4" aria-hidden="true" />}
              trailing={
                <span className="font-mono text-xs text-muted-foreground">
                  {hostname}
                </span>
              }
            >
              Go to {site.name || hostname}
            </PaletteItem>
          );
        })}
      </PaletteGroup>
      {isSearching ? (
        <div
          role="status"
          aria-live="polite"
          className="px-3 pb-2 text-center text-xs text-muted-foreground"
        >
          Searching sites...
        </div>
      ) : null}
    </>
  );
}

// ── Internal building blocks ────────────────────────────────────────────────

interface PaletteGroupProps {
  heading: string;
  children: ReactNode;
  /** Render even when no child item matches the typed term (GH #349). */
  forceMount?: boolean;
}

function PaletteGroup({ heading, children, forceMount }: PaletteGroupProps) {
  return (
    <Command.Group
      heading={heading}
      forceMount={forceMount}
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
  /** Stable identity for cmdk; defaults to the rendered text. */
  value?: string;
  /** Extra text cmdk scores the term against (name, url, tags). */
  keywords?: string[];
  /** Render regardless of cmdk's own filter (GH #349 server results). */
  forceMount?: boolean;
}

function PaletteItem({
  onSelect,
  icon,
  trailing,
  children,
  value,
  keywords,
  forceMount,
}: PaletteItemProps) {
  return (
    <Command.Item
      onSelect={onSelect}
      value={value}
      keywords={keywords}
      forceMount={forceMount}
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
