import * as React from "react";

import { cn } from "@/lib/utils";

// SegmentedControl — a small, dependency-free "radio group rendered as
// buttons" primitive (WAI-ARIA radiogroup pattern), for a short, mutually
// exclusive choice with 2-4 text options (e.g. a payment provider, a
// currency). Modeled on the icon-only density toggle in
// features/sites/sites-toolbar.tsx, generalized to take a label per option
// and real `role="radiogroup"`/`role="radio"` semantics (that toggle uses
// `aria-pressed` buttons instead, which is the right call for icon buttons
// but not for a labeled multi-option choice like this one).
//
//   <SegmentedControl
//     aria-label="Payment provider"
//     value={provider}
//     onChange={setProvider}
//     options={[
//       { value: "stripe", label: "Stripe" },
//       { value: "razorpay", label: "Razorpay" },
//     ]}
//   />

export interface SegmentedControlOption<T extends string> {
  value: T;
  label: React.ReactNode;
}

export interface SegmentedControlProps<T extends string> {
  value: T;
  onChange: (value: T) => void;
  options: readonly SegmentedControlOption<T>[];
  "aria-label": string;
  className?: string;
}

export function SegmentedControl<T extends string>({
  value,
  onChange,
  options,
  className,
  ...aria
}: SegmentedControlProps<T>) {
  // Arrow-key roving over the options, Home/End to jump to the first/last —
  // mirrors TabsList's keyboard handling (components/ui/tabs.tsx) so every
  // "pick one of a short row of options" control in the app behaves the same
  // way under the keyboard.
  function onKeyDown(e: React.KeyboardEvent<HTMLDivElement>) {
    if (!["ArrowRight", "ArrowLeft", "Home", "End"].includes(e.key)) return;
    if (options.length === 0) return;
    const current = options.findIndex((o) => o.value === value);
    let next = current;
    switch (e.key) {
      case "ArrowRight":
        next = current < 0 ? 0 : (current + 1) % options.length;
        break;
      case "ArrowLeft":
        next = current <= 0 ? options.length - 1 : current - 1;
        break;
      case "Home":
        next = 0;
        break;
      case "End":
        next = options.length - 1;
        break;
    }
    e.preventDefault();
    const option = options[next];
    if (option) onChange(option.value);
  }

  return (
    <div
      role="radiogroup"
      onKeyDown={onKeyDown}
      className={cn(
        "inline-flex items-center rounded-md border border-[var(--color-border)] bg-[var(--color-background)] p-0.5",
        className,
      )}
      {...aria}
    >
      {options.map((option) => {
        const active = option.value === value;
        return (
          <button
            key={option.value}
            type="button"
            role="radio"
            aria-checked={active}
            tabIndex={active ? 0 : -1}
            onClick={() => onChange(option.value)}
            className={cn(
              "inline-flex items-center justify-center rounded-sm px-3 py-1 text-sm font-medium text-[var(--color-muted-foreground)] transition-colors",
              "hover:text-[var(--color-foreground)]",
              "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)]",
              active &&
                "bg-[var(--color-accent)] text-[var(--color-accent-foreground)]",
            )}
          >
            {option.label}
          </button>
        );
      })}
    </div>
  );
}
