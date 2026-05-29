import { createFileRoute } from "@tanstack/react-router";

import { ErrorsTable } from "@/features/errors/errors-table";

// ADR-037 Sprint 2 — `/sites/:siteId/errors` route. Thin wrapper around the
// ErrorsTable feature component. Real logic lives in apps/web/src/features/errors/.

export const Route = createFileRoute("/_authed/sites/$siteId/errors")({
  component: ErrorsTab,
});

function ErrorsTab() {
  const { siteId } = Route.useParams();
  return <ErrorsTable siteId={siteId} />;
}
