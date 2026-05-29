import { useState } from "react";
import { Copy, EyeOff } from "lucide-react";

import { Button } from "@/components/ui/button";
import { TableCell, TableRow } from "@/components/ui/table";
import { relativeTime } from "@/lib/utils";
import type { PhpError } from "@wpmgr/api";

// One row of the error table. The whole row is keyboard-clickable so the
// detail dialog opens on Enter/Space; the action buttons stop propagation so
// silence + copy do not also open the dialog.

export interface ErrorRowProps {
  error: PhpError;
  onOpen: () => void;
  onSilence: (silenced: boolean) => void;
}

export function ErrorRow({ error, onOpen, onSilence }: ErrorRowProps) {
  const [copied, setCopied] = useState(false);

  const copyFingerprint = (e: React.MouseEvent) => {
    e.stopPropagation();
    void navigator.clipboard.writeText(error.md5).then(() => {
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1500);
    });
  };

  const toggleSilence = (e: React.MouseEvent) => {
    e.stopPropagation();
    onSilence(!error.silenced);
  };

  const fileLine =
    error.line > 0 ? `${error.file}:${error.line}` : error.file;

  return (
    <TableRow
      onClick={onOpen}
      onKeyDown={(e) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          onOpen();
        }
      }}
      tabIndex={0}
      className="cursor-pointer"
      data-silenced={error.silenced || undefined}
    >
      <TableCell>
        <SeverityBadge severity={error.severity} />
      </TableCell>
      <TableCell className="max-w-[280px] truncate font-mono text-xs">
        {fileLine}
      </TableCell>
      <TableCell className="max-w-[420px] truncate text-sm">
        {error.message}
      </TableCell>
      <TableCell className="text-right tabular-nums">
        {error.occurrence_count}
      </TableCell>
      <TableCell className="text-right text-xs text-muted-foreground tabular-nums">
        {relativeTime(error.last_seen_at) ?? "—"}
      </TableCell>
      <TableCell className="text-right">
        <div className="inline-flex gap-1">
          <Button
            size="sm"
            variant="ghost"
            type="button"
            onClick={toggleSilence}
            aria-label={error.silenced ? "Unsilence" : "Silence"}
            title={error.silenced ? "Unsilence" : "Silence"}
          >
            <EyeOff aria-hidden="true" className="size-4" />
          </Button>
          <Button
            size="sm"
            variant="ghost"
            type="button"
            onClick={copyFingerprint}
            aria-label="Copy fingerprint"
            title={copied ? "Copied" : "Copy fingerprint"}
          >
            <Copy aria-hidden="true" className="size-4" />
          </Button>
        </div>
      </TableCell>
    </TableRow>
  );
}

function SeverityBadge({ severity }: { severity: string }) {
  const tone =
    severity === "fatal"
      ? "border-red-500/40 bg-red-500/10 text-red-700 dark:text-red-300"
      : severity === "warning"
        ? "border-amber-500/40 bg-amber-500/10 text-amber-700 dark:text-amber-300"
        : severity === "notice"
          ? "border-blue-500/40 bg-blue-500/10 text-blue-700 dark:text-blue-300"
          : severity === "deprecated"
            ? "border-purple-500/40 bg-purple-500/10 text-purple-700 dark:text-purple-300"
            : "border-border bg-muted text-muted-foreground";
  return (
    <span
      className={
        "inline-flex items-center rounded-full border px-2 py-0.5 text-xs font-medium " +
        tone
      }
    >
      {severity}
    </span>
  );
}
