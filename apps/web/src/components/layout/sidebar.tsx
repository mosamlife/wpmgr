import { useEffect, useId, useRef, useState, type ReactNode } from "react";
import { Link, useLocation } from "@tanstack/react-router";
import {
  Activity,
  ArrowLeft,
  Building2,
  ChevronRight,
  Globe,
  LineChart,
  Mail,
  RefreshCw,
  Settings,
  Share2,
  Shield,
  ShieldAlert,
  TrendingUp,
  Users,
  type LucideIcon,
} from "lucide-react";

import { FleetHubLogo, Wordmark } from "@/components/brand/logo";
import { useShellState } from "@/components/layout/app-shell-context";
import { useMe, isSuperadmin } from "@/features/auth/use-auth";
import { useSites } from "@/features/sites/use-sites";
import { cn } from "@/lib/utils";

// Phase 4 / Sprint 1 surface 4.2 - primary navigation.
//
// Five groups in fixed order: Sites, Operations, Insights, Security,
// Settings (bottom-aligned). Per-item: 8px / 12px padding, 6px radius,
// active = accent + 2px left ring primary (DESIGN.md "Sidebar item").
//
// COLLAPSIBLE GROUPS (Operations, Insights, Security): the group header is a
// <button> that toggles open/closed. Default = collapsed, except the group
// containing the active route auto-expands on first render. Manual toggles
// persist to localStorage["wpmgr.sidebar.groups"] (label->boolean map).
// Resolution rule: persisted map entry wins; otherwise open iff active route.
//
// SETTINGS is now a single leaf link to /settings (the settings area owns its
// own left-sidebar for the 8 sub-sections). The 8 inline sub-items and the
// org-scoped filtering block are removed from the main sidebar.
//
// Collapsed state (64px rail): icons-only. Collapsible groups still show their
// sub-items as hover/focus flyouts (unchanged). Settings leaf = icon link.

const GROUPS_KEY = "wpmgr.sidebar.groups";

/** Reads the persisted open-state map from localStorage. Safe in private mode. */
function readGroupsMap(): Record<string, boolean> {
  if (typeof window === "undefined") return {};
  try {
    const raw = window.localStorage.getItem(GROUPS_KEY);
    if (!raw) return {};
    const parsed: unknown = JSON.parse(raw);
    if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) return {};
    return parsed as Record<string, boolean>;
  } catch {
    return {};
  }
}

/** Writes an updated entry to the persisted map. Safe in private mode. */
function writeGroupsMap(label: string, open: boolean): void {
  try {
    const current = readGroupsMap();
    window.localStorage.setItem(
      GROUPS_KEY,
      JSON.stringify({ ...current, [label]: open }),
    );
  } catch {
    // Private mode / quota denied — toggle still works for the session.
  }
}

interface NavItem {
  label: string;
  to?: string;
  /** Mock count rendered right-aligned. Replaced with real queries later. */
  count?: number;
  /** When true, render as an inert link with a "Coming soon" hint. */
  todo?: boolean;
}

interface NavGroup {
  label: string;
  icon: LucideIcon;
  to?: string;
  count?: number;
  todo?: boolean;
  items?: NavItem[];
  /** When true this group is collapsible in expanded sidebar mode. */
  collapsible?: boolean;
  /**
   * When true, this leaf only lights up on an EXACT pathname match, never a
   * sub-route prefix match. Needed for a top-level index route that has
   * sibling leaves sharing its prefix (Admin's "Users" leaf lives at
   * `/admin`, and would otherwise also light up on `/admin/accounts`,
   * `/admin/revenue`, etc. via the default prefix-match `isActive`).
   */
  exactMatch?: boolean;
}

// Top groups (rendered above the bottom-aligned Settings entry).
//
// Operations, Insights, Security are marked collapsible. Sites, Clients, Email,
// and Shared with me are leaf links and are not collapsible.
const TOP_GROUPS: ReadonlyArray<NavGroup> = [
  {
    label: "Sites",
    icon: Globe,
    to: "/sites",
    // count is injected live from useSites() in <Sidebar>; see below.
  },
  {
    label: "Operations",
    icon: Activity,
    collapsible: true,
    items: [
      { label: "Backups", to: "/backups" },
      { label: "Updates", to: "/updates" },
      // Backup storage targets (managed / local / S3) — a backup concern, not
      // an account setting, so it lives with Operations.
      { label: "Destinations", to: "/destinations" },
    ],
  },
  {
    label: "Insights",
    icon: LineChart,
    collapsible: true,
    items: [
      { label: "Uptime", to: "/uptime" },
      { label: "Performance", to: "/performance" },
      // Downtime-notification config — tied to uptime monitoring, not an
      // account setting, so it lives with Insights.
      { label: "Alerts", to: "/alerts" },
    ],
  },
  {
    label: "Clients",
    icon: Users,
    to: "/clients",
  },
  {
    label: "Email",
    icon: Mail,
    to: "/email",
  },
  {
    label: "Security",
    icon: Shield,
    collapsible: true,
    items: [
      { label: "Vulnerabilities", to: "/vulnerabilities" },
      { label: "Audit", to: "/audit" },
    ],
  },
];

// Settings is now a single leaf link. The 8 sub-sections live in the
// /settings area (with its own left sidebar managed by Agent A).
const SETTINGS_LEAF: NavGroup = {
  label: "Settings",
  icon: Settings,
  to: "/settings",
};

const SHARED_WITH_ME_GROUP: NavGroup = {
  label: "Shared with me",
  icon: Share2,
  to: "/shared-with-me",
};

// Superadmin-only nav entries — promoted directly into the primary sidebar
// (Option A, GH admin-layout fix) rather than living inside a second, nested
// "Instance Admin" left-nav inside the /admin route layout. Only mounted when
// me.user.is_superadmin === true. "Users" is the /admin index route, so it
// carries `exactMatch` — otherwise its prefix-matching `isActive` check would
// also light it up on every other /admin/* leaf below it.
const ADMIN_NAV_GROUPS: ReadonlyArray<NavGroup> = [
  { label: "Users", icon: Users, to: "/admin", exactMatch: true },
  { label: "Accounts", icon: Building2, to: "/admin/accounts" },
  { label: "Revenue", icon: TrendingUp, to: "/admin/revenue" },
  { label: "Vulnerability feed", icon: ShieldAlert, to: "/admin/vuln-feed" },
  { label: "Agent mirror", icon: RefreshCw, to: "/admin/agent-mirror" },
];

// Bottom-aligned app-switcher leaf, mirroring how the tenant sidebar
// bottom-aligns its single Settings leaf.
const BACK_TO_SITES_LEAF: NavGroup = {
  label: "Back to Sites",
  icon: ArrowLeft,
  to: "/sites",
};

export function Sidebar() {
  const { collapsed, mobileOpen, setMobileOpen } = useShellState();
  const location = useLocation();
  const pathname = location.pathname;
  const { data: me } = useMe();
  const superadmin = isSuperadmin(me);

  // Live "Sites" count for the nav badge (active, non-archived) — shares the
  // sites-list query cache with the Sites page, so it's deduped.
  const { data: sitesData } = useSites();
  const sitesCount = sitesData?.length;

  // Close the mobile drawer on route change so navigation always feels resolved.
  useEffect(() => {
    setMobileOpen(false);
  }, [pathname, setMobileOpen]);

  return (
    <>
      {/* Mobile backdrop. Fades via opacity (DESIGN.md: never animate
          width/height/top/left). Click to dismiss. */}
      <div
        aria-hidden="true"
        onClick={() => setMobileOpen(false)}
        className={cn(
          "fixed inset-0 z-30 bg-foreground/30 transition-opacity duration-150 md:hidden",
          mobileOpen
            ? "pointer-events-auto opacity-100"
            : "pointer-events-none opacity-0",
        )}
      />
      <nav
        aria-label="Primary"
        // Mobile: fixed overlay sliding in via transform. Desktop: in-grid,
        // col 1 spans both rows.
        className={cn(
          "fixed inset-y-0 left-0 z-40 flex w-[240px] flex-col border-r border-border bg-background transition-transform duration-150 will-change-transform",
          "md:static md:col-start-1 md:row-span-2 md:row-start-1 md:w-auto md:translate-x-0",
          mobileOpen ? "translate-x-0" : "-translate-x-full md:translate-x-0",
        )}
      >
        {/* Brand strip - height matches the TopBar (48px) so the border
            separating sidebar and topbar reads as one clean line. */}
        <BrandStrip collapsed={collapsed} />

        <div className="flex flex-1 flex-col overflow-y-auto px-2 py-3">
          {superadmin ? (
            // Superadmin is monitoring-only: show ONLY the Admin area. They
            // have no org and never manage sites/settings.
            <>
              <ul className="flex flex-col gap-0.5">
                {ADMIN_NAV_GROUPS.map((group) => (
                  <li key={group.label}>
                    <NavGroupItem
                      group={group}
                      pathname={pathname}
                      collapsed={collapsed}
                    />
                  </li>
                ))}
              </ul>
              {/* Back to Sites — bottom-aligned, mirrors the tenant
                  sidebar's bottom-aligned Settings leaf. */}
              <ul className="mt-auto flex flex-col gap-0.5 pt-3">
                <li>
                  <NavGroupItem
                    group={BACK_TO_SITES_LEAF}
                    pathname={pathname}
                    collapsed={collapsed}
                  />
                </li>
              </ul>
            </>
          ) : (
            <>
              <ul className="flex flex-col gap-0.5">
                {TOP_GROUPS.map((group) => {
                  // Inject the live Sites count; other groups carry no badge.
                  const g =
                    group.label === "Sites" && typeof sitesCount === "number"
                      ? { ...group, count: sitesCount }
                      : group;
                  return (
                    <li key={group.label}>
                      <NavGroupItem
                        group={g}
                        pathname={pathname}
                        collapsed={collapsed}
                      />
                    </li>
                  );
                })}
                {/* Shared with me — visible to all users. */}
                <li>
                  <NavGroupItem
                    group={SHARED_WITH_ME_GROUP}
                    pathname={pathname}
                    collapsed={collapsed}
                  />
                </li>
              </ul>
              {/* Settings — single leaf link, bottom-aligned. */}
              <ul className="mt-auto flex flex-col gap-0.5 pt-3">
                <li>
                  <NavGroupItem
                    group={SETTINGS_LEAF}
                    pathname={pathname}
                    collapsed={collapsed}
                  />
                </li>
              </ul>
            </>
          )}
        </div>
      </nav>
    </>
  );
}

function BrandStrip({ collapsed }: { collapsed: boolean }) {
  return (
    <div
      className={cn(
        "flex h-12 items-center border-b border-border",
        collapsed ? "justify-center px-0" : "px-4",
      )}
    >
      <Link
        to="/sites"
        aria-label="WPMgr home"
        className="inline-flex items-center gap-2 rounded-md text-sm font-semibold tracking-tight text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2"
      >
        <FleetHubLogo size={20} />
        {collapsed ? null : <Wordmark />}
      </Link>
    </div>
  );
}

interface GroupProps {
  group: NavGroup;
  pathname: string;
  collapsed: boolean;
}

function NavGroupItem({ group, pathname, collapsed }: GroupProps) {
  const hasItems = !!group.items?.length;

  // Group is "active" when its own route or any sub-item route is the
  // current pathname (or prefix). `exactMatch` opts a leaf out of the
  // prefix-match behavior (see the NavGroup interface doc above).
  const selfActive = group.to
    ? group.exactMatch
      ? pathname === group.to
      : isActive(pathname, group.to)
    : false;
  const childActive =
    hasItems &&
    group.items!.some((item) => item.to && isActive(pathname, item.to));
  const active = selfActive || childActive;

  if (collapsed) {
    return (
      <CollapsedGroup group={group} pathname={pathname} active={active} />
    );
  }

  // Leaf groups (no sub-items): the row itself is the link.
  if (!hasItems) {
    return (
      <NavLeaf
        label={group.label}
        to={group.to}
        icon={group.icon}
        count={group.count}
        todo={group.todo}
        active={active}
        variant="group"
      />
    );
  }

  // Collapsible group with sub-items.
  if (group.collapsible) {
    return (
      <CollapsibleGroup
        group={group}
        pathname={pathname}
        active={active}
        childActive={childActive}
      />
    );
  }

  // Non-collapsible group with sub-items (fallback, not currently used).
  return (
    <div>
      <GroupHeader group={group} active={active} />
      <ul className="mt-0.5 flex flex-col gap-0.5">
        {group.items!.map((item) => (
          <li key={item.label}>
            <NavLeaf
              label={item.label}
              to={item.to}
              count={item.count}
              todo={item.todo}
              active={item.to ? isActive(pathname, item.to) : false}
              variant="sub"
            />
          </li>
        ))}
      </ul>
    </div>
  );
}

// ---------------------------------------------------------------------------
// CollapsibleGroup — the collapsible variant for expanded sidebar mode.
// ---------------------------------------------------------------------------

interface CollapsibleGroupProps {
  group: NavGroup;
  pathname: string;
  active: boolean;
  childActive: boolean;
}

function CollapsibleGroup({
  group,
  pathname,
  active,
  childActive,
}: CollapsibleGroupProps) {
  const regionId = useId();

  // Determine initial open state:
  //   1. If localStorage has an explicit entry for this group, use it.
  //   2. Otherwise, open iff the active route is inside this group.
  // This means the user's manual choices always win, but un-touched groups
  // auto-expand when you navigate into them.
  const [open, setOpen] = useState<boolean>(() => {
    const map = readGroupsMap();
    if (Object.prototype.hasOwnProperty.call(map, group.label)) {
      return map[group.label] === true;
    }
    return childActive;
  });

  // When navigation lands the active route inside this group, always reveal it
  // — even if the user previously collapsed it. Otherwise navigating via the
  // URL bar, back/forward, or the command palette would hide the active page
  // inside a closed group with no content visible. Persist so the next session
  // starts consistent. Groups the user is NOT navigating into keep their
  // remembered open/closed state.
  const prevChildActive = useRef(childActive);
  useEffect(() => {
    if (childActive && !prevChildActive.current) {
      setOpen(true);
      writeGroupsMap(group.label, true);
    }
    prevChildActive.current = childActive;
  }, [childActive, group.label]);

  const toggle = () => {
    setOpen((prev) => {
      const next = !prev;
      writeGroupsMap(group.label, next);
      return next;
    });
  };

  const Icon = group.icon;

  return (
    <div>
      {/* Collapsible group header — a button so Enter/Space work natively. */}
      <button
        type="button"
        id={`${regionId}-trigger`}
        aria-expanded={open}
        aria-controls={regionId}
        onClick={toggle}
        className={cn(
          "flex w-full items-center gap-2 rounded-md px-3 py-2",
          "text-xs uppercase tracking-[0.02em] font-medium",
          "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2",
          "hover:bg-muted/50",
          active ? "text-foreground" : "text-muted-foreground",
          // Left rail when the group (or a child) is active.
          "border-l-2",
          active ? "border-primary" : "border-transparent",
        )}
      >
        <Icon aria-hidden="true" className="size-4 shrink-0" />
        <span className="flex-1 truncate text-left">{group.label}</span>
        {/* Chevron rotates 90deg when open. Respects prefers-reduced-motion
            via the global CSS rule (transition-duration collapses to ~0ms). */}
        <ChevronRight
          aria-hidden="true"
          className={cn(
            "size-3.5 shrink-0 text-muted-foreground transition-transform duration-150",
            open && "rotate-90",
          )}
        />
      </button>

      {/* Collapsible region. Uses the grid-rows [0fr]<->[1fr] technique to
          animate height without animating height directly (DESIGN.md).
          overflow-hidden on the inner div clips content during collapse.
          prefers-reduced-motion: reduce collapses transitions globally via
          the globals.css rule (duration → 0.01ms). */}
      <div
        id={regionId}
        role="region"
        aria-labelledby={`${regionId}-trigger`}
        className={cn(
          "grid transition-[grid-template-rows] duration-150",
          open ? "grid-rows-[1fr]" : "grid-rows-[0fr]",
        )}
      >
        <div className="overflow-hidden">
          <ul className="mt-0.5 flex flex-col gap-0.5 pb-0.5">
            {group.items!.map((item) => (
              <li key={item.label}>
                <NavLeaf
                  label={item.label}
                  to={item.to}
                  count={item.count}
                  todo={item.todo}
                  active={item.to ? isActive(pathname, item.to) : false}
                  variant="sub"
                />
              </li>
            ))}
          </ul>
        </div>
      </div>
    </div>
  );
}

function GroupHeader({ group, active }: { group: NavGroup; active: boolean }) {
  const Icon = group.icon;
  return (
    <div
      className={cn(
        "flex items-center gap-2 rounded-md px-3 py-2 text-sm font-medium",
        "text-xs uppercase tracking-[0.02em] text-muted-foreground",
        active && "text-foreground",
      )}
    >
      <Icon aria-hidden="true" className="size-4" />
      <span>{group.label}</span>
    </div>
  );
}

interface LeafProps {
  label: string;
  to?: string;
  icon?: LucideIcon;
  count?: number;
  todo?: boolean;
  active: boolean;
  variant: "group" | "sub";
}

function NavLeaf({ label, to, icon: Icon, count, todo, active, variant }: LeafProps) {
  // Shared visual: 8px/12px padding (sub gets a little extra indent), 6px
  // radius, active = accent + 2px left primary border (DESIGN.md "Sidebar
  // item"). Hover muted/50 on inactive only. Focus-visible ring per the
  // global rule.
  const className = cn(
    "relative flex items-center gap-2 rounded-md py-2 text-sm font-medium",
    "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2",
    variant === "group" ? "px-3" : "pl-9 pr-3 text-xs",
    active
      ? "bg-accent text-accent-foreground"
      : "text-foreground hover:bg-muted/50",
    "border-l-2",
    active ? "border-primary" : "border-transparent",
  );

  const inner: ReactNode = (
    <>
      {Icon ? <Icon aria-hidden="true" className="size-4" /> : null}
      <span className="flex-1 truncate">{label}</span>
      {typeof count === "number" ? (
        <span className="font-mono text-xs tabular-nums text-muted-foreground">
          {count}
        </span>
      ) : null}
    </>
  );

  if (!to || todo) {
    return (
      <span
        role="link"
        aria-disabled="true"
        tabIndex={-1}
        title="Coming soon"
        className={cn(className, "cursor-not-allowed opacity-50")}
      >
        {inner}
      </span>
    );
  }

  return (
    <Link to={to} className={className} aria-current={active ? "page" : undefined}>
      {inner}
    </Link>
  );
}

// ---------------------------------------------------------------------------
// CollapsedGroup — 64px rail mode. Preserved from the original implementation.
// Collapsible groups still expose their sub-items as hover/focus flyouts.
// The single Settings leaf is just an icon link (no flyout needed).
// ---------------------------------------------------------------------------

function CollapsedGroup({
  group,
  pathname,
  active,
}: {
  group: NavGroup;
  pathname: string;
  active: boolean;
}) {
  const Icon = group.icon;
  const hasItems = !!group.items?.length;
  const [open, setOpen] = useState(false);
  const flyoutId = useId();

  // Single icon button. Active 2px primary left rail via `before:` pseudo so
  // border-l-2 doesn't conflict with rounded-md (DESIGN.md pattern).
  const iconClass = cn(
    "relative flex h-9 w-full items-center justify-center rounded-md",
    "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2",
    "before:absolute before:left-0 before:top-1 before:bottom-1 before:w-0.5 before:rounded-sm",
    active
      ? "bg-accent text-accent-foreground before:bg-primary"
      : "text-foreground hover:bg-muted/50 before:bg-transparent",
  );

  if (!hasItems) {
    // Leaf group (Sites, Settings, etc.) in collapsed mode.
    if (!group.to || group.todo) {
      return (
        <span
          role="link"
          aria-disabled="true"
          tabIndex={-1}
          title={`${group.label} (coming soon)`}
          className={cn(iconClass, "cursor-not-allowed opacity-50")}
        >
          <Icon aria-hidden="true" className="size-5" />
        </span>
      );
    }
    return (
      <Link
        to={group.to}
        title={group.label}
        aria-label={group.label}
        aria-current={active ? "page" : undefined}
        className={iconClass}
      >
        <Icon aria-hidden="true" className="size-5" />
      </Link>
    );
  }

  // Group with sub-items: the icon opens a popover flyout to the right.
  return (
    <div
      className="relative"
      onMouseEnter={() => setOpen(true)}
      onMouseLeave={() => setOpen(false)}
      onFocus={() => setOpen(true)}
      onBlur={(e) => {
        if (!e.currentTarget.contains(e.relatedTarget)) setOpen(false);
      }}
    >
      <button
        type="button"
        title={group.label}
        aria-label={group.label}
        aria-expanded={open}
        aria-controls={flyoutId}
        className={iconClass}
      >
        <Icon aria-hidden="true" className="size-5" />
      </button>
      {/* Disclosure panel — a plain labelled list of links, NOT an ARIA menu.
          A role="menu" here would promise arrow-key roving + Escape semantics
          we don't implement; for a hover/focus rail flyout, regular links with
          Tab order + focus-visible rings + aria-current are the correct,
          fully accessible pattern. */}
      <div
        id={flyoutId}
        aria-label={group.label}
        className={cn(
          "absolute left-full top-0 z-50 ml-1 min-w-[180px] rounded-md border border-border bg-popover p-1 text-popover-foreground transition-opacity duration-150",
          open ? "pointer-events-auto opacity-100" : "pointer-events-none opacity-0",
        )}
      >
        <div className="border-b border-border px-2 py-1.5 text-xs font-medium uppercase tracking-[0.02em] text-muted-foreground">
          {group.label}
        </div>
        <ul className="mt-1 flex flex-col gap-0.5">
          {group.items!.map((item) => (
            <li key={item.label}>
              <FlyoutItem item={item} pathname={pathname} />
            </li>
          ))}
        </ul>
      </div>
    </div>
  );
}

function FlyoutItem({
  item,
  pathname,
}: {
  item: NavItem;
  pathname: string;
}) {
  const active = item.to ? isActive(pathname, item.to) : false;
  const className = cn(
    "flex items-center gap-2 rounded-sm px-2 py-1.5 text-sm",
    "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2",
    active
      ? "bg-accent text-accent-foreground"
      : "text-foreground hover:bg-muted/50",
  );
  const inner = (
    <>
      <span className="flex-1 truncate">{item.label}</span>
      {typeof item.count === "number" ? (
        <span className="font-mono text-xs tabular-nums text-muted-foreground">
          {item.count}
        </span>
      ) : null}
    </>
  );
  if (!item.to || item.todo) {
    return (
      <span
        role="link"
        aria-disabled="true"
        tabIndex={-1}
        title="Coming soon"
        className={cn(className, "cursor-not-allowed opacity-50")}
      >
        {inner}
      </span>
    );
  }
  return (
    <Link
      to={item.to}
      className={className}
      aria-current={active ? "page" : undefined}
    >
      {inner}
    </Link>
  );
}

/**
 * A route is "active" when the current pathname matches it exactly or is a
 * sub-route. /sites/$siteId still lights up /sites; /settings/api-keys still
 * lights up /settings.
 */
function isActive(pathname: string, to: string): boolean {
  if (pathname === to) return true;
  return pathname.startsWith(`${to}/`);
}
