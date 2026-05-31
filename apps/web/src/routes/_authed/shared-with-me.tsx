import { createFileRoute, Link } from "@tanstack/react-router";
import { ExternalLink, Share2 } from "lucide-react";

import { Button } from "@/components/ui/button";
import { PageError } from "@/components/feedback";
import { PageHeader } from "@/components/shared/page-header";
import { StatusChip } from "@/components/status";
import { relativeTime } from "@/lib/utils";
import { useSharedWithMe } from "@/features/sharing/use-shared-with-me";

export const Route = createFileRoute("/_authed/shared-with-me")({
  component: SharedWithMePage,
});

// "Shared with me" — lists sites that have been shared TO the current user
// across all orgs, with their per-site role and a link to the site detail.
// A site-scoped collaborator lands here as their primary view; org members
// can also see it to find sites shared from other orgs.

function SharedWithMePage() {
  const { data: shared, isPending, isError, error, refetch, isRefetching } =
    useSharedWithMe();

  return (
    <section aria-labelledby="shared-heading" className="max-w-3xl space-y-6">
      <PageHeader
        title="Shared with me"
        subline="Sites that other organisations have shared directly with your account."
      />

      {isPending ? (
        <p role="status" className="text-sm text-muted-foreground">
          Loading…
        </p>
      ) : isError ? (
        <PageError
          what="Could not load shared sites."
          why={error.message}
          onRetry={() => void refetch()}
          retryLabel="Reload"
          isRetrying={isRefetching}
        />
      ) : !shared || shared.length === 0 ? (
        <div
          role="status"
          className="flex flex-col items-center gap-3 rounded-xl border border-dashed border-[var(--color-border)] py-16 text-center"
        >
          <Share2
            aria-hidden="true"
            strokeWidth={1.5}
            className="size-8 text-muted-foreground/50"
          />
          <div className="space-y-1">
            <p className="text-sm font-medium">No shared sites</p>
            <p className="text-sm text-muted-foreground">
              When someone shares a site with you, it will appear here.
            </p>
          </div>
        </div>
      ) : (
        <div className="divide-y divide-[var(--color-border)] rounded-xl border border-[var(--color-border)]">
          {shared.map((item) => {
            const display = item.site_name ?? item.site_id;
            const isExpired =
              item.expires_at != null &&
              new Date(item.expires_at) < new Date();
            return (
              <div
                key={item.id}
                className="flex items-center justify-between gap-4 px-4 py-3"
              >
                <div className="min-w-0 flex-1 space-y-0.5">
                  <div className="flex min-w-0 flex-wrap items-center gap-2">
                    <span className="truncate text-sm font-medium text-foreground">
                      {display}
                    </span>
                    <span className="shrink-0 rounded-full border border-[var(--color-border)] px-2 py-0.5 text-xs capitalize text-muted-foreground">
                      {item.role}
                    </span>
                    {isExpired ? (
                      <StatusChip tone="destructive" label="Expired" />
                    ) : null}
                  </div>
                  <p className="text-xs text-muted-foreground">
                    {item.org_name ? (
                      <span>From {item.org_name}</span>
                    ) : null}
                    {item.expires_at && !isExpired ? (
                      <span className="ml-2 opacity-70">
                        Expires {relativeTime(item.expires_at)}
                      </span>
                    ) : null}
                    {item.site_url ? (
                      <span className="ml-2 font-mono opacity-70">
                        {item.site_url}
                      </span>
                    ) : null}
                  </p>
                </div>
                {!isExpired ? (
                  <Button asChild variant="outline" size="sm">
                    <Link
                      to="/sites/$siteId/health"
                      params={{ siteId: item.site_id }}
                      aria-label={`Open ${display}`}
                    >
                      <ExternalLink aria-hidden="true" className="size-3.5" />
                      Open site
                    </Link>
                  </Button>
                ) : null}
              </div>
            );
          })}
        </div>
      )}
    </section>
  );
}
