import type { ReactNode } from "react";
import { Link, useNavigate } from "@tanstack/react-router";
import { Globe, LogOut, KeyRound, RefreshCw, BellRing } from "lucide-react";

import { Button } from "@/components/ui/button";
import { ThemeToggle } from "@/components/layout/theme-toggle";
import {
  useMe,
  useLogout,
  activeRole,
  canManage,
  canOperate,
} from "@/features/auth/use-auth";

// Authenticated app shell: semantic landmarks (banner header, nav sidebar,
// main). The header shows the logged-in user, their active tenant role, and a
// working logout. Auth state comes from TanStack Query (GET /auth/me) — the
// guard guarantees `me` is present by the time this renders.
export function AppShell({ children }: { children: ReactNode }) {
  const { data: me } = useMe();
  const logout = useLogout();
  const navigate = useNavigate();

  const role = activeRole(me);
  const showKeys = canManage(me);
  const showAlerts = canOperate(me);

  function handleLogout() {
    logout.mutate(undefined, {
      onSettled: () => void navigate({ to: "/login" }),
    });
  }

  return (
    <div className="flex min-h-dvh flex-col">
      <header className="flex items-center justify-between border-b border-[var(--color-border)] px-4 py-3">
        <Link
          to="/sites"
          className="flex items-center gap-2 font-semibold"
          aria-label="WPMgr home"
        >
          <Globe aria-hidden="true" className="size-5" />
          <span>WPMgr</span>
        </Link>
        <div className="flex items-center gap-3">
          {me ? (
            <span className="hidden text-sm text-[var(--color-muted-foreground)] sm:inline">
              {me.user.email}
              {role ? (
                <span className="ml-2 rounded-full bg-[var(--color-accent)] px-2 py-0.5 text-xs capitalize">
                  {role}
                </span>
              ) : null}
            </span>
          ) : null}
          <ThemeToggle />
          <Button
            variant="outline"
            size="sm"
            onClick={handleLogout}
            disabled={logout.isPending}
            aria-label="Log out"
          >
            <LogOut aria-hidden="true" />
            <span>Logout</span>
          </Button>
        </div>
      </header>

      <div className="flex flex-1">
        <nav
          aria-label="Primary"
          className="w-48 shrink-0 border-r border-[var(--color-border)] p-4"
        >
          <ul className="space-y-1 text-sm">
            <li>
              <Link
                to="/sites"
                className="block rounded-md px-3 py-2 hover:bg-[var(--color-accent)] [&.active]:bg-[var(--color-accent)] [&.active]:font-medium"
              >
                Sites
              </Link>
            </li>
            <li>
              <Link
                to="/updates"
                className="flex items-center gap-2 rounded-md px-3 py-2 hover:bg-[var(--color-accent)] [&.active]:bg-[var(--color-accent)] [&.active]:font-medium"
              >
                <RefreshCw aria-hidden="true" className="size-4" />
                Updates
              </Link>
            </li>
            {showAlerts ? (
              <li>
                <Link
                  to="/settings/alerts"
                  className="flex items-center gap-2 rounded-md px-3 py-2 hover:bg-[var(--color-accent)] [&.active]:bg-[var(--color-accent)] [&.active]:font-medium"
                >
                  <BellRing aria-hidden="true" className="size-4" />
                  Alerts
                </Link>
              </li>
            ) : null}
            {showKeys ? (
              <li>
                <Link
                  to="/settings/api-keys"
                  className="flex items-center gap-2 rounded-md px-3 py-2 hover:bg-[var(--color-accent)] [&.active]:bg-[var(--color-accent)] [&.active]:font-medium"
                >
                  <KeyRound aria-hidden="true" className="size-4" />
                  API keys
                </Link>
              </li>
            ) : null}
          </ul>
        </nav>

        <main className="flex-1 p-6">{children}</main>
      </div>
    </div>
  );
}
