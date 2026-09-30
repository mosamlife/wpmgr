import { Link } from "@tanstack/react-router";

import { useSiteAssistantRequests } from "./use-ai-requests";

// The Cache-tab banner (§2.6 "Site banner"): "N request(s) from an AI
// connection to clear this site's cache. [Review]". Read-only -- there is no
// approve button here, on purpose; approving happens on the card, which
// carries the digest this banner never fetches more of than it needs to
// know a number.
//
// SILENT ON EVERY STATE BUT "SOMETHING IS WAITING". No skeleton, no error
// banner: a site with nothing waiting, a site whose count failed to load,
// and a page still loading all render as nothing, because the one thing
// this component exists to say is "come look", and it has nothing false to
// say in the other three cases. The Cache tab's own page-level loading and
// error states already cover the request failing loudly elsewhere.

export interface AiRequestSiteBannerProps {
  siteId: string;
  /** PermSiteCachePurge — pass the same gate the Cache tab already computes
   *  for purge/preload (canOperate) rather than re-deriving it here. */
  canView: boolean;
}

export function AiRequestSiteBanner({ siteId, canView }: AiRequestSiteBannerProps) {
  const query = useSiteAssistantRequests(canView ? siteId : undefined);

  if (!canView || !query.isSuccess) return null;
  const count = query.data.pending_count;
  if (count <= 0) return null;

  return (
    <div
      role="status"
      className="flex flex-wrap items-center justify-between gap-2 rounded-md border border-border bg-muted/40 px-3 py-2 text-sm"
    >
      <p className="text-foreground">
        {count} {count === 1 ? "request" : "requests"} from an AI connection to clear this
        site&rsquo;s cache.
      </p>
      <Link
        to="/ai/requests"
        className="text-sm font-medium text-primary underline underline-offset-2 hover:opacity-80"
      >
        Review
      </Link>
    </div>
  );
}
