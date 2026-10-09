import { useState } from "react";
import type { AiActivityItem } from "@wpmgr/api";

import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { PageError } from "@/components/feedback/page-error";
import { AbilityRequestCard } from "@/features/ability-requests/ability-request-card";
import { RanAutomaticallyChip } from "@/features/ability-requests/auto-approval";
import { abilityCardTitle, abilityStatus, clockTime } from "@/features/ability-requests/ability-card-model";
import { isRestWrite, parseRestCardFacts, restCardTitle, restWriteStatus } from "@/features/ability-requests/rest-card-model";
import { useAbilityCardActions } from "@/features/ability-requests/use-ability-card-actions";
import { RequestCard } from "@/features/ai-requests/request-card";
import { cardTitle, requestStatusLine } from "@/features/ai-requests/request-card-model";

import { ACTIVITY_EMPTY, ACTIVITY_FILTERED_EMPTY, ranAutomatically, shortDate } from "./ai-trust-copy";
import { useAiActivityPages, type ActivityFilters } from "./use-ai-trust";

// The AI activity feed (design §8.7): every request an AI connection made that
// a person or a site's setting approved, newest first, from both request
// tables. Each row is WPMgr's own text (the kind of change and the site's
// label); "Details" opens the same card the queues show, with its Undo.

/** A row's title, built from WPMgr's own names for the change and the site's label. */
function itemTitle(item: AiActivityItem): string {
  if (item.kind === "cache_purge_request") return cardTitle(item.request);
  const r = item.request;
  return isRestWrite(r) ? restCardTitle(parseRestCardFacts(r), r.site_label) : abilityCardTitle(r);
}

function itemStatus(item: AiActivityItem, currentUserId: string | null): string {
  if (item.kind === "cache_purge_request") return requestStatusLine(item.request, currentUserId).text;
  const r = item.request;
  return isRestWrite(r) ? restWriteStatus(r).text : abilityStatus(r).text;
}

function itemTime(item: AiActivityItem): string {
  const at = item.request.decided_at ?? item.request.created_at;
  return `${shortDate(at)}, ${clockTime(at)}`;
}

function noop() {}

export function AiActivityList({
  filters,
  currentUserId,
  refetchInterval,
}: {
  filters: ActivityFilters;
  currentUserId: string | null;
  /** 15 s on a site, 30 s organisation-wide. */
  refetchInterval: number;
}) {
  const query = useAiActivityPages(filters, refetchInterval);
  const actions = useAbilityCardActions();
  const [open, setOpen] = useState<string | null>(null);

  const items = query.data?.pages.flatMap((p) => p.items) ?? [];
  const filtered = filters.filter !== "all" || filters.siteId !== undefined || filters.grantId !== undefined;

  // A principal without site.content.edit is refused the feed. The list is
  // simply not theirs, so it renders nothing rather than a failure.
  if (query.isError && items.length === 0 && query.error.status === 403) return null;

  if (query.isPending) {
    return (
      <div aria-label="Loading AI activity" className="space-y-3">
        <Skeleton className="h-16 w-full" />
        <Skeleton className="h-16 w-full" />
      </div>
    );
  }
  if (query.isError && items.length === 0) {
    return (
      <PageError
        what="Could not load AI activity."
        why={query.error.message}
        onRetry={() => void query.refetch()}
        retryLabel="Reload activity"
        isRetrying={query.isFetching}
      />
    );
  }
  if (items.length === 0) {
    return (
      <p data-testid="ai-activity-empty" className="text-sm text-muted-foreground">
        {filtered ? ACTIVITY_FILTERED_EMPTY : ACTIVITY_EMPTY}
      </p>
    );
  }

  return (
    <div className="space-y-3" data-testid="ai-activity">
      {query.isRefetchError && !query.isFetchNextPageError ? (
        <div role="alert" className="text-sm text-destructive">
          Couldn&apos;t refresh the activity. What you see may be out of date.
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
      <ul className="space-y-2">
        {items.map((item) => {
          const key = `${item.kind}:${item.request.id}`;
          const title = itemTitle(item);
          const expanded = open === key;
          const auto = item.kind === "ability_request" && ranAutomatically(item.request.approval);
          return (
            <li key={key} className="space-y-2 rounded-lg border border-border bg-card p-3">
              <div className="flex flex-wrap items-start justify-between gap-2">
                <div className="min-w-0 space-y-0.5">
                  <p className="flex flex-wrap items-center gap-2 text-sm font-medium text-foreground">
                    <span className="min-w-0 break-words">{title}</span>
                    {auto ? <RanAutomaticallyChip /> : null}
                  </p>
                  <p className="text-xs text-muted-foreground">
                    {itemTime(item)} · {item.request.grant_label}
                  </p>
                  <p className="text-sm text-foreground">{itemStatus(item, currentUserId)}</p>
                </div>
                <Button
                  type="button"
                  variant="ghost"
                  size="sm"
                  aria-expanded={expanded}
                  aria-label={`${expanded ? "Hide details" : "Details"}: ${title}`}
                  onClick={() => setOpen(expanded ? null : key)}
                >
                  {expanded ? "Hide details" : "Details"}
                </Button>
              </div>
              {expanded ? (
                item.kind === "ability_request" ? (
                  <AbilityRequestCard
                    request={item.request}
                    currentUserId={currentUserId}
                    notice={actions.notices[item.request.id] ?? null}
                    onApprove={actions.handleApprove}
                    onDecline={actions.handleDecline}
                    onUndo={actions.handleUndo}
                    undoPending={actions.undoPendingId === item.request.id}
                  />
                ) : (
                  <RequestCard
                    request={item.request}
                    currentUserId={currentUserId}
                    onApprove={noop}
                    onDecline={noop}
                  />
                )
              ) : null}
            </li>
          );
        })}
      </ul>
      {query.isFetchNextPageError ? (
        <div role="alert" className="text-sm text-destructive">
          Could not load more activity.
          <Button type="button" variant="outline" size="sm" className="ml-2" onClick={() => void query.fetchNextPage()}>
            Retry
          </Button>
        </div>
      ) : null}
      {query.hasNextPage ? (
        <Button
          type="button"
          variant="outline"
          data-testid="ai-activity-show-more"
          disabled={query.isFetchingNextPage}
          onClick={() => void query.fetchNextPage()}
        >
          {query.isFetchingNextPage ? "Loading..." : "Show more"}
        </Button>
      ) : null}
    </div>
  );
}
