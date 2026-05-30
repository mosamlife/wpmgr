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
        "h-9 w-full appearance-none rounded-md border border-[var(--color-input)] bg-transparent px-3 text-sm text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)] disabled:cursor-not-allowed disabled:opacity-50",
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
