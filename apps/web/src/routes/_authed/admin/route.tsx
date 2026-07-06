import { Component, type ErrorInfo, type ReactNode } from "react";
import { createFileRoute, Link, Outlet, redirect, useLocation } from "@tanstack/react-router";
// The admin billing pages render Tooltip components (approximate-storage marker,
// overrides dot, etc.). The app has no global TooltipProvider — each feature
// provides its own — so the admin layout provides one for every /admin page.
import { TooltipProvider } from "@/components/ui/tooltip";
import {
  ArrowLeft,
  Building2,
  ShieldCheck,
  TrendingUp,
  Users,
  type LucideIcon,
} from "lucide-react";

import { ensureMe, isSuperadmin } from "@/features/auth/use-auth";
import { cn } from "@/lib/utils";

// ---------------------------------------------------------------------------
// Superadmin layout route — wraps all /admin/* sub-pages with a left nav.
//
// Auth gate: beforeLoad checks isSuperadmin and redirects non-superadmins to
// /sites. This gate runs on every navigation into any /admin/* route, including
// direct URL access, so sub-pages never need their own guard.
// ---------------------------------------------------------------------------

export const Route = createFileRoute("/_authed/admin")({
  beforeLoad: async ({ context }) => {
    const me = await ensureMe(context.queryClient);
    if (!isSuperadmin(me)) {
      throw redirect({ to: "/sites" });
    }
  },
  component: AdminLayout,
});

// ---------------------------------------------------------------------------
// Error boundary — wraps the admin Outlet so a failed admin chunk or
// render error never propagates up to the tenant app (/sites etc).
// This is a class component because React error boundaries must be class-based.
// ---------------------------------------------------------------------------

interface AdminErrorBoundaryState {
  hasError: boolean;
  errorMessage: string;
}

class AdminErrorBoundary extends Component<
  { children: ReactNode },
  AdminErrorBoundaryState
> {
  constructor(props: { children: ReactNode }) {
    super(props);
    this.state = { hasError: false, errorMessage: "" };
  }

  static getDerivedStateFromError(err: unknown): AdminErrorBoundaryState {
    const msg =
      err instanceof Error
        ? err.message
        : "An unexpected error occurred in the admin panel.";
    return { hasError: true, errorMessage: msg };
  }

  override componentDidCatch(err: Error, info: ErrorInfo) {
    // Log for visibility in production monitoring.
    console.error("[admin] Caught error:", err, info);
  }

  override render() {
    if (this.state.hasError) {
      return (
        <div className="rounded-xl border border-destructive/30 bg-destructive/5 p-6 space-y-4">
          <p className="text-sm font-medium text-destructive">
            The admin panel failed to load.
          </p>
          <p className="text-xs text-muted-foreground">
            {this.state.errorMessage}
          </p>
          <div className="flex gap-2">
            <button
              type="button"
              className="text-xs text-primary underline-offset-2 hover:underline"
              onClick={() =>
                this.setState({ hasError: false, errorMessage: "" })
              }
            >
              Try again
            </button>
            <span className="text-xs text-muted-foreground">or</span>
            <Link
              to="/sites"
              className="text-xs text-primary underline-offset-2 hover:underline"
            >
              Back to Sites
            </Link>
          </div>
        </div>
      );
    }
    return this.props.children;
  }
}

// ---------------------------------------------------------------------------
// Nav item definitions — add new admin sections here only.
// ---------------------------------------------------------------------------

type AdminNavPath =
  | "/admin"
  | "/admin/accounts"
  | "/admin/revenue"
  | "/admin/vuln-feed";

interface AdminNavItem {
  label: string;
  to: AdminNavPath;
  icon: LucideIcon;
}

const ADMIN_NAV_ITEMS: AdminNavItem[] = [
  { label: "Users", to: "/admin", icon: Users },
  { label: "Accounts", to: "/admin/accounts", icon: Building2 },
  { label: "Revenue", to: "/admin/revenue", icon: TrendingUp },
  { label: "Vulnerability feed", to: "/admin/vuln-feed", icon: ShieldCheck },
];

// ---------------------------------------------------------------------------
// Layout component
// ---------------------------------------------------------------------------

function AdminLayout() {
  const location = useLocation();
  const pathname = location.pathname;

  return (
    <TooltipProvider>
    <div className="mx-auto w-full max-w-5xl">
      <div className="flex flex-col gap-6 md:flex-row md:gap-10">
        {/* Left navigation — mirrors the Settings area side-menu pattern. */}
        <nav
          aria-label="Admin"
          className={cn(
            "flex flex-row gap-1 overflow-x-auto pb-1",
            "md:w-[210px] md:shrink-0 md:flex-col md:overflow-x-visible md:pb-0",
          )}
        >
          <p
            aria-hidden="true"
            className="hidden select-none px-3 pb-1 text-xs font-medium uppercase tracking-[0.06em] text-muted-foreground md:block"
          >
            Instance Admin
          </p>

          {ADMIN_NAV_ITEMS.map((item) => {
            // Exact match for the index (/admin) so it does not also light up
            // for /admin/vuln-feed; prefix match for sub-routes of the others.
            const active =
              item.to === "/admin"
                ? pathname === "/admin"
                : pathname === item.to || pathname.startsWith(`${item.to}/`);

            const Icon = item.icon;
            return (
              <Link
                key={item.to}
                to={item.to}
                aria-current={active ? "page" : undefined}
                className={cn(
                  "relative flex min-w-max items-center gap-2.5 rounded-md px-3 py-2 text-sm font-medium transition-colors",
                  "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2",
                  "border-b-2 md:border-b-0 md:border-l-2",
                  active
                    ? "border-primary bg-accent text-accent-foreground"
                    : "border-transparent text-foreground hover:bg-muted/50",
                  "md:w-full md:min-w-0",
                )}
              >
                <Icon aria-hidden="true" className="size-4 shrink-0" />
                <span className="truncate">{item.label}</span>
              </Link>
            );
          })}

          {/* App-switcher: back to the tenant app. Superadmins can access
              /sites directly when they have an org — this provides a quick
              return path when navigating out of the admin area. */}
          <div className="mt-2 hidden border-t border-border pt-2 md:block">
            <Link
              to="/sites"
              className={cn(
                "flex min-w-max items-center gap-2.5 rounded-md px-3 py-2 text-sm transition-colors",
                "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2",
                "border-transparent text-muted-foreground hover:text-foreground hover:bg-muted/50",
                "md:w-full md:min-w-0",
              )}
            >
              <ArrowLeft aria-hidden="true" className="size-4 shrink-0" />
              <span className="truncate">Back to Sites</span>
            </Link>
          </div>
        </nav>

        {/* Page content — wrapped in an error boundary so a failed admin
            chunk import never propagates up to the tenant app routes. */}
        <main className="min-w-0 flex-1">
          <AdminErrorBoundary>
            <Outlet />
          </AdminErrorBoundary>
        </main>
      </div>
    </div>
    </TooltipProvider>
  );
}
