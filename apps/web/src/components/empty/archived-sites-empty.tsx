import { ArrowLeft, Archive } from "lucide-react";

// The Sites page's archived list holds nothing. This is its own state, not the
// first-run screen: the tenant has sites, they are all active, and the way back
// to them has to stay on the page.
//
// It is shown to every role. A viewer can reach the archived list by link but
// has no archived chip to leave it with, so the button below is their way out.

export interface ArchivedSitesEmptyProps {
  /** Leaves the archived list and returns to the active sites. */
  onBack: () => void;
}

export function ArchivedSitesEmpty({ onBack }: ArchivedSitesEmptyProps) {
  return (
    <div
      role="status"
      aria-label="No archived sites"
      className="flex flex-col items-center gap-3 py-12 text-center"
    >
      <Archive
        aria-hidden="true"
        strokeWidth={1.5}
        className="size-8 text-[var(--color-muted-foreground)]/50"
      />
      <p className="text-balance text-sm text-[var(--color-foreground)]">
        No archived sites
      </p>
      <p className="text-balance text-sm text-[var(--color-muted-foreground)]">
        A site you disconnect is archived here, with its history kept.
      </p>
      <button
        type="button"
        onClick={onBack}
        className="inline-flex items-center gap-1 text-sm font-medium text-[var(--color-primary)] underline-offset-4 transition-colors hover:underline focus-visible:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)] focus-visible:ring-offset-2"
      >
        <ArrowLeft aria-hidden="true" className="size-3.5" />
        Back to active sites
      </button>
    </div>
  );
}
