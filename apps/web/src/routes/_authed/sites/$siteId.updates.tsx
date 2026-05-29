import { createFileRoute } from "@tanstack/react-router";

import { AvailableUpdatesCard } from "@/features/updates/available-updates-card";

// `/sites/$siteId/updates` — available updates table for this site.

export const Route = createFileRoute("/_authed/sites/$siteId/updates")({
  component: UpdatesTab,
});

function UpdatesTab() {
  const { siteId } = Route.useParams();

  return (
    <section
      aria-labelledby="updates-heading"
      className="px-6 pt-6 pb-8"
    >
      <h2
        id="updates-heading"
        className="mb-3 text-xs font-medium uppercase tracking-wide text-muted-foreground"
      >
        Updates
      </h2>
      <AvailableUpdatesCard siteId={siteId} />
    </section>
  );
}
