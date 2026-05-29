import { ArrowRight, Search } from "lucide-react";

// Surface 4.12 — inline empty state shown inside a table region when filters
// narrow the result set down to zero rows. Smaller and quieter than the full-
// page empty state: the operator hasn't "got nothing", they've just over-
// filtered. The fix is one keystroke (Clear filters) so the affordance is the
// loudest thing in the panel.
//
// `description` is a pre-composed, human-readable filter summary the caller
// builds — e.g. "status:down tag:client-x". We render it as part of the
// headline so the operator sees exactly which filters are doing the rejecting.
//
// The "Clear filters →" link is a text-link affordance (not a primary button)
// because the action is reversible and low-stakes. Verb-first label, trailing
// arrow icon for direction.

export interface FilterEmptyProps {
  /**
   * Human-readable filter summary, e.g. `status:down tag:client-x`. Rendered
   * inline at the end of the headline. Caller is responsible for formatting.
   */
  description: string;
  /** Invoked when the operator activates the Clear filters affordance. */
  onClearFilters: () => void;
}

export function FilterEmpty({ description, onClearFilters }: FilterEmptyProps) {
  return (
    <div
      role="status"
      aria-label="No sites match the current filters"
      className="flex flex-col items-center gap-3 py-12 text-center"
    >
      <Search
        aria-hidden="true"
        strokeWidth={1.5}
        className="size-8 text-[var(--color-muted-foreground)]/50"
      />
      <p className="text-balance text-sm text-[var(--color-foreground)]">
        No sites match{" "}
        <span className="font-mono text-[var(--color-muted-foreground)]">
          {description}
        </span>
        .
      </p>
      <button
        type="button"
        onClick={onClearFilters}
        className="inline-flex items-center gap-1 text-sm font-medium text-[var(--color-primary)] underline-offset-4 transition-colors hover:underline focus-visible:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)] focus-visible:ring-offset-2"
      >
        Clear filters
        <ArrowRight aria-hidden="true" className="size-3.5" />
      </button>
    </div>
  );
}
