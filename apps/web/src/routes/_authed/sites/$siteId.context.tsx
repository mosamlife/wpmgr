import { createFileRoute } from "@tanstack/react-router";

import { EffectiveContextPreview } from "@/features/context/effective-context-preview";
import { SiteContextSection } from "@/features/context/site-context-section";
import { SiteContextHistorySection } from "@/features/context/site-context-history-section";
import { useMe, canOperate } from "@/features/auth/use-auth";

// ADR-064 S5 — `/sites/:siteId/context` route.
//
// Stage A shipped the effective-context preview (Decision 8, Screen 1).
// Stage B adds the site override editor (layer 3, Screen 2) and version
// history / diff / restore (Decision 5, Screen 4) below it.

export const Route = createFileRoute("/_authed/sites/$siteId/context")({
  component: ContextTab,
});

function ContextTab() {
  const { siteId } = Route.useParams();
  const { data: me } = useMe();
  // Decision 6: "site-scope write additionally requires access to that
  // specific site" — narrower than read, but NOT narrower than
  // organisation-scope write (which Decision 6 states explicitly IS
  // admin-only). `canManage` (owner/admin only) was too strict here and hid
  // the editor and history from an operator-level org member or an
  // operator-role site collaborator, both of whom `context.site.write`
  // actually permits — caught in review (Greptile P1, "Operator writes are
  // hidden"). `canOperate` matches this codebase's own existing definition
  // of "site access at operator tier or above." The server's own capability
  // check remains the real enforcement point regardless (a 403 from the
  // PATCH still surfaces if this client-side gate is ever wrong).
  const canWrite = canOperate(me);

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

      <section aria-labelledby="site-history-heading" className="space-y-4">
        <div className="space-y-0.5">
          <h2
            id="site-history-heading"
            className="text-xs font-medium uppercase tracking-wide text-muted-foreground"
          >
            Version history
          </h2>
          <p className="text-xs text-muted-foreground">
            Every accepted write, newest first. A diff compares what was
            authored in two versions, not what either enforced at the time.
          </p>
        </div>
        <SiteContextHistorySection siteId={siteId} canWrite={canWrite} />
      </section>
    </div>
  );
}
