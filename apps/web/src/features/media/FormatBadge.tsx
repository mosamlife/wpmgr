import { ArrowRight } from "lucide-react";

import { cn } from "@/lib/utils";

// FormatBadge — "JPG → AVIF". Shows the source format and, when optimized, the
// current/target format it was encoded to. Mono so the format codes align with
// the rest of the table's technical columns.

/** Normalize a mime ("image/jpeg") or format ("avif") into a short label. */
function formatLabel(input: string): string {
  const v = input.toLowerCase();
  if (v.includes("jpeg") || v.includes("jpg")) return "JPG";
  if (v.includes("png")) return "PNG";
  if (v.includes("webp")) return "WebP";
  if (v.includes("avif")) return "AVIF";
  if (v.includes("gif")) return "GIF";
  if (v.includes("original")) return "Original";
  // mime like "image/svg+xml" → "SVG"
  const slash = v.indexOf("/");
  if (slash >= 0) {
    return v.slice(slash + 1).replace(/\+.*$/, "").toUpperCase();
  }
  return input.toUpperCase();
}

export interface FormatBadgeProps {
  /** Source mime or format ("image/jpeg", "jpg"). */
  source: string;
  /** Current/target format the asset is now in ("avif", "webp", "original"). */
  current?: string;
  className?: string;
}

export function FormatBadge({ source, current, className }: FormatBadgeProps) {
  const from = formatLabel(source);
  // Only show the arrow when the current format is a real, different encoding.
  const to =
    current && current.toLowerCase() !== "original"
      ? formatLabel(current)
      : null;
  const changed = to !== null && to !== from;

  return (
    <span
      className={cn(
        "inline-flex items-center gap-1 font-mono text-xs",
        className,
      )}
      aria-label={changed ? `${from} converted to ${to}` : `${from}`}
    >
      <span className="text-[var(--color-muted-foreground)]">{from}</span>
      {changed ? (
        <>
          <ArrowRight
            aria-hidden="true"
            className="size-3 text-[var(--color-muted-foreground)]"
          />
          <span className="text-[var(--color-foreground)]">{to}</span>
        </>
      ) : null}
    </span>
  );
}
