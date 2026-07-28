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
  /**
   * Native `title` tooltip — pass the exact absolute timestamp so hovering
   * the chip reveals full precision alongside the relative `time` text.
   */
  title?: string;
  /**
   * Dense presentation for a table column already headed "Backup" (GH
   * #255): the status word is moved into visually-hidden text so the chip
   * renders just the relative time. Standalone surfaces (site card, health
   * page) have no such header and keep the default full label.
   */
  compact?: boolean;
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
 *
 * In `compact` mode the leading noun moves into visually-hidden text: a
 * success reads "10h ago" on screen and still announces "Backed up 10h
 * ago". Only the redundant word is dropped. The failed state keeps its own
 * icon, its destructive palette AND its visible "Failed" word, because that
 * is the one row an operator must never skim past.
 *
 * The announcement is assembled as ONE visually-hidden string with the
 * visible fragments marked aria-hidden, rather than interleaving hidden and
 * visible text: adjacent inline spans are concatenated without a separator
 * when a name is computed from contents, which turned "Backup" + "Failed"
 * into "BackupFailed".
 */
export function BackupChip({
  status,
  time,
  progressPercent,
  onRetry,
  className,
  title,
  compact = false,
}: BackupChipProps) {
  // With no time to show, "compact success" would render an empty chip, so
  // it keeps the full label regardless.
  const compactSuccess = compact && Boolean(time);
  const percent =
    typeof progressPercent === "number" ? Math.round(progressPercent) : null;
  const announcement =
    status === "running"
      ? `Backup running${percent !== null ? ` ${percent}%` : ""}`
      : "Backup failed";
  return (
    <span
      title={title}
      className={cn(
        "inline-flex items-center gap-1.5 rounded px-2 py-0.5 text-xs font-medium",
        compact && "whitespace-nowrap",
        statusBg[status],
        className,
      )}
    >
      {status === "success" ? (
        <>
          <CheckCircle2 aria-hidden="true" className="size-3" />
          {compactSuccess ? (
            <>
              <span className="sr-only">{`Backed up ${time}`}</span>
              <span aria-hidden="true" className="tabular-nums">
                {time}
              </span>
            </>
          ) : (
            <span>Backed up{time ? ` ${time}` : ""}</span>
          )}
        </>
      ) : null}
      {status !== "success" && compact ? (
        <span className="sr-only">{announcement}</span>
      ) : null}
      {status === "running" ? (
        <>
          <Loader2
            aria-hidden="true"
            className="size-3 motion-safe:animate-spin"
          />
          <span aria-hidden={compact ? "true" : undefined}>
            {compact ? "Running" : "Backup running"}
          </span>
          {percent !== null ? (
            <span
              aria-hidden={compact ? "true" : undefined}
              className="font-mono tabular-nums"
            >
              {percent}%
            </span>
          ) : null}
        </>
      ) : null}
      {status === "failed" ? (
        <>
          <XCircle aria-hidden="true" className="size-3" />
          <span aria-hidden={compact ? "true" : undefined}>Failed</span>
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
