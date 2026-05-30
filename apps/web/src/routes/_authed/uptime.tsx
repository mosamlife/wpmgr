import { createFileRoute, Link } from "@tanstack/react-router";

import { PlannedFeature } from "@/components/feedback/planned-feature";

export const Route = createFileRoute("/_authed/uptime")({
  component: UptimePage,
});

function UptimePage() {
  return (
    <PlannedFeature
      title="Uptime"
      summary="A fleet-wide uptime and status overview is planned: see at a glance which sites are up, down, or degraded, with response-time trends and incident history across all connected sites."
      availableToday={
        <Link
          to="/sites"
          className="underline underline-offset-4 hover:text-[var(--color-foreground)] transition-colors"
        >
          per-site uptime on the site detail
        </Link>
      }
    />
  );
}
