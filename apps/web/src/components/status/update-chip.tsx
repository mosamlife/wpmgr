import { ArrowUp } from "lucide-react";

import { cn } from "@/lib/utils";

export interface UpdateChipProps {
  /** Count of pending updates. Shown as "{count} updates" (or "1 update"). */
  count: number;
  /** Major bumps trigger the warning palette; minor stays info. */
  severity: "minor" | "major";
  /** Optional descriptor surfaced as title tooltip (e.g. "Major: WP 7.0"). */
  description?: string;
  className?: string;
}

const severityClasses: Record<UpdateChipProps["severity"], string> = {
  minor: "bg-info-subtle text-info-subtle-fg",
  major: "bg-warning-subtle text-warning-subtle-fg",
};

/**
 * UpdateChip — "↑ N updates" pill for surfaces that aggregate pending
 * WordPress / plugin / theme updates per site. Minor bumps render in the
 * info-subtle palette; major bumps escalate to warning-subtle so they read
 * as "this needs operator attention, not just a routine sweep."
 */
export function UpdateChip({
  count,
  severity,
  description,
  className,
}: UpdateChipProps) {
  const label = count === 1 ? "1 update" : `${count} updates`;
  return (
    <span
      title={description}
      className={cn(
        // whitespace-nowrap: in the Sites table this pill sits in a fixed
        // track between the Agent and Backup columns, where a wrapped
        // "12 updates" doubled the row height and collided with both
        // neighbours (GH #255).
        "inline-flex items-center gap-1 whitespace-nowrap rounded px-2 py-0.5 text-xs font-medium",
        severityClasses[severity],
        className,
      )}
    >
      <ArrowUp aria-hidden="true" className="size-3" />
      <span>{label}</span>
    </span>
  );
}
