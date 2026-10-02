
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { PageError } from "@/components/feedback/page-error";

import { AbilityRequestCard } from "./ability-request-card";
import { useAbilityCardActions } from "./use-ability-card-actions";
import {
  CODE_AGENT_OUTDATED,
  AbilityRequestError,
  useAbilityRequestPages,
  useContentEditing,
  useEnableContentEditing,
} from "./use-ability-requests";

// The "AI editing" part of a site's Content tab: the per-site switch, then the
// site's AI page-creation requests. The API lists requests per site only, so
// this lives on the site and not on the organisation-wide /ai/requests page.

export const EDITING_OFF_COPY = "AI page creation is off for this site.";
export const AGENT_OUTDATED_COPY = "Update the WPMgr plugin to 0.61.156 or later, then turn this on.";
export const UNREACHABLE_COPY = "WPMgr could not reach this site, so nothing was turned on. Try again.";

/** Plain-words copy for a failed enable, keyed on the server's own codes. */
export function enableErrorCopy(err: AbilityRequestError): string {
  if (err.code === CODE_AGENT_OUTDATED) return AGENT_OUTDATED_COPY;
  if (err.status === 503) return UNREACHABLE_COPY;
  if (err.status === 403) return "You do not have permission to turn this on.";
  return err.message;
}

export interface AiEditingSectionProps {
  siteId: string;
  siteUrl?: string | null;
  /** operator+ on this site (site.content.edit): may turn on, approve and undo. */
  canOperate: boolean;
}

export function AiEditingSection({ siteId, siteUrl, canOperate }: AiEditingSectionProps) {
  return (
    <section aria-label="AI page creation" className="space-y-4">
      <AiEditingSwitch siteId={siteId} canOperate={canOperate} />
      {canOperate ? <AbilityRequestList siteId={siteId} siteUrl={siteUrl} /> : null}
    </section>
  );
}

function AiEditingSwitch({ siteId, canOperate }: { siteId: string; canOperate: boolean }) {
  const state = useContentEditing(siteId);
  const enable = useEnableContentEditing(siteId);

  if (state.isPending) {
    return <Skeleton aria-label="Loading AI editing" className="h-14 w-full" />;
  }
  if (state.isError) {
    return (
      <PageError
        what="Could not load the AI editing setting."
        why={state.error.message}
        onRetry={() => void state.refetch()}
        isRetrying={state.isFetching}
      />
    );
  }

  const on = state.data.enabled;
  return (
    <div className="space-y-2 rounded-lg border border-border bg-card p-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="space-y-0.5">
          <h2 className="text-sm font-semibold text-foreground">AI editing</h2>
          <p data-testid="ai-editing-state" className="text-sm text-muted-foreground">
            {on
              ? "AI page creation is on. Each draft the AI asks for still needs your approval."
              : EDITING_OFF_COPY}
          </p>
        </div>
        {!on && canOperate ? (
          <Button type="button" disabled={enable.isPending} onClick={() => enable.mutate()}>
            {enable.isPending ? "Turning on…" : "Turn on"}
          </Button>
        ) : null}
      </div>
      {!on && !canOperate ? (
        <p className="text-xs text-muted-foreground">An operator can turn this on.</p>
      ) : null}
      {enable.isError ? (
        <p role="alert" className="text-sm text-destructive">
          {enableErrorCopy(enable.error)}
        </p>
      ) : null}
    </div>
  );
}

function AbilityRequestList({ siteId, siteUrl }: { siteId: string; siteUrl?: string | null }) {
  const query = useAbilityRequestPages(siteId, true);
  const actions = useAbilityCardActions();

  const loaded = query.data?.pages.flatMap((p) => p.requests) ?? [];
  // Pending first so a waiting decision is never below the fold; each group
  // keeps the server's newest-first order.
  const requests = [
    ...loaded.filter((r) => r.state === "pending"),
    ...loaded.filter((r) => r.state !== "pending"),
  ];
  const firstPendingId = requests.find((r) => r.state === "pending")?.id ?? null;

  return (
    <div className="space-y-3" data-testid="ability-requests">
      <h2 className="text-sm font-semibold text-foreground">AI page requests</h2>
      {query.isPending ? (
        <div aria-hidden="true" className="space-y-3">
          <Skeleton className="h-40 w-full" />
        </div>
      ) : query.isError && loaded.length === 0 ? (
        <PageError
          what="Could not load AI page requests."
          why={query.error.message}
          onRetry={() => void query.refetch()}
          retryLabel="Reload requests"
          isRetrying={query.isFetching}
        />
      ) : requests.length === 0 ? (
        <p data-testid="ability-requests-empty" className="text-sm text-muted-foreground">
          No AI page requests for this site yet.
        </p>
      ) : (
        <div className="space-y-4">
          {query.isRefetchError && !query.isFetchNextPageError ? (
            <div role="alert" className="text-sm text-destructive">
              Couldn&apos;t refresh the requests. What you see may be out of date.
              <Button
                type="button"
                variant="outline"
                size="sm"
                className="ml-2"
                disabled={query.isFetching}
                onClick={() => void query.refetch()}
              >
                Retry
              </Button>
            </div>
          ) : null}
          {requests.map((r) => (
            <AbilityRequestCard
              key={r.id}
              request={r}
              siteUrl={siteUrl}
              notice={actions.notices[r.id] ?? null}
              onApprove={actions.handleApprove}
              onDecline={actions.handleDecline}
              onUndo={actions.handleUndo}
              approvePending={actions.approvePendingId === r.id}
              declinePending={actions.declinePendingId === r.id}
              undoPending={actions.undoPendingId === r.id}
              autoFocusDecline={r.id === firstPendingId}
            />
          ))}
          {query.isFetchNextPageError ? (
            <div role="alert" className="text-sm text-destructive">
              Could not load more requests.
              <Button
                type="button"
                variant="outline"
                size="sm"
                className="ml-2"
                onClick={() => void query.fetchNextPage()}
              >
                Retry
              </Button>
            </div>
          ) : null}
          {query.hasNextPage ? (
            <>
              <p role="status" className="text-sm text-muted-foreground">
                Older requests are not shown yet. A request still waiting for you may be among them.
              </p>
              <Button
                type="button"
                variant="outline"
                data-testid="ability-requests-show-more"
                disabled={query.isFetchingNextPage}
                onClick={() => void query.fetchNextPage()}
              >
                {query.isFetchingNextPage ? "Loading..." : "Show more"}
              </Button>
            </>
          ) : null}
        </div>
      )}
    </div>
  );
}
