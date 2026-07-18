import { Link } from "@tanstack/react-router";
import { Compass } from "lucide-react";

import { AppShell } from "@/components/layout/app-shell";
import { Button } from "@/components/ui/button";

// Branded router-level 404 (GH #243). Wired as the router's
// `defaultNotFoundComponent` (see src/router.tsx) so an unmatched path (a
// stale/mistyped deep link, a removed route like the old
// `/sites/$siteId/performance` before its redirect existed) lands on this
// instead of TanStack Router's bare, unstyled fallback. Renders inside the
// same AppShell chrome as the rest of the app (sidebar + topbar) so the
// operator never loses their navigation — minimal content, on-system tokens.

export function NotFoundPage() {
  return (
    <AppShell>
      <div
        role="status"
        className="flex min-h-[60vh] flex-col items-center justify-center gap-3 text-center"
      >
        <Compass
          aria-hidden="true"
          strokeWidth={1.5}
          className="size-10 text-[var(--color-muted-foreground)]/50"
        />
        <div className="space-y-1">
          <h1 className="text-lg font-semibold text-[var(--color-foreground)]">
            This page doesn't exist.
          </h1>
          <p className="text-sm text-[var(--color-muted-foreground)]">
            The link may be out of date, or the page may have moved.
          </p>
        </div>
        <Button asChild size="sm" className="mt-2">
          <Link to="/sites">Back to Sites</Link>
        </Button>
      </div>
    </AppShell>
  );
}
