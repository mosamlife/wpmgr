import * as React from "react";

import { cn } from "@/lib/utils";

// Lightweight, accessible checkbox built on the native <input type="checkbox">
// (no extra Radix dependency). It forwards all native props/ref so it works
// with labels (htmlFor), aria-* attributes, and controlled state. Styling
// follows the same CSS-variable theme as the other shadcn primitives.
export type CheckboxProps = React.InputHTMLAttributes<HTMLInputElement>;

const Checkbox = React.forwardRef<HTMLInputElement, CheckboxProps>(
  ({ className, ...props }, ref) => (
    <input
      ref={ref}
      type="checkbox"
      className={cn(
        "size-4 shrink-0 cursor-pointer rounded border border-[var(--color-border)] accent-[var(--color-primary)] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)] disabled:cursor-not-allowed disabled:opacity-50",
        className,
      )}
      {...props}
    />
  ),
);
Checkbox.displayName = "Checkbox";

export { Checkbox };
