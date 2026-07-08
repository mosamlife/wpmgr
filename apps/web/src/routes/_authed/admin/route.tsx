import { Component, type ErrorInfo, type ReactNode } from "react";
import { createFileRoute, Link, Outlet, redirect } from "@tanstack/react-router";
// The admin billing pages render Tooltip components (approximate-storage marker,
// overrides dot, etc.). The app has no global TooltipProvider — each feature
// provides its own — so the admin layout provides one for every /admin page.
import { TooltipProvider } from "@/components/ui/tooltip";

import { ensureMe, isSuperadmin } from "@/features/auth/use-auth";

// ---------------------------------------------------------------------------
// Superadmin layout route.
//
// The four admin sections (Users, Accounts, Revenue, Vulnerability feed) live
// in the PRIMARY sidebar's superadmin branch (components/layout/sidebar.tsx),
// not in a second, nested left-nav here — this layout used to duplicate that
// navigation in a ~210px column capped at max-w-5xl, which both boxed in
// every admin page AND doubled up the navigation surface. This layout is now
// just the error boundary + the shared TooltipProvider, full-width.
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
// Layout component — full-width, no boxed max-width column. Navigation into
// the four admin sections lives in the primary sidebar (see the module doc
// above); this layout only supplies the shared TooltipProvider (see the
// import comment) and the error boundary around the matched sub-page.
// ---------------------------------------------------------------------------

function AdminLayout() {
  return (
    <TooltipProvider>
      <div className="w-full min-w-0 space-y-6">
        <AdminErrorBoundary>
          <Outlet />
        </AdminErrorBoundary>
      </div>
    </TooltipProvider>
  );
}
