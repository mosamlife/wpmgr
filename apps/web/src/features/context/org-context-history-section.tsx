import { useState } from "react";

import { toast } from "@/components/toast";
import type { GovContextVersionSummary } from "@wpmgr/api";

import { ContextVersionHistory, RestoreVersionDialog } from "./context-version-history";
import {
  useOrgContextVersionDiff,
  useOrgContextVersions,
  useRestoreOrgContextVersion,
} from "./use-context";

// ADR-064 S5 Stage B, Screen 4 — organisation-scope sibling of
// site-context-history-section.tsx. Same shared component, same shape,
// different hook pair.

export function OrgContextHistorySection({
  orgId,
  canWrite,
}: {
  orgId: string;
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
  } = useOrgContextVersions(orgId);

  const [expandedId, setExpandedId] = useState<string | null>(null);
  const diff = useOrgContextVersionDiff(orgId, expandedId ?? "", {
    enabled: expandedId !== null,
  });

  const [confirmVersion, setConfirmVersion] = useState<GovContextVersionSummary | null>(null);
  const restore = useRestoreOrgContextVersion(orgId);

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
        scopeLabel="organisation"
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
        isRestoring={restore.isPending}
      />
      <RestoreVersionDialog
        open={confirmVersion !== null}
        onClose={() => {
          setConfirmVersion(null);
          restore.reset();
        }}
        version={confirmVersion}
        scopeLabel="organisation"
        onConfirm={() => void handleConfirmRestore()}
        isPending={restore.isPending}
        error={restore.error}
      />
    </>
  );
}
