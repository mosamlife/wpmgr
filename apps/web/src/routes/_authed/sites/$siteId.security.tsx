import { createFileRoute } from "@tanstack/react-router";
import { ShieldOff } from "lucide-react";

import { Button } from "@/components/ui/button";

// `/sites/$siteId/security` — vulnerability scan stub.
//
// No scan backend exists yet. This surface renders an honest empty state
// rather than fabricated "0 findings". When the scan endpoint lands:
//   - swap this empty state for an ErrorsTable-style table pattern
//   - use VulnSeverityChip from '@/components/status/vuln-severity-chip'
//     for severity cells (critical/high/medium/low with bg-severity-* tokens)
//   - keep the DefinitionList/KvRow pattern for the detail drawer

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
        className="mb-4 text-xs font-medium uppercase tracking-wide text-muted-foreground"
      >
        Vulnerabilities
      </h2>

      {/* Scan-backend-pending: no card wrapper per DESIGN rule "never nest cards" */}
      <div
        role="status"
        aria-label="No scan results yet"
        className="flex flex-col items-center gap-3 py-12 text-center"
      >
        <ShieldOff
          aria-hidden="true"
          strokeWidth={1.5}
          className="size-8 text-[var(--color-muted-foreground)]/50"
        />
        <div className="space-y-1">
          <p className="text-balance text-sm font-medium text-[var(--color-foreground)]">
            Not scanned yet.
          </p>
          <p className="text-balance text-sm text-[var(--color-muted-foreground)]">
            Run a scan to check plugins, themes, and WordPress core against the
            WPScan database.
          </p>
        </div>
        <Button
          type="button"
          variant="outline"
          size="sm"
          disabled
          aria-disabled="true"
          title="Scan endpoint coming in a future release."
        >
          Run scan
        </Button>
      </div>
    </section>
  );
}
