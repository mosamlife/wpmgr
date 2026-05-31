import { useMemo } from "react";
import { useLocation, useNavigate, Link } from "@tanstack/react-router";
import {
  Bell,
  ChevronRight,
  HelpCircle,
  LogOut,
  Menu,
  PanelLeftClose,
  PanelLeftOpen,
  Search,
  UserRound,
} from "lucide-react";

import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { ThemeToggle } from "@/components/layout/theme-toggle";
import { useShellState } from "@/components/layout/app-shell-context";
import { useCommandPalette } from "@/features/command/use-command-palette";
import { useLogout, useMe } from "@/features/auth/use-auth";
import { useBulkAction } from "@/features/sites/use-bulk-action";
import { OrgSwitcher } from "@/features/orgs/org-switcher";
import { BUILD_VERSION } from "@/lib/build";
import { cn } from "@/lib/utils";

// Phase 4 / Sprint 1 surface 4.3 - top bar.
//
// 48px tall, 3-column grid: breadcrumb (1fr) | command-palette placeholder
// (auto) | right cluster (auto). Per DESIGN.md "App shell" + PRODUCT.md
// "calm, clinical, operator-grade": no avatar image, no logo lockup, no
// hero copy.
//
// The command palette button is a non-functional placeholder - cmdk wiring
// lands in Sprint 3.

export function TopBar() {
  const { collapsed, toggleCollapsed, mobileOpen, setMobileOpen } =
    useShellState();
  const location = useLocation();
  const breadcrumb = useBreadcrumb(location.pathname);

  return (
    <header
      // Sits in the right column on desktop, top row on mobile. Border-b
      // separates it from the main pane (DESIGN.md "Borders over shadows").
      className="col-start-1 row-start-1 grid h-12 grid-cols-[1fr_auto_auto] items-center gap-3 border-b border-border bg-background px-6 md:col-start-2"
    >
      {/* LEFT: mobile menu toggle + org switcher + breadcrumb + desktop collapse toggle. */}
      <div className="flex min-w-0 items-center gap-2">
        {/* Mobile drawer toggle (visible <md). */}
        <Button
          type="button"
          variant="ghost"
          size="icon"
          className="md:hidden"
          aria-label={mobileOpen ? "Close navigation" : "Open navigation"}
          aria-expanded={mobileOpen}
          aria-controls="primary-navigation"
          onClick={() => setMobileOpen(!mobileOpen)}
        >
          <Menu aria-hidden="true" />
        </Button>

        {/* Desktop collapse toggle (visible >=md). */}
        <Button
          type="button"
          variant="ghost"
          size="icon"
          className="hidden md:inline-flex"
          aria-label={collapsed ? "Expand sidebar" : "Collapse sidebar"}
          aria-pressed={collapsed}
          onClick={toggleCollapsed}
        >
          {collapsed ? (
            <PanelLeftOpen aria-hidden="true" />
          ) : (
            <PanelLeftClose aria-hidden="true" />
          )}
        </Button>

        {/* Org switcher — allows switching the active organisation. */}
        <OrgSwitcher />

        <Breadcrumb crumbs={breadcrumb} />
      </div>

      {/* CENTER: ⌘K search placeholder. Sprint 3 wires cmdk. */}
      <CommandPalettePlaceholder />

      {/* RIGHT cluster: build badge, theme, bell, help, user menu. */}
      <div className="flex items-center gap-3">
        <code
          data-testid="header-build-badge"
          aria-label={`UI build ${BUILD_VERSION}`}
          className="hidden font-mono text-xs text-muted-foreground sm:inline"
        >
          {BUILD_VERSION}
        </code>
        <ThemeToggle />
        <NotificationsBell />
        <Button
          type="button"
          variant="ghost"
          size="icon"
          aria-label="Help and docs"
          // TODO(sprint-3): open the docs drawer or external help.
          onClick={() => {
            if (import.meta.env.DEV) console.debug("[topbar] help clicked");
          }}
        >
          <HelpCircle aria-hidden="true" />
        </Button>
        <UserMenu />
      </div>
    </header>
  );
}

// ── Breadcrumb ───────────────────────────────────────────────────────────────

interface Crumb {
  label: string;
  /** Absolute pathname this crumb links to, or null for the leaf. */
  to: string | null;
}

/**
 * Derive a humanized breadcrumb from the current pathname. We don't (yet)
 * pull titles off route static data - sprint 2 may add that - so we
 * humanize the path segments directly. Param segments (e.g. /sites/$siteId)
 * are reasonably rare at the top level; we render the raw id as the leaf
 * which is what an operator wants anyway.
 *
 * Separator is ">" (a `ChevronRight` icon), never an em dash.
 */
function useBreadcrumb(pathname: string): Crumb[] {
  return useMemo(() => {
    const segments = pathname.split("/").filter(Boolean);
    if (segments.length === 0) return [{ label: "Home", to: null }];
    const crumbs: Crumb[] = [];
    let acc = "";
    segments.forEach((segment, i) => {
      acc += `/${segment}`;
      const isLast = i === segments.length - 1;
      crumbs.push({
        label: humanize(segment),
        to: isLast ? null : acc,
      });
    });
    return crumbs;
  }, [pathname]);
}

const TITLES: Record<string, string> = {
  sites: "Sites",
  updates: "Updates",
  backups: "Backups",
  migrations: "Migrations",
  uptime: "Uptime",
  performance: "Performance",
  vulnerabilities: "Vulnerabilities",
  audit: "Audit",
  settings: "Settings",
  alerts: "Alerts",
  "api-keys": "API keys",
};

function humanize(segment: string): string {
  if (TITLES[segment]) return TITLES[segment];
  // Path params arrive as raw values (e.g. an ID). Display them in mono via
  // the consumer; here we just pass through.
  return segment;
}

function Breadcrumb({ crumbs }: { crumbs: Crumb[] }) {
  return (
    <nav aria-label="Breadcrumb" className="min-w-0">
      <ol className="flex min-w-0 items-center gap-1.5 text-sm font-medium text-foreground">
        {crumbs.map((crumb, i) => {
          const isLast = i === crumbs.length - 1;
          return (
            <li key={`${crumb.label}-${i}`} className="flex min-w-0 items-center gap-1.5">
              {i > 0 ? (
                <ChevronRight
                  aria-hidden="true"
                  className="size-4 shrink-0 text-muted-foreground"
                />
              ) : null}
              {crumb.to && !isLast ? (
                <Link
                  to={crumb.to}
                  className={cn(
                    "truncate rounded-sm text-muted-foreground hover:text-foreground",
                    "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2",
                  )}
                >
                  {crumb.label}
                </Link>
              ) : (
                <span
                  className="truncate"
                  aria-current={isLast ? "page" : undefined}
                >
                  {crumb.label}
                </span>
              )}
            </li>
          );
        })}
      </ol>
    </nav>
  );
}

// ── Command-palette placeholder ─────────────────────────────────────────────

// Sprint 3 surface 4.4 wires this to the cmdk-backed CommandPalette mounted
// by AppShell. Clicking opens the palette; the keyboard shortcut (⌘K on Mac,
// Ctrl-K elsewhere) is owned by the provider so it works everywhere.
function CommandPalettePlaceholder() {
  const { setOpen } = useCommandPalette();
  return (
    <button
      type="button"
      onClick={() => setOpen(true)}
      aria-label="Open command palette"
      aria-keyshortcuts="Meta+K Control+K"
      className={cn(
        "hidden h-9 w-full max-w-[28rem] items-center gap-2 rounded-md border border-border bg-muted px-3 text-sm text-muted-foreground md:inline-flex",
        "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2",
        "hover:text-foreground",
      )}
    >
      <Search aria-hidden="true" className="size-4 shrink-0" />
      <span className="flex-1 truncate text-left">Search sites, runs, snapshots</span>
      <kbd className="rounded border border-border bg-background px-1.5 font-mono text-xs text-muted-foreground">
        ⌘K
      </kbd>
    </button>
  );
}

// ── Notifications bell ──────────────────────────────────────────────────────

/**
 * Notifications bell. Sprint 3 wires this to the BulkActionProvider: when
 * one or more bulk runs are in-flight, a small red dot appears in the
 * top-right of the icon and clicking the bell re-opens the most recent
 * un-settled run. With no in-flight runs the bell is a placeholder until
 * the broader notifications center lands (post-Phase-4).
 */
function NotificationsBell() {
  const { inFlightCount, reopenLatest } = useBulkAction();
  const hasInFlight = inFlightCount > 0;
  const label = hasInFlight
    ? `Notifications (${inFlightCount} update${
        inFlightCount === 1 ? "" : "s"
      } in progress)`
    : "Notifications";
  return (
    <Button
      type="button"
      variant="ghost"
      size="icon"
      aria-label={label}
      className="relative"
      onClick={() => {
        if (hasInFlight) {
          reopenLatest();
          return;
        }
        if (import.meta.env.DEV) console.debug("[topbar] notifications clicked");
      }}
    >
      <Bell aria-hidden="true" />
      {hasInFlight ? (
        <span
          aria-hidden="true"
          className="absolute -top-1 -right-1 size-2 rounded-full bg-destructive"
        />
      ) : null}
    </Button>
  );
}

// ── User menu ────────────────────────────────────────────────────────────────

function UserMenu() {
  const { data: me } = useMe();
  const logout = useLogout();
  const navigate = useNavigate();

  if (!me) {
    // Guard should prevent this, but render a placeholder rather than crash.
    return null;
  }

  const displayName = me.user.name || me.user.email;

  function handleSignOut() {
    logout.mutate(undefined, {
      onSettled: () => void navigate({ to: "/login" }),
    });
  }

  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button
          type="button"
          variant="ghost"
          size="sm"
          aria-label={`Account menu for ${displayName}`}
          className="max-w-[12rem] gap-2"
        >
          {/* No avatar image (DESIGN.md: "no avatar in top bar"). A
              generic user icon flags this as the account control. */}
          <UserRound aria-hidden="true" className="size-4" />
          <span className="hidden truncate text-sm font-medium sm:inline">
            {displayName}
          </span>
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end" className="min-w-[14rem]">
        <DropdownMenuLabel>{displayName}</DropdownMenuLabel>
        <DropdownMenuSeparator />
        <DropdownMenuItem asChild>
          <Link to="/settings/account">Account settings</Link>
        </DropdownMenuItem>
        <DropdownMenuSeparator />
        <DropdownMenuItem
          onSelect={(event) => {
            event.preventDefault();
            handleSignOut();
          }}
          disabled={logout.isPending}
        >
          <LogOut aria-hidden="true" className="size-4" />
          <span>Sign out</span>
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}
