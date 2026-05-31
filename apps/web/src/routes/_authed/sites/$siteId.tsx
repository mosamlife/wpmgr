import { useCallback, useState } from "react";
import { createFileRoute, Link, Outlet } from "@tanstack/react-router";
import {
  Archive,
  Check,
  Copy,
  MoreHorizontal,
  RotateCw,
  Share2,
  Unplug,
  Zap,
} from "lucide-react";

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
import { ConnectionStateBadge } from "@/components/status";
import { DestructiveConfirm } from "@/components/dialogs/destructive-confirm";
import { ShareSiteDialog } from "@/features/sharing/share-site-dialog";
import { AddSiteDialog } from "@/features/sites/add-site-dialog";
import { useSite, NotFoundError } from "@/features/sites/use-sites";
import { useSitesLiveSync } from "@/features/sites/use-sites-live";
import {
  connectionStateOf,
  isReconnectable,
} from "@/features/sites/connection-state";
import {
  useRevokeSite,
  useArchiveSite,
  useRestoreSite,
  useCreateEnrollmentCode,
} from "@/features/sites/use-site-connection";
import { useRecordRecentSite } from "@/features/command/use-recent-sites";
import { useMe, canManage } from "@/features/auth/use-auth";
import { toast } from "@/components/toast";
import { cn } from "@/lib/utils";
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
  // ADR-043 — Media Optimizer tab. Route file:
  // apps/web/src/routes/_authed/sites/$siteId.media.tsx
  { to: "/sites/$siteId/media", label: "Media" },
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

  // Phase 5 — keep the connection badge + detail cache live over SSE while the
  // detail page is open (no polling). The shared singleton stream is reused.
  useSitesLiveSync();

  // Sprint 3 surface 4.4: feed the command palette's "recently viewed" list.
  // Fires once per site (the hook keys on site.id internally), not per tab
  // switch — the layout component does not unmount when the Outlet swaps.
  useRecordRecentSite(site);

  return (
    <div className="mx-auto w-full max-w-[1200px]">
      {isPending ? (
        <SiteShellSkeleton />
      ) : isError ? (
        <div className="px-4 py-6 sm:px-6 sm:py-8">
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
      <div className="-mx-4 flex h-12 items-center gap-3 border-b border-border bg-background px-4 sm:-mx-6 sm:px-6">
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
      <div className="-mx-4 flex h-12 items-center gap-4 overflow-x-auto border-b border-border bg-background px-4 sm:-mx-6 sm:gap-6 sm:px-6">
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
  const [shareOpen, setShareOpen] = useState(false);
  const [disconnectOpen, setDisconnectOpen] = useState(false);
  const [archiveOpen, setArchiveOpen] = useState(false);
  const [reconnect, setReconnect] = useState<{
    siteId: string;
    url: string;
    enrollmentCode: string;
    expiresAt: string;
  } | null>(null);
  const { data: me } = useMe();
  const manage = canManage(me);

  const revoke = useRevokeSite();
  const archive = useArchiveSite();
  const restore = useRestoreSite();
  const enrollmentCode = useCreateEnrollmentCode();

  const hostname = hostnameOf(site.url);
  const adminUrl = `${stripTrailingSlash(site.url)}/wp-admin/`;
  const connectionState = connectionStateOf(site);
  // pending_enrollment ("Awaiting agent") also gets the code action — the raw
  // enrollment code is shown once, so a stuck-pending site needs a way back to it.
  const canReconnect =
    isReconnectable(connectionState) ||
    connectionState === "pending_enrollment";
  const reconnectLabel =
    connectionState === "pending_enrollment"
      ? "Get enrollment code"
      : "Reconnect";
  const canDisconnect =
    connectionState === "connected" || connectionState === "degraded";
  const canArchive =
    connectionState === "disconnected" || connectionState === "revoked";

  const copySiteId = () => {
    void navigator.clipboard.writeText(site.id).then(() => {
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1500);
    });
  };

  const confirmDisconnect = useCallback(() => {
    revoke.mutate(
      { siteId: site.id, reason: "operator disconnect" },
      {
        onSuccess: () => {
          setDisconnectOpen(false);
          toast.success(`${hostname} disconnected.`, {
            description: "Backups and monitoring are paused. History is kept.",
            action: {
              label: "Undo",
              onClick: () => {
                restore.mutate(
                  { siteId: site.id },
                  {
                    onSuccess: () => toast.success(`${hostname} reconnecting…`),
                    onError: (err) =>
                      toast.error("Could not undo", {
                        description: err.message,
                      }),
                  },
                );
              },
            },
          });
        },
        onError: (err) =>
          toast.error(`Could not disconnect ${hostname}`, {
            description: err.message,
          }),
      },
    );
  }, [revoke, restore, site.id, hostname]);

  const confirmArchive = useCallback(() => {
    archive.mutate(
      { siteId: site.id, reason: "operator archive" },
      {
        onSuccess: () => {
          setArchiveOpen(false);
          toast.success(`${hostname} archived.`);
        },
        onError: (err) =>
          toast.error(`Could not archive ${hostname}`, {
            description: err.message,
          }),
      },
    );
  }, [archive, site.id, hostname]);

  const startReconnect = useCallback(() => {
    toast.info(`Generating an enrollment code for ${hostname}…`);
    enrollmentCode.mutate(
      { siteId: site.id },
      {
        onSuccess: (result) =>
          setReconnect({
            siteId: site.id,
            url: site.url,
            enrollmentCode: result.enrollment_code,
            expiresAt: result.expires_at,
          }),
        onError: (err) =>
          toast.error(`Could not start reconnecting ${hostname}`, {
            description: err.message,
          }),
      },
    );
  }, [enrollmentCode, site.id, site.url, hostname]);

  return (
    <>
      {/* Site header strip. Scrolls with the page — only the AppShell topbar
          stays pinned. Shows site name + font-mono URL subtext + status chip. */}
      <header
        className={cn(
          "-mx-4 flex h-12 min-w-0 items-center gap-3 border-b border-border bg-background px-4",
          "sm:-mx-6 sm:px-6",
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

        <ConnectionStateBadge
          state={connectionState}
          lastSeenAt={site.last_seen_at ?? null}
        />

        <div className="ml-auto flex items-center gap-2">
          {manage ? (
            <Button
              type="button"
              variant="outline"
              size="sm"
              aria-label="Share this site with collaborators"
              onClick={() => setShareOpen(true)}
              className="gap-1.5"
            >
              <Share2 aria-hidden="true" className="size-4" />
              Share
            </Button>
          ) : null}
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
              {manage && canReconnect ? (
                <>
                  <DropdownMenuSeparator />
                  <DropdownMenuItem onSelect={startReconnect}>
                    <RotateCw aria-hidden="true" className="size-4" />
                    {reconnectLabel}
                  </DropdownMenuItem>
                </>
              ) : null}
              {manage && canArchive ? (
                <DropdownMenuItem onSelect={() => setArchiveOpen(true)}>
                  <Archive aria-hidden="true" className="size-4" />
                  Archive
                </DropdownMenuItem>
              ) : null}
              {manage && canDisconnect ? (
                <>
                  <DropdownMenuSeparator />
                  <DropdownMenuItem
                    className="text-destructive focus:text-destructive"
                    onSelect={() => setDisconnectOpen(true)}
                  >
                    <Unplug aria-hidden="true" className="size-4" />
                    Disconnect site
                  </DropdownMenuItem>
                </>
              ) : null}
            </DropdownMenuContent>
          </DropdownMenu>
        </div>

        {/* Share site dialog — only rendered when opened to keep DOM clean. */}
        <ShareSiteDialog
          siteId={siteId}
          siteName={site.name}
          open={shareOpen}
          onClose={() => setShareOpen(false)}
        />

        {/* Phase 5 — Disconnect (type the hostname). */}
        <DestructiveConfirm
          open={disconnectOpen}
          onClose={() => setDisconnectOpen(false)}
          onConfirm={confirmDisconnect}
          title={`Disconnect ${hostname}`}
          resourceName={hostname}
          confirmLabel="Disconnect site"
          cancelLabel="Keep connected"
          isPending={revoke.isPending}
          errorMessage={revoke.isError ? revoke.error.message : null}
          consequencesBody={
            <div className="space-y-2">
              <p>
                We'll send a revoke to the agent on its next heartbeat
                (within ~60 seconds). The agent stops accepting commands and
                clears its credentials.
              </p>
              <p>
                Backups and monitoring stop. The site is archived with its full
                history kept — you can reconnect later.
              </p>
            </div>
          }
        />

        {/* Phase 5 — Archive a disconnected/revoked site. */}
        <DestructiveConfirm
          open={archiveOpen}
          onClose={() => setArchiveOpen(false)}
          onConfirm={confirmArchive}
          title={`Archive ${hostname}`}
          resourceName={hostname}
          confirmLabel="Archive site"
          cancelLabel="Keep in list"
          isPending={archive.isPending}
          errorMessage={archive.isError ? archive.error.message : null}
          consequencesBody={
            <p>
              The site is hidden from the default sites list. Its history is
              kept and you can restore it from the Archived filter at any time.
            </p>
          }
        />

        {/* Phase 5 — Reconnect: open the Add-site modal at step B. */}
        <AddSiteDialog
          open={reconnect !== null}
          onClose={() => setReconnect(null)}
          initialSite={reconnect ?? undefined}
        />
      </header>

      {/* Tab bar — scrolls with the page, not pinned. Each tab is a real
          TanStack Router Link; the router owns the active state via
          `activeProps`. */}
      <nav
        aria-label="Site sections"
        className="-mx-4 flex h-12 items-center gap-4 overflow-x-auto border-b border-border bg-background px-4 sm:-mx-6 sm:gap-6 sm:px-6"
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
