import { useState } from "react";

import { cn } from "@/lib/utils";

import { truncateAddress } from "./request-card-model";

// The fixed "Page address" slot (tracka-cache-purge-design-v7 §2.6, §3.6,
// the Human-card test). This is the ONE model-chosen value anywhere on the
// card, so it gets its own component rather than living inline in
// request-card.tsx -- every rule below is load-bearing and each one is
// tested in isolation.
//
//   - a text node, never an href, a link label, or a title attribute;
//   - percent-encoding shown exactly as stored, never decoded, so an
//     encoded bidi override or encoded prose stays visibly encoded;
//   - capped at 120 characters plus "…", with "Show full address" revealing
//     the untouched value in the same slot.
//
// A planted hostile string (a bidi override, `IGNORE PREVIOUS`, a fake
// "your admin asked for this") renders here exactly like any other string:
// as inert characters inside a <p>, never interpreted.

export interface PageAddressProps {
  url: string;
  className?: string;
}

export function PageAddress({ url, className }: PageAddressProps) {
  const [expanded, setExpanded] = useState(false);
  const { display, truncated } = truncateAddress(url);
  const shown = expanded ? url : display;

  return (
    <div className={cn("space-y-1", className)}>
      {/* No href, no title, no dangerouslySetInnerHTML: this is exactly the
          text node the design requires and nothing else. */}
      <p className="min-w-0 break-all rounded-md border border-border bg-muted/40 px-2 py-1.5 font-mono text-sm text-foreground">
        {shown}
      </p>
      {truncated && !expanded ? (
        <button
          type="button"
          onClick={() => setExpanded(true)}
          className="text-xs text-muted-foreground underline underline-offset-2 hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2"
        >
          Show full address
        </button>
      ) : null}
    </div>
  );
}
