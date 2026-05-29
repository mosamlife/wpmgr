import type { ReactNode } from "react";

// Phase 4 / Sprint 4 surface 4.14 - Forms.
//
// Two-column form section. On large screens the title + description sit in
// the left column (lg:col-span-1) and the inputs sit in the right column
// (lg:col-span-2). On small screens the two stack vertically. Matches the
// Linear/Sentry/Stripe settings-page convention so operators scanning a long
// settings surface get the structure for free.
//
// Sections stack with a 1px border between them (no nested cards per
// DESIGN.md "Components - Card: Never nested").

interface FormSectionProps {
  title: string;
  description?: string;
  children: ReactNode;
}

export function FormSection({ title, description, children }: FormSectionProps) {
  return (
    <section className="border-b border-border py-8 last:border-b-0">
      <div className="grid grid-cols-1 gap-6 lg:grid-cols-3">
        <div className="lg:col-span-1">
          <h2 className="text-base font-semibold text-foreground">{title}</h2>
          {description ? (
            <p className="mt-1 text-sm text-muted-foreground">{description}</p>
          ) : null}
        </div>
        <div className="space-y-6 lg:col-span-2">{children}</div>
      </div>
    </section>
  );
}
