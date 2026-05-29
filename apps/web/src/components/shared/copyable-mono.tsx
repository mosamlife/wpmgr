import { useState } from "react";
import { Check, Copy } from "lucide-react";

import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";

// CopyableMono — a monospaced value (hash, id, IP, version, path) paired with a
// one-tap copy button. Generalizes the per-drawer HashRow copy pattern (ADR-037
// Batch 0) so Health-detail, Backup-detail, and Errors share one implementation.
//
//   • Value renders in font-mono so hashes/ids/IPs read in the right register.
//   • `truncate` middle-truncates long values with the full string in `title`.
//   • Copy uses navigator.clipboard, guarded for SSR, with a 1.5s Copied revert.
//   • Tokens only; verb-first button label ("Copy" / "Copied").

export interface CopyableMonoProps {
  /** The string copied to the clipboard and displayed in mono. */
  value: string;
  /** Middle-truncate long values, keeping the full string in the title. */
  truncate?: boolean;
  /** Accessible label for the copy button (defaults to "Copy value"). */
  label?: string;
  className?: string;
}

/** Middle-truncate so the head and tail of a hash/id stay legible. */
function middleTruncate(value: string, head = 10, tail = 8): string {
  if (value.length <= head + tail + 1) return value;
  return `${value.slice(0, head)}…${value.slice(-tail)}`;
}

export function CopyableMono({
  value,
  truncate = false,
  label,
  className,
}: CopyableMonoProps) {
  const [copied, setCopied] = useState(false);

  const onCopy = () => {
    if (typeof navigator === "undefined" || !navigator.clipboard) return;
    void navigator.clipboard.writeText(value).then(() => {
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1500);
    });
  };

  const display = truncate ? middleTruncate(value) : value;

  return (
    <span className={cn("inline-flex min-w-0 items-center gap-1.5", className)}>
      <code
        title={truncate ? value : undefined}
        className={cn(
          "min-w-0 rounded bg-muted/50 px-1.5 py-0.5 font-mono text-xs text-foreground",
          truncate ? "truncate" : "break-all",
        )}
      >
        {display}
      </code>
      <Button
        size="sm"
        variant="ghost"
        type="button"
        onClick={onCopy}
        aria-label={label ?? "Copy value"}
        className="shrink-0"
      >
        {copied ? (
          <>
            <Check aria-hidden="true" className="size-3.5" />
            Copied
          </>
        ) : (
          <>
            <Copy aria-hidden="true" className="size-3.5" />
            Copy
          </>
        )}
      </Button>
    </span>
  );
}
