import { createFileRoute } from "@tanstack/react-router";

import { LoginProtectionPanel } from "@/features/security/login-protection-panel";
import { LoginEventsTable } from "@/features/security/login-events-table";
import { ScanPanel } from "@/features/security/scan-panel";

// `/sites/$siteId/security` — three sections:
//
//   1. Login Protection (S2) — config panel + recent events table.
//
//   2. Vulnerabilities — WPScan-based vuln scan (future sprint stub).
//      Retained from prior batch. When the scan endpoint lands:
//        - swap the empty state for an ErrorsTable-style table pattern
//        - use VulnSeverityChip for severity cells
//
//   3. Integrity scan (S3) — core file integrity scan using WordPress.org
//      checksums. Powered by hand-rolled Gin endpoints (not in ogen client).

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
        className="space-y-6 px-4 pb-8 pt-6 sm:px-6"
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
        className="px-4 pb-8 pt-6 sm:px-6"
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
          <p className="text-balance text-sm text-[var(--color-muted-foreground)]">
            Run a scan to check plugins, themes, and WordPress core against the
            WPScan database.
          </p>
        </div>
      </section>

      {/* ── Section 3: Integrity scan (S3) ── */}
      <section
        aria-labelledby="integrity-scan-heading"
        className="space-y-4 px-4 pb-8 pt-6 sm:px-6"
      >
        <h2
          id="integrity-scan-heading"
          className="text-xs font-medium uppercase tracking-wide text-muted-foreground"
        >
          Integrity scan
        </h2>

        <ScanPanel siteId={siteId} />
      </section>
    </div>
  );
}
