import { PageError } from "@/components/feedback";
import { Skeleton } from "@/components/ui/skeleton";

import { usePerfConfig, useUpdatePerfConfig } from "../hooks/usePerfConfig";
import { useCacheStats } from "../hooks/useCacheStats";
import { usePerfEvents } from "../hooks/usePerfEvents";
import type { PerfConfig } from "../types";
import { CacheOverview } from "./CacheOverview";
import { CacheControls } from "./CacheControls";
import { CacheBasicSettings } from "./CacheBasicSettings";
import { PreloadTuningCard } from "./PreloadTuningCard";
import { CacheAdvancedSettings } from "./CacheAdvancedSettings";
import { WooCommerceSection } from "./WooCommerceSection";
import { ServerStatusCard } from "./ServerStatusCard";
import { PreloadProgress } from "./PreloadProgress";
import { CacheHitRatioTrendCard } from "./CacheHitRatioTrendCard";
import { ObjectCachePanel } from "./ObjectCachePanel";

// CacheTab — the Cache entry surface. Overview tiles + live preload progress +
// the action toolbar + the server-status card + the basic/advanced settings
// panels + the Object Cache section. SSE is wired here via usePerfEvents so the
// tab patches its own caches live (no polling). Every setting autosaves
// (optimistic PUT → toast on error).

export interface CacheTabProps {
  siteId: string;
  hostname: string;
  /** operator+ — change settings, purge, preload, enable/disable. */
  canOperate: boolean;
  /** admin+ — the destructive delete-everything purge. */
  canManage: boolean;
  /** Agent version string from the site record (may be undefined). */
  agentVersion?: string;
}

export function CacheTab({
  siteId,
  hostname,
  canOperate,
  canManage,
  agentVersion,
}: CacheTabProps) {
  usePerfEvents(siteId);

  const config = usePerfConfig(siteId);
  const stats = useCacheStats(siteId);
  const update = useUpdatePerfConfig(siteId);

  function save(patch: Partial<PerfConfig>) {
    if (!config.data) return;
    update.mutate({ ...config.data, ...patch });
  }

  if (config.isPending) {
    return <CacheTabSkeleton />;
  }

  if (config.isError || !config.data) {
    return (
      <PageError
        what="Could not load this site's cache settings."
        why={config.error?.message ?? "Unknown error"}
        onRetry={() => void config.refetch()}
        retryLabel="Reload cache"
      />
    );
  }

  const cfg = config.data;
  const disabled = !canOperate || update.isPending;

  return (
    <div className="space-y-4">
      <CacheOverview stats={stats.data} />

      <PreloadProgress siteId={siteId} />

      <CacheHitRatioTrendCard siteId={siteId} htaccessManaged={cfg.htaccess_managed} />

      {canOperate || canManage ? (
        <CacheControls
          siteId={siteId}
          hostname={hostname}
          cacheEnabled={cfg.cache_enabled}
          canPurge={canOperate}
          canManage={canOperate}
          canDeleteAll={canManage}
        />
      ) : null}

      <ServerStatusCard siteId={siteId} config={cfg} />

      <CacheBasicSettings
        config={cfg}
        save={save}
        disabled={disabled}
        saving={update.isPending}
      />

      <PreloadTuningCard config={cfg} save={save} disabled={disabled} />

      <CacheAdvancedSettings config={cfg} save={save} disabled={disabled} />

      <WooCommerceSection
        config={cfg}
        save={save}
        disabled={disabled}
        saving={update.isPending}
      />

      <section aria-labelledby="oc-section-heading">
        <h2
          id="oc-section-heading"
          className="mb-3 text-sm font-semibold text-foreground"
        >
          Object Cache
        </h2>
        <ObjectCachePanel
          siteId={siteId}
          agentVersion={agentVersion}
          canOperate={canOperate}
        />
      </section>
    </div>
  );
}

function CacheTabSkeleton() {
  return (
    <div
      role="status"
      aria-busy="true"
      aria-label="Loading cache settings"
      className="space-y-4"
    >
      <span className="sr-only">Loading cache settings</span>
      <div className="grid grid-cols-2 gap-px overflow-hidden rounded-xl border border-border bg-border sm:grid-cols-4">
        {Array.from({ length: 4 }).map((_, i) => (
          <div key={i} className="space-y-2 bg-card p-4">
            <Skeleton className="h-3 w-20" />
            <Skeleton className="h-7 w-16" />
            <Skeleton className="h-3 w-24" />
          </div>
        ))}
      </div>
      <div className="flex gap-2">
        <Skeleton className="h-8 w-32" />
        <Skeleton className="h-8 w-28" />
        <Skeleton className="h-8 w-36" />
      </div>
      <Skeleton className="h-32 w-full rounded-xl" />
      <Skeleton className="h-64 w-full rounded-xl" />
    </div>
  );
}
