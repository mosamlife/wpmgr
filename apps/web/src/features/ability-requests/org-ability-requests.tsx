import { Link } from "@tanstack/react-router";

import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { PageError } from "@/components/feedback/page-error";

import { AbilityRequestCard } from "./ability-request-card";
import { useAbilityCardActions } from "./use-ability-card-actions";
import { AbilityRequestError, useOrgAbilityRequestPages } from "./use-ability-requests";

// AI requests across every site, under /ai/requests beside the cache
// clear requests. Each card carries approve, decline and undo inline (the same
// card and handlers as a site's Content tab) and links to that site's Content
// tab. The badge count is the server's pending_count, never the page length.

export function OrgAbilityRequests() {
  const query = useOrgAbilityRequestPages();
  const actions = useAbilityCardActions();

  const loaded = query.data?.pages.flatMap((p) => p.requests) ?? [];
  const requests = [
    ...loaded.filter((r) => r.state === "pending"),
    ...loaded.filter((r) => r.state !== "pending"),
  ];
  const firstPendingId = requests.find((r) => r.state === "pending")?.id ?? null;
  const pendingCount = query.data?.pages[0]?.pending_count ?? 0;
  const pendingLoaded = loaded.filter((r) => r.state === "pending").length;

  // A principal without site.content.edit gets 403 here. That is not a failure
  // to report: the section is simply not theirs, so it renders nothing.
  if (query.isError && loaded.length === 0 && query.error instanceof AbilityRequestError && query.error.status === 403) {
    return null;
  }

  return (
    <section aria-label="AI requests" data-testid="org-ability-requests" className="space-y-4">
      <h2 className="text-sm font-semibold text-foreground">AI requests</h2>
      {query.isPending ? (
        <div aria-hidden="true">
          <Skeleton className="h-40 w-full" />
        </div>
      ) : query.isError && loaded.length === 0 ? (
        <PageError
          what="Could not load AI requests."
          why={query.error.message}
          onRetry={() => void query.refetch()}
          retryLabel="Reload page requests"
          isRetrying={query.isFetching}
        />
      ) : requests.length === 0 ? (
        <p data-testid="org-ability-requests-empty" className="text-sm text-muted-foreground">
          No AI requests yet.
        </p>
      ) : (
        <div className="space-y-4">
          {requests.map((r) => (
            <div key={r.id} className="space-y-1">
              <AbilityRequestCard
                request={r}
                notice={actions.notices[r.id] ?? null}
                onApprove={actions.handleApprove}
                onDecline={actions.handleDecline}
                onUndo={actions.handleUndo}
                approvePending={actions.approvePendingId === r.id}
                declinePending={actions.declinePendingId === r.id}
                undoPending={actions.undoPendingId === r.id}
                autoFocusDecline={r.id === firstPendingId}
              />
              <Link
                to="/sites/$siteId/content"
                params={{ siteId: r.site_id }}
                className="text-sm font-medium text-primary underline underline-offset-2 hover:opacity-80"
              >
                {`Open the Content tab for ${r.site_label}`}
              </Link>
            </div>
          ))}
          {pendingCount > pendingLoaded ? (
            <p role="status" className="text-sm text-muted-foreground">
              {pendingCount - pendingLoaded} more page requests are waiting on older pages. Show more to reach them.
            </p>
          ) : null}
          {query.hasNextPage ? (
            <Button
              type="button"
              variant="outline"
              data-testid="org-ability-requests-show-more"
              disabled={query.isFetchingNextPage}
              onClick={() => void query.fetchNextPage()}
            >
              {query.isFetchingNextPage ? "Loading..." : "Show more"}
            </Button>
          ) : null}
        </div>
      )}
    </section>
  );
}
