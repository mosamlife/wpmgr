import { useId, useState } from "react";
import { Check, ChevronDown, ChevronRight, Copy } from "lucide-react";

import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";

// The full agent log for one task: monospace, newline-preserving, scrollable
// past a reasonable height, and copyable in one click. It carries `task.error`,
// the raw diagnostic behind the sentence an update row or table cell leads
// with.
export function TaskLogPanel({
  id,
  error,
  className,
}: {
  id: string;
  error: string;
  className?: string;
}) {
  const [copied, setCopied] = useState(false);

  const onCopy = () => {
    if (typeof navigator === "undefined" || !navigator.clipboard) return;
    void navigator.clipboard.writeText(error).then(() => {
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1500);
    });
  };

  return (
    <div id={id} className={cn("space-y-2 border-t border-border p-3", className)}>
      <div className="flex items-center justify-between gap-2">
        <h3 className="text-xs font-medium uppercase tracking-wide text-muted-foreground">
          Agent log
        </h3>
        <Button
          type="button"
          variant="ghost"
          size="sm"
          onClick={onCopy}
          aria-label="Copy agent log"
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
      </div>
      <pre className="max-h-64 overflow-auto rounded-md border border-border bg-muted/50 p-3 font-mono text-xs whitespace-pre-wrap break-words text-foreground">
        {error}
      </pre>
    </div>
  );
}

// A toggle that opens a task's raw log inline, for surfaces that show one task
// as a line of text rather than as a table row with a second row beneath it.
export function TaskLogDisclosure({ log }: { log: string }) {
  const [open, setOpen] = useState(false);
  const panelId = useId();

  return (
    <div className="flex w-full flex-col items-start gap-1">
      <Button
        type="button"
        variant="ghost"
        size="sm"
        onClick={() => setOpen((v) => !v)}
        aria-expanded={open}
        aria-controls={panelId}
        className="h-auto px-1.5 py-0.5 font-sans text-xs"
      >
        {open ? (
          <ChevronDown aria-hidden="true" className="size-3.5" />
        ) : (
          <ChevronRight aria-hidden="true" className="size-3.5" />
        )}
        {open ? "Hide log" : "Show log"}
      </Button>
      {open ? (
        <TaskLogPanel
          id={panelId}
          error={log}
          className="w-full rounded-md border"
        />
      ) : null}
    </div>
  );
}
