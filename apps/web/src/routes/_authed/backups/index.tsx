import { createFileRoute, Link } from "@tanstack/react-router";

import { PlannedFeature } from "@/components/feedback/planned-feature";

export const Route = createFileRoute("/_authed/backups/")({
  component: BackupsIndexPage,
});

function BackupsIndexPage() {
  return (
    <PlannedFeature
      title="Backups"
      summary="A fleet-wide backup view is planned: browse, filter, and manage snapshots across all connected sites from one place. Download or restore any snapshot without navigating site by site."
      availableToday={
        <Link
          to="/sites"
          className="underline underline-offset-4 hover:text-[var(--color-foreground)] transition-colors"
        >
          per-site backups on each site's Backups tab
        </Link>
      }
    />
  );
}
