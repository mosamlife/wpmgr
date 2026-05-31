import { cn } from "@/lib/utils";
import { StatusDot } from "@/components/status/status-dot";

import { assetStatusPresentation } from "./asset-status";
import type { AssetStatus } from "./types";

// AssetStatusChip — the 8-state status indicator for a media asset row.
// Presentation is resolved by the pure `assetStatusPresentation` table
// (asset-status.ts), which is unit-tested per state. This component is a thin
// view over it: dot + label, with `excluded` rendered ring-only (no fill) and
// `failed` rendered as a button that surfaces the failure reason on click.

export interface AssetStatusChipProps {
  status: AssetStatus;
  /** 0–100 — only used for `optimizing` ("Optimizing • {n}%"). */
  progress?: number;
  /** Failure reason; shown via title + click for `failed`. */
  reason?: string;
  /** Click handler for `failed` (e.g. open the job detail with the reason). */
  onReasonClick?: () => void;
  className?: string;
}

export function AssetStatusChip({
  status,
  progress,
  reason,
  onReasonClick,
  className,
}: AssetStatusChipProps) {
  const p = assetStatusPresentation(status, progress);

  const inner = (
    <>
      <StatusDot tone={p.tone} pulse={p.pulse} />
      <span className="whitespace-nowrap">{p.label}</span>
    </>
  );

  const baseChip = cn(
    "inline-flex items-center gap-1.5 rounded px-2 py-0.5 text-xs font-medium",
    p.variant === "ring"
      ? "border border-[var(--color-border)] text-[var(--color-muted-foreground)]"
      : "text-[var(--color-foreground)]",
    className,
  );

  // `failed` is interactive: clicking reveals the reason (job detail). Render as
  // a real <button> so it is keyboard-focusable and AA-contrast.
  if (p.interactive && onReasonClick) {
    return (
      <button
        type="button"
        onClick={onReasonClick}
        title={reason ?? "View failure reason"}
        aria-label={`${p.label}. ${reason ? `Reason: ${reason}. ` : ""}View details`}
        className={cn(
          baseChip,
          "cursor-pointer underline-offset-2 hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)]",
        )}
      >
        {inner}
      </button>
    );
  }

  return (
    <span
      className={baseChip}
      title={status === "failed" && reason ? reason : undefined}
      aria-label={p.label}
    >
      {inner}
    </span>
  );
}
