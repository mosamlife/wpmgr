import { AlertTriangle } from "lucide-react";

import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";

// Phase 6 (harden) — calm, in-page error state for top-level query failures.
//
// Pattern (DESIGN.md "Don't write generic errors. Always: what, why, how."):
//
//   [!]  Could not load sites.
//        We hit the server but it returned 500.
//        [Try again]
//
// Why a dedicated primitive:
//   • Every page that fetches via TanStack Query needs the same shape.
//   • The previous per-route ad-hoc `<p className="text-destructive">{error}</p>`
//     leaks raw error messages and offers no recovery affordance.
//   • Borders over shadows; muted-on-destructive — never red rectangles.
//
// Hardening contract:
//   • The container has `min-w-0` so it survives inside narrow flex/grid cells
//     without breaking the parent layout when the `why` line is a long German
//     translation (+30%) or a verbose backend error.
//   • Headline and prose use `text-wrap: pretty` / `text-wrap: balance` so
//     long sentences wrap cleanly without ragged orphans.
//   • The icon is `aria-hidden`; the role="alert" on the container is what
//     the screen reader announces, with `what` as the live region label.
//
// Token contract:
//   • Surface: `--card` on `--background` (page-level), but the inner border
//     uses `--destructive` at low alpha — paints "this is an error" without
//     blasting the page red. Matches the Sprint 3 alert tone in
//     `routes/_authed/backups/$snapshotId.tsx`.
//   • Headline: `--foreground` body weight 600.
//   • Why: `--muted-foreground` body-sm.
//   • Button: outline, sm — recovery is reversible and low-stakes by design.

export interface PageErrorProps {
  /** What failed, headline-style. Verb-first if the operator caused it; noun-first if the system did. */
  what: string;
  /** Optional why-line. Plain prose, not a raw exception message. */
  why?: string;
  /** Optional retry handler. When provided, surfaces a verb-first "Try again" button. */
  onRetry?: () => void;
  /** Override the retry label if the page wants verb-specific copy ("Reload snapshot"). */
  retryLabel?: string;
  /** Disable the retry button while a refetch is already in flight. */
  isRetrying?: boolean;
  /** Optional extra classes for callers that need to constrain the width. */
  className?: string;
}

export function PageError({
  what,
  why,
  onRetry,
  retryLabel = "Try again",
  isRetrying,
  className,
}: PageErrorProps) {
  return (
    <div
      role="alert"
      aria-live="polite"
      className={cn(
        // Single calm surface — never a card-in-card. min-w-0 prevents the
        // alert from forcing its parent flex/grid to grow.
        "flex min-w-0 items-start gap-3 rounded-lg border border-[var(--color-destructive)]/30 bg-[var(--color-card)] p-4",
        className,
      )}
    >
      <AlertTriangle
        aria-hidden="true"
        className="mt-0.5 size-4 shrink-0 text-[var(--color-destructive)]"
      />
      <div className="flex min-w-0 flex-1 flex-col gap-2">
        <div className="space-y-1">
          <p
            className="text-balance text-sm font-semibold text-[var(--color-foreground)]"
            style={{ textWrap: "balance" }}
          >
            {what}
          </p>
          {why ? (
            <p
              className="text-sm text-[var(--color-muted-foreground)]"
              style={{ textWrap: "pretty" }}
            >
              {why}
            </p>
          ) : null}
        </div>
        {onRetry ? (
          <div>
            <Button
              type="button"
              variant="outline"
              size="sm"
              onClick={onRetry}
              disabled={isRetrying}
            >
              {isRetrying ? "Retrying…" : retryLabel}
            </Button>
          </div>
        ) : null}
      </div>
    </div>
  );
}
