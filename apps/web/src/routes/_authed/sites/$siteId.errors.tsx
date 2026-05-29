import { createFileRoute } from "@tanstack/react-router";

import { ErrorsTable } from "@/features/errors/errors-table";

// ADR-037 Batch 4 — `/sites/:siteId/errors` route. Thin wrapper around the
// ErrorsTable feature component. All fetch/render logic lives in
// apps/web/src/features/errors/. The tab layout is owned by the parent site
// route; this file only mounts the feature and passes siteId.

export const Route = createFileRoute("/_authed/sites/$siteId/errors")({
  component: ErrorsTab,
});

function ErrorsTab() {
  const { siteId } = Route.useParams();
  return <ErrorsTable siteId={siteId} />;
}
