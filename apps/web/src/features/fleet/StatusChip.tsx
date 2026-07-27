// StatusChip — icon + label for an uptime status kind, shared by the fleet
// Uptime table's Status column (the "site row" an operator scans) and any
// other place a per-site `FleetStatusItem.status` needs the same triple
// encoded (colour + label + icon) treatment.
//
// GH #291: also renders a short, plain-language explanation under a
// `degraded` chip when the control plane sends a recognised
// `status_reason` code. The explanation is real text in the DOM (not a
// hover-only tooltip, not colour alone) so it is announced to a screen
// reader and stays visible without a pointer. An absent or unrecognised
// reason renders NOTHING extra — the chip looks exactly like it always has.

import { cn } from "@/lib/utils";
import { STATUS_ICON, STATUS_LABEL, STATUS_COLOR_CLASS, statusReasonCopy } from "./uptime-status";
import type { UptimeStatusKind } from "./fleet-types";

export interface StatusChipProps {
  status: UptimeStatusKind;
  /** `FleetStatusItem.status_reason` — only surfaced for a `degraded` status. */
  reason?: string | null;
  className?: string;
}

export function StatusChip({ status, reason, className }: StatusChipProps) {
  const Icon = STATUS_ICON[status];
  const note = status === "degraded" ? statusReasonCopy(reason) : null;

  return (
    <span className={cn("inline-flex flex-col items-start gap-0.5", className)}>
      <span
        className={cn(
          "inline-flex items-center gap-1 text-xs",
          STATUS_COLOR_CLASS[status],
        )}
      >
        <Icon aria-hidden="true" className="size-3.5 shrink-0" />
        {STATUS_LABEL[status]}
      </span>
      {note && (
        <span className="text-[11px] leading-snug text-[var(--color-muted-foreground)]">
          {note}
        </span>
      )}
    </span>
  );
}
