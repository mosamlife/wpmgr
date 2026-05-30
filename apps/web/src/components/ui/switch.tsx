import * as React from "react";

import { cn } from "@/lib/utils";

// Lightweight accessible Switch built on a hidden <input type="checkbox">.
// Compatible with react-hook-form Controller (use field.value + field.onChange)
// or standalone (checked + onCheckedChange). WCAG 2.2 AA: role="switch" +
// aria-checked so screen readers announce the state correctly.

export interface SwitchProps {
  checked?: boolean;
  onCheckedChange?: (checked: boolean) => void;
  disabled?: boolean;
  id?: string;
  "aria-label"?: string;
  "aria-labelledby"?: string;
  className?: string;
}

const Switch = React.forwardRef<HTMLButtonElement, SwitchProps>(
  (
    {
      checked = false,
      onCheckedChange,
      disabled = false,
      id,
      className,
      ...ariaProps
    },
    ref,
  ) => (
    <button
      ref={ref}
      type="button"
      role="switch"
      id={id}
      aria-checked={checked}
      disabled={disabled}
      onClick={() => onCheckedChange?.(!checked)}
      className={cn(
        "relative inline-flex h-5 w-9 shrink-0 cursor-pointer items-center rounded-full border-2 border-transparent transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)] focus-visible:ring-offset-2 focus-visible:ring-offset-background disabled:cursor-not-allowed disabled:opacity-50",
        checked
          ? "bg-[var(--color-primary)]"
          : "bg-[var(--color-input)]",
        className,
      )}
      {...ariaProps}
    >
      <span
        aria-hidden="true"
        className={cn(
          "pointer-events-none inline-block size-4 rounded-full bg-white shadow-sm ring-0 transition-transform",
          checked ? "translate-x-4" : "translate-x-0",
        )}
      />
    </button>
  ),
);
Switch.displayName = "Switch";

export { Switch };
