import { useEffect, useState, type ReactNode } from "react";
import { Link, useLocation } from "@tanstack/react-router";
import {
  Activity,
  Globe,
  LineChart,
  Settings,
  Share2,
  Shield,
  type LucideIcon,
} from "lucide-react";

import { useShellState } from "@/components/layout/app-shell-context";
import { useMe, isOrgScoped } from "@/features/auth/use-auth";
import { cn } from "@/lib/utils";

// Phase 4 / Sprint 1 surface 4.2 - primary navigation.
//
// Five groups in fixed order: Sites, Operations, Insights, Security,
// Settings (bottom-aligned). Per-item: 8px / 12px padding, 6px radius,
// active = accent + 2px left ring primary (DESIGN.md "Sidebar item").
//
// Routes that don't exist yet (migrations, uptime, performance,
// vulnerabilities, audit, the /settings index) render as disabled items with
// `aria-disabled` and a "Coming soon" tooltip. The brief allows this for
// Sprint 1; Sprint 4 will fill them in.
//
// Collapsed state (64px rail) shows icons only. Active items still highlight;
// the active group's sub-items move into a flyout that opens on hover/focus
// of the group icon. Tooltips on the icons use the native `title` attribute
// - sufficient for the current shadcn primitive set (no Tooltip primitive
// installed yet).

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
}

// Top groups (rendered above the bottom-aligned Settings entry).
//
// Counts are placeholders sourced from a hard-coded mock until the real
// queries land (Sprint 2 onward). They use mono / tabular-nums so the
// column reads cleanly even with multi-digit numbers.
const TOP_GROUPS: ReadonlyArray<NavGroup> = [
  {
    label: "Sites",
    icon: Globe,
    to: "/sites",
    count: 12,
  },
  {
    label: "Operations",
    icon: Activity,
    items: [
      // /backups index doesn't exist yet - only /backups/$snapshotId. Mark
      // disabled until Sprint 4 adds the index.
      { label: "Backups", to: "/backups", count: 3 },
      { label: "Updates", to: "/updates", count: 8 },
      // /migrations doesn't exist yet.
      { label: "Migrations", to: "/migrations" },
    ],
  },
  {
    label: "Insights",
    icon: LineChart,
    items: [
      { label: "Uptime", to: "/uptime" },
      { label: "Performance", to: "/performance" },
    ],
  },
  {
    label: "Security",
    icon: Shield,
    items: [
      { label: "Vulnerabilities", to: "/vulnerabilities", count: 0 },
      { label: "Audit", to: "/audit" },
    ],
  },
];

const SETTINGS_GROUP: NavGroup = {
  label: "Settings",
  icon: Settings,
  // Settings is now a grouped section so the destinations sub-item (ADR-036 P1
  // storage adapter) has a stable home. The group highlights and the API keys
  // page is still the "first" target — Sprint 4 may add a real /settings index
  // page later, at which point the `to` here can flip to that.
  items: [
    { label: "Account", to: "/settings/account" },
    { label: "Organisation", to: "/settings/organization" },
    { label: "API keys", to: "/settings/api-keys" },
    { label: "Destinations", to: "/settings/destinations" },
    { label: "Alerts", to: "/settings/alerts" },
    { label: "Members", to: "/settings/members" },
  ],
};

const SHARED_WITH_ME_GROUP: NavGroup = {
  label: "Shared with me",
  icon: Share2,
  to: "/shared-with-me",
};

export function Sidebar() {
  const { collapsed, mobileOpen, setMobileOpen } = useShellState();
  const location = useLocation();
  const pathname = location.pathname;
  const { data: me } = useMe();
  const orgScoped = isOrgScoped(me);

  // Site-scoped collaborators only see a filtered settings group (Account only).
  const settingsGroup: NavGroup = orgScoped
    ? SETTINGS_GROUP
    : {
        ...SETTINGS_GROUP,
        items: SETTINGS_GROUP.items?.filter((item) =>
          item.to === "/settings/account",
        ),
      };

  // Close the mobile drawer on route change so navigation always feels
  // resolved. Effect, not in the click handler, so back/forward also close.
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
          <ul className="flex flex-col gap-0.5">
            {TOP_GROUPS.map((group) => (
              <li key={group.label}>
                <NavGroupItem group={group} pathname={pathname} collapsed={collapsed} />
              </li>
            ))}
            {/* Shared with me — visible to all users. */}
            <li>
              <NavGroupItem
                group={SHARED_WITH_ME_GROUP}
                pathname={pathname}
                collapsed={collapsed}
              />
            </li>
          </ul>
          <ul className="mt-auto flex flex-col gap-0.5 pt-3">
            <li>
              <NavGroupItem
                group={settingsGroup}
                pathname={pathname}
                collapsed={collapsed}
              />
            </li>
          </ul>
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
        <Globe aria-hidden="true" className="size-5 text-primary" />
        {collapsed ? null : <span>WPMgr</span>}
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
  // current pathname (or prefix). Sub-paths under e.g. /sites/$siteId still
  // light up Sites.
  const selfActive = group.to ? isActive(pathname, group.to) : false;
  const childActive =
    hasItems &&
    group.items!.some((item) => item.to && isActive(pathname, item.to));
  const active = selfActive || childActive;

  // Expanded sidebar: render the group as a row, and (when active OR has no
  // items) render sub-items below.
  // Collapsed sidebar: render the icon centered, with a hover flyout for
  // sub-items.

  if (collapsed) {
    return (
      <CollapsedGroup group={group} pathname={pathname} active={active} />
    );
  }

  // For leaf groups (Sites, Settings), the row itself is the link.
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

  // Group with sub-items. The header is a section label; the sub-items ALWAYS
  // render below it so every route is reachable from the expanded sidebar.
  // (Previously they were gated on `active`, which hid a group's children
  // unless you were already inside that group — leaving Backups/Migrations/
  // Uptime/Performance/Vulnerabilities/Audit unreachable from the nav.)
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

function GroupHeader({ group, active }: { group: NavGroup; active: boolean }) {
  const Icon = group.icon;
  return (
    <div
      className={cn(
        "flex items-center gap-2 rounded-md px-3 py-2 text-sm font-medium",
        // Group headers don't act like links - they label the open section.
        // Tracking matches the DESIGN.md caption rule for label rows.
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
    // Left rail accent on active rows. Border on the left edge (Tailwind has
    // no directional ring; the border is the established equivalent per the
    // brief).
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

  // Single icon button - links straight through when the group has its own
  // route, or opens a flyout on hover/focus when it groups sub-items. The
  // active 2px primary left rail is rendered as a `before:` pseudo so the
  // border utility doesn't share a line with `rounded-md` (Tailwind has no
  // directional ring; per the brief either border-l-2 or `before:` is
  // valid and the pseudo keeps the surface readable for static analysis).
  const iconClass = cn(
    "relative flex h-9 w-full items-center justify-center rounded-md",
    "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2",
    "before:absolute before:left-0 before:top-1 before:bottom-1 before:w-0.5 before:rounded-sm",
    active
      ? "bg-accent text-accent-foreground before:bg-primary"
      : "text-foreground hover:bg-muted/50 before:bg-transparent",
  );

  if (!hasItems) {
    // Leaf group (Sites, Settings) in collapsed mode.
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
        aria-haspopup="menu"
        aria-expanded={open}
        className={iconClass}
      >
        <Icon aria-hidden="true" className="size-5" />
      </button>
      <div
        role="menu"
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
            <li key={item.label} role="none">
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
        role="menuitem"
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
      role="menuitem"
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
