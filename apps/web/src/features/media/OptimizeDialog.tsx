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
import { Label } from "@/components/ui/label";

import {
  optimizeExplainer,
  validateOptimize,
} from "./optimize-validation";
import type { TargetFormat, TargetQuality } from "./types";

// OptimizeDialog — pick target format (AVIF / WebP / Original) + quality
// (Lossy / Lossless) with a "what happens" explainer. Submits via onConfirm
// (no HTML <form> — onClick per the project rule). Validation is the pure
// `validateOptimize`; the confirm button reflects its result.

export interface OptimizeDialogProps {
  open: boolean;
  onClose: () => void;
  /** Number of explicitly selected assets (0 → "all pending" is the only mode). */
  selectedCount: number;
  /** Total pending assets, for the "all pending" count label. */
  pendingCount: number;
  onConfirm: (input: {
    targetFormat: TargetFormat;
    targetQuality: TargetQuality;
    allPending: boolean;
  }) => void;
  isPending?: boolean;
  errorMessage?: string | null;
}

const FORMAT_OPTIONS: { value: TargetFormat; label: string; hint: string }[] = [
  { value: "avif", label: "AVIF", hint: "Smallest. Best for photos." },
  { value: "webp", label: "WebP", hint: "Broad support. Safe default." },
  {
    value: "original",
    label: "Original",
    hint: "Re-compress, keep the format.",
  },
];

const QUALITY_OPTIONS: { value: TargetQuality; label: string; hint: string }[] =
  [
    { value: "lossy", label: "Lossy", hint: "Smaller files, near-identical." },
    { value: "lossless", label: "Lossless", hint: "Pixel-perfect, larger." },
  ];

export function OptimizeDialog({
  open,
  onClose,
  selectedCount,
  pendingCount,
  onConfirm,
  isPending = false,
  errorMessage,
}: OptimizeDialogProps) {
  const titleId = useId();
  const [targetFormat, setTargetFormat] = useState<TargetFormat>("avif");
  const [targetQuality, setTargetQuality] = useState<TargetQuality>("lossy");

  // "all pending" when nothing is explicitly selected.
  const allPending = selectedCount === 0;
  const count = allPending ? pendingCount : selectedCount;

  const validation = validateOptimize({
    targetFormat,
    targetQuality,
    selectedCount,
    allPending,
  });
  const canConfirm = validation.ok && !isPending && count > 0;

  function handleConfirm() {
    if (!canConfirm) return;
    onConfirm({ targetFormat, targetQuality, allPending });
  }

  return (
    <Dialog open={open} onClose={isPending ? () => {} : onClose}>
      <DialogContent ariaLabelledBy={titleId}>
        <DialogHeader>
          <DialogTitle id={titleId}>
            Optimize{" "}
            <span className="tabular-nums">{count.toLocaleString()}</span>{" "}
            {allPending ? "pending " : ""}
            {count === 1 ? "asset" : "assets"}
          </DialogTitle>
        </DialogHeader>

        <DialogBody>
          {/* Target format */}
          <fieldset className="space-y-2" aria-label="Target format">
            <Label className="text-xs uppercase tracking-[0.02em] text-[var(--color-muted-foreground)]">
              Target format
            </Label>
            <div className="grid grid-cols-3 gap-2">
              {FORMAT_OPTIONS.map((opt) => (
                <ChoiceTile
                  key={opt.value}
                  name="target-format"
                  selected={targetFormat === opt.value}
                  label={opt.label}
                  hint={opt.hint}
                  onSelect={() => setTargetFormat(opt.value)}
                  disabled={isPending}
                />
              ))}
            </div>
          </fieldset>

          {/* Quality */}
          <fieldset className="space-y-2" aria-label="Quality">
            <Label className="text-xs uppercase tracking-[0.02em] text-[var(--color-muted-foreground)]">
              Quality
            </Label>
            <div className="grid grid-cols-2 gap-2">
              {QUALITY_OPTIONS.map((opt) => (
                <ChoiceTile
                  key={opt.value}
                  name="target-quality"
                  selected={targetQuality === opt.value}
                  label={opt.label}
                  hint={opt.hint}
                  onSelect={() => setTargetQuality(opt.value)}
                  disabled={isPending}
                />
              ))}
            </div>
          </fieldset>

          {/* What happens */}
          <div className="rounded-md border border-[var(--color-border)] bg-[var(--color-muted)]/40 p-3">
            <p className="text-xs font-medium uppercase tracking-[0.02em] text-[var(--color-muted-foreground)]">
              What happens
            </p>
            <p className="mt-1 text-sm text-[var(--color-foreground)]">
              {optimizeExplainer(targetFormat, targetQuality)}
            </p>
          </div>

          {errorMessage ? (
            <p
              role="alert"
              className="rounded-md border border-[var(--color-destructive)]/40 bg-[var(--color-destructive)]/10 p-2 text-sm text-[var(--color-destructive)]"
            >
              {errorMessage}
            </p>
          ) : !validation.ok ? (
            <p
              role="alert"
              className="text-sm text-[var(--color-muted-foreground)]"
            >
              {validation.error}
            </p>
          ) : null}
        </DialogBody>

        <DialogFooter className="pt-2">
          <Button
            type="button"
            variant="outline"
            onClick={onClose}
            disabled={isPending}
          >
            Keep as is
          </Button>
          <Button type="button" onClick={handleConfirm} disabled={!canConfirm}>
            {isPending ? (
              <>
                <Loader2 aria-hidden="true" className="animate-spin" />
                <span className="sr-only">Start optimizing</span>
              </>
            ) : (
              `Optimize ${count.toLocaleString()}`
            )}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

interface ChoiceTileProps {
  name: string;
  selected: boolean;
  label: string;
  hint: string;
  onSelect: () => void;
  disabled?: boolean;
}

function ChoiceTile({
  selected,
  label,
  hint,
  onSelect,
  disabled,
}: ChoiceTileProps) {
  return (
    <button
      type="button"
      onClick={onSelect}
      disabled={disabled}
      aria-pressed={selected}
      className={
        "flex flex-col items-start gap-0.5 rounded-md border p-2.5 text-left transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)] disabled:opacity-50 " +
        (selected
          ? "border-[var(--color-primary)] bg-[var(--color-primary)]/5 ring-1 ring-[var(--color-primary)]"
          : "border-[var(--color-border)] hover:bg-[var(--color-muted)]/50")
      }
    >
      <span className="text-sm font-medium text-[var(--color-foreground)]">
        {label}
      </span>
      <span className="text-[11px] leading-tight text-[var(--color-muted-foreground)]">
        {hint}
      </span>
    </button>
  );
}
