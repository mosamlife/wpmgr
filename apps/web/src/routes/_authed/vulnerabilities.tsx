import { createFileRoute, Link } from "@tanstack/react-router";

import { PlannedFeature } from "@/components/feedback/planned-feature";

export const Route = createFileRoute("/_authed/vulnerabilities")({
  component: VulnerabilitiesPage,
});

function VulnerabilitiesPage() {
  return (
    <PlannedFeature
      title="Vulnerabilities"
      summary="An aggregated vulnerability and integrity overview is planned: surface plugin, theme, and WordPress core CVEs across all sites, prioritised by severity, with one-click remediation paths."
      availableToday={
        <Link
          to="/sites"
          className="underline underline-offset-4 hover:text-[var(--color-foreground)] transition-colors"
        >
          per-site integrity scan under a site's Security tab
        </Link>
      }
    />
  );
}
