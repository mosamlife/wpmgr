import { createFileRoute } from "@tanstack/react-router";
import { toast } from "sonner";
import type { AssistantRequest } from "@wpmgr/api";

import { Button } from "@/components/ui/button";
import { PageHeader } from "@/components/shared/page-header";
import { PageError } from "@/components/feedback/page-error";
import { Skeleton } from "@/components/ui/skeleton";
import { useMe } from "@/features/auth/use-auth";
import { AiAreaTabs } from "@/features/ai-requests/ai-area-tabs";
import { RequestCard } from "@/features/ai-requests/request-card";
import {
  useAssistantRequestPages,
  useApproveAssistantRequest,
  useDeclineAssistantRequest,
} from "@/features/ai-requests/use-ai-requests";

// /ai/requests — the AI request queue (tracka-cache-purge-design-v7 §2.6,
// slice W2). A tab beside /ai's Connections list, with a count badge fed by
// the same query (AiAreaTabs).
//
// ROUTE PLACEMENT. Under _authed/, like every other AI-area route: every row
// here names one of this tenant's own sites, so this page 403s on every call
// for a logged-out request rather than ever rendering for one.
//
// NO SEPARATE SIDEBAR ENTRY, same reasoning as /ai/connect next door -- this
// is reached from the tab bar on /ai (and from the Cache tab's banner), not
// from the primary nav.

export const Route = createFileRoute("/_authed/ai/requests")({
  component: AiRequestsPage,
});

function AiRequestsPage() {
  const query = useAssistantRequestPages();
  const approve = useApproveAssistantRequest();
  const decline = useDeclineAssistantRequest();
  const { data: me } = useMe();

  const loaded = query.data?.pages.flatMap((p) => p.requests) ?? [];
  // Pending first, then decided, each keeping the server's newest-first order.
  const requests = [
    ...loaded.filter((r) => r.state === "pending"),
    ...loaded.filter((r) => r.state !== "pending"),
  ];
  // pending_count is the server's whole-queue figure; when it exceeds what is
  // loaded, a pending request is still on a later page.
  const pendingCount = query.data?.pages[0]?.pending_count ?? 0;
  const pendingLoaded = loaded.filter((r) => r.state === "pending").length;
  // Decline is the default-focused control on the FIRST pending card only
  // (ADR-061 :554-557) -- autofocusing every card in the list would just
  // hand focus to whichever one renders last.
  const firstPendingId = requests.find((r) => r.state === "pending")?.id ?? null;

  function handleApprove(request: AssistantRequest) {
    if (request.presented_digest === undefined) {
      // Can only happen for a principal the digest rule already withholds it
      // from (an API key); the button is disabled for that case, so this is
      // a backstop, not a path this UI drives anyone into.
      toast.error("This request has no digest to approve with. Reload the page.");
      return;
    }
    approve.mutate(
      {
        siteId: request.site_id,
        requestId: request.id,
        presentedDigest: request.presented_digest,
      },
      {
        onSuccess: () => toast.success("Approved. WPMgr will send it to the site shortly."),
        onError: (err) => toast.error(err.message),
      },
    );
  }

  function handleDecline(request: AssistantRequest) {
    decline.mutate(
      { siteId: request.site_id, requestId: request.id },
      {
        onSuccess: () => toast.success("Declined."),
        onError: (err) => toast.error(err.message),
      },
    );
  }

  return (
    <div className="space-y-6">
      <PageHeader
        title="AI connections"
        subline="Each request is approved on its own. There is no approve-all."
      />
      <AiAreaTabs />

      {query.isPending ? (
        <RequestsSkeleton />
      ) : query.isError && loaded.length === 0 ? (
        <PageError
          what="Could not load AI requests."
          why={query.error.message}
          onRetry={() => void query.refetch()}
          retryLabel="Reload requests"
          isRetrying={query.isFetching}
        />
      ) : requests.length === 0 ? (
        <p data-testid="ai-requests-empty" className="text-sm text-muted-foreground">
          No AI requests are waiting for a decision.
        </p>
      ) : (
        <div className="space-y-4">
          <h2 className="text-sm font-semibold text-foreground">Requests waiting for you</h2>
          {requests.map((request) => (
            <RequestCard
              key={request.id}
              request={request}
              currentUserId={me?.user.id ?? null}
              onApprove={handleApprove}
              onDecline={handleDecline}
              approvePending={
                approve.isPending && approve.variables?.requestId === request.id
              }
              declinePending={
                decline.isPending && decline.variables?.requestId === request.id
              }
              autoFocusDecline={request.id === firstPendingId}
            />
          ))}
          {pendingCount > pendingLoaded && (
            <p role="status" data-testid="ai-requests-more-pending" className="text-sm text-muted-foreground">
              {pendingCount - pendingLoaded} more waiting on older pages. Show more to reach them.
            </p>
          )}
          {query.isFetchNextPageError && (
            <div role="alert" data-testid="ai-requests-next-page-error" className="text-sm text-destructive">
              Could not load more requests. {query.error?.message}
              <Button
                type="button"
                variant="outline"
                size="sm"
                className="ml-2"
                data-testid="ai-requests-next-page-retry"
                onClick={() => void query.fetchNextPage()}
              >
                Retry
              </Button>
            </div>
          )}
          {query.hasNextPage && (
            <Button
              type="button"
              variant="outline"
              data-testid="ai-requests-show-more"
              disabled={query.isFetchingNextPage}
              onClick={() => void query.fetchNextPage()}
            >
              {query.isFetchingNextPage ? "Loading..." : "Show more"}
            </Button>
          )}
        </div>
      )}
    </div>
  );
}

function RequestsSkeleton() {
  return (
    <div className="space-y-4" aria-hidden="true">
      {[0, 1].map((i) => (
        <div key={i} className="space-y-3 rounded-lg border border-border p-4">
          <Skeleton className="h-4 w-48" />
          <Skeleton className="h-3 w-32" />
          <Skeleton className="h-16 w-full" />
        </div>
      ))}
    </div>
  );
}
