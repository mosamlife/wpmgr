import { CheckCircle2, Loader2, XCircle } from "lucide-react";

import { cn } from "@/lib/utils";

export type BackupChipStatus = "success" | "running" | "failed";

export interface BackupChipProps {
  status: BackupChipStatus;
  /** Pre-formatted relative time for completed backups ("2h ago"). */
  time?: string;
  /** 0-100 progress while running. Rendered as "{n}%". */
  progressPercent?: number;
  /** When provided on failed backups, renders an inline "Retry" link button. */
  onRetry?: () => void;
  className?: string;
}

const statusBg: Record<BackupChipStatus, string> = {
  success: "bg-success-subtle text-success-subtle-fg",
  running: "bg-info-subtle text-info-subtle-fg",
  failed: "bg-destructive-subtle text-destructive-subtle-fg",
};

/**
 * BackupChip — single chip with three states for backup status surfaces
 * (snapshot list rows, site row "last backup" column, restore drawer header).
 *
 * - success: check icon + "Backed up {time}"
 * - running: spinning loader + "Backup running" + percent (when known)
 * - failed:  X icon + "Failed" + optional inline Retry
 */
export function BackupChip({
  status,
  time,
  progressPercent,
  onRetry,
  className,
}: BackupChipProps) {
  return (
    <span
      className={cn(
        "inline-flex items-center gap-1.5 rounded px-2 py-0.5 text-xs font-medium",
        statusBg[status],
        className,
      )}
    >
      {status === "success" ? (
        <>
          <CheckCircle2 aria-hidden="true" className="size-3" />
          <span>Backed up{time ? ` ${time}` : ""}</span>
        </>
      ) : null}
      {status === "running" ? (
        <>
          <Loader2
            aria-hidden="true"
            className="size-3 motion-safe:animate-spin"
          />
          <span>Backup running</span>
          {typeof progressPercent === "number" ? (
            <span className="font-mono tabular-nums">
              {Math.round(progressPercent)}%
            </span>
          ) : null}
        </>
      ) : null}
      {status === "failed" ? (
        <>
          <XCircle aria-hidden="true" className="size-3" />
          <span>Failed</span>
          {onRetry ? (
            <button
              type="button"
              onClick={onRetry}
              className="ml-1 cursor-pointer text-xs underline underline-offset-2"
            >
              Retry
            </button>
          ) : null}
        </>
      ) : null}
    </span>
  );
}
