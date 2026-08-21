// PortalSiteCard v2 — site card driven by summary.sites (PortalSummarySite).
//
// Changes from v1 (PortalSite-based card):
//   - Letter avatar (site name initial on --color-primary/10 background)
//   - Uptime sparkline from uptime_daily[]
//   - Uptime pct + vitals rating chip
//   - Backups + updates in-period counts
//   - Links to the same /portal/sites/$siteId detail page
//
// Soft status wording (locked contract §2.9):
//   connected -> "Monitoring active"
//   anything else -> "Needs attention"

import { Link } from "@tanstack/react-router";
import { ExternalLink, Shield, ShieldOff } from "lucide-react";

import {
  Card,
  CardContent,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Sparkline } from "@/components/charts/sparkline";
import { cn } from "@/lib/utils";
import { relativeTime } from "@/lib/utils";
import type { PortalSummarySite, PortalUptimeDay } from "./use-portal";

// ---------------------------------------------------------------------------
// Soft status helpers (locked decision, contract §2.9)
// ---------------------------------------------------------------------------

function siteStatusLabel(status: string): string {
  return status === "connected" ? "Monitoring active" : "Needs attention";
}

function siteStatusVariant(
  status: string,
): "default" | "secondary" | "destructive" | "outline" {
  return status === "connected" ? "default" : "destructive";
}

// ---------------------------------------------------------------------------
// TLS expiry helper
// ---------------------------------------------------------------------------

function tlsLabel(expiresAt: string | null | undefined): string | null {
  if (!expiresAt) return null;
  const exp = new Date(expiresAt);
  const now = Date.now();
  const daysLeft = Math.round((exp.getTime() - now) / 86_400_000);
  if (daysLeft <= 0) return "TLS expired";
  if (daysLeft <= 30) return `TLS expires in ${daysLeft}d`;
  return null;
}

// ---------------------------------------------------------------------------
// Letter avatar
// ---------------------------------------------------------------------------

function LetterAvatar({ name }: { name: string }) {
  const initial = name.trim().charAt(0).toUpperCase() || "?";
  return (
    <span
      aria-hidden="true"
      className="flex size-8 shrink-0 items-center justify-center rounded-full bg-[var(--color-primary)]/10 text-sm font-semibold text-[var(--color-primary)]"
    >
      {initial}
    </span>
  );
}

// ---------------------------------------------------------------------------
// Vitals chip
// ---------------------------------------------------------------------------

const VITALS_LABEL: Record<string, string> = {
  good: "Good",
  "needs-improvement": "Fair",
  poor: "Poor",
};

const VITALS_VARIANT: Record<
  string,
  "default" | "secondary" | "destructive" | "outline"
> = {
  good: "default",
  "needs-improvement": "secondary",
  poor: "destructive",
};

function VitalsChip({ rating }: { rating: PortalSummarySite["vitals_rating"] }) {
  if (!rating) return null;
  return (
    <Badge
      variant={VITALS_VARIANT[rating] ?? "outline"}
      className="text-xs"
    >
      {VITALS_LABEL[rating] ?? rating}
    </Badge>
  );
}

// ---------------------------------------------------------------------------
// Sparkline adapter
// ---------------------------------------------------------------------------

function toSparklineData(days: PortalUptimeDay[] | undefined): number[] {
  if (!days || days.length < 2) return [];
  return days.map((d) => d.uptime_pct);
}

// ---------------------------------------------------------------------------
// Component
// ---------------------------------------------------------------------------

export interface PortalSiteCardV2Props {
  site: PortalSummarySite;
}

export function PortalSiteCardV2({ site }: PortalSiteCardV2Props) {
  const statusLabel = siteStatusLabel(site.status);
  const statusVariant = siteStatusVariant(site.status);
  const tls = tlsLabel(site.tls_expires_at);
  const sparkData = toSparklineData(site.uptime_daily);
  const sparkTone =
    site.status === "connected" ? ("success" as const) : ("warning" as const);

  return (
    <Card className="flex flex-col transition-shadow hover:shadow-sm">
      <CardHeader className="pb-2">
        <div className="flex items-start gap-2">
          <LetterAvatar name={site.name} />
          <div className="min-w-0 flex-1">
            <CardTitle className="text-base leading-tight">
              <Link
                to="/portal/sites/$siteId"
                params={{ siteId: site.id }}
                className="truncate hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)]"
              >
                {site.name}
              </Link>
            </CardTitle>
            <a
              href={site.url}
              target="_blank"
              rel="noopener noreferrer"
              className="flex min-w-0 items-center gap-1 truncate text-xs text-[var(--color-muted-foreground)] hover:text-[var(--color-foreground)]"
            >
              <ExternalLink aria-hidden="true" className="size-3 shrink-0" />
              <span className="truncate">
                {site.url.replace(/^https?:\/\//, "")}
              </span>
            </a>
          </div>
          <Badge variant={statusVariant} className="shrink-0 text-xs">
            {statusLabel}
          </Badge>
        </div>
      </CardHeader>

      <CardContent className="flex flex-1 flex-col gap-2 pt-0">
        {/* Uptime row: pct + sparkline */}
        <div className="flex items-center justify-between gap-2">
          {site.uptime_pct != null ? (
            <span className="font-mono text-xs tabular-nums text-[var(--color-muted-foreground)]">
              {site.uptime_pct.toFixed(2)}% uptime
            </span>
          ) : (
            <span className="text-xs text-[var(--color-muted-foreground)]">
              No uptime data
            </span>
          )}
          {sparkData.length >= 2 ? (
            <Sparkline
              data={sparkData}
              width={56}
              height={16}
              tone={sparkTone}
              ariaLabel={`${site.name} uptime sparkline`}
            />
          ) : null}
        </div>

        {/* Vitals chip */}
        <VitalsChip rating={site.vitals_rating} />

        {/* TLS warning */}
        {tls ? (
          <span
            className={cn(
              "flex items-center gap-1 text-xs",
              tls === "TLS expired"
                ? "text-[var(--color-destructive)]"
                : "text-[var(--color-warning)]",
            )}
          >
            {tls === "TLS expired" ? (
              <ShieldOff aria-hidden="true" className="size-3 shrink-0" />
            ) : (
              <Shield aria-hidden="true" className="size-3 shrink-0" />
            )}
            {tls}
          </span>
        ) : null}

        {/* Backup row */}
        {site.last_backup_at ? (
          <p className="text-xs text-[var(--color-muted-foreground)]">
            Last backup {relativeTime(site.last_backup_at)}
          </p>
        ) : (
          <p className="text-xs text-[var(--color-muted-foreground)]">
            No backups recorded
          </p>
        )}

        {/* Period counts */}
        {(site.backups_in_period > 0 || site.updates_in_period > 0) ? (
          <div className="flex flex-wrap gap-3 border-t border-[var(--color-border)] pt-2 text-xs text-[var(--color-muted-foreground)]">
            {site.backups_in_period > 0 ? (
              <span>
                <span className="font-mono tabular-nums font-medium text-[var(--color-foreground)]">
                  {site.backups_in_period}
                </span>{" "}
                {site.backups_in_period === 1 ? "backup" : "backups"} this period
              </span>
            ) : null}
            {site.updates_in_period > 0 ? (
              <span>
                <span className="font-mono tabular-nums font-medium text-[var(--color-foreground)]">
                  {site.updates_in_period}
                </span>{" "}
                {site.updates_in_period === 1 ? "update" : "updates"} this period
              </span>
            ) : null}
          </div>
        ) : null}
      </CardContent>
    </Card>
  );
}
