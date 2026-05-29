import { createFileRoute } from "@tanstack/react-router";

import { ActivityTable } from "@/features/activity/activity-table";

// ADR-037 Sprint 3 — `/sites/:siteId/activity` route. Thin wrapper around the
// ActivityTable feature component (the tamper-evident WordPress activity log).
// Real logic lives in apps/web/src/features/activity/.

export const Route = createFileRoute("/_authed/sites/$siteId/activity")({
  component: ActivityTab,
});

function ActivityTab() {
  const { siteId } = Route.useParams();
  return <ActivityTable siteId={siteId} />;
}
