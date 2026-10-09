import { createFileRoute } from "@tanstack/react-router";

import { AiEditingSection } from "@/features/ability-requests/ai-editing-section";
import { ContentTab } from "@/features/content/ContentTab";
import { useSite } from "@/features/sites/use-sites";
import { useMe, canWriteSiteContext } from "@/features/auth/use-auth";

// `/sites/$siteId/content` (Track B slice S1): read-only page inventory. The
// refresh button mirrors the server's site.content.refresh gate (operator+, or a site-scoped operator share for this site).

export const Route = createFileRoute("/_authed/sites/$siteId/content")({
  component: ContentTabRoute,
});

function ContentTabRoute() {
  const { siteId } = Route.useParams();
  const { data: site } = useSite(siteId);
  const { data: me } = useMe();
  return (
    <section aria-label="Content" className="space-y-6 px-4 pb-8 pt-6 sm:px-6">
      <AiEditingSection
        siteId={siteId}
        siteUrl={site?.url ?? null}
        canOperate={canWriteSiteContext(me)}
      />
      <ContentTab
        siteId={siteId}
        hostname={hostnameOf(site?.url ?? "")}
        canOperate={canWriteSiteContext(me)}
      />
    </section>
  );
}

function hostnameOf(url: string): string {
  try {
    return new URL(url).host;
  } catch {
    return url;
  }
}
