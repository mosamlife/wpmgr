import { DestructiveConfirm } from "@/components/dialogs/destructive-confirm";

// DeleteOriginalsDialog — IRREVERSIBLE. Deleting the archived originals reclaims
// disk on the site but makes restore impossible. We mirror the established
// destructive-confirm pattern (type the site hostname) used for Disconnect /
// Archive — the SAME friction operators already know for irreversible actions.

export interface DeleteOriginalsDialogProps {
  open: boolean;
  onClose: () => void;
  /** The site hostname the operator must type to confirm. */
  hostname: string;
  /** Number of assets whose originals will be deleted. */
  count: number;
  onConfirm: () => void;
  isPending?: boolean;
  errorMessage?: string | null;
}

export function DeleteOriginalsDialog({
  open,
  onClose,
  hostname,
  count,
  onConfirm,
  isPending,
  errorMessage,
}: DeleteOriginalsDialogProps) {
  return (
    <DestructiveConfirm
      open={open}
      onClose={onClose}
      onConfirm={onConfirm}
      title={`Delete originals for ${count.toLocaleString()} ${count === 1 ? "attachment" : "attachments"}`}
      resourceName={hostname}
      confirmLabel="Delete originals"
      cancelLabel="Keep originals"
      isPending={isPending}
      errorMessage={errorMessage}
      consequencesBody={
        <div className="space-y-2">
          <p>
            We permanently delete the archived original files on{" "}
            <code className="font-mono text-xs">{hostname}</code>. This reclaims
            disk space on the site.
          </p>
          <p className="font-medium text-[var(--color-destructive)]">
            This cannot be undone. Once originals are deleted you can no longer
            restore these attachments. Only assets that are fully optimized are
            eligible.
          </p>
        </div>
      }
    />
  );
}
