import { createFileRoute } from "@tanstack/react-router";
import { ShieldOff } from "lucide-react";

import { Button } from "@/components/ui/button";

import { LoginProtectionPanel } from "@/features/security/login-protection-panel";
import { LoginEventsTable } from "@/features/security/login-events-table";

// `/sites/$siteId/security` — two sections:
//
//   1. Login Protection (S2) — config panel + recent events table. This is the
//      focus of this sprint. See features/security/ for data hooks and components.
//
//   2. Vulnerabilities — scan stub (honest empty state, no fabricated data).
//      Retained for future-sprint implementation. When the scan endpoint lands:
//        - swap the empty state for an ErrorsTable-style table pattern
//        - use VulnSeverityChip for severity cells
//        - keep DefinitionList/KvRow for the detail drawer

export const Route = createFileRoute("/_authed/sites/$siteId/security")({
  component: SecurityTab,
});

function SecurityTab() {
  const { siteId } = Route.useParams();

  return (
    <div className="divide-y divide-[var(--color-border)]">
      {/* ── Section 1: Login protection ── */}
      <section
        aria-labelledby="login-protection-heading"
        className="px-6 pt-6 pb-8 space-y-6"
      >
        <h2
          id="login-protection-heading"
          className="text-xs font-medium uppercase tracking-wide text-muted-foreground"
        >
          Login protection
        </h2>

        <LoginProtectionPanel siteId={siteId} />

        {/* Recent events table sits below the config panel in the same section */}
        <div className="pt-2">
          <LoginEventsTable siteId={siteId} />
        </div>
      </section>

      {/* ── Section 2: Vulnerabilities (future-sprint stub) ── */}
      <section
        aria-labelledby="vulnerabilities-heading"
        className="px-6 pt-6 pb-8"
      >
        <h2
          id="vulnerabilities-heading"
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
    </div>
  );
}
