import { createFileRoute } from "@tanstack/react-router";

import { AvailableUpdatesCard } from "@/features/updates/available-updates-card";

// `/sites/$siteId/updates` — available updates panel for this site.
// Thin wrapper around the AvailableUpdatesCard feature component.
// Real logic lives in apps/web/src/features/updates/.

export const Route = createFileRoute("/_authed/sites/$siteId/updates")({
  component: UpdatesTab,
});

function UpdatesTab() {
  const { siteId } = Route.useParams();

  return (
    <section aria-label="Available updates" className="px-4 pb-8 pt-6 sm:px-6">
      <AvailableUpdatesCard siteId={siteId} />
    </section>
  );
}
