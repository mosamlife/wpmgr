import { createFileRoute, Link } from "@tanstack/react-router";

import { PlannedFeature } from "@/components/feedback/planned-feature";

export const Route = createFileRoute("/_authed/audit")({
  component: AuditPage,
});

function AuditPage() {
  return (
    <PlannedFeature
      title="Audit log"
      summary="A fleet-wide audit and activity stream is planned: every backup, restore, update, login event, and settings change across all sites in a searchable, filterable timeline."
      availableToday={
        <Link
          to="/sites"
          className="underline underline-offset-4 hover:text-[var(--color-foreground)] transition-colors"
        >
          per-site activity on each site's Activity tab
        </Link>
      }
    />
  );
}
