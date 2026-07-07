import * as React from "react";

import { cn } from "@/lib/utils";

// Shadcn-style Select primitive built on the native <select> element.
// Forwards all native props + ref so it composes cleanly with react-hook-form
// {...register()} or a Controller's field.onChange/value. Styling mirrors the
// Input primitive (same border, height, ring) so the two always look sibling.

export type SelectProps = React.SelectHTMLAttributes<HTMLSelectElement>;

const Select = React.forwardRef<HTMLSelectElement, SelectProps>(
  ({ className, children, ...props }, ref) => (
    <select
      ref={ref}
      className={cn(
        // Explicit bg/text (not "transparent") so the native option popup -
        // which several browsers paint using the <select>'s own computed
        // background/color rather than the ancestor surface - matches the
        // app's popover surface in both themes instead of falling back to an
        // unreadable OS default (GH #150). `color-scheme` itself (set on
        // :root/.dark in globals.css, following the .dark class) is what
        // makes Chromium/WebKit render the popup chrome as dark to begin
        // with; this covers the browsers that also key off background/color.
        "h-9 w-full appearance-none rounded-md border border-[var(--color-input)] bg-[var(--color-popover)] px-3 text-sm text-[var(--color-popover-foreground)] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)] disabled:cursor-not-allowed disabled:opacity-50",
        className,
      )}
      {...props}
    >
      {children}
    </select>
  ),
);
Select.displayName = "Select";

export { Select };
