import { Check, Copy, EyeOff, Eye } from "lucide-react";
import { useState } from "react";

import { Button } from "@/components/ui/button";
import { TableCell, TableRow } from "@/components/ui/table";
import { relativeTime } from "@/lib/utils";
import type { PhpError } from "@wpmgr/api";

import { PHPSeverityChip } from "./php-severity-chip";

// One row of the PHP error table (ADR-037 Batch 4, Impeccable Restyle).
//
// Layout mirrors the Activity feed density: the error message leads as the
// primary sentence; file:line and count are always surfaced on the same row
// as structured metadata. The whole row is keyboard-clickable to open the
// detail dialog; action buttons stop propagation so silence + copy do not
// also open the dialog.
//
// Typography rules:
//   • font-mono on file path, line number, and the message head (stack context).
//   • tabular-nums on occurrence count and relative time columns.
//   • relativeTime in a <time> element for accessibility.
//   • PHPSeverityChip replaces the off-token local SeverityBadge.

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
      {/* 1. Severity — chip with dot+label, token colors only */}
      <TableCell>
        <PHPSeverityChip severity={error.severity} />
      </TableCell>

      {/* 2. File:line — font-mono, truncate long paths */}
      <TableCell
        className="max-w-[280px] truncate font-mono text-xs"
        title={fileLine}
      >
        {fileLine}
      </TableCell>

      {/* 3. Message excerpt — font-mono so stack context reads in the right register */}
      <TableCell
        className="max-w-[420px] truncate font-mono text-xs text-foreground"
        title={error.message}
      >
        {error.message}
      </TableCell>

      {/* 4. Occurrence count — tabular-nums, right-aligned */}
      <TableCell className="text-right tabular-nums text-sm">
        {error.occurrence_count}
      </TableCell>

      {/* 5. Last seen — relative time in <time>, tabular-nums */}
      <TableCell className="text-right">
        <time
          dateTime={error.last_seen_at}
          title={error.last_seen_at}
          className="text-xs tabular-nums text-muted-foreground"
        >
          {relativeTime(error.last_seen_at) ?? "just now"}
        </time>
      </TableCell>

      {/* 6. Actions — silence/unsilence + copy fingerprint */}
      <TableCell className="text-right">
        <div className="inline-flex gap-1">
          <Button
            size="sm"
            variant="ghost"
            type="button"
            onClick={toggleSilence}
            aria-label={error.silenced ? "Unsilence error" : "Silence error"}
            title={error.silenced ? "Unsilence" : "Silence"}
          >
            {error.silenced ? (
              <Eye aria-hidden="true" className="size-4" />
            ) : (
              <EyeOff aria-hidden="true" className="size-4" />
            )}
          </Button>
          <Button
            size="sm"
            variant="ghost"
            type="button"
            onClick={copyFingerprint}
            aria-label="Copy fingerprint"
            title={copied ? "Copied" : "Copy fingerprint"}
          >
            {copied ? (
              <Check aria-hidden="true" className="size-4" />
            ) : (
              <Copy aria-hidden="true" className="size-4" />
            )}
          </Button>
        </div>
      </TableCell>
    </TableRow>
  );
}
