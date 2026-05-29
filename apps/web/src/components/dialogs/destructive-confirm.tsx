import { useId, useState, type ReactNode } from "react";
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

// Sprint 3 destructive-confirm primitive. Per DESIGN.md:
//   "Modal: popover, 12px radius, shadow-lg, max 480px. Title is the action.
//    Destructive requires typing the resource name."
//
// The confirm button stays disabled until the operator types `resourceName`
// verbatim (case-sensitive, trimmed against trailing whitespace). This is the
// only friction; no second confirm dialog, no "yes/no" buttons, no checkbox.
// Both action labels are verb-first ("Restore site", "Keep current state").
//
// Use this for: Restore, Disconnect, Revoke, Delete. Do NOT use for ordinary
// form submission — that's the additive modal pattern.

export interface DestructiveConfirmProps {
  open: boolean;
  onClose: () => void;
  onConfirm: () => void | Promise<void>;
  /** Verb-first title; the action being committed. */
  title: string;
  /** Consequences: what changes, why it's irreversible, how long it takes. */
  consequencesBody: ReactNode;
  /** The literal string the operator must type to enable the destructive button. */
  resourceName: string;
  /** Verb-first label on the destructive button. */
  confirmLabel: string;
  /** Verb-first label on the cancel-equivalent. NEVER "Cancel". */
  cancelLabel: string;
  /** When true: confirm button shows a spinner and both buttons disable. */
  isPending?: boolean;
  /** Optional inline error message, rendered above the input. */
  errorMessage?: string | null;
}

export function DestructiveConfirm({
  open,
  onClose,
  onConfirm,
  title,
  consequencesBody,
  resourceName,
  confirmLabel,
  cancelLabel,
  isPending = false,
  errorMessage,
}: DestructiveConfirmProps) {
  const titleId = useId();
  const descriptionId = useId();
  const inputId = useId();
  const [typed, setTyped] = useState("");

  // Reset the typed value whenever the dialog opens; preserve it across
  // re-renders while open so a failed confirmation doesn't wipe the operator's
  // input (they may want to retry).
  const [prevOpen, setPrevOpen] = useState(open);
  if (open !== prevOpen) {
    setPrevOpen(open);
    if (open) setTyped("");
  }

  const matches = typed === resourceName;
  const canConfirm = matches && !isPending;

  async function handleConfirm() {
    if (!canConfirm) return;
    await onConfirm();
  }

  return (
    <Dialog open={open} onClose={isPending ? () => {} : onClose}>
      <DialogContent
        ariaLabelledBy={titleId}
        ariaDescribedBy={descriptionId}
      >
        <DialogHeader>
          <DialogTitle id={titleId}>{title}</DialogTitle>
        </DialogHeader>

        <DialogBody>
          <div
            id={descriptionId}
            className="text-sm text-[var(--color-foreground)]"
          >
            {consequencesBody}
          </div>

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
              Type{" "}
              <code className="rounded-sm bg-[var(--color-muted)] px-1 font-mono text-xs text-[var(--color-foreground)]">
                {resourceName}
              </code>{" "}
              to confirm
            </Label>
            <Input
              id={inputId}
              value={typed}
              onChange={(e) => setTyped(e.target.value)}
              autoComplete="off"
              autoCorrect="off"
              spellCheck={false}
              data-autofocus
              aria-invalid={typed.length > 0 && !matches ? true : undefined}
              disabled={isPending}
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
            {cancelLabel}
          </Button>
          <Button
            type="button"
            variant="destructive"
            onClick={() => void handleConfirm()}
            disabled={!canConfirm}
          >
            {isPending ? (
              <>
                <Loader2 aria-hidden="true" className="animate-spin" />
                <span className="sr-only">{confirmLabel}</span>
              </>
            ) : (
              confirmLabel
            )}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
