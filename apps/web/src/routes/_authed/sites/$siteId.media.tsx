import { createFileRoute } from "@tanstack/react-router";

import { MediaTab } from "@/features/media/MediaTab";
import { useSite } from "@/features/sites/use-sites";
import { useMe, canOperate, canManage } from "@/features/auth/use-auth";

// `/sites/$siteId/media` — Media Optimizer (ADR-043). Mirrors the Backups tab
// wiring (recon §5): a thin file-route that delegates to the feature component.
// optimize/restore/sync gate on operator+ (PermSiteWrite); delete-originals on
// admin+ (PermMediaDeleteOriginals) — the server is authoritative, we mirror
// the gate in the UI so viewers don't see write actions they can't perform.

export const Route = createFileRoute("/_authed/sites/$siteId/media")({
  component: MediaTabRoute,
});

function MediaTabRoute() {
  const { siteId } = Route.useParams();
  const { data: site } = useSite(siteId);
  const { data: me } = useMe();

  const hostname = hostnameOf(site?.url ?? "");

  return (
    <section aria-label="Media" className="px-4 pb-8 pt-6 sm:px-6">
      <MediaTab
        siteId={siteId}
        hostname={hostname}
        canOperate={canOperate(me)}
        canManage={canManage(me)}
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
