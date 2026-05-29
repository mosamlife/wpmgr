import type { HTMLAttributes } from "react";

import { cn } from "@/lib/utils";

// Surface 4.13 — skeleton primitive.
//
// One element, one job: paint a muted block that breathes. The pulse is
// implemented as the `.wpmgr-skeleton-pulse` utility in globals.css (opacity
// 0.4 → 0.7 → 0.4 over 1.4s linear, per DESIGN.md), not Tailwind's built-in
// `animate-pulse` whose timing/curve is wrong for this surface. The
// `motion-reduce:animate-none` is a belt-and-braces companion to the
// reduced-motion @media rule that already neutralizes the keyframe directly.
//
// Color: `bg-muted` (token from DESIGN.md), held at /60 so the breathing
// effect lands in a comfortable range against `--background`.

export function Skeleton({
  className,
  ...props
}: HTMLAttributes<HTMLDivElement>) {
  return (
    <div
      aria-hidden="true"
      className={cn(
        "wpmgr-skeleton-pulse rounded-md bg-muted/60 motion-reduce:animate-none",
        className,
      )}
      {...props}
    />
  );
}
