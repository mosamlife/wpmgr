import { useEffect } from "react";
import { AlertTriangle, Download, X } from "lucide-react";

import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogDescription,
  DialogFooter,
} from "@/components/ui/dialog";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";

import { useFindingFile, type ScanFinding } from "./use-scan";

// S3 — File viewer modal for scan findings.
//
// Opened only when the operator explicitly clicks "View file". Shows the
// raw content of the finding (base64-decoded from the API response) in a
// read-only, scrollable mono code block.
//
// Safety rules:
//   - Content is READ ONLY — no editing affordance.
//   - A "potentially malicious — read only" warning is shown unconditionally
//     at the top of the modal.
//   - Display is capped at MAX_DISPLAY_CHARS chars to prevent locking the
//     browser tab on enormous files (truncation note is shown).
//   - atob() is used to decode content_base64; errors are caught gracefully.
//   - The file endpoint is called lazily on open (useFindingFile mutation),
//     not eagerly when the table renders.

const MAX_DISPLAY_CHARS = 50_000;

interface FindingFileModalProps {
  siteId: string;
  runId: string;
  finding: ScanFinding | null;
  onClose: () => void;
}

export function FindingFileModal({
  siteId,
  runId,
  finding,
  onClose,
}: FindingFileModalProps) {
  const isOpen = finding !== null;
  const fetchFile = useFindingFile();

  // Reset the mutation state when a new finding is selected so we don't show
  // a previous finding's content while the new request is in-flight.
  useEffect(() => {
    if (isOpen && finding) {
      fetchFile.reset();
      fetchFile.mutate({ siteId, runId, findingId: finding.id });
    }
    // Only re-run when the finding identity changes, not on fetchFile reference change.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [finding?.id, siteId, runId, isOpen]);

  const [decodedContent, decodeError] = decodeBase64Content(
    fetchFile.data?.content_base64,
  );

  return (
    <Dialog open={isOpen} onClose={onClose}>
      <DialogContent
        ariaLabelledBy="file-modal-title"
        ariaDescribedBy="file-modal-desc"
        className="flex max-h-[80vh] max-w-[min(720px,calc(100vw-2rem))] flex-col"
      >
        <DialogHeader>
          <DialogTitle id="file-modal-title">
            View file
          </DialogTitle>
          <DialogDescription id="file-modal-desc">
            {finding
              ? finding.path
              : "File contents"}
          </DialogDescription>
        </DialogHeader>

        {/* Safety warning — always shown, unconditionally */}
        <div
          role="alert"
          className="flex items-start gap-2 rounded-md border border-[var(--color-destructive)]/30 bg-[var(--color-destructive)]/5 px-3 py-2"
        >
          <AlertTriangle
            aria-hidden="true"
            className="mt-0.5 size-4 shrink-0 text-[var(--color-destructive)]"
          />
          <p className="text-xs text-[var(--color-foreground)]">
            <span className="font-semibold">Potentially malicious content.</span>{" "}
            This file is shown read-only. Do not copy and execute any content
            you find here.
          </p>
        </div>

        {/* Content area */}
        <div className="flex-1 overflow-hidden rounded-md border border-[var(--color-border)] bg-[var(--color-muted)]/30">
          <FileContentBody
            isPending={fetchFile.isPending}
            isError={fetchFile.isError}
            error={fetchFile.error}
            content={decodedContent}
            decodeError={decodeError}
            path={finding?.path ?? null}
            size={fetchFile.data?.size ?? null}
          />
        </div>

        <DialogFooter>
          <Button
            type="button"
            variant="outline"
            size="sm"
            onClick={onClose}
          >
            <X aria-hidden="true" className="size-3.5" />
            Close
          </Button>
          {decodedContent && finding ? (
            <DownloadButton content={decodedContent} path={finding.path} />
          ) : null}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

// ---------------------------------------------------------------------------
// File content body
// ---------------------------------------------------------------------------

interface FileContentBodyProps {
  isPending: boolean;
  isError: boolean;
  error: Error | null;
  content: string | null;
  decodeError: string | null;
  path: string | null;
  size: number | null;
}

function FileContentBody({
  isPending,
  isError,
  error,
  content,
  decodeError,
  path,
  size,
}: FileContentBodyProps) {
  if (isPending) {
    return (
      <div
        role="status"
        aria-busy="true"
        aria-label="Loading file"
        className="p-4 space-y-2"
      >
        <span className="sr-only">Loading file contents</span>
        <Skeleton className="h-3 w-full" />
        <Skeleton className="h-3 w-3/4" />
        <Skeleton className="h-3 w-full" />
        <Skeleton className="h-3 w-1/2" />
      </div>
    );
  }

  if (isError) {
    return (
      <div className="flex items-start gap-2 p-4">
        <AlertTriangle
          aria-hidden="true"
          className="mt-0.5 size-4 shrink-0 text-[var(--color-destructive)]"
        />
        <div>
          <p className="text-sm font-medium text-[var(--color-foreground)]">
            Could not load file.
          </p>
          {error ? (
            <p className="text-xs text-[var(--color-muted-foreground)]">
              {error.message}
            </p>
          ) : null}
        </div>
      </div>
    );
  }

  if (decodeError) {
    return (
      <div className="p-4">
        <p className="text-xs text-[var(--color-destructive)]">{decodeError}</p>
      </div>
    );
  }

  if (!content) {
    return null;
  }

  const truncated = content.length > MAX_DISPLAY_CHARS;
  const display = truncated ? content.slice(0, MAX_DISPLAY_CHARS) : content;

  return (
    <div className="flex h-full flex-col overflow-hidden">
      {/* File metadata bar */}
      {(path ?? size) ? (
        <div className="flex items-center gap-4 border-b border-[var(--color-border)] px-3 py-1.5">
          {path ? (
            <span
              className="truncate font-mono text-xs text-[var(--color-muted-foreground)]"
              title={path}
            >
              {path}
            </span>
          ) : null}
          {size != null ? (
            <span className="ml-auto shrink-0 text-xs tabular-nums text-[var(--color-muted-foreground)]">
              {formatBytes(size)}
            </span>
          ) : null}
        </div>
      ) : null}

      {truncated ? (
        <div className="border-b border-[var(--color-border)] bg-[var(--color-warning-subtle,_oklch(0.97_0.05_85))] px-3 py-1">
          <p className="text-xs text-[var(--color-warning-subtle-fg,_oklch(0.45_0.15_85))]">
            File is large — showing first{" "}
            <span className="tabular-nums font-mono">
              {MAX_DISPLAY_CHARS.toLocaleString()}
            </span>{" "}
            characters.
          </p>
        </div>
      ) : null}

      <pre
        className="flex-1 overflow-auto p-4 font-mono text-xs leading-relaxed text-[var(--color-foreground)] whitespace-pre-wrap break-all"
        aria-label="File contents"
        aria-readonly="true"
      >
        {display}
      </pre>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Download button — triggers browser download of the decoded file
// ---------------------------------------------------------------------------

function DownloadButton({ content, path }: { content: string; path: string }) {
  function handleDownload() {
    const blob = new Blob([content], { type: "text/plain" });
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    // Extract the filename from the path
    const filename = path.split("/").pop() ?? "file.txt";
    a.download = filename;
    a.click();
    URL.revokeObjectURL(url);
  }

  return (
    <Button
      type="button"
      variant="outline"
      size="sm"
      onClick={handleDownload}
    >
      <Download aria-hidden="true" className="size-3.5" />
      Download
    </Button>
  );
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

/** Decode base64 content safely. Returns [decoded, errorMessage]. */
function decodeBase64Content(
  base64: string | undefined,
): [string | null, string | null] {
  if (!base64) return [null, null];
  try {
    const decoded = atob(base64);
    return [decoded, null];
  } catch {
    return [null, "Could not decode file content (invalid base64)."];
  }
}

function formatBytes(bytes: number): string {
  if (bytes === 0) return "0 B";
  const units = ["B", "KB", "MB", "GB"];
  const exp = Math.min(
    Math.floor(Math.log(bytes) / Math.log(1024)),
    units.length - 1,
  );
  const value = bytes / Math.pow(1024, exp);
  return `${exp === 0 ? value : value.toFixed(1)} ${units[exp]}`;
}
