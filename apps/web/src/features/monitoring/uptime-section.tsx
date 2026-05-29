import { useState } from "react";

import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  useSiteUptime,
  UPTIME_WINDOWS,
  NotFoundError,
  type UptimeWindow,
} from "@/features/monitoring/use-uptime";
import { UptimeStatusBadge } from "@/features/monitoring/uptime-badges";
import { statusFromStatus } from "@/features/monitoring/uptime-badges-helpers";
import { UptimeChart } from "@/features/monitoring/uptime-chart";
import { relativeTime } from "@/lib/utils";
import type { UptimeStatus } from "@wpmgr/api";

// Uptime section for the site detail page. Shows current up/down status,
// uptime % / avg latency for a selectable window (7d/30d/90d), last-checked
// relative time, TLS certificate expiry (warning under 14 days), and a
// latency/uptime sparkline. Data refetches on a 60s interval (see useSiteUptime).

const DAY_MS = 24 * 60 * 60 * 1000;
const TLS_WARN_DAYS = 14;

export function UptimeSection({ siteId }: { siteId: string }) {
  const [window, setWindow] = useState<UptimeWindow>("7d");
  const { data, isPending, isError, error, refetch, isFetching } =
    useSiteUptime(siteId, window);

  return (
    <Card>
      <CardHeader>
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div className="space-y-1">
            <CardTitle>Uptime</CardTitle>
            <CardDescription>
              Availability, latency and TLS health from periodic probes.
            </CardDescription>
          </div>
          <WindowToggle value={window} onChange={setWindow} busy={isFetching} />
        </div>
      </CardHeader>
      <CardContent>
        {isPending ? (
          <p
            role="status"
            className="text-sm text-[var(--color-muted-foreground)]"
          >
            Loading uptime…
          </p>
        ) : isError ? (
          error instanceof NotFoundError ? (
            <p className="text-sm text-[var(--color-muted-foreground)]">
              No checks yet for this site.
            </p>
          ) : (
            <div role="alert" className="space-y-2">
              <p className="text-sm text-[var(--color-destructive)]">
                {error.message}
              </p>
              <Button
                variant="outline"
                size="sm"
                onClick={() => void refetch()}
              >
                Retry
              </Button>
            </div>
          )
        ) : data.series.length === 0 && !data.last_check ? (
          <p className="text-sm text-[var(--color-muted-foreground)]">
            No checks yet for this site.
          </p>
        ) : (
          <UptimeBody status={data} />
        )}
      </CardContent>
    </Card>
  );
}

function UptimeBody({ status }: { status: UptimeStatus }) {
  const lastChecked = relativeTime(status.last_check);
  const tls = tlsExpiryInfo(status.tls_expiry);
  // tls_issuer / tls_subject are optional on the wire — the CP added them as
  // part of v0.9.3-tls-cert-fields but they were dropped from the regenerated
  // schema in the post-restyle reverts. Read them defensively so the section
  // degrades to "Unknown" when absent and starts populating the moment the
  // schema re-adds them.
  const tlsExtras = status as UptimeStatus & {
    tls_issuer?: string | null;
    tls_subject?: string | null;
  };
  const tlsIssuer = tlsExtras.tls_issuer ?? "";
  const tlsSubject = tlsExtras.tls_subject ?? "";

  return (
    <div className="space-y-4">
      <dl className="grid grid-cols-2 gap-x-6 gap-y-4 text-sm sm:grid-cols-4">
        <Stat label="Status">
          <UptimeStatusBadge status={statusFromStatus(status)} />
        </Stat>
        <Stat label="Uptime">
          <span className="text-lg font-semibold">
            {status.uptime_pct.toFixed(2)}%
          </span>
        </Stat>
        <Stat label="Avg latency">
          <span className="text-lg font-semibold">
            {status.avg_latency_ms} ms
          </span>
        </Stat>
        <Stat label="Last checked">
          <span className="font-medium">{lastChecked ?? "Never"}</span>
        </Stat>
      </dl>

      <div className="space-y-1 text-sm">
        <div className="text-[var(--color-muted-foreground)]">
          TLS certificate
        </div>
        <dl className="grid grid-cols-[7rem_1fr] gap-x-4 gap-y-1">
          <dt className="text-[var(--color-muted-foreground)]">Issued by</dt>
          <dd className="font-mono">
            {tlsIssuer ? (
              tlsIssuer
            ) : (
              <span className="text-[var(--color-muted-foreground)]">Unknown</span>
            )}
          </dd>
          <dt className="text-[var(--color-muted-foreground)]">Subject</dt>
          <dd className="font-mono">
            {tlsSubject ? (
              tlsSubject
            ) : (
              <span className="text-[var(--color-muted-foreground)]">Unknown</span>
            )}
          </dd>
          <dt className="text-[var(--color-muted-foreground)]">Expires</dt>
          <dd>
            {tls ? (
              <span
                className={
                  tls.warn
                    ? "font-mono font-medium text-[var(--color-destructive)]"
                    : "font-mono font-medium"
                }
              >
                {tls.relative}
                {tls.warn ? " — renew soon" : ""}
              </span>
            ) : (
              <span className="text-[var(--color-muted-foreground)]">Unknown</span>
            )}
          </dd>
        </dl>
      </div>

      <UptimeChart series={status.series} />
    </div>
  );
}

function Stat({
  label,
  children,
}: {
  label: string;
  children: React.ReactNode;
}) {
  return (
    <div className="space-y-1">
      <dt className="text-[var(--color-muted-foreground)]">{label}</dt>
      <dd>{children}</dd>
    </div>
  );
}

function WindowToggle({
  value,
  onChange,
  busy,
}: {
  value: UptimeWindow;
  onChange: (w: UptimeWindow) => void;
  busy: boolean;
}) {
  return (
    <div
      role="group"
      aria-label="Uptime window"
      className="inline-flex rounded-md border border-[var(--color-border)]"
    >
      {UPTIME_WINDOWS.map((w) => {
        const active = w.value === value;
        return (
          <button
            key={w.value}
            type="button"
            aria-pressed={active}
            onClick={() => onChange(w.value)}
            className={
              "px-3 py-1.5 text-sm font-medium transition-colors first:rounded-l-md last:rounded-r-md focus-visible:ring-2 focus-visible:ring-[var(--color-ring)] focus-visible:outline-none " +
              (active
                ? "bg-[var(--color-primary)] text-[var(--color-primary-foreground)]"
                : "hover:bg-[var(--color-accent)]")
            }
          >
            {w.value}
            {active && busy ? (
              <span className="sr-only"> (refreshing)</span>
            ) : null}
          </button>
        );
      })}
    </div>
  );
}

function tlsExpiryInfo(
  tlsExpiry: string | undefined,
): { relative: string; warn: boolean } | null {
  if (!tlsExpiry) return null;
  const expiry = Date.parse(tlsExpiry);
  if (Number.isNaN(expiry)) return null;
  const relative = relativeTime(tlsExpiry) ?? "soon";
  const daysLeft = (expiry - Date.now()) / DAY_MS;
  return { relative, warn: daysLeft < TLS_WARN_DAYS };
}
