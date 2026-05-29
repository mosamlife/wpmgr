import { cn } from "@/lib/utils";

import {
  eolChipClasses,
  eolDaysLabel,
  eolTone,
  phpEolDays,
} from "./php-eol";

// EolChip — the PHP-EOL countdown rendered as a token-colored status chip.
// Shared by the header ribbon and the PHP card so the operator reads the same
// signal in both places. Warning tone within 90 days (or past EOL), success
// otherwise. Renders nothing when the version is unknown or off-calendar.

export function EolChip({ version }: { version: string | null }) {
  if (!version) return null;
  const days = phpEolDays(version);
  if (days === null) return null;
  const tone = eolTone(days);
  return (
    <span
      className={cn(
        "inline-flex items-center gap-1.5 rounded px-2 py-0.5 text-xs font-medium tabular-nums",
        eolChipClasses(tone),
      )}
    >
      <span
        aria-hidden="true"
        className={cn(
          "size-1.5 rounded-full",
          tone === "warning" ? "bg-warning" : "bg-success",
        )}
      />
      {eolDaysLabel(days)}
    </span>
  );
}
