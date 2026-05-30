import { useCallback } from "react";

import { CopyableMono } from "@/components/shared/copyable-mono";
import { FreshnessBadge } from "@/components/shared/freshness-badge";
import { PageError } from "@/components/feedback";
import { toast } from "@/components/toast";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { useSite } from "@/features/sites/use-sites";
import { cn } from "@/lib/utils";
import type { SiteDiagnosticsCard } from "@wpmgr/api";

import { CardConstants } from "./card-constants";
import { CardCron } from "./card-cron";
import { CardDirectorySizes } from "./card-directory-sizes";
import { CardFilesystem, type FilesystemDisk } from "./card-filesystem";
import { CardHTTP } from "./card-http";
import { CardMedia } from "./card-media";
import { CardMySQL } from "./card-mysql";
import { CardPermissions } from "./card-permissions";
import { CardPHP } from "./card-php";
import { CardPlugins } from "./card-plugins";
import { CardSecurity } from "./card-security";
import { CardThemes } from "./card-themes";
import { CardUsers } from "./card-users";
import { EolChip } from "./eol-chip";
import { cardFor, useDiagnostics, useRefreshDiagnostics } from "./use-diagnostics";

// ADR-037 Impeccable, Batch 1 — the redesigned Site Health tab.
//
// One header ribbon (Host / PHP+EOL / WP / collected / as-of / Re-run all)
// replaces the old <h2> + duplicate host badge. Diagnostics are grouped into
// titled sections (Runtime / Storage / Content / Delivery / Configuration)
// rather than a flat 13-card grid. Two surface levels only: the section label
// is plain text, each card is the single surface (cards never nest).

export function HealthTab({ siteId }: { siteId: string }) {
  const { data, isPending, isError, error, refetch } = useDiagnostics(siteId);
  const { data: site } = useSite(siteId);
  const refresh = useRefreshDiagnostics(siteId);

  const onRefresh = useCallback(() => {
    refresh.mutate(undefined, {
      onSuccess: () => {
        toast.success("Queued a re-run of all checks", {
          description: "The agent will push fresh diagnostics shortly.",
        });
      },
      onError: (err) => {
        // The 503 "diagnostics_refresh_unwired" path surfaces as an honest
        // toast rather than inline amber text in the page body.
        toast.error("Could not queue a re-run", {
          description: err.message,
        });
      },
    });
  }, [refresh]);

  if (isPending) {
    return <HealthSkeleton />;
  }
  if (isError) {
    return (
      <section className="px-4 pb-8 pt-6 sm:px-6">
        <PageError
          what="Could not load site diagnostics."
          why={error instanceof Error ? error.message : "Unknown error"}
          onRetry={() => void refetch()}
          retryLabel="Reload diagnostics"
        />
      </section>
    );
  }

  const disk = diskFromSite(site?.components);

  return (
    <div className="space-y-6">
      <HealthRibbon
        data={data}
        refreshing={refresh.isPending}
        onRefresh={onRefresh}
      />

      <Section label="Runtime">
        <CardPHP card={cardFor(data, "php")} />
        <CardMySQL card={cardFor(data, "mysql")} />
      </Section>

      <Section label="Storage">
        <CardDirectorySizes card={cardFor(data, "wp_native")} />
        <CardFilesystem card={cardFor(data, "filesystem")} disk={disk} />
        <CardPermissions card={cardFor(data, "wp_native")} />
      </Section>

      <Section label="Content">
        <CardThemes card={cardFor(data, "themes")} />
        <CardPlugins card={cardFor(data, "plugins")} siteId={siteId} />
        <CardUsers card={cardFor(data, "users")} />
        <CardMedia card={cardFor(data, "wp_native")} />
      </Section>

      <Section label="Delivery">
        <CardHTTP card={cardFor(data, "http")} />
        <CardCron card={cardFor(data, "cron")} />
      </Section>

      <Section label="Configuration">
        <CardConstants card={cardFor(data, "wp_native")} />
        <CardSecurity card={cardFor(data, "security")} />
      </Section>
    </div>
  );
}

// ── Sections ──────────────────────────────────────────────────────────────────

// A titled group of diagnostic cards. The label is plain text (not a card), so
// the cards beneath are the only surface — never nested. Cards: 1-col <640,
// 2-col 768-1024, 3-col >1024.
function Section({
  label,
  children,
}: {
  label: string;
  children: React.ReactNode;
}) {
  return (
    <section className="space-y-3">
      <h3 className="text-xs font-medium uppercase tracking-wide text-muted-foreground">
        {label}
      </h3>
      <div className="grid grid-cols-1 gap-4 md:grid-cols-2 lg:grid-cols-3">
        {children}
      </div>
    </section>
  );
}

// ── Header ribbon ───────────────────────────────────────────────────────────

function HealthRibbon({
  data,
  refreshing,
  onRefresh,
}: {
  data: SiteDiagnosticsCard[] | undefined;
  refreshing: boolean;
  onRefresh: () => void;
}) {
  const identityCard = cardFor(data, "identity");
  const hostingCard = cardFor(data, "hosting");
  const phpCard = cardFor(data, "php");

  const identity = (identityCard?.payload ?? {}) as Record<string, unknown>;
  const hosting = (hostingCard?.payload ?? {}) as Record<string, unknown>;
  const php = (phpCard?.payload ?? {}) as Record<string, unknown>;

  const platform = pickPlatform(hosting);
  const phpVersion = typeof php.version === "string" ? php.version : null;
  const wpVersion =
    typeof identity.wp_version === "string"
      ? identity.wp_version
      : typeof identity.version === "string"
        ? identity.version
        : null;
  const asOfHash =
    typeof identity.site_as_of_hash === "string"
      ? identity.site_as_of_hash
      : null;
  // The freshest collection timestamp across the cards is the "Collected" time.
  const collectedAt = newestCollectedAt(data);

  return (
    <section
      aria-labelledby="health-heading"
      className="flex flex-wrap items-center gap-x-4 gap-y-2 rounded-lg border border-border bg-card px-4 py-3"
    >
      <h2
        id="health-heading"
        className="text-xs font-medium uppercase tracking-wide text-muted-foreground"
      >
        Health
      </h2>
      <Sep />
      <RibbonFact label="Host" value={platform ?? "Unrecognized"} />
      <Sep />
      <span className="inline-flex items-center gap-1.5 text-sm">
        <span className="text-muted-foreground">PHP</span>
        <span className="font-mono tabular-nums text-foreground">
          {phpVersion ?? "–"}
        </span>
        <EolChip version={phpVersion} />
      </span>
      {wpVersion ? (
        <>
          <Sep />
          <span className="inline-flex items-center gap-1.5 text-sm">
            <span className="text-muted-foreground">WP</span>
            <span className="font-mono tabular-nums text-foreground">
              {wpVersion}
            </span>
          </span>
        </>
      ) : null}
      <Sep />
      <span className="inline-flex items-center gap-1.5 text-sm text-muted-foreground">
        Collected <FreshnessBadge collectedAt={collectedAt} />
      </span>
      {asOfHash ? (
        <>
          <Sep />
          <span className="inline-flex items-center gap-1.5 text-sm">
            <span className="text-muted-foreground">as-of</span>
            <CopyableMono value={asOfHash} truncate label="Copy as-of hash" />
          </span>
        </>
      ) : null}

      <Button
        type="button"
        size="sm"
        variant="outline"
        className="ml-auto"
        disabled={refreshing}
        onClick={onRefresh}
      >
        {refreshing ? "Queuing…" : "Re-run all checks"}
      </Button>
    </section>
  );
}

function RibbonFact({ label, value }: { label: string; value: string }) {
  return (
    <span className="inline-flex items-center gap-1.5 text-sm">
      <span className="text-muted-foreground">{label}</span>
      <span className="text-foreground">{value}</span>
    </span>
  );
}

function Sep() {
  return (
    <span aria-hidden="true" className="text-muted-foreground">
      ·
    </span>
  );
}

// ── Skeleton ──────────────────────────────────────────────────────────────────

function HealthSkeleton() {
  return (
    <div className="space-y-6" role="status" aria-busy="true">
      <span className="sr-only">Loading diagnostics</span>
      <div className="flex flex-wrap items-center gap-4 rounded-lg border border-border bg-card px-4 py-3">
        <Skeleton className="h-4 w-16" />
        <Skeleton className="h-4 w-28" />
        <Skeleton className="h-4 w-24" />
        <Skeleton className="ml-auto h-8 w-32" />
      </div>
      {Array.from({ length: 2 }).map((_, s) => (
        <div key={s} className="space-y-3">
          <Skeleton className="h-3 w-20" />
          <div className="grid grid-cols-1 gap-4 md:grid-cols-2 lg:grid-cols-3">
            {Array.from({ length: 3 }).map((_, c) => (
              <div
                key={c}
                className={cn(
                  "flex flex-col gap-3 rounded-lg border border-border bg-card p-5",
                )}
              >
                <div className="flex items-center justify-between">
                  <Skeleton className="h-4 w-24" />
                  <Skeleton className="h-3 w-16" />
                </div>
                <Skeleton className="h-3 w-full" />
                <Skeleton className="h-3 w-5/6" />
                <Skeleton className="h-3 w-2/3" />
              </div>
            ))}
          </div>
        </div>
      ))}
    </div>
  );
}

// ── Helpers ──────────────────────────────────────────────────────────────────

function pickPlatform(hosting: Record<string, unknown>): string | null {
  if (hosting.is_wpengine) return "WP Engine";
  if (hosting.is_kinsta) return "Kinsta";
  if (hosting.is_pressable) return "Pressable";
  if (hosting.is_atomic) return "WordPress.com Atomic";
  if (hosting.is_flywheel) return "Flywheel";
  if (hosting.is_gridpane) return "GridPane";
  if (hosting.is_runcloud) return "RunCloud";
  if (hosting.is_cloudways) return "Cloudways";
  return null;
}

function newestCollectedAt(
  data: SiteDiagnosticsCard[] | undefined,
): string | null {
  let newest: number | null = null;
  let newestIso: string | null = null;
  for (const card of data ?? []) {
    if (!card.collected_at) continue;
    const ts = Date.parse(card.collected_at);
    if (Number.isNaN(ts)) continue;
    if (newest === null || ts > newest) {
      newest = ts;
      newestIso = card.collected_at;
    }
  }
  return newestIso;
}

// The agent's sparse-metadata expansion writes disk usage as sibling keys to
// plugins/themes inside the site's JSONB `components` blob. The generated Site
// type does not surface these optional keys, so we read through a tolerant
// shape. Old agents send none of these and the bar simply does not render.
type SiteComponentsExtras = {
  disk?: {
    wp_content_bytes?: number;
    uploads_bytes?: number;
    free_bytes?: number;
  };
};

function diskFromSite(components: unknown): FilesystemDisk | undefined {
  if (!components || typeof components !== "object") return undefined;
  const disk = (components as SiteComponentsExtras).disk;
  if (!disk) return undefined;
  return {
    wpContentBytes: disk.wp_content_bytes ?? 0,
    uploadsBytes: disk.uploads_bytes ?? 0,
    freeBytes: disk.free_bytes ?? 0,
  };
}
