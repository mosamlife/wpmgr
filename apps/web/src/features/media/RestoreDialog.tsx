import { useId, useState } from "react";
import { Loader2 } from "lucide-react";

import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogBody,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";

// RestoreDialog — type-the-count SOFT confirm. Restoring reverts optimized
// attachments to their archived originals. It's reversible (you can re-optimize)
// so this is a lighter gate than delete-originals: the operator types the count
// of assets being restored to confirm intent, not the site hostname.

export interface RestoreDialogProps {
  open: boolean;
  onClose: () => void;
  /** Number of assets to restore. */
  count: number;
  onConfirm: () => void;
  isPending?: boolean;
  errorMessage?: string | null;
}

export function RestoreDialog({
  open,
  onClose,
  count,
  onConfirm,
  isPending = false,
  errorMessage,
}: RestoreDialogProps) {
  const titleId = useId();
  const inputId = useId();
  const [typed, setTyped] = useState("");

  // Reset on open.
  const [prevOpen, setPrevOpen] = useState(open);
  if (open !== prevOpen) {
    setPrevOpen(open);
    if (open) setTyped("");
  }

  const expected = String(count);
  const matches = typed.trim() === expected;
  const canConfirm = matches && !isPending && count > 0;

  function handleConfirm() {
    if (!canConfirm) return;
    onConfirm();
  }

  return (
    <Dialog open={open} onClose={isPending ? () => {} : onClose}>
      <DialogContent ariaLabelledBy={titleId}>
        <DialogHeader>
          <DialogTitle id={titleId}>
            Restore <span className="tabular-nums">{count.toLocaleString()}</span>{" "}
            {count === 1 ? "attachment" : "attachments"}
          </DialogTitle>
        </DialogHeader>

        <DialogBody>
          <p className="text-sm text-[var(--color-foreground)]">
            We revert each attachment to its archived original and remove the
            optimized variants from the site. This is reversible: you can
            optimize again later.
          </p>

          {errorMessage ? (
            <p
              role="alert"
              className="rounded-md border border-[var(--color-destructive)]/40 bg-[var(--color-destructive)]/10 p-2 text-sm text-[var(--color-destructive)]"
            >
              {errorMessage}
            </p>
          ) : null}

          <div className="space-y-2">
            <Label htmlFor={inputId}>
              Type the count{" "}
              <code className="rounded-sm bg-[var(--color-muted)] px-1 font-mono text-xs tabular-nums text-[var(--color-foreground)]">
                {expected}
              </code>{" "}
              to confirm
            </Label>
            <Input
              id={inputId}
              inputMode="numeric"
              value={typed}
              onChange={(e) => setTyped(e.target.value)}
              autoComplete="off"
              data-autofocus
              aria-invalid={typed.length > 0 && !matches ? true : undefined}
              disabled={isPending}
              className="tabular-nums"
            />
          </div>
        </DialogBody>

        <DialogFooter className="pt-2">
          <Button
            type="button"
            variant="outline"
            onClick={onClose}
            disabled={isPending}
          >
            Keep optimized
          </Button>
          <Button type="button" onClick={handleConfirm} disabled={!canConfirm}>
            {isPending ? (
              <>
                <Loader2 aria-hidden="true" className="animate-spin" />
                <span className="sr-only">Restore attachments</span>
              </>
            ) : (
              `Restore ${count.toLocaleString()}`
            )}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
