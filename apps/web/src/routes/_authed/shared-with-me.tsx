import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { ExternalLink, Loader2, Share2 } from "lucide-react";

import { Button } from "@/components/ui/button";
import { PageError } from "@/components/feedback";
import { PageHeader } from "@/components/shared/page-header";
import { StatusChip } from "@/components/status";
import { toast } from "@/components/toast";
import { relativeTime } from "@/lib/utils";
import { useMe } from "@/features/auth/use-auth";
import { useActivateOrg } from "@/features/orgs/use-orgs";
import { useSharedWithMe, type SharedSite } from "@/features/sharing/use-shared-with-me";

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
  const { data: me } = useMe();
  const navigate = useNavigate();
  const activateOrg = useActivateOrg();

  // Opening a shared site requires the session's active org to be the one that
  // OWNS the site — otherwise the by-id site read runs under the wrong tenant and
  // 404s. So switch org first (a site-scoped collaborator can now activate an org
  // they hold a share in), then navigate. If already in that org, just navigate.
  async function openSharedSite(item: SharedSite) {
    const target = item.org_id;
    if (target && me?.active_tenant_id !== target) {
      try {
        await activateOrg.mutateAsync(target);
      } catch {
        toast.error("Could not switch to the shared site's organisation.");
        return;
      }
    }
    void navigate({
      to: "/sites/$siteId/health",
      params: { siteId: item.site_id },
    });
  }

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
                  <Button
                    type="button"
                    variant="outline"
                    size="sm"
                    aria-label={`Open ${display}`}
                    disabled={activateOrg.isPending}
                    onClick={() => void openSharedSite(item)}
                  >
                    {activateOrg.isPending ? (
                      <Loader2 aria-hidden="true" className="size-3.5 animate-spin" />
                    ) : (
                      <ExternalLink aria-hidden="true" className="size-3.5" />
                    )}
                    Open site
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
