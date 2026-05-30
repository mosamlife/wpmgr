import type { CSSProperties, ReactNode } from "react";
import { Construction } from "lucide-react";
import { cn } from "@/lib/utils";

// PlannedFeature — a tasteful gap-page for routes that have a nav entry but no
// real implementation yet (ADR-037 "planned-feature placeholder").
//
// Design contract (DESIGN.md / Impeccable conventions):
//   • Tokens only — no off-token colours, no purple, no shadows.
//   • Borders over shadows (rounded-lg border border-[var(--color-border)]).
//   • No nested cards — one surface level, one panel.
//   • Verb-first copy; operator-centric language.
//   • WCAG 2.2 AA: heading hierarchy (h1 below any skip-nav landmark), icon
//     is aria-hidden, the "Available today" link is an accessible <a> via
//     TanStack Router's <Link>.
//   • Construction icon at strokeWidth 1.5 (consistent with lucide usage site-wide).

export interface PlannedFeatureProps {
  /** Page / feature title displayed in the h1. */
  title: string;
  /** One or two sentences describing what the planned feature will do. */
  summary: string;
  /**
   * When provided, renders an "Available today:" line with this node.
   * Typically a TanStack <Link> to the nearest available equivalent.
   */
  availableToday?: ReactNode;
  className?: string;
}

export function PlannedFeature({
  title,
  summary,
  availableToday,
  className,
}: PlannedFeatureProps) {
  return (
    <section
      aria-labelledby="planned-feature-heading"
      className={cn("space-y-6", className)}
    >
      {/* Page heading block — matches the pattern used in UpdatesPage /
          AlertSettingsPage: a plain heading + muted subline, no wrapper card. */}
      <div className="space-y-1">
        <h1
          id="planned-feature-heading"
          className="text-lg font-semibold text-[var(--color-foreground)]"
        >
          {title}
        </h1>
        <p className="text-sm text-[var(--color-muted-foreground)]">
          Planned feature
        </p>
      </div>

      {/* Single bordered panel — one surface level; no card-in-card. */}
      <div className="rounded-lg border border-[var(--color-border)] p-6">
        <div className="flex items-start gap-4">
          <Construction
            aria-hidden="true"
            strokeWidth={1.5}
            className="mt-0.5 size-5 shrink-0 text-[var(--color-muted-foreground)]"
          />
          <div className="min-w-0 space-y-3">
            <p
              className="text-sm text-[var(--color-muted-foreground)]"
              style={{ textWrap: "pretty" } satisfies CSSProperties}
            >
              {summary}
            </p>
            {availableToday != null ? (
              <p className="text-sm text-[var(--color-muted-foreground)]">
                <span className="font-medium text-[var(--color-foreground)]">
                  Available today:
                </span>{" "}
                {availableToday}
              </p>
            ) : null}
          </div>
        </div>
      </div>
    </section>
  );
}
