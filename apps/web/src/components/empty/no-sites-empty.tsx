import { Plug } from "lucide-react";

import { AddSiteDialog } from "@/features/sites/add-site-dialog";

// Surface 4.12 — full-page empty state shown when the tenant has zero sites
// enrolled AND has already completed (or skipped) the first-site onboarding
// wizard. The onboarding wizard (see ./onboarding-wizard.tsx) handles the
// first-ever zero-state; this is the steady-state empty surface used after a
// tenant deletes their last site, or for read-only operators who never run the
// wizard.
//
// Composition is intentionally minimal per DESIGN.md "calm, dense, operator-
// grade" — a single line illustration, an instrument-panel headline (period,
// not a question), one body sentence that quantifies the value (sub-90s
// enrollment), and one verb-first primary action. No "Welcome to", no hero
// metric theatre, no encouragement copy.
//
// The CTA reuses the existing AddSiteDialog (Sprint 3 modals — locked) so the
// pairing-code flow stays in one place. The dialog renders its own trigger
// button, which is already the verb-first "Add site" — semantics match.

export interface NoSitesEmptyProps {
  /**
   * Render an alternative CTA slot (e.g. an inert placeholder for read-only
   * operators who can't enroll sites). Defaults to the standard AddSiteDialog.
   */
  cta?: React.ReactNode;
}

export function NoSitesEmpty({ cta }: NoSitesEmptyProps = {}) {
  return (
    <div
      role="status"
      aria-label="No sites enrolled"
      className="mx-auto flex min-h-[60vh] max-w-md flex-col items-center justify-center gap-4 px-6 text-center"
    >
      <Plug
        aria-hidden="true"
        strokeWidth={1.25}
        className="size-16 text-[var(--color-muted-foreground)]/40"
      />
      <h2 className="text-balance text-xl font-semibold text-[var(--color-foreground)]">
        Connect your first WordPress site.
      </h2>
      <p className="text-balance text-sm text-[var(--color-muted-foreground)]">
        We&apos;ll pull updates, backups, and uptime checks within 90 seconds.
      </p>
      <div className="pt-2">{cta ?? <AddSiteDialog />}</div>
    </div>
  );
}
