import { createFileRoute } from "@tanstack/react-router";

import { Button } from "@/components/ui/button";

// `/sites/$siteId/security` — vulnerability scan stub. Sprint 4 will wire to
// the scan endpoint; the calm empty state + Run-scan affordance is the
// operator-correct default until then (DESIGN.md: state is the design).

export const Route = createFileRoute("/_authed/sites/$siteId/security")({
  component: SecurityTab,
});

function SecurityTab() {
  return (
    <section
      aria-labelledby="security-heading"
      className="px-6 pt-6 pb-8"
    >
      <h2
        id="security-heading"
        className="mb-3 text-xs font-medium uppercase tracking-wide text-muted-foreground"
      >
        Security
      </h2>
      <div className="rounded-lg border border-border bg-card p-6">
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div className="space-y-1">
            <p className="text-sm text-muted-foreground">
              No vulnerabilities found in the last scan.
            </p>
            <p className="text-xs text-muted-foreground">
              Scan results appear here after the agent completes a sweep.
            </p>
          </div>
          <Button size="sm" variant="outline" disabled title="Coming soon">
            Run scan
          </Button>
        </div>
      </div>
    </section>
  );
}
