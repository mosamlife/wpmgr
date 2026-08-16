/**
 * Site row action menu — hoisted from sites-table.tsx so it can be reused by
 * both the table rows AND the site cards in the grid view. Pure presentational:
 * all state and event handlers come in as props. No behavior was changed during
 * the move.
 */
import {
  Camera,
  MoreHorizontal,
  PauseCircle,
  PlayCircle,
  RefreshCw,
  RotateCw,
  Trash2,
  Unplug,
  Zap,
} from "lucide-react";
import type { Site } from "@wpmgr/api";

import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import {
  isReconnectable,
  type ConnectionState,
} from "@/features/sites/connection-state";
import {
  useRecheckConnection,
  AgentUnreachableError,
} from "@/features/sites/use-site-connection";
import { useRefreshScreenshot } from "@/features/sites/use-sites";
import { isMonitoringPaused } from "@/features/sites/monitoring-pause";
import { cn } from "@/lib/utils";
import { toast } from "@/components/toast";

export interface SiteRowActionsProps {
  site: Site;
  connectionState: ConnectionState;
  onOpenAutoLogin?: (site: Site) => void;
  onOpenDetail?: (site: Site) => void;
  onDisconnect?: (site: Site) => void;
  onReconnect?: (site: Site) => void;
  onRemove?: (site: Site) => void;
  /** GH #414 — opens the pause-confirmation dialog for this one site. */
  onPauseMonitoring?: (site: Site) => void;
  /** GH #414 — resumes this one site directly, no confirmation (matches bulk). */
  onResumeMonitoring?: (site: Site) => void;
}

export function SiteRowActions({
  site,
  connectionState,
  onOpenAutoLogin,
  onOpenDetail,
  onDisconnect,
  onReconnect,
  onRemove,
  onPauseMonitoring,
  onResumeMonitoring,
}: SiteRowActionsProps) {
  // pending_enrollment ("Awaiting agent") also needs the code action — the raw
  // code is shown once, so a stuck-pending site has no other way back to it.
  const canReconnect =
    isReconnectable(connectionState) ||
    connectionState === "pending_enrollment";
  const reconnectLabel =
    connectionState === "pending_enrollment"
      ? "Get enrollment code"
      : "Reconnect";
  const canDisconnect =
    connectionState === "connected" || connectionState === "degraded";
  // Remove is only surfaced for archived/disconnected sites — states where
  // neither connecting nor active management is possible.
  const canRemove = isReconnectable(connectionState);
  // Re-check is available for any enrolled, signing-capable site — including
  // disconnected, where it is the primary way to recover a site that merely
  // fell behind on heartbeats (an unreachable agent returns a calm error, not
  // a hard failure). pending_enrollment/revoked/archived have no signed command
  // channel, so they stay excluded.
  const canRecheck =
    connectionState === "connected" ||
    connectionState === "degraded" ||
    connectionState === "disconnected";
  const recheck = useRecheckConnection();
  const refreshScreenshot = useRefreshScreenshot();

  // Screenshot refresh is only meaningful for enrolled sites (connected /
  // degraded / disconnected) — pending/revoked/archived have no agent to
  // trigger a capture job.
  const canRefreshScreenshot =
    connectionState === "connected" ||
    connectionState === "degraded" ||
    connectionState === "disconnected";

  // GH #414 — pause/resume is meaningful for the same enrolled states as
  // re-check/refresh-screenshot; pending/revoked/archived sites have nothing
  // to pause.
  const canToggleMonitoring =
    connectionState === "connected" ||
    connectionState === "degraded" ||
    connectionState === "disconnected";
  const paused = isMonitoringPaused(site);

  return (
    <div className="flex items-center justify-end gap-1">
      {canRecheck ? (
        <button
          type="button"
          aria-label="Re-check connection"
          title="Re-check connection"
          disabled={recheck.isPending}
          onClick={(e) => {
            e.stopPropagation();
            recheck.mutate(
              { siteId: site.id },
              {
                onSuccess: () =>
                  toast.success("Connection refreshed", {
                    description: "Agent responded.",
                  }),
                onError: (err) => {
                  if (err instanceof AgentUnreachableError) {
                    toast.info(err.message);
                  } else {
                    toast.error("Re-check failed", { description: err.message });
                  }
                },
              },
            );
          }}
          className="inline-flex size-7 items-center justify-center rounded text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 disabled:opacity-50"
        >
          <RefreshCw
            aria-hidden="true"
            className={cn("size-3.5", recheck.isPending && "animate-spin")}
          />
        </button>
      ) : null}
      <button
        type="button"
        aria-label={`Log in to ${site.name}`}
        title="Log in to site"
        onClick={(e) => {
          e.stopPropagation();
          onOpenAutoLogin?.(site);
        }}
        disabled={!onOpenAutoLogin}
        className="inline-flex size-7 items-center justify-center rounded text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 disabled:opacity-50"
      >
        <Zap aria-hidden="true" className="size-4" />
      </button>
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <button
            type="button"
            aria-label={`More actions for ${site.name}`}
            onClick={(e) => e.stopPropagation()}
            className="inline-flex size-7 items-center justify-center rounded text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2"
          >
            <MoreHorizontal aria-hidden="true" className="size-4" />
          </button>
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end">
          <DropdownMenuItem
            onSelect={(e) => {
              e.preventDefault();
              onOpenDetail?.(site);
            }}
          >
            Open site
          </DropdownMenuItem>
          <DropdownMenuItem
            onSelect={(e) => {
              e.preventDefault();
              onOpenAutoLogin?.(site);
            }}
            disabled={!onOpenAutoLogin}
          >
            Log in to site
          </DropdownMenuItem>
          <DropdownMenuSeparator />
          <DropdownMenuItem
            onSelect={(e) => {
              e.preventDefault();
              window.open(site.url, "_blank", "noopener,noreferrer");
            }}
          >
            Open site URL
          </DropdownMenuItem>
          {canRefreshScreenshot ? (
            <>
              <DropdownMenuSeparator />
              <DropdownMenuItem
                disabled={refreshScreenshot.isPending}
                onSelect={(e) => {
                  e.preventDefault();
                  refreshScreenshot.mutate(site.id, {
                    onSuccess: () =>
                      toast.success("Screenshot queued", {
                        description: "A new screenshot will appear shortly.",
                      }),
                    onError: (err) =>
                      toast.error("Could not refresh screenshot", {
                        description: err.message,
                      }),
                  });
                }}
              >
                <Camera aria-hidden="true" className="size-4" />
                Refresh screenshot
              </DropdownMenuItem>
            </>
          ) : null}
          {canToggleMonitoring && (paused ? onResumeMonitoring : onPauseMonitoring) ? (
            <>
              <DropdownMenuSeparator />
              <DropdownMenuItem
                onSelect={(e) => {
                  e.preventDefault();
                  if (paused) onResumeMonitoring?.(site);
                  else onPauseMonitoring?.(site);
                }}
              >
                {paused ? (
                  <>
                    <PlayCircle aria-hidden="true" className="size-4" />
                    Resume monitoring
                  </>
                ) : (
                  <>
                    <PauseCircle aria-hidden="true" className="size-4" />
                    Pause monitoring
                  </>
                )}
              </DropdownMenuItem>
            </>
          ) : null}
          {canReconnect && onReconnect ? (
            <>
              <DropdownMenuSeparator />
              <DropdownMenuItem
                onSelect={(e) => {
                  e.preventDefault();
                  onReconnect(site);
                }}
              >
                <RotateCw aria-hidden="true" className="size-4" />
                {reconnectLabel}
              </DropdownMenuItem>
            </>
          ) : null}
          {canRemove && onRemove ? (
            <>
              <DropdownMenuSeparator />
              <DropdownMenuItem
                className="text-destructive focus:text-destructive"
                onSelect={(e) => {
                  e.preventDefault();
                  onRemove(site);
                }}
              >
                <Trash2 aria-hidden="true" className="size-4" />
                Remove
              </DropdownMenuItem>
            </>
          ) : null}
          {canDisconnect && onDisconnect ? (
            <>
              <DropdownMenuSeparator />
              <DropdownMenuItem
                className="text-destructive focus:text-destructive"
                onSelect={(e) => {
                  e.preventDefault();
                  onDisconnect(site);
                }}
              >
                <Unplug aria-hidden="true" className="size-4" />
                Disconnect
              </DropdownMenuItem>
            </>
          ) : null}
        </DropdownMenuContent>
      </DropdownMenu>
    </div>
  );
}
