import { useId } from "react";
import { Link } from "@tanstack/react-router";
import { AlertTriangle, CheckCircle2, Minus, MinusCircle, XCircle } from "lucide-react";

import { Skeleton } from "@/components/ui/skeleton";
import { cn } from "@/lib/utils";

import { CELL_WARNING_ARIA_LABEL, type AiCell } from "./readiness-cell-model";
import type { StatusTone } from "./readiness-copy";

// The Sites list "AI" column (variant "cell") and the grid card's chip
// (variant "chip"). Both render the same four states from one AiCell:
//
//   loading      a small skeleton while the fleet rollup is on its way
//   unavailable  a muted dash: the rollup failed or was refused, or this site is
//                not in it (archived, never enrolled). Never a page error
//   result       Ready / "N to fix" / Not checked, linking to the site's
//                Content tab, with what to fix on hover
//
// The amber triangle is separate from the result: an open AI connection point
// is a warning about the site and never changes Ready or "N to fix".

const ICON: Record<StatusTone, typeof CheckCircle2> = {
  ready: CheckCircle2,
  attention: XCircle,
  neutral: MinusCircle,
};

const ICON_CLASS: Record<StatusTone, string> = {
  ready: "text-[var(--color-success)]",
  attention: "text-[var(--color-destructive)]",
  neutral: "text-[var(--color-muted-foreground)]",
};

const CHIP_CLASS: Record<StatusTone, string> = {
  ready: "bg-success-subtle text-success-subtle-fg",
  attention: "bg-destructive-subtle text-destructive-subtle-fg",
  neutral: "bg-muted text-muted-foreground",
};

export interface AiReadinessCellProps {
  siteId: string;
  cell: AiCell;
  variant?: "cell" | "chip";
  className?: string;
}

function UnavailableMark() {
  return (
    <span
      role="img"
      aria-label="AI readiness unavailable"
      className="inline-flex text-[var(--color-muted-foreground)]/60"
    >
      <Minus aria-hidden="true" className="size-3.5" />
    </span>
  );
}

export function AiReadinessCell({ siteId, cell, variant = "cell", className }: AiReadinessCellProps) {
  const detailId = useId();
  const chip = variant === "chip";

  if (cell.kind === "loading") {
    return <Skeleton className={cn(chip ? "h-5 w-16" : "h-4 w-16", className)} />;
  }

  if (cell.kind === "unavailable") {
    return chip ? (
      <span
        className={cn(
          "inline-flex items-center gap-1 rounded bg-muted px-2 py-0.5 text-xs font-medium text-muted-foreground",
          className,
        )}
      >
        <span>AI</span>
        <UnavailableMark />
      </span>
    ) : (
      <span className={className}>
        <UnavailableMark />
      </span>
    );
  }

  const Icon = ICON[cell.tone];
  const detail = cell.lines.join(". ");
  return (
    <Link
      to="/sites/$siteId/content"
      params={{ siteId }}
      title={cell.lines.join("\n")}
      aria-describedby={detailId}
      onClick={(e) => e.stopPropagation()}
      className={cn(
        "inline-flex items-center gap-1.5 whitespace-nowrap text-xs font-medium",
        "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-1",
        chip
          ? cn("rounded px-2 py-0.5", CHIP_CLASS[cell.tone])
          : cn(
              "rounded-sm hover:underline",
              cell.tone === "neutral" ? "text-muted-foreground" : "text-foreground",
            ),
        className,
      )}
    >
      {chip ? <span className="font-semibold">AI</span> : null}
      <Icon
        aria-hidden="true"
        className={cn("size-3.5 shrink-0", chip ? "" : ICON_CLASS[cell.tone])}
      />
      <span>{cell.label}</span>
      {cell.hasWarning ? (
        <span
          role="img"
          aria-label={CELL_WARNING_ARIA_LABEL}
          className="inline-flex text-[var(--color-warning)]"
        >
          <AlertTriangle aria-hidden="true" className="size-3.5" />
        </span>
      ) : null}
      <span id={detailId} className="sr-only">
        {detail}
      </span>
    </Link>
  );
}
