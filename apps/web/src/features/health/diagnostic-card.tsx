import type { ReactNode } from "react";

import { FreshnessBadge } from "@/components/shared/freshness-badge";
import { cn } from "@/lib/utils";
import type { SiteDiagnosticsCard } from "@wpmgr/api";

// Shared card shell for the Health-tab diagnostics cards (ADR-037 Impeccable,
// Batch 1). Each card-foo.tsx translates its category-specific payload into a
// shared DefinitionList and passes it as children.
//
// What changed in the Impeccable pass:
//   - The per-card "Re-run check" footer button is GONE. The refresh mutation
//     is site-scoped (useRefreshDiagnostics), so 13 per-card buttons were
//     misleading; the ribbon owns the single "Re-run all checks" action.
//   - Freshness is the shared FreshnessBadge, not bespoke text. A stale card
//     also flags a warning dot beside the title so a scan catches it.
//   - Cards never nest cards: the card is the surface, the body is plain.
//
// Voice + typography rules still apply at the call sites: verb-first labels,
// tabular-nums on numbers, font-mono on paths/versions/constants, no em-dashes.

export interface DiagnosticCardProps {
  title: string;
  card: SiteDiagnosticsCard | undefined;
  children?: ReactNode;
  /** A status chip rendered on the title row (e.g. PHP-EOL, plugin updates). */
  titleChip?: ReactNode;
  /** Replaces the freshness badge with a one-line status note when set. */
  note?: ReactNode;
  className?: string;
}

export function DiagnosticCard({
  title,
  card,
  children,
  titleChip,
  note,
  className,
}: DiagnosticCardProps) {
  const collectedAt = card?.collected_at ?? null;
  // The card's own `fresh` flag is the source of truth for the stale dot; the
  // FreshnessBadge then renders the relative time using the shared threshold.
  const stale = card != null && collectedAt != null && card.fresh === false;

  return (
    <div
      className={cn(
        "flex flex-col gap-3 rounded-lg border border-border bg-card p-5",
        className,
      )}
    >
      <div className="flex flex-wrap items-start justify-between gap-x-3 gap-y-1.5">
        <div className="flex min-w-0 items-center gap-2">
          {stale ? (
            <span
              aria-hidden="true"
              className="size-1.5 shrink-0 rounded-full bg-warning"
            />
          ) : null}
          <h3 className="text-sm font-semibold text-foreground">{title}</h3>
          {titleChip}
        </div>
        {note != null ? (
          <span className="text-xs text-warning-subtle-fg">{note}</span>
        ) : (
          <FreshnessBadge collectedAt={collectedAt} />
        )}
      </div>

      {card?.payload ? (
        children
      ) : (
        <p className="text-sm text-muted-foreground">
          Awaiting first sync from the agent.
        </p>
      )}
    </div>
  );
}
