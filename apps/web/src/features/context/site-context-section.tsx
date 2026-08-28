import { PageError } from "@/components/feedback";
import { Skeleton } from "@/components/ui/skeleton";
import { toast } from "@/components/toast";

import { GovContextEditor, type ContextFormValues } from "./gov-context-editor";
import { useSiteContext, usePatchSiteContext } from "./use-context";

// ADR-064 S5 Stage B, Screen 2 — the site override editor (layer 3, Decision
// 1/6/13). Thin wrapper: owns the GET/PATCH hooks and the three loading /
// error / data states for the GET; delegates the form itself (and every
// write-path refusal) to the shared `GovContextEditor`.

export function SiteContextSection({
  siteId,
  canWrite,
}: {
  siteId: string;
  canWrite: boolean;
}) {
  const { data, isPending, isError, error, refetch } = useSiteContext(siteId);
  const patch = usePatchSiteContext(siteId);

  if (isPending) {
    return <SiteContextSkeleton />;
  }

  if (isError) {
    return (
      <PageError
        what="Could not load this site's context."
        why={error instanceof Error ? error.message : "Unknown error."}
        onRetry={() => void refetch()}
        retryLabel="Reload"
      />
    );
  }

  // `data` is guaranteed defined past the isPending/isError checks above,
  // but that narrowing doesn't survive into the nested closures below (each
  // is its own function scope TS re-widens) — bind it to a local const here
  // instead of asserting `data!` at every use site.
  const current = data;

  async function handleSave(values: ContextFormValues) {
    try {
      await patch.mutateAsync({
        base_version: current.version,
        restrictions: values.restrictions,
        guidance: values.guidance,
      });
      toast.success("Site context saved");
    } catch {
      // Surfaced to the operator via `patch.error` inside GovContextEditor —
      // nothing further to do here.
    }
  }

  async function handleReloadLatest() {
    const result = await refetch();
    patch.reset();
    return result.data;
  }

  return (
    <GovContextEditor
      scopeLabel="site"
      current={current}
      onSave={handleSave}
      onReloadLatest={handleReloadLatest}
      saveError={patch.error}
      isSaving={patch.isPending}
      canWrite={canWrite}
    />
  );
}

function SiteContextSkeleton() {
  return (
    <div
      role="status"
      aria-busy="true"
      aria-label="Loading this site's context"
      className="space-y-3 rounded-lg border border-border p-4"
    >
      <Skeleton className="h-3 w-24" />
      <Skeleton className="h-8 w-full" />
      <Skeleton className="h-3 w-20" />
      <Skeleton className="h-16 w-full" />
    </div>
  );
}
