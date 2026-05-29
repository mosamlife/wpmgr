import { useState } from "react";
import { createFileRoute, Link, Outlet } from "@tanstack/react-router";
import { Check, Copy, MoreHorizontal, Zap } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { PageError } from "@/components/feedback";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { StatusChip, type StatusTone } from "@/components/status";
import { useSite, NotFoundError } from "@/features/sites/use-sites";
import { useRecordRecentSite } from "@/features/command/use-recent-sites";
import { relativeTime, cn } from "@/lib/utils";
import type { Site } from "@wpmgr/api";

// Site detail page (Sprint 3 → Sprint 5 → restored).
//
// This file is the LAYOUT for `/sites/$siteId/*`. Six tabs (Health, Updates,
// Backups, Security, Activity, Settings) are real child routes via TanStack
// Router file convention (dot notation: `$siteId.health.tsx` →
// `/sites/$siteId/health`). Each tab is a real URL — operators deep-link,
// reload, and the right tab is active.
//
// Sticky layering (Vercel convention — calm scroll, no in-page pinning):
//   • Topbar (AppShell)        — z-40, h-12, top-0          ← pinned
//   • Site header strip        — h-12, scrolls normally     ← NOT sticky
//   • Tab bar                  — h-12, scrolls normally     ← NOT sticky
//
// The tabs are the navigation; once the operator picks one, the section is
// the workspace. Re-pinning tabs while scrolling would burn a header height
// on every screen for navigation they're already inside of.

export const Route = createFileRoute("/_authed/sites/$siteId")({
  component: SiteDetailLayout,
});

const TABS = [
  { to: "/sites/$siteId/health", label: "Health" },
  { to: "/sites/$siteId/updates", label: "Updates" },
  { to: "/sites/$siteId/backups", label: "Backups" },
  { to: "/sites/$siteId/security", label: "Security" },
  { to: "/sites/$siteId/activity", label: "Activity" },
  // ADR-037 Sprint 2 — PHP-error monitor tab. Route file:
  // apps/web/src/routes/_authed/sites/$siteId.errors.tsx
  { to: "/sites/$siteId/errors", label: "Errors" },
  { to: "/sites/$siteId/settings", label: "Settings" },
] as const;

function SiteDetailLayout() {
  const { siteId } = Route.useParams();
  const { data: site, isPending, isError, error, refetch } = useSite(siteId);

  // Sprint 3 surface 4.4: feed the command palette's "recently viewed" list.
  // Fires once per site (the hook keys on site.id internally), not per tab
  // switch — the layout component does not unmount when the Outlet swaps.
  useRecordRecentSite(site);

  return (
    <div className="mx-auto w-full max-w-[1200px]">
      {isPending ? (
        <SiteShellSkeleton />
      ) : isError ? (
        <div className="px-6 py-8">
          {error instanceof NotFoundError ? (
            <div role="alert" className="space-y-2">
              <h1 className="text-2xl font-semibold">Site not found</h1>
              <p className="text-sm text-muted-foreground">
                No site exists with id{" "}
                <code className="font-mono">{siteId}</code>.
              </p>
              <Button asChild variant="outline" size="sm">
                <Link to="/sites">Back to sites</Link>
              </Button>
            </div>
          ) : (
            <PageError
              what="Could not load site."
              why={whyFromSiteError(error)}
              onRetry={() => void refetch()}
              retryLabel="Reload site"
            />
          )}
        </div>
      ) : (
        <SiteShell site={site} siteId={siteId} />
      )}
    </div>
  );
}

// ── Loading skeleton ────────────────────────────────────────────────────────

function SiteShellSkeleton() {
  return (
    <>
      {/* Header strip skeleton */}
      <div className="-mx-6 flex h-12 items-center gap-3 border-b border-border bg-background px-6">
        <div className="flex min-w-0 flex-col gap-1">
          <Skeleton className="h-3.5 w-32" />
          <Skeleton className="h-2.5 w-48" />
        </div>
        <div className="ml-auto flex items-center gap-2">
          <Skeleton className="h-8 w-28 rounded-md" />
          <Skeleton className="size-8 rounded-md" />
        </div>
      </div>
      {/* Tab bar skeleton */}
      <div className="-mx-6 flex h-12 items-center gap-6 border-b border-border bg-background px-6">
        {TABS.map((t) => (
          <Skeleton key={t.to} className="h-3 w-14" />
        ))}
      </div>
    </>
  );
}

// ── Header + tab bar shell ──────────────────────────────────────────────────

function SiteShell({ site, siteId }: { site: Site; siteId: string }) {
  const [copied, setCopied] = useState(false);

  const hostname = hostnameOf(site.url);
  const adminUrl = `${stripTrailingSlash(site.url)}/wp-admin/`;
  const tone = toneForHealth(site.health_status);
  const toneLabel = labelForHealth(site.health_status);
  const lastSeen = relativeTime(site.last_seen_at) ?? undefined;

  const copySiteId = () => {
    void navigator.clipboard.writeText(site.id).then(() => {
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1500);
    });
  };

  return (
    <>
      {/* Site header strip. Scrolls with the page — only the AppShell topbar
          stays pinned. Shows site name + font-mono URL subtext + status chip. */}
      <header
        className={cn(
          "-mx-6 flex h-12 items-center gap-3 border-b border-border bg-background px-6",
          "min-w-0",
        )}
      >
        {/* Name + URL: name as the visual primary, hostname in mono as the URL */}
        <div className="flex min-w-0 flex-col gap-px">
          <span
            className="truncate text-sm font-medium text-foreground"
            title={site.name}
          >
            {site.name}
          </span>
          <span
            className="truncate font-mono text-[11px] text-muted-foreground"
            title={site.url}
          >
            {hostname}
          </span>
        </div>

        <StatusChip tone={tone} label={toneLabel} time={lastSeen} />

        <div className="ml-auto flex items-center gap-2">
          <Button asChild size="sm" aria-label="Open in wp-admin">
            <a href={adminUrl} target="_blank" rel="noopener noreferrer">
              <Zap aria-hidden="true" className="size-4" />
              Open wp-admin
            </a>
          </Button>
          <DropdownMenu>
            <DropdownMenuTrigger asChild>
              <Button
                type="button"
                variant="outline"
                size="icon"
                aria-label="More site actions"
              >
                <MoreHorizontal aria-hidden="true" className="size-4" />
              </Button>
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end">
              <DropdownMenuItem disabled title="Coming soon">
                Run health check
              </DropdownMenuItem>
              <DropdownMenuItem onClick={copySiteId}>
                {copied ? (
                  <>
                    <Check aria-hidden="true" className="size-4" />
                    Copied site ID
                  </>
                ) : (
                  <>
                    <Copy aria-hidden="true" className="size-4" />
                    Copy site ID
                  </>
                )}
              </DropdownMenuItem>
              <DropdownMenuSeparator />
              <DropdownMenuItem disabled title="Coming soon">
                Disconnect site
              </DropdownMenuItem>
            </DropdownMenuContent>
          </DropdownMenu>
        </div>
      </header>

      {/* Tab bar — scrolls with the page, not pinned. Each tab is a real
          TanStack Router Link; the router owns the active state via
          `activeProps`. */}
      <nav
        aria-label="Site sections"
        className="-mx-6 flex h-12 items-center gap-6 border-b border-border bg-background px-6"
      >
        {TABS.map((t) => (
          <Link
            key={t.to}
            to={t.to}
            params={{ siteId }}
            className="inline-flex h-12 items-center border-b-2 border-transparent text-sm text-muted-foreground transition-colors hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2"
            activeProps={{
              className:
                "inline-flex h-12 items-center -mb-px border-b-2 border-primary text-sm font-medium text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2",
            }}
          >
            {t.label}
          </Link>
        ))}
      </nav>

      {/* Child route content. Each child renders its own padded section. */}
      <Outlet />
    </>
  );
}

// ── Helpers ──────────────────────────────────────────────────────────────────

function whyFromSiteError(err: unknown): string {
  const message =
    err instanceof Error ? err.message : typeof err === "string" ? err : "";
  if (/500/.test(message)) return "We hit the server but it returned 500.";
  if (/503|504/.test(message)) return "The server is busy or restarting.";
  if (/Network|Failed to fetch/i.test(message))
    return "We couldn't reach the server. Check the network and try again.";
  return message || "The request failed without a server response.";
}

function hostnameOf(url: string): string {
  try {
    return new URL(url).host;
  } catch {
    return url;
  }
}

function stripTrailingSlash(url: string): string {
  return url.endsWith("/") ? url.slice(0, -1) : url;
}

function toneForHealth(status: Site["health_status"]): StatusTone {
  switch (status) {
    case "healthy":
      return "success";
    case "unreachable":
      return "destructive";
    case "unknown":
    default:
      return "muted";
  }
}

function labelForHealth(status: Site["health_status"]): string {
  switch (status) {
    case "healthy":
      return "Up";
    case "unreachable":
      return "Down";
    case "unknown":
    default:
      return "Unknown";
  }
}
