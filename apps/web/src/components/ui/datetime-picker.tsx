import * as React from "react";

import { cn } from "@/lib/utils";

// DateTimePicker — a shadcn-styled date AND time control.
//
// Built on the native `<input type="datetime-local">` so it needs no extra
// dependency (the repo's frozen lockfile can't take react-day-picker), while
// matching the design system's Input styling. The public value is an RFC3339 /
// ISO-8601 string (what the API wants); internally it converts to/from the
// browser-local "YYYY-MM-DDTHH:mm" the native control uses, so the user picks a
// local wall-clock time and we store the absolute instant.

export interface DateTimePickerProps {
  id?: string;
  /** RFC3339/ISO string, or "" when unset. */
  value: string;
  /** Emits an RFC3339 string, or "" when cleared. */
  onChange: (iso: string) => void;
  /** RFC3339 lower bound (e.g. "now") — the control disallows earlier values. */
  min?: string;
  disabled?: boolean;
  className?: string;
  "aria-invalid"?: boolean | "true" | "false";
  "aria-describedby"?: string;
}

/** RFC3339/ISO -> local "YYYY-MM-DDTHH:mm" for the native input (empty on bad input). */
function isoToLocalInput(iso: string): string {
  if (!iso) return "";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "";
  // Shift to local time, then trim the seconds/zone the control doesn't use.
  const tzOffsetMs = d.getTimezoneOffset() * 60_000;
  return new Date(d.getTime() - tzOffsetMs).toISOString().slice(0, 16);
}

/** Native "YYYY-MM-DDTHH:mm" (local) -> RFC3339 absolute instant. */
function localInputToIso(local: string): string {
  if (!local) return "";
  const d = new Date(local); // interpreted as local time
  if (Number.isNaN(d.getTime())) return "";
  return d.toISOString();
}

export const DateTimePicker = React.forwardRef<
  HTMLInputElement,
  DateTimePickerProps
>(({ id, value, onChange, min, disabled, className, ...aria }, ref) => {
  return (
    <input
      ref={ref}
      id={id}
      type="datetime-local"
      disabled={disabled}
      value={isoToLocalInput(value)}
      min={min ? isoToLocalInput(min) : undefined}
      onChange={(e) => onChange(localInputToIso(e.target.value))}
      className={cn(
        "flex h-9 w-full rounded-md border border-[var(--color-input)] bg-transparent px-3 py-1 text-sm shadow-sm transition-colors placeholder:text-[var(--color-muted-foreground)] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)] disabled:cursor-not-allowed disabled:opacity-50",
        className,
      )}
      {...aria}
    />
  );
});
DateTimePicker.displayName = "DateTimePicker";
