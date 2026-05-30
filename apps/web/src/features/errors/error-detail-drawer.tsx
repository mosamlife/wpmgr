import { useState } from "react";

import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogBody,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { DefinitionList, KvRow } from "@/components/shared/definition-list";
import { relativeTime } from "@/lib/utils";
import type { PhpError, PhpErrorFrame } from "@wpmgr/api";
import { toast } from "@/components/toast/use-toast-helpers";

import { PHPSeverityChip } from "./php-severity-chip";
import { useErrorConfig, useUpdateErrorConfig } from "./use-error-config";

// Detail drawer for one PHP error fingerprint (ADR-037 Batch 4, Impeccable
// Restyle). The UI library ships no Sheet primitive yet, so we render the
// shared Dialog widened to max-w-[720px].
//
// Changes from Sprint 2:
//   • PHPSeverityChip in the title (chip with dot+label, never bare mono text).
//   • DefinitionList/KvRow replaces the hand-rolled Field grid — handles mono,
//     copyable, and absent values consistently with the rest of the app.
//   • Fingerprint rendered via KvRow copyable= (CopyableMono with copy button).
//   • All actions are verb-first ("Copy fingerprint", "Silence", "Unsilence", "Close").
//   • Message full-text remains in a scrollable <pre> for stack context readability.
//
// Backtrace surface reserved: ErrorMonitor writes NULL until the backtrace-
// capture sprint lands; the pre block is kept so the column is always present.

export interface ErrorDetailDrawerProps {
  siteId: string;
  error: PhpError | null;
  onClose: () => void;
  onSilence: (silenced: boolean) => void;
}

export function ErrorDetailDrawer({
  siteId,
  error,
  onClose,
  onSilence,
}: ErrorDetailDrawerProps) {
  const [copied, setCopied] = useState(false);
  const { data: config } = useErrorConfig(siteId);
  const updateConfig = useUpdateErrorConfig(siteId);

  if (!error) return null;

  const isAlreadyIgnored = config?.ignore_md5s.includes(error.md5) ?? false;

  function handleIgnore() {
    if (!config) return;
    const next = isAlreadyIgnored
      ? config.ignore_md5s.filter((m) => m !== error!.md5)
      : [...config.ignore_md5s, error!.md5];
    updateConfig.mutate(
      { error_level: config.error_level, ignore_md5s: next },
      {
        onSuccess: () => {
          toast.success(
            isAlreadyIgnored ? "Fingerprint removed from ignore-list." : "Fingerprint added to ignore-list.",
            { description: isAlreadyIgnored ? "The agent will capture this error again." : "The agent will suppress this error fingerprint." },
          );
          onClose();
        },
        onError: (err: Error) => {
          toast.error("Could not update ignore-list.", { description: err.message });
        },
      },
    );
  }

  const copy = () => {
    void navigator.clipboard.writeText(error.md5).then(() => {
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1500);
    });
  };

  const lineValue = error.line > 0 ? String(error.line) : undefined;
  const firstSeen = relativeTime(error.first_seen_at) ?? "recently";
  const lastSeen = relativeTime(error.last_seen_at) ?? "recently";

  return (
    <Dialog open={true} onClose={onClose}>
      <DialogContent
        ariaLabelledBy="error-detail-title"
        className="max-w-[min(720px,calc(100vw-2rem))]"
      >
        <DialogHeader>
          <DialogTitle id="error-detail-title">
            <span className="flex flex-wrap items-center gap-2">
              <PHPSeverityChip severity={error.severity} />
              <span className="text-sm text-foreground">
                {shortMessage(error.message)}
              </span>
            </span>
          </DialogTitle>
          <DialogDescription>
            First seen{" "}
            <time dateTime={error.first_seen_at} title={error.first_seen_at}>
              {firstSeen}
            </time>
            , last seen{" "}
            <time dateTime={error.last_seen_at} title={error.last_seen_at}>
              {lastSeen}
            </time>
            , occurred{" "}
            <span className="font-medium tabular-nums">
              {error.occurrence_count}
            </span>{" "}
            times.
          </DialogDescription>
        </DialogHeader>

        <DialogBody className="space-y-5">
          {/* Core metadata — DefinitionList with KvRow for consistent grid */}
          <DefinitionList>
            <KvRow
              label="Fingerprint"
              copyable={error.md5}
            />
            <KvRow
              label="File"
              value={error.file || undefined}
              mono
            />
            <KvRow
              label="Line"
              value={lineValue}
              mono
              tabular
            />
            <KvRow
              label="Code"
              value={String(error.code)}
              mono
            />
            <KvRow
              label="Request path"
              value={error.request_path || undefined}
              mono
            />
            <KvRow
              label="First seen"
              value={error.first_seen_at}
              mono
            />
            <KvRow
              label="Last seen"
              value={error.last_seen_at}
              mono
            />
            <KvRow
              label="Occurrences"
              value={String(error.occurrence_count)}
              tabular
            />
          </DefinitionList>

          {/* Full message — monospaced block for stack context readability */}
          <div className="space-y-2">
            <h3 className="text-xs font-medium uppercase tracking-wide text-muted-foreground">
              Message
            </h3>
            <pre className="max-h-64 overflow-auto rounded-md border border-border bg-muted/50 p-3 font-mono text-xs text-foreground whitespace-pre-wrap break-words">
              {error.message}
            </pre>
          </div>

          {/* Backtrace — only rendered when frames are present (S1.1+) */}
          {error.backtrace.length > 0 && (
            <BacktraceSection frames={error.backtrace} />
          )}
        </DialogBody>

        <DialogFooter>
          <Button
            type="button"
            variant="ghost"
            onClick={copy}
            aria-label="Copy fingerprint"
          >
            {copied ? "Copied" : "Copy fingerprint"}
          </Button>
          <Button
            type="button"
            variant="outline"
            onClick={() => onSilence(!error.silenced)}
          >
            {error.silenced ? "Unsilence" : "Silence"}
          </Button>
          <Button
            type="button"
            variant="outline"
            onClick={handleIgnore}
            disabled={updateConfig.isPending || !config}
            aria-busy={updateConfig.isPending}
            title={
              isAlreadyIgnored
                ? "Remove this fingerprint from the agent ignore-list"
                : "Add this fingerprint to the agent ignore-list (durable, survives restarts)"
            }
          >
            {updateConfig.isPending
              ? "Saving..."
              : isAlreadyIgnored
                ? "Unignore"
                : "Ignore this error"}
          </Button>
          <Button type="button" onClick={onClose}>
            Close
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function shortMessage(m: string): string {
  if (m.length <= 80) return m;
  return m.slice(0, 80) + "…";
}

// Renders the S1.1 per-error backtrace frames. Each frame is a mono row
// showing the function name followed by the file path and line number.
// Most-recent-call-first order is preserved from the API response.
function BacktraceSection({ frames }: { frames: PhpErrorFrame[] }) {
  return (
    <div className="space-y-2">
      <h3 className="text-xs font-medium uppercase tracking-wide text-muted-foreground">
        Stack trace
      </h3>
      <div className="max-h-64 overflow-auto rounded-md border border-border bg-muted/50 p-3 space-y-1">
        {frames.map((frame, idx) => (
          <div
            key={idx}
            className="font-mono text-xs flex gap-2 items-baseline"
          >
            <span className="text-muted-foreground tabular-nums shrink-0">
              {idx}
            </span>
            <span className="text-foreground shrink-0">
              {frame.function !== "" ? frame.function : "(file scope)"}
            </span>
            <span className="text-muted-foreground truncate">
              {frame.file}
              <span className="text-foreground tabular-nums">
                :{frame.line}
              </span>
            </span>
          </div>
        ))}
      </div>
    </div>
  );
}
