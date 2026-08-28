import { PageError } from "@/components/feedback";
import { Skeleton } from "@/components/ui/skeleton";
import { toast } from "@/components/toast";

import { GovContextEditor, type ContextFormValues } from "./gov-context-editor";
import { useOrgContext, usePatchOrgContext } from "./use-context";

// ADR-064 S5 Stage B, Screen 3 — the organisation-defaults editor (layer 2,
// Decision 1/6/13). Mirrors site-context-section.tsx exactly — same shared
// GovContextEditor, same three GET states, same refusal handling — because
// `GET/PATCH .../orgs/{orgId}/context` and `.../sites/{siteId}/context` are
// the identical contract at a different scope.

export function OrgContextSection({
  orgId,
  canWrite,
}: {
  orgId: string;
  canWrite: boolean;
}) {
  const { data, isPending, isError, error, refetch } = useOrgContext(orgId);
  const patch = usePatchOrgContext(orgId);

  if (isPending) {
    return <OrgContextSkeleton />;
  }

  if (isError) {
    return (
      <PageError
        what="Could not load this organisation's context."
        why={error instanceof Error ? error.message : "Unknown error."}
        onRetry={() => void refetch()}
        retryLabel="Reload"
      />
    );
  }

  const current = data;

  async function handleSave(values: ContextFormValues) {
    try {
      await patch.mutateAsync({
        base_version: current.version,
        restrictions: values.restrictions,
        guidance: values.guidance,
      });
      toast.success("Organisation context saved");
    } catch {
      // Surfaced to the operator via `patch.error` inside GovContextEditor.
    }
  }

  async function handleReloadLatest() {
    const result = await refetch();
    patch.reset();
    return result.data;
  }

  return (
    <GovContextEditor
      scopeLabel="organisation"
      current={current}
      onSave={handleSave}
      onReloadLatest={handleReloadLatest}
      saveError={patch.error}
      isSaving={patch.isPending}
      canWrite={canWrite}
    />
  );
}

function OrgContextSkeleton() {
  return (
    <div
      role="status"
      aria-busy="true"
      aria-label="Loading this organisation's context"
      className="space-y-3 rounded-lg border border-border p-4"
    >
      <Skeleton className="h-3 w-24" />
      <Skeleton className="h-8 w-full" />
      <Skeleton className="h-3 w-20" />
      <Skeleton className="h-16 w-full" />
    </div>
  );
}
