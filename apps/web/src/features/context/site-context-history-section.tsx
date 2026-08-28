import { useState } from "react";

import { toast } from "@/components/toast";
import type { GovContextVersionSummary } from "@wpmgr/api";

import { ContextVersionHistory, RestoreVersionDialog } from "./context-version-history";
import {
  useRestoreSiteContextVersion,
  useSiteContextVersionDiff,
  useSiteContextVersions,
} from "./use-context";

// ADR-064 S5 Stage B, Screen 4 — site-scope wrapper. Owns the hooks and the
// "which row is expanded" / "which version is pending restore confirmation"
// state; delegates rendering to the shared ContextVersionHistory.

export function SiteContextHistorySection({
  siteId,
  canWrite,
}: {
  siteId: string;
  canWrite: boolean;
}) {
  const {
    items,
    isPending,
    isError,
    error,
    hasNextPage,
    isFetchingNextPage,
    fetchNextPage,
    refetch,
  } = useSiteContextVersions(siteId);

  const [expandedId, setExpandedId] = useState<string | null>(null);
  const diff = useSiteContextVersionDiff(siteId, expandedId ?? "", {
    enabled: expandedId !== null,
  });

  const [confirmVersion, setConfirmVersion] = useState<GovContextVersionSummary | null>(null);
  const restore = useRestoreSiteContextVersion(siteId);

  async function handleConfirmRestore() {
    if (!confirmVersion) return;
    try {
      const result = await restore.mutateAsync(confirmVersion.id);
      toast.success(`Restored to version ${confirmVersion.version}`, {
        description: `Now version ${result.version}.`,
      });
      setConfirmVersion(null);
      restore.reset();
    } catch {
      // Surfaced inside RestoreVersionDialog via `restore.error` — stay open.
    }
  }

  return (
    <>
      <ContextVersionHistory
        scopeLabel="site"
        items={items}
        isPending={isPending}
        isError={isError}
        error={error}
        onRetry={() => void refetch()}
        hasNextPage={hasNextPage}
        isFetchingNextPage={isFetchingNextPage}
        onLoadMore={() => void fetchNextPage()}
        expandedId={expandedId}
        onToggleExpand={(id) => setExpandedId((cur) => (cur === id ? null : id))}
        diff={diff}
        canWrite={canWrite}
        onRequestRestore={(version) => {
          restore.reset();
          setConfirmVersion(version);
        }}
      />
      <RestoreVersionDialog
        open={confirmVersion !== null}
        onClose={() => {
          setConfirmVersion(null);
          restore.reset();
        }}
        version={confirmVersion}
        scopeLabel="site"
        onConfirm={() => void handleConfirmRestore()}
        isPending={restore.isPending}
        error={restore.error}
      />
    </>
  );
}
