import { createFileRoute } from "@tanstack/react-router";

import { PlannedFeature } from "@/components/feedback/planned-feature";

export const Route = createFileRoute("/_authed/migrations")({
  component: MigrationsPage,
});

function MigrationsPage() {
  return (
    <PlannedFeature
      title="Migrations"
      summary="Site migrations are planned and not yet available. The feature will let you clone or move WordPress sites between hosts, with automatic URL rewriting and DNS cut-over coordination."
    />
  );
}
