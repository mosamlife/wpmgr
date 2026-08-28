import { createFileRoute } from "@tanstack/react-router";

import { EffectiveContextPreview } from "@/features/context/effective-context-preview";

// ADR-064 S5 Stage A — `/sites/:siteId/context` route.
//
// Stage A ships ONLY the effective-context preview (Decision 8, Screen 1).
// The org/site context editors, version history, diff and restore screens
// (Decision 5/13) are Stage B and land in this same file as additional
// sections — see the handoff note in the S5 PR description before starting
// that work.

export const Route = createFileRoute("/_authed/sites/$siteId/context")({
  component: ContextTab,
});

function ContextTab() {
  const { siteId } = Route.useParams();
  return (
    <section aria-labelledby="context-heading" className="space-y-4 px-4 pb-8 pt-6 sm:px-6">
      <h2
        id="context-heading"
        className="text-xs font-medium uppercase tracking-wide text-muted-foreground"
      >
        Context
      </h2>
      <EffectiveContextPreview siteId={siteId} />
    </section>
  );
}
