import { forwardRef, type ButtonHTMLAttributes, type ReactNode } from "react";

import { Badge } from "@/components/ui/badge";
import { cn } from "@/lib/utils";
import { resolveTagStyle } from "@/lib/tag-color";

// GH #230 "rich tags" — the shared tag chip, used everywhere a site's tags
// render (the sites table, the sites grid card, the tag editor, the bulk
// tag-edit panel's results, and the tags filter dropdown's active chips).

export interface TagChipProps {
  tag: { name: string; color?: string | null };
  /** When present, the chip renders as a focusable button (click-to-filter). */
  onClick?: () => void;
  /** Optional trailing slot, e.g. a remove (×) button. */
  trailing?: ReactNode;
  className?: string;
}

const CHIP_BASE =
  "inline-flex h-5 max-w-[9rem] items-center truncate rounded-md border px-1.5 py-0 text-xs font-medium";

export function TagChip({ tag, onClick, trailing, className }: TagChipProps) {
  const visual = resolveTagStyle(tag);
  const classes = cn(CHIP_BASE, visual.className, className);

  if (onClick) {
    return (
      <button
        type="button"
        onClick={onClick}
        title={tag.name}
        aria-label={`Filter by tag ${tag.name}`}
        style={visual.style}
        className={cn(
          classes,
          "gap-1 transition-opacity hover:opacity-80",
          "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-1",
        )}
      >
        <span className="truncate">{tag.name}</span>
        {trailing}
      </button>
    );
  }

  return (
    <Badge
      variant="outline"
      title={tag.name}
      style={visual.style}
      className={cn(classes, "gap-1")}
    >
      <span className="truncate">{tag.name}</span>
      {trailing}
    </Badge>
  );
}

export interface TagOverflowChipProps
  extends Omit<ButtonHTMLAttributes<HTMLButtonElement>, "onClick" | "type"> {
  count: number;
  /** Optional — omit when this chip is used purely as a Popover/Dialog trigger
   *  (the trigger wrapper supplies its own onClick via `asChild`). */
  onClick?: () => void;
}

/**
 * The "+N" overflow chip. ALWAYS a focusable button, never a hover-only
 * affordance — keyboard and touch users must be able to reach the remaining
 * tags (and, via the click handler, open the tag picker) exactly like mouse
 * users can. Forwards its ref so it composes correctly as a Radix
 * `asChild` trigger (Popover/Dialog position/anchor to the real DOM node).
 */
export const TagOverflowChip = forwardRef<HTMLButtonElement, TagOverflowChipProps>(
  function TagOverflowChip({ count, onClick, className, ...rest }, ref) {
    return (
      <button
        ref={ref}
        type="button"
        onClick={onClick}
        aria-label={`${count} more ${count === 1 ? "tag" : "tags"}. Edit tags.`}
        className={cn(
          CHIP_BASE,
          "border-transparent bg-muted text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground",
          "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-1",
          className,
        )}
        {...rest}
      >
        +{count}
      </button>
    );
  },
);
