import { createFileRoute } from "@tanstack/react-router";

import { EffectiveContextPreview } from "@/features/context/effective-context-preview";
import { SiteContextSection } from "@/features/context/site-context-section";
import { useMe, canManage } from "@/features/auth/use-auth";

// ADR-064 S5 — `/sites/:siteId/context` route.
//
// Stage A shipped the effective-context preview (Decision 8, Screen 1).
// Stage B adds the site override editor below it (layer 3, Screen 2).
// Version history, diff and restore (Decision 5) are not yet built.

export const Route = createFileRoute("/_authed/sites/$siteId/context")({
  component: ContextTab,
});

function ContextTab() {
  const { siteId } = Route.useParams();
  const { data: me } = useMe();
  // Decision 6: site-scope write is narrower than read, but this codebase
  // has no finer-grained "context.site.write" signal on the client today —
  // gate the editable form the same way every other site-configuration
  // write (hardening, login protection, destinations) already does, and let
  // the server's own capability check be the real enforcement point
  // regardless (a 403 from the PATCH still surfaces if this client-side
  // gate is ever wrong in either direction).
  const canWrite = canManage(me);

  return (
    <div className="space-y-8 px-4 pb-8 pt-6 sm:px-6">
      <section aria-labelledby="context-heading" className="space-y-4">
        <h2
          id="context-heading"
          className="text-xs font-medium uppercase tracking-wide text-muted-foreground"
        >
          Context
        </h2>
        <EffectiveContextPreview siteId={siteId} />
      </section>

      <section aria-labelledby="site-override-heading" className="space-y-4">
        <div className="space-y-0.5">
          <h2
            id="site-override-heading"
            className="text-xs font-medium uppercase tracking-wide text-muted-foreground"
          >
            Site override
          </h2>
          <p className="text-xs text-muted-foreground">
            Layer 3 — may narrow this organisation&apos;s defaults, never widen
            what it or WPMgr&apos;s own policy restricts.
          </p>
        </div>
        <SiteContextSection siteId={siteId} canWrite={canWrite} />
      </section>
    </div>
  );
}
