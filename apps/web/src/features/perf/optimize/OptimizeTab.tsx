import { useCallback, useRef, useState } from "react";

import { PageError } from "@/components/feedback";
import { Skeleton } from "@/components/ui/skeleton";

import { usePerfConfig, useUpdatePerfConfig } from "../hooks/usePerfConfig";
import { usePerfEvents } from "../hooks/usePerfEvents";
import type { PerfConfig } from "../types";
import { CssJsSection } from "./CssJsSection";
import { FontsSection } from "./FontsSection";
import { MediaHtmlSection } from "./MediaHtmlSection";
import { CdnSection } from "./CdnSection";
import { BloatSection } from "./BloatSection";
import { DatabaseSection } from "./DatabaseSection";
import { RucssResultsTable } from "./RucssResultsTable";
import { FontResultsTable } from "./FontResultsTable";
import { RumSection } from "./RumSection";
import { RumResultsTable } from "./RumResultsTable";

// OptimizeTab — the asset-optimization entry surface. Settings sections
// (CSS/JS, Fonts, Media/HTML, CDN, Bloat, Database, RUM) plus results tables
// (RUCSS, Fonts, RUM). SSE is wired here via usePerfEvents so config, RUCSS
// results, and RUM rollup data stay live. Every setting autosaves
// (optimistic PUT with rollback on error).

export interface OptimizeTabProps {
  siteId: string;
  hostname: string;
  /** operator+ — change settings, clear RUCSS, clean DB. */
  canOperate: boolean;
}

export function OptimizeTab({ siteId, hostname, canOperate }: OptimizeTabProps) {
  usePerfEvents(siteId);

  const config = usePerfConfig(siteId);
  const update = useUpdatePerfConfig(siteId);

  // Per-key saving state: track which config keys are currently in-flight.
  // Use a ref for the Set (mutated imperatively) and a counter to force
  // re-renders when membership changes.
  const inFlightKeys = useRef<Set<string>>(new Set());
  const [, setRenderTick] = useState(0);
  const forceUpdate = useCallback(() => setRenderTick((n) => n + 1), []);

  function save(patch: Partial<PerfConfig>, onError?: (err: Error) => void) {
    if (!config.data) return;
    const keys = Object.keys(patch);
    // Register keys as in-flight before the mutation fires.
    for (const k of keys) inFlightKeys.current.add(k);
    forceUpdate();
    update.mutate(
      { ...config.data, ...patch },
      {
        onError: onError
          ? (err) => {
              // The global toast fires from usePerfConfig's own onError handler.
              // The optional callback lets the caller surface an inline error too.
              onError(err);
            }
          : undefined,
        onSettled: () => {
          // Remove these keys from the in-flight set once the PUT completes
          // (success or error). onSettled fires after onSuccess/onError.
          for (const k of keys) inFlightKeys.current.delete(k);
          forceUpdate();
        },
      },
    );
  }

  // Per-row saving predicate: true only for the row whose key is currently
  // in-flight. The ref is stable so [] deps is correct; forceUpdate re-renders
  // this component so every call reads the latest Set contents.
  const isSaving = useCallback(
    (key: string) => inFlightKeys.current.has(key),
    [],
  );

  if (config.isPending) {
    return <OptimizeTabSkeleton />;
  }

  if (config.isError || !config.data) {
    return (
      <PageError
        what="Could not load this site's optimization settings."
        why={config.error?.message ?? "Unknown error"}
        onRetry={() => void config.refetch()}
        retryLabel="Reload optimization"
      />
    );
  }

  const cfg = config.data;
  // Only gate on operator permission — per-key saving handles row-level
  // disabled state so one save no longer disables all other rows.
  const disabled = !canOperate;

  return (
    <div className="space-y-4">
      <CssJsSection config={cfg} save={save} disabled={disabled} isSaving={isSaving} />
      <FontsSection config={cfg} save={save} disabled={disabled} isSaving={isSaving} />
      <MediaHtmlSection
        config={cfg}
        save={save}
        disabled={disabled}
        isSaving={isSaving}
      />
      <CdnSection config={cfg} save={save} disabled={disabled} isSaving={isSaving} />
      <BloatSection config={cfg} save={save} disabled={disabled} isSaving={isSaving} />
      <DatabaseSection
        siteId={siteId}
        config={cfg}
        save={save}
        disabled={disabled}
        isSaving={isSaving}
        canOperate={canOperate}
      />
      <RucssResultsTable
        siteId={siteId}
        hostname={hostname}
        canOperate={canOperate}
      />
      <FontResultsTable
        siteId={siteId}
        hostname={hostname}
        canOperate={canOperate}
      />
      <RumSection
        siteId={siteId}
        config={cfg}
        save={save}
        disabled={disabled}
        isSaving={isSaving}
        canOperate={canOperate}
      />
      <RumResultsTable siteId={siteId} perSite />
    </div>
  );
}

function OptimizeTabSkeleton() {
  return (
    <div
      role="status"
      aria-busy="true"
      aria-label="Loading optimization settings"
      className="space-y-4"
    >
      <span className="sr-only">Loading optimization settings</span>
      {Array.from({ length: 4 }).map((_, i) => (
        <Skeleton key={i} className="h-48 w-full rounded-xl" />
      ))}
    </div>
  );
}
