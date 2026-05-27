import type { ReactNode } from "react";
import { Link, useNavigate } from "@tanstack/react-router";
import { Globe, LogOut } from "lucide-react";

import { Button } from "@/components/ui/button";
import { ThemeToggle } from "@/components/layout/theme-toggle";
import { useSessionStore } from "@/lib/session-store";

// Authenticated app shell: semantic landmarks (banner header, nav sidebar,
// main). Used by the routes nested under the protected layout.
export function AppShell({ children }: { children: ReactNode }) {
  const session = useSessionStore((s) => s.session);
  const signOut = useSessionStore((s) => s.signOut);
  const navigate = useNavigate();

  function handleLogout() {
    signOut();
    void navigate({ to: "/login" });
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
        <div className="flex items-center gap-2">
          {session ? (
            <span className="hidden text-sm text-[var(--color-muted-foreground)] sm:inline">
              {session.email}
            </span>
          ) : null}
          <ThemeToggle />
          <Button
            variant="outline"
            size="sm"
            onClick={handleLogout}
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
          </ul>
        </nav>

        <main className="flex-1 p-6">{children}</main>
      </div>
    </div>
  );
}
