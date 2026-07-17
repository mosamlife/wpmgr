import { createFileRoute, Link, Outlet, useLocation } from "@tanstack/react-router";
import {
  Building2,
  CreditCard,
  KeyRound,
  Mail,
  ShieldCheck,
  Tag,
  User,
  Users,
  type LucideIcon,
} from "lucide-react";

import { useMe, isOrgScoped, activeRole } from "@/features/auth/use-auth";
import { cn } from "@/lib/utils";

export const Route = createFileRoute("/_authed/settings")({
  component: SettingsLayout,
});

// ---------------------------------------------------------------------------
// Nav item definitions — single source of truth for every settings section.
// ---------------------------------------------------------------------------

interface SettingsNavItem {
  label: string;
  to: string;
  icon: LucideIcon;
  /** When true, only org-scoped members see this item. */
  orgOnly?: boolean;
  /** When true, only the active tenant's owner sees this item. */
  ownerOnly?: boolean;
  /** When true, only a hosted instance (me.hosted) shows this item. */
  hostedOnly?: boolean;
}

export const SETTINGS_NAV_ITEMS: SettingsNavItem[] = [
  { label: "Account",       to: "/settings/account",      icon: User },
  // Security (2FA) is a PERSONAL account setting backed by per-user
  // /auth/2fa/* endpoints — it must render for every authenticated principal
  // (superadmin, site-scoped collaborator, org member), not just org-scoped
  // members. Do NOT add `orgOnly` back here.
  { label: "Security",      to: "/settings/security",     icon: ShieldCheck },
  { label: "Organisation",  to: "/settings/organization", icon: Building2,    orgOnly: true },
  // Billing is owner-only (like the audit re-baseline) AND only ever
  // meaningful on a hosted instance — self-hosted installs never see it.
  { label: "Billing",       to: "/settings/billing",      icon: CreditCard,   orgOnly: true, ownerOnly: true, hostedOnly: true },
  { label: "API keys",      to: "/settings/api-keys",     icon: KeyRound,     orgOnly: true },
  { label: "Tags",          to: "/settings/tags",         icon: Tag,          orgOnly: true },
  { label: "Email / SMTP",  to: "/settings/smtp",         icon: Mail,         orgOnly: true },
  { label: "Members",       to: "/settings/members",      icon: Users,        orgOnly: true },
];

// ---------------------------------------------------------------------------
// Route component
// ---------------------------------------------------------------------------

function SettingsLayout() {
  const { data: me } = useMe();
  const orgScoped = isOrgScoped(me);
  const isOwner = activeRole(me) === "owner";
  const hosted = me?.hosted === true;
  const location = useLocation();
  const pathname = location.pathname;

  // Site-scoped collaborators only see Account. Billing additionally hides
  // behind ownerOnly + hostedOnly (self-hosted installs never see it, and
  // only the tenant owner may manage it).
  const visibleItems = SETTINGS_NAV_ITEMS.filter((item) => {
    if (item.orgOnly && !orgScoped) return false;
    if (item.ownerOnly && !isOwner) return false;
    if (item.hostedOnly && !hosted) return false;
    return true;
  });

  return (
    // Constrain the entire settings area to a sensible max width so text
    // columns stay readable on wide screens.
    <div className="mx-auto w-full max-w-5xl">
      {/*
        Two layouts:
        - Mobile (<md): stacked — horizontal scrollable strip then content below.
        - Desktop (>=md): side-by-side — ~210px left nav column + flex-1 content.
      */}
      <div className="flex flex-col gap-6 md:flex-row md:gap-10">
        {/* ------------------------------------------------------------------ */}
        {/* Settings navigation                                                  */}
        {/* ------------------------------------------------------------------ */}
        <nav
          aria-label="Settings"
          // On mobile: horizontal scroll strip. On desktop: vertical column
          // with a fixed width.
          className={cn(
            // Mobile: full-width horizontal strip (overflow-x-auto, no wrap).
            "flex flex-row gap-1 overflow-x-auto pb-1",
            "md:w-[210px] md:shrink-0 md:flex-col md:overflow-x-visible md:pb-0",
          )}
        >
          {/* Compact "Settings" eyebrow label — only visible in the desktop
              column. Hidden on mobile because screen space is tight. */}
          <p
            aria-hidden="true"
            className="hidden select-none px-3 pb-1 text-xs font-medium uppercase tracking-[0.06em] text-muted-foreground md:block"
          >
            Settings
          </p>

          {visibleItems.map((item) => {
            // A route is active when the pathname matches it exactly or starts
            // with its path (handles future sub-routes).
            const active =
              pathname === item.to || pathname.startsWith(`${item.to}/`);
            const Icon = item.icon;

            return (
              <Link
                key={item.to}
                // Cast to a registered path. The settings sub-routes are all
                // registered through the file-based router; this avoids
                // widening to `string`.
                to={item.to as "/settings/account"}
                aria-current={active ? "page" : undefined}
                className={cn(
                  // Base: flex row, icon + label, rounded, sized to content on
                  // mobile (min-w-max prevents label wrapping in the scroll
                  // strip), full-width on desktop.
                  "relative flex min-w-max items-center gap-2.5 rounded-md px-3 py-2 text-sm font-medium transition-colors",
                  "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2",
                  // The sanctioned active row visual: accent background + a 2px
                  // primary rail. In the mobile horizontal strip the rail reads
                  // as an underline (border-b); in the desktop column it's the
                  // established left rail (border-l). Inactive: muted hover.
                  "border-b-2 md:border-b-0 md:border-l-2",
                  active
                    ? "border-primary bg-accent text-accent-foreground"
                    : "border-transparent text-foreground hover:bg-muted/50",
                  // On mobile the items are inline; the full-width style only
                  // makes sense in the desktop column.
                  "md:w-full md:min-w-0",
                )}
              >
                <Icon aria-hidden="true" className="size-4 shrink-0" />
                <span className="truncate">{item.label}</span>
              </Link>
            );
          })}
        </nav>

        {/* ------------------------------------------------------------------ */}
        {/* Page content rendered by the matched child route                    */}
        {/* ------------------------------------------------------------------ */}
        <main className="min-w-0 flex-1">
          <Outlet />
        </main>
      </div>
    </div>
  );
}
