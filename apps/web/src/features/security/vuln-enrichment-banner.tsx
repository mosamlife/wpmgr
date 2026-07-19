import { AlertTriangle } from "lucide-react";

// VulnEnrichmentBanner: GH #245 degraded-signal banner.
//
// Shown when the vulnerability feed IS configured and scanning fine
// (`feed_ok === true`) but the last sync's CVSS enrichment pass did not
// complete (`enrichment_available === false`). This is a DIFFERENT state
// from "feed not configured" (`feed_ok === false`, handled by each caller's
// own FeedNotConfiguredState / early-return, which always takes priority).
// Here the scanner ran and findings exist, but some of them may be showing
// as `unknown` severity even though a real CVSS rating exists upstream.
//
// Tone: calm amber (`warning-subtle`), matching the app's other degraded /
// recoverable-state banners (see components/feedback/offline-banner.tsx,
// features/portal/portal-status-banner.tsx). Never destructive, since this
// is a transient upstream condition, not an operator error.

export function VulnEnrichmentBanner() {
  return (
    <div
      role="status"
      aria-live="polite"
      className="flex items-start gap-3 rounded-lg bg-[var(--color-warning-subtle)] px-4 py-3 text-sm text-[var(--color-warning-subtle-fg)]"
    >
      <AlertTriangle aria-hidden="true" className="mt-px size-4 shrink-0" />
      <p className="min-w-0 flex-1">
        <span className="font-medium">Severity data unavailable.</span> The
        CVSS enrichment feed was not reachable on the last sync, so some
        findings may be understated.
      </p>
    </div>
  );
}
